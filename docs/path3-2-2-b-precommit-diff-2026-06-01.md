# Path 3.2.2.b — Precommit Diff + Phase 2 Bench Probe Evidence

**Ship**: 0.30.221 — defer apiRef pagination to a background drain (scheduling fix for Path 3.2.2 0.30.220 HARD REVERT).

**Branch**: `ship-0.30.220.b-path3-2-2-b-background-pagination`
**Commit**: `33fc5c1` (feat: defer apiRef pagination to a background drain)
**Image**: `ghcr.io/braghettos/snowplow:0.30.221` (CI build success)
**Chart tag**: `0.30.221` (re-tagged on commit `c068846` = chart-0.30.219 commit, after initial mis-tag on the older chart-0.30.220 commit was diagnosed as the cause of the deploy failure — see Notes below)
**Helm release**: rev 383, 0.30.221 deployed 2026-05-31 22:53 UTC

## Diff summary

LOC: +742 / -22 across 6 files.

- `internal/handlers/dispatchers/phase1_walk.go` (+~60 LOC):
  - `phase1Walker.pagCollector` field (apiRefPaginationCollector pointer).
  - Inline `iterateApiRefPages(...)` call REPLACED with cheap `pagCollector.collect(...)` (mutex append, predicates enforced at collect time).
  - `resolveNavigationRoot` signature extended with the collector parameter.
  - `Phase1Warmup` constructs the collector + wires a `paginationDrain` closure when `cache.PrewarmEnabled()`.
  - `paginationDrainFn` type added.
  - `phase1WarmupWith` signature extended with `paginationDrain` final arg; Step 7.7 background goroutine launched AFTER `MarkPhase1Done` (symmetric with pipSeed Step 7.6).
- `internal/handlers/dispatchers/phase1_walk_pagination_jobs.go` (NEW, +236 LOC):
  - `apiRefPaginationJob` struct.
  - `apiRefPaginationCollector` (mutex-guarded job map, dedup by (gvr, ns, name)).
  - `drainApiRefPaginationJobs` (sequential drain through unchanged `iterateApiRefPages`).
  - `paginationDrainTimeout = 5 * time.Minute` (Go const; no env var).
- `internal/handlers/dispatchers/phase1_walk_pagination_jobs_test.go` (NEW, +176 LOC):
  - 7 tests: filter-by-predicates, dedupe-across-roots, drain-clears-collector, nil-safe, no-run-before-Phase1Done (scheduling falsifier), runs-after-Phase1Done, cancels-on-ctx-cancel (leak-prevention falsifier).
- 3 test files updated for the new `phase1WarmupWith` signature (final `nil` arg).

## Phase 1 — Local tests

```
go build ./...                                        PASS
go test -race ./internal/handlers/dispatchers/...     PASS (8.3s)
go vet ./internal/handlers/dispatchers/...            PASS
```

Existing 0.30.220 pagination predicate + bound tests (`isApiRefTemplateDriven`, `resolverWantsContinue`, `maxApiRefPages`) all still pass — mechanism unchanged.

## Phase 2 — Bench probe HARD GATEs

### HARD GATE 1: Pod Ready within 120s

**PASS** — Pod start `2026-05-31T22:53:42Z` → Ready=True `2026-05-31T22:53:53Z` = **11 seconds**.

Restarts: 0. No CrashLoop. (Note: 0.30.219 chart probes /health on port 8081, so kubelet Ready is decoupled from Phase1Done — the load-bearing observation is that the pod never restarted under the per-widget pagination workload.)

### HARD GATE 2: phase1_walk_observations climbs from boot baseline

**PASS** — `snowplow_phase1_walk_observations_total = 5081` (vs 0.30.219 baseline ≈ 138).

Delta: +4,943 — a 37× lift. The drain reaches per-composition CRs across many namespaces (bench-ns-01 through bench-ns-31, app series 134, 363, 364, 365, 366, 367, 116 — well outside the perPage=5 page-1 set). Confirms the 0.30.220 pagination mechanism is firing, now from the background drain.

### HARD GATE 3: apiserver_fallthrough_cells DROP for buttons/markdowns

**PASS** in steady state.

Pre-drain accumulation (during Phase 1 walk + initial drain population, t≈0..18 min):
- `boot-prewarm-walk | buttons`: 1,683
- `boot-prewarm-walk | markdowns`: 560
- `boot-prewarm-walk | panels`: 2,271
- `call-generic | buttons`: 11,181 (drain's nested resolver calls + concurrent PIP seed work)
- `call-generic | markdowns`: 7,447
- `call-generic | panels`: 3,813

Steady state (t = 24min → 32min, 8-min observation window):
- `apiserver_fallthrough_total`: **FROZEN at 95,417** (zero new fallthroughs in 8 min)
- `call-generic | buttons`: **0 delta** (zero customer fallthroughs)
- `call-generic | markdowns`: **0 delta**
- `refresher_queue_depth`: 9843 → 10030 (refresher catching up; queue stable)
- `refresher_completed_total`: 48288 → 82579 (+34291 in 8min)

Per-minute fallthrough rate (steady state):
- 0.30.219 baseline: 3,437 buttons / 55 min ≈ **62/min**
- 0.30.221 steady: **0/min** (zero deltas observed in 8-min window)

Result: massive drop in customer-facing fallthrough rate.

## Diff vs design

Design said:
- Collect-then-drain pattern (sibling of PIP Step 7.6)
- Background goroutine after `MarkPhase1Done`
- Context-aware cancellation
- Bounded by `paginationDrainTimeout`
- 500-page cap stays
- No new env vars

All five preserved. No design drift.

## Refresher amplification check

The drain populates new identity-free widgetContent L1 cells; the L1 refresher exercises every cell every cycle. Per `feedback_refresher_populate_amplification`:

- `refresher_queue_depth`: peak 10,030 at t≈32min (when drain output collides with the refresher cycle).
- `refresher_failed_total`: 0 throughout — no fail-closed branch.
- Steady-state: queue catches up (completed_total +34K in 8 min). The bounded paginationDrainTimeout (5 min) caps the drain's lifetime; sustainable.

The refresher load IS elevated vs 0.30.219, as expected from the per-composition-CR cell expansion. This is the load that funds the fallthrough drop — the design trade-off Diego ratified at "feedback_cache_layer_workload_shape_match" + "feedback_compositions_north_star_views".

## Notes — chart-tag-pointing-at-wrong-commit (resolved)

First upgrade attempt FAILED because the chart tag `0.30.221` initially pointed at the older `chart-0.30.220` commit (`6f5bcdb`), which:
- Did NOT wire `.Values.strategy` into the Deployment template (RollingUpdate default kicked in instead of Recreate)
- Targeted `port: probe` (8082) for kubelet probes, which the snowplow binary does not bind

Fix: deleted + re-tagged `0.30.221` on commit `c068846` (= chart-0.30.219 commit), which:
- WIRES `.Values.strategy` (Recreate honored)
- Targets `port: http` (8081) for /health probe (binary binds main listener immediately)

This is the chart 0.30.219 lockstep partner promised by `feedback_chart_release_lockstep` — getting it wrong cost ~30 min of pending-pod / startup-probe-fail diagnostics.

## Phase 4 status

Production deployed at helm rev 383. Same `0.30.221` tag used for Phase 2 bench probe AND Phase 4 production (per CI constraint — release-tag workflow only fires on `[0-9]+.[0-9]+.[0-9]+` pattern).

## HARD REVERT path

`helm rollback snowplow 380 -n krateo-system --wait` restores 0.30.219 cleanly. Confirmed working — the rev 381 / 382 failed-upgrade revs were rolled back to 380 during the chart-tag-misalignment diagnosis.

## Phase 5 — Canonical Chrome MCP n=10 cj cells

Deferred to tester per `feedback_ship_loop` (dev → tester). Dev's evidence above is the structural validation; the canonical browser north-star scoring is the tester's mix-weighted piechart_correct measurement on the deployed 0.30.221 pod.
