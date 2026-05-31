# Path 3.2.2 — Closeout (0.30.220 HARD REVERT)

## Verdict

**HARD REVERT** at Phase 3 bench probe. Production restored to 0.30.219
(helm rev 380 = "Rollback to 378").

## What worked

The data-driven walker apiRef pagination mechanism is **functionally
sound**:

- `isApiRefTemplateDriven` predicate correctly identifies
  compositions-page-datagrid + blueprints-page-datagrid (the two
  apiRef+resourcesRefsTemplate widgets in the INIT tree).
- `resolverWantsContinue` reads the resolver's own
  `status.resourcesRefs.slice.continue` signal correctly.
- Pagination materialised 5× more walk events than the 0.30.219
  baseline (708 → 3,425 events in ~5 min, vs the 138-event baseline).
- New per-composition CRs in namespaces NOT in the baseline were
  discovered: bench-ns-23-439, bench-ns-29-440 — exactly the
  "walker reaches per-composition widgets" goal.
- No hardcoded GVRs anywhere in the data-driven path.
- Unit tests + race tests all pass.

## What failed

The mechanism runs INLINE on the boot critical path. The walker
goroutine is what `Phase1Warmup` blocks on before flipping `Phase1Done`
and therefore before `/ready` returns 200.

**Empirical cost on 50K compositions**:
- Per-composition recursion (depth 4–7): ~25–100ms
- Per apiRef page (5 compositions): ~125–500ms wall-clock
- 500-page cap × 2 apiRef widgets = up to 250s per widget × 2 = 500s
- Plus existing Phase 1 work = boot critical path > 360s startup
  probe budget → kubelet kills the container with SIGTERM → restart
  loop → pod never Ready

**Falsifier verdict**: bench probe failed at AC #1 ("pod restarts = 0").
North-star metrics never measured because pod never became Ready.

## Root cause class

`feedback_refresher_populate_amplification` (Phase B 0.30.185 HARD
REVERT) but applied to the BOOT critical path instead of steady-state
refresher cycles. Boot-path amplification is strictly worse because it
blocks `/ready` instead of just degrading steady-state.

## Followup ships (deferred — not in scope of this dispatch)

- **3.2.2.b**: refactor `iterateApiRefPages` to run in a background
  goroutine AFTER `Phase1Done` flips. Walker collects pagination work
  into a queue; a separate post-readiness pass drains it.
- **3.2.2.c (alternative)**: bound `iterateApiRefPages` per-widget time
  budget (e.g. 5s wall-clock max) at boot, rest deferred to a
  refresher-driven pass.

Either option requires re-ship + re-bench probe.

## Branch state

- Branch `ship-0.30.220-path3-2-2-walker-pagination` retained on
  braghettos remote for follow-up work.
- Tag `0.30.220` retained (CI built image successfully — the defect
  is at deployment time, not build time).
- Chart tag `0.30.220` retained.
- Snowplow on cluster: **0.30.219 helm rev 380**.

## Regression journal entry

Appended to `project_regression_journal.md` 2026-06-01 — full
empirical detail + prevention rules.
