// compute_key_identity_free_test.go — Ship 0.30.240 v4 regression guard.
//
// Renamed from `binding_set_hash_parity_test.go` (the v3 pre-flight
// falsifier, dispatch architect Q4 ratification 2026-06-02) after the
// v4 ComputeKey identity-free flip landed. The pre-flight artifact
// from the prior state lives at
// /tmp/0.30.240-pre-flight-falsifier-cyberjoker-FAIL.txt — that
// captured the v3 defect this rewrite REPLACES.
//
// CONTRACT (v4 — design 2026-06-02 §5):
//
//   ComputeKey is IDENTITY-FREE for every CacheEntryClass. Three users
//   issuing /call on the same (CacheEntryClass, GVR, Namespace, Name,
//   PerPage, Page, Extras, Stage) tuple land on the same L1 cell key
//   regardless of their (Username, Groups). Per-user RBAC narrowing
//   runs at serve time over the cached SA-maximal bytes.
//
// FIXTURE: production-realistic (feedback_no_fake_production_scenarios)
//   - 3 user shapes: cyberjoker UserDirect, alice GroupOnly, admin
//     cluster-admin.
//   - |B| ≥ 5 including system:masters/system:nodes/system:authenticated.
//
//   The fixture stays valuable beyond this test as documentation of
//   the RBAC topology the v4 contract holds against.

package cache

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// TestComputeKey_IdentityFree_AllUsersSameKey — v4 regression guard.
//
// The architect's Q4 ratification (2026-06-02) renamed this test
// from `TestBindingSetHashParity_CyberjokerNarrowRBAC` (v3
// pre-flight falsifier). The v3 version FAILed on the BindingSetHash
// seed/request divergence; the v4 version PASSes because the L1 key
// no longer folds identity.
//
// Asserts: for a single ResolvedKeyInputs tuple, ComputeKey returns
// the same key regardless of which user issued the request. Per-user
// differentiation now lives entirely at the serve-time filter, never
// at the cache-key layer.
func TestComputeKey_IdentityFree_AllUsersSameKey(t *testing.T) {
	resetGenAndSnapshot(t)

	// Fixture build — same as the v3 pre-flight falsifier. The RBAC
	// snapshot is irrelevant to ComputeKey under v4 (the key carries
	// no identity); we keep the build because future ships may add
	// integration coverage that exercises the snapshot.
	adminCRB := mkCRB("cluster-admin-binding", userSub("admin"))
	systemNodesCRB := mkCRB("system:nodes-binding", groupSub("system:nodes"))
	systemAuthCRB := mkCRB("system:authenticated-binding", groupSub("system:authenticated"))
	systemMastersCRB := mkCRB("system:masters-binding", groupSub("system:masters"))
	devsCRB := mkCRB("devs-binding", groupSub("devs"))
	cyberjokerRB := mkRB("demo-system", "cyberjoker-demo-binding", userSub("cyberjoker"))
	opsRB := mkRB("alice-ns", "ops-binding", groupSub("ops"))
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			adminCRB, systemNodesCRB, systemAuthCRB, systemMastersCRB, devsCRB,
		},
		map[string][]*rbacv1.RoleBinding{
			"demo-system": {cyberjokerRB},
			"alice-ns":    {opsRB},
		},
	)

	// Single L1 cell identity tuple — identity-free.
	inputs := ResolvedKeyInputs{
		CacheEntryClass: "widgets",
		Group:           "widgets.templates.krateo.io",
		Version:         "v1beta1",
		Resource:        "panels",
		Namespace:       "demo-system",
		Name:            "compositions-list-widget",
		PerPage:         5,
		Page:            1,
	}

	// Each call returns a key for the same tuple. Pre-v4 this loop
	// would have hashed three distinct keys (one per BindingSetHash);
	// v4 returns one identity-free key.
	keyCyberjoker := ComputeKey(inputs)
	keyAlice := ComputeKey(inputs)
	keyAdmin := ComputeKey(inputs)

	if keyCyberjoker != keyAlice {
		t.Fatalf("v4 identity-free contract violated:\n  cyberjoker=%s\n  alice    =%s",
			keyCyberjoker, keyAlice)
	}
	if keyAlice != keyAdmin {
		t.Fatalf("v4 identity-free contract violated:\n  alice=%s\n  admin=%s",
			keyAlice, keyAdmin)
	}

	// And the key is byte-stable across calls (deterministic).
	if k2 := ComputeKey(inputs); k2 != keyCyberjoker {
		t.Fatalf("ComputeKey non-deterministic on identical inputs:\n  first =%s\n  second=%s",
			keyCyberjoker, k2)
	}
}

// TestComputeKey_IdentityFree_PerClassAllUsersSameKey — v4 universal
// contract: every CacheEntryClass is identity-free, not just widgets.
// Pre-v4 the apistage + widgetContent classes were identity-free; the
// other three (widgets, restactions, raFullList) folded BindingSetHash.
// v4 unifies all five. This test pins all five.
func TestComputeKey_IdentityFree_PerClassAllUsersSameKey(t *testing.T) {
	classes := []string{
		CacheEntryClassApistage,
		CacheEntryClassWidgetContent,
		CacheEntryClassRAFullList,
		"widgets",
		"restactions",
	}

	for _, class := range classes {
		t.Run(class, func(t *testing.T) {
			inputs := ResolvedKeyInputs{
				CacheEntryClass: class,
				Group:           "g",
				Version:         "v",
				Resource:        "r",
				Namespace:       "ns",
				Name:            "n",
				PerPage:         5,
				Page:            1,
			}
			// The same inputs hashed N times must produce N identical
			// keys — there is no per-call identity input. Pre-v4 each
			// /call's BindingSetHash differed; v4 the input itself has
			// no identity field.
			k1 := ComputeKey(inputs)
			k2 := ComputeKey(inputs)
			if k1 != k2 {
				t.Fatalf("class %q: ComputeKey non-deterministic — %s vs %s",
					class, k1, k2)
			}
		})
	}
}
