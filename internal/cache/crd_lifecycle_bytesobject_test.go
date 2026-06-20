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
	"encoding/json"
	"expvar"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestCRDDelete_NoFalseTeardownOfUnrelatedInformer — Ship L+1 backlog test
// gap (#200a): the CRD DELETE bridge must tear down ONLY the GVRs derived
// from the deleted CRD's own spec, and MUST NOT tear down served state for
// resources snowplow holds for OTHER (non-deleted) CRDs.
//
// This is the SOFT (negative-isolation) companion to
// TestCRDDelete_TearsDownInformer_BytesObject. That test asserts the
// POSITIVE: the deleted CRD's informer is gone. This test pins the missing
// NEGATIVE assertion: a DELETE for CRD-A's group must leave CRD-B's
// informer running. Without it, a regression that made triggerCRDDelete
// sweep too broadly (e.g. tearing down by group-prefix, or clearing the
// whole informers map) would pass the positive test while silently
// dropping unrelated served state — exactly the kind of over-broad
// teardown the 0.30.246 bridge's per-served-version derivation is designed
// to prevent (RemoveResourceType acts on the exact GVR only;
// deletePerGVRStateLocked at watcher.go:1436 deletes one map key).
func TestCRDDelete_NoFalseTeardownOfUnrelatedInformer(t *testing.T) {
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

	// The CRD that WILL be deleted declares only targetGVR.
	targetGVR := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1-2-2",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	// A SEPARATE GVR snowplow also serves — a different group entirely, NOT
	// covered by the deleted CRD's spec. Stands in for "served state for an
	// unrelated CRD that must survive the DELETE".
	bystanderGVR := schema.GroupVersionResource{
		Group:    "other.example.io",
		Version:  "v1",
		Resource: "widgets",
	}

	// Pre-populate BOTH informers (sentinel nil values exercise the
	// teardown path without spinning real informers — same technique as
	// TestCRDDelete_TearsDownInformer_BytesObject).
	rw.informers = map[schema.GroupVersionResource]informers.GenericInformer{}
	rw.informers[targetGVR] = nil
	rw.informers[bystanderGVR] = nil

	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil) })

	// DELETE the CRD that declares ONLY targetGVR's group/plural/version.
	bo := crdBytesObjMultiVersion(t, "ghscp.composition.krateo.io",
		"composition.krateo.io", "githubscaffoldingwithcompositionpages",
		[]versionSpec{{Name: "v1-2-2", Served: true}})
	handlers.DeleteFunc(bo)

	// Wait for the deleted CRD's informer to be torn down (the positive
	// half — proves the DELETE actually ran).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rw.mu.RLock()
		_, targetGone := rw.informers[targetGVR]
		rw.mu.RUnlock()
		if !targetGone {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rw.mu.RLock()
	_, targetStillRegistered := rw.informers[targetGVR]
	_, bystanderStillRegistered := rw.informers[bystanderGVR]
	rw.mu.RUnlock()

	if targetStillRegistered {
		t.Fatalf("precondition: deleted CRD's informer %s was NOT torn down — "+
			"the DELETE side-effect did not run. Counters: %s",
			targetGVR, crdDiscoveryStatsString())
	}
	// THE no-false-teardown assertion: the unrelated informer MUST survive.
	if !bystanderStillRegistered {
		t.Fatalf("#200a FAIL: CRD DELETE for %s falsely tore down the unrelated "+
			"informer %s. The teardown must be scoped to the deleted CRD's own "+
			"served GVRs only — never other served state. Counters: %s",
			targetGVR, bystanderGVR, crdDiscoveryStatsString())
	}

	// Exactly one teardown was processed; no skip/panic.
	s := CRDDiscoveryStatsSnapshot()
	if s.DeletesProcessed < 1 {
		t.Fatalf("#200a: DeletesProcessed=%d want >=1", s.DeletesProcessed)
	}
	if s.PanicsRecovered != 0 {
		t.Errorf("#200a: PanicsRecovered=%d want 0", s.PanicsRecovered)
	}
}

// TestProcessEvent_UnknownKind_DefensiveDefault — #200b: the
// crdLifecycleKind switch in processEvent has a defensive default for an
// out-of-range kind. submitCRDLifecycleEvent only ever enqueues a named
// constant, so this branch is structurally unreachable in production; the
// test drives it directly to lock the contract: an unknown kind must NOT
// panic, must NOT fire any ADD/UPDATE/DELETE side-effect, and must still
// count the event as processed (the worker stays alive).
func TestProcessEvent_UnknownKind_DefensiveDefault(t *testing.T) {
	withCleanCRDDiscovery(t)
	// No SA rc / no Global watcher set — if the default branch wrongly fell
	// through to triggerCRDDiscovery or triggerCRDDelete, those would still
	// be soft-safe, so we assert on the counters that DISCRIMINATE the
	// branches instead: discovery/delete counters MUST stay zero.
	c := crdDiscoverySingleton()
	before := CRDDiscoveryStatsSnapshot()

	// An out-of-range crdLifecycleKind (the enum has 0..2; 99 is invalid).
	c.processEvent(crdDiscoveryEvent{obj: nil, kind: crdLifecycleKind(99)})

	after := CRDDiscoveryStatsSnapshot()

	// Processed counter advances exactly once (the event was drained).
	if after.EventsProcessed != before.EventsProcessed+1 {
		t.Fatalf("EventsProcessed = %d, want %d (the default branch must still "+
			"count the event as processed)", after.EventsProcessed, before.EventsProcessed+1)
	}
	// NO side-effect fired: neither the ADD/UPDATE discovery path nor the
	// DELETE path ran.
	if after.DiscoveryInvoked != before.DiscoveryInvoked {
		t.Errorf("DiscoveryInvoked moved (%d→%d) — the unknown-kind default must "+
			"NOT invoke discovery", before.DiscoveryInvoked, after.DiscoveryInvoked)
	}
	if after.DiscoverySkippedNG != before.DiscoverySkippedNG {
		t.Errorf("DiscoverySkippedNG moved (%d→%d) — the unknown-kind default must "+
			"NOT route through triggerCRDDiscovery at all", before.DiscoverySkippedNG, after.DiscoverySkippedNG)
	}
	if after.DeletesProcessed != before.DeletesProcessed ||
		after.DeleteSkippedNG != before.DeleteSkippedNG {
		t.Errorf("DELETE counters moved — the unknown-kind default must NOT route "+
			"through triggerCRDDelete")
	}
	// No panic was recovered (the default is a clean log+return, not a crash).
	if after.PanicsRecovered != before.PanicsRecovered {
		t.Errorf("PanicsRecovered moved (%d→%d) — the default branch must not panic",
			before.PanicsRecovered, after.PanicsRecovered)
	}
}

// --- Commit-5 — expvar /debug/vars exposure ----------------------------------

// TestCRDDiscoveryExpvarHandler — Ship L (0.30.246) closes followup #143.
//
// Validates the second half of the bridge observability surface: the
// counters exposed via crd_discovery_expvar.go's expvar.Publish are
// actually visible through expvar.Handler() (the handler main.go mounts
// at /debug/vars). Without this test a future expvar-registration
// regression in init() would silently break the operator surface — the
// same regression class that hid the Ship 0.30.233 bytesObject defect
// for 13 ships.
//
// Per the published shape (crd_discovery_expvar.go):
//
//	snowplow_crd_discovery → map[string]uint64 with 10 keys:
//	  events_enqueued / events_dropped / events_processed
//	  discovery_invoked / discovery_skipped_ng
//	  deletes_processed / delete_skipped_ng
//	  panics_recovered
//	  schema_relists_fired / schema_unchanged
//	    (followup-crd-schema-widen-informer-relist)
func TestCRDDiscoveryExpvarHandler(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	// CFG-1 (Ship 0.30.163): expvar gauges register only when
	// CACHE_ENABLED is truthy at package init() time. The `go test`
	// process boots with the env unset, so init() returned early.
	// RegisterExpvarForTest re-registers via sync.Once.
	RegisterExpvarForTest()

	// Drive the bridge once so the counters are non-zero — Enqueued
	// for the ADD; Processed + Invoked once the worker drains.
	withCleanCRDDiscovery(t)
	fake := &fakeDiscoveryForCRD{
		group:   "expvar.test.io",
		version: "v1",
		res:     []metav1.APIResource{{Name: "samples", Kind: "Sample"}},
	}
	installFakeDiscoveryForCRD(t, fake)
	SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

	crdGVR := CRDGVRForTest()
	rw := newGateWatcher()
	ch := make(chan struct{})
	close(ch)
	rw.syncCh[crdGVR] = ch
	handlers := rw.depEventHandlers(crdGVR)

	handlers.AddFunc(crdBytesObj(t, "samples.expvar.test.io", "expvar.test.io"))
	if !WaitCRDDiscoveryProcessedForTest(1, 2000) {
		t.Fatalf("expvar precondition: ADD not processed in 2s; counters: %s",
			crdDiscoveryStatsString())
	}

	// Mount expvar.Handler on a test server (the same one-line mount
	// main.go does at /debug/vars).
	mux := http.NewServeMux()
	mux.Handle("/debug/vars", expvar.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/debug/vars")
	if err != nil {
		t.Fatalf("HTTP GET /debug/vars: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/debug/vars status = %d", resp.StatusCode)
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal /debug/vars JSON: %v\nbody: %s", err, string(body))
	}

	rawVal, ok := doc["snowplow_crd_discovery"]
	if !ok {
		t.Fatalf("snowplow_crd_discovery key missing from /debug/vars; "+
			"the Ship-L expvar.Publish never fired. Top-level keys: %v",
			topLevelKeys(doc))
	}
	m, ok := rawVal.(map[string]any)
	if !ok {
		t.Fatalf("snowplow_crd_discovery wrong shape: got %T want map[string]any", rawVal)
	}

	// All 10 keys must be present (incl. the schema-widen relist counters —
	// followup-crd-schema-widen-informer-relist: guards against a future
	// expvar-publisher regression that silently drops the relist surface).
	wantKeys := []string{
		"events_enqueued", "events_dropped", "events_processed",
		"discovery_invoked", "discovery_skipped_ng",
		"deletes_processed", "delete_skipped_ng",
		"panics_recovered",
		"schema_relists_fired", "schema_unchanged",
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("snowplow_crd_discovery missing key %q; have %v", k, mapKeys(m))
		}
	}

	// events_enqueued must be >=1 (the ADD we drove). JSON numbers
	// decode as float64.
	enqueued, ok := m["events_enqueued"].(float64)
	if !ok || uint64(enqueued) < 1 {
		t.Errorf("events_enqueued = %#v; want >=1", m["events_enqueued"])
	}
	discoveryInvoked, ok := m["discovery_invoked"].(float64)
	if !ok || uint64(discoveryInvoked) < 1 {
		t.Errorf("discovery_invoked = %#v; want >=1", m["discovery_invoked"])
	}
	// New Ship L keys must be present even when DELETE didn't run.
	if v, ok := m["deletes_processed"].(float64); !ok || uint64(v) != 0 {
		t.Errorf("deletes_processed = %#v; want 0", m["deletes_processed"])
	}
	if v, ok := m["delete_skipped_ng"].(float64); !ok || uint64(v) != 0 {
		t.Errorf("delete_skipped_ng = %#v; want 0", m["delete_skipped_ng"])
	}
}

// topLevelKeys + mapKeys — small test-only helpers for failure messages.
func topLevelKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
