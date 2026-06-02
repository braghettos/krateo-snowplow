# Ship 0.30.237 — Stage 1: boot scope parallelism (LOAD-BEARING)

**Architect (peer reviewer)** | **Date**: 2026-06-02 | **Predecessor**: docs/ship-0.30.237-f4-prewarm-engine-trace-2026-06-02.md (HYPOTHESIS, scope-split per PM gate)

> Per `feedback_recurring_regression_pattern` Change A: this is the FRESH peer-reviewed
> design. The original architect's F4 trace is treated as a hypothesis. Findings below
> reflect independent re-trace of every code claim plus structural simplification:
> Stage 1 is reduced to parallelism + F3 retention. Stage 2 (CRUD/RBAC delta triggers)
> is **DEFERRED** to ship 0.30.238 — see companion doc `ship-0.30.238-stage-2-crud-triggers-2026-06-02.md`.

## 1. Symptom — empirical evidence (0.30.236 Gate C failure)

Identical to the predecessor trace. Live cluster on rolled-back rev 409 (= 0.30.235):

```
snowplow_phase1_walk_observations_total: 7,824
snowplow_phase1_bindingset_classes_total: 35       (global cohort count)
snowplow_cohort_memo_entries_total: 10             (only 10/35 cohorts actually populated)
snowplow_phase1_seed_widgets_total: 0              (counter wired only on engine path)
snowplow_phase1_seed_restactions_total: 0          (counter wired only on engine path)
snowplow_refresher_skipped_no_entry_total: 1,287,015   (99% of completions: entry gone)
```

0.30.236 deployed and observed (8-min boot scope):
- `phase1_seed_restactions_total = 327` → 327 puts in 480s = **0.68 puts/s wall-clock** (architect's
  "1.47 puts/s" on the predecessor trace line 142 is unit-inverted; the correct interpretation
  is **1.47s mean per put**, **0.68 puts/s throughput**).
- `phase1_seed_widgets_total = 0` → widget loop never started (predecessor finding confirmed).

## 2. PEER-REVIEW FINDINGS — original F4 design defects

### 2.1 Defect 1 (PM-caught): RBAC-shift hook insertion site does NOT exist

Predecessor §3.4 + §5.1 wires the rbac-shift hook into `bindings_by_gvr_delta.go:onBindingChanged`.
**No such function exists.** Independent re-trace
(`/Users/diegobraga/krateo/snowplow-cache/snowplow/internal/cache/bindings_by_gvr_delta.go:131,151,176,203`):

```
onBindingAdd(obj)                                    line 131
onBindingUpdate(oldObj, newObj)                      line 151
onBindingDelete(obj)                                 line 176
onRoleObjectChanged(obj)                             line 203
```

Four mutator sites; no `onBindingChanged(affected []GVR)`. Additionally,
the "affected GVRs" set used by the predecessor's hook signature is NOT
explicitly produced anywhere — it is derived **implicitly inside**
`enrolLocked` (`bindings_by_gvr.go:301-325`, iterates `idx.navigated`
and checks each GR via `rulesGrantGetList(rules, gr)`) and
**implicitly inside** `unrolLocked` (`bindings_by_gvr.go:332-343`,
touches all `byGVR` buckets without recording which actually contained
the binding). The predecessor design cannot land as written. Stage 2 (separate ship)
addresses the affected-GVR aggregator design properly with file:line citations.

### 2.2 Defect 2: Stage-2 helpers `snapshotForGVR` / `snapshotForTargetGVR` do NOT exist

Predecessor §3.4 references:
```go
widgetEntries := deps.navHarv.snapshotForGVR(s.gvr)
restactionRefs := deps.harvester.snapshotForTargetGVR(ctx, s.gvr)
```

Independent grep — neither exists. The harvester only exposes `snapshot()`
(full set) at `phase1_content_prewarm.go:177` and `phase1_pip_seed.go:209-...`
(navWidgetHarvester analogue). Helper code is design-time fabrication, not
prior art. Stage 2 must spec these as NEW code with explicit signatures.

### 2.3 Defect 3: Worst-case budget math is structurally thin

Predecessor §3.2 line 311: "8× speedup on a 25-min serial workload = 3.1 min".
This rests on the unstated assumption that **total unit count is ~1020**
(25 × 60 × 0.68 = 1020). Empirical evidence:
- Only **327 puts observed in 8 min** — no measurement of the true total.
- Architect estimates "RAs ~1500, widgets ~5000" from `walk_observations: 7824`
  — these are **inferences from a single aggregated counter**, not direct counts.
- At GOMAXPROCS=8 the **observable** parallel throughput at the per-put cost is
  8 / 1.47 = **5.45 puts/s**, giving a 6-min budget of **1962 units**. If the
  true unit count is materially > 1962 (e.g. 6500 harvested entries × ~3 scoped
  cohorts = 19,500 units), Stage 1 still misses Gate C by 6-10×.

PM's "<2× safety margin" flag stands. Stage 1 must include three defensive levers
(§4.5) so margin failure is recoverable without a HARD REVERT.

### 2.4 Findings on the predecessor's code claims that are CORRECT

Confirmed by independent file:line trace:

- **`prewarm_engine.go:254` runs ONE worker** — CONFIRMED. `go e.runWorker(ctx)`
  single goroutine; `runWorker` (line 267-306) is a serial dequeue loop.
- **`prewarm_engine_boot.go:256-338` is serial outer×inner** — CONFIRMED. Lines 257
  (outer RA `for`) and 272 (inner cohort `for`) are both serial; lines 301 / 315
  same for widgets. No goroutines anywhere.
- **`phase1_pip_seed.go:814-824` per-unit = full `restactions.Resolve`** — CONFIRMED.
  Full resolver path including jq, inner /call hops, RBAC. The 1.47s observed cost
  is **plausible** for a compositions-list-like RA at 5K composition scale.
- **`prewarm_engine_boot.go:228-238` global-fallback path** — CONFIRMED. `cohortsFor`
  returns `globalCohorts = EnumerateBindingSetClasses()` (35 on live cluster) for any
  RA where `restActionTargetGVR` returns `(zero, false)`.
- **`deps_watch.go:191-237` widget-CR hook site** — CONFIRMED. AddFunc/UpdateFunc
  /DeleteFunc are clean insertion points. (Stage 2.)

### 2.5 Independent finding the predecessor missed — LEGACY parallelism precedent

The legacy `runPIPSeed` (`phase1_pip_seed.go:379-425`) **already uses parallelism**:

```go
g, gctx := errgroup.WithContext(pctx)
limit := runtime.GOMAXPROCS(0)
g.SetLimit(limit)
for _, c := range cohorts {                  // cohort PARALLEL
    g.Go(func() error {
        return seedCohort(gctx, cohort, restactionRefs, widgetEntries, ...)
    })
}
```

The new engine `seedScopeYielding` (`prewarm_engine_boot.go:217-340`) inverted the loop
shape to `for RA { for cohort { ... } }` and **dropped the parallelism**. This is a
silent regression — the engine path replaced a known-working parallel seed with a
serial one. Stage 1 restores parallelism aligning with the proven legacy shape
(`runPIPSeed` parallelises cohorts) — see §4 design.

This precedent is also what makes Stage 1 LOW RISK: we are returning to a known shape,
not introducing a new one.

## 3. STAGE-1 SCOPE

Per PM mandate to scope-split:
- **THIS SHIP (Stage 1)**: boot scope parallelism + retain F3 Pinned diff (already in HEAD = 0.30.236).
- **DEFERRED to ship 0.30.238 (Stage 2)**: widget-CR + RA-CR + RBAC-shift delta triggers.

Stage 1 fixes Gate C **alone** (boot seed completes within budget, every cohort cell is seeded,
L1 miss% on widgets/RA drops to <1% after boot). Stage 2 is a CRUD-fresh-data optimisation —
without it, an admin who creates a new widget CR pays a cold-fill on the first /call until
the boot scope re-runs (which it doesn't on the same process lifetime). Stage 1 is the
load-bearing customer-correctness fix; Stage 2 is an admin-UX improvement.

## 4. DESIGN

### 4.1 Architectural direction — cohort-parallel, RA/widget-serial (matches legacy)

Restore the legacy `runPIPSeed` parallelism shape: each cohort runs as one goroutine,
and inside a cohort the (RA × widget) sequence is serial. This is the proven shape
from 0.30.179 onwards, validated for 50+ cohorts on prior bench evidence
(`phase1_pip_seed.go:39-50` cite "F2 prewarm" sister parallelism).

**Critical change from predecessor's design**: predecessor proposed (RA × cohort) flat
unit pool parallelisation. That is a NEW shape (no prior art). Cohort-parallel is what
the legacy `runPIPSeed` has shipped 30+ times; it is the established mechanism.

### 4.2 Concrete refactor — `seedScopeYielding` → `seedScopeParallel`

File: `internal/handlers/dispatchers/prewarm_engine_boot.go` (~80 LOC changed).

Replace the serial outer-RA loop + serial widget loop with a cohort-keyed parallel pass:

```go
// New helper:
//   1. Build the full cohort universe up front: union of every per-target-GVR scoped
//      set + the global fallback (for RAs whose target GVR doesn't scope).
//   2. For each cohort, build the list of (RA, widget) seed units that target it.
//   3. Bounded errgroup at GOMAXPROCS — each goroutine drains one cohort's units serially.
//   4. Customer-priority yield BEFORE acquiring a goroutine slot AND between units
//      inside a cohort. Customer arrives -> in-flight cohorts complete on their own
//      120s per-cohort timeout cap; new cohorts wait.
//   5. Per-cohort wrapped in pipCohortTimeout (existing 120s cap) so a stuck cohort
//      cannot wedge the pool.

func seedScopeParallel(ctx context.Context,
    restactionRefs []templatesv1.ObjectReference,
    widgetEntries []navWidgetEntry,
    saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {

    // (1) Build per-cohort work plans.
    plans := buildCohortPlans(ctx, restactionRefs, widgetEntries)  // map[Cohort]cohortPlan

    if len(plans) == 0 {
        return nil
    }

    // (2) Bounded errgroup — GOMAXPROCS, matches phase1_pip_seed.go:383
    //     legacy precedent (cohort-parallel shape).
    g, gctx := errgroup.WithContext(ctx)
    limit := runtime.GOMAXPROCS(0)
    if limit < 1 {
        limit = 1
    }
    g.SetLimit(limit)

    log := slog.Default()

    for c, plan := range plans {
        cohort := c       // pin
        cohortPlan := plan

        // CUSTOMER PRIORITY — yield before scheduling a new cohort goroutine.
        // In-flight goroutines complete on their own pipCohortTimeout cap.
        engineYieldCheckpoint(gctx)
        if gctx.Err() != nil {
            break
        }

        g.Go(func() error {
            // Per-cohort 120s deadline (matches pipCohortTimeout legacy cap).
            cctx, ccancel := context.WithTimeout(gctx, pipCohortTimeout)
            defer ccancel()
            cohortCtx := withCohortSeedContext(cctx, cohort, saEP, saRC)
            label := cohortLogLabel(cohort)

            // Restactions for this cohort (only those whose target-GVR scoping
            // included this cohort — the precise scoping the engine already does).
            for _, ref := range cohortPlan.restactionRefs {
                if cohortCtx.Err() != nil {
                    return nil   // non-fatal — cohort timeout fires, others continue
                }
                engineYieldCheckpoint(cohortCtx)   // yield between RAs within a cohort

                if err := seedOneRestaction(cohortCtx, label, ref, authnNS); err != nil {
                    slog.Warn("prewarm.engine.seed.restaction_skipped", ...)
                    continue
                }
                pipSeedRestactionsTotal.Add(1)
                incCohortCounter(&pipSeedRestactionsByCohort, label)
            }

            // Widgets for this cohort.
            for _, e := range cohortPlan.widgetEntries {
                if cohortCtx.Err() != nil {
                    return nil
                }
                engineYieldCheckpoint(cohortCtx)
                if err := seedOneWidget(cohortCtx, e, authnNS); err != nil {
                    slog.Warn("prewarm.engine.seed.widget_skipped", ...)
                    continue
                }
                pipSeedWidgetsTotal.Add(1)
                incCohortCounter(&pipSeedWidgetsByCohort, label)
            }

            return nil
        })
    }

    if err := g.Wait(); err != nil {
        return err
    }
    return ctx.Err()
}
```

**`buildCohortPlans` — derivation logic** (~40 LOC):

```go
type cohortPlan struct {
    restactionRefs []templatesv1.ObjectReference
    widgetEntries  []navWidgetEntry
}

// buildCohortPlans inverts the (RA -> cohorts) and (widget -> cohorts)
// mappings into (cohort -> [RAs], [widgets]) using EXACTLY the same per-target-GVR
// scoping rules as the legacy seedScopeYielding (cohortsFor on each).
//
// Returns: map keyed by Cohort (compared by reflect-equal-on-Cohort struct via
// canonical hash — Cohort has Username + Groups []string; sort-Groups-then-format
// as map key).
func buildCohortPlans(ctx context.Context,
    restactionRefs []templatesv1.ObjectReference,
    widgetEntries []navWidgetEntry) map[Cohort]*cohortPlan {

    plans := map[Cohort]*cohortPlan{}
    addRA := func(c cache.Cohort, ref templatesv1.ObjectReference) {
        p := plans[c]
        if p == nil { p = &cohortPlan{}; plans[c] = p }
        p.restactionRefs = append(p.restactionRefs, ref)
    }
    addWidget := func(c cache.Cohort, e navWidgetEntry) {
        p := plans[c]
        if p == nil { p = &cohortPlan{}; plans[c] = p }
        p.widgetEntries = append(p.widgetEntries, e)
    }
    globalCohorts := cache.EnumerateBindingSetClasses()
    for _, ref := range restactionRefs {
        targetGVR, haveTarget := restActionTargetGVR(ctx, ref)
        cohorts := globalCohorts
        if haveTarget {
            if rc := cache.EnumerateResourceCohorts(targetGVR); len(rc) > 0 {
                cohorts = rc
            }
        }
        for _, c := range cohorts {
            addRA(c, ref)
        }
    }
    for _, e := range widgetEntries {
        cohorts := globalCohorts
        if rc := cache.EnumerateResourceCohorts(e.GVR); len(rc) > 0 {
            cohorts = rc
        }
        for _, c := range cohorts {
            addWidget(c, e)
        }
    }
    return plans
}
```

### 4.3 F3 retention (NOT "re-apply") — clarification

The predecessor design §5.2 step 1 / §9 instructs "F3 re-apply via `git checkout 0.30.236 -- <5 files>`".
This is **incorrect framing**. The working-tree HEAD is currently AT 0.30.236
(commit `c892de3`, verified). The cluster was rolled back via helm rev 409 but the
git tree was NOT reverted. F3 is **already in the source**. Stage 1 needs only to:

1. Keep the 5 F3 Pinned:true sites unchanged (no action required).
2. Ensure the new `seedScopeParallel` Put path still produces Pinned:true entries —
   the call to `seedOneRestaction` / `seedOneWidget` is unchanged, so the Put inside
   those functions retains `Pinned: true` automatically.

The "git checkout 0.30.236 -- ..." procedure should be **deleted from the dev-execution
plan**. Stage 1 builds from current HEAD (= 0.30.236) and modifies `prewarm_engine_boot.go`
only.

### 4.4 Customer-priority yield invariant

Identical to predecessor design but verified at every call site:
- `engineYieldCheckpoint(gctx)` BEFORE `g.Go(...)` for each cohort.
- `engineYieldCheckpoint(cohortCtx)` between each RA and widget unit within a cohort.

Under a customer burst:
- New cohort goroutines pause at yield checkpoint until burst clears.
- In-flight cohorts run to completion OR until their 120s per-cohort timeout.
- Worst-case in-flight CPU contention = GOMAXPROCS=8 background seed goroutines vs
  customer goroutines. Go scheduler time-slices fairly; refresher precedent
  (`feedback_customer_priority_over_refresher`) accepts this contention.

### 4.5 DEFENSIVE LEVERS — protection against tight-margin worst case

Per PM's "<2× safety margin" finding, Stage 1 ships THREE defensive levers (mechanism-uniform,
no special-cases per `feedback_no_special_cases`, no flag-gated parking per
`feedback_no_park_broken_behind_flag`):

**Lever A: Configmap-tunable global timeout** (changes default).

Today: `pipGlobalTimeout = 8 * time.Minute` const. Stage 1: make it env-configurable
with the **same default** but readable from `PREWARM_BOOT_TIMEOUT_MINUTES` (configmap).
This avoids a recurrence of the 0.30.179 + 0.30.180 hard-coded timeout debates.

```go
// phase1_pip_seed.go (replaces line 157)
var pipGlobalTimeout = func() time.Duration {
    m := env.Int(envPrewarmBootTimeoutMinutes, 8)
    if m < 1 { m = 1 }
    return time.Duration(m) * time.Minute
}()
```

Operator can shift to 12 / 15 min via chart values without a code change. The default
stays 8 min so the falsifier projection is unchanged.

**Lever B: Per-cohort progress checkpoint logging** (observability, no behavior change).

When `seedOneRestaction` / `seedOneWidget` returns, log at every 50-unit cohort progress
point (in addition to the existing per-RA log). Operator can read `kubectl logs` mid-boot
to see exactly how many units have completed per cohort and project the ETA. This makes
"the seed is slow but will complete" visibly distinct from "the seed is stuck".

**Lever C: Stage-1 telemetry — `phase1_seed_units_planned_total`** (new counter).

Add `pipSeedUnitsPlannedTotal expvar.Int` published as `snowplow_phase1_seed_units_planned_total`,
incremented by `buildCohortPlans` to expose the actual unit-count up front. The Gate C
falsifier (§7) reads this metric to verify the math projection. If planned-units >
empirical-throughput × 8min, Stage 1 fails the projection cleanly (logged warning at
boot, gate falsifies, no production damage) and operators can raise Lever A in lockstep.

### 4.6 What Stage 1 does NOT do

- **Does NOT add CRUD/RBAC delta triggers** — deferred to Stage 2 (ship 0.30.238).
- **Does NOT change /readyz** — engine boot still runs in background post-MarkPhase1Done
  (`phase1_walk.go:595-643` unchanged).
- **Does NOT touch refresher** — the 1.29M `skipped_no_entry` will drop naturally once
  seeded cells exist before the refresher tries to refresh them.
- **Does NOT add lazy-fill-cold** — per `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill`.
- **Does NOT change cohort enumeration** — `EnumerateBindingSetClasses` /
  `EnumerateResourceCohorts` semantics unchanged.

### 4.7 Files changed / created

| File | Type | LOC | Purpose |
|---|---|---:|---|
| `internal/handlers/dispatchers/prewarm_engine_boot.go` | Refactor | ~100 | `seedScopeYielding` → `seedScopeParallel` + `buildCohortPlans` helper |
| `internal/handlers/dispatchers/phase1_pip_seed.go` | Edit | ~6 | `pipGlobalTimeout` → env-configurable (Lever A) |
| `internal/handlers/dispatchers/phase1_seed_metrics.go` (NEW or extend existing) | Edit | ~15 | `pipSeedUnitsPlannedTotal` counter (Lever C) |
| `internal/env/keys.go` (or equivalent) | Edit | ~5 | `envPrewarmBootTimeoutMinutes` constant |
| `chart/values.yaml` (lockstep) | Edit | ~5 | Add `prewarm.bootTimeoutMinutes: 8` chart value |
| **TOTAL** | | **~131** | |

(Well under PM's ~200 LOC Stage-1 ceiling.)

## 5. PRE-COMMIT FALSIFIER

Identical substrate to predecessor §6: live cluster rev 409 (= 0.30.235) at
47K widget CRs, 15K RAs, 4989 compositions, 35 cohorts.

**Falsifier A — repro 0.30.236's Gate C failure** (sanity check before fix):

1. With rev 409 pod live, capture `/debug/vars` baseline (already done; saved
   to `/tmp/snowplow-vars-detail.json`).
2. Confirm 5K-scale bench shows `widgets|panels miss > 30%`, `markdowns miss > 30%`,
   `buttons miss > 30%` (matches 0.30.236 Gate C numbers).

**Falsifier B — Stage 1 build PASSES Gate C**:

1. Build 0.30.237 lockstep with chart 0.30.237 (chart value `prewarm.bootTimeoutMinutes: 8`).
2. Deploy. Within 90 seconds of pod-ready, `/debug/vars` shows:
   - `snowplow_phase1_seed_units_planned_total > 0` (Lever C)
3. Within `bootTimeoutMinutes` of pod-ready, observe in pod logs:
   - `prewarm.engine.boot.complete elapsed_ms=N` where N < bootTimeoutMinutes * 60_000
   - NO `prewarm.engine.scope_incomplete` logs since boot
4. Verify `/debug/vars`:
   - `phase1_seed_restactions_total > 1000`
   - `phase1_seed_widgets_total > 5000`
   - `phase1_seed_units_planned_total ≈ restactions_total + widgets_total` (sanity:
     planned should match what actually got done)
5. Run `e2e/bench/snowplow_test.py` at SCALE=5000. Verify each (stage, page) cell:
   - `widgets|markdowns miss% ≤ 1%`
   - `widgets|panels miss% ≤ 1%`
   - `widgets|buttons miss% ≤ 1%`

**Falsifier C — Stage 1 with Lever A applied (if Falsifier B fails on the default 8 min)**:

If Falsifier B step 3 shows the boot exceeded 8 min, do NOT HARD REVERT. Instead:
1. Run `helm upgrade --set prewarm.bootTimeoutMinutes=15` lockstep with chart 0.30.237.
2. Re-run Falsifier B from step 3.

This is the bounded recovery path that protects against the predecessor's "<2× safety
margin" worst case.

## 6. POST-DEPLOY GATE

**Gate C primary** (`feedback_test_scale_50k` + `feedback_l1_hit_invariant_is_100_percent`,
`feedback_validate_content_not_just_status`):

Bench at SCALE=5000, full S1–S8; for every (stage, page) cell:

```
COLD waterfall_ms ≤ 1.05 × WARM waterfall_ms
```

Content check: Compositions page `4989 names match` at every stage.

**Gate C secondary** — `/debug/vars` snapshot:

| Metric | Pre-fix (0.30.236) | Pass criterion |
|---|---:|---|
| `dispatch_l1_lookups widgets|markdowns miss%` | 52% | **≤ 1%** |
| `dispatch_l1_lookups widgets|panels miss%`    | 44% | **≤ 1%** |
| `dispatch_l1_lookups widgets|buttons miss%`   | 53% | **≤ 1%** |
| `phase1_seed_widgets_total`                   | 0   | > 5000 |
| `phase1_seed_restactions_total`               | 327 | > 1000 |
| `phase1_seed_units_planned_total` (NEW)       | n/a | > 6000 |
| `refresher_skipped_no_entry_total` delta over bench | ~3K/min | < 100/min |
| `prewarm.engine.scope_incomplete` log count | several | 0 |
| `resident_demote_total`                       | 0   | 0 (unchanged) |

## 7. RISK REGISTER

| Risk | Likelihood | Severity | Mitigation |
|---|---|---|---|
| GOMAXPROCS-parallel seed steals CPU from customer /call | Med | Med | `engineYieldCheckpoint` before goroutine schedule + between units; mirrors legacy `runPIPSeed` shape proven 30+ ships |
| Worst-case unit count exceeds 8-min budget at parallel-8 | Med | Low (with Lever A) | Lever A (configmap-tunable timeout) + Lever C (planned-units telemetry) make this observable + recoverable without REVERT |
| Per-cohort 120s timeout fires more often under parallelism | Low | Low | Same per-cohort cap as legacy `runPIPSeed`; if a cohort can't complete in 120s the cell is dirty-marked and refresher re-populates — no customer-correctness loss |
| `buildCohortPlans` doubles ctx fetch cost (re-derives target-GVR per RA) | Low | Low | `restActionTargetGVR` calls `objects.Get(ctx, ref)` — already cached at the objects.Get layer; rebuild is O(N) over harvest with one cached fetch per RA, ~negligible |
| Map keyed by Cohort: struct equality on `[]string Groups` | Low | Med | Cohort has `Username string` + `Groups []string`; Go map can't key on slice. Mitigation: build a canonical string key `username + "|" + sortedJoin(groups, ",")` and use `map[string]*cohortPlan`; resolve back via parallel `map[string]Cohort` |
| Cohorts that the index didn't enumerate (sentinel + group-only) hash collision | Low | Low | `EnumerateResourceCohorts` already emits the sentinel pattern at `bindings_by_gvr.go:579`; canonical-key handler must mirror this |
| Recurring regression pattern (5+ visits) | Confirmed | Med | Stage 1 reduces predecessor's 402 LOC → 131 LOC; PEER-REVIEW gate complete (this doc) |
| F3 Pinned diff retention errors | Low | High | F3 is in HEAD; predecessor's "git checkout" procedure is DELETED — no risk of mechanical re-apply error |
| 1.5 GiB resident cap insufficient | Low | Med | Pin-honour at `resolved.go:761-773` degrades to transient on overflow; telemetry `resident_demote_total` flags it |

## 8. RECURRING-REGRESSION FLAG (peer-review status)

Per `feedback_recurring_regression_pattern` Change A — Stage 1 satisfies:

- [x] **Fresh-architect peer review** — THIS DOC, by independent architect.
- [x] **PM-stage gate before commit** — PM is already in-loop; this doc is the
      gate input.
- [x] **Empirical re-trace of every claim** — §2 documents 2 fabrications + 5 confirmed
      claims + 1 found regression precedent the predecessor missed.
- [x] **Scope-reduced from 402 LOC → 131 LOC** — eliminates the 3 highest-risk
      surfaces (parallel seed proven via legacy; CRUD/RBAC deferred).
- [x] **Defensive levers for tight budget** — Lever A (config-tunable timeout),
      Lever B (per-cohort progress), Lever C (planned-units telemetry).

## 9. RESIDUAL REVERT PROBABILITY

Stage 1 risk model (independent peer reviewer assessment):

| Factor | Risk |
|---|---|
| Mechanism novelty | LOW (cohort-parallel = legacy `runPIPSeed` proven shape; restoration not invention) |
| LOC size | LOW (131 LOC, single file refactor + 4 small touchups) |
| Falsifier coverage | HIGH (Gate C is the failure mode, directly measurable) |
| Recovery path if budget too tight | MEDIUM (Lever A allows in-cluster recovery without REVERT) |
| Latent defect surface | LOW (no new package imports; no new informer hooks; no new public API) |

**Independent residual revert probability estimate: ~15%.**

The dominant residual risk is **budget margin** (PM's flag): if the true unit count is
materially higher than 1000, even GOMAXPROCS=8 may not complete inside 8 min. Lever A
turns this from a hard REVERT into a chart-value bump. Lever C makes the diagnosis
mechanical. The recovery path is bounded and operator-driven; no second ship required.

Compare to the predecessor's implicit MEDIUM (their 4 self-flagged fresh-eyes checks
+ 402 LOC + 3 surfaces): predecessor's revert probability is more like 35-50% by the
same scoring, dominated by the imaginary code paths flagged in §2.

## 10. STAGE-2 PREVIEW (ship 0.30.238)

See companion doc `docs/ship-0.30.238-stage-2-crud-triggers-2026-06-02.md`.

Stage 2 ships:
- Widget-CR / RA-CR re-seed scope (hooked at `deps_watch.go:201,221`).
- RBAC-shift re-seed scope (hooked at `bindings_by_gvr_delta.go:131,151,176,203`
  + new helper that returns the affected GVR set explicitly).
- Helper code that does NOT exist today: `harvester.snapshotForGVR`,
  `navHarv.snapshotForGVR`.

Stage 2 is **NOT load-bearing** for the Gate C failure. It's an admin-UX improvement.
Stage 1 customer correctness must land + bench first; Stage 2 follows on the next
ship cadence cycle.

## 11. REFERENCES

- `docs/ship-0.30.237-f4-prewarm-engine-trace-2026-06-02.md` — predecessor HYPOTHESIS
- `docs/ship-0.30.238-stage-2-crud-triggers-2026-06-02.md` — companion follow-up
- `internal/handlers/dispatchers/prewarm_engine_boot.go:217-340` — current serial seedScopeYielding
- `internal/handlers/dispatchers/phase1_pip_seed.go:379-425` — legacy `runPIPSeed`
  cohort-parallel shape (the PRIOR ART Stage 1 restores)
- `internal/cache/bindings_by_gvr_delta.go:131,151,176,203` — actual mutator entry points
  (NOT the imagined `onBindingChanged`)
- `internal/cache/bindings_by_gvr.go:301-343` — `enrolLocked` / `unrolLocked` (where
  the affected-GVR set is implicitly computed; Stage-2 will surface it)
- `internal/cache/bindings_by_gvr.go:520-596` — `EnumerateResourceCohorts`
- `feedback_recurring_regression_pattern` Change A — fresh-eyes peer review for repeat-surface
- `feedback_no_park_broken_behind_flag` — Lever A is a tuning knob NOT a defect-gate flag
- `feedback_customer_priority_over_refresher` — engine yields; never starves customer
- `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill` — no lazy-fill-cold
- `feedback_no_special_cases` — mechanism-uniform
- `feedback_chart_release_lockstep` — chart bump in lockstep (Lever A configmap value)
- Live evidence: pod `snowplow-5d696f64c4-q8wmr` (rev 409 = 0.30.235),
  `/tmp/snowplow-vars-detail.json`
- 0.30.236 Gate C failure: `/tmp/0.30.236-gate-A.txt`, `/tmp/0.30.236-gate-C-bench-final.log`

— architect (peer reviewer), 0.30.237 Stage 1, 2026-06-02
