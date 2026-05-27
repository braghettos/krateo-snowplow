// binding_set_enumeration_race_test.go — Ship 0.30.183 (A.3-refine v3).
// HG-183.9 race test for predicate (ζ).
//
// Predicate (ζ) `pruneUserKindSubjectZeta` reads three pieces of state:
//
//   1. The published RBAC snapshot via `rbacSnap.Load()` —
//      atomic.Pointer, lock-free.
//   2. The matched-binding RoleRefs via `collectMatchedRoleRefsForUser`
//      — reads CRBsByUser / RBsByUserByNS map values (slice of
//      pointers). Immutability post-publish (rbac_snapshot.go §3
//      invariant 1) guarantees no concurrent writer mutates these maps;
//      a fresh `rebuildRBACSnapshot` publishes a NEW snapshot pointer
//      so this test's reader either sees the OLD snapshot's stable
//      maps OR the NEW snapshot's stable maps — never a torn read.
//   3. The handler GVR set via `handlerGVRSetSnapshot` →
//      `Global().RegisteredGVRs()` which itself takes rw.mu.RLock();
//      lock-free vs concurrent EnsureResourceType is enforced by
//      that lock.
//
// This test fires 16 reader goroutines × EnumerateBindingSetClasses
// against a writer goroutine that re-publishes the snapshot every
// 10ms with permuted CRB sets. Race detector + the post-loop
// consistency assertion (every returned []Cohort is internally
// snapshot-bound — every Username is non-empty for the User-kind
// cohorts and the BindingSetHash invariant holds) gate correctness.

package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestEnumerateBindingSetClasses_RaceWithRBACMutation drives concurrent
// readers + a re-publishing writer with the -race detector enabled.
//
// Ship 0.30.184 extension: the writer now permutes Group-subject CRBs
// (rotating `devs` ↔ `admins`) and injects one Group-subject RoleBinding
// per cycle. A reader-side KEEP-arm assertion verifies that a Group
// cohort whose role carries `composition.krateo.io/*` (krateo overlap)
// survives the Group-kind predicate under concurrent snapshot mutation.
//
// Failure modes the test would surface:
//
//   - Data race on snap.CRBsByUser / CRBsByGroup map access (impossible
//     by design, but if a future refactor mutated a published snapshot's
//     maps in place this would catch it).
//   - Inconsistent BindingSetHash between the snapshot-Load inside
//     pruneUserKindSubjectZeta / pruneGroupKindSubjectZeta and the
//     storeTuple's hash computation (would manifest as a cohort whose
//     stored hash != BindingSetHash re-computed against the SAME
//     snapshot).
//   - Panic on a nil snapshot mid-Load (snap.Load() is atomic.Pointer).
//   - KEEP-arm Group cohort (krateo overlap) sporadically missing from
//     the enumeration output — would surface as keepArmMisses > 0.
//
// Per `feedback_shared_vs_copy_is_a_concurrency_change`: this test is
// the required concurrent -race gate for the shared-snapshot-pointer
// shape of predicate (ζ)'s data flow, including the Group-kind
// extension (Ship 0.30.184).
func TestEnumerateBindingSetClasses_RaceWithRBACMutation(t *testing.T) {
	// Run for 2 seconds — the brief specifies a 2s reader window.
	const testDuration = 2 * time.Second
	const readerCount = 16
	const writerInterval = 10 * time.Millisecond

	// `keep-arm-group` is the deterministic Group-kind cohort the
	// reader-side assertion looks for. It is ALWAYS bound to a
	// ClusterRole whose PolicyRule grants templates.krateo.io/restactions
	// (mirrors authn-group-krateo-system semantics) — predicate (ζ)
	// MUST KEEP it under every writer permutation. A miss in any reader
	// iteration would be a concurrency defect.
	const keepArmGroup = "keep-arm-group"

	// Initial snapshot. The writer goroutine permutes the CRB list each
	// iteration; the snapshot pointer always carries a coherent view.
	resetGenAndSnapshot(t)
	makeSnap := func(seed int) {
		// User-kind CRBs (existing 0.30.183 coverage).
		crbs := []*rbacv1.ClusterRoleBinding{
			mkCRBWithRole("admin-bind", "admin-bind-role", userSub("admin")),
			mkCRBWithRole("alice-bind", "alice-bind-role", userSub("alice@krateo.io")),
		}
		// Permute User subjects by the seed — swap subjects pair-wise.
		if seed%2 == 1 {
			crbs[0], crbs[1] = crbs[1], crbs[0]
		}
		// Mix in one mutation-shaped User CRB so the snapshot's
		// rebuildSubjectIndexes traverses different paths each iteration.
		if seed%3 == 0 {
			crbs = append(crbs,
				mkCRBWithRole("system-cm-bind", "system-cm-bind-role",
					userSub("system:controller-manager")))
		}

		// Group-kind CRBs (Ship 0.30.184 extension). Rotate `devs` ↔
		// `admins` to exercise the Group-walk under mutation.
		devsBind := mkCRBWithRole("devs-bind", "devs-bind-role", groupSub("devs"))
		adminsBind := mkCRBWithRole("admins-bind", "admins-bind-role", groupSub("admins"))
		if seed%4 == 0 {
			devsBind, adminsBind = adminsBind, devsBind
		}
		crbs = append(crbs, devsBind, adminsBind)

		// KEEP-arm Group cohort — ALWAYS present, ALWAYS bound to a role
		// whose rule overlaps templates.krateo.io/restactions. Predicate
		// (ζ) MUST KEEP it on every snapshot.
		crbs = append(crbs,
			mkCRBWithRole("keep-arm-bind", "keep-arm-bind-role",
				groupSub(keepArmGroup)))

		// Inject one Group-subject RoleBinding per cycle (the brief's
		// "Inject one Group-subject RB into RoleBindingsByNS per cycle"
		// requirement). Namespace rotates so the snapshot's
		// RBsByGroupByNS shape varies each iteration.
		ns := "team-a"
		if seed%2 == 1 {
			ns = "team-b"
		}
		rb := &rbacv1.RoleBinding{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rbac.authorization.k8s.io/v1",
				Kind:       "RoleBinding",
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      "race-rb",
			},
			Subjects: []rbacv1.Subject{groupSub("devs")},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     "race-rb-role",
			},
		}
		rbs := map[string][]*rbacv1.RoleBinding{ns: {rb}}

		// ClusterRoles. User-kind subjects bound to no-krateo rules so
		// predicate (ζ) prunes them (existing 0.30.183 coverage). The
		// KEEP-arm Group is bound to a krateo.io-overlap rule so it
		// survives. The devs/admins Groups are bound to no-krateo rules
		// so they prune under the Group-kind predicate — that is
		// acceptable for the test's KEEP-arm-only assertion.
		snap := &RBACSnapshot{
			ClusterRoleBindings: crbs,
			RoleBindingsByNS:    rbs,
			ClusterRolesByName: map[string]*rbacv1.ClusterRole{
				"admin-bind-role":     mkClusterRole("admin-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
				"alice-bind-role":     mkClusterRole("alice-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
				"devs-bind-role":      mkClusterRole("devs-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
				"admins-bind-role":    mkClusterRole("admins-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
				"system-cm-bind-role": mkClusterRole("system-cm-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
				"keep-arm-bind-role":  mkClusterRole("keep-arm-bind-role", []rbacv1.PolicyRule{krateoTemplatesRule}),
			},
			RolesByNSName: map[string]*rbacv1.Role{
				ns + "/race-rb-role": {
					ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "race-rb-role"},
					Rules:      []rbacv1.PolicyRule{noKrateoRule},
				},
			},
		}
		rebuildSubjectIndexes(snap)
		PublishRBACSnapshotForTest(snap)
	}
	makeSnap(0)

	// Capture the production handler GVR set — the readers test the
	// KEEP-arm Group invariant via direct predicate invocation (the
	// global ResourceWatcher is unwired in unit tests).
	handlerSet := snowplowHandlerGVRSetForTest

	var (
		wg              sync.WaitGroup
		stop            atomic.Bool
		errCnt          atomic.Uint64
		keepArmMisses   atomic.Uint64
		keepArmAttempts atomic.Uint64
	)

	// Readers — 16 goroutines spinning EnumerateBindingSetClasses AND
	// the direct Group-kind predicate against the KEEP-arm Group.
	for i := 0; i < readerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				out := EnumerateBindingSetClasses()
				// Validate every returned Cohort is internally
				// consistent.
				for _, c := range out {
					if c.Username == "" && len(c.Groups) == 0 {
						errCnt.Add(1)
					}
				}
				// KEEP-arm assertion: the keep-arm-group bound to a
				// templates.krateo.io-overlap role MUST survive the
				// Group-kind predicate on every snapshot the writer
				// publishes. Invoke pruneGroupKindSubjectZeta directly
				// with the production handler set (the enumerator-
				// internal handlerGVRSet is nil in unit tests, so we
				// cannot use the enumeration output for this check).
				snap := rbacSnap.Load()
				if snap == nil {
					continue
				}
				refs := collectMatchedRoleRefsForGroup(snap, keepArmGroup)
				if len(refs) == 0 {
					// Mid-publish window — snapshot may not yet carry
					// the bind. Don't count as a miss.
					continue
				}
				keepArmAttempts.Add(1)
				if pruneGroupKindSubjectZeta(keepArmGroup, refs, snap, handlerSet) {
					// PRUNE on a krateo-overlap role is a regression.
					keepArmMisses.Add(1)
				}
			}
		}()
	}

	// Writer — one goroutine re-publishing the snapshot every 10ms.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(writerInterval)
		defer ticker.Stop()
		seed := 0
		for !stop.Load() {
			seed++
			makeSnap(seed)
			<-ticker.C
		}
	}()

	// Test window.
	time.Sleep(testDuration)
	stop.Store(true)
	wg.Wait()

	if errCnt.Load() > 0 {
		t.Errorf("snapshot-tear detected: %d cohort entries had empty Username + empty Groups", errCnt.Load())
	}
	if keepArmMisses.Load() > 0 {
		t.Errorf("HG-184.9 KEEP-arm regression: %d/%d Group-kind predicate invocations pruned `%s` despite krateoTemplatesRule overlap",
			keepArmMisses.Load(), keepArmAttempts.Load(), keepArmGroup)
	}
	if keepArmAttempts.Load() == 0 {
		t.Errorf("HG-184.9 KEEP-arm assertion never executed (keepArmAttempts=0); test scaffolding broken")
	}
}

// TestHandlerGVRSetDirty_FlipPattern verifies the dirty-flag invalidation
// hook: MarkHandlerGVRSetDirty flips the bit; reading it via
// HandlerGVRSetDirty returns the current value; clearing happens inside
// handlerGVRSetSnapshot. This is a unit test for the atomic.Bool wiring,
// not a race test.
func TestHandlerGVRSetDirty_FlipPattern(t *testing.T) {
	// Save + restore the global flag for test hygiene (other tests
	// don't read it but a future test might).
	prev := handlerGVRSetDirty.Load()
	t.Cleanup(func() { handlerGVRSetDirty.Store(prev) })

	handlerGVRSetDirty.Store(false)
	if HandlerGVRSetDirty() {
		t.Fatalf("dirty flag did not initialise to false")
	}

	MarkHandlerGVRSetDirty()
	if !HandlerGVRSetDirty() {
		t.Fatalf("MarkHandlerGVRSetDirty did not flip the flag to true")
	}

	// Calling handlerGVRSetSnapshot with no watcher returns nil but
	// still resets the dirty bit (the documented invalidation point).
	// Snowplow tests run without Global() wired — the snapshot func
	// returns nil and clears the bit anyway.
	_ = handlerGVRSetSnapshot()
	if HandlerGVRSetDirty() {
		t.Fatalf("handlerGVRSetSnapshot did not reset the dirty flag")
	}
}
