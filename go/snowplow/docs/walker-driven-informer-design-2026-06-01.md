# Walker-driven informer / plurals-based design (v6) — 2026-06-01

Status: Design captured through v6. Three ships: Ship 0 deployed 0.30.222, Ship 0.5 deletes crdwatch.go (v6), Ship 1 plurals additive, Ship 2 plurals replacement.

v6 supersedes v5 on the CRD-informer wiring question only. v5 walker-spawned the CRD GVR via a `sync.Once` inside `AddAutoDiscoverGroup`. v6 goes further: the CRD informer is **not spawned at all**. Composition GVR discovery is done by one-shot apiserver discovery (`Discovery.ServerResourcesForGroupVersion`) per navigation-discovered group, invoked synchronously from the walker. The CRD-watch event backplane (`crdwatch.go`, 459 LOC) is **deleted**. The group-set is preserved under a renamed accessor (`IsNavigationDiscoveredGroup`) and moves to a new `discovery_lookup.go` because two call sites in `watcher.go` (`:749`, `:1064`) consume it as the removable-discriminator key. Plurals layer (v3) unchanged — orthogonal to v6.

Bottom line: snowplow already runs the walker-driven architecture this design names. Ship 0 (deployed 0.30.222) consolidated the handler-extension registry + removed `customResourceDefinitionGVR` from `MetaQuerySeeds`. Ship 0.5 (v6, this doc) deletes the CRD-watch event backplane entirely. Ships 1 + 2 remain the plurals layer (v3 design, unchanged).

---

## 1. Why (empirical motivation, unchanged through v6)

TRACED 2026-06-01 (`project_warm_root_cause_2026_06_01`):

- snowplow 0.30.221: cj /dashboard warm p50 = **1693ms** (Chrome MCP), vs **800ms north-star**.
- Direct snowplow-LB cj /call p50 = **339ms warm**; CPU per call = **~20ms**. Gap = **~319ms scheduling/GC delay**.
- Refresher CPU = **52.55% steady-state, load-INDEPENDENT** (58% no-load control). bgMarkWorker = **34.49%**.
- Refresher allocations = **~700 MB / 30s**. L1 hit rate = **100% (610/610)** on customer path — proactive refresh works architecturally; the cost is the defect.
- Projection cj /call → **≤50ms warm** post-Ship-2 → 16-call waterfall p50 ≤ **800ms** → north-star HIT.

Root cause: `internal/dynamic/dynamic.go:18-57` constructs a fresh `DeferredDiscoveryRESTMapper(NewMemCacheClient(...))` per call. The mem-cache wrapper is fresh per invocation → caches nothing across calls → every invocation does full apiserver discovery LIST + JSON parse + `AddSpecific × all GVRs`. The refresher hot loop drives this 186,632× per 30s = 6,221 items/sec.

Secondary motivation for v6 specifically: post-Ship-0 the CRD informer is walker-spawned, but it still exists as an in-process LIST/WATCH against `apiextensions.k8s.io/v1/customresourcedefinitions`. The only consumer of that informer's event stream is composition-GVR auto-discovery (CRD ADD → `registerCRDObject` → `EnsureResourceType(compositionGVR)`). On a 30K-CRD cluster the informer's initial LIST + steady-state WATCH costs are non-trivial and the marginal benefit over one-shot discovery is bounded by CRD CREATE/DELETE cadence (which is near-zero in production — see §5 lag tradeoff). v6 removes the informer, keeps the composition GVR spawn behavior via synchronous discovery.

---

## 2. TRACED current state (post-Ship-0, pre-Ship-0.5)

1. **Informer set is walker-driven.** Dynamic shared informer factory at `internal/cache/watcher.go:317-322`. Every informer registers lazily through `EnsureResourceType` (`watcher.go:577-680`). Walker-driven `EnsureResourceType` sites (TRACED):
   - `internal/handlers/dispatchers/deps_extract.go:96, :101, :117`
   - `internal/resolvers/restactions/api/resolve.go:974`
   - `internal/handlers/dispatchers/phase1_pip_seed.go:840`
   - `internal/handlers/dispatchers/restactions.go:250`

2. **Dirty-mark wiring uniform across all informers.** `watcher.go:981` (`addResourceTypeLocked`) + `watcher.go:730` (`addResourceTypeMetadataOnlyLocked`) install `rw.depEventHandlers(gvr)` (`deps_watch.go:191-205`) feeding `Deps().OnAdd/OnUpdate/OnDelete` (`deps_watch.go:200-205`). RBAC GVRs additionally attach `rbacSnapshotEventHandlers` via the handler-extension registry. DELETE evicts only self-key per `feedback_l1_invalidation_delete_only`.

3. **CRD informer status post-Ship-0 (0.30.222).** Walker-spawned via `AddAutoDiscoverGroup`'s `sync.Once` (`crdwatch.go:133-160`) on the first templated path. The CRD informer's store has TWO consumers outside the resolver path — both auto-discovery of composition GVRs (`crdwatch.go:249-271` register / `crdwatch.go:293-311` unregister / `crdwatch.go:339-357` reconcile). It is NEVER consulted for kind→plural resolution.

4. **Auto-discover group-set is load-bearing for `watcher.go`'s removable discriminator.** `matchesAutoDiscoverGroup(gvr.Group)` at `watcher.go:749` (metadata-only path) and `watcher.go:1064` (full-informer path) is the predicate that decides whether to use the **standalone** dynamic informer (informer per GVR — tearable down via `RemoveResourceType`) versus the **shared factory** path (informer permanently bound to factory lifetime). The comment block at `watcher.go:986-988, :1058-1060` documents this explicitly: *"the removable discriminator is matchesAutoDiscoverGroup"*. The group-set survives Ship 0.5 verbatim — only its file location and accessor name change.

5. **`crds.Get` returns `map[string]any` not `*CustomResourceDefinition`.** `internal/resolvers/crds/get.go:42-54` returns `got.UnstructuredContent()`. Downstream consumer at `internal/resolvers/crds/schema/extract.go:11` takes the map directly — no typed conversion required at the call site.

6. **`plurals` already a runtime-active snowplow dependency.** `internal/handlers/plurals.go:13` imports `github.com/krateoplatformops/plumbing/kubeutil/plurals`. Sole usage at `internal/handlers/plurals.go:54-58` (`plurals.Get` for `/api-info/names`). No new import.

7. **plumbing plurals shape (v0.9.3 confirmed against `go.mod`).**
   - `Info{Plural, Singular, Shorts}` — `Plural` is the plural resource name we need.
   - TTL hardcoded to **48h** inside `plurals.Get` (`plurals.go:57`).
   - **Critical constraint**: `plurals.ResolveAPINames` constructs its own `rest.InClusterConfig()` + `discovery.NewDiscoveryClientForConfig(rc)` per invocation (`plurals.go:84-90`). It does NOT accept the caller's `*rest.Config`. v3 stops using `plurals.Get` entirely on the resolver path — see §3.2.

---

## 2.1 Why a permanent store for plurals (v3 correction, unchanged through v6)

**The CRD `metadata.name` is K8s-immutable and required to equal `<plural>.<group>`.** Therefore the (group, version, kind) → plural mapping is **stable for the entire lifetime of the CRD object**. A CRD cannot be "renamed" — only deleted and recreated.

TRACED non-mutation check: snowplow contains **zero** `customresourcedefinitions.Update` / `Patch` / rename code (grep across `internal/` 2026-06-01). Built-in scheme entries (`scheme.Scheme.AllKnownTypes`) are compile-time constants — they cannot change at runtime. CRD DELETE: stale entry becomes unreachable (no cluster object has that kind). Worst case if consulted = one apiserver 404, identical to no-cache.

Consequence: the 48h TTL in `plurals.Get` is vestigial. v3 replaces TTL with a **never-expiring `sync.Map[schema.GroupVersionKind]plurals.Info`**, populated once per gvk per process. Memory footprint: ≪1000 entries; ~bytes per entry. Bounded by GVR set, not by traffic.

---

## 3. TRACED design — five layers (Layer 2 simplified in v6)

### Layer 1 — Walker (NO CHANGE)

What it does: navmenus / routes-loader / widgets / resourcesRefs / RESTActions are walked. For each dispatched apiserver path the resolver parses a GVR and calls `cache.Global().EnsureResourceType(gvr)`. Widget `resourceRef.apiVersion + kind` → walker resolves via Layer 5 → forms GVR → `EnsureResourceType(gvr)`.

Invariant: walker is the sole source of "which informers run." No static GVR catalog. No boot primordials beyond the 7 anchors in `MetaQuerySeeds` (routesloaders + navmenus + restactions + 4 RBAC).

### Layer 2 — Informers (v6 simplifies: CRD informer NEVER spawned)

`EnsureResourceType(gvr)` (`watcher.go:577`) is idempotent. Every walker-discovered GVR gets one informer through this entry point — **except the CRD GVR itself**.

**v6 change**: the walker no longer spawns the CRD informer at all. When the walker reaches a templated apiserver path:

1. It extracts the static group via `cache.ExtractAPIServerGroupFromTemplatedPath(path)` — pure-string parse, moves from `crdwatch.go:206` to `discovery_lookup.go:path_parse.go`. No code change inside this function.
2. It calls `cache.AddNavigationDiscoveredGroup(group)` — renamed from `cache.AddAutoDiscoverGroup`, moves to `discovery_lookup.go`. Pure set-add, no informer side-effect.
3. It calls `cache.DiscoverGroupResources(ctx, rc, group)` — NEW one-shot apiserver discovery call. Lists `ServerResourcesForGroupVersion` for every version of `group`. For each `APIResource` whose `Kind` looks like a CRD-backed kind (heuristic: not in `scheme.Scheme.AllKnownTypes()` for that GV — i.e., not a built-in), forms the composition GVR and calls `rw.EnsureResourceType(compositionGVR)`.
4. The dirty-mark contract for newly-spawned composition informers is satisfied **synchronously** via `cache.OnResourceTypeAvailable(gvr, namespace, name)` — same callback chain `crdwatch.go:registerCRDObject` invoked post-Ship-0. The callback set is preserved verbatim; only the invocation site moves from CRD-informer AddFunc to the walker's discovery completion.

**The CRD informer never exists.** No `EnsureResourceType(customResourceDefinitionGVR)` is invoked anywhere in the cache codebase post-Ship-0.5. The `customResourceDefinitionGVR` constant is **deleted** from `phase1.go:155`.

### Layer 3 — Dirty marks (NO CHANGE)

`deps_watch.go:191-205` wires `Deps().OnAdd/OnUpdate/OnDelete` for every informer. `deps.go:493-616` per-event fan-out keyed by `(gvr, namespace, name)` and `(gvr, namespace, "*")` LIST-scope. DELETE evicts only self-key; ADD/UPDATE dirty-marks dependents and enqueues to the refresher workqueue. Composition informers register exactly as today.

### Layer 4 — Refresher (NO CHANGE)

Event-driven workqueue (`internal/cache/refresher.go`). Customer-priority yield at `refresher.go:60-80` per `feedback_customer_priority_over_refresher`. The 0.30.221 700 MB/30s defect is upstream of the refresher — caused by the two `dynamic.{KindFor,ResourceFor}` per-call sites the refresher hot-loops through. Removing those sites is what closes the defect.

### Layer 5 (orthogonal) — Plurals via a permanent process-wide store (v3, unchanged)

Kind → plural (and the reverse, plural → kind) resolution is a separate concern from L1 / informers. Done entirely in-process through a never-expiring store. **No `plurals.Get` calls on the resolver path.** Spec preserved verbatim from v3 (§3.2 below).

---

## 3.1 DELETE plan — v6 expands the deletion set

### v6 (Ship 0.5) deletes the CRD-watch event backplane entirely

`internal/cache/crdwatch.go` — **deleted** (459 LOC).
- `RegisterHandlerExtension` for the CRD predicate (lines 60-67 of current file) → deleted; no more CRD handler-extension entry.
- `AddAutoDiscoverGroup` / `MatchesAutoDiscoverGroup` / `AutoDiscoverGroupsSnapshot` / `ExtractAPIServerGroupFromTemplatedPath` → migrate to `discovery_lookup.go` + `path_parse.go` with renamed accessors. NOT deleted; relocated.
- `crdInformerSpawned sync.Once` (`crdwatch.go:122`) → deleted. No replacement.
- `registerCRDObject` / `unregisterCRDObject` / `compositionGVRFromCRDObject` → deleted at the CRD-informer event site; the composition GVR derivation logic migrates into `DiscoverGroupResources` body in `discovery_lookup.go`.
- `ReconcileAutoDiscoverCRDs` (`crdwatch.go:339`) → deleted; its purpose (close the replay-vs-discover race after the post-walk pass) is moot because there is no CRD informer to replay.

`internal/cache/crdwatch_internal_test.go` — **deleted** (277 LOC). All assertions reference the CRD-informer event flow that no longer exists.

`internal/cache/crdwatch_lifecycle_falsifier_test.go` — **deleted** (373 LOC). Same.

`internal/cache/crdwatch_walker_spawn_test.go` — **deleted** (291 LOC). Test corpus migrates to `discovery_lookup_test.go` re-targeting the synchronous discovery path.

`internal/cache/phase1.go:155` — `customResourceDefinitionGVR` var declaration **deleted**. `CustomResourceDefinitionGVR()` accessor at `:216` **deleted**. (Already removed from `MetaQuerySeeds` in Ship 0; v6 removes the constant itself since nothing references it post-deletion.)

`internal/cache/handler_registry.go` — the CRD-predicate registration (was added in Ship 0) **deleted**. The registry stays for RBAC.

`internal/handlers/dispatchers/phase1_walk.go` — any remaining `ReconcileAutoDiscoverCRDs()` callsite **deleted**.

`internal/cache/watcher.go:749, :1064` — call sites for `matchesAutoDiscoverGroup` → rename target `IsNavigationDiscoveredGroup` (literal find/replace of the function name; semantics identical). Comment block `:986-988, :1058-1060` updated to reference the new name + new file location.

### v6 adds (Ship 0.5)

`internal/cache/discovery_lookup.go` (~180 LOC, new) — owns:
- `var navDiscoveredGroups map[string]struct{}` + `var navDiscoveredGroupsMu sync.RWMutex` (relocated from `crdwatch.go:97-110`).
- `AddNavigationDiscoveredGroup(group string)` (renamed from `AddAutoDiscoverGroup`; spawn side-effect removed — pure set-add).
- `IsNavigationDiscoveredGroup(group string) bool` (renamed from `matchesAutoDiscoverGroup`).
- `NavigationDiscoveredGroupsSnapshot() []string` (renamed from `AutoDiscoverGroupsSnapshot`).
- `ResetNavigationDiscoveredGroupsForTest()` (renamed from `ResetAutoDiscoverGroupsForTest`).
- `DiscoverGroupResources(ctx context.Context, rc *rest.Config, group string) (int, error)` — NEW. Lists `ServerResourcesForGroupVersion` for every version of `group`; for each non-built-in `APIResource.Kind`, calls `rw.EnsureResourceType(compositionGVR)` + `cache.OnResourceTypeAvailable(...)` synchronously. Returns count of newly-registered composition GVRs.
- Emits new counter `cache.discovery.group_resources_fetched{group=X}` per call, and `cache.discovery.group_resources_registered{group=X}` per registered GVR.

`internal/cache/path_parse.go` (~35 LOC, new) — owns:
- `ExtractAPIServerGroupFromTemplatedPath(path string) (string, bool)` (relocated verbatim from `crdwatch.go:206-235`).

`internal/cache/discovery_lookup_test.go` (~150 LOC, new) — owns:
- `TestAddNavigationDiscoveredGroup_PureSetAdd` (asserts NO informer side-effect; counter-zero on group add).
- `TestDiscoverGroupResources_RegistersCompositionGVRs` (against `discoveryfake.FakeDiscovery`).
- `TestDiscoverGroupResources_SkipsBuiltins` (asserts `scheme.Scheme.AllKnownTypes()` GVRs are filtered out).
- `TestIsNavigationDiscoveredGroup_RemovableDiscriminator` (regression: `watcher.go:749, :1064` predicate semantics preserved).
- `TestRaceSafe_ConcurrentDiscoverGroupResources` (`-race`).

**Net Ship 0.5 LOC**: −459 (crdwatch.go) −277 (internal_test) −373 (lifecycle_falsifier_test) −291 (walker_spawn_test) +180 (discovery_lookup.go) +35 (path_parse.go) +150 (discovery_lookup_test.go) − const decls in phase1.go = **net −1226 LOC**.

### v3 deletion plan (Ship 2, unchanged)

`internal/dynamic/dynamic.go`:
- Lines **18-57 DELETED** — `ResourceFor` + `KindFor` go away. No fallback path. No cache-off retention.
- Lines **1-16** lose unused imports (`xenv`, `discovery`, `memory`, `cacheddiscovery`, `rest`, `restmapper`).
- Lines **59-103 retained** — pure shape accessors. Zero apiserver round-trips.
- File becomes ~50 lines.

`internal/cache/fallthrough_meter.go`:
- Lines **85-86 DELETED** — `ReasonRestmapperKindFor`, `ReasonRestmapperResourceFor`.
- Line **84** (`ReasonCRDGet`) DELETED.
- Closed-enum count adjusted.

`internal/resolvers/crds/get.go`: **deleted** in Ship 2. `internal/resolvers/crds/` directory removed.

---

## 3.2 REPLACE plan — plurals layer (Ship 1 + Ship 2, unchanged from v3)

### New file — `internal/cache/plurals_resolver.go` (~110 LOC)

Public surface (4 functions):
```go
// PluralFor resolves a GVK to its plural resource name. Built-in scheme
// first, permanent store next, snowplow-side discovery on miss.
func PluralFor(gvk schema.GroupVersionKind, rc *rest.Config) (plurals.Info, error)

// KindForGVR resolves the reverse direction (plural → Kind) using the
// same permanent store.
func KindForGVR(gvr schema.GroupVersionResource, rc *rest.Config) (string, error)

// GVRFor is convenience: PluralFor + form GVR.
func GVRFor(gvk schema.GroupVersionKind, rc *rest.Config) (schema.GroupVersionResource, error)

// PluralsStore exposes store metadata for tests / observability only.
func PluralsStore() *sync.Map
```

Internal:
```go
var pluralsStore sync.Map // schema.GroupVersionKind → plurals.Info, permanent.

var (
    builtinGVKToInfo map[schema.GroupVersionKind]plurals.Info
    builtinGVRToKind map[schema.GroupVersionResource]string
)

func init() {
    builtinGVKToInfo = make(map[schema.GroupVersionKind]plurals.Info)
    builtinGVRToKind = make(map[schema.GroupVersionResource]string)
    for gvk := range scheme.Scheme.AllKnownTypes() {
        gvr, _ := meta.UnsafeGuessKindToResource(gvk)
        info := plurals.Info{Plural: gvr.Resource}
        builtinGVKToInfo[gvk] = info
        builtinGVRToKind[gvr] = gvk.Kind
    }
}

func PluralFor(gvk schema.GroupVersionKind, rc *rest.Config) (plurals.Info, error) {
    if info, ok := builtinGVKToInfo[gvk]; ok {
        return info, nil
    }
    if v, ok := pluralsStore.Load(gvk); ok {
        return v.(plurals.Info), nil
    }
    info, err := discoverPlural(gvk, rc)
    if err != nil {
        return plurals.Info{}, err
    }
    actual, _ := pluralsStore.LoadOrStore(gvk, info)
    return actual.(plurals.Info), nil
}

func discoverPlural(gvk schema.GroupVersionKind, rc *rest.Config) (plurals.Info, error) {
    cache.RecordApiserverFallthrough(ctx, cache.ReasonPluralsDiscoveryHop, gvk.String())
    dc, err := discovery.NewDiscoveryClientForConfig(rc)
    if err != nil { return plurals.Info{}, err }
    list, err := dc.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
    if err != nil { return plurals.Info{}, err }
    for _, el := range list.APIResources {
        if el.Kind == gvk.Kind {
            return plurals.Info{Plural: el.Name, Singular: el.SingularName, Shorts: el.ShortNames}, nil
        }
    }
    return plurals.Info{}, fmt.Errorf("kind %q not found in %s", gvk.Kind, gvk.GroupVersion())
}
```

`KindForGVR` mirrors `PluralFor` against `builtinGVRToKind` + a reverse-index `sync.Map`. Discovery arm populates both directions from one `ServerResourcesForGroupVersion` response.

### Site A — `internal/resolvers/widgets/resourcesrefs/resolve.go:64-72`

```go
kind, err := cache.KindForGVR(gvr, rc)
if err != nil { return all, err }
gvk := gvr.GroupVersion().WithKind(kind)
```

### Site B — `internal/resolvers/crds/schema/schema.go:21-67`

```go
gv := dynamic.GroupVersion(obj)
gvk := gv.WithKind(dynamic.GetKind(obj))

gvr, err := cache.GVRFor(gvk, rc)
if err != nil { return err }

cli, err := dynamic.NewClient(rc)
if err != nil { return err }
got, err := cli.Get(ctx, fmt.Sprintf("%s.%s", gvr.Resource, gvr.Group), dynamic.Options{
    GVR: schema.GroupVersionResource{
        Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
    },
})
if err != nil { return err }
crd := got.UnstructuredContent()
crv, err := extractOpenAPISchemaFromCRD(crd, gvr.Version)
```

### `internal/handlers/plurals.go` — Option A (RECOMMENDED, ratified v3)

- `pluralsHandler` carries no store field.
- `ServeHTTP` calls `cache.PluralFor(gvk, rc)` directly. No TTL.
- LOC: −8 / +5.

---

## 3.3 What disappears cumulatively (v6)

- `internal/cache/crdwatch.go` — **deleted** (Ship 0.5, v6).
- `internal/cache/crdwatch_*_test.go` — three files **deleted** (Ship 0.5, v6).
- `customResourceDefinitionGVR` constant + `CustomResourceDefinitionGVR()` accessor — **deleted** (Ship 0.5, v6).
- `ReconcileAutoDiscoverCRDs` — **deleted** (Ship 0.5, v6).
- CRD-predicate `RegisterHandlerExtension` entry — **deleted** (Ship 0.5, v6).
- `internal/dynamic/dynamic.go:18-57` — `KindFor`, `ResourceFor` **deleted** (Ship 2, v3).
- `internal/resolvers/crds/get.go` — **deleted** (Ship 2, v3).
- `internal/resolvers/crds/` directory — **removed** (Ship 2, v3).
- `internal/cache/fallthrough_meter.go` — three constants deleted: `ReasonCRDGet`, `ReasonRestmapperKindFor`, `ReasonRestmapperResourceFor` (Ship 2, v3).
- All "RESTMapper" / "restmapper" terminology — **expunged**.
- Process-wide RESTMapper / `DeferredDiscoveryRESTMapper` / `NewMemCacheClient` — **never constructed anywhere**.
- `plurals.Get` calls — **zero on resolver path**.
- `cache.TTLCache[string, plurals.Info]` instances — **zero**.

---

## 4. Architectural deliverables — v6 rewrites §4.1, preserves §4.2/§4.3/§4.4

### 4.1 — CRD-informer wiring under v6 (FULLY REWRITTEN)

**v6 invariant: no CRD informer, ever.** Composition GVR discovery is one-shot apiserver discovery, invoked synchronously from the walker.

**Walker side-effect chain (v6)**:

```go
// internal/resolvers/restactions/api/resolve.go:958-961 (logical site)
if grp, ok := cache.ExtractAPIServerGroupFromTemplatedPath(path); ok {
    cache.AddNavigationDiscoveredGroup(grp)
    // v6: one-shot synchronous discovery. NO informer for the CRD GVR.
    // Spawns composition informers via EnsureResourceType + fires
    // OnResourceTypeAvailable callback per registered GVR.
    if _, err := cache.DiscoverGroupResources(ctx, rc, grp); err != nil {
        slog.Warn("cache.discovery.group_resources_fetch_failed", "group", grp, "err", err)
        // Soft-fail: missing composition GVRs surface as 404 on first
        // dispatch hit; the walker continues. Subsequent walks retry.
    }
}
```

**`DiscoverGroupResources` body** (`internal/cache/discovery_lookup.go`, ~70 LOC of the file's 180):

1. Build `discovery.NewDiscoveryClientForConfig(rc)` (one client per call; allocation profile acceptable per `feedback_capacity_caps_empirical_per_entry_cost` — bounded by group set, not by traffic).
2. List `ServerGroups()` to enumerate every version of `group`. For each `(group, version)`:
3. Call `ServerResourcesForGroupVersion(gv.String())`.
4. For each `APIResource el`:
   - Skip subresources (`strings.Contains(el.Name, "/")`).
   - Form `gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: el.Name}`.
   - Form `gvk := gvr.GroupVersion().WithKind(el.Kind)`.
   - Skip if `gvk` is in `scheme.Scheme.AllKnownTypes()` (built-in; not a composition GVR).
   - Call `rw.EnsureResourceType(gvr)`. Idempotent.
   - Call `cache.OnResourceTypeAvailable(gvr)` — synchronous dirty-mark via the existing callback set (preserved verbatim from post-Ship-0 `registerCRDObject` body).
   - Increment counter `cache.discovery.group_resources_registered{group=X}`.
5. Increment counter `cache.discovery.group_resources_fetched{group=X}` once at end.

**Lag tradeoff — TRACED + accepted per Diego ratification 2026-06-01**:

| Event | Pre-v6 (CRD-informer event-driven) | v6 (one-shot discovery on walk) |
|---|---|---|
| CRD CREATE for existing nav-discovered group | Detected by CRD-informer WATCH within ~1 RTT of apiserver | Detected on **next walker pass** that walks any path under that group (bounded by walker re-entry — Phase 1 + widget CRUD re-walks) |
| CRD CREATE for new nav-discovered group | Detected when walker first walks templated path under group | Identical — walker is the trigger |
| CRD DELETE | Detected by CRD-informer DELETE event; composition informer torn down via `unregisterCRDObject` → `RemoveResourceType` | **Unbounded — informer stays until pod restart.** Composition GVR stays in `rw.informers` set; LIST/WATCH continues against a deleted CRD; apiserver returns 404 on initial LIST → informer enters error backoff. |

CRD-DELETE asymmetric lag is **acceptable** per Diego ratification 2026-06-01:
- Production CRD-DELETE cadence is near-zero (DELETE is exclusively manual operator action; CRD churn in production is dominated by CREATE on composition install).
- Worst-case impact = one composition informer per deleted-CRD-GVR in error backoff. Bounded RSS impact. No correctness violation — dispatch through the dead GVR surfaces 404 (identical to no-cache cache-off path).
- Followup task tracked at **#117**: periodic sweep (e.g., 6h ticker) that compares `rw.RegisteredGVRs()` against current discovery; tears down composition informers for GVRs whose CRD no longer exists.

**No replay-vs-discover race in v6**: the race that `ReconcileAutoDiscoverCRDs` closed in v5 (`crdwatch.go:339`) was intrinsic to the CRD-informer initial-LIST replay path. v6 deletes the CRD informer → no replay → no race. `DiscoverGroupResources` is a single synchronous transaction: list resources, register each. There is no asynchronous fan-out, no AddFunc replay window, no out-of-order delivery.

**Boot-order under v6**:
1. `main.go` → `NewResourceWatcher` (RBAC GVRs eager-registered via handler-extension registry).
2. `main.go` → `StartSecretsInformer` (primordial — F-3).
3. `main.go` → `StartControllerHealthInformer` (primordial — Resilience-1).
4. `main.go` → spawn Phase1Warmup goroutine.
5. `Phase1Warmup` → `RegisterMetaQuerySeeds` (registers **7** anchors: routesloaders + navmenus + restactions + 4 RBAC; **CRD GVR is NOT here**; identical to post-Ship-0).
6. `Phase1Warmup` → walker resolves roots; on first templated apiserver path the resolver calls `AddNavigationDiscoveredGroup(grp)` + `DiscoverGroupResources(ctx, rc, grp)` synchronously. Composition GVRs get registered via `EnsureResourceType` in-line; their informers' initial LIST + dirty-mark wiring proceed exactly as today.
7. `Phase1Warmup` → `WaitAllInformersSynced` — count-equality re-pass loop naturally includes every composition informer spawned during the walk. **No `ReconcileAutoDiscoverCRDs` call** — deleted.

### 4.2 — `IsNavigationDiscoveredGroup` survives as removable-discriminator (v6)

The group-set semantically survives v6 unchanged. Two call sites in `watcher.go` consume `matchesAutoDiscoverGroup(gvr.Group)` (renamed `IsNavigationDiscoveredGroup`):

- `watcher.go:749` (metadata-only addResourceType path) — controls whether to construct a **standalone** `metadatainformer.NewFilteredMetadataInformer` (tearable down) vs the shared factory path. Comment `:750-755` explains: standalone path is the GVR-removable path. Composition GVRs need this to be teardown-eligible. **No semantic change in v6.**

- `watcher.go:1064` (full-informer addResourceType path) — same predicate, controls whether to use `dynamicinformer.NewFilteredDynamicInformer` (standalone, removable) vs the shared factory.

Comment block `watcher.go:986-988, :1058-1060` documents the contract: *"the removable discriminator is matchesAutoDiscoverGroup — the special-case (feedback_no_special_cases.md): a GVR is removable iff its group is one the CRD-watch auto-registers, which is exactly the set RemoveResourceType is ever wired to tear down"*. v6 updates the comment text to: *"the removable discriminator is IsNavigationDiscoveredGroup — a GVR is removable iff its group is one the walker has reached via a templated apiserver path (composition GVRs)"*. Mechanism unchanged.

### 4.3 — Walker walking to a CRD-typed object: same code path as any other GVR?

[unchanged from v2 — confirmed TRACED. Walker calls `cache.KindForGVR` (post-Ship-2) → built-in scheme arm hits → forms `customresourcedefinitions` GVR → `EnsureResourceType` runs as for any built-in GVR. **v6 note**: walking *to* the CRD GVR (i.e., a frontend nav-menu that lists CRDs as resources) IS the only way the CRD informer would ever spawn under v6. This is by design — Diego's invariant 2026-06-01: "no CRD informer if the CRD object itself is not walked." In the customer corpus there is no such walk, so the informer never spawns.]

### 4.4 — Process-wide permanent store for plurals (v3, unchanged)

[unchanged from v3 §4.4 — `sync.Map` for `gvk → plurals.Info`, race-safe, no eviction, no TTL, < 100 KiB worst-case ceiling, bounded by GVR set not by traffic.]

---

## 5. Migration sequencing — three ships

### Ship 0 — DEPLOYED 0.30.222

Handler-extension registry + `StartCRDWatch` deletion + CRD-GVR walker-spawn via `sync.Once`. Removed `customResourceDefinitionGVR` from `MetaQuerySeeds()`. The CRD informer is post-Ship-0 walker-spawned (rather than boot-primordial).

Ship 0 is **deployed** at 0.30.222. The artifacts assumed by the rest of this doc as "current state" are post-Ship-0.

### Ship 0.5 — v6, this design

**Scope**: delete the CRD-watch event backplane entirely. Replace with one-shot synchronous discovery.

**Files DELETED**:
- `internal/cache/crdwatch.go` (459 LOC)
- `internal/cache/crdwatch_internal_test.go` (277 LOC)
- `internal/cache/crdwatch_lifecycle_falsifier_test.go` (373 LOC)
- `internal/cache/crdwatch_walker_spawn_test.go` (291 LOC)

**Files ADDED**:
- `internal/cache/discovery_lookup.go` (~180 LOC)
- `internal/cache/path_parse.go` (~35 LOC)
- `internal/cache/discovery_lookup_test.go` (~150 LOC)

**Files CHANGED**:
- `internal/cache/watcher.go:749, :1064` — `matchesAutoDiscoverGroup` → `IsNavigationDiscoveredGroup` (literal rename). Comment block `:986-988, :1058-1060` updated.
- `internal/cache/phase1.go:155, :216` — `customResourceDefinitionGVR` var + `CustomResourceDefinitionGVR()` accessor DELETED.
- `internal/cache/handler_registry.go` — CRD-predicate `RegisterHandlerExtension` body DELETED (registry itself stays for RBAC).
- `internal/resolvers/restactions/api/resolve.go:958-961` — walker calls `cache.AddNavigationDiscoveredGroup(grp) + cache.DiscoverGroupResources(ctx, rc, grp)` instead of just `cache.AddAutoDiscoverGroup(grp)`.
- `internal/handlers/dispatchers/phase1_walk.go` — any remaining `ReconcileAutoDiscoverCRDs()` callsite DELETED.

**Net Ship 0.5 LOC**: **−1226 LOC**.

**Risk**: medium. New invariants preserved by test:
- `RegisteredGVRs()` MUST NOT include `apiextensions.k8s.io/v1/customresourcedefinitions` post-walk (unless someone walks `kind:CustomResourceDefinition`).
- Composition informer count post-walk MUST equal post-Ship-0 baseline (proves discovery path registers the same set).
- `grep -r customResourceDefinitionGVR internal/` returns ZERO matches post-merge.
- `ls internal/cache/crdwatch*` returns no entries post-merge.

**Lag tradeoff acceptable** per Diego ratification 2026-06-01 (table in §4.1). Followup: CRD-DELETE periodic sweep tracked at #117.

### Ship 1 — additive (low risk, plurals)

Files added:
- `internal/cache/plurals_resolver.go` (~110 LOC).
- `internal/cache/plurals_resolver_test.go` — `-race`; built-in arm hit table; discovery arm fake; idempotent `LoadOrStore` race test.
- `internal/cache/plurals_resolver_bench_test.go` — built-in arm ≤ 100 ns/op, zero allocs.

Files changed:
- `internal/handlers/plurals.go` — Option A swap. Per-handler `cache.NewTTL` removed; `ServeHTTP` calls `cache.PluralFor` directly. ~8 LOC removed, ~5 LOC added.
- `internal/cache/fallthrough_meter.go` — add `ReasonPluralsDiscoveryHop` (single new counter). Old reasons still present (paths not yet replaced).

Risk: low — additive. No resolver hot-path change yet.

### Ship 2 — replacement + deletion

Files changed:
- `internal/resolvers/widgets/resourcesrefs/resolve.go:64-72` — replace per §3.2.
- `internal/resolvers/crds/schema/schema.go:21-67` — replace per §3.2.
- `internal/dynamic/dynamic.go` — DELETE lines 18-57. Remove unused imports.
- `internal/resolvers/crds/get.go` — DELETE file.
- `internal/resolvers/crds/` — DELETE directory.
- `internal/cache/fallthrough_meter.go` — DELETE `ReasonRestmapperKindFor`, `ReasonRestmapperResourceFor`, `ReasonCRDGet`.

Files added:
- `internal/handlers/dispatchers/restactions_plurals_lookup_test.go` — falsifier: `ReasonPluralsDiscoveryHop` counter rises monotonically to fixed ceiling then stays.
- `internal/cache/plurals_resolver_integration_test.go` — discovery arm against envtest; verify permanent-store HIT after first miss.

Risk: medium — resolver hot path. Rollback: `helm upgrade` to previous tag (chart in lockstep per `feedback_chart_release_lockstep`).

Cache-off transparency per `project_cache_off_is_transparent_fallback` preserved across all three ships.

---

## 6. Falsifier — v6

Per `feedback_falsifier_first_before_ship`.

### Ship 0.5 (v6) falsifier — NEW

**Pre-ship empirical capture (against 0.30.222 main)**:
1. `cache.RegisteredGVRs()` at end of Phase1Warmup INCLUDES `apiextensions.k8s.io/v1/customresourcedefinitions` (proves Ship 0 walker-spawn fires).
2. Composition informer set: capture count + list of composition GVRs registered post-walk on cyberjoker /dashboard corpus.
3. `ls internal/cache/crdwatch*` shows 4 files (`.go` + 3 `_test.go`).
4. `grep -r customResourceDefinitionGVR internal/` shows >0 matches.

**Post-ship gates (PASS = ALL true)**:
1. `cache.RegisteredGVRs()` at end of Phase1Warmup **DOES NOT** include `apiextensions.k8s.io/v1/customresourcedefinitions` (unless test corpus walks `kind:CustomResourceDefinition`; customer corpus does not).
2. Composition informer set post-walk **identical** to pre-ship capture (same count, same GVRs). Proves discovery path registers the same set the CRD-informer event backplane registered.
3. `ls internal/cache/crdwatch*` returns no entries.
4. `grep -r customResourceDefinitionGVR internal/` returns ZERO matches.
5. `grep -r ReconcileAutoDiscoverCRDs internal/` returns ZERO matches.
6. New counter `cache.discovery.group_resources_fetched{group=X}` ≥ 1 after first templated walk per group per Phase 1 pass. Specifically for the cyberjoker corpus: `cache.discovery.group_resources_fetched{group="composition.krateo.io"}` = 1 post-Phase1.
7. `cache.discovery.group_resources_registered{group="composition.krateo.io"}` equals the pre-ship composition GVR count (parity check).
8. cj /call direct-LB p50 within ±5% of post-Ship-0 baseline (Ship 0.5 is a structural refactor; should be perf-neutral; mild improvement possible from removing CRD-informer LIST+WATCH overhead).
9. `-race` clean across `discovery_lookup_test.go` and existing watcher tests.

If gate (2) fails → discovery path missed a composition GVR. Investigate `DiscoverGroupResources` body (likely the built-in scheme skip filter rejecting too broadly).

If gate (1) fails → something still spawns the CRD informer. Grep for stray `EnsureResourceType(customResourceDefinitionGVR)` callsites that were missed in the deletion sweep.

### Ship 2 (plurals) falsifier — unchanged from v3

**Pre-ship empirical capture (against 0.30.222)**:
1. No-load control profile (5s): refresher CPU = 58.23%; `bgMarkWorker` ~34%; `internal/dynamic.KindFor` + `ResourceFor` non-zero.
2. Sustained cj load profile (30s): cj direct-LB p50 = 339ms; 16-call waterfall = 1693ms; bgMarkWorker = 34.49%; refresher CPU = 52.55%; L1 hit rate = 100%.
3. Counter baseline: `cache.fallthrough.restmapper_kind_for` / `restmapper_resource_for` / `crd_get` each > 6,000/sec.
4. Unique gvk corpus: pre-walk capture of unique `(group, version, kind)` set across walker corpus. Magnitude: 50–200.

**Post-Ship-2 gates (PASS = ALL true)**:
- cj /call direct-LB p50 **≤ 80ms warm** (vs 339ms today — ≥76% reduction). **Projection: ≤50ms.**
- 16-call cj /dashboard waterfall p50 **≤ 800ms** (north-star HIT).
- `bgMarkWorker` CPU **≤ 10%** under same load (vs 34.49% today).
- `internal/dynamic.KindFor` + `ResourceFor` symbols at **0%** in pprof (they are deleted).
- `cache.fallthrough.plurals_discovery_hop` counter **≤ N_unique_gvks_in_walker_corpus** across entire process lifetime. Monotonically rises then stays.
- `cache.fallthrough.restmapper_kind_for` / `restmapper_resource_for` / `crd_get` — REMOVED from `/debug/vars`.
- L1 hit rate **≥ 99%** (no regression).
- `/api-info/names` HTTP endpoint serves 200s with identical body shape.
- Permanent store size **≤ 1,000 entries, ≤ 100 KiB** total.

If ANY miss → revert per `feedback_empirical_root_cause_trace_before_fix`.

---

## 7. Open followups

1. **CRD-DELETE periodic sweep** (tracked at **#117**). Out of scope for Ship 0.5. Implementation sketch: 6h ticker compares `rw.RegisteredGVRs()` ∩ {GVRs with non-built-in kind} against current `DiscoverGroupResources` snapshot per nav-discovered group; tears down composition informers for GVRs whose CRD no longer exists (apiserver returns ENOTFOUND). Bounded RSS impact justifies the deferral.

2. **Reverse-lookup population for plurals** (Ship 1). Discovery arm builds (gvk → info) + (gvr → kind) from the same `ServerResourcesForGroupVersion` response. No extra apiserver hop. Confirm at Ship 1 implementation.

3. **`scheme.Scheme.AllKnownTypes()` coverage** (Ship 1). Registers core/v1, apps/v1, batch/v1, networking.k8s.io, rbac.k8s.io, apiextensions.k8s.io. All standard k8s objects covered. CRDs fall through to discovery — by design. Coverage-table test in Ship 1.

4. **Walker re-walk on widget/RESTAction CRUD**. Out of scope per `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill`. Re-walks call `EnsureResourceType` + `DiscoverGroupResources` for newly-named groups; v6 discovery path is idempotent (sync.Map LoadOrStore-equivalent for the group set; `EnsureResourceType` idempotent for the GVR set).

5. **Cache-off mode behaviour**. `cache.PluralFor` / `DiscoverGroupResources` run identically regardless of `CACHE_ENABLED`. Permanent stores are process-wide.

6. **PM gate items**.
   - Confirm Ship 0.5 falsifier captures Pre-ship composition GVR set BEFORE deletion sweep (so parity check is grounded).
   - Confirm Ship 2 counter ceiling (`N_unique_gvks_in_walker_corpus`) is captured pre-ship from real walker pass.
   - Confirm rollout: stage → bench → prod, with `helm upgrade` rollback path on miss.

---

## 8. Constraints honored

- **No new endpoints** — `Plurals()` HTTP handler exists; Option A only changes the store instance.
- **No frontend modifications.**
- **No hardcoded GVRs** (`feedback_no_special_cases`) — built-in table from `scheme.Scheme.AllKnownTypes()`; nav-discovered groups exclusively walker-driven.
- **Walking INIT** — unchanged.
- **Cohort prewarm = ONE dynamic engine** (`feedback_dynamic_cohort_prewarm_no_static_no_cold_fill`) — unchanged.
- **L1 per-user-keyed** (`feedback_l1_per_user_keyed_never_cohort`) — unchanged.
- **Customer priority absolute** (`feedback_customer_priority_over_refresher`) — Ship 2 removes per-call cost; Ship 0.5 is perf-neutral but reduces RSS overhead from CRD-informer LIST+WATCH.
- **RESTMapper not required anywhere** — verified.
- **CRD informer never for plurals** — verified.
- **No CRD informer at all under v6** (Diego invariant 2026-06-01) — verified.
- **No TTL where TTL adds no value** — verified.
- **Capacity caps empirical** (`feedback_capacity_caps_empirical_per_entry_cost`) — store size bounded by GVR set cardinality.
- **Prior art** (`feedback_check_k8s_clientgo_prior_art`) — `Discovery.ServerResourcesForGroupVersion` is the canonical client-go one-shot discovery primitive.

---

## 9. Citations

All file:line references cite the **main tree** at branch `ship-0.30.185-phase-b-postjq-encoded` as of 2026-06-01, post-Ship-0 (0.30.222 deployed). Worktrees excluded.

TRACED anchors:
- `internal/cache/watcher.go:749, :1064` — `matchesAutoDiscoverGroup` removable-discriminator predicate (rename target for `IsNavigationDiscoveredGroup`).
- `internal/cache/watcher.go:986-988, :1058-1060` — comment block documenting the discriminator contract.
- `internal/cache/crdwatch.go` (459 LOC) — full file slated for deletion.
- `internal/cache/crdwatch_internal_test.go` (277 LOC), `crdwatch_lifecycle_falsifier_test.go` (373 LOC), `crdwatch_walker_spawn_test.go` (291 LOC) — three test files slated for deletion.
- `internal/cache/phase1.go:155` — `customResourceDefinitionGVR` constant (delete).
- `internal/cache/phase1.go:216` — `CustomResourceDefinitionGVR()` accessor (delete).
- `internal/resolvers/restactions/api/resolve.go:958-961` — walker side-effect site (rewire).

Empirical artifacts: `project_warm_root_cause_2026_06_01.md`, `project_refresher_walker_informer_design_2026_06_01.md`, `project_burst_root_cause_2026_05_27.md`.

External dependency surface verified against `/Users/diegobraga/go/pkg/mod/github.com/krateoplatformops/plumbing@v0.9.3/kubeutil/plurals/plurals.go`. v3+v6 uses only `plurals.Info` from this package.

Architectural guardrails: `feedback_l1_invalidation_delete_only`, `feedback_l1_per_user_keyed_never_cohort`, `feedback_customer_priority_over_refresher`, `feedback_no_special_cases`, `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill`, `feedback_falsifier_first_before_ship`, `feedback_empirical_root_cause_trace_before_fix`, `project_cache_off_is_transparent_fallback`, `feedback_check_k8s_clientgo_prior_art`, `feedback_chart_release_lockstep`, `feedback_capacity_caps_empirical_per_entry_cost`, `feedback_architect_design_rigor`.
