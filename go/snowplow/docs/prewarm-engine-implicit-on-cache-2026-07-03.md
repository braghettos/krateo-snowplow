# Fold the prewarm flag FAMILY implicit-on-cache — design

Date: 2026-07-03
Author: cache architect
Frozen ref: `19e7f1c` — tip of `feat/prewarm-boot-race-tolerant-shapeA`
  ("feat(prewarm): boot-race-tolerant self-healing Phase-1 prewarm (shape A)")
Sibling of: shape A (same branch). Ships as one "prewarm boot correctness + single-flag" change.
Status: design only — NOT implemented. Arch→PM re-gate ready. Dev builds after PM.

REVISED SCOPE (coordinator-relayed as Diego-confirmed 2026-07-03 — NOTE: I have not seen Diego's own
confirmation of the expansion; PM/Diego must confirm the family scope before dev). The original
ENGINE-only fold was **incomplete** (proven §0): the engine needs THREE default-off flags on. This doc
folds the whole prewarm flag family to implicit-on-cache, includes PROACTIVE_RA_SEED, and resolves the
SEED_FOOTPRINT_BUDGET_BYTES redundancy.

---

## 0. Why the ENGINE-only fold was incomplete (TRACED @ 19e7f1c)

The engine gate at `phase1_walk.go:411` is `if PrewarmEngineEnabled() && navHarvester != nil`. Tracing the
gate chain up:

- **TRACED** `phase1_walk.go:377`: `navHarvester` (required at :411) is built only when
  `cache.PrewarmEnabled() && PrewarmContentEnabled() && PrewarmPIPEnabled()`.
- **TRACED** `phase1_walk.go:355`: the content `harvester` is built only when
  `cache.PrewarmEnabled() && PrewarmContentEnabled()`.
- **TRACED** `phase1_content_prewarm.go:111-113`: `PrewarmContentEnabled()` = `env "PREWARM_CONTENT_ENABLED"=="true"`, default `""` ⇒ **OFF**.
- **TRACED** `phase1_pip_seed.go:164-167`: `PrewarmPIPEnabled()` = `env "PREWARM_PIP_ENABLED"=="true"`, default `"false"` ⇒ **OFF**.
- **TRACED** `prewarm_engine.go:81-83`: `PrewarmEngineEnabled()` = `env "PREWARM_ENGINE_ENABLED"=="true"`, default `"false"` ⇒ **OFF**.

So on a cache-only cluster (installer-test: `CACHE_ENABLED=true`, none of the three set), `navHarvester`
is nil ⇒ engine gate false ⇒ **engine inert**, and worse, `pipSeed` is also nil (`phase1_walk.go:376-382`)
so `seedFn` is nil — **no seed at all**, cold first-nav. Folding only `PREWARM_ENGINE_ENABLED` would flip
:411's first conjunct true but leave `navHarvester` nil, so the engine still never runs. **All three
flags must fold together** for the engine to run under cache-on.

---

## 1. THE FAMILY FOLD (per-flag, TRACED)

### 1.1 Prior-art pattern to mirror (#57)

- **TRACED** `internal/cache/phase1.go:74-76`: `func PrewarmEnabled() bool { return !Disabled() }` — the
  canonical fold; helper stops reading its env var, returns the nearest master gate.
- **TRACED** `internal/cache/cache.go:39-46`: `Disabled()` = the single `CACHE_ENABLED` read.
- **TRACED** `internal/cache/resolved.go:530`: `ApistageL1Enabled()` folds to `== ResolvedCacheEnabled()`
  (fold under the *nearest* master gate, not always CACHE_ENABLED directly).
- **TRACED** `internal/cache/retired_flags.go:64-77`: the retired-flag audit slice; the fold-family test
  enumerates it via `RetiredFlagNamesForTest()` (`:156-162`).

Invariant: the helper returns the nearest existing master gate; the env name is registered in
`retiredFlags`.

### 1.2 Per-flag fold

| Flag | Gate fn (file:line @19e7f1c) | Fold to | Rationale |
|---|---|---|---|
| `PREWARM_CONTENT_ENABLED` | `PrewarmContentEnabled` — `phase1_content_prewarm.go:111-113` | `return cache.PrewarmEnabled()` | content pass is prewarm; same master gate as PIP/engine |
| `PREWARM_PIP_ENABLED` | `PrewarmPIPEnabled` — `phase1_pip_seed.go:164-167` | `return cache.PrewarmEnabled()` | PIP seed is prewarm |
| `PREWARM_ENGINE_ENABLED` | `PrewarmEngineEnabled` — `prewarm_engine.go:81-83` | `return cache.PrewarmEnabled()` | engine is the seed strategy for prewarm |
| `PROACTIVE_RA_SEED_ENABLED` | `ProactiveRASeedEnabled` — `prewarm_engine.go:102-104` | `return cache.PrewarmEnabled()` | §2 |

**Recommendation: gate all four on `cache.PrewarmEnabled()`, NOT a raw `CACHE_ENABLED` read.**
`PrewarmEnabled()` IS `!Disabled()` today (`phase1.go:74-76`) so behavior is identical, but routing
through the prewarm master gate keeps the family bound to *prewarm* — reads as intent and follows any
future prewarm/cache split (the `ApistageL1Enabled()==ResolvedCacheEnabled()` discipline). All four
helpers already live in a package that imports `internal/cache` (`phase1_walk.go` calls
`cache.PrewarmEnabled()` at :355/:377), so no import cycle.

**Resulting gate chain (TRACED, becomes all-cache-driven):**
- content harvester (`phase1_walk.go:355`): `PrewarmEnabled() && PrewarmEnabled()` → `PrewarmEnabled()`.
- `navHarvester` (`phase1_walk.go:377`): `PrewarmEnabled() && PrewarmEnabled() && PrewarmEnabled()` → `PrewarmEnabled()`.
- engine (`phase1_walk.go:411`): `PrewarmEnabled() && navHarvester!=nil` → true whenever cache-on ⇒ **engine runs when CACHE_ENABLED.**

Const cleanup: only the *enable* gate folds. `prewarmContentMaxBytes`/`prewarmPageLimit`
(`phase1_content_prewarm.go:117/125`) read their own SEPARATE envs (`PREWARM_CONTENT_MAX_BYTES`,
`PREWARM_PAGE_LIMIT`) — those are NOT part of this fold; keep them and the `env` import. Delete
`envPrewarmPIPEnabled` (`phase1_pip_seed.go:119`) and `envPrewarmEngineEnabled` (`prewarm_engine.go:76`)
if unreferenced after the fold (grep at dev time).

### 1.3 retired_flags.go registration (append 4 entries, `retired_flags.go:64-77`)

```go
{name: "PREWARM_CONTENT_ENABLED",   behavior: "the content-prewarm pass is now implicit-on-cache; set CACHE_ENABLED=false to disable"},
{name: "PREWARM_PIP_ENABLED",       behavior: "the PIP seed is now implicit-on-cache; set CACHE_ENABLED=false to disable"},
{name: "PREWARM_ENGINE_ENABLED",    behavior: "the prewarm engine is now implicit-on-cache; set CACHE_ENABLED=false to disable"},
{name: "PROACTIVE_RA_SEED_ENABLED", behavior: "proactive composition-detail RA seeding is now implicit-on-cache; set CACHE_ENABLED=false to disable"},
```

Severity split (`retired_flags.go:107-128`) is automatic: `=false` ⇒ **Warn** (silent behavior change —
the installer-test posture, now loud), else ⇒ Info. `RetiredFlagNamesForTest` (`:156-162`) enumerates the
slice so all four are audit-covered with no audit-code change.

---

## 2. PROACTIVE_RA_SEED_ENABLED — fold verdict: YES, safe

### 2.1 What it widens (TRACED)

- **TRACED** `prewarm_engine.go:85-98`: when ON, the engine boot seed **UNIONS**
  `cache.RBACReachableRestActionRefs` into the nav-walk harvester snapshot, so the per-composition
  click-through DETAIL RESTActions (never reached by the nav walk) get warmed at boot. This is task #1
  Option A — it widens **WHICH refs the seed loop iterates**, never the per-request authz boundary
  (F-6: served content unchanged; the flag only changes what is pre-warmed, not what any user can see).
- Engine-only: the legacy `runPIPSeed` path does NOT consult it (`prewarm_engine.go:97`) — moot post-fold
  (that path is deleted, §4). Diego's intent (per coordinator): zero-cold detail-page navigation.

### 2.2 Why folding it on is safe given the seed bound (TRACED — CORRECTED per PM)

**Correction (PM adversarial trace, binding):** the memory that guards the widened seed is the
**SEED-UNIT adaptive bound (§3, this ship's new mechanism)**, NOT the nested-resolve bound. The dominant
seed allocation — `seedRAFullListForWidget`'s unpaginated full-list (§3.1) — is a plain-LIST resolve that
NEVER enters `enterNestedResolveUnit` (Gate 4 rejects it). Widening the proactive ref set widens the
number of such full-list seed units, and each is admitted against LIVE headroom by the seed-unit adaptive
bound and SERIALIZES under pressure — more refs ⇒ more serialization, never more simultaneous footprint.

- **TRACED** the engine seed ctx is marked `WithBackgroundResolve` (`prewarm_engine_boot.go:238`, shape A
  D3, already committed @19e7f1c) ⇒ seed units yield to waiting customers and never fail a customer.
- The seed's incidental nested-CR resolves (`resolve:true` refs) still hit `enterNestedResolveUnit`
  (depth 0); the two bounds cover DISJOINT allocations (§3.3 double-count trace) and compose safely.

**Verdict: fold it on.** Seed-SOURCE widener; memory bounded by the §3 seed-unit adaptive bound; its F-6
falsifier already asserts content-neutrality. PM-gate item: confirm detail-RA warm is wanted by default
(enlarges boot seed work — bounded, non-zero). Product call, not safety.

---

## 3. SEED BOUND — make it ADAPTIVE zero-knob (Diego Option B; SUPERSEDES the R1 retire)

**R1 (retire-as-redundant) is WITHDRAWN. Its premise was false, PM-proven.** The seed bound must be
KEPT and made adaptive (zero-knob), not deleted.

### 3.1 The gap R1 missed (PM adversarial trace — INDEPENDENTLY RE-VERIFIED here, TRACED @19e7f1c)

The adaptive `enterNestedResolveUnit` is armed ONLY at `resolve_inprocess.go:164`, which sits AFTER four
gates. Gate 4 (`resolve_inprocess.go:120-132`) **rejects plain LISTs and non-CR paths**:
- **TRACED** `resolve_inprocess.go:126-127`: `gvr, ns, name, parseOK := ParseAPIServerPathToDep(call.Path); if !parseOK || name == "" { return nil, false, nil }` — a LIST yields `name==""` ⇒ rejected before the bound arm.
- **TRACED** `resolve_inprocess.go:129-131`: only `restactions`/`widgets` GVR paths proceed; a business-GVR data path (e.g. `compositions.krateo.io`) is rejected.
- **TRACED** the mechanism is the **nested-CR-substitution seam only** (`resolve_inprocess.go:1-32`
  header: "an api-step's `path` points DIRECTLY at the apiserver path of a snowplow RESTAction or Widget
  CR … `resolve: true` … in-process"). It is NOT armed on a RA's own data-stage LIST.

The **dominant seed allocation** bypasses it entirely:
- **TRACED** `seedOneWidget:1230` calls `seedRAFullListForWidget` (`phase1_pip_seed.go:1250`), which
  (`:1281`) calls `apiref.Resolve` → `raFullListServe` (`widgets/apiref/ra_full_list.go:65`) which
  **resolves the RA UNPAGINATED** (`ra_full_list.go:176` "Resolve UNPAGINATED -> full F") via
  `restactions.Resolve` (`widgets/apiref/resolve.go:105`). That RA's own stages are plain data LISTs
  against business GVRs — depth-0, `name==""` / non-CR ⇒ **Gate 4 rejects them ⇒ they NEVER enter
  `enterNestedResolveUnit`.** This unpaginated full-list envelope IS the #23 multi-hundred-MB unit
  (`seed_bound.go:5-8`).
- **TRACED** the code's OWN hazard doc confirms disjointness: `prewarm_engine_boot.go:200-206` — the seed
  errgroup "BYPASSES the (a) process-wide weighted memory budget (which is on the CUSTOMER resolve entry,
  not the legacy seed loop)". The two bounds cover DISJOINT allocations.

**Conclusion (corrected): the adaptive nested bound does NOT cover the seed's big allocation.**
`enterSeedUnit` (`SEED_FOOTPRINT_BUDGET_BYTES`) is the ONLY thing that bounds `seedRAFullListForWidget`
today — and it defaults to 0 (DISABLED, `seed_bound.go:88-90,198-199`). So on a cache-on cluster with the
seed flags folded ON (this ship), the seed's dominant allocation is UNBOUNDED unless we act. We make the
seed bound adaptive.

### 3.2 Design — the SEED-UNIT adaptive bound

**Placement (unchanged from today):** the existing `enterSeedUnit` brackets — `seedOneRestaction`
(`phase1_pip_seed.go:983`) and `seedOneWidget` (`:1149`), AFTER the identity short-circuit, so the
customer /call path is untouched (`feedback_bounding_mechanism_discipline`). `seedOneWidget`'s bracket
covers both `widgets.Resolve` (`:1191`) AND `seedRAFullListForWidget` (`:1230`) — the whole widget seed
unit including the unpaginated full-list. Keep these two brackets; swap only the admission mechanism
inside `enterSeedUnit`.

**Admission mechanism (replace fixed budget with the 1.5.28 headroom calc):** inside `enterSeedUnit`
(`seed_bound.go:196`), replace the `seedBudgetBytes()`/`SEED_FOOTPRINT_BUDGET_BYTES`
`semaphore.Weighted` with the SAME adaptive headroom calc as `nested_resolve_bound.go`:
- `headroom = GOMEMLIMIT(debug.SetMemoryLimit(-1)) − liveHeap(runtime/metrics /memory/classes/heap/objects:bytes)`
- `ceiling = headroom − reserve`, `reserve = GOMEMLIMIT / 8` (the documented 1/8 fraction,
  `nested_resolve_bound.go:80-81,193`).
- Transparent when GOMEMLIMIT is unset (`math.MaxInt64`) — byte-identical to no bound.

**DRY — factor a shared primitive (TRACED feasibility):** the 1.5.28 calc lives UNEXPORTED in package
`api` (`nested_resolve_bound.go:97 prodRuntimeMemoryLimit`, `:105 prodLiveHeapBytes`,
`:188 admissionCeiling`). `seed_bound.go` is package `dispatchers` and already imports `internal/cache`
(`seed_bound.go:47`). RECOMMEND: factor the pure headroom calc into `internal/cache` (a new
`cache.AdmissionCeiling() (ceiling int64, unlimited bool)` + the two samplers with test seams) — BOTH
`api`'s `admissionCeiling` and the new seed bound call it. Both packages already import `cache`, no
cycle. This is the mirror-existing / DRY move (`feedback_check_k8s_clientgo_prior_art`). If factoring
proves noisy, the fallback is to duplicate the ~15-line calc in `dispatchers` — but the shared primitive
is preferred so the reserve-fraction + sampler are defined once.

### 3.3 Admit/serialize semantics + the double-count resolution (the subtle correctness point, TRACED)

**Serialize-not-503 (the seed is background):** the customer nested bound returns an honest-503-class
error on ctx-exhaustion (`nested_resolve_bound.go:22-24`). The seed must NEVER fail a customer and has no
browser deadline, so it SERIALIZES / parks:
- Admit the (N+1)th SEED unit iff `inFlightWeight + estUnit <= ceiling`; else PARK (block, ctx-bounded by
  the seed's own ctx — the boot budget / `pipCohortTimeout`), re-check on release. On ctx
  cancel/deadline return the ctx error (the seed unit is abandoned best-effort, log-only — matching
  today's `enterSeedUnit` ctx-cancel return at `seed_bound.go:203-207` and `seedRAFullListForWidget`'s
  non-fatal posture `:1244`). NEVER a hard failure, NEVER a customer-visible error.
- **inFlightCount==0 ⇒ admit unconditionally** (anti-deadlock / guaranteed-progress — mirror
  `nested_resolve_bound.go:211,271`): a lone oversized seed unit runs ALONE rather than parking forever.

**Composition with `seedScopeYielding`'s `customerInFlight()` yield — no double-park, no deadlock
(TRACED):** the engine already yields the WHOLE seed loop to customers at `engineYieldCheckpoint`
(`prewarm_engine.go`, between units) via `customerInFlight()`. These are two DIFFERENT axes and compose:
- `customerInFlight()` yield is BETWEEN seed units (coarse, "pause the loop while any customer is live").
- the seed-unit adaptive admission is a memory gate AT each unit entry (fine, "is there headroom for THIS
  unit's footprint").
- No double-park: the yield checkpoint is checked before `seedOneWidget`/`seedOneRestaction` is called;
  the admission gate is inside it. A unit that passes the yield checkpoint then parks on memory does NOT
  re-enter the yield loop (the checkpoint already returned). No deadlock: the admission park is
  ctx-bounded and has the inFlightCount==0 escape; the yield loop is bounded by the engine's own budget.
  They are strictly nested (yield outer, admission inner) — no lock-order inversion (the same
  outer→inner discipline the old `enterSeedUnit` header claimed vs the errgroup, `seed_bound.go:192-195`).

**DOUBLE-COUNT (the load-bearing trace):** a widget seed unit does BOTH a plain-LIST full-list resolve
(bounded ONLY by the new seed-unit bound) AND may trigger `resolve:true` nested-CR resolves (bounded ONLY
by `enterNestedResolveUnit` at depth 0). Question: does the same tree's weight get counted by BOTH gates?
- **No, they count DISJOINT allocations (TRACED §3.1):** the full-list LIST resolve never enters
  `enterNestedResolveUnit` (Gate 4 rejects LISTs). The nested-CR resolve is a single-CR GET that DOES
  enter `enterNestedResolveUnit` but is NOT the full-list allocation. So each byte is accounted by at most
  one gate — the two weights are on different allocations, not the same tree double-counted.
- **Where they STACK (and it's conservative-safe):** within one `seedOneWidget` unit, the seed-unit gate
  holds `estUnit_seed` for the whole unit's duration, and if that unit's `widgets.Resolve` internally
  triggers a depth-0 nested-CR resolve, the nested gate ALSO holds `estUnit_nested` concurrently. The two
  admissions are held SIMULTANEOUSLY against the SAME `GOMEMLIMIT − liveHeap` headroom (if the shared
  primitive §3.2 is used, both read the same live-heap denominator). This is **over-reservation, which is
  conservative-safe** (it admits LESS aggregate, never more) — the exact safe direction
  (`feedback_capacity_caps_empirical_per_entry_cost`, the 1.5.1 lesson: over-reserve safe, under-reserve
  is the failure mode). **Deadlock-free:** each gate independently has the inFlightCount==0 unconditional
  escape, so even if both gates are "full" a lone stacked unit still runs. No gate waits on the other; they
  are independent semaphores with independent progress guarantees.
- **INFERRED (verify at dev-time -race test):** the stacking is on the SAME goroutine (the serial engine
  seed calls `seedOneWidget` which synchronously calls `widgets.Resolve` which synchronously triggers the
  nested resolve). A single goroutine holding two independent admissions in strict nest order (seed-unit
  acquired first, nested acquired inside) cannot self-deadlock. Confirm with the §6 -race test that a unit
  which triggers a nested resolve completes (does not hang) under a tight injected headroom.

### 3.4 estUnit without the fallback env (zero-knob, mirror 1.5.28, TRACED)

The nested bound estimates per-tree weight by CALIBRATION, not an env: `currentNestedEstUnit`
(`nested_resolve_bound.go:156-165`) returns a calibrated value once available, else the code-constant
`defaultNestedEstUnitBytes = 256 MiB` (`:72`); `calibrateNestedEstUnit` (`:171-181`) records the FIRST
tree's MEASURED live-heap delta one-shot (`:311` on release). **Mirror this for the seed unit:**
- per-seed-unit weight = calibrated-from-first-unit's measured live-heap delta, else a documented
  code-constant fallback (256 MiB, conservative-high — the same rationale as `seed_bound.go:64-67`'s
  `defaultSeedEstUnitBytesFallback`, now a CODE CONSTANT not an env).
- clamp to the ceiling so a single unit larger than the whole headroom runs alone rather than parking
  forever (mirror `nested_resolve_bound.go` / the existing `currentEstUnit` clamp at `seed_bound.go:117-131`).

`seed_bound.go` already HAS a calibration harness (`calibrateEstUnit`, `currentEstUnit`,
`estUnitCalibrated`, `seed_bound.go:113-148`) — it measured HeapInuse deltas. Reuse the calibration
skeleton; swap the DENOMINATOR from HeapInuse to the runtime/metrics live-heap sampler (the 1.5.28 choice
— cheap, no stop-the-world, `nested_resolve_bound.go:101-117`), and swap the admission from the
fixed-budget semaphore to the recompute-per-admission headroom check.

### 3.5 What is DELETED (zero-knob — RECOMMEND remove, not retire-to-flags)

- **`SEED_FOOTPRINT_BUDGET_BYTES`** env + `seedBudgetBytes()` (`seed_bound.go:52-57,88-90`) — DELETE.
- **`SEED_EST_UNIT_BYTES_FALLBACK`** env + `seedEstUnitFallback()` (`seed_bound.go:59-67,93-95`) — DELETE;
  the fallback becomes a code constant (§3.4).
- The `semaphore.Weighted` machinery (`seedBound()`, `seedBoundSem/Budget/Built`, `seed_bound.go:70-111`)
  — REPLACE with the adaptive cond/headroom gate (mirror `aggregateBound`, `nested_resolve_bound.go:125-324`).
- **RECOMMEND: just remove the two envs (do NOT add to `retiredFlags`).** Rationale: `retiredFlags`
  documents flags FOLDED into CACHE_ENABLED (a behavior an operator might have set). These two are
  capacity-tuning byte budgets that defaulted OFF/inert; there is no silent-behavior-change to warn about
  (the adaptive bound is strictly MORE protective than the default-0 no-op they replace). Adding them to
  `retiredFlags` would mis-signal "you asked for X, now you get Y" when the truth is "an inert tuning knob
  is gone, and the path is now always protected." If PM prefers belt-and-suspenders, adding them as Info
  entries is harmless — flag for PM ruling.
- Keep the diagnostic `AssertSeedUnitFootprint` per-unit oversize log (`seed_bound.go:219`) — cheap
  observability, re-home it into the adaptive release path.

**PM-gate item:** confirm Option B (adaptive) + the remove-vs-retire choice for the two envs. Safety
falsifier = §6 seed-unit RED arm + the §5 50K run with NO seed env set holding peak RSS < 8Gi + 0
restarts (adaptive seed bound alone contains `seedRAFullListForWidget`).

---

## 4. DEAD runPIPSeed PATH + orphaned-falsifier MIGRATION

### 4.1 Deadness (TRACED, both prod + test builds)

Post-fold `phase1_walk.go:377`'s conjunction collapses to `PrewarmEnabled()` and `:411`'s
`PrewarmEngineEnabled()` collapses to `PrewarmEnabled()`. So `navHarvester != nil` ⇒ engine gate true ⇒
`seedFn = engineSeed` (`phase1_walk.go:483-486`). The `pipSeed`/`runPIPSeed` branch (`:379-380`) is
reached only when `navHarvester != nil && !PrewarmEngineEnabled()` — **unsatisfiable**.

**Dead subgraph** (every OTHER symbol in phase1_pip_seed.go is ALSO called by `prewarm_engine_boot.go`, so
NOT dead):

| Symbol | file:line | Dead? |
|---|---|---|
| `runPIPSeed` | phase1_pip_seed.go:421 | YES (only caller `phase1_walk.go:380`) |
| `seedCohort` | phase1_pip_seed.go:685 | YES (only via `seedCohortFn` :529,:611) |
| `seedCohortFn` (var) | phase1_pip_seed.go:645 | YES |
| `enumerateAggregatePrewarmTargets` | phase1_pip_seed.go:351 | YES (only via `Fn` @ :441) |
| `enumerateAggregatePrewarmTargetsFn` (var) | phase1_pip_seed.go:405 | YES |

**Shared — do NOT delete** (engine reuse): `seedOneRestaction` (:937→`prewarm_engine_boot.go:507`),
`seedOneWidget` (:1094→:388), `seedRAFullListForWidget` (:1250), `withCohortSeedContext` (:903→:478),
`cohortLogLabel` (:1318→:507), `navWidgetHarvester`/`navWidgetEntry`/`seedTarget` types.

Also-dead: `prewarm_engine_boot.go:212-222` (`if !PrewarmEngineEnabled()` hazard Warn — unreachable
post-fold). Update `seed_bound.go:7-12` + `phase1_pip_seed.go:679-684` comments (the "lever stands"
framing is now false).

**Test-build deadness:** the two orphaned falsifiers (§4.3) MUST migrate/delete in the SAME ship or the
package won't compile after the prod delete — this is the §6 compile-level falsifier (both prod + test).

### 4.2 Delete-now (unchanged, still binding)

Delete the dead subgraph this ship. `project_caching_is_provisional` NOT violated: engine-vs-legacy is an
internal seed-strategy choice, not a cache LAYER; removability seam is `CACHE_ENABLED=false` (§5),
preserved. Leaving ~400-500 LOC of a documented OOM-hazard path reachable-by-nothing is a re-wire hazard.

### 4.3 Orphaned-falsifier MIGRATION partition (TRACED — confirms + refines D-CONDITION-2)

**(a) `TestRunPIPSeed_DiscriminatesDenyVsOperational` (`phase1_seed_classify_test.go:156`) →
DELETE-WITH-MIGRATION.** It injects `enumerateAggregatePrewarmTargetsFn` + `seedCohortFn` (dead symbols,
`:171-203`) to assert #158 deny-vs-operational + bounded-retry. **Coverage-equivalent exists on the engine
path**: `classifyEngineSeedErr` (`prewarm_engine_boot.go:410`) + the `reEnqueued` latch (`:409,434-435`)
are exercised by `prewarm_engine_seed_latch_test.go` — **5 tests (verified)**:
`TestSeedScopeYielding_OneOperationalFailure_EnqueuesExactlyOnce` (:228),
`_NOperationalFailures_LatchEnqueuesExactlyOnce` (:278), `_RBACDeny_NoEnqueue_BumpsDenyCounter` (:309),
`_CounterSumInvariant` (:363), `_TwoInvocations_QueueCoalescesToOnePending` (:448). Same assertions
(deny-counter bump, operational retry-exactly-once, coalesce) on the surviving path. Delete the runPIPSeed
test; coverage is not lost. Confirm at diff-review the latch suite names the deny counter + operational
retry (it does).

**(b) `TestSeedCohort_CtxCancelEmitsAbortLog` (`phase1_pip_seed_test.go:148`) → RE-POINT + ADD EMIT.** It
asserts the 0.30.191 Fix-C `phase1.cohort.abort` reporter fires on ctx-cancel. **The engine has NO analog
(TRACED):** `seedScopeYielding`'s ctx-cancel paths (`prewarm_engine_boot.go:472,491,521-522,538-539`) just
`return ctx.Err()` — no greppable `phase1.cohort.abort`-equivalent emit. So this is a coverage GAP, not a
duplicate. Two-step:
  1. **Dev item (small — flag to PM):** add a ctx-cancel abort-log emit to `seedScopeYielding` mirroring
     the Fix-C reporter (greppable line + phase/cause/targets-processed/elapsed fields) so the
     post-deploy-grep observability survives on the engine path.
  2. Re-point the test to drive `seedScopeYielding` with a pre-cancelled ctx and assert the new engine
     abort line (replacing the `seedCohort` call at `phase1_pip_seed_test.go:181`).

I confirm the coordinator's partition and refine (b) into "add engine emit + re-point" — a bare re-point
would fail (no emit to assert).

### 4.4 Full test-update list (grep-confirmed @19e7f1c)

- `proactive_ra_seed_falsifier_test.go:197-208`: `ProactiveRASeedEnabled()` now defaults to
  `PrewarmEnabled()`, so the `:198-200` "defaults OFF" assertion INVERTS — assert ON under
  `CACHE_ENABLED=true`, OFF under cache-off. Replace `t.Setenv(envPrewarmEngineEnabled,"true")` (`:203`)
  with `t.Setenv("CACHE_ENABLED","true")` + assert-true, add cache-off assert-false (mirror
  `resolved_test.go:349-368`). Rewrite comments `:193-206`.
- `phase1_seed_classify_test.go`: delete `TestRunPIPSeed_DiscriminatesDenyVsOperational` (§4.3a) + any
  sibling tests in the file that call `runPIPSeed`/`seedCohortFn`/`enumerateAggregatePrewarmTargetsFn`.
- `phase1_pip_seed_test.go`: re-point `TestSeedCohort_CtxCancelEmitsAbortLog` (§4.3b); audit the file for
  other direct `seedCohort(`/`runPIPSeed(` calls → migrate/delete.
- New family-fold test (mirror `resolved_test.go:TestApistageL1Enabled_FoldedUnderResolvedCache`): assert
  all four helpers ON iff `CACHE_ENABLED` truthy, ignore stale `=false`. This is the installer-test
  regression guard.
- `retired_flags_test.go`: auto-covers the 4 names; add explicit `=false ⇒ Warn` cases.
- **Shape-A boot-race falsifier** (shape A doc §3): "engine-enabled arm" must express engine-on via
  `CACHE_ENABLED=true` with NO retired prewarm flags. RED arm runs a pre-fold binary → keeps old flags.
  Cross-doc dependency — note so both shipped falsifiers agree.
- **Adaptive seed bound (§3):** migrate `seed_bound`'s existing test helpers to the adaptive gate:
  `resetSeedBoundForTest` (`seed_bound.go:238`) → reset the cond/headroom state + calibration + restore
  prod samplers (mirror `resetNestedResolveBoundForTest`, `nested_resolve_bound.go:363`);
  `calibratedEstUnitForTest` stays (now over the live-heap denominator); delete any test that sets
  `SEED_FOOTPRINT_BUDGET_BYTES`/`SEED_EST_UNIT_BYTES_FALLBACK` via env and re-express via injected
  headroom seams (mirror `setRuntimeSeamsForTest`, `nested_resolve_bound.go:379`). Grep
  `seed_bound`/`boundSeedUnit`/`SEED_FOOTPRINT` test refs at dev time.

No other test sets/asserts the four flags (grep-confirmed: only `proactive_ra_seed_falsifier_test.go`
references engine/proactive; content/PIP gate helpers exercised only through the walk).

---

## 5. BACK-OUT, #42, BLAST RADIUS

- **Back-out lever** becomes **`CACHE_ENABLED=false`** (Phase 1 never runs, Phase1Done pre-set true,
  /readyz immediate-200, transparent apiserver fallback: `phase1.go:44-51`,
  `project_cache_off_is_transparent_fallback`). No prod code branches on engine-off-but-cache-on after the
  fold — unreachable (cache-on ⇒ PrewarmEnabled ⇒ all four true). Diff-review: no surviving
  `if PrewarmEngineEnabled()/PrewarmContentEnabled()/PrewarmPIPEnabled()/ProactiveRASeedEnabled()` in prod
  except the folded helpers.
- **#42 50K** = tester ship-VALIDATION, not a design blocker (PM to rule). Scored prod/bench path is
  ALREADY family-on (helm overlay — main-chart-reconciliation; the 2026-06-11 50K PASS ran engine-on). The
  fold moves only the MISCONFIGURED path (cache-on-flags-forgotten, installer-test) from no-seed/cold to
  the validated engine path; adaptive nested bound (§3.1) covers the seed OOM. Falsifier: deploy
  SCALE=50000 with **only `CACHE_ENABLED=true`** (none of the four flags, `SEED_FOOTPRINT_BUDGET_BYTES`
  UNSET) — assert: no `runPIPSeed`/`phase1.seed.*` legacy log lines; engine boot scope ran; peak RSS <
  8Gi across lifecycle; 0 restarts; warm-nav CONTENT correct (`l1_hit:"hit"`, non-empty rows,
  compositions + composition-DETAIL panels warm ⇒ proves PROACTIVE_RA_SEED folded on); convergence in the
  2026-06-11 envelope (warm ~911 / cold ~2053). One run validates the family fold AND the SEED-bound
  retirement (§3.3).
- **BLAST RADIUS: code-only. No CRD change. No schema change.**
  - Behavioral delta (intended fix): cache-on-flags-forgotten deployments (installer-test) flip from
    no-seed/cold-first-nav to the bounded engine seed + boot self-heal (shape A). **That IS the fix.**
  - Already family-on (prod, bench): **byte-identical** — same engine path; gates derive from
    `CACHE_ENABLED`. Stale `=true` ⇒ ignored Info no-op; stale `=false` ⇒ ignored **Warn** (loud,
    correct).
  - Cache-off: unchanged.
  - **Chart cleanup (chart-side, NOT this repo — `feedback_chart_release_lockstep`):** remove the now-inert
    `PREWARM_CONTENT_ENABLED` / `PREWARM_PIP_ENABLED` / `PREWARM_ENGINE_ENABLED` /
    `PROACTIVE_RA_SEED_ENABLED` and `SEED_FOOTPRINT_BUDGET_BYTES` / `SEED_EST_UNIT_BYTES_FALLBACK`
    (deleted §3.5) from the snowplow-chart values overlay. Snowplow tag + chart tag ship together.
  - Ships with shape A as one change: shape A hardens boot-race self-heal ON the engine path; this fold
    guarantees the engine path is the only path under cache-on. Complementary.

---

## 6. Falsifier (this fold specifically)

Unit-level (all four helpers): each returns **true** with `CACHE_ENABLED=true` and its own env UNSET (the
installer-test scenario — the assertion that would have caught the defect); **false** with
`CACHE_ENABLED=false`; **true** with `CACHE_ENABLED=true` AND its own env `=false` (retired value ignored
— proves the fold, not the old read). `AuditRetiredFlags` emits **Warn** for each `=false`. Compile-level:
after deleting `runPIPSeed`/`seedCohort`/`enumerateAggregatePrewarmTargets*` AND migrating the two
falsifiers, BOTH prod and test builds compile with no `//nolint:unused` — proving the subgraph was fully
dead in both build tags (§4.1). Integration: the §5 50K run.

**Seed-unit adaptive-bound RED arm (the dev's new falsifier — §3):** drive the seed path with M
oversized seed units (M > 1, each `seedRAFullListForWidget`-shaped: an unpaginated full-list ≥ the
injected per-unit weight) under an INJECTED tight headroom (`setRuntimeSeamsForTest`-style seams so the
gate admits ~1 unit at a time). Assert:
- **GREEN (fixed):** peak concurrent seed footprint stays ≤ ceiling (the M units SERIALIZE, never all
  in-flight at once); all M units eventually complete (no drop, no OOM); the seed yields to an injected
  `customerInFlight()` (never fails a customer); a unit that triggers a nested-CR resolve completes (no
  self-deadlock across the stacked seed-unit + nested gates — §3.3 INFERRED, verify with `-race`).
- **RED (proves discrimination):** with the adaptive gate disabled (or against the default-0 fixed budget
  = today's inert state), the M oversized units admit concurrently → peak footprint = M × unit >> ceiling
  (the OOM that would fire at 50K). The RED arm must show the unbounded peak so the test isn't vacuously
  green (`feedback_falsifier_shape_must_discriminate`: M > 1 required, degenerate M=1 masks it).
- Also assert zero-knob: the gate engages with NO `SEED_FOOTPRINT_BUDGET_BYTES` / `SEED_EST_UNIT_BYTES_FALLBACK`
  set (both deleted) — driven purely by the injected GOMEMLIMIT/live-heap seams.

---

## Appendix — key file:line index (@19e7f1c)

- Gate chain: `phase1_walk.go:355` (content harvester), `:377` (navHarvester), `:411` (engine),
  `:376-382` (pipSeed dead branch), `:483-486` (seedFn selection).
- Gate fns: `phase1_content_prewarm.go:111-113` (PrewarmContentEnabled);
  `phase1_pip_seed.go:164-167` (PrewarmPIPEnabled); `prewarm_engine.go:81-83` (PrewarmEngineEnabled),
  `:102-104` (ProactiveRASeedEnabled).
- Fold prior art: `phase1.go:74-76` (PrewarmEnabled=!Disabled); `cache.go:39-46` (Disabled);
  `resolved.go:530` (ApistageL1Enabled).
- Retired flags: `retired_flags.go:64-77` (slice), `:107-128` (severity), `:156-162` (names-for-test).
- Adaptive nested bound (does NOT cover the seed full-list — §3.1): `nested_resolve_bound.go:188-324`
  (admissionCeiling + enterNestedResolveUnit + aggregateBound), `:80-81,193` (reserve = GOMEMLIMIT/8),
  `:97,105` (GOMEMLIMIT/live-heap samplers), `:156-181,311` (estUnit calibration one-shot),
  `:363,379` (test seams); invoked `resolve_inprocess.go:164-172` (AFTER Gate 4 `:120-132` which rejects
  LISTs/non-CR). engine seed marks bg `prewarm_engine_boot.go:238`.
- Seed's dominant unbounded-by-nested allocation (§3.1): `phase1_pip_seed.go:1230` (seedOneWidget →
  seedRAFullListForWidget), `:1250-1307` (seedRAFullListForWidget), `:1281` (apiref.Resolve),
  `widgets/apiref/resolve.go:105` (restactions.Resolve), `widgets/apiref/ra_full_list.go:65,176`
  (raFullListServe UNPAGINATED); disjointness confirmed `prewarm_engine_boot.go:200-206`.
- Seed bound to make adaptive (§3): brackets `phase1_pip_seed.go:983` (seedOneRestaction), `:1149`
  (seedOneWidget); `seed_bound.go:52-68` (envs to DELETE), `:88-95` (budget/fallback readers to DELETE),
  `:100-111` (semaphore to REPLACE), `:113-148` (calibration skeleton to REUSE), `:196-222` (enterSeedUnit
  bracket to swap), `:219` (per-unit assert to keep), `:238` (resetSeedBoundForTest to migrate).
- Dead subgraph: `phase1_pip_seed.go:351/405` (enumerate*), `:421` (runPIPSeed), `:645/685` (seedCohort*);
  hazard branch `prewarm_engine_boot.go:212-222`.
- Engine coverage-equivalents: `prewarm_engine_boot.go:410` (classifyEngineSeedErr), `:409,434-435`
  (reEnqueued latch); `prewarm_engine_seed_latch_test.go:228,278,309,363,448` (5 latch tests).
- Orphaned falsifiers: `phase1_seed_classify_test.go:156` (delete-migrate),
  `phase1_pip_seed_test.go:148` (re-point + add engine emit); `proactive_ra_seed_falsifier_test.go:197-208`.
- Seed ctx (NOT bg, legacy — being deleted): `phase1_pip_seed.go:900-919` (withCohortSeedContext).
