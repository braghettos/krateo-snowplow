# Ship 0.30.237 Stage 1 — pre-commit diff artifact

**Date**: 2026-06-02 | **Ship**: 0.30.237 Stage 1 (boot scope cohort-parallel + Lever A/B/C)
**Branch**: `ship-0.30.237-stage-1-boot-parallel-2026-06-02` (proposed)
**Base commit**: `c892de3` (= 0.30.236 source, helm rolled back to rev 409 / 0.30.235)
**Design**: `docs/ship-0.30.237-stage-1-boot-parallel-f3-reapply-2026-06-02.md`

## Files changed (snowplow)

| File | Lines added | Lines removed | Notes |
|---|---:|---:|---|
| `internal/handlers/dispatchers/prewarm_engine_boot.go` | 240 | 108 | Refactor `seedScopeYielding` → `seedScopeParallel` + `buildCohortPlans`. Heavy comments (~half of additions) document the legacy shape-match. |
| `internal/handlers/dispatchers/phase1_pip_seed.go` | 31 | 9 | Lever A: `pipGlobalTimeout` const→var, reads `PREWARM_BOOT_TIMEOUT_MINUTES`; new env-key constant. |
| `internal/handlers/dispatchers/phase1_pip_metrics.go` | 16 | 0 | Lever C: `pipSeedUnitsPlannedTotal` atomic + expvar publish. |
| **TOTAL (snowplow)** | **287** | **117** | Executable-LOC net add ~88 (rest is comment headers). |

## Files changed (chart — braghettos/snowplow-chart)

| File | Lines added | Lines removed | Notes |
|---|---:|---:|---|
| `chart/values.yaml` | 35 | 1 | Lever A configmap key `PREWARM_BOOT_TIMEOUT_MINUTES: "8"`; image tag 0.30.236 → 0.30.237 with comment header documenting the regression + recovery shape. |

## Gate self-check

### G1 — build + race tests
- `go build ./...` PASS (no compile errors).
- `go test -race -count=1 ./internal/handlers/dispatchers/... ./internal/cache/...` PASS (8.097s + 24.258s; no races; existing tests unchanged).

### G2 — feedback_no_special_cases
- `buildCohortPlans` keys on canonical `cohort.Username + "|" + sort(Groups).join(",")` — mechanism-uniform.
- Errgroup limit = `runtime.GOMAXPROCS(0)` — same as `runPIPSeed`. No per-kind / per-GVR / per-user literals.
- Lever A reads a single env key; Lever B logs one structured event per cohort; Lever C is a single atomic counter.

### G3 — legacy-shape match
Cohort-parallel pattern matches `phase1_pip_seed.go:382-387` line-by-line:

| `runPIPSeed` (legacy, 0.30.179+) | `seedScopeParallel` (this ship) |
|---|---|
| `g, gctx := errgroup.WithContext(pctx)` | `g, gctx := errgroup.WithContext(ctx)` |
| `limit := runtime.GOMAXPROCS(0)` | `limit := runtime.GOMAXPROCS(0)` |
| `if limit < 1 { limit = 1 }` | `if limit < 1 { limit = 1 }` |
| `g.SetLimit(limit)` | `g.SetLimit(limit)` |
| `for _, c := range cohorts { cohort := c; g.Go(func() error { ... }) }` | `for _, key := range planOrder { plan := plans[key]; cohort := plan.cohort; g.Go(func() error { ... }) }` |
| Per-cohort `seedCohort` body iterates restactions then widgets serially | Per-cohort goroutine body iterates `cohortPlan.restactionRefs` then `cohortPlan.widgetEntries` serially |
| `g.Wait()` returns err; cohort-level errors swallowed to `nil` | `g.Wait()` returns err; cohort-level errors logged + `continue` per unit |

Additional engine-only invariant: `engineYieldCheckpoint` is invoked BEFORE `g.Go(...)` for each cohort AND between units within a cohort — satisfies `feedback_customer_priority_over_refresher`.

### G4 — F3 untouched + present
F3 `Pinned: true` sites (per design §4.3) verified present in HEAD source — UNCHANGED by this diff:

```
internal/handlers/dispatchers/restactions.go:245:      Pinned:  true,
internal/handlers/dispatchers/widgets.go:272:          Pinned:  true,
internal/handlers/dispatchers/phase1_pip_seed.go:871:  Pinned:  true,
internal/handlers/dispatchers/phase1_pip_seed.go:998:  Pinned:  true,
```

Refresher re-pin branch present in `resolve_populate.go:274-277`:
```go
switch inputs.CacheEntryClass {
case cache.CacheEntryClassWidgets, cache.CacheEntryClassRestactions:
    pinThis = true
}
```

T1 constants present in `internal/cache/resolved.go:178-184`:
```
CacheEntryClassWidgets
CacheEntryClassRestactions
```

`git diff HEAD -- internal/handlers/dispatchers/phase1_pip_seed.go internal/cache/resolved.go internal/handlers/dispatchers/resolve_populate.go internal/handlers/dispatchers/restactions.go internal/handlers/dispatchers/widgets.go` shows ZERO removal of these lines. (`phase1_pip_seed.go` diff is purely the Lever A env-key addition + the const→var move on `pipGlobalTimeout`; no Put-site lines touched.)

### G5 — LOC budget (~131 per design §4.7)
Snowplow net add executable: ~88 LOC (excluding comment headers and chart). Within the design budget. Comment overhead is intentional — the new `seedScopeParallel` and `buildCohortPlans` carry full provenance + legacy-shape annotations so a future reviewer cannot mistake the cohort-parallel shape for a new invention.

### G6 — pre-commit STOP
This document. Team-lead routes to PM final gate before commit/tag/push.

## Risk register acknowledgement

Per design §7 Risk Register:
- GOMAXPROCS-parallel seed steals CPU from customer /call — Mitigated by `engineYieldCheckpoint` before each `g.Go` and between units within a cohort (same shape as legacy `runPIPSeed`, proven 30+ ships).
- Worst-case unit count exceeds 8-min budget — Mitigated by Lever A (`PREWARM_BOOT_TIMEOUT_MINUTES` chart-tunable) + Lever C (planned-units expvar). Operator can recover via `helm upgrade --set env.PREWARM_BOOT_TIMEOUT_MINUTES=15` without a HARD REVERT.
- Map keyed by Cohort — Mitigated by canonical `string` key (Username + sorted Groups join) + retain `cache.Cohort` on `*cohortPlan` for goroutine to pass to `withCohortSeedContext`.
- F3 retention errors — F3 is in HEAD; no `git checkout` step in this dev plan. ZERO risk.

Independent residual revert probability estimate (design §9): ~15%, dominated by budget-margin worst case (Lever A is the recovery path).

## Reference artifacts

- Predecessor design (HYPOTHESIS): `docs/ship-0.30.237-f4-prewarm-engine-trace-2026-06-02.md`
- Stage 1 design (peer-reviewed): `docs/ship-0.30.237-stage-1-boot-parallel-f3-reapply-2026-06-02.md`
- Stage 2 follow-up (deferred): `docs/ship-0.30.238-stage-2-crud-triggers-2026-06-02.md`
- Diff artifacts: `/tmp/0.30.237-snowplow.diff`, `/tmp/0.30.237-chart.diff`

— cache-developer, 0.30.237 Stage 1, 2026-06-02
