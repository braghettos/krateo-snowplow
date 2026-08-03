# Task #194 — Ship L (0.30.246) implementation spec: CRD lifecycle bytesObject fix (ADD + UPDATE + DELETE)

**Author:** architect
**Date:** 2026-06-04 (AMENDED — Diego expanded scope from ADD-only to full CRD lifecycle + lint rule + doc-comment update)
**Status:** READY FOR DEV (pending PM gate)
**Source-of-truth root cause:** `docs/task-194-s4-convergence-timeout-trace-2026-06-04.md`
**Defect commit:** `dbbea37` (Ship 0.30.233)
**Target tag:** snowplow `0.30.246` + snowplow-chart `0.30.246` lockstep
**Branch base:** current HEAD of `ship-0.30.242-Hc-layered` = `af0f487` (snowplow 0.30.245 tip)
**Branch name:** `ship-0.30.246-bytesobject-crd-lifecycle-fix`

---

## AMENDMENT NOTE (2026-06-04)

Original spec covered CRD ADD only. Diego ratified expanded scope:

- **§3 (ADD fix)** — unchanged from v1.
- **§3a NEW** — CRD UPDATE handler + falsifier.
- **§3b NEW** — CRD DELETE handler + falsifier (uses existing `RemoveResourceType` at `watcher.go:1317` and `OnResourceTypeRemoved` at `deps.go:716` — wiring missing in production per followup #117).
- **§6 (audit)** — expanded to lint rule, now bundled INTO Ship L (Q2 ratified).
- **§3.5 NEW** — line 72 comment update (Q3 ratified).
- **§7 (commits)** — revised to 6-commit chain for bisectability.

Updated LOC budget: ~350-500 LOC (vs ~150-200 original). Scope is still
bounded to `internal/cache/` + `scripts/lint/`. No new flags, no schema
changes, no chart logic changes (image tag only).

---

## 1. Executive summary

Ship 0.30.233 (`dbbea37`) introduced `crd_discovery_side_effect.go` to bridge
CRD-ADD events to `DiscoverGroupResources`. The bridge's worker reads
`spec.group` via a hard type-assert at `crd_discovery_side_effect.go:248`:

```go
u, ok := obj.(*unstructured.Unstructured)
if !ok || u == nil { c.discoverySkippedNG.Add(1); return }
```

Post Ship H5 (streaming-listwatch is the default for every dynamic informer
per `watcher.go:1035-1047`), the customresourcedefinitions informer delivers
`*bytesObject`, not `*unstructured.Unstructured`. The assertion silently
fails, the bridge bumps `discoverySkippedNG`, returns, and the per-group
composition informer is never spawned. On a clean-cluster install-burst the
L1 piechart cell stays stuck at `{"title":"0"}` indefinitely (Phase 6 run:
~48 min before nav-derived discovery finally fired). Full root-cause chain
in `docs/task-194-s4-convergence-timeout-trace-2026-06-04.md`.

Ship L (AMENDED) fixes the full CRD lifecycle bytesObject gap. Original
scope was ADD-only; Diego ratified expansion to UPDATE + DELETE plus a
bundled lint rule to catch the regression class.

- **ADD (§3):** route obj through existing `decodeBytesObject`
  (`bytesobject.go:394-407`) before reading `spec.group`. Fixes the H4 stuck-0
  defect.
- **UPDATE (§3a):** wire UpdateFunc to re-fire discovery + dirty-mark
  dependent L1 (new served versions on a CRD UPDATE need their per-resource
  informer registered).
- **DELETE (§3b):** wire DeleteFunc to call existing `RemoveResourceType`
  (`watcher.go:1317`) + `OnResourceTypeRemoved` (`deps.go:716`) for each
  served version. Closes followup #117 (the documented-but-missing
  CRD-DELETE wiring).
- **Expvar (§5):** publish `CRDDiscoveryStats` (closes followup #143).
- **Lint (§6.1):** new `go/ast` lint catches raw
  `*unstructured.Unstructured` asserts in informer-event-handler literal
  bodies (mirrors Phase 2 F4 lint pattern).

Scope: ~535 LOC net. Single PR, 6 commits for bisectability. Risk: LOW for
ADD/UPDATE/expvar/lint, MEDIUM for DELETE (only path that mutates
ResourceWatcher state — failure modes enumerated §10.4).

**Customer impact:** any install-burst from clean cluster state where
`composition.krateo.io` doesn't pre-exist at snowplow boot. Without the fix
the piechart stays stuck-0 indefinitely; with the fix S4 converges in
seconds.

---

## 3. The fix patch — exact code shape

**File:** `internal/cache/crd_discovery_side_effect.go`
**Function:** `triggerCRDDiscovery` (lines 227-299)
**Edit site:** lines 248-258 (the type-assert + the first soft-skip return)

### 3.1 Patch shape

Replace:

```go
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || u == nil {
		// bytesObject / PartialObjectMetadata / nil — the CRD
		// informer is dynamic-full-Unstructured by construction
		// (see watcher.go addResourceTypeLocked path). A non-
		// Unstructured arrival means a routing change that future
		// editors should be aware of.
		c.discoverySkippedNG.Add(1)
		return
	}

	group, found, err := unstructured.NestedString(u.Object, "spec", "group")
```

With:

```go
	// Ship 0.30.246 — decode-on-access. Post Ship H5 the
	// streaming-listwatch is the default for every dynamic informer
	// (watcher.go:1035-1047), so the customresourcedefinitions
	// informer delivers *bytesObject here, NOT *unstructured.Unstructured.
	// decodeBytesObject (bytesobject.go:394) is the established
	// H5-aware decode dance: *bytesObject -> fresh Unstructured via
	// .Decode(); *unstructured.Unstructured -> returned as-is.
	// Anything else (PartialObjectMetadata, nil, etc.) -> (nil, false)
	// and we soft-skip.
	u, ok := decodeBytesObject(obj)
	if !ok || u == nil {
		c.discoverySkippedNG.Add(1)
		// One-shot WARN: surface unknown delivery shapes in pod logs
		// without volume. Future routing changes will surface here.
		warnOnceCRDDecodeSkip(obj)
		return
	}

	group, found, err := unstructured.NestedString(u.Object, "spec", "group")
```

### 3.2 Idiomatic choice: `decodeBytesObject` over inline type-switch

The trace doc sketched a 3-case type-switch. **Use the 1-line
`decodeBytesObject` call** because: (a) the helper exists and is
well-tested (`bytesobject.go:394-407`); (b) the 5 existing call sites in
`watcher.go` (1561, 1619, etc.) all use this helper — Ship L restores
consistency; (c) single decode contract — any future delivery shape gets
added to `decodeBytesObject` once and all 6 call sites benefit; (d)
behaviourally byte-identical to the type-switch (returns `(nil, false)`
for any non-{bytesObject,Unstructured} shape, trips the same soft-skip).

### 3.3 Decode-failure handling

`decodeBytesObject` returns `(nil, false)` on either a `Decode()` error
(malformed raw bytes — should not happen with `newBytesObjectFromRaw`-produced
bytes) or a non-{bytesObject,Unstructured} shape. Both paths bump
`discoverySkippedNG` and emit a one-shot WARN via `warnOnceCRDDecodeSkip`
(§3.4). No panic; the recover wrapper at `:234` covers any
`unstructured.NestedString` panic on a degenerate map.

### 3.4 New helper: `warnOnceCRDDecodeSkip`

Add to `crd_discovery_side_effect.go` (just below `triggerCRDDiscovery`):

```go
// warnOnceCRDDecodeSkip emits a single WARN per unique go-type observed at
// the decode-skip path. Bounded by a sync.Map keyed on the type name so log
// volume is bounded by the number of distinct delivery shapes (1-2 in
// practice).
var crdDecodeSkipWarnedTypes sync.Map // map[string]struct{}

func warnOnceCRDDecodeSkip(obj interface{}) {
    typeName := goTypeOf(obj) // existing helper in rbac_snapshot.go
    if _, loaded := crdDecodeSkipWarnedTypes.LoadOrStore(typeName, struct{}{}); loaded {
        return
    }
    slog.Warn("cache.crd_discovery.decode_skipped",
        slog.String("subsystem", "cache"),
        slog.String("got_type", typeName),
        slog.String("hint", "CRD-ADD event arrived in an undecodable shape — "+
            "decodeBytesObject returned (nil,false). Inspect goTypeOf to "+
            "identify the new delivery shape; if it is *bytesObject, the "+
            "raw bytes are malformed (rare); if it is something else, "+
            "decodeBytesObject (bytesobject.go:394) needs a new case."),
    )
}
```

`goTypeOf` already exists at `rbac_snapshot.go:779` and is the package-internal
"`%T`-without-reflect" helper.

### 3.5 Doc-comment update (Q3 ratified)

`crd_discovery_side_effect.go:72`:

Replace:
```go
type crdDiscoveryEvent struct {
    obj interface{} // *unstructured.Unstructured (or bytesObject — see decode below)
}
```

With:
```go
type crdDiscoveryEvent struct {
    // obj is the CRD event payload. Production delivery shape post-Ship-H5
    // is *bytesObject (streaming-listwatch is the default for every dynamic
    // informer per watcher.go:1035-1047). The stock-informer fallback path
    // delivers *unstructured.Unstructured. triggerCRDDiscovery routes both
    // through decodeBytesObject (bytesobject.go:394) — see Ship L /
    // 0.30.246. DELETE events may wrap obj in clientcache.DeletedFinalStateUnknown
    // (see processDeleteEvent — unwrap before decode).
    obj interface{}
    // kind discriminates ADD vs UPDATE vs DELETE so the worker dispatches
    // to the right side-effect (Ship L / 0.30.246). UPDATE refreshes
    // discovery + dirty-marks dependent L1; DELETE tears down the
    // per-resource informer + dirty-marks dependent L1.
    kind crdLifecycleKind
}

// crdLifecycleKind discriminates the three CRD events the bridge handles.
type crdLifecycleKind uint8

const (
    crdLifecycleAdd    crdLifecycleKind = iota // CRD CREATE
    crdLifecycleUpdate                         // CRD UPDATE (spec.versions[]/served[]/group changes)
    crdLifecycleDelete                         // CRD DELETE (group is no longer served by the apiserver)
)
```

### 3a. CRD UPDATE handler

**Why UPDATE matters:** A CRD UPDATE can: (a) add a new `spec.versions[]`
entry that wasn't previously served — the new GVR has no informer; (b) flip
`spec.versions[N].served` from false→true or vice-versa — the previously-not-
served GVR needs an informer (or vice-versa); (c) bump the schema in a
way that changes downstream resolver behaviour. The trace doc's "0.30.233
stuck-0 later in same run" anecdote (trace lines 244-250) hints that a CRD
UPDATE on an already-discovered group could re-fire the same
nav-discovered/informer-state mismatch.

**Wiring** — in `deps_watch.go:221-224`, the current `UpdateFunc` is:

```go
UpdateFunc: func(_, newObj interface{}) {
    ns, name := metaNSName(newObj)
    Deps().OnUpdate(gvr, ns, name)
},
```

This is bytesObject-safe (only ns/name via `metaNSName`), but it does NOT
fire any CRD-discovery side-effect. Ship L wires a CRD-specific UPDATE
branch:

```go
UpdateFunc: func(oldObj, newObj interface{}) {
    ns, name := metaNSName(newObj)
    Deps().OnUpdate(gvr, ns, name)
    if crdSideEffect {
        // Ship L / 0.30.246 — CRD UPDATE may add new served versions or
        // change spec.group. Re-run discovery for the new spec.group so
        // any newly-served version's per-resource informer is registered.
        // Idempotent — DiscoverGroupResources is per-group singleflighted
        // (discovery_lookup.go:228+).
        crdDiscoverySingleton().submitCRDLifecycleEvent(newObj, crdLifecycleUpdate)
    }
},
```

**Worker side** — extend `submitCRDDiscoveryEvent` to a generalised
`submitCRDLifecycleEvent(obj, kind)` that enqueues the discriminated event.
`processEvent` dispatches on `kind`:

```go
func (c *crdDiscovery) processEvent(ev crdDiscoveryEvent) {
    c.eventsProcessed.Add(1)
    switch ev.kind {
    case crdLifecycleAdd:
        triggerCRDDiscovery(ev.obj, crdLifecycleAdd)
    case crdLifecycleUpdate:
        triggerCRDDiscovery(ev.obj, crdLifecycleUpdate)
    case crdLifecycleDelete:
        triggerCRDDelete(ev.obj)
    }
}
```

`triggerCRDDiscovery` accepts `kind` so it can branch on UPDATE-only behaviour
(e.g. NOT re-add to nav-discovered-set if already present — `AddNavigationDiscoveredGroup`
is already idempotent at `discovery_lookup.go:87-102`).

**Contract for UPDATE:**
1. Decode `newObj` via `decodeBytesObject` (same H5-aware path as ADD).
2. Extract `spec.group` via `unstructured.NestedString`.
3. Call `AddNavigationDiscoveredGroup(group)` — idempotent no-op if already present.
4. Call `DiscoverGroupResources(ctx, saRC, group)` — singleflighted; picks up any newly-served versions and registers per-resource informers via `EnsureResourceType`.
5. Call `Deps().OnResourceTypeAvailable(gvr_for_each_served_version)` to dirty-mark dependent L1 entries (the schema may have changed; downstream resolvers should re-resolve). The GVR(s) to dirty-mark are derived from `spec.versions[].name` × `(spec.group, spec.names.plural)` — same pattern as `cache_mode.go:310-321`.

**LOC estimate:** ~30 LOC in `crd_discovery_side_effect.go` (extend
`triggerCRDDiscovery` signature + add a kind-specific tail branch) + 1-line
wiring in `deps_watch.go` + ~5 LOC for the new constants.

**Risk:** LOW. `AddNavigationDiscoveredGroup` is idempotent;
`DiscoverGroupResources` is singleflighted; `OnResourceTypeAvailable` is
no-op-on-empty (`deps.go:692-698`). Worst case: an UPDATE that changes
nothing dirty-marks zero dependent keys — wall-clock cost ≈ one Decode +
one map iteration.

### 3b. CRD DELETE handler

**Why DELETE matters:** When a CRD is removed (operator uninstall, blueprint
cleanup, etc.), the apiserver stops serving that group's resources. Without
teardown:
- The per-resource informer's WATCH keeps returning 404 — log spam + retry storm.
- L1 entries that resolved against the GVR hold stale data (last-known LIST) until evicted by some other mechanism.
- The standalone informer's Run goroutine and sync-watcher leak (per `watcher.go:1300-1308`).

Snowplow already has the teardown primitive: `RemoveResourceType` at
`watcher.go:1317-1344` is documented as "wired ONLY to CRD-delete... the
CRD-DELETE periodic-sweep follow-up (#117) will re-wire it post-Ship-2"
(watcher.go:1289-1290). **The wiring referenced in that comment is missing
in production.** Grep confirms: zero production callers of
`RemoveResourceType` — only test fixtures (`grep RemoveResourceType( ...
| grep -v _test.go` returns only the definition site at `watcher.go:1317`).

**Wiring** — in `deps_watch.go:225-237`, the current `DeleteFunc`:

```go
DeleteFunc: func(obj interface{}) {
    if tomb, ok := obj.(clientcache.DeletedFinalStateUnknown); ok {
        obj = tomb.Obj
    }
    ns, name := metaNSName(obj)
    w.submitDeleteEvent(depDeleteEvent{gvr: gvr, namespace: ns, name: name})
},
```

Already unwraps `DeletedFinalStateUnknown` (good). Already bytesObject-safe
via `metaNSName`. Ship L adds the CRD-specific tail:

```go
DeleteFunc: func(obj interface{}) {
    if tomb, ok := obj.(clientcache.DeletedFinalStateUnknown); ok {
        obj = tomb.Obj
    }
    ns, name := metaNSName(obj)
    w.submitDeleteEvent(depDeleteEvent{gvr: gvr, namespace: ns, name: name})
    if crdSideEffect {
        // Ship L / 0.30.246 — CRD DELETE tears down the per-resource
        // informer(s) for every spec.versions[] of the deleted CRD and
        // dirty-marks dependent L1 entries.
        crdDiscoverySingleton().submitCRDLifecycleEvent(obj, crdLifecycleDelete)
    }
},
```

**Worker side** — new `triggerCRDDelete(obj)` in
`crd_discovery_side_effect.go`:

```go
// triggerCRDDelete handles a CRD DELETE event: derive the GVRs that
// were served, tear down each per-resource informer via
// RemoveResourceType, and dirty-mark dependent L1 entries via
// OnResourceTypeRemoved.
//
// IDENTITY INVARIANTS — same shape as triggerCRDDiscovery:
//   - obj is decoded via decodeBytesObject (H5-aware).
//   - spec.group + spec.names.plural + each served spec.versions[].name
//     produce the GVRs that need teardown.
//   - RemoveResourceType is idempotent (watcher.go:1292): unknown GVR is a
//     no-op. Safe under double-fire (DELETE storm).
//   - OnResourceTypeRemoved is no-op-on-empty (deps.go:716-722).
//
// FAILURE MODES (see §10):
//   - decodeBytesObject fails -> soft-skip + WARN. The informer keeps
//     running until it WATCH-404s and the controller-health snapshot
//     re-establishes via OnResourceTypeRemoved on the next sync.
//   - spec.versions[] is empty -> no GVR to tear down. WARN + skip.
//   - cache.Global() returns nil (test path) -> RemoveResourceType is
//     itself nil-receiver-safe (watcher.go:1318).
func triggerCRDDelete(obj interface{}) {
    c := crdDiscoverySingleton()
    defer func() {
        if rec := recover(); rec != nil {
            c.panicsRecovered.Add(1)
            slog.Error("cache.crd_discovery.delete.panic_recovered", /* ... */)
        }
    }()

    u, ok := decodeBytesObject(obj)
    if !ok || u == nil {
        c.deleteSkippedNG.Add(1)
        warnOnceCRDDecodeSkip(obj)
        return
    }

    group, _, _ := unstructured.NestedString(u.Object, "spec", "group")
    plural, _, _ := unstructured.NestedString(u.Object, "spec", "names", "plural")
    if group == "" || plural == "" {
        c.deleteSkippedNG.Add(1)
        return
    }

    versions, found, err := unstructured.NestedSlice(u.Object, "spec", "versions")
    if err != nil || !found || len(versions) == 0 {
        c.deleteSkippedNG.Add(1)
        return
    }

    rw := Global()
    torn := 0
    for _, v := range versions {
        vm, vok := v.(map[string]any)
        if !vok {
            continue
        }
        name, _, _ := unstructured.NestedString(vm, "name")
        served, _, _ := unstructured.NestedBool(vm, "served")
        if name == "" || !served {
            // Not-served versions had no informer wired (per
            // cache_mode.go:312-316); skip.
            continue
        }
        gvr := schema.GroupVersionResource{Group: group, Version: name, Resource: plural}
        if rw != nil {
            rw.RemoveResourceType(gvr) // idempotent, nil-safe
        }
        Deps().OnResourceTypeRemoved(gvr) // dirty-mark dependent L1
        torn++
    }

    c.deletesProcessed.Add(1)
    slog.Info("cache.crd_discovery.delete.processed",
        slog.String("subsystem", "cache"),
        slog.String("group", group),
        slog.String("plural", plural),
        slog.Int("gvrs_torn_down", torn),
    )

    // NOTE: we do NOT remove `group` from navDiscoveredGroups.
    // The set is documented append-only (discovery_lookup.go:52-68 +
    // phase1.go:306). A CRD re-CREATE with the same group is the only
    // re-entry path; AddNavigationDiscoveredGroup is idempotent. The
    // removable-discriminator predicate at watcher.go:749 + :1074 is
    // load-bearing for the informer-construction strategy on re-CREATE
    // (standalone vs shared-factory). Removing the group from the set
    // would silently change the re-CREATE informer's lifecycle.
    // PROPOSED FOLLOWUP: explicit RemoveNavigationDiscoveredGroup with
    // a re-CREATE corner-case test before wiring. NOT in Ship L scope.
}
```

**Contract for DELETE:**
1. Decode `obj` (or its unwrapped `DeletedFinalStateUnknown.Obj`) via `decodeBytesObject`.
2. Extract `spec.group`, `spec.names.plural`, `spec.versions[]` from the deleted CRD's last-known spec.
3. For each served version: `RemoveResourceType(gvr)` (informer teardown — idempotent) + `OnResourceTypeRemoved(gvr)` (L1 dirty-mark — idempotent).
4. Do NOT remove the group from `navDiscoveredGroups` (see inline note — followup required before wiring removal).
5. New counters: `deletesProcessed`, `deleteSkippedNG`.

**LOC estimate:** ~70 LOC for `triggerCRDDelete` (the function above) +
~10 LOC for new counters + ~5 LOC for the kind constant + 4-line DeleteFunc
extension in `deps_watch.go`.

**Risk:** MEDIUM (highest in Ship L). See §10.4.

---

**File:** `internal/cache/crd_add_discovery_test.go`. Existing fixture
`crdObj()` at lines 134-143 builds `*unstructured.Unstructured`. Add a
parallel `crdBytesObj()` and a new test `TestCRDAdd_TriggersGroupDiscovery_BytesObject`.

### 4.1 New fixture builder

```go
// crdBytesObj builds a synthetic CRD *bytesObject carrying (name, spec.group)
// in its raw JSON. This mirrors the production delivery shape from the
// streaming-listwatch path (newBytesObjectFromRaw at bytesobject.go:198):
// the metadata sub-object is decoded into the embedded ObjectMeta; the
// spec/status body lives verbatim inside `raw` and is decoded on-demand
// via decodeBytesObject.
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
```

### 4.2 New test — the falsifier

```go
// TestCRDAdd_TriggersGroupDiscovery_BytesObject — Ship 0.30.246 falsifier.
//
// FAILS pre-Ship-0.30.246: the AddFunc dispatches the *bytesObject to
// submitCRDDiscoveryEvent, the worker calls triggerCRDDiscovery, which
// type-asserts (*unstructured.Unstructured) — fails — bumps
// discoverySkippedNG and returns. No discovery side-effect fires.
//
// PASSES post-Ship-0.30.246: triggerCRDDiscovery routes through
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
        t.Fatalf("Ship 0.30.246 FAIL: CRD ADD via *bytesObject did NOT add "+
            "composition.krateo.io to navDiscoveredGroups. The discovery "+
            "bridge is silently dropping streaming-shape CRD events.")
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
```

### 4.3 Dual-state verification (ADD)

Dev MUST verify pre/post-fix:
1. Add fixture + test FIRST (commit-1). Run
   `go test -run TestCRDAdd_TriggersGroupDiscovery_BytesObject ./internal/cache/`
   — must FAIL (`IsNavigationDiscoveredGroup`=false, `DiscoverySkippedNG=1`,
   `DiscoveryInvoked=0`).
2. Apply fix (commit-2). Re-run — must PASS. Re-run full suite — existing
   `TestCRDAdd_*` tests must stay green.

### 4.4 New falsifier — `TestCRDUpdate_TriggersGroupDiscovery_BytesObject`

Same fixture pattern as ADD; exercises the UpdateFunc end-to-end via a
`*bytesObject` payload. Builds two bytesObject CRDs (old + new) where `new`
adds a served version to `spec.versions[]`. Calls
`handlers.UpdateFunc(oldBO, newBO)`. Waits for the worker to process.
Asserts:
- `IsNavigationDiscoveredGroup("composition.krateo.io")` returns true.
- `DiscoveryInvoked >= 1` (UPDATE re-fires the discovery hop).
- `DiscoverySkippedNG == 0`.
- `Deps().Stats().DirtyMarkTotal` has incremented (the GVR-dirty-mark
  side-effect fired — uses `OnResourceTypeAvailable`).

Pre-fix: `DiscoveryInvoked=0` (UpdateFunc has no discovery side-effect at
all today). Post-fix: passes per the contract.

```go
func TestCRDUpdate_TriggersGroupDiscovery_BytesObject(t *testing.T) {
    withCleanCRDDiscovery(t)

    fake := &fakeDiscoveryForCRD{
        group: "composition.krateo.io", version: "v1-2-2",
        res:   []metav1.APIResource{{Name: "githubscaffoldingwithcompositionpages", Kind: "GHSCP"}},
    }
    installFakeDiscoveryForCRD(t, fake)
    SetProcessSARestConfig(&rest.Config{Host: "https://fake.test"})

    crdGVR := CRDGVRForTest()
    rw := newGateWatcher()
    ch := make(chan struct{}); close(ch); rw.syncCh[crdGVR] = ch
    handlers := rw.depEventHandlers(crdGVR)

    // OLD: CRD with single served version.
    // NEW: CRD with an added served version. Post-fix, the new version's
    // GVR should be discovered + dirty-marked.
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
        t.Fatalf("worker did not process UPDATE event in 2s: %s",
            crdDiscoveryStatsString())
    }
    if !IsNavigationDiscoveredGroup("composition.krateo.io") {
        t.Fatalf("Ship L FAIL (UPDATE): composition.krateo.io NOT added "+
            "to navDiscoveredGroups via UpdateFunc")
    }
    s := CRDDiscoveryStatsSnapshot()
    if s.DiscoveryInvoked < 1 {
        t.Fatalf("DiscoveryInvoked=%d want >=1 on UPDATE", s.DiscoveryInvoked)
    }
    if s.DiscoverySkippedNG != 0 {
        t.Errorf("DiscoverySkippedNG=%d want 0", s.DiscoverySkippedNG)
    }
}
```

`crdBytesObjMultiVersion` is a fixture helper (same shape as `crdBytesObj`
in §4.1) but builds a CRD JSON with multiple `spec.versions[]` entries.
~25 LOC.

### 4.5 New falsifier — `TestCRDDelete_TearsDownInformer_BytesObject`

Builds a bytesObject CRD payload. Pre-fires an ADD via the AddFunc to wire
up the per-resource informer (this exercises the §3 fix as a precondition).
Then fires a DELETE through `handlers.DeleteFunc(crdBO)`. Asserts:
- `rw.RemoveResourceType` was called for each served version's GVR. Verified
  by snapshotting `rw.informers` (the per-GVR map) before/after.
- `Deps().Stats().DirtyMarkTotal` incremented by the count of dependent L1
  keys (use the `cache_event.consumed type=CRD_DELETE` log entry as
  ground-truth).
- `deletesProcessed >= 1`, `deleteSkippedNG == 0`.

```go
func TestCRDDelete_TearsDownInformer_BytesObject(t *testing.T) {
    withCleanCRDDiscovery(t)
    // ... [fake discovery + rw + handlers setup as TestCRDUpdate above] ...

    bo := crdBytesObjMultiVersion(t, "ghscp.composition.krateo.io",
        "composition.krateo.io", "githubscaffoldingwithcompositionpages",
        []versionSpec{{Name: "v1-2-2", Served: true}})

    // Precondition: ADD fires informer registration.
    handlers.AddFunc(bo)
    if !WaitCRDDiscoveryProcessedForTest(1, 2000) {
        t.Fatalf("ADD precondition failed: %s", crdDiscoveryStatsString())
    }
    targetGVR := schema.GroupVersionResource{
        Group: "composition.krateo.io", Version: "v1-2-2",
        Resource: "githubscaffoldingwithcompositionpages",
    }

    // DELETE fires teardown.
    handlers.DeleteFunc(bo)
    if !WaitCRDDiscoveryProcessedForTest(2, 2000) {
        t.Fatalf("DELETE not processed in 2s: %s", crdDiscoveryStatsString())
    }

    s := CRDDiscoveryStatsSnapshot()
    if s.DeletesProcessed < 1 {
        t.Fatalf("DeletesProcessed=%d want >=1", s.DeletesProcessed)
    }
    if s.DeleteSkippedNG != 0 {
        t.Errorf("DeleteSkippedNG=%d want 0", s.DeleteSkippedNG)
    }
    // The informer registered by AddFunc must be GONE.
    rw.mu.RLock()
    _, stillRegistered := rw.informers[targetGVR]
    rw.mu.RUnlock()
    if stillRegistered {
        t.Fatalf("Ship L FAIL (DELETE): per-resource informer for %s "+
            "was NOT torn down by CRD DELETE event", targetGVR)
    }
}
```

Plus a tombstone variant `TestCRDDelete_TearsDownInformer_DeletedFinalStateUnknown`
that wraps `crdBytesObj(...)` in `clientcache.DeletedFinalStateUnknown{Key: "...", Obj: bo}`
and asserts the same teardown — verifies the existing unwrap at
`deps_watch.go:229-231` continues to work with bytesObject inside the
tombstone wrapper.

---

## 5. Expvar wiring (closes followup #143)

`CRDDiscoveryStatsSnapshot` (`crd_discovery_side_effect.go:314-324`) is not
yet exposed at `/debug/vars`. Had it been, the regression would have
surfaced in seconds (non-zero `DiscoverySkippedNG` on a healthy cluster is
a flashing red flag) rather than a 5-min bench failure.

### 5.1 Add new file `internal/cache/crd_discovery_expvar.go`

```go
// crd_discovery_expvar.go — publishes CRDDiscoveryStats to expvar so the
// CRD-ADD discovery bridge's counters are observable at /debug/vars without
// hitting the pod's debug socket. Mirrors fallthrough_meter_expvar.go and
// refresher_metrics.go in pattern: sync.Once-guarded registration, called
// from init() unless CACHE_ENABLED=false.

package cache

import (
    "expvar"
    "sync"
)

var crdDiscoveryExpvarOnce sync.Once

func init() {
    if Disabled() {
        return
    }
    registerCRDDiscoveryExpvar()
}

func registerCRDDiscoveryExpvar() {
    crdDiscoveryExpvarOnce.Do(func() {
        expvar.Publish("snowplow_crd_discovery", expvar.Func(func() any {
            s := CRDDiscoveryStatsSnapshot()
            return map[string]uint64{
                "events_enqueued":      s.EventsEnqueued,
                "events_dropped":       s.EventsDropped,
                "events_processed":     s.EventsProcessed,
                // ADD path
                "discovery_invoked":    s.DiscoveryInvoked,
                "discovery_skipped_ng": s.DiscoverySkippedNG,
                // DELETE path (Ship L)
                "deletes_processed":    s.DeletesProcessed,
                "delete_skipped_ng":    s.DeleteSkippedNG,
                "panics_recovered":     s.PanicsRecovered,
            }
        }))
    })
}
```

The `CRDDiscoveryStats` struct (`crd_discovery_side_effect.go:304-311`)
gains 2 new fields:

```go
type CRDDiscoveryStats struct {
    EventsEnqueued     uint64
    EventsDropped      uint64
    EventsProcessed    uint64
    DiscoveryInvoked   uint64 // ADD + UPDATE (DiscoverGroupResources calls)
    DiscoverySkippedNG uint64 // ADD + UPDATE decode-skip
    DeletesProcessed   uint64 // NEW (Ship L) — successful DELETE teardowns
    DeleteSkippedNG    uint64 // NEW (Ship L) — DELETE decode-skip / no-served-versions
    PanicsRecovered    uint64
}
```
`CRDDiscoveryStatsSnapshot()` populates the two new fields from the
corresponding `atomic.Uint64` counters added to the `crdDiscovery` struct.

### 5.2 Test helper

Add a `registerCRDDiscoveryExpvar()` line to
`expvar_test_helpers.go`'s `RegisterExpvarForTest()` (already calls
`registerRefresherMetrics` etc.) so in-process tests that t.Setenv
CACHE_ENABLED can still see the var. The `map[string]uint64` shape matches
the existing `snowplow_bindings_by_gvr_delta_skipped_non_typed` flat-keyed
pattern (`bindings_by_gvr_metrics.go:60`).

---

## 6. Audit — analogous unchecked-assertion sites

Audit query: `grep -rn "obj.(\*unstructured.Unstructured)" internal/cache/`
plus all `AddFunc/UpdateFunc/DeleteFunc` sites in `internal/cache/`.

**Content-bearing raw `*unstructured.Unstructured` asserts:**

| File:line | Site | Bytes-safe? |
|---|---|---|
| `crd_discovery_side_effect.go:248` | `triggerCRDDiscovery` | **NO — THIS IS THE DEFECT** |
| `rbac_snapshot.go:764` | `rebuildSkipNonTyped` | YES — WARN-log path only; convertible objects route through `convertUnstructuredCRB/RB/CR/R` (`rbac_snapshot.go:652+`) → `fallbackUnstructuredFromIndexer` → `decodeBytesObject`. |
| `strip.go:217` | `defaultStripUnstructured` | YES — wired for stock-informer path only; streaming uses `stripItemJSON` per `watcher.go:1024-1026` (single-source-of-truth predicate). |

**Event-handler AddFunc/UpdateFunc/DeleteFunc sites:**

| Site | Content read? | Bytes-safe? |
|---|---|---|
| `deps_watch.go:201-237` (depEventHandlers) | ns/name only via `metaNSName` (uses `nsNameAccessor` interface — both shapes satisfy) | YES |
| `rbac_snapshot.go:857-880` (rbacSnapshotEventHandlers) | Yes via `onBindingAdd/Update/Delete/onRoleObjectChanged` | YES — routes through `asCRB/asRB/asCR/asRole` → `convertUnstructured*` → `decodeBytesObject` |
| `controller_health.go:351-353` | No (schedules rebuild only) | YES |
| `secrets_informer.go:222-224` | No (schedules rebuild only) | YES |

**Verdict:** the CRD-discovery bridge is the **only** AddFunc site doing a
raw content-bearing `*unstructured.Unstructured` assert. Ship L scope is
correctly bounded to this single site. No analogous defect elsewhere.

**Pre-merge gate (Q2 ratified — bundled INTO Ship L):** see §6.1.

### 6.1 Lint rule — `scripts/lint/no_unchecked_unstructured_assert.go`

New `go/ast`-based lint that mirrors the Phase 2 F4 lint at
`scripts/lint/no_parallel_binding_derivation.go`. Same `//go:build ignore`
+ `go run scripts/lint/no_unchecked_unstructured_assert.go` invocation
shape.

**Contract:**
1. Walk every non-`_test.go` `.go` file under `internal/cache/`.
2. For each `*ast.TypeAssertExpr` whose target type is `*unstructured.Unstructured`
   (matched by inspecting the `Type` field — `*ast.StarExpr` →
   `*ast.SelectorExpr` with `X.Name == "unstructured"` + `Sel.Name == "Unstructured"`):
   - Find the enclosing `*ast.FuncLit` (lambda) OR `*ast.FuncDecl`.
   - Check whether the enclosing function is a CompositeLit field initialiser for `ResourceEventHandlerFuncs` (i.e. an AddFunc/UpdateFunc/DeleteFunc body).
   - If yes: this assertion is content-bearing on an informer event payload. It MUST be reachable via a call to `decodeBytesObject` or `fallbackUnstructuredFromIndexer` (search the enclosing function body's call expressions for one of these names).
   - If no such call: report `file:line` as a violation.
3. Exit 1 with the violations list if any.

**Allowlist policy:** initially empty. The only legitimate raw-assert case
identified in §6's audit table (`rebuildSkipNonTyped` at
`rbac_snapshot.go:764`) is NOT inside an informer event-handler body, so
the lint will not flag it. If a future false-positive emerges, the
allowlist follows the F4 pattern (relative path list at the top of the lint
file with a doc-comment justifying each entry).

**Dual-state proof:**
- **MUST FAIL on commit `af0f487` (current HEAD, pre-Ship-L):** the
  `triggerCRDDiscovery` assertion at `crd_discovery_side_effect.go:248` is
  inside the worker function, not the AddFunc body directly. But the
  AddFunc dispatches via `submitCRDDiscoveryEvent` → worker queue →
  `triggerCRDDiscovery`. The lint as scoped to "the AddFunc body or any
  function it can reach via local-call" is too aggressive and false-positives.
  **Refined scope:** lint flags raw asserts inside the LITERAL handler
  bodies (the `*ast.FuncLit` immediately under the
  `ResourceEventHandlerFuncs{}` composite literal). Today's defect site
  is NOT in such a literal body (it's in `triggerCRDDiscovery`), so the
  lint would NOT catch this specific regression direct-in-the-literal.
  **Dual-state target instead:** synthesise a regression file
  `scripts/lint/testdata/regression_unchecked_assert.go` containing a
  test-only handler with a raw `obj.(*unstructured.Unstructured)` inside
  the literal — lint MUST flag it. Real code MUST pass.
- **MUST PASS on Ship L HEAD:** all production handler bodies pass through
  `decodeBytesObject` / `fallbackUnstructuredFromIndexer` / read only
  ns/name (no content assert).

**Lint scope honest disclosure:** the lint catches the regression CLASS
(raw assert in a literal AddFunc/UpdateFunc/DeleteFunc body) — it does NOT
catch the specific Ship 0.30.233 mistake (a raw assert in a function
reachable from the literal body via a channel + worker hop). Catching the
latter requires call-graph analysis which is out of scope. The remediation
for the call-graph class is the pattern itself: **any function that
receives an `interface{}` from an informer-event source must consume it via
`decodeBytesObject` / `metaNSName`** — a coding convention enforced by code
review + the audit table in §6, not by static analysis.

**LOC estimate:** ~180 LOC for the lint + ~30 LOC for the regression
test-fixture file. Wire into `Makefile`:

```make
lint-no-unchecked-unstructured-assert:
	go run scripts/lint/no_unchecked_unstructured_assert.go
```

CI gate wires this target into the pre-merge check.

---

## 7. Branch + tag + ship plan

### 7.1 Snowplow repo

- **Base:** current HEAD of `ship-0.30.242-Hc-layered` = commit `af0f487` (Ship K / 0.30.245 tip)
- **New branch:** `ship-0.30.246-bytesobject-crd-lifecycle-fix`
- **Commits (6 — bisectable; each commit either RED-on-tests-only or fully GREEN):**
  1. `test(cache): Ship L / 0.30.246 falsifiers — bytesObject CRD lifecycle (FAILING)` — adds `crdBytesObj`, `crdBytesObjMultiVersion` fixtures + the 3 falsifier tests (ADD §4.2, UPDATE §4.4, DELETE §4.5 + tombstone variant). All 3 fail on this commit; that is the triple dual-state proof. Existing tests still green.
  2. `fix(cache): Ship L / 0.30.246 — decode bytesObject in triggerCRDDiscovery + doc-comment` — applies §3 + §3.5 (line-72 comment). After this commit ONLY `TestCRDAdd_*_BytesObject` passes; UPDATE + DELETE falsifiers still RED.
  3. `feat(cache): Ship L / 0.30.246 — CRD UPDATE lifecycle handler` — applies §3a. UPDATE falsifier turns green; DELETE still RED.
  4. `feat(cache): Ship L / 0.30.246 — CRD DELETE lifecycle handler + RemoveResourceType wiring` — applies §3b. DELETE falsifier turns green. All falsifiers GREEN.
  5. `feat(cache): Ship L / 0.30.246 — publish CRDDiscoveryStats to /debug/vars (closes followup #143)` — adds `crd_discovery_expvar.go` per §5 (incl. new DeletesProcessed/DeleteSkippedNG fields).
  6. `feat(lint): Ship L / 0.30.246 — no-unchecked-unstructured-assert lint rule` — adds `scripts/lint/no_unchecked_unstructured_assert.go` + `scripts/lint/testdata/regression_unchecked_assert.go` (the FAIL-side fixture) + Makefile target per §6.1. Lint passes on Ship-L HEAD, fails on the regression fixture.
- **Tag:** `0.30.246` on commit-6.
- **Push:** `git push braghettos ship-0.30.246-bytesobject-crd-lifecycle-fix --tags` (NEVER upstream per `feedback_never_push_upstream`).

### 7.2 Chart repo (`/Users/diegobraga/krateo/snowplow-chart`)

Per `feedback_chart_release_lockstep` + `feedback_chart_repo_origin_is_upstream`
(chart `origin` IS upstream — inverted vs snowplow; push tags to `braghettos`
EXPLICITLY).

- **Base:** branch holding the most-recent chart sync (e.g. `chart-0.30.233` carries commits up to 0.30.241; dev `git log` to confirm; tip commit `f268fe4` is "chart 0.30.241 — image tag bump").
- **New branch:** `chart-0.30.246-bytesobject-discovery-fix`
- **Commit:** `chore: lockstep chart 0.30.246 — image tag bump (CRD-ADD bytesObject decode)` — bumps default image tag in `chart/values.yaml` to `0.30.246`. Embedded CRD definitions in `chart/templates/templates.krateo.io_restactions.yaml` UNCHANGED (code-only fix).
- **Tag + push:** `0.30.246`; `git push braghettos chart-0.30.246-bytesobject-discovery-fix 0.30.246`.

### 7.3 Single PR with 6 commits

The 6 commits tell a bisectable story (falsifiers-fail → ADD-fix →
UPDATE-fix → DELETE-fix → expvar → lint). Each commit either turns one
falsifier green or adds an observability/CI surface. Per
`feedback_dev_review_with_architect_pm_before_commit` dev shares the full
diff with architect + PM BEFORE committing the chain; PR opens against
braghettos/snowplow:main AFTER sign-off.

---

## 8. Dual-gate plan (architect + PM pre-commit)

### 8.1 Architect mini-review checklist (separate dispatch on dev's diff)

1. ADD patch site is exactly `crd_discovery_side_effect.go:248-258` — no scope creep.
2. `decodeBytesObject` is the helper invoked across ADD + UPDATE + DELETE paths (NOT open-coded type-switches).
3. UPDATE wiring at `deps_watch.go:221+` passes BOTH `oldObj` AND `newObj`-derived events? Spec passes `newObj` only — verify.
4. DELETE wiring at `deps_watch.go:225+` correctly unwraps `DeletedFinalStateUnknown` BEFORE handing to `submitCRDLifecycleEvent` (the existing unwrap at :229-231 stays).
5. `triggerCRDDelete` uses `Global()` (nil-safe) for `rw.RemoveResourceType`.
6. `triggerCRDDelete` does NOT remove from `navDiscoveredGroups` (load-bearing predicate per `watcher.go:749 + :1074`).
7. New counters `deletesProcessed`, `deleteSkippedNG` are atomic.Uint64; exposed via `CRDDiscoveryStatsSnapshot`.
8. `warnOnceCRDDecodeSkip` uses existing `goTypeOf` + sync.Map (bounded log volume).
9. Test fixtures `crdBytesObj` + `crdBytesObjMultiVersion` use `newBytesObjectFromRaw` (production constructor).
10. Tests assert FULL counter envelope including new fields.
11. Expvar publish key is `snowplow_crd_discovery` (matches `snowplow_refresher_*` naming convention); new keys `deletes_processed`, `delete_skipped_ng` present.
12. Lint at `scripts/lint/no_unchecked_unstructured_assert.go` mirrors the F4 pattern (`//go:build ignore`, allowlist-at-top, file:line violations, exit 1). Regression fixture file has `//go:build ignore`.
13. No new env flags (per `feedback_no_park_broken_behind_flag`).

### 8.2 PM mini-review checklist

1. Symptom-disappears check: trace doc answers yes ("decode bytesObject → AddNavigationDiscoveredGroup fires → walker/dispatcher spawns composition informer → ADD events invalidate piechart cell → next poll re-resolves → api becomes 20").
2. Falsifier dual-state validated by dev pre-commit per `feedback_falsifier_first_before_ship`.
3. Risk surface (§10) acceptable; fix is additive.
4. Chart lockstep planned; braghettos-explicit-push acknowledged.
5. Phase 6 retrigger plan in place post-deploy.

---

## 9. Deploy + verification plan

### 9.1 GKE context guard (mandatory)

Per `feedback_kubectl_verify_gke_context`: BEFORE any helm/kubectl invocation,
verify `kubectl config current-context` equals
`gke_neon-481711_us-central1-a_cluster-1`.

### 9.2 Helm upgrade (NOT `--reuse-values`)

Per `feedback_helm_no_reuse_values_on_chart_default_change`: pass the full
override set explicitly. The Ship L chart bumps only the image tag, so the
override set is the same as the current 0.30.245 deploy. Dev uses the
prevailing values file (see `feedback_helm_values_file_image_tag_override`).

```
helm upgrade snowplow ./chart \
    -f /path/to/active-values.yaml \
    --set image.tag=0.30.246
```

(Or whichever flow the dev currently uses for image-tag override; key invariant
is no `--reuse-values`.)

### 9.3 Hard-revert path

If the new pod fails verification or crashloops:

```
helm rollback snowplow <REV-1>
```

(`REV-1` is the rev that deployed 0.30.245; dev confirms via
`helm history snowplow -n krateo-system` before upgrading.) Rollback is the
hard-revert per the project pattern — NEVER kubectl-apply / kubectl-set-image.

### 9.4 Post-deploy validation (synthetic CRD lifecycle)

Three empirical checks BEFORE tester dispatch:

**Check 1 — expvar visibility.** After helm upgrade + pod Ready:

```
kubectl port-forward -n krateo-system deploy/snowplow 8181:8181 &
curl -s http://localhost:8181/debug/vars | jq '.snowplow_crd_discovery'
```

Expect 8-key map: events_enqueued, events_dropped, events_processed,
discovery_invoked, discovery_skipped_ng, deletes_processed, delete_skipped_ng,
panics_recovered. Pre-sync ADD events drop, so events_enqueued may be 0 at
boot; events_dropped must be 0.

**Check 2 — synthetic CRD ADD.** Deploy a CompositionDefinition that
installs a NEW CRD subtype not in the cluster. Watch:

```
kubectl logs -n krateo-system deploy/snowplow -f | \
    grep -E "cache\.discovery\.navigation_discovered_group_added|cache\.crd_discovery"
```

Expect `navigation_discovered_group_added group=composition.krateo.io`
within ~1s. Pre-fix log was absent 48 min; post-fix near-immediate.
Re-check expvar: `discovery_invoked >= 1`, `discovery_skipped_ng = 0`.

**Check 3 — synthetic CRD DELETE.** Uninstall the CompositionDefinition.
Watch for `cache.crd_discovery.delete.processed group=composition.krateo.io
gvrs_torn_down=1` AND `cache.resource_type.removed` (the
`RemoveResourceType` Info log at `watcher.go:1339`). Re-check expvar:
`deletes_processed >= 1`, `delete_skipped_ng = 0`. Verify subsequent
`/call` against the resolver that referenced this CRD returns the
apiserver's 404 (or empty list) — NOT stale cached data.

**Check 4 (optional) — synthetic CRD UPDATE.** Edit an existing CRD to
add a new served version (via `kubectl edit crd` — note the
`feedback_never_kubectl_apply` rule applies to helm-deployed manifests,
NOT to ad-hoc kubectl edits on bench-installed CRDs for diagnostic
purposes). Confirm a second `discovery_invoked` bump and a
`cache.lazy_register` log for the new GVR.

**All required checks (1, 2, 3) must pass before tester dispatch.**

### 9.5 Tester dispatch (Phase 6 retrigger)

Once both checks pass: dispatch tester for full Phase 6 on the clean-cluster
path. Expected: S4 VERIFY converges in seconds (vs 0.30.233 fast-path 3.4s
reference in the trace doc), not 5+ min. Bench `cluster_baseline` must
re-capture against the 0.30.246 binary per `feedback_anchor_is_cluster_state_dependent`.

---

## 10. Risk assessment

### 10.1 Failure-mode walkthrough

1. **`decodeBytesObject` returns `(nil, true)`** — cannot happen
   (`bytesobject.go:394-407` returns `(nil, false)` for any nil/undecodable).
2. **Decoded `uns.Object == nil`** — `Decode()` always builds
   `&unstructured.Unstructured{Object: make(map[string]any)}` (`bytesobject.go:374-380`),
   never nil. Recover wrapper at `:234` is the belt-and-braces.
3. **Malformed raw bytes** — `Decode()` errors, helper returns `(nil, false)`,
   soft-skip with WARN.
4. **Missing/empty/non-string `spec.group`** — pre-existing path at
   `crd_discovery_side_effect.go:260` catches all three, soft-skips. Byte-identical.
5. **Non-CRD bytesObject reaches the bridge** — gated by `IsCRDGVR(gvr)` at
   `deps_watch.go:199` (structural equality against the CRD-meta GVR per
   `crd_gvr.go:44`). Cannot happen.
6. **Decode cost** — `utiljson.Unmarshal` on ~tens-KB CRD JSON ≈ hundreds of
   µs/event. CRD ADDs are rare (blueprint install: ~10-30 in a few hundred ms;
   steady state: 0). Worker channel serialises. No hot-path or GC impact.
7. **Concurrency** — no new shared state. `decodeBytesObject` is pure;
   `warnOnceCRDDecodeSkip` uses lock-free sync.Map LoadOrStore; expvar publish
   is one-shot via sync.Once. Per `feedback_shared_vs_copy_is_a_concurrency_change`,
   no shared-vs-copy change: Decode builds a fresh Unstructured backed by a
   freshly-allocated map. No mutation of the informer's bytesObject.

### 10.2 Why no flag

Per `feedback_no_park_broken_behind_flag`: the defect is correctness, not
performance. ON by default, no opt-out.

### 10.3 UPDATE-specific risk

The UPDATE handler re-fires `DiscoverGroupResources` on every CRD
UPDATE. `DiscoverGroupResources` is per-group singleflighted
(`discovery_lookup.go:228+`) so concurrent UPDATEs on the same group
collapse to one discovery hop. The dirty-mark side-effect
(`OnResourceTypeAvailable`) is no-op-on-empty
(`deps.go:692-698`). Worst-case cost on a UPDATE storm: one decode + one
singleflight wait + one map iteration per event. No goroutine spawn, no
new shared state. Bench profile from prior CRD ops shows < 1ms per
UPDATE; under 30 UPDATEs/sec (an aggressive customer scenario) the
cumulative is < 30ms/sec on the worker. Safe.

### 10.4 DELETE-specific risk (HIGHEST IN SHIP L)

DELETE teardown is the only Ship L code path that mutates `ResourceWatcher`
internal state (`RemoveResourceType` deletes from `rw.informers`, closes
`rw.informerStop[gvr]`). Failure modes:

1. **Wrong GVR torn down** (a real GVR mis-identified as one of the deleted
   CRD's versions). Mitigation: GVR is derived strictly from the deleted
   CRD's `spec.group + spec.names.plural + spec.versions[i].name` —
   structural, no string-build. The same derivation
   `cache_mode.go:310-321` uses successfully today.

2. **Race vs. CRD re-CREATE under the same name** (operator install →
   uninstall → reinstall). `RemoveResourceType` is idempotent and the next
   CRD ADD walks through `EnsureResourceType` which constructs a FRESH
   standalone informer (per `watcher.go:1300-1308`). Safe under interleave
   — the AddFunc → submitCRDLifecycleEvent(ADD) ordering is preserved by
   the single worker queue (single-writer to the discovery side-effect),
   and `RemoveResourceType` + the next `EnsureResourceType` are both
   `rw.mu`-serialised.

3. **Tearing down an informer that has in-flight LIST/WATCH responses** —
   `RemoveResourceType` closes the per-GVR stop channel which causes the
   Run goroutine to exit (per `watcher.go:1281-1284`). In-flight LIST
   responses are drained; in-flight L1 dispatcher reads against this GVR
   return stale data once and then re-resolve against the apiserver
   (which returns 404 for the now-deleted CRD's resources). Customer-
   acceptance: a deleted CRD's UI cells correctly transition to "no
   resources" once the apiserver responds 404. Bench scope: tester should
   verify the post-DELETE poll converges to a CRD-not-found error state,
   NOT to a stuck-stale state. Add as Phase 6 post-deploy verification
   item.

4. **DELETE storm during clean-uninstall** — a blueprint uninstall might
   delete N CRDs in burst. Each DELETE enqueues one event onto the
   `crdDiscoveryQueueDepth=256` channel (`crd_discovery_side_effect.go:65`).
   At realistic blueprint scale (~10-30 CRDs) the queue is fine. At
   pathological scale (256+ concurrent CRD deletes) events spill to
   `eventsDropped` with a WARN — the dropped events are recovered on the
   next controller-health rebuild (which scans the apiserver and would
   notice the missing GVRs on its next pass).

5. **navDiscoveredGroups stays populated for the deleted group** —
   documented in §3b inline. The `removable-discriminator` predicate at
   `watcher.go:749 + :1074` stays "true" for the deleted group, so a
   re-CREATE under the same group correctly gets a standalone informer
   (rather than a shared-factory one). This is the SAFE direction: any
   re-CREATE under the same group works. The cost: the group remains in
   the predicate set even if no CRD currently exists for it — a small
   memory footprint, no behavioural drift. Followup item to surface to PM
   (§11).

6. **`Global()` returns nil under test** — `RemoveResourceType` has a nil-
   receiver guard at `watcher.go:1318`. Safe.

7. **The recover wrapper** at the top of `triggerCRDDelete` (mirrors
   `triggerCRDDiscovery`'s wrapper) catches any panic — e.g. an unexpected
   shape inside `unstructured.NestedSlice` on `spec.versions`. The worker
   keeps draining; the pod stays up.

### 10.5 (deduped — see §10.2 for the no-flag stance)

---

## 11. Open questions for Diego

Resolved by amendment (Q2 + Q3 ratified, lint bundled into Ship L, comment
updated in commit-2). New questions surfaced by UPDATE + DELETE design:

1. **`navDiscoveredGroups` stays append-only on CRD DELETE.** Spec §3b
   chooses NOT to remove the group on DELETE because the predicate is
   load-bearing for the re-CREATE informer-construction path. Net effect:
   memory footprint of the set grows monotonically over a pod lifetime
   (~tens of strings; bounded by the number of CRD groups ever installed).
   Acceptable? Or should Ship L+1 introduce `RemoveNavigationDiscoveredGroup`
   with a re-CREATE corner-case test? Architect recommends Ship L+1 — keeps
   Ship L's blast radius bounded.

2. **`secrets_informer.go` / `controller_health.go` AddFunc bodies use
   `_ interface{}` and don't read content** — the lint rule (§6.1) would
   not flag them. Should we tighten the lint to also flag any AddFunc that
   does NOT call any decode/access helper (i.e. require explicit content-
   handling)? Architect recommends NO — the `_ interface{}` pattern is
   already the safest possible signal of "content is not read here".

3. **CRD UPDATE for a version that GOES OUT-OF-SERVED** — Ship L's UPDATE
   handler currently fires `OnResourceTypeAvailable` for the NEW spec's
   served versions. It does NOT fire `OnResourceTypeRemoved` for versions
   that were `served:true` in OLD spec and `served:false` in NEW spec
   (semantically: a version is being retired). Out of scope for Ship L
   (this is a multi-version-CRD retirement scenario; rare). Followup if PM
   confirms customer demand.

4. **Lint regression-fixture file** at
   `scripts/lint/testdata/regression_unchecked_assert.go` needs a
   `//go:build ignore` so `go build ./...` skips it (it's intentionally
   incorrect code). Architect confirms this is in the dev's checklist; PM
   flags if not.

---

## 11.1 Open question deep-dive

Diego requested expanded reasoning per OQ before ratification. Each
deep-dive below restates the OQ, enumerates concrete impact, cites the
file:line evidence, walks each option with cost/benefit/risk, gives the
architect's recommendation, and notes what changes if the other option is
picked.

---

### OQ1 — `navDiscoveredGroups` append-only on DELETE; defer remove primitive to L+1

**What the OQ asks.** On CRD DELETE, Ship L deliberately does NOT remove
the deleted CRD's `spec.group` from the process-scoped `navDiscoveredGroups`
set (`discovery_lookup.go:65-68`). Should Ship L instead introduce a
`RemoveNavigationDiscoveredGroup` primitive and call it at the end of
`triggerCRDDelete`, or is "append-only with monotonic growth" acceptable?

**What's at stake.** Two concrete properties of the runtime depend on the
membership of `navDiscoveredGroups`:

1. The **removable-discriminator predicate** at `watcher.go:749` (metadata-
   only informer path) and `watcher.go:1074` (dynamic full-informer path).
   The predicate is `standalone := IsNavigationDiscoveredGroup(gvr.Group)`
   — when true, the watcher constructs a **standalone informer** via
   `dynamicinformer.NewFilteredDynamicInformer` (line 1076-1083) or
   `metadatainformer.NewFilteredMetadataInformer` (line 751-758); when
   false, the watcher uses the **shared-factory informer** via
   `rw.factory.ForResource(gvr)` (line 1085) or
   `rw.metaFactory.ForResource(gvr)` (line 771).
2. The construction branch is **load-bearing for teardown correctness**:
   the doc-comment at `watcher.go:984-986` (mirrored at `:1058-1060`)
   states "a factory-built informer torn down by RemoveResourceType would
   be handed back — stopped and frozen — by a later EnsureResourceType
   (CRD delete→recreate)". Only standalone informers can be safely re-
   constructed after teardown. If we remove a group from
   `navDiscoveredGroups` between DELETE and a subsequent re-CREATE, the
   re-CREATE's `EnsureResourceType` would route through the SHARED-FACTORY
   branch — producing a frozen-stopped informer on the next re-DELETE-then-
   re-CREATE cycle.

**Memory bound.** Each entry is a Go map entry keyed by a CRD group string
(typical lengths: `composition.krateo.io`, `argoproj.io`, ~20-30 bytes
each). Map overhead per entry is ~50-100 bytes amortised. **Realistic
upper bound: ~10,000 distinct CRD groups ever installed on the lifetime
of a single pod ≈ ~1 MB of process RSS, max.** At realistic customer
scale (Krateo blueprint catalog is ~30-50 CRDs across ~3-5 groups; even
the 29,907 ArgoCD Applications scenario per `project_argocd_apps_scale.md`
operates within ~10 distinct groups), the footprint is **tens of strings,
single-digit KB**.

**Evidence:**
- Predicate definition: `discovery_lookup.go:112-117`.
- Predicate consumers: `watcher.go:749` + `:1074`.
- Construction branches: `watcher.go:751-758` (standalone metadata),
  `:1076-1083` (standalone dynamic), `:771` + `:1085` (shared-factory).
- Append-only contract: `discovery_lookup.go:52-68` ("populated only by
  AddNavigationDiscoveredGroup"); `phase1.go:306` ("set is append-only
  during Phase 1 — RemoveResourceType is wired only").
- Frozen-on-recreate hazard doc: `watcher.go:984-986` + `:1058-1060`.
- No production caller of any remove-from-set primitive (grep
  `RemoveNavigationDiscoveredGroup` returns zero hits).

**Option A — add `RemoveNavigationDiscoveredGroup` in Ship L.**
- **Cost:** ~15 LOC for the primitive + ~80 LOC for the re-CREATE corner-
  case falsifier test (`TestCRDDelete_ThenReCreate_InformerLifecycleCorrect`)
  that asserts: DELETE → group removed → re-CREATE → standalone informer
  reconstructed correctly → DELETE again → no frozen-factory entry. This
  test must reach inside `rw.factory` / `rw.metaFactory` internals to
  verify which branch fired — touching factory test-internals raises the
  test fragility surface.
- **Benefit:** Eliminates the monotonic growth (single-digit KB worst-case
  → 0). Cleaner mental model — set membership tracks live CRDs.
- **Risk:** The factory `ForResource` cache is keyed by GVR and has no
  eviction API per `watcher.go:741-742`. If the re-CREATE happens BEFORE
  `RemoveNavigationDiscoveredGroup` (race: DELETE worker enqueues remove,
  user re-creates the CRD, AddFunc fires before remove completes), the
  re-CREATE could observe `IsNavigationDiscoveredGroup`=true (good) — but
  if the remove fires AFTER the re-CREATE's `EnsureResourceType`, we now
  have an inconsistent state (set says "not navigation-discovered" but a
  standalone informer is wired). Mitigation requires either
  (a) serialise remove with the AddFunc → bigger lock surface, or
  (b) skip remove if a registered informer for any GVR in this group still
  exists → predicate becomes "no live informer for any GVR in group".
  Either pulls non-trivial coupling.

**Option B — defer to Ship L+1 (architect recommendation).**
- **Cost:** ~1 MB upper-bound process RSS over multi-year pod lifetime;
  in realistic deployments ~single-digit KB. Slightly leakier mental model.
- **Benefit:** Ship L stays at ~535 LOC and avoids the race/coupling
  surface above. The DELETE path's blast radius is bounded to
  `RemoveResourceType` + `OnResourceTypeRemoved` — both already idempotent
  and well-tested.
- **Risk:** LOW. The monotonic growth is below any meaningful operational
  threshold.

**Recommendation: Option B (defer).** The growth bound is empirically
negligible; the race surface from Option A is real but soluble; soluble in
~2 days of careful design at Ship L+1 vs ~2 hours of pressure in Ship L.

**What changes if Diego picks Option A.** Ship L LOC jumps from ~535 to
~630 (+95). The 6-commit cadence gains commit-4b
(`feat(cache): Ship L — RemoveNavigationDiscoveredGroup + re-CREATE corner-case
test`). Two architect-review items move to checklist: race-window analysis
between the DELETE worker and a concurrent AddFunc, and a `-race` test on
the corner case. Dev hours: +6-8.

---

### OQ2 — Lint rule does NOT flag `_ interface{}` AddFuncs that read content elsewhere

**What the OQ asks.** The §6.1 lint flags raw
`obj.(*unstructured.Unstructured)` asserts in literal handler bodies
(AddFunc/UpdateFunc/DeleteFunc). Should it ALSO flag any handler whose
parameter is `_ interface{}` (discard) when that handler indirectly causes
content reads via a downstream rebuild path?

**What `_ interface{}` means semantically.** Go's blank identifier `_` as
a parameter name DECLARES the parameter exists (so the function signature
satisfies the
`clientcache.ResourceEventHandlerFuncs.AddFunc func(obj interface{})`
contract — see `client-go/tools/cache/shared_informer.go`) but FORBIDS any
read of that value in the function body — `_` is not addressable, not
assignable, and not readable in any expression. The compiler enforces
this. Therefore, NO content read of the informer event payload is
syntactically possible inside a `func(_ interface{}) { … }` body.

**Concrete codebase sites.** Two AddFunc bodies use this pattern:

- `controller_health.go:351-353`:
  ```go
  AddFunc:    func(_ interface{}) { scheduleControllerHealthRebuild() },
  UpdateFunc: func(_, _ interface{}) { scheduleControllerHealthRebuild() },
  DeleteFunc: func(_ interface{}) { scheduleControllerHealthRebuild() },
  ```
- `secrets_informer.go:222-224`: identical pattern, calls
  `scheduleSecretsRebuild()`.

Both call a `scheduleXRebuild()` function that sets a dirty bit and
returns. The actual rebuild runs on a detached goroutine that walks the
informer's indexer (`inf.GetIndexer().List()`) — and at THAT point the
indexer returns `*bytesObject` or typed objects which are then routed
through `decodeBytesObject` / `fallbackUnstructuredFromIndexer` (already
verified bytes-safe in §6's audit table).

**False-positive risk of flagging `_ interface{}`.**
- Both sites are CORRECT today. They don't read content in the handler;
  the rebuild does, and the rebuild's read path is bytes-safe.
- Flagging them as "MUST call decodeBytesObject" would force one of:
  (a) change to `func(obj interface{}) { _ = obj; scheduleXRebuild() }` —
  cosmetic, no semantic gain; or
  (b) add `decodeBytesObject(obj)` and discard the result — defeats the
  whole point of the scheduler pattern (`scheduleXRebuild` is meant to
  be O(1) atomic; adding an unneeded ~hundreds-µs decode hurts throughput
  on storm events); or
  (c) add an allowlist entry — drifts toward the lint becoming "list of
  exceptions" rather than "principle".

**When WOULD `_ interface{}` need scrutiny?** Only if a handler body
CLAIMED to be O(1) scheduler-pattern but actually invoked content-reading
code transitively. The lint is a static-AST check and cannot prove
transitive call-graph properties. The remediation for the transitive
class is code review + the §6 audit table (which is itself an artifact in
the repo). The lint's job is to catch the IMMEDIATE syntactic regression
— raw assert in a literal handler body. The transitive class is
handled by the architectural rule: any function that receives an
`interface{}` from an informer-event-source must consume it via
`decodeBytesObject` / `metaNSName` (documented at
`deps_watch.go:166-177`).

**Option A — tighten lint to flag `_ interface{}`.**
- **Cost:** ~20 LOC of lint AST logic + ~2 allowlist entries
  (`controller_health.go`, `secrets_informer.go`).
- **Benefit:** Tighter "principle" — every AddFunc must explicitly
  acknowledge the obj.
- **Risk:** Allowlist drift over time (every new schedule-rebuild handler
  needs an allowlist entry); false-positive friction without catching
  any real regression class.

**Option B — keep lint at "raw-assert" scope (architect recommendation).**
- **Cost:** None.
- **Benefit:** Lint catches the one regression class it was designed to
  catch; no allowlist drift; no friction on new scheduler-pattern handlers.
- **Risk:** None — the `_ interface{}` syntax is its own safest signal.

**Recommendation: Option B.** `_ interface{}` is the strongest possible
syntactic guarantee that no content is read. There is no false-negative
class to mitigate.

**What changes if Diego picks Option A.** Lint gains ~20 LOC + 2
allowlist entries. The §6.1 dual-state shifts: the lint MUST pass on
Ship-L HEAD with the 2 allowlist entries present; absence of either
allowlist entry causes lint failure. Dev hours: +1.

---

### OQ3 — CRD UPDATE retiring a served version (served:true→false) stays out of Ship L

**What the OQ asks.** A CRD's `spec.versions[]` is a list of version
sub-objects, each with a `served:bool` field
(`apiextensions.k8s.io/v1.CustomResourceDefinitionVersion`). When
`served` transitions from `true` to `false` (the cluster operator retires
a version), Kubernetes stops serving that GVR; existing informers will
WATCH-404 indefinitely. Ship L's UPDATE handler (§3a) currently fires
`OnResourceTypeAvailable` for each NEW spec's served version — it does
NOT fire `OnResourceTypeRemoved` for OLD versions that have become
unserved. Should Ship L handle the retirement transition?

**The retirement scenario.** A customer chart bump (e.g.
`composition.krateo.io/v1alpha1` → `v1`) typically:
1. Adds `v1` to `spec.versions[]` with `served:true,storage:true`.
2. Leaves `v1alpha1` in `spec.versions[]` with `served:true,storage:false`
   (so existing v1alpha1 objects can still be read).
3. Optionally, a later chart bump flips `v1alpha1` to `served:false`
   when the operator confirms migration is complete.

Per Kubernetes CRD lifecycle docs, step 3 is a CRD UPDATE event — not a
CRD DELETE. The full CRD object remains; only the `spec.versions[].served`
field changes. Snowplow's informer for `v1alpha1` will then start
receiving 404 from the apiserver on its WATCH and enter a
sticky-broken state per `controllerHealthWatchBroken` (see
`controller_health.go:366+`).

**Walkthrough — what happens today (with Ship L, but without retirement
handling).** UpdateFunc fires. The Ship L handler decodes the new CRD,
extracts `spec.versions[]`, calls `OnResourceTypeAvailable` for each
served version. The retired version (`v1alpha1` now `served:false`) is
SKIPPED by the cache_mode.go-pattern filter at
`cache_mode.go:314 (if !v.Served { continue })`. No `OnResourceTypeRemoved`
fires for it; `RemoveResourceType` is not called; the per-resource
informer for `v1alpha1` keeps running and keeps 404'ing on WATCH. L1
entries that LIST-depend on `v1alpha1` stay populated with stale data
until the L1 TTL expires or the cell is dirty-marked by some other
event.

**Symptom severity.**
- WATCH-404 log noise (sticky-broken state surfaces in
  `controller_health` indicator).
- Stale L1 data for `v1alpha1`-keyed cells until TTL.
- No outright crash; no piechart-stuck-at-0; not on the customer's
  install-burst critical path.

**Comparison to ADD/DELETE defects fixed in Ship L.**
- ADD: stuck-zero-state for entire 300s+ Phase 6 window → CUSTOMER-FACING
  CONVERGENCE FAILURE.
- DELETE: leaked informer goroutine + stale L1 + WATCH-404 storm until
  controller-health auto-heals → operational noise + log spam.
- UPDATE retirement: stale L1 for one version + WATCH-404 on one
  informer → narrow operational noise, single version, transient.

**Customer demand check.** Per the Krateo blueprint catalog, version
retirement is rare (operators tend to leave v1alpha1 served-true for
extended overlap periods). The `project_composition_install_rbac_scale`
memory file mentions composition controllers installing N compositions,
not version retirements. Customer-acceptance impact: low.

**Effort estimate for the followup (call it Ship L+2).**
- UpdateFunc handler in `triggerCRDDiscovery` (UPDATE kind) needs to
  compare OLD and NEW `spec.versions[]` and detect retirement
  transitions. This requires CHANGING `submitCRDLifecycleEvent(newObj,
  crdLifecycleUpdate)` → `submitCRDLifecycleEvent(oldObj, newObj,
  crdLifecycleUpdate)` (capture both shapes).
- For each version present in OLD with `served:true` but absent or
  `served:false` in NEW, derive the GVR and call `RemoveResourceType` +
  `OnResourceTypeRemoved` (same as DELETE-per-version).
- Falsifier: `TestCRDUpdate_RetiresServedVersion_TearsDownInformer`.
- LOC: ~50 fix + ~80 test = ~130 LOC.
- Effort: ~1 day of focused dev work.

**Option A — fold retirement handling into Ship L.**
- **Cost:** +~130 LOC (LOC budget jumps to ~665). +1 day dev. UpdateFunc
  worker-queue payload widens (must carry both oldObj and newObj — a small
  but real change to `crdDiscoveryEvent`).
- **Benefit:** One complete lifecycle ship. Reduces "operational noise"
  symptom class to zero in one go.
- **Risk:** Falsifier needs to construct a TWO-event sequence (initial
  ADD with v1alpha1 served-true; UPDATE with v1alpha1 served-false). The
  test fixture is larger and has more failure modes.

**Option B — defer to Ship L+2 (architect recommendation).**
- **Cost:** Customer keeps the WATCH-404 noise on the rare retirement
  event until L+2.
- **Benefit:** Ship L's scope is bounded; the critical
  install-burst-stuck-0 defect lands faster.
- **Risk:** None — the retirement symptom is narrow and not customer-
  facing.

**Recommendation: Option B.** Severity is operational-noise, not
customer-facing. Ship L's purpose is the install-burst stuck-0 fix;
widening it for a low-severity edge case dilutes the ship's clarity.

**What changes if Diego picks Option A.** Ship L LOC: ~665.
`crdDiscoveryEvent` gains `oldObj interface{}` (in addition to `obj`).
UpdateFunc wiring in `deps_watch.go` captures both. A new
`triggerCRDDiscoveryUpdate(oldObj, newObj)` function emerges (with its
own panic-recover wrapper, mirroring DELETE). Commit cadence: insert a
new commit-3b after the UPDATE commit-3 for the retirement-handling
diff. Dev hours: +6-8.

---

### OQ4 — Lint regression-fixture file `//go:build ignore` tag

**What the OQ asks.** The Ship L lint at
`scripts/lint/no_unchecked_unstructured_assert.go` ships with a
**regression-fixture file** at
`scripts/lint/testdata/regression_unchecked_assert.go` containing the
forbidden raw assert. The lint must FAIL on the fixture. Does the
fixture file need `//go:build ignore`, and what happens if it doesn't?

**`//go:build ignore` semantics.** Go's build-constraint system
(`go/build`, defined at golang.org/ref/spec#Build_constraints) reads the
`//go:build` directive at the top of a `.go` file as a boolean
expression. The token `ignore` is not a defined GOOS/GOARCH/tag — by
convention it is a tag that nobody passes via `-tags=...`. So
`//go:build ignore` evaluates to false for every standard invocation
(`go build`, `go vet`, `go test`), and the file is EXCLUDED from those
build sets. The file remains a valid `.go` file that `go run` can target
explicitly with the filename argument.

**Codebase precedents.**
- `scripts/lint/no_parallel_binding_derivation.go:60` — `//go:build ignore`.
- `scripts/sa-endpoint-shape-proof.go:26` — `//go:build ignore`.

Both are standalone scripts invoked via `go run <file>.go`. Without the
tag, `go build ./...` and `go vet ./...` would compile them — and since
neither is part of an importable package (and both `package main`), the
top-level build would fail with duplicate-`main` errors.

**The regression-fixture case.** The fixture file is INTENDED to contain
incorrect code — a raw `obj.(*unstructured.Unstructured)` inside a
literal handler body. Without `//go:build ignore`:

1. **`go build ./...`** would compile `scripts/lint/testdata/...`. The
   fixture file declares its own `package`. If it imports `k8s.io/.../
   unstructured` and uses the assert legitimately, it would compile —
   but `go vet ./scripts/...` would fail on it, since `vet` flags
   suspicious patterns. More likely: the file's package conflicts
   with sibling files in the same directory (if any), or it pulls
   imports that don't resolve under the `testdata/` convention.
2. **`go test ./...`** — `testdata/` directories are ALREADY excluded by
   the `go` tooling (per `cmd/go/internal/load/pkg.go` — directories
   named exactly `testdata` are skipped by package discovery). So
   `go test ./...` would NOT process the fixture. This is the SAFETY
   NET that makes the missing `//go:build ignore` non-catastrophic.
3. **The lint itself** invokes `go/parser` directly with the fixture
   filename argument (or walks `scripts/lint/testdata/`). The parser
   does not enforce build constraints — it parses any `.go` file. So
   the lint sees the fixture either way.

**Concrete failure modes if `//go:build ignore` is absent.**
- `go build ./...` and `go vet ./...` exclude `testdata/` by default —
  so the absence does NOT cause a top-level build failure.
- Some IDE tooling (gopls, goimports) DOES process `testdata/` files
  for hover/diagnostics. The fixture would surface compiler/lint
  squiggles in the dev's editor — annoying but not breaking.
- A future refactor moving the fixture OUT of `testdata/` (e.g. to
  `scripts/lint/fixtures/`) would lose the testdata-directory escape
  and immediately break `go build ./...`. The `//go:build ignore` tag
  is the resilient guard against this refactor.

**Why "PM flag at dual-gate" rather than hard pre-implementation
requirement.** The fixture being inside `testdata/` is already a
sufficient escape hatch for the immediate ship (per (2) above). The
`//go:build ignore` tag is a defence-in-depth measure against future
refactors. The architect treats it as a SHOULD (best-practice) not a
MUST (correctness). The PM dual-gate is the right surface to catch a
missing best-practice marker without blocking the ship if it's omitted.

**Option A — make `//go:build ignore` a HARD requirement (architect MUST
verify pre-commit).**
- **Cost:** One extra architect-checklist item.
- **Benefit:** Defence-in-depth against future refactors out of
  `testdata/`.
- **Risk:** None.

**Option B — keep as a PM dual-gate SHOULD (architect recommendation).**
- **Cost:** A future refactor could break `go build ./...` if the
  fixture is moved out of `testdata/` without the tag. Catchable by CI
  on the refactor PR.
- **Benefit:** Slightly looser pre-commit checklist.

**Recommendation: Option A.** This deep-dive surfaces that
`//go:build ignore` is the established codebase pattern at
`scripts/lint/no_parallel_binding_derivation.go:60` and
`scripts/sa-endpoint-shape-proof.go:26`. Consistency outweighs the
small checklist cost.

**NEW FINDING** (surfaced by this deep-dive): the original spec at §6.1
already requires the tag at line 1178; the architect-review checklist
at §8.1 line 937 mentions it as item 12. But the §11 OQ summary
described it as "PM flags if not" — implying a dual-gate SHOULD. The
two surfaces drift. Recommend tightening §11 entry 4 to MUST-be-checked
by both architect AND PM, matching the §8.1 item 12 phrasing.

**What changes if Diego picks Option B.** Nothing in the diff; the
checklist phrasing in §8.1 item 12 softens from "Regression fixture
file HAS `//go:build ignore`" to "Regression fixture file SHOULD have
`//go:build ignore`". Architect would still call this out at dual-gate
review.

---

## 11.2 OQ1 worked-examples deep-dive

Diego requested concrete sequences. Per-scenario walkthrough below traces
the exact code path file:line and contrasts Option A (remove-on-DELETE)
vs Option B (append-only) at each step.

### LOAD-BEARING FINDING surfaced by tracing actual paths (NEW FINDING)

Before walking the scenarios, one structural fact reshapes the entire OQ1
question. Reading `addResourceTypeLocked` end-to-end at `watcher.go:966-1087`:

- Line 1035 (Ship H5 routing inversion): `if !isStreamingException(gvr) && compositionStreamingListEnabled() { … newStreamingDynamicInformer(rw.restConfig, rw.dyn, gvr, …) …; gi = sgi }`.
- `newStreamingDynamicInformer` (`streaming_list.go:186-205`) returns `(nil, false)` ONLY when `restConfig == nil || dyn == nil` OR the REST client build fails. In production both are wired at boot.
- Line 1073 `if gi == nil { standalone := IsNavigationDiscoveredGroup(gvr.Group); … }` — the predicate's consumer on the dynamic full path.

For every composition GVR in production:
1. `newStreamingDynamicInformer` succeeds (restConfig + dyn non-nil; REST client build is reliable for any valid GVR).
2. `gi` is assigned the **standalone** `*streamingDynamicInformer` (the streaming informer is BY DEFINITION standalone — it has its own `ListWatch` and is never returned by `rw.factory.ForResource`).
3. The `if gi == nil` block at `:1073-1087` is **NEVER ENTERED** for composition GVRs.
4. Therefore `IsNavigationDiscoveredGroup` is **dead code** for the dynamic full-Unstructured path in production.

The metadata-only path at `:749` also has `IsNavigationDiscoveredGroup`,
but reading `shouldUseMetadataOnly` at `cache_mode.go:149-203`:
- Rule 1 (`:156-158`) returns false for the `rbac.authorization.k8s.io` group.
- Rule 2 (`:190`) requires `isStreamingException(gvr)` to be TRUE; that is true ONLY for the 4 typed-RBAC GVRs.
- Combined: Rule 1 already eliminates the RBAC group, so Rule 2 never finds a match. The function never reaches `return true` in production.
- The author's own comment at `cache_mode.go:180-185` is explicit: "post-H5 only the 4 typed-RBAC GVRs are non-streaming, and Rule 1 already returns false for those — **so the metadata-only path is inert (no GVR reaches a return true)**. The metadata-only mechanism is superseded; a later dead-code-removal ship deletes it."

**Therefore `IsNavigationDiscoveredGroup` is also dead code on the
metadata-only path in production.** The only path where the predicate is
LIVE is when `RESOLVER_COMPOSITION_STREAMING_LIST=false` (env-disabled
streaming) — a bench-comparison switch, not a customer mode (chart
default is ON; see `project_single_cache_flag_direction.md`).

**Impact on OQ1.** The original concern — "removing the group from
`navDiscoveredGroups` would silently change the re-CREATE informer
lifecycle from standalone to shared-factory" — is **a documentation-
preserved invariant** but **not a live production hazard** under H5. The
standalone informer is selected by the streaming-first path at
`:1035-1047` independently of the predicate. The predicate's
load-bearing role is preserved only for the
`RESOLVER_COMPOSITION_STREAMING_LIST=false` fallback case.

The walkthroughs below cite this finding at every step.

---

### Example A — Vanilla install/uninstall (DOMINANT customer pattern)

**Customer narrative.** Alice deploys snowplow into a fresh cluster. At
T+5m she helm-installs the `github-scaffolding-with-composition-pages`
blueprint. At T+10m she creates 50 compositions via the portal. At
T+1h she uninstalls.

**Timeline:**

| T (mm:ss) | Action | Code path | navDiscoveredGroups |
|---|---|---|---|
| 00:00 | Pod boot; empty cluster | `main.go` → `NewResourceWatcher` (restConfig+dyn wired). Only RBAC GVRs registered. | `{}` |
| 05:00 | helm install creates CRD | apiserver fires CRD ADD to snowplow's CRD informer | `{}` |
| 05:00.005 | AddFunc at `deps_watch.go:201` → `IsCRDGVR(gvr)` true → `submitCRDLifecycleEvent(obj, crdLifecycleAdd)` → worker dequeues → `triggerCRDDiscovery(*bytesObject, ADD)` with Ship L's `decodeBytesObject` per §3 | extracts `spec.group="composition.krateo.io"` | `{}` |
| 05:00.010 | `AddNavigationDiscoveredGroup("composition.krateo.io")` (`discovery_lookup.go:87-102`) — first call → adds + logs | mutates set | **`{composition.krateo.io}`** |
| 05:00.015 | `DiscoverGroupResources` → `EnsureResourceType(gvr_v122)` (`watcher.go:571`) → miss → `addResourceTypeLocked` (`:966`) | `shouldUseMetadataOnly` false (inert); streaming-first at `:1035` succeeds; `gi := sgi` standalone streaming informer; `:1073` block NOT ENTERED | unchanged |
| 05:00.020 | Informer Run goroutine spawned; LIST → 0 compositions; sync channel closed | informer healthy | unchanged |
| 10:00 | 50 compositions created via portal | apiserver fires 50 ADDs to composition informer; `Deps().OnAdd` dirty-marks piechart cell | unchanged |
| 10:05 | Portal piechart `/call` | L1 dispatcher revalidates dirty cell → resolver lists 50 → returns 50 | unchanged |
| 60:00 | helm uninstall — apiserver deletes CRD | apiserver fires CRD DELETE | `{composition.krateo.io}` |
| 60:00.005 | DeleteFunc at `deps_watch.go:225+` → `submitCRDLifecycleEvent(obj, crdLifecycleDelete)` → worker → `triggerCRDDelete` | decodes bytesObject; for each served version derives GVR | unchanged |
| 60:00.010 | `rw.RemoveResourceType(gvr_v122)` (`watcher.go:1317`) + `Deps().OnResourceTypeRemoved(gvr_v122)` (`deps.go:716`) | per-GVR stop channel closed → informer Run exits; `delete(rw.informers, gvr_v122)`; L1 piechart cell dirty-marked | unchanged |
| 60:00.015 | **Option A:** `RemoveNavigationDiscoveredGroup("composition.krateo.io")` | set drops the entry | **`{}`** |
| 60:00.015 | **Option B (Ship L default):** no remove | set unchanged | `{composition.krateo.io}` |
| 60:01 | Next piechart `/call` | revalidates; no composition informer; fallthrough to apiserver LIST → 404 (CRD gone) → cell = 0 / "no resources" | unchanged |

**Customer-visible outcome:** **IDENTICAL.** Piechart correctly transitions
to 0 under both A and B.

**Set memory footprint:** A=0; B=1 entry (~30-100 bytes amortised).

**Verdict A:** A and B indistinguishable on the dominant flow.

---

### Example B — Install → uninstall → re-install with SAME versions

**Customer narrative.** After Example A's uninstall at 60:00, Alice
re-installs the SAME chart at T+2h. CRD `spec.versions[]` is identical:
`[{name:"v1-2-2",served:true,storage:true}]`.

| T | Action | Code path | navDiscoveredGroups | Predicate live? |
|---|---|---|---|---|
| 120:00 (B start) | helm install | apiserver re-creates CRD → AddFunc fires | A: `{}` ; B: `{composition.krateo.io}` | n/a yet |
| 120:00.005 | `triggerCRDDiscovery` decodes; `AddNavigationDiscoveredGroup` — A: first-call (adds); B: idempotent no-op (`discovery_lookup.go:91-94`) | A: `{}`→`{group}` ; B: unchanged | true after add in both | n/a |
| 120:00.010 | `EnsureResourceType` → miss → `addResourceTypeLocked` → streaming-first at `:1035` succeeds; `gi := sgi` (standalone streaming informer); `:1073` block NOT ENTERED | unchanged | predicate dead-code under H5 default |

**Customer-visible outcome:** **IDENTICAL.** Streaming-first path always
wins regardless of predicate state.

**Verdict B:** A and B indistinguishable. The predicate truly does NOT
govern the construction path in this scenario — verified by tracing.

---

### Example C — Install → uninstall → re-install with DIFFERENT versions

**Customer narrative.** Same as Example B but the bumped chart ships
`spec.versions[] = [{name:"v1-2-3",served:true,storage:true}]`.

| T | Action | Code path | navDiscoveredGroups |
|---|---|---|---|
| 180:00 | helm install bumped chart | CRD created; AddFunc fires | A: `{}` ; B: `{group}` |
| 180:00.005 | `triggerCRDDiscovery` decodes; group same; `AddNavigationDiscoveredGroup` — A: adds; B: idempotent | A: → `{group}` ; B: unchanged |
| 180:00.010 | `DiscoverGroupResources` returns v1-2-3 GVR (fresh); `EnsureResourceType(gvr_v123)` — miss (never registered) → `addResourceTypeLocked` → streaming-first succeeds → standalone streaming informer for v1-2-3 | unchanged |

**Customer-visible outcome:** **IDENTICAL.** Same as Example B.

**Verdict C:** A and B indistinguishable.

---

### Example D — Concurrent re-create during DELETE handler (the "race")

**Worker model — critical structural fact.** The CRD-discovery worker is
single-threaded by construction (`crd_discovery_side_effect.go:121-154`):
`sync.Once` + ONE spawned goroutine running a `select` loop over
`c.queue` and `c.stopCh`. Events are processed in FIFO order. The
AddFunc/DeleteFunc at `deps_watch.go:201/225` run on the CRD informer's
processor goroutine (also serial per client-go's `shared_informer.go`
processor model). So enqueue order is deterministic and dequeue is
serial.

**Sub-case D1: DELETE-enqueued-before-ADD (typical helm upgrade).**

| T | Worker step | navDiscoveredGroups A | navDiscoveredGroups B |
|---|---|---|---|
| 0ms | DELETE enqueued | `{group}` | `{group}` |
| 1ms | ADD enqueued (queue: [DELETE, ADD]) | `{group}` | `{group}` |
| 2ms | Worker dequeues DELETE → `triggerCRDDelete` runs | unchanged | unchanged |
| 5ms | `RemoveResourceType`; A only: `RemoveNavigationDiscoveredGroup` | `{}` | `{group}` (B does not remove) |
| 6ms | Worker dequeues ADD → `triggerCRDDiscovery` runs | `{}` | `{group}` |
| 7ms | `AddNavigationDiscoveredGroup` — A: adds; B: idempotent no-op | `{group}` | `{group}` |
| 8ms | `EnsureResourceType` → `addResourceTypeLocked` → streaming-first succeeds → standalone streaming informer | identical | identical |

**Outcome D1:** A and B both correct.

**Sub-case D2: ADD-enqueued-before-DELETE (rare; transient operator
mistake — create then immediately remove).** Starting state: no CRD, no
informer.

| T | Worker step | navDiscoveredGroups A | navDiscoveredGroups B |
|---|---|---|---|
| 0ms | ADD enqueued | `{}` | `{}` |
| 1ms | DELETE enqueued | `{}` | `{}` |
| 2ms | Worker dequeues ADD; adds group + builds informer | `{group}` | `{group}` |
| 8ms | Worker dequeues DELETE; `RemoveResourceType` clears informer; A: `RemoveNavigationDiscoveredGroup` | `{}` | `{group}` |
| 9ms | End state: no informer; A clean, B carries one stale entry | A: predicate FALSE; B: predicate TRUE | (predicate dead-code under H5 — observationally identical) |

**Outcome D2:** observationally identical (predicate dead-code in
production).

**Sub-case D3: actually-concurrent A-removes vs B-adds interleave.**

Asked specifically in Diego's prompt: what if AddFunc runs BEFORE
remove? what if AFTER? **The interleave does not exist.** The CRD-
discovery worker is single-threaded; both lifecycle events go through
the same FIFO channel and are processed serially. Channel-send is
non-blocking (`crd_discovery_side_effect.go:175-187`), so the producer
(AddFunc/DeleteFunc on the informer processor) does not block, but the
consumer worker always picks events up in the order they were sent. No
A-vs-B interleave is possible inside the worker.

**The race that §11.1 OQ1 cited does not exist** — that analysis was
**wrong** about the worker concurrency model. Corrected here.

**Outcome D3:** non-issue.

**Verdict D:** A and B produce identical customer-visible outcomes under
all three sub-cases. The original race-safety justification for Option B
was incorrect.

---

### Example E — Customer-scale install/uninstall churn

**Concrete bytes per entry.** Go map entry for a typical CRD group
string (`composition.krateo.io` = 20 chars):
- String header (`reflect.StringHeader`): 16 bytes on amd64.
- Underlying bytes: 20.
- Go runtime hmap bucket overhead: ~144 bytes per 8-entry bucket → ~18 bytes amortised per entry.
- `struct{}` value: 0 bytes.
- **Total per entry: ~54 bytes.**

**Customer scenarios:**

| Scenario | Distinct groups (lifetime) | Memory (Option B) | Memory (Option A) |
|---|---|---|---|
| Single-tenant typical | 3-5 | ~300 bytes | 0 (live = ~150 bytes when CRDs present) |
| Multi-tenant typical | 10-20 | ~1 KB | 0 (live ~500 bytes) |
| 6-month churn @ 1 install/week | ~25 (high dedup) | ~1.4 KB | 0 (live varies) |
| Multi-tenant fleet | 500 | ~27 KB | 0 (live varies) |
| Pathological decade-of-churn | 5000 | ~270 KB | 0 (live varies) |

**Reference points:**
- Snowplow resident set at 50K compositions: 3-7 GiB (per Phase 6).
- Single L1 piechart cohort cell: ~2 KB resident (per trace doc line 73).
- One `*bytesObject` in the composition informer at 50K scale: ~hundreds of KB.

**Conclusion:** Option B's leak is 5-7 orders of magnitude below the
resident working set. **Never measurable.** No operational scenario
produces a meaningful memory effect.

**Verdict E:** Option B's "leak" is unobservable.

---

### Cross-example synthesis

| Scenario | Option A outcome | Option B outcome | Different? |
|---|---|---|---|
| A. Vanilla install/uninstall | piechart=0 post-uninstall | piechart=0 post-uninstall | NO |
| B. Same-version re-install | standalone streaming informer | standalone streaming informer | NO |
| C. Different-version re-install | standalone streaming informer | standalone streaming informer | NO |
| D1. DELETE-then-ADD upgrade | standalone streaming informer | standalone streaming informer | NO |
| D2. ADD-then-DELETE (rare) | no informer | no informer | NO |
| D3. Concurrent race | does not exist (single worker) | does not exist | NO |
| E. 6-month churn | 0 KB | ~1.4 KB | YES (unmeasurable) |

**No scenario produces a different customer outcome.**

### Answers to Diego's 5 questions

1. **Different customer outcome under A vs B?** No. All five scenarios
   identical. OQ1 is purely a memory-hygiene question, not a
   correctness question.

2. **Is the Example D race actually safe under Option B?** The race
   does not exist. CRD-discovery worker is single-threaded by
   construction. The §11.1 OQ1 "race vs concurrent AddFunc" framing was
   wrong — corrected in §11.2 Sub-case D3.

3. **At what customer scale does the leak start mattering?** Never.
   ~1.4 KB at 6-month churn; ~270 KB at decade-extreme; resident set is
   3-7 GiB. 5-7 orders of magnitude below noise.

4. **Could the predicate at `watcher.go:749 + :1074` be REDESIGNED so
   `navDiscoveredGroups` is no longer the discriminator?** It already
   effectively HAS been by Ship H5. The predicate is **dead code** on
   the dynamic full-Unstructured path (streaming-first at `:1035` wins)
   and **dead code** on the metadata-only path (`shouldUseMetadataOnly`
   inert per H5 — `cache_mode.go:180-185`). The predicate's live
   consumer is only the `RESOLVER_COMPOSITION_STREAMING_LIST=false`
   fallback path. A clean **Option C** emerges:

   **Option C (NEW):** retire the dead use at `watcher.go:1073-1087`
   (replace with unconditional standalone construction so the env-flag
   fallback path also gets a standalone informer — matching the H5
   default), and add `RemoveNavigationDiscoveredGroup`. ~30 LOC delta.
   This eliminates the conceptual coupling that motivated OQ1 entirely.

5. **B vs A recommendation after the worked examples?** See below.

### 11.2 final recommendation — REVISED

The worked examples invalidate the original §11.1 framing of OQ1. The
race does not exist; the predicate is dead-code for production; the
memory leak is unmeasurable.

**Updated option matrix:**

- **Option A** (Ship L adds `RemoveNavigationDiscoveredGroup`): ~95 LOC, no customer benefit, no correctness benefit, requires falsifier covering the now-rarely-live `RESOLVER_COMPOSITION_STREAMING_LIST=false` path.
- **Option B** (Ship L defers; append-only): ~0 LOC, ~1.4 KB unmeasurable leak.
- **Option C** (Ship L+1: retire dead predicate use + add remove primitive): ~30 LOC, architecturally clean, eliminates the OQ1 question entirely. Touches `watcher.go` informer-construction path — requires H5-fallback regression suite per `feedback_check_k8s_clientgo_prior_art`.

**Architect-revised recommendation: Option B for Ship L; Option C for
Ship L+1.**

Why not Option C in Ship L: Ship L is a defect-fix gating customer
S4 convergence. Option C is an architectural refactor touching the
highest-risk file in `internal/cache/`. Per
`feedback_no_shortcuts_or_workarounds` and the closed ship loop, refactor
+ defect-fix in one ship is the kind of scope-creep that bites. Defer C.

Why not Option A in Ship L (sharpened reasoning vs §11.1): the original
"race-safety" deferral reason was wrong. The actual reason to defer is
now: Option A adds ~95 LOC of code that produces no customer benefit, no
correctness benefit, and a measurable leak prevention of <1 KB. The cost
exceeds the benefit by orders of magnitude.

**What changes if Diego picks Option A despite the analysis.** Ship L
LOC: +95 to ~630. Architect-review checklist gains item: verify the
falsifier exercises the `RESOLVER_COMPOSITION_STREAMING_LIST=false`
re-CREATE path specifically (not the streaming-first default), since
the streaming default makes the remove operation cosmetically silent.
The falsifier setup is non-trivial — needs an env-flag override harness
in the test that toggles streaming OFF before the re-CREATE.

**What changes if Diego picks Option C in Ship L.** Ship L LOC: +30 to
~565. Commit cadence gains commit-4b
(`refactor(cache): retire dead IsNavigationDiscoveredGroup at watcher.go:1073-1087; add RemoveNavigationDiscoveredGroup`). Risk: touches
the watcher informer-construction path. Falsifier suite required:
proving informer lifecycle correctness under
`RESOLVER_COMPOSITION_STREAMING_LIST=false` (the new dead-stripped
fallback constructs a standalone informer unconditionally — must verify
it does NOT regress to factory). Architect estimates +2 days dev + +1
day falsifier validation. **Not bundled into Ship L because Ship L is
gating customer-acceptance convergence and a refactor of this
load-bearing file warrants its own ship.**

### NEW FINDING surfaced for follow-up tracking

The §11.1 OQ1 race analysis was incorrect. The CRD-discovery worker is
single-threaded; the race the original deep-dive cited cannot occur.
Future architect dispatches should consult `crd_discovery_side_effect.go:121-154`
before reasoning about CRD-lifecycle event concurrency.

A second NEW FINDING: `shouldUseMetadataOnly` (`cache_mode.go:149-203`)
is inert in production post-H5 — the metadata-only routing mechanism is
documented as superseded but still present. A future dead-code-removal
ship should delete it. Independent of Ship L scope; flag for backlog.

---

### Summary of deep-dive findings

After expansion (§11.1 + §11.2):
- **OQ1: REVISED to Option B for Ship L, Option C for Ship L+1.** The
  worked examples (§11.2) revealed the predicate is dead-code under H5
  for production composition GVRs; the race I cited in §11.1 does not
  exist; the leak is unmeasurable. A new Option C (retire dead
  predicate + add remove primitive in Ship L+1) is the clean answer.
- **OQ2: confirmed Option B (keep narrow lint).** `_ interface{}` is
  syntactically the safest signal — no false-negative class.
- **OQ3: confirmed Option B (defer retirement to L+2).** Severity is
  operational-noise, not customer-facing — Ship L scope stays bounded.
- **OQ4: SWITCHED to Option A (HARD requirement, not PM-SHOULD).** The
  deep-dive surfaced the existing codebase precedent at
  `scripts/lint/no_parallel_binding_derivation.go:60` —
  `//go:build ignore` is the established pattern. Inconsistency with
  precedent is a refactor-time landmine. Tighten the spec accordingly.

---

## 12. Deliverable summary

- ADD fix (§3 + §3.5): ~30 LOC.
- UPDATE fix (§3a): ~35 LOC.
- DELETE fix (§3b): ~85 LOC (includes `triggerCRDDelete` + new counters + wiring).
- Falsifiers (§4.2 + §4.4 + §4.5 + tombstone variant): ~150 LOC.
- Expvar publish + struct extension (§5): ~25 LOC.
- Lint rule + regression fixture + Makefile (§6.1): ~210 LOC.
- **Total: ~535 LOC net.**
- No new flags, no new env, no schema changes.
- Tag pair: `snowplow:0.30.246` + `snowplow-chart:0.30.246` lockstep on braghettos.
- Triple falsifier dual-state (ADD/UPDATE/DELETE) proven by dev pre-commit.
- Lint rule catches the regression class going forward.
- Expected outcome: (1) S4 stuck-0 on clean-cluster install-burst eliminated;
  (2) CRD UPDATE (new served versions) wires informers within ~1s;
  (3) CRD DELETE tears down informers + dirty-marks L1 within ~1s;
  (4) `/debug/vars/snowplow_crd_discovery` exposes the bridge state for ops.

---

## AMENDMENT — 2026-06-12 (#201): UPDATE contract drift vs shipped semantics

This section is APPENDED (the §3a text above is preserved verbatim as the
historical spec). It records where the live `triggerCRDDiscovery` UPDATE
handling diverged from the §3a "Contract for UPDATE", traced against
`internal/cache/crd_discovery_side_effect.go` at branch
`ship-0.30.250-paged-list` @ c3988e6.

### What §3a specified (5-step UPDATE contract, lines 281–286)

> 1. Decode `newObj` via `decodeBytesObject`.
> 2. Extract `spec.group` via `unstructured.NestedString`.
> 3. Call `AddNavigationDiscoveredGroup(group)` — idempotent.
> 4. Call `DiscoverGroupResources(ctx, saRC, group)` — singleflighted.
> 5. **Call `Deps().OnResourceTypeAvailable(gvr_for_each_served_version)`**
>    to dirty-mark dependent L1 entries (the schema may have changed;
>    downstream resolvers should re-resolve). The GVR(s) to dirty-mark are
>    derived from `spec.versions[].name` × `(spec.group, spec.names.plural)`.

§3a also showed `processEvent` with TWO separate `case` arms
(`crdLifecycleAdd` / `crdLifecycleUpdate`) and `eventsProcessed.Add(1)` at
the TOP of the function, and stated `triggerCRDDiscovery` would "branch on
UPDATE-only behaviour".

### What shipped (live semantics — TRACED)

`triggerCRDDiscovery(obj, kind)` (`crd_discovery_side_effect.go:306-401`)
handles `crdLifecycleAdd` and `crdLifecycleUpdate` **identically** — there
is no UPDATE-only branch. The realized steps are:

1. `decodeBytesObject(obj)` — H5-aware decode (matches §3a step 1).
2. `unstructured.NestedString(..., "spec", "group")` (matches step 2).
3. `AddNavigationDiscoveredGroup(group)` (matches step 3).
4. `DiscoverGroupResources(ctx, saRC, group)` (matches step 4).
5. **`invalidateSADiscovery()` (Task #322) + `invalidateCRDSchemaMemo()`
   (Task #323)** — NOT in the original §3a contract; both landed AFTER this
   spec was written (#318-R2).

`processEvent` (`crd_discovery_side_effect.go:191-215`) collapses ADD+UPDATE
into ONE arm — `case crdLifecycleAdd, crdLifecycleUpdate:
triggerCRDDiscovery(ev.obj, ev.kind)` — and bumps `eventsProcessed` at the
END, AFTER the side-effect (Task #85 happens-before fix), not at the top as
§3a sketched.

### The delta (spec − live)

| §3a contract | Shipped | Assessment |
|---|---|---|
| Step 5: explicit `OnResourceTypeAvailable(gvr)` per served version inside the UPDATE handler | **NOT present** as an explicit per-version loop in `triggerCRDDiscovery` | Dirty-marking on UPDATE is achieved INDIRECTLY by two mechanisms, see below |
| Two `processEvent` case arms; per-kind branch in `triggerCRDDiscovery` | Single combined ADD+UPDATE arm; no per-kind branch (`kind` is used only for the log label + decode-skip WARN) | Simplification — ADD and UPDATE genuinely share the discovery path |
| `eventsProcessed.Add(1)` at top of `processEvent` | At the END (Task #85) | Honest "processed == side-effect ran" semantics |
| (not specified) `invalidateSADiscovery` + `invalidateCRDSchemaMemo` tail | Present (#322/#323) | Net-new schema-coherence steps added by a later ship |

### Why the live UPDATE path still dirty-marks dependent L1 (the §3a-step-5 intent is met by other means)

The §3a step-5 GOAL — "downstream resolvers re-resolve after a CRD UPDATE"
— is satisfied without an explicit per-served-version `OnResourceTypeAvailable`
loop in the UPDATE handler:

- **Newly-served versions:** `DiscoverGroupResources` → `EnsureResourceType`
  spawns the informer and, in the `if added` branch
  (`discovery_lookup.go:305`), calls `Deps().OnResourceTypeAvailable(gvr)` —
  this dirty-marks stale-negative LIST deps for the genuinely-new GVR. So a
  new served version on a CRD UPDATE IS dirty-marked, just from the
  discovery side-effect rather than the handler tail.
- **Schema change on an ALREADY-served version (no new GVR):** here
  `EnsureResourceType` returns `added=false`, so `OnResourceTypeAvailable`
  does NOT fire. Re-resolution coherence is instead carried by Task #323's
  `invalidateCRDSchemaMemo()` (forces recompile of the per-GVR CRD schema)
  + Task #322's `invalidateSADiscovery()` (rebuilds the REST mapper). The
  next `ValidateObjectStatus` recompiles from fresh bytes.

NET: an UPDATE that ONLY mutates an existing version's schema without adding
a served version does NOT issue an explicit L1 dirty-mark via
`OnResourceTypeAvailable` (the §3a-step-5 mechanism). It relies on the
schema-memo + discovery-cache invalidation. For the LIST-dep / GET-dep L1
cells whose payload would change with the new schema, the explicit
dirty-mark §3a envisaged is absent. This is the substantive residual delta.

### Status / disposition

- This is a DOCUMENTATION amendment only — no code change is made under
  #201. The shipped behaviour has been live since 0.30.246 and through
  #322/#323 without an observed regression in the Phase 6 bench.
- Whether the residual delta (no explicit per-version `OnResourceTypeAvailable`
  on a same-version schema-only UPDATE) needs closing is a separate
  question for the architect: it is only observable if a customer mutates a
  CRD's schema in place (same `spec.versions[].name`, changed schema) AND
  has live L1 cells whose serialized payload depends on the changed schema.
  If that path matters, the fix is ~5 LOC — an `OnResourceTypeAvailable(gvr)`
  loop over served versions in the `crdLifecycleUpdate` case (mirroring
  `triggerCRDDelete`'s `OnResourceTypeRemoved` loop). Flagged for backlog,
  not actioned in this wave.

---

**End of spec.**
