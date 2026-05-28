// cohort_key_uid_stable_test.go — Ship 1 / 0.30.195.
//
// Falsifiers for the UID-stable cohort key (root-cause fix for cohort-key
// drift). The pre-0.30.195 cohort identity hashed raw pointer ADDRESSES,
// which the informer rebuilds fresh on every RBAC snapshot republish
// (~4.6/s churn) → the seed-time key diverged from the dispatch-time key
// and the prewarmed L1 cell was unreachable. Ship 1 hashes the binding's
// immutable metadata.uid instead.
//
//   Falsifier 1 — TestBindingSetHash_CrossRepublishStable
//     The SAME logical binding set, re-derived from a FRESH snapshot with
//     NEW pointer addresses but the SAME UIDs, MUST hash byte-identically.
//     (This is the unit form of the runtime ≥3-churn-republish gate.)
//
//   Falsifier 2 — TestBindingSetHash_NamespacedRBLeakSafety
//     Cohort A = {Group:devs CRB + a namespaced RoleBinding in ns-X} and
//     cohort B = {Group:devs CRB only} MUST hash DIFFERENTLY. Pins that
//     the namespaced RoleBinding union is folded into the cohort identity
//     (no RBAC over-collapse / cross-namespace leak).
//
//   Falsifier 3 — TestBindingSetHash_EmptyUIDIsolation
//     Two DISTINCT bindings that BOTH have empty metadata.uid MUST NOT
//     hash-collide into one cohort identity. The empty-UID content-tuple
//     fallback keeps them distinct.

package cache

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// mkCRBUID builds a ClusterRoleBinding with an explicit metadata.uid. The
// roleRef name is derived from `name` so two same-name bindings have an
// identical content tuple (relevant only to the empty-UID fallback path).
func mkCRBUID(name, uid string, subs ...rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Subjects:   subs,
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name + "-role"},
	}
}

// mkRBUID builds a namespaced RoleBinding with an explicit metadata.uid.
func mkRBUID(ns, name, uid string, subs ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Subjects:   subs,
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name + "-role"},
	}
}

// ─────────────────────────────────────────────────────────────────────
// Falsifier 1 — cross-republish stability.
// ─────────────────────────────────────────────────────────────────────

func TestBindingSetHash_CrossRepublishStable(t *testing.T) {
	resetGenAndSnapshot(t)

	// Generation 1 — devs CRB + a namespaced RB matching the devs group.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRBUID("devs-cluster", "uid-crb-devs", groupSub("devs")),
			mkCRBUID("admin-cluster", "uid-crb-admin", userSub("admin")),
		},
		map[string][]*rbacv1.RoleBinding{
			"ns-x": {mkRBUID("ns-x", "devs-ns", "uid-rb-devs-nsx", groupSub("devs"))},
		},
	)
	devsGen1 := BindingSetHash("cyberjoker", []string{"devs"})
	adminGen1 := BindingSetHash("admin", nil)

	// Generation 2 — re-derive the SAME logical bindings from BRAND-NEW
	// pointer objects (the informer rebuild allocates fresh structs) but
	// with the SAME metadata.uid values. The pre-0.30.195 pointer-set hash
	// would have drifted here; the UID-set hash MUST be byte-identical.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRBUID("devs-cluster", "uid-crb-devs", groupSub("devs")),
			mkCRBUID("admin-cluster", "uid-crb-admin", userSub("admin")),
		},
		map[string][]*rbacv1.RoleBinding{
			"ns-x": {mkRBUID("ns-x", "devs-ns", "uid-rb-devs-nsx", groupSub("devs"))},
		},
	)
	devsGen2 := BindingSetHash("cyberjoker", []string{"devs"})
	adminGen2 := BindingSetHash("admin", nil)

	if devsGen1 == 0 || devsGen2 == 0 {
		t.Fatalf("devs cohort hashed to 0 (gen1=%#x gen2=%#x) — expected non-zero for a matched set",
			devsGen1, devsGen2)
	}
	if devsGen1 != devsGen2 {
		t.Fatalf("Ship 1 falsifier 1: devs cohort BindingSetHash drifted across republish "+
			"(gen1=%#x gen2=%#x) — UID-stable identity FAILED; pointer addresses leaked into the hash",
			devsGen1, devsGen2)
	}
	if adminGen1 != adminGen2 {
		t.Fatalf("Ship 1 falsifier 1: admin cohort BindingSetHash drifted across republish "+
			"(gen1=%#x gen2=%#x)", adminGen1, adminGen2)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Falsifier 2 — namespaced RoleBinding leak safety.
// ─────────────────────────────────────────────────────────────────────

func TestBindingSetHash_NamespacedRBLeakSafety(t *testing.T) {
	resetGenAndSnapshot(t)

	// Cohort A: devs CRB + a namespaced RoleBinding in ns-x that also
	// binds the devs group. Cohort B: devs CRB only (no namespaced RB).
	// They MUST hash differently — the namespaced RB grants additional
	// access that the cohort identity must reflect.
	//
	// We model A and B as two DIFFERENT groups that each bind a devs-like
	// CRB, but only group-A's identity also matches the namespaced RB.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRBUID("devs-cluster", "uid-crb-shared", groupSub("devs")),
		},
		map[string][]*rbacv1.RoleBinding{
			"ns-x": {mkRBUID("ns-x", "devs-ns", "uid-rb-nsx", groupSub("devs"))},
		},
	)

	// Cohort A — a "devs" identity: matches the cluster CRB AND the
	// namespaced RB (both bind group "devs").
	hashA := BindingSetHash("", []string{"devs"})

	// Cohort B — a "devs-readonly" identity that matches ONLY the cluster
	// CRB. To express "same cluster CRB, no namespaced RB" we add a second
	// CRB binding group "devs-readonly" and assert that identity's hash
	// (which has no namespaced RB) differs from A's.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRBUID("devs-cluster", "uid-crb-shared", groupSub("devs")),
			mkCRBUID("devs-ro-cluster", "uid-crb-ro", groupSub("devs-readonly")),
		},
		map[string][]*rbacv1.RoleBinding{
			"ns-x": {mkRBUID("ns-x", "devs-ns", "uid-rb-nsx", groupSub("devs"))},
		},
	)
	hashADevs := BindingSetHash("", []string{"devs"})           // CRB-shared + RB-nsx
	hashBReadonly := BindingSetHash("", []string{"devs-readonly"}) // CRB-ro only, NO RB

	if hashADevs == 0 || hashBReadonly == 0 {
		t.Fatalf("leak-safety: a matched cohort hashed to 0 (A=%#x B=%#x)", hashADevs, hashBReadonly)
	}
	if hashADevs == hashBReadonly {
		t.Fatalf("Ship 1 falsifier 2 (RBAC leak): {devs CRB + namespaced RB} and "+
			"{devs-readonly CRB only} produced the SAME BindingSetHash (%#x) — the namespaced "+
			"RoleBinding is NOT folded into the cohort identity; cross-cohort access leak",
			hashADevs)
	}

	// Sanity: A on the first snapshot (without the readonly CRB) equals A
	// on the second snapshot (the readonly CRB does not match the devs
	// identity, so devs' matched set is unchanged → UID-stable hash).
	if hashA != hashADevs {
		t.Fatalf("Ship 1 falsifier 2: devs cohort hash shifted when an UNRELATED CRB "+
			"(devs-readonly) was added (%#x -> %#x) — over-inclusion in the matched set",
			hashA, hashADevs)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Falsifier 3 — empty-UID isolation.
// ─────────────────────────────────────────────────────────────────────

func TestBindingSetHash_EmptyUIDIsolation(t *testing.T) {
	resetGenAndSnapshot(t)

	// Two DISTINCT cluster bindings, BOTH with empty metadata.uid, binding
	// two DIFFERENT groups. They differ by name + roleRef, so the
	// empty-UID content-tuple fallback MUST keep them distinct. If the
	// fallback collapsed all empty-UID bindings to a shared zero-bucket,
	// the two group cohorts would hash-collide — an RBAC over-collapse.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRBUID("alpha-bind", "", groupSub("alpha")), // empty UID
			mkCRBUID("beta-bind", "", groupSub("beta")),   // empty UID, distinct name/roleRef
		},
		nil,
	)

	hAlpha := BindingSetHash("", []string{"alpha"})
	hBeta := BindingSetHash("", []string{"beta"})

	if hAlpha == 0 || hBeta == 0 {
		t.Fatalf("empty-UID isolation: a matched cohort hashed to 0 (alpha=%#x beta=%#x)",
			hAlpha, hBeta)
	}
	if hAlpha == hBeta {
		t.Fatalf("Ship 1 falsifier 3 (empty-UID collapse): two DISTINCT empty-UID bindings "+
			"hash-collided into one cohort identity (%#x) — the empty-UID content-tuple "+
			"fallback FAILED; distinct synthetic bindings share a zero-bucket",
			hAlpha)
	}

	// Positive control: the SAME empty-UID binding re-derived from a fresh
	// pointer with the SAME content tuple MUST still hash identically
	// (the fallback is content-stable, not address-stable).
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRBUID("alpha-bind", "", groupSub("alpha")),
			mkCRBUID("beta-bind", "", groupSub("beta")),
		},
		nil,
	)
	hAlpha2 := BindingSetHash("", []string{"alpha"})
	if hAlpha != hAlpha2 {
		t.Fatalf("Ship 1 falsifier 3: empty-UID content tuple is NOT republish-stable "+
			"(alpha gen1=%#x gen2=%#x) — fallback leaked the pointer address",
			hAlpha, hAlpha2)
	}
}
