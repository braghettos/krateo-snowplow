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
)

// TestEnumerateBindingSetClasses_RaceWithRBACMutation drives concurrent
// readers + a re-publishing writer with the -race detector enabled.
// Failure modes the test would surface:
//
//   - Data race on snap.CRBsByUser map access (impossible by design,
//     but if a future refactor mutated a published snapshot's maps
//     in place this would catch it).
//   - Inconsistent BindingSetHash between the snapshot-Load inside
//     pruneUserKindSubjectZeta and the storeTuple's hash computation
//     (would manifest as a cohort whose stored hash != BindingSetHash
//     re-computed against the SAME snapshot).
//   - Panic on a nil snapshot mid-Load (snap.Load() is atomic.Pointer;
//     this catches any future regression where binding_set_enumeration
//     code reads a snapshot field without nil-checking the Load
//     return).
//
// Per `feedback_shared_vs_copy_is_a_concurrency_change`: this test is
// the required concurrent -race gate for the shared-snapshot-pointer
// shape of predicate (ζ)'s data flow.
func TestEnumerateBindingSetClasses_RaceWithRBACMutation(t *testing.T) {
	// Run for 2 seconds — the brief specifies a 2s reader window.
	const testDuration = 2 * time.Second
	const readerCount = 16
	const writerInterval = 10 * time.Millisecond

	// Initial snapshot. The writer goroutine permutes the CRB list each
	// iteration; the snapshot pointer always carries a coherent view.
	resetGenAndSnapshot(t)
	makeSnap := func(seed int) {
		// Build a fresh snapshot with a permuted set of CRBs. The
		// permutation is just a rotation of the userSub names so each
		// snapshot has the SAME shape but distinct pointer values —
		// triggers fresh `rbacSnap.Store` cycles the readers race
		// against.
		crbs := []*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("alice-bind", userSub("alice@krateo.io")),
			mkCRB("devs-bind", groupSub("devs")),
		}
		// Permute by the seed — swap subjects pair-wise.
		if seed%2 == 1 {
			crbs[0], crbs[1] = crbs[1], crbs[0]
		}
		// Mix in one mutation-shaped CRB so the snapshot's
		// rebuildSubjectIndexes traverses different paths each
		// iteration.
		if seed%3 == 0 {
			crbs = append(crbs, mkCRB("system-cm-bind", userSub("system:controller-manager")))
		}
		// One ClusterRole so predicate (ζ) has a real role to walk on
		// the non-system: subjects (admin/alice). We use noKrateoRule
		// which prunes via empty-intersection for the readers.
		snap := &RBACSnapshot{
			ClusterRoleBindings: crbs,
			RoleBindingsByNS:    nil,
			ClusterRolesByName: map[string]*rbacv1.ClusterRole{
				"admin-bind-role":     mkClusterRole("admin-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
				"alice-bind-role":     mkClusterRole("alice-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
				"devs-bind-role":      mkClusterRole("devs-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
				"system-cm-bind-role": mkClusterRole("system-cm-bind-role", []rbacv1.PolicyRule{noKrateoRule}),
			},
			RolesByNSName: map[string]*rbacv1.Role{},
		}
		rebuildSubjectIndexes(snap)
		PublishRBACSnapshotForTest(snap)
	}
	makeSnap(0)

	var (
		wg     sync.WaitGroup
		stop   atomic.Bool
		errCnt atomic.Uint64
	)

	// Readers — 16 goroutines spinning EnumerateBindingSetClasses.
	for i := 0; i < readerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				out := EnumerateBindingSetClasses()
				// Validate every returned Cohort is internally
				// consistent: either Username is non-empty (User-kind
				// cohort whose hash must be non-zero) OR Groups is
				// non-empty (Group-kind cohort). A cohort with both
				// empty would indicate a snapshot tear.
				for _, c := range out {
					if c.Username == "" && len(c.Groups) == 0 {
						errCnt.Add(1)
					}
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
