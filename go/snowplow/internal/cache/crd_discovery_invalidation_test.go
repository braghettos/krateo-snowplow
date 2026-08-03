// crd_discovery_invalidation_test.go — Task #322 (#318-R2) Commit 1
// falsifier F-3: the SA-discovery invalidator fires STRICTLY AFTER
// DiscoverGroupResources (ADD/UPDATE) / teardown (DELETE), via the
// EXISTING 0.30.233 CRD-lifecycle bridge. This is the F-4-safety gate:
// a count-only assertion would pass even if invalidation raced AHEAD of
// discovery, re-creating the S4 stale-discovery class. Per C5/Q3-b the
// assertion is ORDERING, not just call-count.
//
// Reuses the production-shape harness from crd_add_discovery_test.go +
// crd_lifecycle_bytesobject_test.go (depEventHandlers, the gated
// watcher, WaitCRDDiscoveryProcessedForTest, *bytesObject builders).
//
// MECHANISM — the worker is single-goroutine, so within triggerCRDDiscovery
// the call order is deterministic: DiscoverGroupResources returns, THEN
// invalidateSADiscovery() fires (the line we added). A shared monotonic
// sequence stamped by (a) the fake discovery's ServerGroups() — invoked
// INSIDE DiscoverGroupResources — and (b) the stub invalidator proves
// invalidatorSeq > discoverySeq. For DELETE, DiscoverGroupResources is
// NOT called; the stub invalidator instead captures DeletesProcessed at
// call-time and asserts teardown already completed (>=1).

package cache

import (
	"sync"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
)

// orderSeq is a process-wide monotonic counter used to stamp the relative
// order of (ServerGroups inside DiscoverGroupResources) vs (the
// invalidator callback). Reset per-test.
var orderSeq atomic.Int64

// recordingDiscoveryForCRD is a discovery.DiscoveryInterface stub that, in
// addition to returning a synthetic CRD-backed group, stamps orderSeq the
// first time ServerGroups() is called — i.e. when DiscoverGroupResources
// performs its discovery hop. Mirrors fakeDiscoveryForCRD but with the
// ordering stamp. Distinct type to avoid mutating the shared fixture.
type recordingDiscoveryForCRD struct {
	discovery.DiscoveryInterface // embed nil — unreached methods panic

	group   string
	version string
	res     []metav1.APIResource

	mu               sync.Mutex
	serverGroupsSeq  int64 // orderSeq value at the first ServerGroups() call (0 = never)
	serverGroupsHits int
}

func (f *recordingDiscoveryForCRD) ServerGroups() (*metav1.APIGroupList, error) {
	f.mu.Lock()
	f.serverGroupsHits++
	if f.serverGroupsSeq == 0 {
		f.serverGroupsSeq = orderSeq.Add(1)
	}
	f.mu.Unlock()
	return &metav1.APIGroupList{
		Groups: []metav1.APIGroup{
			{
				Name: f.group,
				Versions: []metav1.GroupVersionForDiscovery{
					{Version: f.version, GroupVersion: f.group + "/" + f.version},
				},
			},
		},
	}, nil
}

func (f *recordingDiscoveryForCRD) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	want := f.group + "/" + f.version
	if gv != want {
		return &metav1.APIResourceList{}, nil
	}
	return &metav1.APIResourceList{GroupVersion: gv, APIResources: f.res}, nil
}

func (f *recordingDiscoveryForCRD) discoverySeq() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.serverGroupsSeq
}

func installRecordingDiscovery(t *testing.T, fake *recordingDiscoveryForCRD) {
	t.Helper()
	discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
		return fake, nil
	}
}

// invalidatorRecorder captures the invalidator's invocation: its count,
// the orderSeq stamp at each call, and the DeletesProcessed counter value
// at call-time (for the DELETE ordering assertion).
type invalidatorRecorder struct {
	mu               sync.Mutex
	calls            int
	lastSeq          int64
	deletesAtLastCall uint64
}

func (r *invalidatorRecorder) hook() func() {
	return func() {
		r.mu.Lock()
		r.calls++
		r.lastSeq = orderSeq.Add(1)
		r.deletesAtLastCall = CRDDiscoveryStatsSnapshot().DeletesProcessed
		r.mu.Unlock()
	}
}

func (r *invalidatorRecorder) snapshot() (calls int, lastSeq int64, deletes uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.lastSeq, r.deletesAtLastCall
}

// withRecordingInvalidator wires a recording invalidator via
// SetSADiscoveryInvalidator and restores the previous one on cleanup.
func withRecordingInvalidator(t *testing.T) *invalidatorRecorder {
	t.Helper()
	orderSeq.Store(0)
	rec := &invalidatorRecorder{}
	SetSADiscoveryInvalidator(rec.hook())
	t.Cleanup(func() { ResetSADiscoveryInvalidatorForTest() })
	return rec
}

// --- F-3 ADD ordering --------------------------------------------------------

// TestCRDAdd_InvalidatorFiresAfterDiscovery asserts that on a CRD ADD the
// SA-discovery invalidator fires STRICTLY AFTER DiscoverGroupResources
// (invalidatorSeq > discoverySeq), exactly once.
func TestCRDAdd_InvalidatorFiresAfterDiscovery(t *testing.T) {
	withCleanCRDDiscovery(t)
	rec := withRecordingInvalidator(t)

	// res is a SUBRESOURCE (Name contains "/") so
	// discoveryIsRegisterableResource returns false and EnsureResourceType
	// is never reached — no dynamic informer factory needed on the bare
	// gate watcher. ServerGroups() + ServerResourcesForGroupVersion() ARE
	// still called (anyOK=true, no error), so the discovery hop runs and
	// stamps the ordering sequence. This isolates the ORDERING assertion
	// (invalidate after discovery) from the informer-spawn machinery the
	// ADD path's downstream needs but this falsifier does not.
	fake := &recordingDiscoveryForCRD{
		group:   "composition.krateo.io",
		version: "v1-2-2",
		res: []metav1.APIResource{
			{Name: "githubscaffoldingwithcompositionpages/status", Kind: "GHSCP", Namespaced: true},
		},
	}
	installRecordingDiscovery(t, fake)
	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	// DiscoverGroupResources early-returns (before ServerGroups) when
	// Global() is nil or in passthrough mode (discovery_lookup.go:220-226).
	// Set the gate watcher (mode==modeInformer, the zero value) as Global
	// so discovery proceeds to the ServerGroups() hop.
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

	// DiscoverGroupResources must have run.
	s := CRDDiscoveryStatsSnapshot()
	if s.DiscoveryInvoked != 1 {
		t.Fatalf("DiscoveryInvoked=%d want 1 (discovery must fire on ADD)", s.DiscoveryInvoked)
	}
	discoverySeq := fake.discoverySeq()
	if discoverySeq == 0 {
		t.Fatalf("ServerGroups() was never called — DiscoverGroupResources did not "+
			"perform its discovery hop; cannot assert ordering")
	}

	calls, invSeq, _ := rec.snapshot()
	if calls != 1 {
		t.Fatalf("invalidator fired %d times on ADD, want exactly 1", calls)
	}
	// THE ORDERING ASSERTION (Q3-b): invalidation STRICTLY AFTER discovery.
	if invSeq <= discoverySeq {
		t.Fatalf("F-4-SAFETY VIOLATION: invalidator fired at seq %d but "+
			"DiscoverGroupResources(ServerGroups) fired at seq %d — invalidation "+
			"must be STRICTLY AFTER discovery, else a mid-run CRD's fresh schema "+
			"could be invalidated away before discovery populated it (S4 stale class)",
			invSeq, discoverySeq)
	}
}

// --- F-3 UPDATE ordering -----------------------------------------------------

// TestCRDUpdate_InvalidatorFiresAfterDiscovery — same ordering gate on the
// CRD UPDATE path (served-version change). UPDATE rides the same
// triggerCRDDiscovery as ADD.
func TestCRDUpdate_InvalidatorFiresAfterDiscovery(t *testing.T) {
	withCleanCRDDiscovery(t)
	rec := withRecordingInvalidator(t)

	// Subresource res (see ADD test rationale) — isolates ordering from
	// the informer-spawn machinery while still exercising the discovery
	// hop. Global must be set so DiscoverGroupResources reaches ServerGroups.
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

	calls, invSeq, _ := rec.snapshot()
	if calls != 1 {
		t.Fatalf("invalidator fired %d times on UPDATE, want exactly 1", calls)
	}
	if invSeq <= discoverySeq {
		t.Fatalf("F-4-SAFETY VIOLATION (UPDATE): invalidator seq %d <= discovery seq %d "+
			"— invalidation must be STRICTLY AFTER discovery", invSeq, discoverySeq)
	}
}

// --- F-3 DELETE ordering -----------------------------------------------------

// TestCRDDelete_InvalidatorFiresAfterTeardown asserts the invalidator
// fires AFTER teardown on a CRD DELETE: at invalidator-call time
// DeletesProcessed is already >=1 (the teardown completed), and the
// invalidator fires exactly once.
func TestCRDDelete_InvalidatorFiresAfterTeardown(t *testing.T) {
	withCleanCRDDiscovery(t)
	rec := withRecordingInvalidator(t)

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
		t.Fatalf("invalidator fired %d times on DELETE, want exactly 1", calls)
	}
	// THE ORDERING ASSERTION (Q3-b, DELETE): teardown completed (counter
	// already bumped) BEFORE the invalidator ran.
	if deletesAtCall < 1 {
		t.Fatalf("F-4-SAFETY VIOLATION (DELETE): invalidator fired with "+
			"DeletesProcessed=%d at call-time — invalidation must be AFTER "+
			"teardown completes (the counter is bumped at the end of "+
			"triggerCRDDelete, just before invalidateSADiscovery)", deletesAtCall)
	}
}

// --- F-3 negative: no invalidation when the bridge does not fire ------------

// TestCRDAdd_NonCRDGVR_NoInvalidation asserts a non-CRD GVR ADD (which
// does NOT reach triggerCRDDiscovery) does NOT fire the invalidator —
// the invalidation is gated by the same IsCRDGVR predicate as discovery.
func TestCRDAdd_NonCRDGVR_NoInvalidation(t *testing.T) {
	withCleanCRDDiscovery(t)
	rec := withRecordingInvalidator(t)

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

	// The non-CRD ADDs enqueue nothing on the CRD-discovery worker; give a
	// brief window then assert no invalidation fired.
	if WaitCRDDiscoveryProcessedForTest(1, 200) {
		t.Fatalf("a non-CRD GVR ADD wrongly enqueued a CRD-discovery event")
	}
	calls, _, _ := rec.snapshot()
	if calls != 0 {
		t.Fatalf("invalidator fired %d times on non-CRD ADDs, want 0 (invalidation "+
			"must be gated by the discovery side-effect, not fire on every event)", calls)
	}
}
