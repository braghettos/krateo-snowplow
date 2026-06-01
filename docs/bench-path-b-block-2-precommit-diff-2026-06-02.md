# Bench Path B — Block 2 pre-commit diff (2026-06-02)

Branch: `bench-path-b-block-2-2026-06-02` off `bench-path-b-block-1-2026-06-02` (tip `d6e856e`).

Sign: cache-developer.

This is the STOP-before-commit artifact required by the dispatch. No commit was created; staging only. PM gate routes from here.

## Files (staged, not committed)

```
 e2e/bench/bench/__main__.py       |  14 +
 e2e/bench/bench/cli.py            | 508 +++++++ (NEW)
 e2e/bench/bench/expected.py       | 353 +++++ (NEW)
 e2e/bench/bench/lifecycle.py      |  21 (-14, +7 — log() shim replaced)
 e2e/bench/bench/storm.py          | 682 +++++++ (NEW)
 e2e/bench/tests/test_cli_check.py | 240 +++++ (NEW)
 e2e/bench/tests/test_expected.py  | 157 +++++ (NEW)
 e2e/bench/tests/test_storm.py     | 159 +++++ (NEW)
 8 files changed, 2120 insertions(+), 14 deletions(-)
```

Module surface — Block 2 ships exactly what plan §G Block 2 lists. Subsequent
subcommands (calibrate, cleanup, storm, converge, measure, phase6, phase7,
phase8, report) land in Block 4.

## Exit-criteria gates (G1-G10)

| Gate | Evidence | Verdict |
|------|----------|---------|
| G1 import | `from bench.storm import *; from bench.expected import *; from bench.cli import cmd_check` → `OK` (venv `.venv-bench`) | PASS |
| G2 `bench check` works | `bench check --tag 0.30.232 --allow-stale-overlay` returned 0 in 16.0s. 7/7 gates PASS. Image+helm matched 0.30.232 (helm rev 405). | PASS |
| G3 `bench check` fails cleanly | `FRONTEND_URL=http://invalid.localhost.invalid:9 bench check --tag 0.30.232` returned 2; stderr named `frontend_lb_reachable` (and `overlay_freshness` since the overlay isn't installed). | PASS |
| G4 non-GKE returns 3 | `kubectl config use-context kind-nova-kog && bench check --tag 0.30.232` returned 3; canonical context restored after. | PASS |
| G5 TOLERANCE = 0 | `grep -rE 'EXPECTED_CALLS_TOLERANCE\s*=' e2e/bench/bench/` → exactly one definition, value `0`. | PASS |
| G6 pytest | `pytest e2e/bench/tests/ -q --tb=line` → 52 passed in 3.05s wall (< 30s). Block 2 adds 20 cases (4 storm + 8 expected + 8 cli_check). Block 1's 32 still pass. | PASS |
| G7 no source introspection | `grep -rE 'inspect\.getsource\|getsource\|sys\.modules\[__name__\]' e2e/bench/bench/ e2e/bench/tests/` → 0 hits. | PASS |
| G8 no external callers of `snowplow_test` | `grep -rE 'from snowplow_test\|import snowplow_test' /Users/diegobraga/krateo/snowplow-cache/snowplow/ --include="*.py" --exclude-dir=.venv --exclude-dir=.venv-bench --exclude-dir=__pycache__ --exclude-dir=.claude` → 0 hits. Inside `e2e/bench/` also 0. | PASS |
| G9 LOC delta | See "LOC accounting" below. | DOCUMENTED |
| G10 lifecycle `log()` shim removed | `grep -E '^def log\b\|^def section\b' e2e/bench/bench/lifecycle.py` → 0 hits. `lifecycle.py` now does `from bench.cli import log, section`. | PASS |

## LOC accounting (G9) — split-report per dispatch

| Bucket | Target (plan §G Block 2) | Actual | Notes |
|---|---|---|---|
| Code (modules + `__main__`) | ~690 (§A.4 ~300 + §A.6 ~140 + partial cli.py ~250) | **1,557** (storm 682 + expected 353 + cli 508 + __main__ 14) | +126% overrun |
| Tests | n/a in plan but dispatch predicted "tests likely double that" | **556** (storm 159 + expected 157 + cli_check 240) | within predicted ~×2 of modules-ex-tests bucket |
| Existing-file diff | ~0 (lifecycle.py log() removal) | **+7 / -14** (net -7) | swap-out of log/section shim |
| **Total inserted** | +750 | **+2,120** | **+183% vs target** |

### Why the code-module overrun

- **storm.py: 682 vs ~300.** Plan §A.4 counted the lines moved verbatim
  (worktree 7567-7621 + 7622-7696 + 7697-7951 + 8225-8392 ≈ 568 LOC).
  Block 2 added Block-3/4 deferred-import shims (`_http_get`,
  `_login_all`, `_get_runtime_metrics`, `_read_l1_ready_ts`, `_record`,
  `_log`, `_section`) and inline fallbacks for `WIDGET_ENDPOINTS` /
  `RESTACTION_ENDPOINTS` so the module is import-clean during Block 2
  even though browser.py + ledger.py don't exist yet. That deferred-import
  surface adds ~60 LOC; expanded docstrings explaining the deferral
  contract add another ~50 LOC; existing whitespace/header docstring
  account for the rest.
- **cli.py: 508 vs ~250.** Plan §A.9 sized only `_gke_context_guard` +
  `cmd_check` + `verify_deployed_image`. The 7-item gate is fatter than
  estimated: 6 gate helpers (~30-60 LOC each) plus argparse + logger
  setup. Reviewer should compare gate helpers against `cmd_check` body
  — each gate helper is ~15-30 LOC and returns `(bool, str)` for testability.
- **expected.py: 353 vs ~140.** The plan-counted source ranges total
  ~85 LOC of substantive code (200-213 + 216-252 + 255-268 ≈ 67 LOC).
  Block 2 added the new `OverlayStale` exception class, `overlay_age_seconds`
  helper, `overlay_freshness_or_die`, `gate_overlay_freshness`, plus
  preserved `run_calibrate_expected_calls` with operator-facing fallbacks
  for the not-yet-shipped `bench.browser` (Block 3). Docstrings on the
  mtime-vs-JSON-field tightening (#4) consume ~12 LOC.

### Annotation of plan §G LOC-band inconsistency (PM carry-forward #3)

**Block 1 finding (from prior STOP report):** target +1,250, actual +3,024 — **+142% overrun.**

**Block 2 prediction:** target +750, actual +2,120 — **+183% overrun.**

Pattern matches: §G LOC targets count the moved-verbatim source ranges
(per §A line-range tables) and IGNORE three structural costs:

1. **Deferred-import shims** — Block 2 cannot top-import `bench.browser`
   or `bench.ledger` (they ship in Blocks 3 + 4). Every moved function
   that calls a Block-3/4 symbol grew an `_xxx()` wrapper that lazily
   imports the dependency. ~10 wrappers × ~6 LOC = ~60 LOC in storm.py
   alone.
2. **Test surface** — §C names 4 / 5 / "check parts" cases per module but
   each case is realistically ~25 LOC with monkey-patch setup. 20
   cases × ~25 LOC ≈ 500 LOC; plan implicitly budgeted ~150.
3. **Operator-facing diagnostics** — every gate helper carries a
   structured `(bool, str)` return + `_stderr_log` line so the operator
   can read what failed from `bench check` stderr alone (acceptance
   criterion (e) demand). Plan §G assumes a tighter "raise/exit"
   shape that's not testable.

**Recommendation for Block 3 + 4 plan refresh:** scale §G targets ×2 for
modules and ×3 for tests; the wire-shape boundaries cost LOC that the
plan's `wc -l` budget doesn't reflect.

## PM carry-forwards from Block 1 — verification

| # | Carry-forward | How verified |
|---|---|---|
| 1 | Replace lifecycle.py's `log()` shim with cli.py's coloured logger | `lifecycle.py` now `from bench.cli import log, section`. Block-1 32 lifecycle/cluster tests still PASS (no log API drift). G10 grep confirms shim removal. |
| 2 | cli.py `_gke_context_guard` is a SUBSET of `cluster.gke_context_guard` | `cli._gke_context_guard()` delegates: line `cluster.gke_context_guard(allow_non_gke=allow_non_gke)` is the entire gate. The wrapper additionally reads `kubectl config current-context` for the caller's return value (additive, not re-implementation). Identity verified by the 3 cli_check tests for the guard — same exit code (3), same env-var bypass, same canonical-context PASS. |
| 3 | Predict + report LOC overrun (annotated) | See "LOC accounting" + "Annotation of plan §G LOC-band inconsistency" above. Block 1 overrun was +142%; Block 2 is +183%. Pattern reproduced. |

## Live-cluster evidence (G2-G4)

```
$ bench check --tag 0.30.232 --allow-stale-overlay
  === bench check — preflight (7 gates) ===
  [01:24:45] gke_context: PASS (context='gke_neon-481711_us-central1-a_cluster-1')
  [01:24:46] snowplow_pod_ready: PASS (replicas 1/1)
  [01:24:46] Deployed image verified: ghcr.io/braghettos/snowplow:0.30.232
  [01:24:46] image_tag_match: PASS (tag=0.30.232)
  [01:25:00] crds_present: PASS (25 CRDs)
  [01:25:01] helm_release_lockstep: PASS (tag=0.30.232)
  [01:25:01] frontend_lb_reachable: PASS (http://34.46.217.105:8080/login)
  WARNING: --allow-stale-overlay bypasses freshness gate (overlay age=missing).
           Run `python -m bench calibrate` to refresh.
  [01:25:01] overlay_freshness: BYPASS (age=missing)
  === check complete in 16.0s ===
  [01:25:01] check PASS (7/7 gates)
exit: 0
```

Helm release lockstep verified: `helm list -n krateo-system` shows
`snowplow snowplow-0.30.232 rev=405`; gate matched.

```
$ FRONTEND_URL=http://invalid.localhost.invalid:9 bench check --tag 0.30.232
  ...
  [01:24:18] frontend_lb_reachable: FAIL (connection error: ...)
  [01:24:18] overlay_freshness: FAIL (EXPECTED_CALLS overlay stale: ...
             run: python -m bench calibrate (or pass --allow-stale-overlay to
             bypass with a loud log).)
  === check complete in 17.3s ===
  check FAILED: 2/7 gates failed: frontend_lb_reachable, overlay_freshness
exit: 2
```

```
$ kubectl config use-context kind-nova-kog
$ bench check --tag 0.30.232
  bench: GKE context guard FAIL — current-context='kind-nova-kog' ...
exit: 3
```

## Unresolved blockers / handling notes

- **Overlay file missing on cluster.** The freshness gate FAILs because
  `/tmp/snowplow-runs/calibration/expected_calls.json` doesn't exist
  on the operator's local box (this is a per-machine overlay path).
  Two handling paths surfaced in G2 evidence:
  1. `--allow-stale-overlay` → BYPASS with loud-log; rest of `bench check`
     PASSes. **Used for G2 verification.**
  2. Run `python -m bench calibrate` to create the overlay. Calibration
     itself depends on Block-3 `bench.browser`; today it returns 2
     with a clear "requires bench.browser (Block 3)" message. **Not
     blocking Block 2** — calibrate lands in Block 4.
- **Subcommands beyond `check`.** Plan §G Block 2 only ships `check`;
  the argparse surface deliberately exposes only that subcommand. Block
  4 adds the rest.
- **Block 3 deferred imports.** `bench.browser` (login_all, http_get,
  get_runtime_metrics, WIDGET_ENDPOINTS, RESTACTION_ENDPOINTS,
  wait_for_compositions) and `bench.ledger` (record, _read_l1_ready_ts)
  are not yet shipped. `storm.run_user_scaling` and
  `expected.run_calibrate_expected_calls` raise clean operator-facing
  ImportError messages if invoked pre-Block-3/4. Block-2 tests stub
  them via `monkeypatch.setitem(sys.modules, "bench.lifecycle",
  ...)` patterns so the import-clean property is verifiable in isolation.

## Pre-commit assertion

```
$ git status
On branch bench-path-b-block-2-2026-06-02
Changes to be committed:
  modified:   e2e/bench/bench/lifecycle.py
  new file:   e2e/bench/bench/__main__.py
  new file:   e2e/bench/bench/cli.py
  new file:   e2e/bench/bench/expected.py
  new file:   e2e/bench/bench/storm.py
  new file:   e2e/bench/tests/test_cli_check.py
  new file:   e2e/bench/tests/test_expected.py
  new file:   e2e/bench/tests/test_storm.py
```

STOP. Awaiting team-lead ACK + PM gate routing before commit + push.
