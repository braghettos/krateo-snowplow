# Troubleshooting — snowplow slow composition-detail `/call` (discovery storm) — 2026-06-22

**Cluster:** `gke_integration-test-431120_europe-west1-b_krateo-enterprise`, snowplow **1.4.1**, CACHE_ENABLED=true. Diagnosis read-only (no pod exec, no remote `go test`). All claims TRACED to live logs/expvar + code:line.

## Symptom
Composition-detail **widget** resolves (`progresses`, `lists`, `descriptions`; e.g. `progress-composition-detail-health`) take **4.7–5.5s**, `l1_hit:"miss"` **every** navigation. snowplow otherwise idle (0.4 CPU / 87 MiB of 4-core/8Gi; health 73ms) — the latency is apiserver round-trip latency, not CPU/scale.

## RC-1 — hot-path discovery storm (snowplow defect, fixable)
- `lazyRegisterInnerCallPaths` (`internal/resolvers/restactions/api/resolve.go:1479-1552`, called per stage at `resolve.go:471`) loops over **one RequestOptions entry per iterator dispatch** = 28 composition kinds here.
- The `DiscoverGroupResources(ctx,cfg,group)` call (`resolve.go:~1520-1538`) sits **above** the `seen` GVR-dedup map (`resolve.go:1546`, which only guards `EnsureResourceType`) → it fires **28× for the same group** `composition.krateo.io`.
- `DiscoverGroupResources` (`internal/cache/discovery_lookup.go:217`) is **deliberately not memoized** (comment `:166-171`; the per-group lock `:172` is serialize-only, no freshness short-circuit). Each call = `ServerGroups()` (1 RT) + a loop of `ServerResourcesForGroupVersion` over **all 20 union versions** (`:269`, `:373-389`) = ~21 apiserver round-trips.
- **Net: 28 × ~21 ≈ ~560 synchronous discovery round-trips on the hot resolve path**, all returning already-known state → `gvrs_spawned:0` on all 88 events/30m (`EnsureResourceType` idempotent no-op). 30 discovery events in one 1s window of one resolve. **This is the 4.7–5.5s.**
- **Version-explosion amplifier:** `composition.krateo.io` = 28 CRDs each pinned to a *distinct* version → 20-version union → 20 RTs/call (a normal 1-2-version group would cost 1-2).

## RC-2 — `vacuum` 404 → permanent uncacheability (frontend/CR-data defect; snowplow hardening only)
- 4 templated paths use version `vacuum` (`krateosnowplows`, `krateosseproxies`, `otelcollectordaemonsets`, `otelcollectordeployments`) → `dispatch:internal-rest-config` → 404 "the server could not find the requested resource".
- `vacuum` is `served:false` on those CRDs and is **absent from snowplow's discovery set** — it is templated by the **frontend RESTAction** from CR data, NOT by snowplow. Live CRs are at `v1-0-21`.
- Per-item 404 → per #313 C-A → `WARN declining to cache the partial result` → no L1 Put → `refresher_skipped_no_entry_total=148771` → cold re-resolve forever. A transient missing-version is thus promoted to a **permanent** slow path.

## Prior art (client-go)
`k8s.io/client-go/discovery/cached/memory` `NewMemCacheClient` = `CachedDiscoveryInterface` with `Invalidate()`. Current code builds a **raw** discovery client every call (`discovery_lookup.go:151-156`), bypassing it. Use the cached client + invalidate on the existing CRD-meta informer hooks (`crd_discovery_side_effect.go` / `discovery_invalidation_hook.go`).

## Fix design
- **Fix A1 (RC-1, ~8 LOC, SHIP FIRST):** hoist the `AddNavigationDiscoveredGroup`+`DiscoverGroupResources` block above the loop / gate it with a `seenGroups` map (mirror the existing `seen` GVR map). 28 calls → 1 per distinct group. **~96% reduction (~560 → ~21 RTs).**
- **Fix A2 (RC-1, ~30 LOC, durable follow-up):** swap the raw discovery client for `memory.NewMemCacheClient`, add a fresh+all-registered short-circuit in `DiscoverGroupResources`, invalidate from the CRD informer hook. Residual ~21 → ~0 on unchanged (logs show `schema_unchanged`). Gated behind the cache path (cache-off bypasses).
- **Fix B (RC-2, strategic, NEEDS frontend-RA read first):** filter templated paths to **served versions** before dispatch (served-version predicate, not a `vacuum` literal → no special-case) → guaranteed-404 becomes a clean skip → result cacheable → 2nd nav warm. **CAVEAT:** must read the live composition-detail RESTAction + `~/krateo/frontend-draganddrop/frontend` to confirm those 4 items aren't *meant* to render — if they are, the fix belongs in the frontend RA (template the served version), and snowplow filtering would hide rows. Alternative: file RC-2 against portal-chart; snowplow ships only Fix A (latency drops ~5s → cost of 28 real GETs + 4 fast 404s).

## Falsifiers (capture before coding)
- RC-1: discovery events per composition-detail resolve drop ~30 → ≤1 (A1); `ServerResourcesForGroupVersion` RTs ~560 → ≤21 (A1) / ≈0-on-unchanged (A2). Per-traceId log-grep + expvar delta.
- Latency (north-star, Chrome MCP): `progress-composition-detail-health` warm nav 4.7–5.5s → <1s. Curl /call = mechanism only.
- RC-2 (if B): `declining to cache` gone for the widget; L1 Put lands; 2nd nav `l1_hit:"hit"`.
- No-regression: a genuinely-new composition CRD CREATE is still discovered after A2 (invalidation event-driven, not time-blind) — existing CRD-CREATE discovery falsifier must still pass.

## A2 rework (post PM gate, 2026-06-22) — the ordering hazard + the corrected design
**PM/architect found A2-as-originally-designed reintroduces the S4/F-4 stuck-zero regression** (`docs/ship-0.30.233-s4-cache-invalidation-trace`). Refined, TRACED:
- The CRD-CREATE/UPDATE discovery hop runs THROUGH `DiscoverGroupResources` (`crd_discovery_side_effect.go:427`) and only THEN invalidates (`:442`). A memoized A2 would read the stale cache on CREATE → register nothing → invalidate one call too late.
- WORSE: `invalidateSADiscovery` resets the `internal/dynamic` mapper (`cached_client.go:285`), NOT any cache A2 adds in `internal/cache` — so a naive A2 cache is **blind-permanent** on the CREATE path, invalidated by nothing on the existing bridge.
- The existing `TestCRDAdd_TriggersGroupDiscovery` / `TestCRDUpdate_TriggersGroupDiscovery_BytesObject` assert `DiscoveryInvoked` (the CALL), NOT that a new GVR was registered → they MASK the defect.

**Corrected A2 design — `forceFresh` parameter (chosen over post-event invalidate):**
- `discoverGroupResources(ctx,rc,group,forceFresh)`; public `DiscoverGroupResources`→`false` (hot `/call`, cached/short-circuit, kills the storm); new `DiscoverGroupResourcesFresh`→`true` (CRD-event path at `crd_discovery_side_effect.go:427`, `Invalidate()` this group then re-read apiserver). No call-ordering dependency, no cross-group invalidation blast radius.
- **Version-complete short-circuit** (recomputed every call, NOT the v6-rejected once-flag): skip only when `cached.Fresh()` AND every registerable GVR of every currently-served version (`serverVersionsForGroup`, `discovery_lookup.go:373-389`) is already `rw.IsRegistered`. A newly-served version (spec.versions[] widen) leaves a GVR un-registered → predicate false → full discovery.
- Layering: cache-local `cacheddiscovery.NewMemCacheClient` in `internal/cache`; does NOT import the `internal/dynamic` singleton.
- Cache-off: build the cached client AFTER the `modePassthrough` guard (`discovery_lookup.go:222-227`) → CACHE_ENABLED=false byte-identical.
- `-race`: cross-group shared cache object + forceFresh `Invalidate()` = shared-vs-copy change → new race test (template `internal/dynamic/cached_client_race_test.go`).
- **Falsifier #3 (load-bearing):** prime G@v1 → fake serves G@v2 → fire CRD UPDATE → assert `DiscoveryGVRsSpawned` incremented AND `IsRegistered(G@v2)` — FAILS blind-cache, PASSES forceFresh. (The existing tests are inadequate.)
- LOC ~35-45.

**PM + architect recommendation: ship A1 alone now (certain ~96% win, zero correctness surface); A2 as a SEPARATE gated follow-up** (Falsifier #3 RED-against-blind/GREEN-against-forceFresh + the `-race` test as hard pre-merge gates). Decouple A1's certain win from A2's stuck-zero blast radius. (Diego decision pending — original directive was "ship A1+A2 together".)

## RC-1 OUTCOME: shipped + merged
A1+A2 built, dual-signed-off, 6/6 falsifiers GREEN (F-A2a RED-first stuck-zero guard independently PM-re-verified; F-A2d -race). **Merged to snowplow main as PR #42 (commit 2fde465).** Untagged/undeployed (deploy avoided). Discovery storm ~560 → ~21 (A1) → ~0-on-unchanged (A2) RTs per composition-detail resolve.

## RC-2 ROOT CAUSE: CONFIRMED (2026-06-22) — portal `composition-detail` RESTAction selects the storage version, not the served version
**SOURCE (TRACED, live CR + local chart):** `~/krateo/krateo-portal-chart/chart/templates/restaction.composition-detail.yaml:25` — the `crds`-step filter does `version: .status.storedVersions[0]` (the CRD STORAGE version), and `:31` builds `/apis/composition.krateo.io/ + .version + / + .plural`. For 4 of the 28 composition CRDs the storage version is `vacuum` (served:false) while the API is served at `v1-0-21`/`v0-1-4`/`v0-1-2`/`v0-2-1` → guaranteed 404 → uncacheable. **Latent across all 28** (24 happen to have storedVersions[0]==served; any future storage-version bump breaks more).
- **The 4 items ARE meant to render** (live CRs exist at the served version) → snowplow must NOT skip/filter (would hide real rows) and must NOT silently substitute the version (cross-team behavioral compensation, violates `project_snowplow_resilience_invariant` + `feedback_no_special_cases`). **snowplow ships NOTHING for RC-2.**
- **Fix = one line in the portal chart** (braghettos/krateo-portal-chart, NO upstream; helm-only per `feedback_helm_only_for_portal`): `version: .status.storedVersions[0]` → `version: ([.spec.versions[] | select(.served==true)][0].name)` (or storage-preferred-if-served: `(([.spec.versions[]|select(.served and .storage)][0]) // ([.spec.versions[]|select(.served)][0])).name`). The CRD discovery payload already exposes `spec.versions[].served/.storage` — no extra api step. Fixes all 28.
- **Falsifier:** post-fix, the `found` step path is `/apis/composition.krateo.io/v1-0-21/krateosnowplows` (served) → 200 → no "declining to cache" → L1 Put lands → 2nd nav `l1_hit:hit`.
