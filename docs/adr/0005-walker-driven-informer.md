# ADR 0005 — Walker-driven informer set; no process-wide RESTMapper on the hot path

- **Status:** Accepted
- **Lead (verify against code):**
  [`docs/walker-driven-informer-design-2026-06-01.md`](../walker-driven-informer-design-2026-06-01.md)
  (the v6 design). Where that note and the current tree disagree, the code wins — see
  *Divergences from the design note* below.
- **Deep dive:** [`docs/architecture/prewarm.md`](../architecture/prewarm.md).

## Context

Snowplow must serve `/call` for an open-ended set of GVRs (compositions, widgets, panels, plus
every CRD-backed resource the frontend navigates) without a static GVR catalog and without
hardcoding navigation paths. Two prior designs created hot-path cost and complexity:

1. **Per-call RESTMapper.** `internal/dynamic/dynamic.go` constructed a fresh
   `DeferredDiscoveryRESTMapper(NewMemCacheClient(...))` per `/call`. The mem-cache wrapper was
   fresh per invocation, so it cached nothing across calls: every call did a full apiserver
   discovery LIST + JSON parse + `AddSpecific` over all GVRs. The refresher hot loop drove this
   ~186K times per 30s (~6.2K items/sec), accounting for ~700 MB of allocations per 30s and a
   large share of steady-state CPU — the dominant warm-path defect.

2. **A dedicated CRD-watch backplane.** A separate in-process informer LIST/WATCHed
   `apiextensions.k8s.io/v1/customresourcedefinitions` purely to auto-discover composition GVRs
   (the ~459-LOC `crdwatch.go`). On a 30K-CRD cluster its initial LIST + steady-state WATCH cost
   was non-trivial, and its only payoff was reacting to CRD CREATE/DELETE, whose production cadence
   is near-zero.

## Decision

**The walker is the sole source of "which informers run."** Navigation roots, the cohort set, and
every business GVR are read from config / the live RBAC index / the walk — never Go literals.

- **No static GVR catalog.** The only hardcoded GVRs at boot are the meta-query anchors:
  `MetaQuerySeeds()` (`phase1.go:250`) registers **exactly 7** informer anchors — `routesloaders`,
  `navmenus`, `restactions`, and the 4 RBAC types — and *no* business GVR. A test asserts the slice
  is exactly those 7. `customresourcedefinitions` is **not** a seed.
- **Composition GVRs are discovered by one-shot apiserver discovery**, invoked synchronously from
  the walker the first time it reaches a templated apiserver path for a group:
  `DiscoverGroupResources(ctx, rc, group)` (`discovery_lookup.go:217`), reached from the resolver
  at `resolvers/restactions/api/resolve.go:1335` (`AddNavigationDiscoveredGroup` +
  `DiscoverGroupResources`). It lists `ServerResourcesForGroupVersion` per version of the group,
  and for each non-built-in `Kind` (filtered against `scheme.Scheme.AllKnownTypes()`) forms the
  composition GVR and registers its informer via `EnsureResourceType`.
- **The dedicated CRD-watch backplane is deleted.** `crdwatch.go` (459 LOC) and its three test
  files are gone; the `customResourceDefinitionGVR` constant and accessor are removed
  (`grep customResourceDefinitionGVR internal/` → zero). `DiscoverGroupResources` is a single
  synchronous transaction — list, register, dirty-mark, return — so there is no initial-LIST replay
  window and no replay-vs-discover race to reconcile.
- **The navigation-discovered group set is load-bearing as the removable discriminator.**
  `IsNavigationDiscoveredGroup(group)` (`discovery_lookup.go:113`, renamed from
  `matchesAutoDiscoverGroup`) is the predicate at `watcher.go:749` / `:1064` that decides whether a
  GVR gets a *standalone* informer (tearable down via `RemoveResourceType`) versus a shared-factory
  one. Composition GVRs must be removable, so their group must be in this set.
- **No process-wide RESTMapper on the hot path.** The per-`/call` cold restmapper is gone from
  `internal/dynamic/dynamic.go`; that file is now pure shape accessors with zero apiserver
  round-trips. Plural⇄kind resolution on the resolver path goes through a never-expiring
  process-wide store (`plurals_resolver.go`): built-in scheme first, permanent `sync.Map` next,
  one discovery hop on miss — bounded by the GVR set, not by traffic.

## Consequences

- **The warm-path allocation/CPU defect is removed.** No fresh restmapper per call; the refresher
  no longer hot-loops discovery. Plural resolution is an in-process map hit after first touch.
- **No CRD informer LIST/WATCH for the common case.** Composition discovery is a synchronous hop
  the first time a group is navigated; subsequent CR events flow through the normal informer →
  `DepTracker` → refresher path.
- **Walker is the trigger; lag is bounded and accepted.** A CRD CREATE for an already-navigated
  group is picked up on the next walker pass under that group (Phase 1 + CRUD re-walks). This was
  ratified as acceptable given near-zero production CRD-DELETE cadence.
- **Strictly read-only discovery.** The walk recurses only on `verb == "GET"` children
  (`walkShouldRecurse`, `phase1_walk.go:1480`), so the SA's privileged credentials never reach a
  mutation endpoint while discovering informers.
- **Toggle-able and removable** per ADR 0004: under `CACHE_ENABLED=false` the walk's harvesters are
  nil and discovery still runs identically (the permanent stores are process-wide, independent of
  the cache toggle).

## Divergences from the design note (code wins, per the documentation rules)

The v6 design note (`docs/walker-driven-informer-design-2026-06-01.md`) is a *plan*; two of its
strongest claims were walked back in the realized tree:

1. **"No CRD informer at all, ever; discovery only from the walker."** Not true as shipped. The
   walker-only chain had a *stuck-zero-state race*: when a CRD is created at runtime, stage 1 of a
   compositions-list serves the cached `crds` LIST (which doesn't yet include the new CRD), the
   stage-2 iterator is empty, the discovery hop is never reached, and the composition informer is
   never registered (traced in `ship-0.30.233-s4-cache-invalidation-trace-2026-06-02.md`).
   **Ship 0.30.233** restored an event path — but in a simpler form than the deleted backplane:
   **one side-effect hook on the existing `customresourcedefinitions` informer's AddFunc**
   (`crd_discovery_side_effect.go`), handing each CRD ADD/UPDATE to a bounded worker
   (depth 256, drop-on-full + WARN) that calls `AddNavigationDiscoveredGroup` +
   `DiscoverGroupResources` *off* the informer processor goroutine. **Ship L (0.30.246)** added the
   DeleteFunc branch for event-driven CRD-DELETE teardown. So a CRD-meta informer *does* exist; the
   thing that was deleted is the dedicated 459-LOC discovery *backplane*, not the CRD informer
   concept. The design's `#117` periodic-sweep followup was closed as superseded by event-driven
   delete (2026-06-12).

2. **"RESTMapper / `DeferredDiscoveryRESTMapper` never constructed anywhere; terminology
   expunged."** Not true as shipped. The RESTMapper is *removed from the per-`/call` hot path*, but
   it is **retained and now cached** for cluster-scoped CRD/schema reads: `internal/dynamic/
   client.go` and `internal/dynamic/cached_client.go` still construct a
   `DeferredDiscoveryRESTMapper(NewMemCacheClient(...))`, the difference being the **built** mapper
   is now reused across calls instead of rebuilt per call. The win was eliminating the *per-call
   cold rebuild*, not eradicating the type.
