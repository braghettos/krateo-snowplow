---
type: Decision
title: "ADR 0005 — Walker-driven informer set; no per-call RESTMapper on the hot path"
description: >-
  Exactly 7 hardcoded meta-query informer anchors; every business GVR is
  discovered by resolution. Composition GVRs come from a cached, per-group
  singleflighted discovery hop driven by the walker plus a CRD-event
  side-effect bridge (with event-driven delete teardown). The per-call cold
  RESTMapper rebuild is gone; a cached mapper and a permanent plural store
  replace it.
resource: snowplow
tags:
  - adr
  - cache
  - informers
  - discovery
status: diverged
timestamp: 2026-08-06T00:00:00Z
---

# ADR 0005 — Walker-driven informer set; no per-call RESTMapper on the hot path

- **Status:** Accepted; the realized tree walked back two of the original v6 design's absolutes
  (recorded below) and has since evolved the discovery hop itself (cached client, freshness
  short-circuit, forced-fresh event path).
- **Design history:**
  [`docs/walker-driven-informer-design-2026-06-01.md`](../walker-driven-informer-design-2026-06-01.md)
  (the v6 plan). Where that note and the current tree disagree, the code wins.
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
  `MetaQuerySeeds()` (`internal/cache/phase1.go`) registers **exactly 7** informer anchors —
  `routesloaders`, `navmenus`, `restactions`, and the 4 RBAC types — and *no* business GVR. A
  test asserts the slice is exactly those 7. `customresourcedefinitions` is **not** a seed (the
  CRD-meta informer that does exist is registered lazily by the walker — below).
- **Composition GVRs are discovered by an apiserver discovery hop**
  (`cache.DiscoverGroupResources`, `internal/cache/discovery_lookup.go`), invoked synchronously
  from the walker/resolver the first time a templated apiserver path is reached for a group
  (`AddNavigationDiscoveredGroup` + `DiscoverGroupResources` at the discovery branch of
  `internal/resolvers/restactions/api/resolve.go`). It lists `ServerResourcesForGroupVersion`
  per version of the group and, for each non-built-in `Kind` (filtered against
  `scheme.Scheme.AllKnownTypes()`), forms the composition GVR and registers its informer via
  `EnsureResourceType`. The hop has since grown three refinements over the original "one-shot"
  transaction: it runs against a **cached** discovery client (a warm walk pays zero apiserver
  round-trips), it is **singleflighted per group**, and it **short-circuits** when the cached
  surface is fresh and every served version's GVR is already registered. The CRD-*event* path
  uses `DiscoverGroupResourcesFresh` (forceFresh), which invalidates the cached surface first so
  a CRD CREATE/UPDATE can never register against a stale read.
- **The dedicated CRD-watch backplane stays deleted.** `crdwatch.go` (459 LOC) and its tests are
  gone. What replaced the *event* concern is not a backplane but a side-effect bridge — see the
  walk-backs below. The single sanctioned CRD-GVR literal lives in `internal/cache/crd_gvr.go`
  (`IsCRDGVR`, the predicate the event bridge consults); every other reference consumes the
  predicate rather than reconstructing the literal.
- **The navigation-discovered group set is load-bearing as the removable discriminator.**
  `IsNavigationDiscoveredGroup(group)` (`internal/cache/discovery_lookup.go`) is the predicate in
  `watcher.go` that decides whether a GVR gets a *standalone* informer (tearable down via
  `RemoveResourceType`) versus a shared-factory one. Composition GVRs must be removable, so their
  group must be in this set.
- **No per-call RESTMapper on the hot path.** The per-`/call` cold restmapper is gone from
  `internal/dynamic/dynamic.go`; that file is now pure shape accessors with zero apiserver
  round-trips. Plural⇄kind resolution on the resolver path goes through a never-expiring
  process-wide store (`internal/cache/plurals_resolver.go`): built-in scheme first, permanent
  `sync.Map` next, one discovery hop on miss — bounded by the GVR set, not by traffic.

## Consequences

- **The warm-path allocation/CPU defect is removed.** No fresh restmapper per call; the refresher
  no longer hot-loops discovery. Plural resolution is an in-process map hit after first touch.
- **No CRD informer LIST/WATCH cost at boot.** The CRD-meta informer is registered lazily by the
  walker (`lazyRegisterInnerCallPaths`) only when a walked path actually touches CRDs; composition
  discovery is a synchronous hop the first time a group is navigated; subsequent CR events flow
  through the normal informer → `DepTracker` → refresher path.
- **CRD lifecycle is event-driven, walker-backstopped.** A CRD CREATE for a new group is picked up
  by the AddFunc side-effect bridge (below) without waiting for a walk; the walker's periodic
  passes remain the backstop.
- **Strictly read-only discovery.** The walk recurses only on `verb == "GET"` children
  (`walkShouldRecurse`, `internal/handlers/dispatchers/phase1_walk.go`), so the SA's privileged
  credentials never reach a mutation endpoint while discovering informers.
- **Toggle-able and removable** per ADR 0004: under `CACHE_ENABLED=false` there are no informers
  to register — `discoverGroupResources` no-ops in passthrough mode — while the permanent
  plural/kind stores remain live (they are correctness-neutral in-process resolution, independent
  of the cache toggle).

## Walk-backs from the v6 design (code wins)

The v6 design note is a *plan*; two of its strongest claims were walked back in the realized tree:

1. **"No CRD informer at all, ever; discovery only from the walker."** Not true as shipped. The
   walker-only chain had a *stuck-zero-state race*: when a CRD is created at runtime, stage 1 of a
   compositions-list serves the cached `crds` LIST (which doesn't yet include the new CRD), the
   stage-2 iterator is empty, the discovery hop is never reached, and the composition informer is
   never registered (traced in
   `docs/notes/ship-0.30.233-s4-cache-invalidation-trace-2026-06-02.md`).
   **Ship 0.30.233** restored an event path — but in a simpler form than the deleted backplane:
   **one side-effect hook on the walker-registered `customresourcedefinitions` informer's
   AddFunc** (`internal/cache/crd_discovery_side_effect.go`, routed via `deps_watch.go` +
   `IsCRDGVR`), handing each CRD ADD/UPDATE to a bounded worker (depth 256, drop-on-full + WARN)
   that calls `AddNavigationDiscoveredGroup` + `DiscoverGroupResourcesFresh` *off* the informer
   processor goroutine. **Ship L (0.30.246)** added the DeleteFunc branch for event-driven
   CRD-DELETE teardown. So a CRD-meta informer *does* exist (lazily, when the walk touches CRDs);
   the thing that was deleted is the dedicated 459-LOC discovery *backplane*, not the CRD informer
   concept. The design's `#117` periodic-sweep followup was closed as superseded by event-driven
   delete (2026-06-12).

2. **"RESTMapper / `DeferredDiscoveryRESTMapper` never constructed anywhere; terminology
   expunged."** Not true as shipped. The RESTMapper is *removed from the per-`/call` hot path*, but
   it is **retained and now cached** for cluster-scoped CRD/schema reads: `internal/dynamic/
   client.go` and `internal/dynamic/cached_client.go` still construct a
   `DeferredDiscoveryRESTMapper(NewMemCacheClient(...))`, the difference being the **built** mapper
   is now reused across calls instead of rebuilt per call. The win was eliminating the *per-call
   cold rebuild*, not eradicating the type.
