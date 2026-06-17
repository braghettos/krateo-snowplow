# ADR 0004 — All caching is provisional and removable; `CACHE_ENABLED=false` is a transparent fallback

- **Status:** Accepted
- **Related:** ADR 0002, ADR 0003 (the cache layers this invariant governs).
  Deep dive: [`docs/architecture/caching.md`](../architecture/caching.md).

## Context

Snowplow's entire cache subsystem — the L3 informer cache, the L1 resolved-entry cache, the
dispatcher seam, prewarm, the typed-RBAC snapshot — exists for one reason: to serve the Krateo
frontend fast enough at 50K scale without re-hitting the apiserver on every `/call`. It is an
optimisation, not the source of truth. The apiserver is.

Two risks follow from a cache that becomes load-bearing for *correctness*:

1. If the cache ever returns data, UI, or RBAC verdicts that differ from a direct apiserver read,
   it is a bug surface that is hard to reason about and hard to roll back.
2. Kubernetes may eventually ship better server-side caching (consistent watch caches, etc.),
   at which point snowplow's in-process cache should be removable without a rewrite.

## Decision

**The cache is provisional and must stay cleanly removable.** This is enforced by a single master
toggle and the invariant that turning it off is a *transparent fallback*, not a degraded mode.

- `CACHE_ENABLED` is the master gate (`cache.go:37` `Disabled()`). With it off, all three tiers
  vanish and **every read goes straight to the apiserver — same data, same UI, same RBAC, just
  slower.** This is not a reduced-functionality path; it is the correctness baseline.
- The fallback is enforced at every tier, by construction, not by special-casing:
  - **L3** switches to `modePassthrough`: `GetObject` issues a live `Get` (`watcher.go:1616`) and
    `ListObjects` issues a fresh paged LIST (`watcher.go:1670`) instead of reading the indexer.
    Same `GetObject`/`ListObjects` surface, two backing implementations chosen once at
    construction (`NewResourceWatcher`).
  - **L1** `ResolvedCache()` returns `nil` and every consumer nil-checks and resolves directly
    (`resolved.go:547-550`, `:484-494`).
  - **RBAC** routes through `UserCan` → `SelfSubjectAccessReview` against the user's own kubeconfig
    (`rbac.go:62-103`), and the dispatcher's in-process `EvaluateRBAC` gate is skipped because the
    per-user apiserver fetch enforces RBAC inline (`restactions.go:92-108`). There is **zero
    `SubjectAccessReview` to the apiserver in cache-on mode**, and `userCanViaSAR` MUST be
    reachable only when `cache.Disabled() == true` (`rbac.go:62-64`).
  - **Prewarm** makes its harvesters nil and the seed a no-op under the master gate
    (`phase1_walk.go:351-395`).
- `CACHE_ENABLED` is the single master gate; prewarm and the informer-serve pivot are implicit
  under it. Only fine-grained back-out knobs remain (`RESOLVED_CACHE_ENABLED`,
  `WIDGET_CONTENT_L1_ENABLED`, `RESOLVED_CACHE_APISTAGE_ENABLED`, `cache.go:15-24`) for narrowing
  a regression to one tier without losing the others.
- The subsystem is kept structurally removable (`cache.go:10-13`).

## Consequences

- **Instant, safe rollback.** A cache-introduced regression is mitigated by one helm `--set
  CACHE_ENABLED=false`; the pod keeps serving correct content from the apiserver, just slower.
  This is the documented incident lever.
- **The cache can never be the correctness story.** Because cache-off is the baseline, any
  cache-on/cache-off behavioural divergence is by definition a bug — which makes the cache testable
  against a known-correct reference (the same `/call` with the toggle flipped).
- **Removability is preserved.** When Kubernetes offers adequate server-side caching, snowplow can
  drop the in-process tiers without changing the request contract, because the passthrough path
  already *is* that world.
- **Performance is the only thing you lose by turning it off** — cold reads, no prewarm, a
  per-request SAR per RBAC check. That is the intended trade: correctness is constant, latency is
  the variable.
- **Boundary for contributors.** A change that makes cache-off return *different* data/UI/RBAC, or
  that makes any correctness path depend on the cache being on, violates this ADR.
