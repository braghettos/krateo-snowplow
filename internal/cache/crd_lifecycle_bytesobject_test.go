// crd_lifecycle_bytesobject_test.go — Ship L (0.30.246) falsifier suite.
//
// Three production-shape falsifiers covering the full CRD lifecycle. Each
// FAILS on Ship-L commit-1 (this file is added BEFORE the fix per
// feedback_falsifier_first_before_ship) and passes after the subsequent
// fix commits (2 = ADD, 3 = UPDATE, 4 = DELETE).
//
//   1. TestCRDAdd_TriggersGroupDiscovery_BytesObject  (§4.2 — flips on commit-2)
//   2. TestCRDUpdate_TriggersGroupDiscovery_BytesObject (§4.4 — flips on commit-3)
//   3. TestCRDDelete_TearsDownInformer_BytesObject    (§4.5 — flips on commit-4)
//   3b.TestCRDDelete_TearsDownInformer_DeletedFinalStateUnknown (§4.5 tombstone
//                                                                — flips on commit-4)
//
// Production delivery shape post-Ship-H5: the customresourcedefinitions
// informer delivers *bytesObject (streaming-listwatch is the default for
// every dynamic informer per watcher.go:1035-1047). The pre-Ship-L bridge
// type-asserts to *unstructured.Unstructured and silently soft-skips;
// these three falsifiers prove that defect class is closed on every
// lifecycle event (ADD, UPDATE, DELETE), not just ADD.

package cache

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
	clientcache "k8s.io/client-go/tools/cache"
)

// versionSpec is the test-fixture mirror of
// apiextensions/v1.CustomResourceDefinitionVersion. Carries the minimal
// (name, served) tuple the bridge derives GVRs from. Local-package only.
type versionSpec struct {
	Name   string
	Served bool
}

// crdBytesObj builds a synthetic CRD *bytesObject carrying (name, spec.group)
// in its raw JSON. Mirrors the production delivery shape from the
// streaming-listwatch path (newBytesObjectFromRaw at bytesobject.go:198):
// the metadata sub-object is decoded into the embedded ObjectMeta; the
// spec body lives verbatim inside `raw` and is decoded on-demand via
// decodeBytesObject.
func crdBytesObj(t *testing.T, name, group string) *bytesObject {
	t.Helper()
	raw := []byte(`{
		"apiVersion":"apiextensions.k8s.io/v1",
		"kind":"CustomResourceDefinition",
		"metadata":{"name":"` + name + `"},
		"spec":{"group":"` + group + `"}
	}`)
	bo, err := newBytesObjectFromRaw(raw)
	if err != nil {
		t.Fatalf("crdBytesObj: newBytesObjectFromRaw failed for name=%s group=%s: %v",
			name, group, err)
	}
	return bo
}

// crdBytesObjMultiVersion builds a CRD *bytesObject with multiple
// spec.versions[] entries. Used by UPDATE + DELETE falsifiers where the
// bridge must derive a GVR per served version from
// (spec.group, spec.names.plural, spec.versions[].name).
func crdBytesObjMultiVersion(t *testing.T, name, group, plural string, versions []versionSpec) *bytesObject {
	t.Helper()
	// Build the spec.versions[] JSON literal. Boolean `served` is the
	// only field we drive; storage/schema/subresources are out of scope.
	versionsJSON := "["
	for i, v := range versions {
		if i > 0 {
			versionsJSON += ","
		}
		served := "false"
		if v.Served {
			served = "true"
		}
		versionsJSON += `{"name":"` + v.Name + `","served":` + served + `,"storage":false}`
	}
	versionsJSON += "]"
	raw := []byte(`{
		"apiVersion":"apiextensions.k8s.io/v1",
		"kind":"CustomResourceDefinition",
		"metadata":{"name":"` + name + `"},
		"spec":{
			"group":"` + group + `",
			"names":{"plural":"` + plural + `","kind":"X"},
			"versions":` + versionsJSON + `
		}
	}`)
	bo, err := newBytesObjectFromRaw(raw)
	if err != nil {
		t.Fatalf("crdBytesObjMultiVersion: newBytesObjectFromRaw failed: %v", err)
	}
	return bo
}

// --- Falsifier 1 (ADD) -------------------------------------------------------

// TestCRDAdd_TriggersGroupDiscovery_BytesObject — Ship L falsifier 1.
//
// FAILS pre-Ship-0.30.246: the AddFunc dispatches the *bytesObject to
// submitCRDDiscoveryEvent, the worker calls triggerCRDDiscovery, which
// type-asserts (*unstructured.Unstructured) — fails — bumps
// discoverySkippedNG and returns. No discovery side-effect fires.
//
// PASSES post-commit-2: triggerCRDDiscovery routes through
// decodeBytesObject, extracts spec.group from the decoded Unstructured,
// adds the group to navDiscoveredGroups, invokes DiscoverGroupResources.
//
// This test is the production-delivery-shape companion to
// TestCRDAdd_TriggersGroupDiscovery. The two together exercise both
// possible AddFunc delivery shapes (stock-informer path and
// streaming-listwatch path).
func TestCRDAdd_TriggersGroupDiscovery_BytesObject(t *testing.T) {
	withCleanCRDDiscovery(t)

	fake := &fakeDiscoveryForCRD{
		group:   "composition.krateo.io",
		version: "v1-2-2",
		res: []metav1.APIResource{
			{Name: "githubscaffoldingwithcompositionpages",
				Kind: "GitHubScaffoldingWithCompositionPages", Namespaced: true},
		},
	}
	installFakeDiscoveryForCRD(t, fake)
	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	crdGVR := CRDGVRForTest()
	rw := newGateWatcher()
	ch := make(chan struct{})
	close(ch)
	rw.syncCh[crdGVR] = ch

	handlers := rw.depEventHandlers(crdGVR)

	// PRODUCTION SHAPE: *bytesObject (not *unstructured.Unstructured).
	handlers.AddFunc(crdBytesObj(t,
		"githubscaffoldingwithcompositionpages.composition.krateo.io",
		"composition.krateo.io"))

	if !WaitCRDDiscoveryProcessedForTest(1, 2000) {
		t.Fatalf("worker did not process the bytesObject CRD ADD within 2s; "+
			"counters: %s", crdDiscoveryStatsString())
	}

	if !IsNavigationDiscoveredGroup("composition.krateo.io") {
		t.Fatalf("Ship L FAIL (ADD): CRD ADD via *bytesObject did NOT add "+
			"composition.krateo.io to navDiscoveredGroups. The discovery "+
			"bridge is silently dropping streaming-shape CRD events. "+
			"Counters: %s", crdDiscoveryStatsString())
	}

	s := CRDDiscoveryStatsSnapshot()
	if s.EventsEnqueued != 1 {
		t.Errorf("EventsEnqueued=%d want 1", s.EventsEnqueued)
	}
	if s.EventsProcessed != 1 {
		t.Errorf("EventsProcessed=%d want 1", s.EventsProcessed)
	}
	if s.DiscoveryInvoked != 1 {
		t.Errorf("DiscoveryInvoked=%d want 1 (DiscoverGroupResources must "+
			"be called for the new group when delivered as *bytesObject)",
			s.DiscoveryInvoked)
	}
	if s.DiscoverySkippedNG != 0 {
		t.Errorf("DiscoverySkippedNG=%d want 0 (bytesObject must be decoded, "+
			"not soft-skipped)", s.DiscoverySkippedNG)
	}
	if s.PanicsRecovered != 0 {
		t.Errorf("PanicsRecovered=%d want 0", s.PanicsRecovered)
	}
}

// --- Falsifier 2 (UPDATE) ----------------------------------------------------

// TestCRDUpdate_TriggersGroupDiscovery_BytesObject — Ship L falsifier 2.
//
// FAILS pre-commit-3: the existing UpdateFunc in depEventHandlers only
// dirty-marks via Deps().OnUpdate; no discovery side-effect fires for
// CRD UPDATE events even if they add a new served version. So
// EventsEnqueued for the CRD UPDATE = 0, DiscoveryInvoked stays at 0,
// composition.krateo.io is not in navDiscoveredGroups.
//
// PASSES post-commit-3: UpdateFunc invokes submitCRDLifecycleEvent
// (kind=Update) → worker routes to triggerCRDDiscovery with
// crdLifecycleUpdate → decode + AddNavigationDiscoveredGroup +
// DiscoverGroupResources fire idempotently for the new spec.
func TestCRDUpdate_TriggersGroupDiscovery_BytesObject(t *testing.T) {
	withCleanCRDDiscovery(t)

	fake := &fakeDiscoveryForCRD{
		group:   "composition.krateo.io",
		version: "v1-2-2",
		res: []metav1.APIResource{
			{Name: "githubscaffoldingwithcompositionpages", Kind: "GHSCP", Namespaced: true},
		},
	}
	installFakeDiscoveryForCRD(t, fake)
	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	crdGVR := CRDGVRForTest()
	rw := newGateWatcher()
	ch := make(chan struct{})
	close(ch)
	rw.syncCh[crdGVR] = ch
	handlers := rw.depEventHandlers(crdGVR)

	// OLD: CRD with single served version.
	// NEW: CRD with an added served version. Post-fix, the new version's
	// GVR should be discovered (DiscoverGroupResources fires for the
	// group; the singleflight gate collapses duplicate concurrent calls
	// — what matters here is the side-effect FIRES at all).
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
		t.Fatalf("Ship L FAIL (UPDATE): worker did not process the CRD UPDATE "+
			"event in 2s — the UpdateFunc never enqueued a CRD-discovery "+
			"event. Counters: %s", crdDiscoveryStatsString())
	}
	if !IsNavigationDiscoveredGroup("composition.krateo.io") {
		t.Fatalf("Ship L FAIL (UPDATE): composition.krateo.io NOT added "+
			"to navDiscoveredGroups via UpdateFunc. Counters: %s",
			crdDiscoveryStatsString())
	}
	s := CRDDiscoveryStatsSnapshot()
	if s.DiscoveryInvoked < 1 {
		t.Fatalf("Ship L FAIL (UPDATE): DiscoveryInvoked=%d want >=1 on UPDATE "+
			"(DiscoverGroupResources must fire so newly-served versions get "+
			"their per-resource informer registered)", s.DiscoveryInvoked)
	}
	if s.DiscoverySkippedNG != 0 {
		t.Errorf("DiscoverySkippedNG=%d want 0 (bytesObject must decode, not skip)",
			s.DiscoverySkippedNG)
	}
	if s.PanicsRecovered != 0 {
		t.Errorf("PanicsRecovered=%d want 0", s.PanicsRecovered)
	}
}

// --- Falsifier 3 (DELETE) ----------------------------------------------------

// TestCRDDelete_TearsDownInformer_BytesObject — Ship L falsifier 3.
//
// FAILS pre-commit-4: the existing DeleteFunc only dirty-marks via
// Deps().OnDelete via the depWatch worker; it does NOT call
// RemoveResourceType for the deleted CRD's served versions. So once a
// composition informer is registered by the ADD path, the DELETE path
// leaves it running indefinitely (the WATCH-404 + leaked goroutine
// problem documented at watcher.go:1300-1308).
//
// PASSES post-commit-4: DeleteFunc invokes submitCRDLifecycleEvent
// (kind=Delete) → worker routes to triggerCRDDelete → decode +
// derive GVRs from spec.versions[].served=true × spec.names.plural ×
// spec.group → rw.RemoveResourceType(gvr) + Deps().OnResourceTypeRemoved(gvr)
// for each served version. The per-resource informer is purged.
//
// This test exercises the full ADD-then-DELETE round-trip: a prior
// AddFunc registers the v1-2-2 informer via the §3 ADD fix (commits
// 2 + 4 must both be applied for this test to pass; it stays RED until
// commit-4 even after commit-2 because the DELETE path is missing).
func TestCRDDelete_TearsDownInformer_BytesObject(t *testing.T) {
	withCleanCRDDiscovery(t)

	fake := &fakeDiscoveryForCRD{
		group:   "composition.krateo.io",
		version: "v1-2-2",
		res: []metav1.APIResource{
			{Name: "githubscaffoldingwithcompositionpages", Kind: "GHSCP", Namespaced: true},
		},
	}
	installFakeDiscoveryForCRD(t, fake)
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

	// Pre-populate the watcher's informers map for the target GVR with a
	// sentinel. RemoveResourceType (watcher.go:1324) checks key existence
	// via `if _, ok := rw.informers[gvr]; !ok` then calls
	// deletePerGVRStateLocked which does `delete(rw.informers, gvr)` — a
	// nil-value entry is sufficient to exercise the teardown path without
	// spinning a real informer. This mimics "an informer was registered
	// earlier" while keeping the test light.
	rw.informers = map[schema.GroupVersionResource]informers.GenericInformer{}
	rw.informers[targetGVR] = nil // sentinel: GVR key present in map.

	// triggerCRDDelete consults Global() to reach the ResourceWatcher
	// it must mutate. Inject the test's rw via SetGlobal so the
	// teardown actually fires against the test fixture, not a nil /
	// production singleton. Cleanup restores nil.
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil) })

	// DELETE fires teardown.
	handlers.DeleteFunc(bo)

	// The worker processes asynchronously; poll for either the teardown
	// to complete or for the counters to indicate the event was handled.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rw.mu.RLock()
		_, stillRegistered := rw.informers[targetGVR]
		rw.mu.RUnlock()
		if !stillRegistered {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rw.mu.RLock()
	_, stillRegistered := rw.informers[targetGVR]
	rw.mu.RUnlock()
	if stillRegistered {
		t.Fatalf("Ship L FAIL (DELETE): per-resource informer for %s was NOT "+
			"torn down by the CRD DELETE event. CRD DELETE handler is missing "+
			"the RemoveResourceType call. Counters: %s",
			targetGVR, crdDiscoveryStatsString())
	}
	s := CRDDiscoveryStatsSnapshot()
	if s.DeletesProcessed < 1 {
		t.Fatalf("Ship L FAIL (DELETE): DeletesProcessed=%d want >=1 "+
			"(the DELETE event must reach triggerCRDDelete)", s.DeletesProcessed)
	}
	if s.DeleteSkippedNG != 0 {
		t.Errorf("DeleteSkippedNG=%d want 0 (well-formed CRD must decode)",
			s.DeleteSkippedNG)
	}
	if s.PanicsRecovered != 0 {
		t.Errorf("PanicsRecovered=%d want 0", s.PanicsRecovered)
	}
}

// TestCRDDelete_TearsDownInformer_DeletedFinalStateUnknown — tombstone
// variant of falsifier 3.
//
// The client-go shared informer wraps the last-known object in
// clientcache.DeletedFinalStateUnknown when the watcher missed the
// explicit DELETE event. The existing DeleteFunc at deps_watch.go:229-231
// already unwraps the tombstone for Deps().OnDelete; Ship L's
// submitCRDLifecycleEvent receives the UNWRAPPED bytesObject so the
// teardown path sees the real CRD spec.
func TestCRDDelete_TearsDownInformer_DeletedFinalStateUnknown(t *testing.T) {
	withCleanCRDDiscovery(t)

	fake := &fakeDiscoveryForCRD{
		group:   "composition.krateo.io",
		version: "v1-2-2",
		res: []metav1.APIResource{
			{Name: "githubscaffoldingwithcompositionpages", Kind: "GHSCP", Namespaced: true},
		},
	}
	installFakeDiscoveryForCRD(t, fake)
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

	tomb := clientcache.DeletedFinalStateUnknown{
		Key: "ghscp.composition.krateo.io",
		Obj: bo,
	}
	handlers.DeleteFunc(tomb)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rw.mu.RLock()
		_, stillRegistered := rw.informers[targetGVR]
		rw.mu.RUnlock()
		if !stillRegistered {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rw.mu.RLock()
	_, stillRegistered := rw.informers[targetGVR]
	rw.mu.RUnlock()
	if stillRegistered {
		t.Fatalf("Ship L FAIL (DELETE tombstone): the DeletedFinalStateUnknown "+
			"unwrap path did NOT tear down the informer for %s. The "+
			"bytesObject inside the tombstone must reach triggerCRDDelete. "+
			"Counters: %s", targetGVR, crdDiscoveryStatsString())
	}
	s := CRDDiscoveryStatsSnapshot()
	if s.DeletesProcessed < 1 {
		t.Fatalf("Ship L FAIL (DELETE tombstone): DeletesProcessed=%d want >=1",
			s.DeletesProcessed)
	}
}
