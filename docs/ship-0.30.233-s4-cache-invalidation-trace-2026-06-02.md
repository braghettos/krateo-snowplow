# Ship 0.30.233 — S4 cache-invalidation defect TRACE & fix design

**Author**: cache-architect
**Date**: 2026-06-02
**Source incident**: Phase 6 / 0.30.232 / run-20260602-064642 S4 ConvergenceTimeout
**Pre-flight artifact ID**: `/tmp/snowplow-runs/0.30.232/run-20260602-064642/proofs/S4.json`
**Status**: ARCHITECT_DESIGN — PM gate pending

---

## 1. Symptom + ground truth

S4 (post-mutation VERIFY) hit `ConvergenceTimeout` after 300s. Cluster: 20
composition CRs Ready=True across `bench-ns-01..20`. Snowplow piechart
RESTAction (`compositions-list` → `dashboard-compositions-panel-row-piechart`)
served `api=0, ui=0` throughout 75 VERIFY polls.

| Signal | Pre-S4 (04:49:26) | Post-S4 (04:54:26) | Post-S4 (04:59:26) |
|---|---|---|---|
| `dep_dirty_mark_total` | 0 | 820 | 2124 |
| `refresh_enqueued` | 0 | 820 | 2124 |
| `refresh_completed` | 0 | 576 | 1172 |
| `evict_delete` | 0 | 0 | 0 |
| `apistage_store_total` | 17 | 40 | 69 |
| `entries` | 262 | 176 | 176 |

**Refresher fires; refreshes complete; piechart key still serves zero-state.**

---

## 2. TRACED facts

### 2.1 The composition-CR informer is NEVER registered

`/debug/vars` `snowplow_plurals_registered_gvrs` (live read 2026-06-02 from
pod `snowplow-76c77d6697-nr588`):

```
count: 30
gvrs:
  apiextensions.k8s.io/v1/customresourcedefinitions
  core.krateo.io/v1alpha1/compositiondefinitions
  rbac.authorization.k8s.io/v1/clusterrolebindings
  rbac.authorization.k8s.io/v1/clusterroles
  rbac.authorization.k8s.io/v1/rolebindings
  rbac.authorization.k8s.io/v1/roles
  templates.krateo.io/v1/restactions
  widgets.templates.krateo.io/v1beta1/* (22 widget GVRs)
```

ZERO entries for `composition.krateo.io/*`. Specifically NO informer for
`composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages`.

### 2.2 `composition.krateo.io` is NEVER added to `navDiscoveredGroups`

Pod logs across the entire pod lifetime (boot at 21:24:26 → present, ~7h42m):

```
$ kubectl logs snowplow-76c77d6697-nr588 | grep "navigation_discovered_group_added"
{"time":"2026-06-01T21:24:26.36775Z", ... "group":"widgets.templates.krateo.io"}
{"time":"2026-06-01T21:24:26.83255Z", ... "group":"core.krateo.io"}
{"time":"2026-06-01T21:24:27.28983Z", ... "group":"apiextensions.k8s.io"}
```

Exactly 3 events, all from boot. No `composition.krateo.io` ever added.

### 2.3 No apiserver fallthrough for `composition.krateo.io`

`/debug/vars` `snowplow_apiserver_fallthrough_cells` enumerates 104 distinct
(callsite × GVR × reason) tuples. ZERO entries contain `composition.krateo.io`
in the GVR slot. ZERO entries contain `githubscaffolding`.

The LIST `/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages`
is **never dispatched** — neither via informer pivot nor via apiserver fallthrough.

### 2.4 CRD ADD event DID fire — dirty-marks DID propagate

```
{"time":"2026-06-02T04:50:02.919554Z","level":"INFO","msg":"cache_event.consumed",
 "type":"ADD","gvr":"apiextensions.k8s.io/v1, Resource=customresourcedefinitions",
 "name":"githubscaffoldingwithcompositionpages.composition.krateo.io",
 "action":"refresh","l1_keys":60}
```

60 L1 keys dirty-marked by the CRD ADD. Refresher consumed them
(`refresh_completed: 0 → 576 → 1172`).

### 2.5 Piechart L1 entry WAS refreshed

Piechart widget L1 key `8df6b577…b8`, `handler=widgets, gvr=piecharts,
name=dashboard-compositions-panel-row-piechart`:

```
04:47:00 hit:false resident_bytes:0    (cold MISS)
04:47:12 hit:true  resident_bytes:2211 (cold-MISS-Put)
... (stable at 2211 through 04:49)
04:50:37 hit:true  resident_bytes:2188 (refresh-Put — bytes CHANGED)
04:50:41 hit:true  resident_bytes:2188 (stable thereafter)
```

The byte size change at 04:50:37 (35s post-CRD-ADD) proves the refresher
ran `resolveWidgetForRefresh` and re-Put. Yet the served output remained
zero-state.

### 2.6 Code citations — discovery hop is exclusively walker-driven

The composition-GVR discovery path is `lazyRegisterInnerCallPaths`
(`internal/resolvers/restactions/api/resolve.go:939`). It runs only at the
`api.Resolve` per-stage iterator-expansion site
(`resolve.go:444`). The discovery hop body
(`resolve.go:979-1000`):

```go
if cache.PrewarmEnabled() {
    if grp, grpOK := cache.ExtractAPIServerGroupFromTemplatedPath(path); grpOK {
        cache.AddNavigationDiscoveredGroup(grp)
        if rcAny, ok := cache.InternalRESTConfigFromContext(ctx); ok {
            if cfg, ok := rcAny.(*rest.Config); ok && cfg != nil {
                if _, err := cache.DiscoverGroupResources(ctx, cfg, grp); err != nil {
                    ...
                }
            }
        }
    }
}
```

This fires ONLY when the `tmp` slice (per-stage RequestOptions) is non-empty
for a path that yields a templated apiserver group. The pre-v6 CRD-informer
backplane that called `DiscoverGroupResources` independently of the walker
was DELETED at Ship 0.5 / 0.30.223 (see `discovery_lookup.go:1-30` v6 header).

### 2.7 Code citations — CRD informer DOES fire ADD events, but they don't drive discovery

`internal/cache/deps_watch.go:191-219` (Ship A / 0.30.110):

```go
func (rw *ResourceWatcher) depEventHandlers(gvr schema.GroupVersionResource) clientcache.ResourceEventHandlerFuncs {
    return clientcache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) {
            if !rw.addEventPostSync(gvr, w) { return }
            ns, name := metaNSName(obj)
            Deps().OnAdd(gvr, ns, name)
        },
        UpdateFunc: ...
        DeleteFunc: ...
    }
}
```

This is the SOLE event handler wired to the `customresourcedefinitions`
informer. It dirty-marks dependent L1 entries. It does **NOT** trigger
`DiscoverGroupResources` for the new CRD's group.

### 2.8 Code citation — `OnResourceTypeAvailable` is no longer wired from a CRD-ADD path

`internal/cache/deps.go:692` (`OnResourceTypeAvailable`) is now called ONLY
from `discovery_lookup.go:305` inside `DiscoverGroupResources` itself. The
pre-v6 wire from CRD-informer AddFunc → `OnResourceTypeAvailable` was
deleted with the CRD informer (Ship 0.5 / 0.30.223). So a CRD ADD no longer
fires the FD1 dirty-mark for stale-negative LIST entries either.

---

## 3. Hypothesis rank-order + falsifications

### H1 (TRACED): Composition CR GVR informer never started — primary

**State**: composition CRD did not exist at S1 (boot) → stage 2 iterator
of `compositions-list` RA was empty at first resolve → `lazyRegisterInnerCallPaths`
never visited the composition path → group never added to `navDiscoveredGroups`,
GVR never registered, informer never started.

**Verified**: §2.1, §2.2, §2.6.

### H2 (FALSIFIED): Informer started but event hook missing

**Falsifier**: §2.1 — the informer is NOT registered (count=30 list excludes
`composition.krateo.io`). So no event hook to wire; H2 does not apply.

### H3 (PARTIAL): F-4 regression (apistage dep-record hook)

**Verified**: `apistage.go:495-504` still records the dep edge on HIT (Ship
0.30.212 F-4 fix preserved). The 60 dirty-marks at 04:50:02 confirm the
apistage `crds` content entry's dep IS firing. Refresher DOES re-resolve.

**Falsified as primary**: F-4 is functioning. The defect is upstream.

### H4 (PARTIAL): Piechart's dep chain doesn't include composition CR GVR

**Verified**: at S1's cold resolve, stage 2's iterator emitted ZERO request
options (CRD didn't exist) → no `RecordList(piechartKey, compositionGVR, "")`
ever fired → piechart's L1 entry has NO dep on composition CR GVR.

**Status**: this is a CONSEQUENCE of H1, not an independent root cause.
Even if a dep existed, no informer would fire OnAdd events for the
composition CR GVR (because no informer for it exists).

### H5 (FALSIFIED): Dirty-mark fires but cache HIT bypasses staleness

**Falsifier**: §2.5 — the piechart key resident_bytes DID change (2211→2188)
during S4, confirming the cache served fresh-but-still-zero bytes from a
re-resolve. Refresher is correctly invalidating + re-resolving. The defect
is that the RE-RESOLVE itself does not yield non-zero output.

### H6 (NEW, INFERRED): Refresher's widget re-resolve has stale stage-1 cache hit

**Mechanism**: when refresher re-resolves the piechart widget L1 entry, stage 1
(`crds` LIST) consults the apistage content cache. If the apistage
`crds` cell is hit-stale (refresher hasn't yet processed THAT key), stage 1
returns 0 CRDs → stage 2 iterator empty → no discovery hop fired.

**Status**: PLAUSIBLE but inferred. Direct verification would require log lines
the current code does not emit. The race between "apistage cell refresh"
and "widget cell refresh" is intrinsically present. Even if H6 fires only
intermittently, the resulting state is permanent: once both cells are
refreshed, the apistage cell IS fresh, but the widget cell has been
re-Put with the iterator-empty result, and no further event drives another
widget refresh.

### Conclusion — load-bearing root cause

**H1 + H6 compound**: the architectural premise of v6 (walker-driven
discovery, no CRD informer) assumes the walker chain re-enters cleanly
when a new CRD appears. In practice, the walker chain's first-stage cache
hit can short-circuit the second-stage iterator expansion ON THE REFRESH
PATH, leaving the discovery hop never invoked for the new group. The
fundamental gap: **a CRD ADD event does not directly drive discovery
for the CRD's group** — it relies on an indirect cache-invalidation chain
that can race itself into a stuck zero-state.

---

## 4. Fix design (Ship 0.30.233)

### 4.1 Design choice

**Add a discovery side-effect to the customresourcedefinitions informer's
AddFunc.** When a CRD ADD event fires:

1. Extract the new CRD's `spec.group` from the typed object.
2. If `group` is non-empty, invoke `DiscoverGroupResources(ctx, saRC, group)`.

This is mechanism-uniform per `feedback_no_special_cases`:

- Keyed on the CRD-ADD-EVENT shape, NOT on hardcoded GVR/group literals.
- Applies to ANY group (composition.krateo.io, core.krateo.io, future
  blueprint kinds).
- Uses the EXISTING `DiscoverGroupResources` mechanism — no new code path.

This RESTORES the pre-v6 backplane (CRD informer → composition GVR discovery)
in a SIMPLER form: one side-effect hook on the existing
customresourcedefinitions informer's AddFunc, NOT a separate informer for
CRDs (the customresourcedefinitions informer is ALREADY registered for
other reasons — see `phase1.go:218-222` and the `cache.lazy_register` line
for it at 04:50:02 in the pod logs).

### 4.2 Code changes (file:line)

#### Change 1 — `internal/cache/deps_watch.go:191-219`

Refactor `depEventHandlers` to accept an optional `addSideEffect` hook
for the CRD GVR. Or — preferred — keep `depEventHandlers` minimal and add
a SEPARATE handler attached at the CRD GVR's registration site.

**Preferred approach**: Wire the CRD-ADD side-effect at the CRD-GVR-specific
attach site, NOT in the generic `depEventHandlers`. This keeps `depEventHandlers`
content-uniform (matches its current "metaNSName only, no content reads"
contract — see deps_watch.go:165-178).

#### Change 2 (PRIMARY) — `internal/cache/watcher.go`

When `EnsureResourceType` registers the `customresourcedefinitions` GVR
(it's a meta-query seed registered by `RegisterMetaQuerySeeds`), ALSO
attach an additional handler that:

- Reads the CRD object (it's an `*unstructured.Unstructured`, has full spec).
- Extracts `spec.group`.
- Calls `cache.DiscoverGroupResources(ctx, saRC, group)`.

The exact site: at `EnsureResourceType` for the CRD GVR (after the
existing `AddEventHandler(rw.depEventHandlers(gvr))` at `watcher.go:804`
or `:1148` depending on the registration path), add a SECOND
`AddEventHandler` call with a CRD-specific handler.

OR — cleaner — predicate-route inside `depEventHandlers`:

```go
func (rw *ResourceWatcher) depEventHandlers(gvr schema.GroupVersionResource) clientcache.ResourceEventHandlerFuncs {
    w := depWatchSingleton()
    crdSideEffect := isCRDGVR(gvr)  // predicate, not literal — see §4.3
    return clientcache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) {
            if !rw.addEventPostSync(gvr, w) {
                w.counters.addDroppedPreSync.Add(1)
                return
            }
            ns, name := metaNSName(obj)
            w.counters.addPropagated.Add(1)
            Deps().OnAdd(gvr, ns, name)
            // Ship 0.30.233 — CRD-ADD discovery side-effect.
            if crdSideEffect {
                triggerCRDDiscovery(obj)
            }
        },
        // UpdateFunc / DeleteFunc unchanged
    }
}
```

`triggerCRDDiscovery(obj)`:

```go
func triggerCRDDiscovery(obj interface{}) {
    u, ok := obj.(*unstructured.Unstructured)
    if !ok {
        // Bytes-object or PartialObjectMetadata — won't have spec.group.
        // The CRD informer is full-Unstructured (not metadata-only) by
        // construction, so this branch should never hit. Log + skip.
        return
    }
    group, found, err := unstructured.NestedString(u.Object, "spec", "group")
    if err != nil || !found || group == "" {
        return
    }
    saRC := getProcessSARestConfig()  // see §4.4
    if saRC == nil {
        return
    }
    // Fire-and-forget: DiscoverGroupResources is singleflighted per group
    // and idempotent; this is safe from the informer processor goroutine
    // because it dispatches a single bounded discovery hop, not arbitrary
    // work. The actual K8s discovery LIST happens on the calling goroutine
    // (NOT a goroutine spawn here) — bounded by the singleflight lock
    // duration (~tens of ms).
    ctx := context.Background()
    cache.AddNavigationDiscoveredGroup(group)
    if _, err := cache.DiscoverGroupResources(ctx, saRC, group); err != nil {
        slog.Warn("cache.crd_add.discovery_failed",
            slog.String("subsystem", "cache"),
            slog.String("group", group),
            slog.Any("err", err))
    }
}
```

### 4.3 `isCRDGVR` predicate (no GVR literal in `deps_watch.go`)

Per `feedback_no_special_cases` — the predicate should be expressed via
a shared constant in `cache/keys.go` or similar, NOT inline literals. The
existing codebase already has:

- `phase1.go:218-222` documenting `customresourcedefinitions` as a meta-query
  seed.
- `discovery_lookup.go:396-413` defining `isBuiltInKind` against
  apiextensionsv1.

Define one canonical exported predicate:

```go
// in cache/keys.go (or a new cache/crd_gvr.go)
var crdGVR = schema.GroupVersionResource{
    Group:    "apiextensions.k8s.io",
    Version:  "v1",
    Resource: "customresourcedefinitions",
}

// IsCRDGVR reports whether gvr is the CRD-meta GVR. Single predicate the
// CRD-ADD side-effect consults — keeps the literal in one place (the type
// system requires SOME literal to identify the CRD GVR; centralising it
// here keeps any future audit single-point).
func IsCRDGVR(gvr schema.GroupVersionResource) bool {
    return gvr == crdGVR
}
```

This is borderline-acceptable under `feedback_no_special_cases`: the rule
forbids hardcoded path/resource/user CASES in resolver Go, but recognising
the CRD-meta GVR (which has a UNIQUE structural role — it's the only GVR
whose Add event needs to drive group-discovery) is necessary to bridge the
informer-event API and the discovery API. The literal is centralised into
ONE constant in the cache package, matching how `apiextensionsv1` is
already referenced at `discovery_lookup.go:43`, `phase1.go:218`, etc.

### 4.4 SA *rest.Config access from informer processor goroutine

`DiscoverGroupResources` needs `*rest.Config`. The current call sites
(`resolve.go:990`) get it from `cache.InternalRESTConfigFromContext(ctx)`.
The informer processor goroutine has no per-call ctx with this attached.

**Solution**: store the SA *rest.Config as a process singleton at startup.
Mirror the existing `Global()` pattern:

```go
// in cache package
var (
    saRCMu sync.RWMutex
    saRC   *rest.Config
)

func SetProcessSARestConfig(rc *rest.Config) {
    saRCMu.Lock()
    defer saRCMu.Unlock()
    saRC = rc
}

func ProcessSARestConfig() *rest.Config {
    saRCMu.RLock()
    defer saRCMu.RUnlock()
    return saRC
}
```

Wire `SetProcessSARestConfig(saRC)` at `main.go` startup, immediately after
`saRC` is built (same site that already passes it to
`RegisterRefreshHandlers(saRC)`). No new wiring in dispatchers — the
informer handlers in `cache/` consume the singleton.

### 4.5 What this change is NOT

- NOT a re-introduction of the pre-v6 CRD informer as a separate informer.
  The customresourcedefinitions GVR's informer is ALREADY registered
  (see §2.4 — the `cache_event.consumed type=ADD gvr=customresourcedefinitions`
  log line at 04:50:02 PROVES it's running). We are adding ONE side-effect
  to its EXISTING handler chain.

- NOT a periodic sweep. The fix is event-driven via the EXISTING CRD ADD
  event delivery.

- NOT a flag-gated feature. The fix is a strict superset of correct
  behavior; no opt-in. Per `feedback_no_park_broken_behind_flag`.

- NOT special-casing `composition.krateo.io`. Any CRD whose spec.group
  is non-empty triggers discovery for that group.

---

## 5. Risk register

| Risk | Severity | Mitigation |
|---|---|---|
| Calling `DiscoverGroupResources` from the informer processor goroutine could block event delivery | HIGH | `DiscoverGroupResources` is per-group singleflighted; the actual discovery hop is bounded (one apiserver `ServerGroups()` + one `ServerResourcesForGroupVersion()` per version, typically <50ms total). Acceptable for an informer event handler. ALTERNATIVE if profile shows blocking: dispatch to a bounded worker channel (mirror the `deleteEvictCh` pattern at deps_watch.go:130-146). |
| Discovery hop fails (apiserver flake) | LOW | `DiscoverGroupResources` soft-fails (`discovery_lookup.go:255-258`). Subsequent CRD events retry; the existing walker-on-/call hop is the fallback. |
| `triggerCRDDiscovery` panics on a malformed CRD object | MEDIUM | Wrap in `defer recover` matching the existing `deleteWorker` panic recovery at deps_watch.go:101-107. |
| The CRD GVR is registered as metadata-only (no spec available) | LOW | Verified at `watcher.go`: the CRD GVR is registered via the standard dynamic informer path (not metadata-only). Defensive: `triggerCRDDiscovery` does a type-assertion check and skips on bytes-object / PartialObjectMetadata. |
| Race between informer initial-replay ADD and the post-sync side-effect | LOW | The existing `addEventPostSync` gate at deps_watch.go:194 already gates the AddFunc; the side-effect inherits the same gate. Initial-replay ADDs are dropped (don't fire discovery); this is correct — Phase 1 walker already discovers all groups that existed at boot. |
| Duplicate discovery hops on rapid CRD churn | LOW | `DiscoverGroupResources` is per-group singleflighted (`discovery_lookup.go:228-232`); duplicate calls serialize, the second observes the fresh state (idempotent EnsureResourceType returns added=false). |
| RBAC: the SA has no `get` on `customresourcedefinitions` | NONE | The CRD informer is ALREADY running with the SA's bindings (post-Ship-0.5). The same SA token is used for discovery. If discovery 403s, soft-fail. |

---

## 6. Pre-commit falsifier

**Reproduction**: Without the fix, the test asserts: a CRD ADD event fed
to the depEventHandlers AddFunc does NOT result in a `DiscoverGroupResources`
invocation for the CRD's spec.group.

**Test file**: `internal/cache/crd_add_discovery_test.go` (NEW).

```go
//go:build !integration

package cache

import (
    "testing"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime/schema"
)

// TestCRDAdd_TriggersGroupDiscovery — Ship 0.30.233 falsifier.
//
// FAILS pre-fix: the customresourcedefinitions AddFunc only dirty-marks
// dependent L1 keys; it does NOT call DiscoverGroupResources for the new
// CRD's spec.group. So a fresh CRD never causes its group to be added
// to navDiscoveredGroups, and the composition CR informer for that group
// is never spawned.
//
// PASSES post-fix: AddFunc invokes triggerCRDDiscovery, which extracts
// spec.group and calls AddNavigationDiscoveredGroup + DiscoverGroupResources.
func TestCRDAdd_TriggersGroupDiscovery(t *testing.T) {
    ResetNavigationDiscoveredGroupsForTest()
    t.Cleanup(ResetNavigationDiscoveredGroupsForTest)

    // Synthesise a CRD ADD with spec.group="composition.krateo.io".
    crd := &unstructured.Unstructured{Object: map[string]any{
        "apiVersion": "apiextensions.k8s.io/v1",
        "kind":       "CustomResourceDefinition",
        "metadata":   map[string]any{"name": "synth.composition.krateo.io"},
        "spec":       map[string]any{"group": "composition.krateo.io"},
    }}
    crdGVR := schema.GroupVersionResource{
        Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
    }

    // Build a minimal post-sync watcher state so addEventPostSync returns true.
    rw := newTestWatcherPostSync(t, crdGVR)

    // Invoke the AddFunc directly with the synthetic CRD object.
    handlers := rw.depEventHandlers(crdGVR)
    handlers.AddFunc(crd)

    // ASSERT: composition.krateo.io is now in navDiscoveredGroups.
    if !IsNavigationDiscoveredGroup("composition.krateo.io") {
        t.Fatal("Ship 0.30.233 FAIL: CRD ADD did NOT add composition.krateo.io " +
            "to navDiscoveredGroups. The CRD informer AddFunc is " +
            "missing the discovery side-effect.")
    }
}
```

Test helper `newTestWatcherPostSync` and the SA-rc stub belong in
`export_test_hooks.go` (an existing file).

### Verification before commit:

1. Run `go test ./internal/cache/ -run TestCRDAdd_TriggersGroupDiscovery -count=1`.
   Pre-fix: FAILS with the assertion message.
   Post-fix: PASSES.

2. Run `go test ./internal/cache/ -race -count=1` to verify no concurrency
   regression.

---

## 7. Post-deploy gate (HARD, per `feedback_gke_pod_boot_readiness_pre_ship_gate`)

After helm upgrade to 0.30.233 lockstep chart tag:

```bash
# Gate 1 — pod boot
POD=$(kubectl --context gke_neon-481711_us-central1-a_cluster-1 -n krateo-system get pod -l app=snowplow -o jsonpath='{.items[0].metadata.name}')
kubectl --context gke_neon-481711_us-central1-a_cluster-1 -n krateo-system wait --for=condition=Ready pod/"$POD" --timeout=300s
# restartCount must be 0
kubectl --context gke_neon-481711_us-central1-a_cluster-1 -n krateo-system get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}'
```

```bash
# Gate 2 — composition GVR registered post-S2 (apply a CRD; check /debug/vars)
# State the bench's S4 already provides: 20 composition CRs Ready=True, CRD present.
# Trigger one more CRD ADD by deploying a stunt CompositionDefinition (or restart-soak).
# Verify within 60s:
kubectl --context gke_neon-481711_us-central1-a_cluster-1 -n krateo-system port-forward "$POD" 18083:8081 &
curl -s http://localhost:18083/debug/vars | jq '.snowplow_plurals_registered_gvrs.gvrs[] | select(contains("composition.krateo.io"))'
# Expected: at least one composition.krateo.io/.../githubscaffoldingwithcompositionpages entry.
```

```bash
# Gate 3 — bench S4 resumption passes
bench phase6 --from-stage S4 --run-dir /tmp/snowplow-runs/0.30.232/run-20260602-064642/
# Expected: S4 PASS within 300s convergence (api>=20, ui>=20 for admin).
```

If any gate fails → `helm rollback snowplow <prev-rev> -n krateo-system` immediately (HARD REVERT).

---

## 8. LOC + wall-clock estimate

- `internal/cache/crd_gvr.go` (new): ~20 LOC — `crdGVR` const + `IsCRDGVR` predicate.
- `internal/cache/sa_rc_singleton.go` (new): ~30 LOC — process-global SA rc setter/getter.
- `internal/cache/deps_watch.go` (edit): ~15 LOC — `triggerCRDDiscovery` call within AddFunc gated by `IsCRDGVR(gvr)`.
- `internal/cache/crd_discovery_side_effect.go` (new): ~40 LOC — `triggerCRDDiscovery` helper.
- `main.go` (edit): ~3 LOC — `cache.SetProcessSARestConfig(saRC)` call.
- `internal/cache/crd_add_discovery_test.go` (new): ~80 LOC — falsifier.

**Total**: ~190 LOC.
**Wall-clock**: 0.5d architect (this doc) + 0.5d dev (code + tests) + 0.5d
PM/Diego gates + 0.5d post-deploy validation = ~2 working days.

---

## 9. Constraint compliance

- [x] No park-behind-flag (`feedback_no_park_broken_behind_flag`): the fix
      is a strict superset; always on.
- [x] No GVR/resource/user special-cases (`feedback_no_special_cases`):
      `IsCRDGVR` is a centralised predicate; the side-effect applies to ALL
      CRD ADDs regardless of group; no per-customer literals.
- [x] No upstream patch (`project_no_upstream_authority`): all changes are
      in snowplow internal packages.
- [x] Empirical root-cause trace (`feedback_empirical_root_cause_trace_before_fix`):
      §2 cites file:line + live /debug/vars + pod logs.
- [x] Falsifier-first (`feedback_falsifier_first_before_ship`): §6 provides
      a pre-commit unit test that FAILS on broken binary, PASSES on fix.
- [x] Audit enumeration (`feedback_audit_enumerate_mechanisms`): §2.6/2.7
      enumerate ALL three discovery mechanisms (walker hook, CRD informer
      events, periodic sweep) and confirm only the walker hook exists.
- [x] Pod-boot-readiness gate (`feedback_gke_pod_boot_readiness_pre_ship_gate`):
      §7 codifies the mandatory post-deploy gate.

---

## 10. Open items for PM review

1. **Should we use the in-line predicate (`§4.2 alternative`) or the
   separate AddEventHandler call?** Architect prefers in-line predicate
   (smaller surface, single registration point). PM decision.

2. **Is the SA *rest.Config singleton acceptable?** It's analogous to
   `cache.Global()` (the ResourceWatcher singleton) and consistent with
   how `phase1_walk.go:820` already manages the SA rc. PM verification.

3. **Race with `addEventPostSync` initial-replay drop**: pre-sync ADDs
   are correctly dropped (the Phase 1 walker discovers all groups that
   existed at boot — this is correct). Verify this matches Diego's intent
   for the fix scope.

4. **Refresher path is NOT touched**: the fix targets the informer-ADD
   path which is the architecturally correct trigger. The refresher's
   widget re-resolve will then succeed on its NEXT cycle once the
   composition informer is registered + synced.

---

ACK — handing off to team-lead for PM gate routing.
