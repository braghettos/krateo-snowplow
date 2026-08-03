// crd_schema_invalidation_test.go — Task #323 (#318-R2 Commit 2-B) falsifier
// F-3 for the per-GVR compiled-CRD-schema memo invalidator: the schema-memo
// reset fires STRICTLY AFTER DiscoverGroupResources (ADD/UPDATE) / teardown
// (DELETE), via the SAME EXISTING 0.30.233 CRD-lifecycle bridge, in lockstep
// with the Commit-1 SA-discovery invalidator.
//
// This is the F-4-safety gate for the memo: a count-only assertion would pass
// even if the reset raced AHEAD of discovery (re-creating the S4 stale-schema
// class — a fresh CRD's schema compiled then immediately reset-away before the
// next read). Per C5/Q3-b the assertion is ORDERING, not just call-count.
//
// Reuses the production-shape harness + the recording-discovery / invalidator-
// recorder helpers from crd_add_discovery_test.go +
// crd_discovery_invalidation_test.go + crd_lifecycle_bytesobject_test.go
// (orderSeq, recordingDiscoveryForCRD, invalidatorRecorder, depEventHandlers,
// WaitCRDDiscoveryProcessedForTest, crdBytesObj*, versionSpec). The schema
// invalidator is wired via SetCRDSchemaInvalidator instead of
// SetSADiscoveryInvalidator — same bridge, sibling hook.
//
// MECHANISM — the worker is single-goroutine, so within triggerCRDDiscovery
// the order is deterministic: DiscoverGroupResources returns, THEN
// invalidateSADiscovery(), THEN invalidateCRDSchemaMemo(). The shared
// monotonic orderSeq stamped by (a) the fake discovery's ServerGroups()
// (inside DiscoverGroupResources) and (b) the recording schema invalidator
// proves schemaInvalidatorSeq > discoverySeq.

package cache

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
)

// withRecordingSchemaInvalidator wires a recording invalidator via
// SetCRDSchemaInvalidator (the Commit-2-B sibling hook) and restores the
// previous one on cleanup. Mirrors withRecordingInvalidator (Commit 1) but for
// the schema-memo trampoline. Resets orderSeq so the stamps start clean.
func withRecordingSchemaInvalidator(t *testing.T) *invalidatorRecorder {
	t.Helper()
	orderSeq.Store(0)
	rec := &invalidatorRecorder{}
	SetCRDSchemaInvalidator(rec.hook())
	t.Cleanup(func() { ResetCRDSchemaInvalidatorForTest() })
	return rec
}

// --- F-3 ADD ordering --------------------------------------------------------

// TestCRDAdd_SchemaInvalidatorFiresAfterDiscovery asserts that on a CRD ADD the
// schema-memo invalidator fires STRICTLY AFTER DiscoverGroupResources
// (schemaSeq > discoverySeq), exactly once. Same isolation trick as the
// Commit-1 ADD test: a SUBRESOURCE res (Name contains "/") makes
// discoveryIsRegisterableResource false so EnsureResourceType / informer-spawn
// is never reached, while ServerGroups() still runs and stamps the order.
func TestCRDAdd_SchemaInvalidatorFiresAfterDiscovery(t *testing.T) {
	withCleanCRDDiscovery(t)
	rec := withRecordingSchemaInvalidator(t)

	fake := &recordingDiscoveryForCRD{
		group:   "composition.krateo.io",
		version: "v1-2-2",
		res: []metav1.APIResource{
			{Name: "githubscaffoldingwithcompositionpages/status", Kind: "GHSCP", Namespaced: true},
		},
	}
	installRecordingDiscovery(t, fake)
	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	crdGVR := CRDGVRForTest()
	rw := newGateWatcher()
	ch := make(chan struct{})
	close(ch)
	rw.syncCh[crdGVR] = ch
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil) })
	handlers := rw.depEventHandlers(crdGVR)

	handlers.AddFunc(crdBytesObj(t,
		"githubscaffoldingwithcompositionpages.composition.krateo.io",
		"composition.krateo.io"))

	if !WaitCRDDiscoveryProcessedForTest(1, 2000) {
		t.Fatalf("worker did not process the CRD ADD within 2s; counters: %s",
			crdDiscoveryStatsString())
	}

	s := CRDDiscoveryStatsSnapshot()
	if s.DiscoveryInvoked != 1 {
		t.Fatalf("DiscoveryInvoked=%d want 1 (discovery must fire on ADD)", s.DiscoveryInvoked)
	}
	discoverySeq := fake.discoverySeq()
	if discoverySeq == 0 {
		t.Fatalf("ServerGroups() was never called — DiscoverGroupResources did not " +
			"perform its discovery hop; cannot assert ordering")
	}

	calls, schemaSeq, _ := rec.snapshot()
	if calls != 1 {
		t.Fatalf("schema invalidator fired %d times on ADD, want exactly 1", calls)
	}
	// THE ORDERING ASSERTION (Q3-b): the memo reset is STRICTLY AFTER discovery.
	if schemaSeq <= discoverySeq {
		t.Fatalf("F-4-SAFETY VIOLATION: schema-memo reset fired at seq %d but "+
			"DiscoverGroupResources(ServerGroups) fired at seq %d — the reset must be "+
			"STRICTLY AFTER discovery, else a mid-run CRD's freshly-compiled schema "+
			"could be reset away before the next read repopulates it (S4 stale class)",
			schemaSeq, discoverySeq)
	}
}

// --- F-3 UPDATE ordering -----------------------------------------------------

// TestCRDUpdate_SchemaInvalidatorFiresAfterDiscovery — same ordering gate on
// the CRD UPDATE path (a served-version / schema change is exactly the case
// the memo MUST invalidate on). UPDATE rides the same triggerCRDDiscovery.
func TestCRDUpdate_SchemaInvalidatorFiresAfterDiscovery(t *testing.T) {
	withCleanCRDDiscovery(t)
	rec := withRecordingSchemaInvalidator(t)

	fake := &recordingDiscoveryForCRD{
		group:   "composition.krateo.io",
		version: "v1-2-2",
		res: []metav1.APIResource{
			{Name: "githubscaffoldingwithcompositionpages/status", Kind: "GHSCP", Namespaced: true},
		},
	}
	installRecordingDiscovery(t, fake)
	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	crdGVR := CRDGVRForTest()
	rw := newGateWatcher()
	ch := make(chan struct{})
	close(ch)
	rw.syncCh[crdGVR] = ch
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil) })
	handlers := rw.depEventHandlers(crdGVR)

	oldBO := crdBytesObjMultiVersion(t, "ghscp.composition.krateo.io",
		"composition.krateo.io", "githubscaffoldingwithcompositionpages",
		[]versionSpec{{Name: "v1-2-2", Served: true}})
	newBO := crdBytesObjMultiVersion(t, "ghscp.composition.krateo.io",
		"composition.krateo.io", "githubscaffoldingwithcompositionpages",
		[]versionSpec{
			{Name: "v1-2-2", Served: true},
			{Name: "v1-3-0", Served: true},
		})

	handlers.UpdateFunc(oldBO, newBO)

	if !WaitCRDDiscoveryProcessedForTest(1, 2000) {
		t.Fatalf("worker did not process the CRD UPDATE within 2s; counters: %s",
			crdDiscoveryStatsString())
	}

	s := CRDDiscoveryStatsSnapshot()
	if s.DiscoveryInvoked < 1 {
		t.Fatalf("DiscoveryInvoked=%d want >=1 on UPDATE", s.DiscoveryInvoked)
	}
	discoverySeq := fake.discoverySeq()
	if discoverySeq == 0 {
		t.Fatalf("ServerGroups() never called on UPDATE — cannot assert ordering")
	}

	calls, schemaSeq, _ := rec.snapshot()
	if calls != 1 {
		t.Fatalf("schema invalidator fired %d times on UPDATE, want exactly 1", calls)
	}
	if schemaSeq <= discoverySeq {
		t.Fatalf("F-4-SAFETY VIOLATION (UPDATE): schema-memo reset seq %d <= discovery seq %d "+
			"— the reset must be STRICTLY AFTER discovery", schemaSeq, discoverySeq)
	}
}

// --- F-3 DELETE ordering -----------------------------------------------------

// TestCRDDelete_SchemaInvalidatorFiresAfterTeardown asserts the schema-memo
// reset fires AFTER teardown on a CRD DELETE: at reset-call time
// DeletesProcessed is already >=1 (teardown completed), exactly once.
func TestCRDDelete_SchemaInvalidatorFiresAfterTeardown(t *testing.T) {
	withCleanCRDDiscovery(t)
	rec := withRecordingSchemaInvalidator(t)

	fake := &recordingDiscoveryForCRD{
		group:   "composition.krateo.io",
		version: "v1-2-2",
		res: []metav1.APIResource{
			{Name: "githubscaffoldingwithcompositionpages", Kind: "GHSCP", Namespaced: true},
		},
	}
	installRecordingDiscovery(t, fake)
	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	crdGVR := CRDGVRForTest()
	rw := newGateWatcher()
	ch := make(chan struct{})
	close(ch)
	rw.syncCh[crdGVR] = ch
	handlers := rw.depEventHandlers(crdGVR)

	bo := crdBytesObjMultiVersion(t, "ghscp.composition.krateo.io",
		"composition.krateo.io", "githubscaffoldingwithcompositionpages",
		[]versionSpec{{Name: "v1-2-2", Served: true}})

	targetGVR := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	rw.informers = map[schema.GroupVersionResource]informers.GenericInformer{}
	rw.informers[targetGVR] = nil

	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil) })

	handlers.DeleteFunc(bo)

	if !WaitCRDDiscoveryProcessedForTest(1, 2000) {
		t.Fatalf("worker did not process the CRD DELETE within 2s; counters: %s",
			crdDiscoveryStatsString())
	}

	s := CRDDiscoveryStatsSnapshot()
	if s.DeletesProcessed < 1 {
		t.Fatalf("DeletesProcessed=%d want >=1 (DELETE must reach triggerCRDDelete)",
			s.DeletesProcessed)
	}

	calls, _, deletesAtCall := rec.snapshot()
	if calls != 1 {
		t.Fatalf("schema invalidator fired %d times on DELETE, want exactly 1", calls)
	}
	// THE ORDERING ASSERTION (Q3-b, DELETE): teardown completed (counter
	// already bumped) BEFORE the reset ran.
	if deletesAtCall < 1 {
		t.Fatalf("F-4-SAFETY VIOLATION (DELETE): schema-memo reset fired with "+
			"DeletesProcessed=%d at call-time — the reset must be AFTER teardown "+
			"completes (the counter is bumped at the end of triggerCRDDelete, just "+
			"before invalidateSADiscovery + invalidateCRDSchemaMemo)", deletesAtCall)
	}
}

// --- F-3 negative: no reset when the bridge does not fire -------------------

// TestCRDAdd_NonCRDGVR_NoSchemaInvalidation asserts a non-CRD GVR ADD (which
// does NOT reach triggerCRDDiscovery) does NOT fire the schema-memo reset —
// gated by the same IsCRDGVR predicate as discovery.
func TestCRDAdd_NonCRDGVR_NoSchemaInvalidation(t *testing.T) {
	withCleanCRDDiscovery(t)
	rec := withRecordingSchemaInvalidator(t)

	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	gvr := gvrCompositions() // a non-CRD-meta GVR
	rw := newGateWatcher()
	ch := make(chan struct{})
	close(ch)
	rw.syncCh[gvr] = ch
	handlers := rw.depEventHandlers(gvr)

	for i := 0; i < 3; i++ {
		handlers.AddFunc(unstructuredObj(gvr, "bench-ns-01", "obj-"+string(rune('a'+i))))
	}

	if WaitCRDDiscoveryProcessedForTest(1, 200) {
		t.Fatalf("a non-CRD GVR ADD wrongly enqueued a CRD-discovery event")
	}
	calls, _, _ := rec.snapshot()
	if calls != 0 {
		t.Fatalf("schema invalidator fired %d times on non-CRD ADDs, want 0 (the reset "+
			"must be gated by the discovery side-effect, not fire on every event)", calls)
	}
}
