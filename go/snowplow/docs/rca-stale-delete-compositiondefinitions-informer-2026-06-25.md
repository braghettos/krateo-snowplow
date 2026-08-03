# RCA + design — deleted CompositionDefinition persists in /blueprints (informer/invalidation gap) — 2026-06-25

**Author**: cache architect
**Status**: TRACE + DESIGN (design-only; not a build). Supersedes the leading "cache-key divergence" hypothesis in `~/Downloads/snowplow-rca-blueprint-cache-stale-delete.md` (that hypothesis is REFUTED below).
**Cluster / artifacts**: `gke_integration-test-431120_europe-west1-b_krateo-enterprise-full`, snowplow **1.5.1** (chart 1.0.27). Empirical artifacts captured this session: expvar `/tmp/sp-vars.json`, snowplow log tail `/tmp/sp-ke.log` (4000 lines). Read-only access via `/tmp/krateo-enterprise.kubeconfig`. Code reviewed @ `1.5.2-1-gd86e33d` (this tree); 1.5.1↔1.5.2 are adjacent and every path cited matches.
**Every claim labelled TRACED (runtime artifact + file:line) or INFERRED.**

---

## 1. Executive summary (5 lines)

1. **Root cause:** the `compositiondefinitions` cluster-LIST that backs `/blueprints` is served on the customer path from the **apistage CONTENT cache HIT** (which short-circuits *before* `dispatchViaInformer` — no informer dispatch counter fires on `call-generic`), while the cached **resolved-output** `/blueprints` entry sits in the same store under a different key; the only thing that evicts either is an informer DELETE event for `compositiondefinitions`, and that event is **not reaching the dep tracker** — `not-servable:28` (boot-prewarm-walk) + **zero** `cache_event.consumed type=DELETE gvr=…compositiondefinitions` in the log = the data informer is not delivering DELETEs (registered-but-not-servable / watch not vouched).
2. **Fix #1 (preferred):** make the api-step LIST path establish, and *confirm*, a synced+watch-healthy informer for the LISTed GVR so the EXISTING `OnDelete → collectMatchesWithDep(cluster-list bucket) → dirty-mark` machinery fires — i.e. close the gap that lets a GVR be `registered` yet permanently `not-servable` and (the real bug) treats "served from apistage content cache" as terminal, never re-touching the informer.
3. **Scale/OOM verdict:** for `compositiondefinitions` it is ONE cluster-wide CR informer (cheap, already registered). The generalization "informer for every api-step-LISTed GVR" is the dangerous one — it is exactly the unbounded cold-informer population that drove the 1.5.1 boot-OOM (`docs/troubleshoot-boot-oom-composition-fanout-2026-06-23.md`). The fix MUST NOT register on the api-step *GET-by-name* path (would fan out to every child GVR) and MUST keep the registration **lazy + idempotent + first-read-still-falls-through** (no boot populate burst).
4. **Fix #2 (fallback if #1's generalization is unsafe at scale):** bound the staleness of *apistage-content + resolved-output entries that were served while the GVR was not informer-servable* — a short, toggleable TTL applied per-entry when `IsServable(gvr)==false` at Put time, so the catalog self-heals in seconds instead of 3600s, with no new watch.
5. **Falsifier:** in-process/kind — register a CR informer, drive a DELETE, assert `cache_event.consumed type=DELETE` fires AND the dependent resolved-output L1 key is evicted/dirty-marked; on-cluster — after fix, `kubectl delete compositiondefinition X` makes `/blueprints` reflect it within seconds and emits `cache_event.consumed type=DELETE gvr=…compositiondefinitions`.

---

## 2. Empirical evidence (what the runtime artifacts actually show)

All from `/tmp/sp-vars.json` (`snowplow_apiserver_fallthrough_cells`) and `/tmp/sp-ke.log`.

| Observation | Value | Source |
|---|---|---|
| `boot-prewarm-walk\|…compositiondefinitions\|informer-fallthrough-not-servable` | **28** | `sp-vars.json` |
| `boot-prewarm-walk\|…compositiondefinitions\|informer-fallthrough-not-synced` | **1** | `sp-vars.json` |
| `call-generic\|…compositiondefinitions\|get-miss-let-apiserver-404` | **9** | `sp-vars.json` |
| `call-generic\|…compositiondefinitions\|` informer-fallthrough-* (any) | **ABSENT** | `sp-vars.json` |
| `call-generic\|…compositiondefinitions\|` informer-LIST-served | n/a (served path uses a separate counter, not in fallthrough_cells) | metrics taxonomy |
| `compositiondefinitions` ∈ `snowplow_plurals_registered_gvrs.gvrs` (count 61) | **YES** (this expvar IS `rw.informers`, see below) | `sp-vars.json` |
| `cache_event.consumed type=DELETE gvr=…compositiondefinitions` in log | **0** (the single `cache_event.consumed` in the window is `type=UPDATE gvr=/v1,namespaces`) | `sp-ke.log:3176` |
| log GETs `/…/compositiondefinitions/aws-vpc-stack` → `not found` | present (the 9 post-delete GET-by-name probes) | `sp-ke.log` |

**Decisive reading of the counters:**
- The `not-servable:28` / `not-synced:1` are scoped **exclusively under `boot-prewarm-walk`** — they describe the boot window, when the lazily-registered informer had not yet synced. On the **customer (`call-generic`) path the LIST produces NO informer-fallthrough counter at all** — the LIST never reaches `dispatchViaInformer` post-boot.
- `snowplow_plurals_registered_gvrs` is **misnamed**: TRACED `internal/cache/registered_gvrs_expvar.go:28` + `internal/cache/phase1.go:412` — it reads `rw.RegisteredGVRs()`, which snapshots **`rw.informers`** under RLock. So the count-61 list IS the live informer registry. **`compositiondefinitions` has a registered informer.** (This REFUTES the team-lead brief's "no synced data informer exists" framing — the informer object exists; what's missing is event delivery.)

---

## 3. Why the RCA's "cache-key divergence" hypothesis is REFUTED

The Downloads RCA's leading hypothesis was that the dep edge is recorded under a different key than the served entry. TRACED refutation:

- The dispatcher threads the **resolved-output `cacheKey`** into the resolve context (`internal/handlers/dispatchers/restactions.go:196` `cache.WithL1KeyContext(ctx, cacheKey)`), and `cacheKey`/`cacheHandle` both come from `cache.ResolvedCache()` (`internal/handlers/dispatchers/helpers.go:201`).
- The inner api-step records its cluster-list edge **inline, off `r.ctx`'s L1 key, before dispatch and regardless of how the call is served** (`internal/resolvers/restactions/api/resolve.go:1365-1383`): `if l1Key := cache.L1KeyFromContext(r.ctx); l1Key != "" … RecordList(l1Key, gvr, ns)`. On the customer resolved-output **MISS** (the only time the resolver runs), `r.ctx`'s L1 key IS the served `cacheKey`.
- Therefore the `/blueprints` resolved-output entry **does** get a `(compositiondefinitions, "", listWildcard)` cluster-list edge under the key the user reads. The edge is recorded under the right key. Key-divergence is not the bug.

The bug is upstream of the dep model: **the DELETE event never arrives**, so `OnDelete` (`internal/cache/deps.go:590`) is never called for `compositiondefinitions`, so neither the resolved-output edge nor the apistage-content edge is ever acted on.

---

## 4. The full TRACED chain (symptom → root cause)

### 4.1 How the catalog is served (two coexisting entries, one store)
- `apistageStore` and the dispatcher's resolved-output cache are the **same** `cache.ResolvedCache()` store (`resolve.go:272`; `helpers.go:201`) — different keys, one store.
- Customer `/blueprints` resolved-output HIT short-circuits the whole resolver (`restactions.go:135-148`, `writeResolvedJSON` + early `return`). After the first miss populates it, every later `/blueprints` is a resolved-output HIT and the resolver **never runs again** → the edge is recorded once and the entry is never recomputed except by invalidation or TTL.
- Inside the resolver (only on a resolved-output miss), the single api-step LIST is served by the **apistage content cache**: `apistageContentServe` (`apistage.go:425`) does `store.Get(contentKey)`; on a **content HIT it returns the stored envelope WITHOUT calling `dispatchViaInformer`** (`apistage.go:481-516`). This is exactly why `call-generic` shows no informer-fallthrough counter for the LIST.
- On a content MISS it calls `dispatchViaInformer(WithApistageContentResolve(ctx), call)` (`apistage.go:517`). For `compositiondefinitions` this returns not-servable (Gate 6 / list_not_servable) and falls through to the apiserver, then Puts the content entry + a `RecordList(contentKey, gvr, ns)` edge (`apistage.go:503`).

### 4.2 The informer + its DELETE wiring (this part is CORRECT)
- `EnsureResourceType` (`watcher.go:612`) registers a full bytes-streaming dynamic informer for `compositiondefinitions` (it is NOT metadata-only: `shouldUseMetadataOnly` is constant-false in prod, `cache_mode.go:127-191`, #342/#197; it is NOT navigation-discovered so it takes the shared factory at `watcher.go:1147` `rw.factory.ForResource(gvr)`, H5 streaming default at `watcher.go:1101`).
- `addResourceTypeLocked` wires the dep handlers UNCONDITIONALLY at registration (`watcher.go:1213` `gi.Informer().AddEventHandler(rw.depEventHandlers(gvr))`), on both eager and lazy paths.
- `depEventHandlers.DeleteFunc` (`deps_watch.go:237-257`) is **NOT post-sync gated** (only `AddFunc` is, `deps_watch.go:200-203`). It unwraps `DeletedFinalStateUnknown`, computes `(ns,name)`, and `submitDeleteEvent` → worker → `Deps().OnDelete(gvr, ns, name)` (`deps_watch.go:117/123`).
- `OnDelete` (`deps.go:590`) → `collectMatchesWithDep(gvr, ns, name)` returns the cluster-list bucket `(gvr,"",listWildcard)` for a namespaced delete; non-self matches are dirty-marked (enqueue refresh), self-representations evicted; it logs `cache_event.consumed type=DELETE` (`deps.go:638`).

**So the moment a DELETE event is delivered for `compositiondefinitions`, the existing machinery dirty-marks the `/blueprints` resolved-output key AND the apistage content key.** No new invalidation path is needed.

### 4.3 The actual break (TRACED by absence + INFERRED final link)
- **TRACED:** zero `cache_event.consumed type=DELETE gvr=…compositiondefinitions` in `/tmp/sp-ke.log` across the post-delete probe window (the only consumed event is the namespaces UPDATE at `sp-ke.log:3176`). The DELETE event is not reaching `OnDelete`.
- **TRACED:** the informer is `registered` (count-61) but was `not-servable:28` throughout boot-prewarm. `servableLocked` (`watcher.go:1822`) is a 4-conjunct gate: `registered ∧ HasSynced ∧ watchHealthy(¬watchBroken) ∧ typeConfirmed(∈rw.confirmed)`. `not-servable` (as opposed to `not-synced`) means it failed conjunct 3 or 4 *after* registration — i.e. **watch broken or type-unconfirmed**, not merely mid-sync.
- **INFERRED (cannot close on-cluster — `kubectl exec` to read live HasSynced/watchBroken is a Production Read denied without explicit user approval):** the `compositiondefinitions` data informer's reflector watch is not in a vouched-healthy state (`watchBroken` set, or `confirmed` never set because the discovery-refresh tick has not confirmed it), and in that state it is **not delivering DELETE events to the processor** — which is why `OnDelete` never fires and the entry survives to TTL. Conjunct-4 (`resourceTypeConfirmedLocked`, `watcher.go:1844`) requires `rw.confirmed[gvr]`, populated by the discovery-refresh ticker over **all `rw.informers`** (`servable.go:228-271`) — if discovery flapped for `core.krateo.io/v1alpha1` (the same group whose generated child CRDs churn at install/uninstall, driving the `crd_discovery_side_effect.go` relist/teardown machinery), the GVR can be un-confirmed (`servable.go:271` `delete(rw.confirmed, gvr)`), latching `not-servable`.

**Closing this last link empirically is the falsifier's job (§7) — the design below is correct for either remaining sub-case** (watch-not-healthy vs content-cache-shields-the-informer), because both are resolved by "establish-and-confirm a healthy watch on the api-step LIST path, then let the existing DELETE machinery run."

---

## 5. Prior-art check (`feedback_check_k8s_clientgo_prior_art`)

Does client-go already solve "a registered informer that isn't delivering events / isn't confirmed"? No single primitive does, but the relevant primitives are already wired in this repo and the fix REUSES them — it does not reinvent:
- `cache.WaitForCacheSync` / per-GVR `HasSynced` polling — already re-implemented as the per-GVR `syncCh` (`watcher.go:1170`, `informer_dispatch.go` R2-b bounded wait).
- `SetWatchErrorHandler` → `watchBroken` conjunct-3 recovery on `LastSyncResourceVersion` advance (`servable.go:273-289`) — the existing watch-health machine.
- Discovery-driven `confirmed` set (conjunct 4, the S4 fix) — `servable.go`.
- The metadata-only `NewFilteredMetadataInformer` bounded watch (`watcher.go:931`) — the prior art for a cheap bounded informer, already analysed in `docs/proactive-cr-warm-design-2026-06-20.md §1` (but that doc is PARKED and is about *un-navigated* CRs; this defect is a *navigated-via-api-step* CR, a different gap).

The fix is a **confirm/heal + re-touch** change on an existing informer, not a new cache.

---

## 6. Fix design

### Fix #1 (PREFERRED) — confirm + serve the api-step LIST via a healthy informer, so the existing DELETE machinery fires

**Mechanism (two coupled parts):**

**(1a) Stop letting the apistage content-cache HIT permanently shield the informer.** Today a content HIT never re-touches `dispatchViaInformer`, so once an entry is Put while the GVR is `not-servable`, nothing ever re-establishes the watch for that LIST. Add, on the apistage content path for a **LIST** (`apistage.go` HIT branch, `apistage.go:481`): a cheap, idempotent `rw.EnsureResourceType(gvr)` (sub-µs singleflight when already registered — `watcher.go:625`) so a registered-but-not-yet-confirmed GVR keeps being nudged, and a one-line servability observation. This does NOT change what is served (still the content HIT) — it only guarantees the informer for a LISTed GVR stays *registered + on the discovery-refresh confirm path*. Bound: O(1) map check per content-HIT; place AFTER the cache read (`feedback_bounding_mechanism_discipline`).
LOC: ~10. Target: `internal/resolvers/restactions/api/apistage.go` (LIST HIT branch, ~`:481-516`).

**(1b) Confirm-on-register for the api-step LIST GVR.** The latch is conjunct 4 (`rw.confirmed`) and/or conjunct 3 (`watchBroken`). The discovery-refresh ticker already iterates `rw.informers` (`servable.go:228`), so a registered GVR WILL be (re)confirmed within `discoveryRefreshInterval` (30 s) **unless** discovery for its group keeps failing. Fix: on the LIST path, when `dispatchViaInformer` registers a GVR (Gate 6, `informer_dispatch.go:359`), **prime one confirmation pass for that GVR** (call the existing per-GVR confirm path, or trigger `RefreshDiscovery` scoped to it) so the first post-boot LIST does not wait a full tick, and a transient boot-time discovery flap self-corrects. This reuses `servable.go`'s confirm machinery; no new predicate.
LOC: ~15. Target: `internal/resolvers/restactions/api/informer_dispatch.go` (Gate 6, after `EnsureResourceType`) + a small exported scoped-confirm helper in `internal/cache/servable.go`.

**Net effect:** once the `compositiondefinitions` informer is confirmed+watch-healthy (the common case within 30 s of boot, guaranteed by 1b; sustained by 1a), its reflector delivers the DELETE → `DeleteFunc` → `OnDelete` → `collectMatchesWithDep` dirty-marks the `/blueprints` resolved-output key + the apistage content key → next `/blueprints` recomputes empty. **Zero new invalidation code** — only "make the watch actually healthy + keep it touched."

**Async-register first-read race:** preserved-correct by the existing Gate-6 contract — the first read after register still falls through to the live apiserver (`informer_dispatch.go:355-415`, "an empty slice from an unsynced informer is indistinguishable from a real answer → fall through"), and only *subsequent* reads serve from the now-synced informer. 1a/1b do not change that; they only ensure the informer reaches synced+confirmed instead of latching not-servable.

**Idempotency:** `EnsureResourceType` is singleflight-idempotent (`watcher.go:625`); a scoped `RefreshDiscovery` is idempotent (`servable.go` LoadOrStore semantics).

**No special-cases (`feedback_no_special_cases`):** the change is uniform over every api-step-LISTed GVR — no `compositiondefinitions` literal, no hardcoded group. The discriminant is structural ("a LIST api-step whose GVR has a registered informer").

### Scale / OOM verdict (the load-bearing constraint — `feedback_bounding_mechanism_discipline` + the 1.5.1 boot-OOM)

- **`compositiondefinitions` alone:** ONE cluster-wide CR informer, already registered (count-61). Fix #1 adds zero new informers for it — it only confirms/heals the existing one. **Safe.**
- **The generalization "register an informer for every api-step-LISTed GVR":** this IS the dangerous path. The 1.5.1 boot-OOM (`docs/troubleshoot-boot-oom-composition-fanout-2026-06-23.md`) was an unbounded cold fan-out: `allCompositionResources` GETs ~26 child GVRs/composition with no informer → live fetch; registering a full-Unstructured informer for each of those at boot reintroduces the OOM. **Fix #1 deliberately does NOT do this:**
  1. 1a/1b only act on the **LIST** branch (`name==""`). The api-step **GET-by-name** path (the child-resource fan-out) is untouched — no informer is forced for GET-by-name GVRs. (The boot-OOM fan-out is GET-by-name of `.status.managed` children; it stays apistage-content-served, never informer-backed.)
  2. Registration stays **lazy** (only a GVR actually LISTed by an api-step ever registers) and **idempotent** — no boot populate burst; the apistage content cache (the #57-fold root-cause fix) still absorbs the cold fan-out.
  3. The set of *cluster-wide-LISTed* GVRs is tiny and bounded by the RESTAction corpus (blueprints, compositions catalog, RBAC-typed) — NOT proportional to 50K composition instances. The 50K objects are GET-by-name children, which 1a/1b never touch.
- **Bounded-event proof:** after fix, the bounded event ("register/confirm an informer for a LISTed GVR") happens **once per distinct cluster-LISTed GVR**, cost-proportional to the LIST corpus (~tens), not to object count. At 50K this is unchanged from today's already-registered LIST informers.

### Fix #2 (FALLBACK if #1's confirm/heal proves insufficient at scale or the watch genuinely cannot be kept healthy) — bounded self-healing TTL for not-servable-at-Put entries

Per `feedback_no_park_broken_behind_flag`, #2 is a *bound on staleness*, not a flag-off of the bug — it is the correct-but-slower path, justified only if #1's watch cannot be made reliable for a given GVR.

**Mechanism:** at the Put site for a resolved-output / apistage-content entry, if `cache.Global().IsServable(gvr) == false` at Put time, stamp the entry with a **short bounded TTL** (`CATALOG_UNSERVABLE_TTL_SECONDS`, default e.g. 30 s) instead of the 3600 s global TTL. When the GVR has no live watch to invalidate it, the entry self-heals within the short window; when the watch IS healthy (servable), the entry keeps the normal TTL and is invalidated by DELETE (fix #1's path).

- **Bounded:** worst-case staleness = the short TTL, not 3600 s. Cost-proportional: only entries served while not-servable pay the extra revalidation; servable-GVR entries are unaffected.
- **Toggleable / removable** (`project_caching_is_provisional`): one env knob; default-on but tunable; setting it to the global TTL disables the behaviour.
- **No special-cases:** the discriminant is `IsServable(gvr)` at Put time — uniform, no GVR literal.
- **Prior art:** the existing per-entry TTL field on `ResolvedEntry` + `RESOLVED_CACHE_TTL_SECONDS` (`internal/cache/resolved.go:51,88`) already support per-entry expiry; this reuses it.

LOC: ~25. Target: the resolved-output Put site (`internal/handlers/dispatchers/restactions.go` post-resolve Put) + the apistage Put (`apistage.go:518-540`) + a TTL knob in `internal/cache/resolved.go`.

**Recommendation:** ship **Fix #1 (1a+1b)** first — it makes the symptom disappear via the existing DELETE path with ~25 LOC and no new staleness window. Keep **Fix #2 as defense-in-depth** behind its knob (default short TTL for `not-servable`-at-Put catalog entries) so that *any* future GVR whose watch can't be vouched still self-heals in seconds, never 1 h. This is a strategic choice (depth vs. minimalism) — flagged for team-lead/Diego; my recommendation is **#1 now, #2 as a small fast-follow**, because the boot-OOM history makes "rely solely on a perfectly-healthy watch for every catalog GVR" a fragile single point.

---

## 7. Falsifier (kind / in-process preferred — NEVER `go test ./internal/rbac/...` against remote kubeconfig, `feedback_no_go_test_against_remote_kubeconfig`)

**In-process / kind (pre-ship gate, `feedback_falsifier_first_before_ship`):**
1. Register a CR informer for a test GVR via `EnsureResourceType`; wait for `HasSynced` + drive a confirm pass so `IsServable==true`.
2. Resolve a RESTAction whose single api-step is a cluster-LIST of that GVR (records the cluster-list dep edge under the resolved-output key); assert the resolved-output entry is present.
3. Fire a DELETE through the informer (fake clientset delete or `DeleteFunc` directly with a real object).
4. **Assert:** `DepWatchStats`/`OnDelete` fires AND `cache_event.consumed type=DELETE gvr=<test>` is emitted AND the dependent resolved-output L1 key is dirty-marked/evicted (check `store.Get` + dirtyMark counter). Negative control: with the GVR `not-servable` at Put (fix #2 path), assert the entry's TTL ≤ the short bound.
5. **Pre/post for fix #1:** without 1a/1b, a content-HIT-served LIST followed by DELETE leaves the resolved-output entry live (reproduces); with 1a/1b the informer is confirmed and the DELETE evicts.

**On-cluster confirmation (the canonical symptom-disappear probe):**
- `kubectl delete compositiondefinition <X>` → `/blueprints` reflects the deletion **within seconds** (not 1 h), AND the snowplow log emits `cache_event.consumed type=DELETE gvr=core.krateo.io/v1alpha1, Resource=compositiondefinitions`.
- expvar regression guard: `…compositiondefinitions\|informer-fallthrough-not-servable` stops climbing post-boot, and `informer_dispatch.summary list_served` for the GVR is non-zero.
- OOM guard (proves the generalization stayed bounded): boot `HeapSys` peak well under the limit, no restart; `…services/deployments/clusters\|informer-fallthrough-not-servable` (the GET-by-name children) is UNCHANGED (proves 1a/1b did NOT force child informers).

---

## 8. Coverage of the brief's trace questions

- **Q1 (never-registered vs registered-but-never-synced vs deliberately-excluded):** **registered-but-not-servable.** TRACED: the informer IS in `rw.informers` (the count-61 `snowplow_plurals_registered_gvrs` set = `rw.RegisteredGVRs()`, `registered_gvrs_expvar.go:28` / `phase1.go:412`). `EnsureResourceType` IS called on the LIST content-miss path (`apistage.go:517 → dispatchViaInformer → informer_dispatch.go:359`), but post-boot the LIST is served from the apistage content HIT and never re-touches the informer. The latch is conjunct 3/4 of `servableLocked` (`not-servable`, not `not-synced`).
- **Q2 (servability predicate + which condition fails):** `servableLocked` (`watcher.go:1822`) = `registered ∧ HasSynced ∧ ¬watchBroken ∧ confirmed`. `compositiondefinitions` fails conjunct **3 (watchBroken)** or **4 (¬confirmed)** — TRACED as `not-servable` (vs `not-synced`); the exact one needs the live-exec probe in §7. Conjunct 4 is the most likely latch given `core.krateo.io` is the group whose generated child CRDs churn through the `crd_discovery_side_effect.go` discovery/relist machinery (`servable.go:271` un-confirms on a discovery miss).
- **Q3 (what distinguishes an informer-backed GVR from an api-step-only one):** boot-seed-7 + widget `resourcesRefs` GVRs are warmed via paths that reach `HasSynced`+confirm before serving; an api-step-only LIST GVR like `compositiondefinitions` is registered lazily on a content-miss, then **shielded by the apistage content cache** so it is never re-touched to push it past `not-servable`. The generalization (RCA §10 blast radius): every GVR read ONLY via a RESTAction api-step LIST (and never via a watch-invalidated widget `resourcesRefs` read) shares this gap — fix #1's 1a/1b closes it uniformly.

---

## 9. Out of scope (per brief)
Implementation (design only). The portal-side `blueprints-list` RESTAction. The CDC per-call-extras un-cacheability (separate, PARKED — `docs/proactive-cr-warm-design-2026-06-20.md`).
