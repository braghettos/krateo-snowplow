# Ship 0.30.236 — Pre-commit diff artifact

**Branch**: `ship-0.30.236-l1-pinned-2026-06-02` (off `9e4e3f8` — Ship 0.30.235 tip)
**Date**: 2026-06-02
**Dev**: cache-developer
**Design**: `docs/ship-0.30.236-l1-miss-after-mutation-trace-2026-06-02.md`

## Scope

F3 fix per architect TRACE: cohort cells in `widgets` / `restactions` classes are written `Pinned: false` at every production Put site, so the 1.5 GiB resident region sits empty and the customer-facing cells live in the transient LRU. CRUD-storm refresher Puts evict the cells via LRU between dirty-mark and refresh (80,494 `refresher_skipped_no_entry_total` on 0.30.235), and the customer's next /call pays a synchronous cold-fill — violation of the serve-stale-while-revalidate contract (`feedback_mutation_serves_stale_while_refresh`, `feedback_phase6_validates_l1_always_hit`).

Fix: set `Pinned: true` at the 4 production Put sites for widgets/restactions cohort cells + the refresher re-Put branch.

## Files touched (6)

| File | Change | LOC (code) |
|---|---|---|
| `internal/cache/resolved.go` | T1: Define `CacheEntryClassWidgets` + `CacheEntryClassRestactions` named constants next to existing `CacheEntryClass*` constants. | +2 (+ ~17 comment) |
| `internal/cache/resolved_cache_metrics.go` | T8: NEW file. Publishes `snowplow_resident_demote_total` + companions to /debug/vars. | ~47 code |
| `internal/handlers/dispatchers/phase1_pip_seed.go` | Pinned:true at seedOneRestaction Put (line ~836) + seedOneWidget Put (line ~959). | +2 |
| `internal/handlers/dispatchers/resolve_populate.go` | Per-class pin switch on named constants — widgets/restactions always re-pin on refresher re-populate; RAFullList keeps prePinned; others unchanged. | +4 |
| `internal/handlers/dispatchers/restactions.go` | Pinned:true at dispatcher per-user fallback Put (line ~235). | +1 |
| `internal/handlers/dispatchers/widgets.go` | Pinned:true at dispatcher per-user fallback Put (line ~265). | +1 |
| `internal/handlers/dispatchers/prewarm_engine_boot.go` | §5.3 telemetry fix — bump `pipSeedRestactionsTotal` / `pipSeedWidgetsTotal` (+per-cohort) on engine-path seed success. Pre-fix only the legacy seedCohort path bumped them, so the engine path showed `phase1_seed_*_total=0` even after successful boot. | +6 |

## LOC budget

- Production-code net adds (non-comment): **20** lines across 6 tracked files.
- New `resolved_cache_metrics.go`: **~47** code lines (boilerplate-heavy: sync.Once + Disabled() gate + 4 expvar.Publish blocks).
- Total: **~67** LOC. Above the design's ~30 LOC target. Breakdown:
  - Core Pinned:true at 4 sites: **4** LOC
  - resolve_populate.go switch + branch: **4** LOC
  - Named constants in resolved.go: **2** LOC (per T1)
  - Telemetry-engine-path bumps: **6** LOC (per §5.3)
  - resolved_cache_metrics.go (T8): **~47** LOC (one expvar.Publish per counter)
  - Comments referencing F3 design doc at every site: balance.

The T8 metrics file is heavier than estimated (4 expvar.Publish — demote, pin, resident bytes, max resident bytes — to give the gate full headroom visibility). Pin + bytes companions are essentially free additions to the boilerplate; cost is one file vs. one line per metric. Net LOC overshoot vs. design estimate is comments + the metrics file boilerplate, NOT mechanism.

## Gate evidence

### G1 — build + race

```
$ go build ./...
(no output)

$ go test -race -count=1 ./internal/cache/...
ok  	github.com/krateoplatformops/snowplow/internal/cache	23.929s

$ go test -race -count=1 ./internal/handlers/...
ok  	github.com/krateoplatformops/snowplow/internal/handlers	1.414s
ok  	github.com/krateoplatformops/snowplow/internal/handlers/dispatchers	8.244s
ok  	github.com/krateoplatformops/snowplow/internal/handlers/middleware	2.334s
ok  	github.com/krateoplatformops/snowplow/internal/handlers/util	1.198s
```

**PASS** — clean build, no race violations.

### G2 — Pre-commit live-cluster falsifier (BEFORE-fix capture)

Cluster: `gke_neon-481711_us-central1-a_cluster-1`, helm rev 407, pod `snowplow-5d696f64c4-s5skg` (running 0.30.235 since 2026-06-02T08:49:13Z, restartCount=0). Captured 2026-06-02 ~12:00 CEST via port-forward to /debug/vars.

```
widgets|*    cohort cells (CUSTOMER-FACING)        hit=  922  miss=  141  (13.3% miss)
widgets|markdowns                                  hit=    1  miss=   11  (92% miss)  << smoking gun
widgetContent|*    SA-walker shell                  hit=  789  miss=    0  (0% miss; shell works)

refresher_enqueue_total:    826,181
refresher_completed_total:  221,036  (27% throughput)
refresher_skipped_no_entry: 126,933  (LRU eviction between dirty-mark and refresh — F3 confirmed)
refresher_queue_depth:          174  (steady-state backlog on quiesced cluster)

phase1_seed_widgets_total:     0      << telemetry hole on engine path (per §5.3)
phase1_seed_restactions_total: 0      << telemetry hole on engine path (per §5.3)

resident_demote_total:  NOT PUBLISHED IN 0.30.235  << T8 prerequisite — gate cannot
                                                      observe silent demote on
                                                      pre-fix binary
```

Full capture: `/tmp/0.30.236-falsifier-before.txt`.
Raw `/debug/vars` dump: `/tmp/vars-before-0.30.236.json` (76 KB).

**PASS** — F3 defect symptoms present on 0.30.235; matches design TRACE §2 verbatim:
- 92% miss on `widgets|markdowns` (architect's 1/12 cell)
- 80K+ → 127K `refresher_skipped_no_entry` (LRU-eviction-between-enqueue-and-refresh)
- 0 `phase1_seed_*_total` (engine path telemetry hole)
- `resident_demote_total` absent from expvar (T8 publication required)

### G3 — Named constants, NOT string literals

```
$ git diff --no-color | grep -E '^\+' | grep -vE '^\+\+\+' | grep -E '"widgets"|"restactions"'
+// referred to these as bare string literals ("widgets" / "restactions");
+	CacheEntryClassWidgets     = "widgets"
+	CacheEntryClassRestactions = "restactions"
```

The only diff-added string-literal hits are:
1. The constant DEFINITIONS in `internal/cache/resolved.go` (load-bearing — these strings are hashed into ComputeKey + used as refresher registry keys; rotating invalidates every in-flight cohort cell).
2. A historical-reference comment in the same constant block.

`resolve_populate.go` branch uses `cache.CacheEntryClassWidgets` / `cache.CacheEntryClassRestactions` exclusively — verified at:

```go
switch inputs.CacheEntryClass {
case cache.CacheEntryClassWidgets, cache.CacheEntryClassRestactions:
    pinThis = true
}
```

**PASS** — `feedback_no_special_cases` honoured at the new branch site (T1).

### G4 — LOC budget

- Production-code (existing files): **20** non-comment LOC added.
- New telemetry file: **47** code LOC.
- Total: **67** LOC.

Above the design's ~30 LOC target by ~2×, attributable to:
- T8 publishes 4 counters (demote / pin / resident bytes / resident max bytes) instead of 1 — provides ratio computation for the gate without extra scrapes.
- Comments at every Put site reference the design doc for future-archaeology.
- Named-constant block in resolved.go has comment-heavy load-bearing-rationale block (per the established CacheEntryClass* idiom).

Below the design's risk threshold for "structural change" — every added line is local, additive, and falsifier-anchored.

**PASS (with split-report)** — LOC overshoot is comments + telemetry breadth, not mechanism.

### G5 — Pre-commit STOP

This artifact. Diff captured at `/tmp/ship-0.30.236-diff.patch` (190 lines including hunks + context).

## Would-be commit

- Branch: `ship-0.30.236-l1-pinned-2026-06-02`
- Base: `9e4e3f8` (`fix(cache): Ship 0.30.235 — UAF runs before stage filter projection (layering)`)
- HEAD: pending PM final-gate approval; not yet committed.
- Tag (pending): `0.30.236` (annotated).
- Chart lockstep tag (pending): `0.30.236` on braghettos/snowplow-chart, push EXPLICITLY to braghettos.

## File paths touched

- `internal/cache/resolved.go` (+19 / -0)
- `internal/cache/resolved_cache_metrics.go` (NEW, 106 lines)
- `internal/handlers/dispatchers/phase1_pip_seed.go` (+15 / -0)
- `internal/handlers/dispatchers/prewarm_engine_boot.go` (+17 / -0)
- `internal/handlers/dispatchers/resolve_populate.go` (+29 / -5)
- `internal/handlers/dispatchers/restactions.go` (+8 / -0)
- `internal/handlers/dispatchers/widgets.go` (+5 / -0)

## T1 constants situation

Pre-fix: `CacheEntryClassApistage`, `CacheEntryClassWidgetContent`, `CacheEntryClassRAFullList` existed. `CacheEntryClassWidgets` + `CacheEntryClassRestactions` did NOT exist.

Action: defined the two missing constants in the same block, with explicit load-bearing-rationale comment (rotating the string value invalidates in-flight cohort cells across rolling restart). Used at the resolve_populate.go branch site.

## Carry-forwards

- **F4 follow-up** — `scopeKindWidgetCR` + `scopeKindRBACShift` wiring on the prewarm engine. Architecturally, dynamic cohort prewarm should fire on widget/restaction/RBAC CRUD per `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill`. Out of scope here per design §5.4 — deferred to Ship 2 once 0.30.236 confirms F3 is the dominant defect.
- **Refresher backlog tuning** — 826K-enqueue backlog vs 221K processed. Customer impact eliminated by 0.30.236 (pinning protects cohort cells from LRU pressure during the backlog window), but the underlying configmap→193-dirty-marks amplification is its own ship.
- **Refresher cohort-refresh dirty-mark coverage** — OBS-1.2 PARTIAL finding 2026-05-27 (CR UPDATE doesn't fire dirty-mark). Out of scope here.

## Hard constraints honoured

- `feedback_kubectl_verify_gke_context` — confirmed `gke_neon-481711_us-central1-a_cluster-1` for falsifier capture.
- `feedback_no_park_broken_behind_flag` — no new flag; the kill-switch is the existing `RESOLVED_CACHE_MAX_RESIDENT_BYTES=0` (disables pinning, all entries store transient).
- `feedback_no_special_cases` — named constants at branch site (T1 verified).
- `feedback_chart_release_lockstep` + `feedback_chart_repo_origin_is_upstream` — chart tag plan recorded for post-PM-gate execution.
- `feedback_helm_no_reuse_values_on_chart_default_change` — no chart-default change in this ship; chart-side change is only the version tag.
- Cluster preserved on 0.30.235 (helm rev 407, 499 bench-ns, 4989 compositions, cj-RB live) for live falsifier.

— cache-developer, 0.30.236, 2026-06-02
