# Boot-race-tolerant, self-healing Phase-1 prewarm — design

Date: 2026-07-03
Author: cache architect
Frozen ref: `49a3b8e` ("feat(readyz): gate readiness on prewarm-complete (1.5.29)")
Runtime artifact: krateo-installer-test 2026-07-03, pod `snowplow-6f946d8797-rt8rv` (snowplow 1.5.29 / chart 1.0.55)
Status: design only — NOT implemented. Arch→PM-gate ready.

---

## 0. Symptom (given, runtime-proven)

Fresh install, three components booting concurrently (snowplow, authn, frontend chart). Phase-1 prewarm
is one-shot at boot with no retry, so it loses a race against BOTH external inputs and warms nothing;
the pod still reaches Ready (backstop) and serves the first navigation COLD.

Two distinct one-shot reads, each 5–9 s ahead of its dependency:

- **seed→authn token**, 06:08:53: `dial tcp …:8082 connect: connection refused`,
  `WARN prewarm.seed_loopback_token_unavailable`. authn Ready at 06:08:58 (5 s later). One failure, no
  later success → token-less seed → nested authenticated-loopback `/call` RAs (#57) never warmed for the
  pod's lifetime.
- **config-vars ConfigMap read**, 06:09:38: `not found`, `WARN cache: Phase 1 startup warmup incomplete`.
  ConfigMap created 06:09:47 (9 s later) → nav roots (INIT / ROUTES_LOADER) empty → nav L1 never warmed.

Both deps healthy now; pure boot race + no-retry. Does not reproduce on rolling-test (deps already warm).

---

## 1. Code trace (TRACED unless labelled)

### 1.1 Phase-1 walker entry + the config-vars ConfigMap read

- **TRACED** — `main.go:750-761`: `Phase1Warmup` runs in a goroutine bounded by
  `p1Ctx = context.WithTimeout(cacheCtx, phase1Timeout)` where `phase1Timeout = env.Int("PHASE1_TIMEOUT_SECONDS", 900)s`
  (`main.go:749`). This is the single Phase-1 budget for BOTH the walk and everything downstream.
- **TRACED** — `internal/handlers/dispatchers/phase1_walk.go:623` (`phase1WarmupWith` Step 3):
  `roots, listErr := lister(ctx)` — a **single call**. On `listErr` (Step 3 error branch, lines 624-635)
  it logs `phase1.warmup.roots_list_failed`, runs the sync barrier over whatever the meta-seeds
  registered, calls `cache.MarkPhase1Done()`, and returns. **No retry, no re-read.**
- **TRACED** — `phase1_roots.go:125-231` `listNavigationRootsFromConfigMap` → `readFrontendConfig`
  (`phase1_roots.go:235-257`): `dynCli.Resource(configMapGVR).Namespace(namespace).Get(...)` — a
  **one-shot direct dynamic GET**, not informer-backed. Namespace = `authnNS`, name =
  `FRONTEND_CONFIG_CONFIGMAP` (default `frontend-config-vars`, `phase1_roots.go:66-71`). A `not found`
  returns an error up to Step 3, which takes the no-retry error branch above. This is the 06:09:38 read.
- **INFERRED→now TRACED (no ConfigMap informer exists)** — grep over `internal/cache/*.go` for a
  configmaps informer: the only `configmaps` references are wire-shape helpers in
  `fallthrough_meter.go:69/108/123` and `plurals_resolver.go:216`. `MetaQuerySeeds()`
  (`internal/cache/phase1.go:251-259`) is **exactly 7 GVRs** (routesloaders, navmenus, restactions +
  4 RBAC) — configmaps is NOT among them and no `EnsureResourceType(configMapGVR)` is called anywhere in
  production. **There is no L3 watch we can trigger off today.** (The cache DOES expose a
  GVR-*discovered* hook — `RegisterGVRDiscoveredHook`, `internal/cache/gvr_discovered_hook.go:63` — but
  it fires on informer registration of *navigated* GVRs, not on ConfigMap object appearance.)

### 1.2 The existing post-sync re-walk (the half-built boot-race fix)

- **TRACED** — `phase1_walk.go:409-476` (`engineSeed`) + `prewarm_engine_boot.go:182-238`
  (`rePrewarmBoot`): when `PrewarmEngineEnabled()` (prod =true, `#341`), Step 7.6 routes through the
  engine, which **re-lists roots via `deps.lister(rctx)` (`prewarm_engine_boot.go:230`) and re-walks**
  with a fresh walker per root. So a *second* config-vars read already exists.
- **TRACED — the gap**: this re-walk fires from exactly **one** `enqueueScope(prewarmScope{kind: scopeKindBoot})`
  (`phase1_walk.go:456`), and `engineSeed` blocks on `<-bootDone` (line 470), which the `scopeDone`
  callback closes the instant that single boot scope finishes (`phase1_walk.go:445-450`). If config-vars
  is still absent at re-walk time, `rePrewarmBoot` hits its own `roots_list_failed` branch
  (`prewarm_engine_boot.go:231-238`), returns `listErr`, closes `bootDone`, and **the engine never
  re-attempts** — no timer, no ConfigMap trigger. The worker stays alive (bound to `cacheCtx`) but only
  processes `scopeKindGVRDiscovered` enqueues, which fire on *navigated-GVR* informer registration —
  never on config-vars appearance. So the re-walk is a one-shot too.

### 1.3 seed→authn token acquisition (#57 loopback token), fire-once-no-retry

- **TRACED** — `main.go:185-186`: `authnClient := authn.New(*urlAuthn, *saTokenPath)`;
  `dispatchers.SetSeedLoopbackTokenProvider(authnClient.Token)`. The provider is
  `authnClient.Token` — the `$URL_AUTHN/serviceaccount/login` exchange lives inside `authn.Client.Token`.
- **TRACED** — `phase1_walk.go:1032-1052` `installSeedLoopbackToken`: calls `fn(ctx)` **exactly once**.
  On `err != nil || tok == ""` it bumps `seedLoopbackTokenErrTotal` and emits
  `WARN prewarm.seed_loopback_token_unavailable` (the 06:08:53 line), then **returns ctx unchanged
  (token-less)**. No retry, no backoff. This runs inside `withPhase1SAContext` (`phase1_walk.go:997`),
  which is called once per root at `resolveNavigationRoot` (`phase1_walk.go:1055`) and once per re-walk
  root (`prewarm_engine_boot.go:229`). So the token is (re-)fetched per walk pass — meaning a retry that
  re-runs the walk *automatically re-attempts the token exchange*, which is architecturally convenient.

### 1.4 MarkPhase1Done / IsPhase1Done / the 1.5.29 readiness interaction

- **TRACED** — `internal/cache/phase1.go:103-113`: `MarkPhase1Done()` is `phase1Done.Store(true)` +
  `markPhase1DoneObserved()` (a `CompareAndSwap`-once for the expvar boundary, lines 96-106 doc).
  **Idempotent** — safe to call repeatedly; the boundary timestamp tracks the FIRST flip.
  `IsPhase1Done()` reads the atomic; `/readyz` (`internal/handlers/readyz.go:51`) returns 200 iff true.
- **TRACED — 1.5.29 = the readiness gate + the backstop** (HEAD `49a3b8e`). Two flip paths:
  1. `phase1_walk.go:743-780` Step 7.6/8 — the PIP seed runs **synchronously before**
     `MarkPhase1Done`, via a `defer cache.MarkPhase1Done()` (line 747) placed BEFORE the seed call. This
     is the "not-ready-until-prewarm-complete" gate (project_readyz_gates_on_prewarm_complete). The seed
     is bounded by `seedCtx = context.WithTimeout(ctx, pipGlobalTimeout)` (line 762; `pipGlobalTimeout =
     8min`, `phase1_pip_seed.go:158`). The `defer` fires on normal return, error, timeout, OR panic-recover
     → **Ready-degraded backstop**: a stuck dep still flips Ready.
  2. `main.go:847-853` — `ShouldFlipPhase1DoneOnStartup` safety-net for cache-off / nil-watcher only.
- **TRACED — the outer backstop** is `PHASE1_TIMEOUT_SECONDS` (900 s) on `p1Ctx` (`main.go:751`): if the
  whole Phase-1 goroutine overruns, `p1Ctx` cancels, the seed's `seedCtx` (child of it) cancels, and the
  Step 7.6 defer flips Ready-degraded. So there are **two nested backstops**: `pipGlobalTimeout` (8 min,
  seed) inside `PHASE1_TIMEOUT_SECONDS` (900 s = 15 min, whole phase).
- **Re-run interaction**: `MarkPhase1Done` being idempotent means a re-run cannot "double-mark" harmfully.
  BUT the current flip is `defer`-scheduled at Step 7.6 and fires as soon as the *first* seed pass
  returns. **Any self-healing re-run must therefore run either (a) before that defer fires (inside the
  Step 7.6 budget) or (b) after Ready, as a background top-up that does NOT touch the flip.** See §2.4.

### 1.5 Idempotency / OOM / customer-yield substrate (for the re-run design)

- **TRACED** — engine queue is idempotent by key: `prewarmScope.key()` returns `"boot"` for all boot
  scopes (`prewarm_engine.go:206-211`); `enqueueScope` coalesces same-key
  (`prewarm_engine.go:273-282`). Re-enqueuing `scopeKindBoot` N times collapses to one pending item.
- **TRACED** — engine worker is bound to the **process-lifetime `cacheCtx`** via
  `SetEngineProcessContext` (`main.go:458`, read at `phase1_walk.go:443`), NOT to `p1Ctx`. So the worker
  **survives past MarkPhase1Done and past `PHASE1_TIMEOUT_SECONDS`** and can process post-Ready enqueues.
  This is the load-bearing property that makes a background self-heal possible without new goroutine
  plumbing.
- **TRACED** — the seed is memory-bounded at the shared primitive: `enterSeedUnit`
  (`seed_bound.go:196`, `seedOneRestaction` at `phase1_pip_seed.go:983`) — the 1.5.28-lineage
  `SEED_FOOTPRINT_BUDGET_BYTES` aggregate bound. Re-runs funnel through the same primitive, so a re-run
  cannot exceed the aggregate footprint.
- **TRACED** — customer yield: `engineYieldCheckpoint` (`prewarm_engine.go:478`) between cohorts uses the
  `customerInFlight()` predicate — a re-run on the engine worker yields to live customer `/call`.
- **INFERRED (readable-but-not-traced-to-a-single-line, low-risk)** — the seed path does NOT set
  `cache.WithBackgroundResolve`; the only production caller is `resolve_populate.go:367` (refresher).
  The engine seed's customer-priority discipline is the yield checkpoint, not the aggregate-fan-out
  admission gate. This is called out as a PM-gate item (§2.4, decision D3): the re-run should be marked
  `WithBackgroundResolve` so it participates in the C5 cold-fan-out admission gate and yields memory to
  customer resolves — a 1-line addition on the re-run ctx.

---

## 2. Design

### 2.0 Prior-art check (opens the design — feedback_check_k8s_clientgo_prior_art)

- **client-go `cache.WaitForCacheSync` / SharedInformerFactory**: solves "block until an informer synced",
  which snowplow already uses (`WaitAllInformersSynced`). It does NOT solve "a config *document* I read
  once-off wasn't there yet" — the config-vars read is a direct dynamic GET, not an informer, precisely
  because it is a bootstrap read that decides *which* informers to create.
- **client-go informer for ConfigMaps**: the idiomatic k8s answer to "react when object X appears" is an
  informer with an AddFunc handler. This is directly applicable and is the recommended shape below —
  register a *namespaced, single-object* ConfigMap informer and re-drive the walk from its AddFunc,
  rather than a hand-rolled poll loop. client-go's `cache.NewInformer` / a filtered
  `SharedInformerFactory.Core().V1().ConfigMaps()` with a `fields.OneTermEqualSelector("metadata.name", …)`
  is the prior art.
- **authn token retry**: `k8s.io/client-go/transport` and `wait.Backoff` / `wait.ExponentialBackoffWithContext`
  are the prior art for bounded retry on a transient dependency. Reuse `k8s.io/apimachinery/pkg/util/wait`
  rather than a hand-rolled ticker.

### 2.1 Root of the fix — invert the trigger: config-vars DRIVES prewarm start

Design intent (Diego, endorsed 2026-07-03): *"snowplow can be deployed before the frontend, but it
won't START the prewarm until the frontend has created the config-vars ConfigMap."*

The primary fix is a **trigger inversion**, not a retry patch. Today the Phase-1 walk starts eagerly at
boot and reads config-vars once; if the ConfigMap isn't there yet it gives up. The fix makes the
**appearance of the config-vars ConfigMap the event that DRIVES the prewarm walk** — prewarm is
gated-on / driven-by that ConfigMap existing, event-driven, with **no eager one-shot-then-give-up**.
When snowplow boots before the frontend, it does not fail — it simply has not started prewarm yet;
it serves cold from the informer substrate (Ready-degraded via the existing backstop) and begins warming
the instant the ConfigMap lands, regardless of how much later that is.

This is enabled by two properties already in the tree: the walk/harvest/seed path is already reusable
via the engine's `scopeKindBoot` (`rePrewarmBoot`, `prewarm_engine_boot.go:182-238`), and the engine
worker **already outlives boot** (bound to the process-lifetime `cacheCtx`, `main.go:458`). So driving
prewarm from a ConfigMap event costs only the trigger + the independent token-readiness axis below —
**no new warming logic.**

The two axes are independent and must NOT be conflated:
- **config-vars presence** decides *whether prewarm can start at all* — the nav roots are unknowable
  without config.json's INIT/ROUTES_LOADER entry points.
- **authn reachability on `:8082`** decides *whether the seed acquires the #57 loopback token*.
  Config-vars being present does NOT imply authn is serving — a fresh install can create config-vars
  while authn is still coming up. So the token axis (§2.3) is a separate bounded retry on its own
  readiness clock, never folded into the ConfigMap trigger.

### 2.2 Config-vars: the ConfigMap-appearance event DRIVES prewarm — PRIMARY design (shape A)

Register a **namespaced single-object ConfigMap informer** for
(`authnNS`, `FRONTEND_CONFIG_CONFIGMAP`) at Phase-1 start on `cacheCtx`. Its **AddFunc** (config-vars
appeared) and **UpdateFunc** (config.json changed — e.g. the frontend rewrote its INIT/ROUTES_LOADER
entry points) **enqueue `scopeKindBoot`** on the existing engine. `rePrewarmBoot` then reads config-vars
(now present), walks the nav roots, harvests, builds the BindingsByGVR index, and seeds — the whole path
already exists. Idempotent by the `"boot"` dedup key (`prewarm_engine.go:206`); the process-lifetime
worker (`cacheCtx`) processes it whether the ConfigMap arrives before OR after the readiness backstop.

**The eager one-shot read is subordinated, not duplicated, and never terminal.** Step 3's synchronous
`lister(ctx)` (`phase1_walk.go:623`) is retained ONLY as a "config-vars already present at boot" fast
path — when the ConfigMap exists at boot, the informer's initial-list AddFunc fires with the object
already there and the eager read + the event-driven enqueue converge on the same idempotent `"boot"`
scope (no double work). When the ConfigMap is absent at boot, the eager read finds nothing and takes
**no error-terminal action**: the informer AddFunc is the authority that STARTS prewarm when the object
lands. Implementation note: Step 3's current `roots_list_failed → WaitAllInformersSynced →
MarkPhase1Done → return` terminal branch (`phase1_walk.go:624-635`) must be softened to "log + proceed,
leaving the config-vars informer to drive the walk when the ConfigMap appears" — it must no longer be the
sole/terminal reader that gives up. (The readiness backstop still flips Ready-degraded on its own budget;
see §2.4 — the softening only removes the *give-up-on-warming* semantics, not the backstop.)

- **Bound / self-adapt**: the informer lives on `cacheCtx` (process lifetime) — no new magic-number env.
  It fires exactly when the object appears, then quiesces (steady state = 0 extra walks). The walk it
  drives is bounded by the engine's own per-cohort `pipCohortTimeout` + `engineYieldCheckpoint`.
- **Why not trigger off an existing informer**: there is none for configmaps (§1.1). We add the smallest
  possible one — single object, single namespace, field-selected — not a cluster-wide ConfigMap watch.
- **feedback_no_magic_env / feedback_prewarm_walk_no_sampling_caps**: the trigger is a k8s event, not a
  poll interval; no new knob.

**Fallback shape A′ (only if the single-object informer proves undesirable — e.g. the SA cannot `watch
configmaps`, see §4):** a ctx-bounded `wait.ExponentialBackoffWithContext(ctx, backoff, …)` retry inside
the roots read, `ctx` = the existing `p1Ctx` (`PHASE1_TIMEOUT_SECONDS`). No new timeout — it consumes the
otherwise-idle Phase-1 budget. Retained only as fallback: it re-introduces polling, blocks the walk
goroutine, and — critically — is NOT post-backstop-capable (it cannot start prewarm for a ConfigMap that
lands after `PHASE1_TIMEOUT_SECONDS`), so it does not fully deliver the any-boot-order tolerance §2.6
states. The informer is the design; A′ is the RBAC-constrained degrade only.

### 2.6 Boot-order tolerance (the customer-facing property this delivers)

Because config-vars presence DRIVES prewarm start (§2.2) and the token exchange retries on its own
readiness clock (§2.3), snowplow becomes tolerant to **ANY** boot order of {snowplow, authn, frontend}:

| Boot order | Behavior |
|---|---|
| all warm before snowplow (rolling-test / steady state) | eager read succeeds; one boot scope; behavior unchanged |
| snowplow before frontend | snowplow boots, serves cold (Ready-degraded backstop), has simply not started prewarm; the config-vars AddFunc STARTS prewarm the instant the frontend creates the ConfigMap — even long after the backstop, no pod restart |
| snowplow before authn (the rt8rv token race) | prewarm runs when config-vars present; the token backoff acquires the #57 loopback JWT once authn serves on `:8082`; nested-loopback RAs warm without restart |
| snowplow before both | the two axes converge independently as each dep appears; warms once both are up |

**Consequence for the installer:** installer-side ordering of {snowplow, authn, frontend} becomes
**optional and harmless** rather than required. A fresh install that brings all three up concurrently
(the rt8rv scenario) self-heals; an install that happens to order them correctly costs nothing extra.
The correctness of prewarm no longer depends on deploy sequencing — snowplow can legitimately be deployed
first and will wait, event-driven, for the frontend's ConfigMap and for authn's `:8082`.

### 2.3 seed→authn token: bounded retry until authn reachable — RECOMMENDED

Replace the fire-once `installSeedLoopbackToken` (`phase1_walk.go:1032-1052`) `fn(ctx)` with a bounded
retry via `wait.ExponentialBackoffWithContext(ctx, backoff, func() (bool, error){ … })` that:
- returns `(true, nil)` on a non-empty token (install it),
- returns `(false, nil)` on a transient/connection error (retry),
- is bounded by the **caller's ctx** — the walk/seed ctx, itself a child of `p1Ctx`
  (`PHASE1_TIMEOUT_SECONDS`) / `seedCtx` (`pipGlobalTimeout`). **No new env var**: the retry cannot
  outlive the phase budget.

`backoff` should be derived, not magic: start small (e.g. `Duration: 250ms`, `Factor: 2`, `Jitter: 0.1`,
`Steps` large enough to fill the budget) — these are *shape* parameters of an exponential backoff, not a
policy timeout; the true bound is ctx. On budget exhaustion it keeps the existing degrade posture
(WARN + `seedLoopbackTokenErrTotal` + token-less) — never fatal.

**Interaction with the §2.2 re-drive**: because `installSeedLoopbackToken` runs inside
`withPhase1SAContext` on **every** walk pass (boot AND every `rePrewarmBoot`), a config-vars-triggered
re-drive *also* re-attempts the token. So even the pure backoff is belt-and-suspenders: if authn is slow
but config-vars is slower, the ConfigMap AddFunc re-drive will re-run the token exchange when authn is
already up. Both mechanisms converge on "warm once the dep is up."

### 2.4 Idempotency, OOM, background-resolve, readiness reconciliation

- **Idempotency**: re-enqueue coalesces on `"boot"` key (`prewarm_engine.go:206-282`). A burst of
  ConfigMap events (Add then Update then Update) produces at most one pending boot scope. The re-walk
  uses a fresh visited-set per root (`prewarm_engine_boot.go:256`) so it re-descends correctly; harvest
  is first-write-wins deduped (`phase1_pip_seed.go:246-252`). No duplicate-warming storm.
- **OOM**: re-run funnels through `enterSeedUnit` / `SEED_FOOTPRINT_BUDGET_BYTES` (1.5.28 aggregate
  bound) — the aggregate is bounded at the shared primitive regardless of how many re-runs fire.
- **WithBackgroundResolve (decision D3)**: mark the re-drive ctx `cache.WithBackgroundResolve` so it
  yields to live customer traffic via the C5 admission gate, complementing `engineYieldCheckpoint`. 1
  line on the re-run ctx builder.
- **Readiness reconciliation (decision D1 — needs PM ruling; see §5)**. Two sub-cases:
  - *Config-vars/authn arrive DURING the Phase-1 budget* (the common fresh-install case: 5–9 s late,
    budgets are 8 min / 15 min): the AddFunc re-drive + token backoff complete BEFORE the Step 7.6
    `defer MarkPhase1Done` fires, so readiness naturally gates on a *successful* prewarm — no gate change
    needed. This already satisfies project_readyz_gates_on_prewarm_complete.
  - *A dep is stuck past the budget*: the existing `pipGlobalTimeout`/`PHASE1_TIMEOUT_SECONDS` backstop
    flips Ready-degraded (correct — a permanently-missing dep must not wedge the pod), and the
    **post-Ready** ConfigMap AddFunc re-drive (on the process-lifetime worker) still self-heals nav L1
    the moment the dep finally appears, with zero pod restart. This is the key new property: **the
    self-heal is not confined to the boot budget** — the informer keeps the trigger live for the
    process lifetime.

  Net: readiness stays "gate on prewarm-complete, but backstop after the budget," and the self-heal
  layer makes a post-backstop late arrival converge anyway. No change to the flip logic itself is
  required — the recommended design achieves the desired reconciliation purely by adding the trigger.

### 2.5 Files touched (design targets, LOC bounds)

- `internal/handlers/dispatchers/phase1_walk.go` — (a) soften Step 3's `roots_list_failed` terminal
  branch (`:624-635`) so a boot-time absent ConfigMap is "log + proceed, informer will drive" rather than
  "give up warming" (§2.2), ~5-10 LOC; (b) `installSeedLoopbackToken` gains a
  `wait.ExponentialBackoffWithContext` wrapper (§2.3), ~15-25 LOC; (c) add the re-run ctx
  `WithBackgroundResolve` mark (§2.4 D3), ~1 LOC.
- New: `internal/handlers/dispatchers/phase1_configvars_watch.go` (or fold into `phase1_roots.go`) — the
  single-object ConfigMap informer + AddFunc/UpdateFunc that `enqueueScope(scopeKindBoot)`; started from
  `Phase1Warmup` on `cacheCtx`. ~60-90 LOC incl. the field-selector factory + started-once guard.
- `main.go` — start the config-vars informer on `cacheCtx` (or pass `cacheCtx` into `Phase1Warmup` so it
  can start it). ~5-10 LOC. (Note: `Phase1Warmup` currently takes `p1Ctx` which is bounded; the informer
  must be on the process-lifetime `cacheCtx`, so either thread `cacheCtx` in or start the informer at the
  `main.go` call-site alongside `SetEngineProcessContext`.)
- No change to `rePrewarmBoot`, the engine queue, `MarkPhase1Done`, `readyz.go`, or the seed primitives —
  they are reused as-is.

Total new/changed ≈ 90-130 LOC, additive, behind the existing engine gate.

---

## 3. Falsifier (must exercise the real timing race)

**Kind/on-cluster test — `phase1_boot_race_selfheal` (integration, kind-backed, serialized per
feedback_serialize_kind_test_runs):**

1. Bring up a kind cluster with the widget/RESTAction CRDs + a nav-root fixture (navmenus +
   routesloaders) and RBAC bindings, but **withhold** (a) the `frontend-config-vars` ConfigMap and (b)
   authn (point `URL_AUTHN` at a port with nothing listening, or a paused authn deployment).
2. Start snowplow with cache on / engine on. Assert **at t0**:
   - `WARN prewarm.seed_loopback_token_unavailable` fired (token race lost) AND
   - `WARN cache: Phase 1 startup warmup incomplete` / `roots_list_failed` fired (config-vars race lost),
   - `SeedLoopbackTokenErrTotal() >= 1`,
   - nav L1 is COLD: a `/call` to the INIT widget under a seeded cohort returns `l1_hit:"miss"` and a
     nested-loopback RA is not warm.
   This reproduces the exact 06:08:53 / 06:09:38 failure. **Gate item: assert these WARNs actually fired
   — if they don't, the test didn't exercise the race and is invalid (feedback_falsifier_shape_must_discriminate).**
3. **Then** create the ConfigMap and un-pause authn (mimicking the +5 s / +9 s real arrival).
4. Assert self-heal WITHOUT a pod restart, bounded by the phase budget:
   - engine log shows a `scopeKindBoot` re-enqueue triggered by the ConfigMap AddFunc,
   - `rePrewarmBoot` completes with `roots_discovered > 0` and `bindings_enrolled > 0`,
   - a subsequent `/call` to INIT under the cohort returns `l1_hit:"hit"` with zero resolve (nav L1 warm),
   - the nested-loopback RA (#57) is warm (token acquired on retry — `seedLoopbackTokenErrTotal` stops
     climbing and a later `prewarm.seed` pass has a non-empty token),
   - pod restart count == 0 throughout.
5. **RED arm (proves the test discriminates the fix)**: run the same scenario against HEAD `49a3b8e`
   (pre-fix) — step 4 must FAIL (nav L1 stays `miss`, no re-enqueue) to confirm the falsifier detects the
   defect and isn't vacuously green.

**On-cluster confirmation (diagnostic, not scoring):** on GKE `gke_neon-481711_...`, a fresh
helm-install with the deploy-order deliberately inverted (delete config-vars + scale authn to 0 before
snowplow rollout, then restore) should show the same self-heal in pod logs + a warm first Chrome
navigation. kubectl diagnostic only; not a latency score.

---

## 4. Blast radius / risk

- **Code-only. No CRD change. No chart change expected** — CONFIRMED: the trigger reuses existing
  chart values (`FRONTEND_CONFIG_CONFIGMAP`, `AUTHN_NAMESPACE`, `URL_AUTHN`), existing budgets
  (`PHASE1_TIMEOUT_SECONDS`, `pipGlobalTimeout`), and the existing `PREWARM_ENGINE_ENABLED` gate. No new
  env var (Diego hard rule satisfied).
  - **One RBAC caveat to verify (PM/dev gate item)**: the SA already holds `*/*` get/list/**watch**
    (`phase1_walk.go:50` cites the native ClusterRoleBinding) so a namespaced ConfigMap **watch** is
    already authorized — confirm empirically with `kubectl auth can-i watch configmaps -n <authnNS>
    --as=system:serviceaccount:<ns>:<sa>` before committing. If (unexpectedly) not granted, fall back to
    shape A′ (bounded GET retry, no watch verb) — that is why A′ is retained.
- **Behavioral neutrality when deps are already warm** (rolling-test / steady state): the ConfigMap is
  present at boot → AddFunc fires once at informer sync with the object already there → one boot
  re-enqueue that coalesces with the initial boot scope (idempotent) → no extra work. authn up → token
  acquired first try → backoff loop exits after one iteration. So steady-state behavior is unchanged.
- **Concurrency**: the AddFunc runs on the informer's handler goroutine and only calls the O(1)
  non-blocking `enqueueScope` (`prewarm_engine.go:273-282`) — it must NOT do walk work inline (the
  hook-must-not-block contract, `prewarm_engine_boot.go:105-108`). Design honors this.
- **Regression watch**: the seed backoff must remain strictly ctx-bounded — a backoff that ignores ctx
  would re-introduce a boot-blocking stall (the 0.30.220 lesson). Falsifier step 4's "bounded by phase
  budget" assertion guards this.

---

## 5. Open decision for the PM gate

**Framing settled (Diego, 2026-07-03):** shape A is the PRIMARY design intent — *"snowplow can be
deployed before the frontend, but it won't START the prewarm until the frontend has created the
config-vars ConfigMap."* Prewarm is event-driven off the config-vars ConfigMap appearing (§2.1/§2.2),
delivering any-boot-order tolerance (§2.6). The items below are the remaining gate confirmations.

**D1 (strategic — readiness timing):** The design keeps the current gate-on-prewarm-complete-with-backstop
and adds an *event-driven, post-Ready-capable* prewarm start so a late dep converges without restart. It
does NOT make readiness wait *longer* for a stuck dep (the 8-min/15-min backstops are unchanged). Confirm
this reconciliation of project_readyz_gates_on_prewarm_complete ("not-ready-until-prewarm-complete") vs.
"a stuck dep must still hit the backstop." Recommendation: **yes** — gate on success within the budget,
backstop after it, event-driven start post-Ready. No flip-logic change.

**D2 (RBAC confirmation for shape A):** shape A (single-object ConfigMap informer) is contingent ONLY on
confirming the SA can `watch configmaps` in `authnNS` (it holds `*/*` get/list/watch per
`phase1_walk.go:50` — verify empirically, §4). A′ (ctx-bounded GET retry) is the degrade fallback IF and
ONLY IF that watch verb turns out unavailable; A′ does not deliver post-backstop tolerance, so shape A is
the target.

**D3 (WithBackgroundResolve on the re-drive):** RECOMMEND marking the re-drive ctx
`cache.WithBackgroundResolve` so the self-heal yields memory to customer resolves via the C5 gate (1 LOC).
Confirm no objection.

---

## Appendix — key file:line index

- `main.go:185-186` seed token provider wiring; `:458` SetEngineProcessContext(cacheCtx);
  `:749-761` Phase1Warmup goroutine + PHASE1_TIMEOUT_SECONDS; `:847-853` cache-off safety-net.
- `internal/handlers/dispatchers/phase1_walk.go:300` Phase1Warmup; `:596-839` phase1WarmupWith
  (Step 3 one-shot roots read `:623`, Step 7.6 seed+defer-MarkPhase1Done `:743-780`);
  `:1032-1052` installSeedLoopbackToken (fire-once token); `:997` install site;
  `:409-476` engineSeed (single scopeKindBoot enqueue `:456`, bootDone wait `:470`).
- `internal/handlers/dispatchers/phase1_roots.go:125-231` listNavigationRootsFromConfigMap;
  `:235-257` readFrontendConfig (one-shot dynamic GET).
- `internal/handlers/dispatchers/prewarm_engine_boot.go:182-238` rePrewarmBoot (re-list `:230`,
  roots_list_failed no-retry `:231-238`).
- `internal/handlers/dispatchers/prewarm_engine.go:206-211` scope key (boot coalesces);
  `:273-282` enqueueScope (idempotent, non-blocking); `:478` engineYieldCheckpoint.
- `internal/cache/phase1.go:103-113` MarkPhase1Done/IsPhase1Done (idempotent);
  `:251-259` MetaQuerySeeds (7 GVRs, no configmaps).
- `internal/cache/gvr_discovered_hook.go:63` RegisterGVRDiscoveredHook (navigated-GVR only, not configmaps).
- `internal/handlers/readyz.go:49-65` ReadyCheck (200 iff IsPhase1Done).
- `internal/handlers/dispatchers/seed_bound.go:196` enterSeedUnit (SEED_FOOTPRINT_BUDGET_BYTES aggregate bound).
