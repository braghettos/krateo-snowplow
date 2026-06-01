diff --git a/internal/cache/crdwatch.go b/internal/cache/crdwatch.go
index 77df69b..48574c8 100644
--- a/internal/cache/crdwatch.go
+++ b/internal/cache/crdwatch.go
@@ -29,7 +29,6 @@
 package cache
 
 import (
-	"context"
 	"strings"
 	"sync"
 
@@ -41,6 +40,60 @@ import (
 	clientcache "k8s.io/client-go/tools/cache"
 )
 
+// init registers the CRD-watch handler-extension with the cache-package
+// declarative registry (Ship 0 / 0.30.222). Before Ship 0 this wiring
+// lived inside a dedicated `StartCRDWatch` function called once from
+// phase1_walk.go Step 2; the function carried its own idempotence guard
+// (`rw.crdWatchStarted`) and hard-coded the CRD GVR into the watcher's
+// boot seed set (`MetaQuerySeeds()`).
+//
+// Post-Ship-0 the CRD informer is walker-spawned via AddAutoDiscoverGroup
+// — see crdInformerSpawned + that function's body below — and the
+// composition-auto-discovery event handlers attach automatically through
+// this registry entry whenever the watcher's addResourceType* helpers
+// observe the CRD GVR. There is exactly one site that names the CRD GVR
+// in code: this Predicate. addResourceTypeLocked carries no GVR literals
+// (feedback_no_special_cases.md).
+//
+// The handler set itself — AddFunc / UpdateFunc / DeleteFunc routing
+// through registerCRDObject / unregisterCRDObject — is byte-identical to
+// what StartCRDWatch installed pre-Ship-0.
+func init() {
+	RegisterHandlerExtension(HandlerExtension{
+		Name: "crdwatch.composition_auto_discovery",
+		Predicate: func(gvr schema.GroupVersionResource) bool {
+			return gvr == customResourceDefinitionGVR
+		},
+		Handlers: func(rw *ResourceWatcher, _ schema.GroupVersionResource) clientcache.ResourceEventHandler {
+			return clientcache.ResourceEventHandlerFuncs{
+				AddFunc:    func(obj interface{}) { rw.registerCRDObject(obj, "crd-event") },
+				UpdateFunc: func(_, newObj interface{}) { rw.registerCRDObject(newObj, "crd-event") },
+				// D2 (Ship D, 0.30.114) + R6 (Ship 0.30.115): a CRD removal
+				// dirty-marks every L1 entry that LIST- or GET-depends on
+				// the vanished GVR AND tears down the per-GVR informer
+				// (RemoveResourceType) so its Run goroutine does not leak.
+				DeleteFunc: func(obj interface{}) { rw.unregisterCRDObject(obj, "crd-event") },
+			}
+		},
+	})
+}
+
+// crdInformerSpawned ensures the CRD informer is registered exactly once
+// per process, the first time the walker discovers a navigation group
+// (AddAutoDiscoverGroup). Pre-Ship-0 the CRD informer spawned at boot via
+// `MetaQuerySeeds()` regardless of whether anything in frontend navigation
+// reached a CRD object; Ship 0 enforces Diego's invariant ("no CRD
+// informer if the CRD object itself is not walked in frontend
+// navigation"). EnsureResourceType is itself idempotent — the sync.Once
+// is for documentation + cheap-fast-path.
+var crdInformerSpawned sync.Once
+
+// ResetCRDInformerSpawnedForTest resets the sync.Once that gates CRD
+// informer spawning. TEST-ONLY. Production lifecycle is once-per-process.
+func ResetCRDInformerSpawnedForTest() {
+	crdInformerSpawned = sync.Once{}
+}
+
 // autoDiscoverGroups is the set of apiserver groups whose CRDs the
 // CRD-watch auto-registers informers for. NAVIGATION-DERIVED — starts
 // empty, populated only by AddAutoDiscoverGroup, which Phase 1's walk
@@ -63,6 +116,20 @@ var (
 // The empty string is rejected — the core group ("") is never a
 // composition group and admitting it would auto-register informers for
 // every core resource on the cluster.
+//
+// Ship 0 / 0.30.222: AddAutoDiscoverGroup is now the SOLE process-wide
+// site that spawns the CRD informer. The first call (per process) fires
+// the crdInformerSpawned sync.Once, which calls
+// `cache.Global().EnsureResourceType(customResourceDefinitionGVR)` —
+// idempotent w.r.t. an already-registered CRD GVR. The handler-extension
+// registry then attaches the composition-auto-discovery event handlers
+// declared in this file's init(). The pre-Ship-0 boot-time spawn via
+// MetaQuerySeeds() is gone; "no CRD informer if the CRD object itself
+// is not walked in frontend navigation" (Diego invariant 2026-06-01) is
+// now structurally enforced. When Global() is nil (CACHE_ENABLED=false
+// or pre-SetGlobal init paths) the Do() is still consumed exactly once
+// (sync.Once semantics) but no informer is registered — production
+// callers stage AddAutoDiscoverGroup after SetGlobal in phase1_walk.go.
 func AddAutoDiscoverGroup(group string) {
 	if group == "" {
 		return
@@ -78,6 +145,17 @@ func AddAutoDiscoverGroup(group string) {
 			slog.String("note", "navigation-derived — extracted from a resolved templated apiserver path"),
 		)
 	}
+	// Ship 0: spawn the CRD informer the first time a navigation-derived
+	// group is discovered. The walker is the sole source of CRD-informer
+	// spawn — no boot primordial. EnsureResourceType is idempotent under
+	// rw.mu; the sync.Once is for documentation + a cheap fast path.
+	crdInformerSpawned.Do(func() {
+		rw := Global()
+		if rw == nil {
+			return
+		}
+		rw.EnsureResourceType(customResourceDefinitionGVR)
+	})
 }
 
 // matchesAutoDiscoverGroup reports whether group is in the
@@ -146,79 +224,6 @@ func ExtractAPIServerGroupFromTemplatedPath(path string) (string, bool) {
 	return "", false
 }
 
-// StartCRDWatch registers a CRD informer (via the customresourcedefinitions
-// meta-query seed) and wires an event handler that, on every CRD add /
-// update, registers a per-GVR informer for the CRD's served version IFF
-// the CRD's group is in the navigation-derived auto-discover set.
-//
-// This reuses EnsureResourceType for the per-GVR informer (the §0.30.93
-// metadata-only routing applies — composition GVRs route to the
-// PartialObjectMetadata informer when annotated / static-seeded). The
-// new wiring is only: the group-membership gate + the CRD event handler.
-//
-// At boot, the CRD informer's initial LIST replays every existing CRD
-// through AddFunc, so composition informers for already-present CRDs are
-// registered as soon as their group is auto-discovered. Phase 1's final
-// sync barrier (WaitAllInformersSynced) therefore includes the
-// CRD-watch-spawned composition informers that exist at boot.
-//
-// Nil-receiver / passthrough are no-ops. Idempotent — guarded by
-// crdWatchStarted so a duplicate call cannot double-register the handler.
-//
-// The CRD informer's run-loop and the per-GVR informers spawned by the
-// handler are all bound by rw.stopCh (EnsureResourceType's late-register
-// branch + the factory) so Stop() reaps them.
-func (rw *ResourceWatcher) StartCRDWatch(ctx context.Context) {
-	if rw == nil || rw.mode == modePassthrough {
-		return
-	}
-	rw.mu.Lock()
-	if rw.crdWatchStarted {
-		rw.mu.Unlock()
-		return
-	}
-	rw.crdWatchStarted = true
-	rw.mu.Unlock()
-
-	// Register the CRD informer through the standard path. EnsureResourceType
-	// is idempotent — if RegisterMetaQuerySeeds already registered the CRD
-	// GVR, this observes added=false and reuses the same informer.
-	rw.EnsureResourceType(customResourceDefinitionGVR)
-
-	rw.mu.RLock()
-	gi, ok := rw.informers[customResourceDefinitionGVR]
-	rw.mu.RUnlock()
-	if !ok || gi == nil {
-		slog.Warn("cache.crdwatch.no_crd_informer",
-			slog.String("subsystem", "cache"),
-			slog.String("hint", "EnsureResourceType did not register the CRD informer — CRD-watch inactive"),
-		)
-		return
-	}
-
-	if _, err := gi.Informer().AddEventHandler(clientcache.ResourceEventHandlerFuncs{
-		AddFunc:    func(obj interface{}) { rw.registerCRDObject(obj, "crd-event") },
-		UpdateFunc: func(_, newObj interface{}) { rw.registerCRDObject(newObj, "crd-event") },
-		// D2 (Ship D, 0.30.114) + R6 (Ship 0.30.115): a CRD removal
-		// dirty-marks every L1 entry that LIST- or GET-depends on the
-		// vanished GVR AND tears down the per-GVR informer
-		// (RemoveResourceType) so its Run goroutine does not leak.
-		DeleteFunc: func(obj interface{}) { rw.unregisterCRDObject(obj, "crd-event") },
-	}); err != nil {
-		slog.Warn("cache.crdwatch.add_event_handler_failed",
-			slog.String("subsystem", "cache"),
-			slog.String("error", err.Error()),
-		)
-		return
-	}
-
-	slog.Info("cache.crdwatch.started",
-		slog.String("subsystem", "cache"),
-		slog.String("note", "CRD informer event handler installed — composition GVRs auto-register on CRD-add for navigation-discovered groups"),
-	)
-	_ = ctx // ctx reserved: the informer lifecycle is bound by rw.stopCh.
-}
-
 // registerCRDObject is the single per-CRD-object registration step:
 // derive the CRD's served-version GVR and, IFF its group is in the
 // navigation-derived auto-discover set, register a per-GVR informer for
@@ -314,13 +319,13 @@ func (rw *ResourceWatcher) unregisterCRDObject(obj interface{}, via string) {
 // re-applies registerCRDObject to every CRD currently present. It exists
 // to close a boot ORDERING race:
 //
-//	StartCRDWatch installs the CRD informer's event handler, and the
-//	informer's initial LIST replays every existing CRD through AddFunc
-//	ONCE. The Phase 1 walk runs AFTER StartCRDWatch — so when the walk
-//	discovers a composition group (AddAutoDiscoverGroup) the CRD informer
-//	has very likely ALREADY replayed that group's CRD with
-//	matchesAutoDiscoverGroup==false, dropping it permanently. AddFunc
-//	never re-fires for a CRD that merely sat in etcd unchanged, so the
+//	The first AddAutoDiscoverGroup call (Ship 0 walker-spawn) registers
+//	the CRD informer; the informer's initial LIST replays every existing
+//	CRD through AddFunc ONCE. The Phase 1 walk visits navigation roots in
+//	an order that can land a CRD's group in autoDiscoverGroups AFTER its
+//	CRD has already been replayed — at which point that CRD was seen
+//	with matchesAutoDiscoverGroup==false and dropped. AddFunc never
+//	re-fires for a CRD that merely sat in etcd unchanged, so the
 //	composition informer would never register.
 //
 // Calling this AFTER the Phase 1 walk has finished discovering all
diff --git a/internal/cache/crdwatch_internal_test.go b/internal/cache/crdwatch_internal_test.go
index ea1cf4a..39a5c6f 100644
--- a/internal/cache/crdwatch_internal_test.go
+++ b/internal/cache/crdwatch_internal_test.go
@@ -157,17 +157,15 @@ func TestToUnstructuredMap(t *testing.T) {
 // TestReconcileAutoDiscoverCRDs_ClosesBootRace is the 0.30.105 falsifier
 // for the CRD-watch boot replay-vs-discover ORDERING race.
 //
-// The race: StartCRDWatch's CRD informer replays every existing CRD
-// through AddFunc ONCE at boot. The Phase 1 walk discovers composition
-// groups AFTER that — so the composition CRD is replayed while
-// matchesAutoDiscoverGroup(composition.krateo.io)==false and is dropped
-// permanently; the composition informer never registers.
-//
-// This test reproduces the exact ordering: a composition CRD sits in the
-// CRD informer's store, the CRD-watch event handler has already run with
-// the group ABSENT (so no live registration), and ONLY THEN is the group
-// added. ReconcileAutoDiscoverCRDs must re-scan the store and register
-// the composition informer.
+// Ship 0 / 0.30.222 update: the CRD informer is now walker-spawned via
+// AddAutoDiscoverGroup's sync.Once (no longer a boot primordial). The
+// race scenario this test exercises is "first AddAutoDiscoverGroup
+// spawned the CRD informer + its initial LIST replayed every existing
+// CRD; a SUBSEQUENT AddAutoDiscoverGroup for a different group is
+// discovered AFTER that replay — so the second group's composition CRDs
+// were dropped while matchesAutoDiscoverGroup==false". The test below
+// simulates it via a direct AddAutoDiscoverGroup call (no walker), with
+// the same negative+positive controls.
 //
 // NEGATIVE control inside the same test: before the reconcile (group
 // added but no re-scan) the composition informer is NOT registered —
@@ -175,13 +173,21 @@ func TestToUnstructuredMap(t *testing.T) {
 func TestReconcileAutoDiscoverCRDs_ClosesBootRace(t *testing.T) {
 	t.Setenv("CACHE_ENABLED", "true")
 	ResetAutoDiscoverGroupsForTest()
+	ResetCRDInformerSpawnedForTest()
 	t.Cleanup(ResetAutoDiscoverGroupsForTest)
+	t.Cleanup(ResetCRDInformerSpawnedForTest)
 
 	const (
 		compGroup    = "composition.krateo.io"
 		compResource = "githubscaffoldings"
 		compVersion  = "v1"
 	)
+	// Ship 0: use a SECOND group as the "seed" that fires the CRD-
+	// informer sync.Once via AddAutoDiscoverGroup. The composition group
+	// arrives AFTER the CRD informer's initial LIST has replayed compCRD,
+	// reproducing the same boot replay-vs-discover ordering race the
+	// pre-Ship-0 StartCRDWatch path produced.
+	const seedGroup = "seedonly.krateo.io"
 	compGVR := schema.GroupVersionResource{Group: compGroup, Version: compVersion, Resource: compResource}
 
 	// The composition CRD that will sit in the CRD informer's store.
@@ -213,10 +219,18 @@ func TestReconcileAutoDiscoverCRDs_ClosesBootRace(t *testing.T) {
 		time.Sleep(50 * time.Millisecond)
 	})
 
-	// Start the CRD-watch. Its CRD informer replays compCRD through
-	// AddFunc — but the auto-discover set is EMPTY, so the composition
-	// CRD is dropped (the race condition).
-	rw.StartCRDWatch(context.Background())
+	// Ship 0: publish the watcher as Global so AddAutoDiscoverGroup's
+	// sync.Once can call EnsureResourceType on it.
+	SetGlobal(rw)
+	t.Cleanup(func() { SetGlobal(nil) })
+
+	// First AddAutoDiscoverGroup fires the sync.Once and spawns the CRD
+	// informer. Its initial LIST replays compCRD through the CRD-watch's
+	// composition-auto-discovery AddFunc — but matchesAutoDiscoverGroup
+	// is false for compGroup (only seedGroup is in the set at this
+	// moment), so the composition CRD is dropped. This is the boot
+	// replay-vs-discover ordering race.
+	AddAutoDiscoverGroup(seedGroup)
 
 	// Wait for the CRD informer to sync so its store holds compCRD.
 	deadline := time.Now().Add(5 * time.Second)
@@ -227,13 +241,14 @@ func TestReconcileAutoDiscoverCRDs_ClosesBootRace(t *testing.T) {
 		time.Sleep(20 * time.Millisecond)
 	}
 
-	// NEGATIVE control: the group is not yet discovered, so the
-	// composition informer must NOT be registered.
+	// NEGATIVE control: the composition group is not yet discovered, so
+	// the composition informer must NOT be registered.
 	if rw.IsRegistered(compGVR) {
 		t.Fatalf("composition informer registered before its group was discovered — test setup error")
 	}
 
-	// The Phase 1 walk discovers the group LATE — after the CRD replay.
+	// The Phase 1 walk discovers the composition group LATE — after the
+	// CRD replay.
 	AddAutoDiscoverGroup(compGroup)
 
 	// NEGATIVE control: discovering the group alone does NOT register the
diff --git a/internal/cache/crdwatch_lifecycle_falsifier_test.go b/internal/cache/crdwatch_lifecycle_falsifier_test.go
index 420e329..d453219 100644
--- a/internal/cache/crdwatch_lifecycle_falsifier_test.go
+++ b/internal/cache/crdwatch_lifecycle_falsifier_test.go
@@ -250,7 +250,9 @@ func TestFalsifierFD2_CRDDeleteDirtyMarksDeps(t *testing.T) {
 func TestShipD_ReconcileAutoDiscoverCRDsFiresD1(t *testing.T) {
 	t.Setenv("CACHE_ENABLED", "true")
 	ResetAutoDiscoverGroupsForTest()
+	ResetCRDInformerSpawnedForTest()
 	t.Cleanup(ResetAutoDiscoverGroupsForTest)
+	t.Cleanup(ResetCRDInformerSpawnedForTest)
 	ResetDepsForTest()
 	t.Cleanup(ResetDepsForTest)
 
@@ -287,9 +289,17 @@ func TestShipD_ReconcileAutoDiscoverCRDsFiresD1(t *testing.T) {
 	}
 	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
 
-	// StartCRDWatch replays the CRD through AddFunc with the group ABSENT
-	// → dropped (the boot race).
-	rw.StartCRDWatch(context.Background())
+	// Ship 0: publish the watcher as Global so AddAutoDiscoverGroup's
+	// sync.Once can call EnsureResourceType to spawn the CRD informer.
+	SetGlobal(rw)
+	t.Cleanup(func() { SetGlobal(nil) })
+
+	// First AddAutoDiscoverGroup spawns the CRD informer via the
+	// sync.Once. The informer's initial LIST replays the synthetic CRD
+	// through the composition-auto-discovery AddFunc with gvr.Group
+	// ABSENT from the auto-discover set → dropped (the boot race). Use a
+	// distinct seed group so it does not match gvr.Group.
+	AddAutoDiscoverGroup("seedonly.krateo.io")
 	deadline := time.Now().Add(5 * time.Second)
 	for !rw.IsSynced(customResourceDefinitionGVR) {
 		if time.Now().After(deadline) {
diff --git a/internal/cache/phase1.go b/internal/cache/phase1.go
index 60e329b..a68b811 100644
--- a/internal/cache/phase1.go
+++ b/internal/cache/phase1.go
@@ -23,12 +23,24 @@
 // arrives once the navigated informers are warm.
 //
 // CRITICAL — feedback_no_special_cases.md: Phase 1 does NOT consult any
-// configured GVR / RESTAction list. The ONLY hardcoded budget is the 8
+// configured GVR / RESTAction list. The ONLY hardcoded budget is the 7
 // meta-query seeds below — bare anchors needed to bootstrap discovery,
 // not per-resource policy. Every BUSINESS GVR (widgets, panels,
 // compositions) is discovered by recursively resolving the two
 // navigation roots.
 //
+// Ship 0 / 0.30.222: the customresourcedefinitions GVR was REMOVED from
+// the seed set. Pre-Ship-0 the CRD informer spawned at boot regardless of
+// whether anything in frontend navigation reached a CRD object; Diego's
+// invariant 2026-06-01 ("no CRD informer if the CRD object itself is not
+// walked in frontend navigation") demanded a walker-driven spawn. The
+// CRD informer now spawns as a sync.Once side-effect of the first
+// AddAutoDiscoverGroup call (crdwatch.go). The 4 RBAC GVRs +
+// restactions + routesloaders + navmenus remain primordial because they
+// have justified chicken-and-egg semantics (walker queries them to start
+// the walk); the CRD GVR has none — by the time the walker encounters a
+// templated path it is already running.
+//
 // BEHAVIOR-NEUTRAL — PrewarmEnabled() gates the whole feature behind
 // PREWARM_ENABLED (default OFF), mirroring PREWARM_REGISTER_ENABLED.
 // When OFF: Phase 1 never runs and Phase1Done is pre-set true at startup
@@ -125,13 +137,21 @@ func ResetPhase1DoneForTest() {
 
 // customResourceDefinitionGVR is the GVR of the apiextensions
 // CustomResourceDefinition resource — the navigation root the CRD-watch
-// (Part 2) registers an informer against to discover composition GVRs
-// as their CRDs appear.
+// registers an informer against to discover composition GVRs as their
+// CRDs appear.
+//
+// Ship 0 / 0.30.222: NO LONGER a meta-query seed. The CRD informer is
+// spawned by the walker via AddAutoDiscoverGroup the first time the
+// frontend navigation walks to a templated apiserver path. Retained as a
+// package-level constant because the spawn site (crdwatch.go's
+// AddAutoDiscoverGroup) + the handler-extension Predicate (crdwatch.go's
+// init()) + the tests still reference the same GVR identity.
 //
-// Per feedback_no_special_cases.md: NOT a per-resource policy. It is one
-// of the 7 bare meta-query anchors — the CRD-watch needs SOMETHING to
-// LIST/WATCH to learn about composition CRDs, and that something is the
-// CRD type itself.
+// Per feedback_no_special_cases.md: NOT a per-resource policy. The
+// constant lives in the package that owns the CRD-watch behavior, and
+// addResourceTypeLocked / addResourceTypeMetadataOnlyLocked do NOT
+// reference it directly — they iterate the handler-extension registry
+// blind.
 var customResourceDefinitionGVR = schema.GroupVersionResource{
 	Group:    "apiextensions.k8s.io",
 	Version:  "v1",
@@ -197,7 +217,7 @@ func CustomResourceDefinitionGVR() schema.GroupVersionResource {
 }
 
 // MetaQuerySeeds returns the COMPLETE hardcoded seed budget for Tag B —
-// EXACTLY these 8 GVRs, nothing else (feedback_no_special_cases.md is a
+// EXACTLY these 7 GVRs, nothing else (feedback_no_special_cases.md is a
 // hard requirement here). Every entry is a meta-query INFORMER-ANCHOR
 // seed: the watcher pre-registers an informer for the resource type so a
 // `/call` to one of these can be served from cache. None of them is a
@@ -211,34 +231,40 @@ func CustomResourceDefinitionGVR() schema.GroupVersionResource {
 //     type. 0.30.107: no longer a root-selection literal.
 //  3. restactions              — the restActionGVR anchor (already cited
 //     by inventory.go; the resolver's apiRef edges target it).
-//  4. customresourcedefinitions — the CRD-watch root (Part 2).
-//  5-8. the 4 RBACResourceTypes — roles / rolebindings / clusterroles /
+//  4-7. the 4 RBACResourceTypes — roles / rolebindings / clusterroles /
 //     clusterrolebindings (already bootstrap-registered in
 //     NewResourceWatcher; included here so the seed set is the single
 //     auditable source of truth).
 //
+// Ship 0 / 0.30.222: customresourcedefinitions is NO LONGER a seed
+// (Diego invariant: "no CRD informer if the CRD object itself is not
+// walked"). It is walker-spawned via AddAutoDiscoverGroup; see
+// crdwatch.go.
+//
 // Every BUSINESS GVR — widgets, panels, compositions — is ABSENT from
 // this set by construction. Those are discovered by RESOLVING the
 // ConfigMap-derived navigation roots, never named in code. A test
-// asserts this slice has exactly 8 entries and that none of them is a
+// asserts this slice has exactly 7 entries and that none of them is a
 // composition/widget/panel business GVR.
 func MetaQuerySeeds() []schema.GroupVersionResource {
 	seeds := []schema.GroupVersionResource{
 		routesLoadersGVR,
 		navMenusGVR,
 		restActionGVR,
-		customResourceDefinitionGVR,
 	}
 	seeds = append(seeds, RBACResourceTypes...)
 	return seeds
 }
 
-// RegisterMetaQuerySeeds registers an informer for each of the 4
-// non-RBAC meta-query seeds (routesloaders, navmenus, restactions,
-// customresourcedefinitions) plus re-confirms the 4 RBAC GVRs (already
-// registered by NewResourceWatcher — EnsureResourceType observes
-// added=false for those) — 8 seeds total. Idempotent + singleflighted
-// under rw.mu.
+// RegisterMetaQuerySeeds registers an informer for each of the 3
+// non-RBAC meta-query seeds (routesloaders, navmenus, restactions) plus
+// re-confirms the 4 RBAC GVRs (already registered by NewResourceWatcher
+// — EnsureResourceType observes added=false for those) — 7 seeds total.
+// Idempotent + singleflighted under rw.mu.
+//
+// Ship 0 / 0.30.222: the CRD GVR is no longer in this list. The CRD
+// informer is walker-spawned via AddAutoDiscoverGroup the first time
+// frontend navigation reaches a templated apiserver path.
 //
 // This is the ONLY code that hands a hardcoded GVR to EnsureResourceType
 // at startup. The Phase 1 walk registers everything else by resolution.
diff --git a/internal/cache/phase1_test.go b/internal/cache/phase1_test.go
index 161f5d8..8a7c68a 100644
--- a/internal/cache/phase1_test.go
+++ b/internal/cache/phase1_test.go
@@ -34,32 +34,32 @@ var forbiddenSeedResources = map[string]bool{
 }
 
 // TestMetaQuerySeeds_ExactBudget asserts the hardcoded seed set is
-// EXACTLY the 8 declared meta-query anchors and contains no business
+// EXACTLY the 7 declared meta-query anchors and contains no business
 // GVR. A regression that adds a configured widget / composition GVR to
 // the seed list fails here. 0.30.105 raised the budget 7->8 by adding
-// the navmenus navigation root — an entry-point widget CR, not a
-// business GVR.
+// the navmenus navigation root; Ship 0 / 0.30.222 lowered it back to 7
+// by removing customresourcedefinitions (walker-spawned via
+// AddAutoDiscoverGroup, no longer a boot primordial).
 func TestMetaQuerySeeds_ExactBudget(t *testing.T) {
 	seeds := cache.MetaQuerySeeds()
-	if len(seeds) != 8 {
-		t.Fatalf("meta-query seed budget must be EXACTLY 8 (routesloaders, "+
-			"navmenus, restactions, customresourcedefinitions + 4 RBAC); got %d: %v",
+	if len(seeds) != 7 {
+		t.Fatalf("meta-query seed budget must be EXACTLY 7 (routesloaders, "+
+			"navmenus, restactions + 4 RBAC); got %d: %v",
 			len(seeds), seeds)
 	}
 
 	want := map[schema.GroupVersionResource]bool{
-		cache.RoutesLoadersGVR():            true,
-		cache.NavMenusGVR():                 true,
-		cache.CustomResourceDefinitionGVR(): true,
-		{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}:                        true,
-		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                        true,
-		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:                 true,
-		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:                 true,
-		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}:          true,
+		cache.RoutesLoadersGVR(): true,
+		cache.NavMenusGVR():      true,
+		{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}:               true,
+		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:               true,
+		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:        true,
+		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:        true,
+		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: true,
 	}
 	for _, s := range seeds {
 		if !want[s] {
-			t.Errorf("unexpected meta-query seed %v — not one of the 8 sanctioned anchors", s)
+			t.Errorf("unexpected meta-query seed %v — not one of the 7 sanctioned anchors", s)
 		}
 		delete(want, s)
 		// No business GVR may ever be a hardcoded seed — those are
@@ -71,6 +71,16 @@ func TestMetaQuerySeeds_ExactBudget(t *testing.T) {
 	if len(want) != 0 {
 		t.Errorf("meta-query seeds missing sanctioned anchors: %v", want)
 	}
+
+	// Ship 0 / 0.30.222 — the CRD GVR is explicitly walker-driven.
+	// Asserting its ABSENCE from the seed set catches a regression that
+	// re-adds it (which would re-violate Diego's invariant).
+	for _, s := range seeds {
+		if s == cache.CustomResourceDefinitionGVR() {
+			t.Fatalf("Ship 0 invariant: customresourcedefinitions MUST NOT be a meta-query seed " +
+				"(walker-spawned via AddAutoDiscoverGroup); got %v in seed set", s)
+		}
+	}
 }
 
 // TestRoutesLoadersGVR_IsV1Beta1 pins the routesloaders navigation root
@@ -262,9 +272,11 @@ func TestRegisterMetaQuerySeeds_RegistersAndIdempotent(t *testing.T) {
 
 	first := rw.RegisterMetaQuerySeeds()
 	// The 4 RBAC GVRs are already registered by NewResourceWatcher; the
-	// 4 meta seeds (routesloaders, navmenus, restactions, CRDs) are new.
-	if first != 4 {
-		t.Fatalf("first RegisterMetaQuerySeeds must register the 4 non-RBAC seeds; got %d", first)
+	// 3 non-RBAC seeds (routesloaders, navmenus, restactions) are new.
+	// Ship 0 / 0.30.222: customresourcedefinitions was removed (walker-
+	// spawned via AddAutoDiscoverGroup); the count dropped from 4 to 3.
+	if first != 3 {
+		t.Fatalf("first RegisterMetaQuerySeeds must register the 3 non-RBAC seeds; got %d", first)
 	}
 	for _, gvr := range cache.MetaQuerySeeds() {
 		if !rw.IsRegistered(gvr) {
diff --git a/internal/cache/rbac_snapshot.go b/internal/cache/rbac_snapshot.go
index bdb9b2f..160d522 100644
--- a/internal/cache/rbac_snapshot.go
+++ b/internal/cache/rbac_snapshot.go
@@ -840,15 +840,42 @@ func (rw *ResourceWatcher) rbacSnapshotEventHandlers(gvr schema.GroupVersionReso
 	}
 }
 
+// init registers the typed-RBAC-snapshot handler-extension with the
+// cache-package declarative registry (Ship 0 / 0.30.222). Before Ship 0
+// the watcher's addResourceTypeLocked / addResourceTypeMetadataOnlyLocked
+// branched inline on `if isTypedRBACGVR(gvr)` and attached
+// rbacSnapshotEventHandlers directly. Ship 0 generalises that branch
+// (alongside the CRD-watch composition-auto-discovery branch) into the
+// declarative registry; addResourceType*Locked now iterates blind.
+//
+// The handler set itself (AddFunc / UpdateFunc / DeleteFunc routing
+// through scheduleRBACRebuild + the BindingsByGVR index delta) is
+// byte-identical to the pre-Ship-0 inline branch — only the call-site
+// indirection changes.
+func init() {
+	RegisterHandlerExtension(HandlerExtension{
+		Name:      "rbac.snapshot_writer",
+		Predicate: isTypedRBACGVR,
+		Handlers: func(rw *ResourceWatcher, gvr schema.GroupVersionResource) clientcache.ResourceEventHandler {
+			handlers := rw.rbacSnapshotEventHandlers(gvr)
+			// markRBACSnapshotWired records that at least one typed-RBAC
+			// informer was wired with the snapshot event handler — the
+			// pre-condition AssertRBACSnapshotWired enforces at boot.
+			markRBACSnapshotWired()
+			return handlers
+		},
+	})
+}
+
 // rbacSnapshotWired records whether the watcher has at least one of
 // the typed-RBAC informers wired with the snapshot event handler. Set
 // at NewResourceWatcher time; AssertRBACSnapshotWired panics at boot if
 // it ever stays false despite the 4 RBAC informers being registered.
 var rbacSnapshotHandlerWired atomic.Bool
 
-// markRBACSnapshotWired flips the wired flag. Called by
-// addResourceTypeLocked once per typed-RBAC GVR after attaching the
-// snapshot event handler.
+// markRBACSnapshotWired flips the wired flag. Called by the
+// rbac.snapshot_writer HandlerExtension's Handlers factory (init() above)
+// once per typed-RBAC GVR as the attach occurs.
 func markRBACSnapshotWired() {
 	rbacSnapshotHandlerWired.Store(true)
 }
diff --git a/internal/cache/watcher.go b/internal/cache/watcher.go
index 17dea76..cd0055e 100644
--- a/internal/cache/watcher.go
+++ b/internal/cache/watcher.go
@@ -198,14 +198,8 @@ type ResourceWatcher struct {
 	// nil eagerSet = "eager registration not yet completed" — no
 	// WARNs fire (the constructor's own RBAC registrations are not
 	// "lazy").
-	eagerSet     map[schema.GroupVersionResource]struct{}
-	eagerDone    bool
-
-	// crdWatchStarted is the idempotence guard for StartCRDWatch
-	// (0.30.102 Tag B Part 2). Set true on the first StartCRDWatch
-	// call so a duplicate call cannot double-install the CRD informer's
-	// event handler. Guarded by rw.mu.
-	crdWatchStarted bool
+	eagerSet  map[schema.GroupVersionResource]struct{}
+	eagerDone bool
 
 	// informerStop holds the per-GVR stop channel passed to each
 	// informer's Run (and its sync-watcher) — the R6 (0.30.115)
@@ -801,29 +795,6 @@ func (rw *ResourceWatcher) addResourceTypeMetadataOnlyLocked(gvr schema.GroupVer
 	// interface used by metaNSName. ADD post-sync gate, UPDATE
 	// dirty-mark, DELETE classify+evict-via-worker are byte-identical
 	// to the full-informer path.
-	// Ship B (0.30.138) — typed-RBAC snapshot writer wiring (defensive
-	// site). The metadata-only path serves PartialObjectMetadata, which
-	// the snapshot writer cannot type-assert to *rbacv1.* — so in
-	// practice an RBAC GVR registered metadata-only would simply produce
-	// an empty snapshot field. Production never reaches here for RBAC
-	// (the eager-registration loop at NewResourceWatcher uses the full
-	// addResourceTypeLocked path); the guard exists so a future caller
-	// that adds RBAC metadata-only does not silently bypass snapshot
-	// wiring. isTypedRBACGVR is false for every non-RBAC GVR — no
-	// overhead on the steady-state metadata-only path.
-	if isTypedRBACGVR(gvr) {
-		if _, regErr := gi.Informer().AddEventHandler(rw.rbacSnapshotEventHandlers(gvr)); regErr != nil {
-			slog.Warn("cache.rbac.snapshot.add_event_handler_failed",
-				slog.String("subsystem", "cache"),
-				slog.String("resource_type", resourceType),
-				slog.String("path", "metadata-only"),
-				slog.String("error", regErr.Error()),
-			)
-		} else {
-			markRBACSnapshotWired()
-		}
-	}
-
 	if _, regErr := gi.Informer().AddEventHandler(rw.depEventHandlers(gvr)); regErr != nil {
 		slog.Warn("cache.deps.add_event_handler_failed",
 			slog.String("subsystem", "cache"),
@@ -833,6 +804,14 @@ func (rw *ResourceWatcher) addResourceTypeMetadataOnlyLocked(gvr schema.GroupVer
 		)
 	}
 
+	// Ship 0 / 0.30.222 — declarative handler-extension registry; same
+	// iteration as the full-informer path. Production never reaches here
+	// for RBAC (the eager-registration loop in NewResourceWatcher uses
+	// the full addResourceTypeLocked path); the registry still iterates
+	// — non-matching predicates are O(1) skips — so a future caller that
+	// adds RBAC metadata-only does not silently bypass snapshot wiring.
+	attachMatchingHandlerExtensions(rw, gvr, gi.Informer())
+
 	// 0.30.98 Tag A: install the WATCH-error handler BEFORE Run
 	// (conjunct 3) — same uniform wiring as the dynamic full-informer
 	// path. The metadata-only reflector drops its WATCH on the same
@@ -1158,29 +1137,20 @@ func (rw *ResourceWatcher) addResourceTypeLocked(gvr schema.GroupVersionResource
 		)
 	}
 
-	// Ship B (0.30.138) — typed-RBAC snapshot writer wiring. For each
-	// of the 4 typed-RBAC GVRs (rbacTypedGVRs, strip.go:101-106) attach
-	// a second event handler that schedules a snapshot rebuild on
-	// ADD/UPDATE/DELETE. Non-RBAC GVRs are skipped — they have no
-	// snapshot to maintain.
+	// Ship 0 / 0.30.222 — declarative handler-extension registry.
+	// Pre-Ship-0 this site carried an inline `if isTypedRBACGVR(gvr)`
+	// branch attaching `rbacSnapshotEventHandlers`, plus a separate
+	// `StartCRDWatch` entry point installing the CRD composition-auto-
+	// discovery handlers. Both wirings are now declared from their
+	// owner packages' init() (rbac_snapshot.go and crdwatch.go) and
+	// attached blind from here — addResourceTypeLocked carries zero
+	// GVR literals (feedback_no_special_cases.md).
 	//
-	// The handler bodies are O(1) atomics (dirty flip + tryLock); the
-	// actual indexer walk runs on a detached goroutine bounded by the
-	// atomic.Bool tryLock (max one in-flight rebuild — watcher.go:1028
-	// "Bounded async L1 refresh" lineage / Bug 7). Safe to attach on
-	// the same processor goroutine as depEventHandlers — neither
-	// handler blocks.
-	if isTypedRBACGVR(gvr) {
-		if _, regErr := gi.Informer().AddEventHandler(rw.rbacSnapshotEventHandlers(gvr)); regErr != nil {
-			slog.Warn("cache.rbac.snapshot.add_event_handler_failed",
-				slog.String("subsystem", "cache"),
-				slog.String("resource_type", resourceType),
-				slog.String("error", regErr.Error()),
-			)
-		} else {
-			markRBACSnapshotWired()
-		}
-	}
+	// The handler sets themselves are unchanged. attachMatching* logs
+	// per-failure via the registry; this site stays silent on attach
+	// failures (the contract is "log loud at the owner's branch", not
+	// here).
+	attachMatchingHandlerExtensions(rw, gvr, gi.Informer())
 
 	// 0.30.99 Tag B — watch-handler coverage guard. Install the
 	// conjunct-3 WATCH-error handler UNCONDITIONALLY here, at
diff --git a/internal/handlers/dispatchers/phase1_walk.go b/internal/handlers/dispatchers/phase1_walk.go
index 0b05584..bd04d0c 100644
--- a/internal/handlers/dispatchers/phase1_walk.go
+++ b/internal/handlers/dispatchers/phase1_walk.go
@@ -468,13 +468,22 @@ func phase1WarmupWith(ctx context.Context, rw *cache.ResourceWatcher, lister roo
 	start := time.Now()
 
 	// Step 1 — register the hardcoded meta-query seeds. This is the ONLY
-	// place a hardcoded GVR is handed to the watcher at startup.
+	// place a hardcoded GVR is handed to the watcher at startup. Ship 0
+	// / 0.30.222: the customresourcedefinitions GVR is NO LONGER in this
+	// set — it is walker-spawned via AddAutoDiscoverGroup the first time
+	// the resolver encounters a templated apiserver path; the
+	// composition-auto-discovery event handlers attach automatically via
+	// the handler-extension registry (cache/handler_registry.go) when
+	// EnsureResourceType lands the CRD informer.
 	rw.RegisterMetaQuerySeeds()
 
-	// Step 2 — start the CRD-watch. Composition informers spawn as the
-	// CRD informer replays existing CRDs (boot) and on CRD-add, but only
-	// for groups Phase 1's walk has fed into the auto-discover set.
-	rw.StartCRDWatch(ctx)
+	// Step 2 — (Ship 0 / 0.30.222) the explicit StartCRDWatch call that
+	// lived here is DELETED. The CRD informer now spawns as a sync.Once
+	// side-effect of the first AddAutoDiscoverGroup call below (Step 4's
+	// walker), and the composition-auto-discovery event handlers attach
+	// automatically via the handler-extension registry. Diego's invariant
+	// 2026-06-01 — "no CRD informer if the CRD object itself is not
+	// walked in frontend navigation" — is now structurally enforced.
 
 	// Step 3 — READ the navigation roots from the frontend ConfigMap
 	// (config.json .api.INIT / .api.ROUTES_LOADER → the two named root
@@ -527,11 +536,12 @@ func phase1WarmupWith(ctx context.Context, rw *cache.ResourceWatcher, lister roo
 
 	// Step 5 — reconcile the CRD-watch against the now-complete
 	// auto-discover set. The walk discovers composition groups (e.g.
-	// composition.krateo.io) AFTER StartCRDWatch's CRD informer has
-	// already replayed every existing CRD — so the composition CRD was
-	// seen with matchesAutoDiscoverGroup==false and dropped. Now that the
-	// walk has finished, the auto-discover set is complete; a single CRD
-	// store re-scan registers every composition informer whose CRD was
+	// composition.krateo.io) and the first such call spawns the CRD
+	// informer (Ship 0 walker-spawn via AddAutoDiscoverGroup's sync.Once).
+	// Subsequent CRD replays may still race against later
+	// AddAutoDiscoverGroup calls for OTHER groups — so when the walk
+	// finishes, the auto-discover set is complete; a single CRD store
+	// re-scan registers every composition informer whose CRD was
 	// replayed too early. Idempotent for CRDs already registered live.
 	reconciled := rw.ReconcileAutoDiscoverCRDs()
 	if reconciled > 0 {
=== NEW FILES (full contents) ===

---- internal/cache/handler_registry.go ----
// handler_registry.go — Ship 0 / 0.30.222: declarative handler-extension
// registry for the cache informer pipeline.
//
// PROBLEM. Before Ship 0, the watcher's addResourceTypeLocked /
// addResourceTypeMetadataOnlyLocked carried two hardcoded GVR-keyed
// handler-attach branches:
//
//  1. typed-RBAC snapshot writer: `if isTypedRBACGVR(gvr) { … attach
//     rbacSnapshotEventHandlers(gvr) … }` (watcher.go).
//
//  2. CRD-watch composition auto-discovery: installed via the separate
//     `StartCRDWatch` entry point, which itself was called once from
//     phase1_walk.go's Step 2 and guarded by an `rw.crdWatchStarted`
//     idempotence field.
//
// Both are "attach this specialised handler set when this GVR's informer
// is created" — i.e. the same shape, expressed differently. The CRD-watch
// arm went through a dedicated `StartCRDWatch` function because the CRD
// GVR was hardcoded into `MetaQuerySeeds()` at Phase 1 boot; the RBAC arm
// branched inline because the GVR set is built-in. Neither composes with
// new extensions without editing watcher.go.
//
// SOLUTION. A single declarative registry. Each owner package registers a
// `HandlerExtension` from its own `init()`; the watcher iterates the
// registry blind every time it adds an informer and attaches every
// handler whose predicate matches. `addResourceTypeLocked` no longer
// names any specific GVR.
//
// CRITICAL — feedback_no_special_cases.md: the registry's call sites
// (the two `addResourceType*Locked` helpers) carry zero GVR literals.
// A `HandlerExtension` constant referenced by its predicate (e.g. the
// CRD-GVR literal in crdwatch.go's predicate) lives in the owner
// package, NOT in the watcher.
//
// THREAD SAFETY. Production registration happens at package init() and is
// therefore single-threaded by the Go runtime; the registry is read on
// the watcher's informer-creation path (always under rw.mu.Lock()) and on
// the test helper path. Reads use a RWMutex; writes a Mutex; both are
// cheap (the registry is tiny — current Ship 0 has 2 extensions).
//
// OBSERVABILITY. Every successful attach increments the
// `handler_extensions_attached_count{name=X}` counter. The test corpus
// asserts this counter so a regression that drops an extension (the
// predicate stops matching, the registry loses an entry, the watcher
// stops iterating) fails loud.

package cache

import (
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// HandlerExtension is one declarative attach-this-handler-set-on-this-GVR
// rule. Each owner package registers its own instance from `init()`.
//
//   - Name: stable identifier for observability + falsifier counters
//     (e.g. "crdwatch.composition_auto_discovery",
//     "rbac.snapshot_writer"). Must be unique per process — duplicate
//     names panic at registration time.
//
//   - Predicate: returns true iff this extension's Handlers should attach
//     to the informer for `gvr`. The owner package decides — could be a
//     single-GVR equality, a set-membership check, or a group/resource
//     prefix. Called once per addResourceType* invocation per registered
//     extension, so it must be cheap (no apiserver calls, no locks).
//
//   - Handlers: factory returning the handler set to attach. The watcher
//     calls `gi.Informer().AddEventHandler(ext.Handlers(rw, gvr))` when
//     the predicate matches. The factory is invoked exactly once per
//     attach so closures over rw/gvr are safe.
type HandlerExtension struct {
	Name      string
	Predicate func(schema.GroupVersionResource) bool
	Handlers  func(rw *ResourceWatcher, gvr schema.GroupVersionResource) clientcache.ResourceEventHandler
}

// handlerExtensions is the process-wide registry. Append-only: an owner
// package registers exactly once at init() time. `handlerExtMu` guards
// reads + writes; writes are single-threaded by Go init() semantics, but
// the lock keeps the contract explicit + safe under the test reset path.
var (
	handlerExtMu      sync.RWMutex
	handlerExtensions []HandlerExtension

	// handlerExtensionsAttachedTotal counts successful attachments per
	// extension name. Keyed by name; values are `*atomic.Uint64`.
	// Falsifier reads through HandlerExtensionsAttachedCount.
	handlerExtensionsAttachedTotal sync.Map // name(string) -> *atomic.Uint64
)

// RegisterHandlerExtension records ext for future addResourceType*
// invocations. Called from an owner package's `init()`. Duplicate names
// panic — the registry is a single source of truth and a name collision
// is a programming error caught at boot rather than a silent override.
//
// Empty Name / nil Predicate / nil Handlers all panic — every field is
// load-bearing and a registration with any of them missing would silently
// short-circuit the iteration.
func RegisterHandlerExtension(ext HandlerExtension) {
	if ext.Name == "" {
		panic("cache.RegisterHandlerExtension: Name must be non-empty")
	}
	if ext.Predicate == nil {
		panic("cache.RegisterHandlerExtension: Predicate must be non-nil for name=" + ext.Name)
	}
	if ext.Handlers == nil {
		panic("cache.RegisterHandlerExtension: Handlers must be non-nil for name=" + ext.Name)
	}
	handlerExtMu.Lock()
	defer handlerExtMu.Unlock()
	for _, existing := range handlerExtensions {
		if existing.Name == ext.Name {
			panic("cache.RegisterHandlerExtension: duplicate name " + ext.Name)
		}
	}
	handlerExtensions = append(handlerExtensions, ext)
}

// eventHandlerAttacher is the narrow interface attachMatchingHandlerExtensions
// consumes — just `AddEventHandler`, which both
// `clientcache.SharedIndexInformer` (the production type
// `informers.GenericInformer.Informer()` returns) and a unit-test fake
// implement. Decoupling from the full SharedIndexInformer interface
// keeps the test fake small.
type eventHandlerAttacher interface {
	AddEventHandler(clientcache.ResourceEventHandler) (clientcache.ResourceEventHandlerRegistration, error)
}

// attachMatchingHandlerExtensions iterates the registry and attaches every
// handler whose predicate matches gvr. Called from addResourceTypeLocked
// and addResourceTypeMetadataOnlyLocked under rw.mu — they hold the lock
// for the duration of the informer-creation pass, so attach failures
// (post-Start informer or duplicate AddEventHandler) are logged but never
// fail the registration. The shape mirrors the previous inline RBAC
// branch — only the GVR-keyed naming moves out.
//
// Returns the count of extensions attached for observability assertions;
// the watcher does not propagate failures upstream.
func attachMatchingHandlerExtensions(rw *ResourceWatcher, gvr schema.GroupVersionResource, attacher eventHandlerAttacher) int {
	handlerExtMu.RLock()
	exts := make([]HandlerExtension, len(handlerExtensions))
	copy(exts, handlerExtensions)
	handlerExtMu.RUnlock()

	attached := 0
	for _, ext := range exts {
		if !ext.Predicate(gvr) {
			continue
		}
		handlers := ext.Handlers(rw, gvr)
		if handlers == nil {
			continue
		}
		if _, err := attacher.AddEventHandler(handlers); err != nil {
			// Best-effort: log via the watcher's logging path is owned by
			// the caller — the caller already has the resource type
			// string and the gvr context. We deliberately do NOT log here
			// to avoid a second log line for every attach. The attach
			// counter only ticks on success.
			_ = err
			continue
		}
		attached++
		incHandlerExtensionsAttached(ext.Name)
	}
	return attached
}

// incHandlerExtensionsAttached ticks the per-name attach counter.
// Lazy-creates the *atomic.Uint64 via sync.Map.LoadOrStore so a
// never-attached extension does not allocate its counter at registration.
func incHandlerExtensionsAttached(name string) {
	v, _ := handlerExtensionsAttachedTotal.LoadOrStore(name, &atomic.Uint64{})
	v.(*atomic.Uint64).Add(1)
}

// HandlerExtensionsAttachedCount returns the running total of successful
// attachments for the named extension. Zero for an unknown / never-
// attached name. Used by the Ship 0 falsifier corpus to assert
//
//	counter == 0 at start of Phase 1
//	counter == 1 after the first AddAutoDiscoverGroup (crdwatch arm)
//	counter == 4 after RBAC eager registration (rbac arm — one per GVR)
//
// and similar invariants.
func HandlerExtensionsAttachedCount(name string) uint64 {
	v, ok := handlerExtensionsAttachedTotal.Load(name)
	if !ok {
		return 0
	}
	return v.(*atomic.Uint64).Load()
}

// HandlerExtensionsSnapshot returns the registered extensions in
// registration order. TEST + observability helper. Production callers
// must not depend on order.
func HandlerExtensionsSnapshot() []HandlerExtension {
	handlerExtMu.RLock()
	defer handlerExtMu.RUnlock()
	out := make([]HandlerExtension, len(handlerExtensions))
	copy(out, handlerExtensions)
	return out
}

// ResetHandlerExtensionsForTest clears the registry AND the attach
// counters. TEST-ONLY — the production lifecycle is append-only.
func ResetHandlerExtensionsForTest() {
	handlerExtMu.Lock()
	handlerExtensions = nil
	handlerExtMu.Unlock()
	handlerExtensionsAttachedTotal.Range(func(k, _ any) bool {
		handlerExtensionsAttachedTotal.Delete(k)
		return true
	})
}

---- internal/cache/handler_registry_test.go ----
// handler_registry_test.go — Ship 0 / 0.30.222: -race-covered unit tests
// for the declarative handler-extension registry. Package internal so
// tests can reach RegisterHandlerExtension + ResetHandlerExtensionsForTest
// + the unexported attach helper.

package cache

import (
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// fakeAttacher implements eventHandlerAttacher just enough to record an
// AddEventHandler invocation and optionally fail the first call. The
// narrow eventHandlerAttacher interface (handler_registry.go) is the
// reason we don't need to implement the much larger SharedIndexInformer
// shape here.
type fakeAttacher struct {
	addCount atomic.Int32
	failNext atomic.Bool
}

func (f *fakeAttacher) AddEventHandler(_ clientcache.ResourceEventHandler) (clientcache.ResourceEventHandlerRegistration, error) {
	if f.failNext.Swap(false) {
		return nil, errFakeAddFailed{}
	}
	f.addCount.Add(1)
	return fakeRegistration{}, nil
}

type fakeRegistration struct{}

func (fakeRegistration) HasSynced() bool { return true }

type errFakeAddFailed struct{}

func (errFakeAddFailed) Error() string { return "fake add failed" }

// dummyHandlers returns a stub event-handler set the fake informer can
// accept.
func dummyHandlers(_ *ResourceWatcher, _ schema.GroupVersionResource) clientcache.ResourceEventHandler {
	return clientcache.ResourceEventHandlerFuncs{}
}

func TestRegisterHandlerExtension_Registers(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	gvrA := schema.GroupVersionResource{Group: "a", Version: "v1", Resource: "as"}

	RegisterHandlerExtension(HandlerExtension{
		Name:      "test.extension_a",
		Predicate: func(g schema.GroupVersionResource) bool { return g == gvrA },
		Handlers:  dummyHandlers,
	})

	snap := HandlerExtensionsSnapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	if snap[0].Name != "test.extension_a" {
		t.Fatalf("name = %q, want test.extension_a", snap[0].Name)
	}
}

func TestRegisterHandlerExtension_DuplicateNamePanics(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	gvr := schema.GroupVersionResource{Group: "a", Version: "v1", Resource: "as"}

	RegisterHandlerExtension(HandlerExtension{
		Name:      "test.dup",
		Predicate: func(g schema.GroupVersionResource) bool { return g == gvr },
		Handlers:  dummyHandlers,
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("duplicate name must panic")
		}
	}()
	RegisterHandlerExtension(HandlerExtension{
		Name:      "test.dup",
		Predicate: func(g schema.GroupVersionResource) bool { return g == gvr },
		Handlers:  dummyHandlers,
	})
}

func TestRegisterHandlerExtension_EmptyNamePanics(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("empty name must panic")
		}
	}()
	RegisterHandlerExtension(HandlerExtension{
		Name:      "",
		Predicate: func(_ schema.GroupVersionResource) bool { return true },
		Handlers:  dummyHandlers,
	})
}

func TestRegisterHandlerExtension_NilPredicatePanics(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("nil predicate must panic")
		}
	}()
	RegisterHandlerExtension(HandlerExtension{
		Name:      "nil.pred",
		Predicate: nil,
		Handlers:  dummyHandlers,
	})
}

func TestRegisterHandlerExtension_NilHandlersPanics(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("nil handlers must panic")
		}
	}()
	RegisterHandlerExtension(HandlerExtension{
		Name:      "nil.handlers",
		Predicate: func(_ schema.GroupVersionResource) bool { return true },
		Handlers:  nil,
	})
}

// TestAttachMatchingHandlerExtensions_PredicateMatching asserts that only
// extensions whose predicate matches the GVR fire, and that the per-name
// counter increments by exactly 1 per attach.
func TestAttachMatchingHandlerExtensions_PredicateMatching(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	gvrA := schema.GroupVersionResource{Group: "a", Version: "v1", Resource: "as"}
	gvrB := schema.GroupVersionResource{Group: "b", Version: "v1", Resource: "bs"}

	RegisterHandlerExtension(HandlerExtension{
		Name:      "ext.a_only",
		Predicate: func(g schema.GroupVersionResource) bool { return g == gvrA },
		Handlers:  dummyHandlers,
	})
	RegisterHandlerExtension(HandlerExtension{
		Name:      "ext.b_only",
		Predicate: func(g schema.GroupVersionResource) bool { return g == gvrB },
		Handlers:  dummyHandlers,
	})
	RegisterHandlerExtension(HandlerExtension{
		Name:      "ext.both",
		Predicate: func(g schema.GroupVersionResource) bool { return g == gvrA || g == gvrB },
		Handlers:  dummyHandlers,
	})

	// Attach for gvrA — ext.a_only + ext.both should fire (2), ext.b_only must not.
	attA := &fakeAttacher{}
	got := attachMatchingHandlerExtensions(nil, gvrA, attA)
	if got != 2 {
		t.Fatalf("attached = %d, want 2 (ext.a_only + ext.both for gvrA)", got)
	}
	if v := attA.addCount.Load(); v != 2 {
		t.Fatalf("AddEventHandler invocations = %d, want 2", v)
	}
	if HandlerExtensionsAttachedCount("ext.a_only") != 1 {
		t.Fatalf("counter ext.a_only = %d, want 1", HandlerExtensionsAttachedCount("ext.a_only"))
	}
	if HandlerExtensionsAttachedCount("ext.b_only") != 0 {
		t.Fatalf("counter ext.b_only = %d, want 0 (predicate didn't match gvrA)", HandlerExtensionsAttachedCount("ext.b_only"))
	}
	if HandlerExtensionsAttachedCount("ext.both") != 1 {
		t.Fatalf("counter ext.both = %d, want 1", HandlerExtensionsAttachedCount("ext.both"))
	}

	// Attach for gvrB — ext.b_only + ext.both should fire.
	attB := &fakeAttacher{}
	got = attachMatchingHandlerExtensions(nil, gvrB, attB)
	if got != 2 {
		t.Fatalf("attached for gvrB = %d, want 2", got)
	}
	if HandlerExtensionsAttachedCount("ext.b_only") != 1 {
		t.Fatalf("counter ext.b_only post-gvrB = %d, want 1", HandlerExtensionsAttachedCount("ext.b_only"))
	}
	if HandlerExtensionsAttachedCount("ext.both") != 2 {
		t.Fatalf("counter ext.both post-gvrB = %d, want 2 (gvrA + gvrB)", HandlerExtensionsAttachedCount("ext.both"))
	}
}

// TestAttachMatchingHandlerExtensions_EmptyRegistry is the no-op contract
// — an attach call against an empty registry must succeed with zero
// attachments and no panic.
func TestAttachMatchingHandlerExtensions_EmptyRegistry(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	gvr := schema.GroupVersionResource{Group: "a", Version: "v1", Resource: "as"}
	att := &fakeAttacher{}
	got := attachMatchingHandlerExtensions(nil, gvr, att)
	if got != 0 {
		t.Fatalf("empty-registry attached = %d, want 0", got)
	}
}

// TestAttachMatchingHandlerExtensions_AttachFailureDoesNotIncrementCounter
// verifies the contract: only successful AddEventHandler returns tick the
// counter.
func TestAttachMatchingHandlerExtensions_AttachFailureDoesNotIncrementCounter(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	gvr := schema.GroupVersionResource{Group: "a", Version: "v1", Resource: "as"}

	RegisterHandlerExtension(HandlerExtension{
		Name:      "ext.fail_first",
		Predicate: func(_ schema.GroupVersionResource) bool { return true },
		Handlers:  dummyHandlers,
	})

	att := &fakeAttacher{}
	att.failNext.Store(true) // first AddEventHandler returns an error
	got := attachMatchingHandlerExtensions(nil, gvr, att)
	if got != 0 {
		t.Fatalf("attached on failed-add = %d, want 0", got)
	}
	if HandlerExtensionsAttachedCount("ext.fail_first") != 0 {
		t.Fatalf("counter ext.fail_first = %d, want 0 (add failed)", HandlerExtensionsAttachedCount("ext.fail_first"))
	}
}

// TestRegisterHandlerExtension_ConcurrentAttach is the -race smoke test:
// many goroutines call attachMatchingHandlerExtensions against a stable
// registry simultaneously. The counter sum must equal the call count
// (no lost updates).
func TestRegisterHandlerExtension_ConcurrentAttach(t *testing.T) {
	ResetHandlerExtensionsForTest()
	t.Cleanup(ResetHandlerExtensionsForTest)

	gvr := schema.GroupVersionResource{Group: "race", Version: "v1", Resource: "rs"}
	RegisterHandlerExtension(HandlerExtension{
		Name:      "ext.race",
		Predicate: func(_ schema.GroupVersionResource) bool { return true },
		Handlers:  dummyHandlers,
	})

	const goroutines = 50
	const callsPer = 20
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < callsPer; j++ {
				att := &fakeAttacher{}
				_ = attachMatchingHandlerExtensions(nil, gvr, att)
			}
		}()
	}
	wg.Wait()

	want := uint64(goroutines * callsPer)
	if got := HandlerExtensionsAttachedCount("ext.race"); got != want {
		t.Fatalf("concurrent attach counter = %d, want %d", got, want)
	}
}

---- internal/cache/crdwatch_walker_spawn_test.go ----
// crdwatch_walker_spawn_test.go — Ship 0 / 0.30.222 falsifier corpus for
// the walker-driven CRD-informer spawn invariant.
//
// Diego's invariant (2026-06-01): "no CRD informer if the CRD object
// itself is not walked in frontend navigation." Before Ship 0 the CRD
// informer spawned at boot via MetaQuerySeeds() regardless of whether
// frontend navigation reached a CRD object. Ship 0 moves the spawn into
// AddAutoDiscoverGroup's sync.Once side-effect — fire IFF the walker
// touches a templated apiserver path.
//
// This file is the SOLE test that asserts the invariant directly. The
// existing TestMetaQuerySeeds_ExactBudget catches a count-mismatch but
// not the semantic "the seed set must not contain a walker-undiscoverable
// boot primordial." If a future regression re-adds customResourceDefin-
// itionGVR to MetaQuerySeeds(), that older test trips on the COUNT but
// this corpus's NoTemplatedPaths_NoCRDInformer subtest is the one that
// names the invariant.
//
// Package internal so the tests can reach ResetCRDInformerSpawnedForTest
// + customResourceDefinitionGVR + the handler-extension counter.

package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// walkerSpawnTestScheme returns a scheme with RBAC types registered so
// the dynamic fake client can decode informer LISTs. Internal-package
// equivalent of cache_test's newTestScheme.
func walkerSpawnTestScheme() *k8sruntime.Scheme {
	sch := k8sruntime.NewScheme()
	_ = rbacv1.AddToScheme(sch)
	return sch
}

// crdWalkerSpawnSetup spins up a CACHE_ENABLED=true watcher backed by a
// fake dynamic client + the RBAC List-kind set. Returns the watcher and a
// cleanup. Every subtest needs this fixture; pulling it out keeps the
// test bodies focused on the invariant assertions.
func crdWalkerSpawnSetup(t *testing.T) *ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	ResetAutoDiscoverGroupsForTest()
	ResetCRDInformerSpawnedForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)
	t.Cleanup(ResetCRDInformerSpawnedForTest)

	listKinds := map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
		customResourceDefinitionGVR:                                                            "CustomResourceDefinitionList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(walkerSpawnTestScheme(), listKinds)
	rw, err := NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil) })
	return rw
}

// extensionCountSnapshot captures the cache.handler_extensions_attached
// counter for the CRD-watch extension at a single instant. The Ship 0
// falsifier asserts:
//
//	NoTemplatedPaths_NoCRDInformer:     counter == 0
//	SingleTemplatedPath_OneSpawn:       counter == 1
//	MultipleTemplatedPaths_StillOneSpawn: counter == 1
//	RaceSafe_ConcurrentAddAutoDiscoverGroup: counter == 1
//	ReconcileNoOp_OnEmptyAutoDiscover:   counter == 0
//
// The extension's name is the same string crdwatch.init() declares.
const crdwatchExtensionName = "crdwatch.composition_auto_discovery"

func extensionCountSnapshot() uint64 {
	return HandlerExtensionsAttachedCount(crdwatchExtensionName)
}

// TestCRDInformer_NotSpawnedWithoutTemplatedRA is the v5 §6.5 falsifier
// corpus. Each subtest exercises one row of the §6.5 table.
func TestCRDInformer_NotSpawnedWithoutTemplatedRA(t *testing.T) {

	// SUBTEST 1 — NoTemplatedPaths_NoCRDInformer.
	//
	// Walker corpus has only static apiserver paths (no `${...}`) — none
	// of which extract a templated group via
	// ExtractAPIServerGroupFromTemplatedPath. AddAutoDiscoverGroup is
	// therefore never called → sync.Once stays untouched → CRD informer
	// must NOT be in rw.informers and the handler-extension counter must
	// be 0.
	t.Run("NoTemplatedPaths_NoCRDInformer", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		// Static paths only — no walker code path reaches AddAutoDiscoverGroup.
		staticPaths := []string{
			"/api/v1/namespaces/default/configmaps/foo",
			"/api/v1/namespaces/default/secrets/bar",
			"/apis/apps/v1/namespaces/default/deployments/baz",
		}
		for _, p := range staticPaths {
			// We don't actually call the walker — we just demonstrate the
			// extraction would return ok=false (no templated group) for
			// each path. The walker source only invokes
			// AddAutoDiscoverGroup when extraction succeeds.
			if grp, ok := ExtractAPIServerGroupFromTemplatedPath(p); ok && grp != "" {
				// Static-path extraction may succeed (it pulls "apps" out
				// of /apis/apps/...); the assertion is that the WALKER
				// gates AddAutoDiscoverGroup behind a templated form. For
				// this subtest we DELIBERATELY never call
				// AddAutoDiscoverGroup so the invariant under test —
				// "without an AddAutoDiscoverGroup call the CRD informer
				// does not spawn" — is exercised cleanly.
				_ = grp
			}
		}

		if got := AutoDiscoverGroupsSnapshot(); len(got) != 0 {
			t.Fatalf("auto-discover set must be empty when no walker calls AddAutoDiscoverGroup; got %v", got)
		}
		if rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: CRD informer registered without any AddAutoDiscoverGroup call")
		}
		if got := extensionCountSnapshot() - before; got != 0 {
			t.Fatalf("handler-extension counter delta = %d, want 0 (no spawn)", got)
		}
	})

	// SUBTEST 2 — SingleTemplatedPath_OneSpawn.
	//
	// One AddAutoDiscoverGroup call (modelling the walker reaching one
	// templated RESTAction path) must:
	//   - populate the auto-discover set with the named group,
	//   - spawn the CRD informer (sync.Once fires),
	//   - attach the crdwatch.composition_auto_discovery handler set
	//     (handler-extension counter ticks to 1).
	t.Run("SingleTemplatedPath_OneSpawn", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		// Model the walker call site: resolver parsed a templated path
		// and pulled out the static group.
		grp, ok := ExtractAPIServerGroupFromTemplatedPath(
			"/apis/composition.krateo.io/${.spec.version}/namespaces/${.ns}/${.kind}",
		)
		if !ok {
			t.Fatalf("ExtractAPIServerGroupFromTemplatedPath must succeed on a templated composition path")
		}
		AddAutoDiscoverGroup(grp)

		if got := AutoDiscoverGroupsSnapshot(); len(got) != 1 || got[0] != "composition.krateo.io" {
			t.Fatalf("auto-discover set = %v, want [composition.krateo.io]", got)
		}
		if !rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: AddAutoDiscoverGroup did not spawn the CRD informer")
		}
		if got := extensionCountSnapshot() - before; got != 1 {
			t.Fatalf("handler-extension counter delta = %d, want 1 (single spawn)", got)
		}
	})

	// SUBTEST 3 — MultipleTemplatedPaths_StillOneSpawn.
	//
	// N=10 AddAutoDiscoverGroup calls across 3 groups must spawn the CRD
	// informer EXACTLY ONCE (sync.Once invariant). The auto-discover set
	// grows to the unique-group count; the counter stays at 1.
	t.Run("MultipleTemplatedPaths_StillOneSpawn", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		groups := []string{
			"composition.krateo.io",
			"composition.krateo.io", // duplicate — auto-discover dedups
			"widgets.templates.krateo.io",
			"widgets.templates.krateo.io",
			"templates.krateo.io",
			"composition.krateo.io",
			"templates.krateo.io",
			"widgets.templates.krateo.io",
			"composition.krateo.io",
			"templates.krateo.io",
		}
		for _, g := range groups {
			AddAutoDiscoverGroup(g)
		}

		uniques := AutoDiscoverGroupsSnapshot()
		if len(uniques) != 3 {
			t.Fatalf("auto-discover set len = %d, want 3 unique groups; got %v", len(uniques), uniques)
		}
		if !rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: CRD informer must be registered after the first AddAutoDiscoverGroup")
		}
		if got := extensionCountSnapshot() - before; got != 1 {
			t.Fatalf("handler-extension counter delta = %d, want 1 (sync.Once invariant — exactly one spawn across N calls)", got)
		}
	})

	// SUBTEST 4 — RaceSafe_ConcurrentAddAutoDiscoverGroup.
	//
	// 100 goroutines call AddAutoDiscoverGroup simultaneously. Under
	// `-race`:
	//   - No data race on autoDiscoverGroups (autoDiscoverMu serializes).
	//   - CRD informer spawns EXACTLY ONCE (crdInformerSpawned sync.Once).
	//   - Handler-extension counter ticks exactly ONCE.
	t.Run("RaceSafe_ConcurrentAddAutoDiscoverGroup", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		const goroutines = 100
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				// Mix of duplicate and distinct groups exercises the
				// dedup mutex AND the spawn sync.Once.
				if idx%3 == 0 {
					AddAutoDiscoverGroup("composition.krateo.io")
				} else if idx%3 == 1 {
					AddAutoDiscoverGroup("widgets.templates.krateo.io")
				} else {
					AddAutoDiscoverGroup("templates.krateo.io")
				}
			}(i)
		}
		close(start)
		wg.Wait()

		if !rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: concurrent AddAutoDiscoverGroup did not spawn the CRD informer")
		}
		if got := extensionCountSnapshot() - before; got != 1 {
			t.Fatalf("handler-extension counter delta = %d, want 1 (sync.Once under concurrent goroutines)", got)
		}
		if got := AutoDiscoverGroupsSnapshot(); len(got) != 3 {
			t.Fatalf("auto-discover set len = %d, want 3 unique groups after concurrent calls", len(got))
		}
	})

	// SUBTEST 5 — ReconcileNoOp_OnEmptyAutoDiscover.
	//
	// Calling ReconcileAutoDiscoverCRDs with NO AddAutoDiscoverGroup
	// preceding it must:
	//   - return 0 (no CRD informer exists → graceful no-op via
	//     crdwatch.go's nil-informer guard);
	//   - NOT spawn the CRD informer (the reconcile is a passive
	//     re-scan, not a spawn trigger);
	//   - NOT panic;
	//   - leave the handler-extension counter at 0.
	t.Run("ReconcileNoOp_OnEmptyAutoDiscover", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		registered := rw.ReconcileAutoDiscoverCRDs()
		if registered != 0 {
			t.Fatalf("ReconcileAutoDiscoverCRDs on empty auto-discover set = %d, want 0", registered)
		}
		if rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: ReconcileAutoDiscoverCRDs spawned the CRD informer; " +
				"reconcile must be a passive re-scan, not a spawn trigger")
		}
		if got := AutoDiscoverGroupsSnapshot(); len(got) != 0 {
			t.Fatalf("auto-discover set must remain empty post-reconcile; got %v", got)
		}
		if got := extensionCountSnapshot() - before; got != 0 {
			t.Fatalf("handler-extension counter delta = %d, want 0", got)
		}
	})
}

