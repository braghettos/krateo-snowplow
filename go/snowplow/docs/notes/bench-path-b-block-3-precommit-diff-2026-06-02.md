# Bench Path B Block 3 — pre-commit diff artifact

**Date:** 2026-06-02
**Branch:** `bench-path-b-block-3-2026-06-02` (off `bench-path-b-block-2-2026-06-02` tip `fd92264`)
**Plan:** `docs/bench-restructure-path-b-plan-2026-06-02.md` v1.2

## Pre-commit STOP — gate evidence

### G1: build / import surface

```
$ python -c "from bench.browser import ConvergenceTimeout, record_video_to_gif, make_browser_context, login_all, http_get, wait_for_compositions; from bench.storm import *; from bench.expected import *"
(exit 0)
```

**PASS** — all symbols listed in the Block 3 dispatch G1 grep resolve cleanly. The Block 2 deferred-import wrappers in `storm.py` (`_http_get`, `_login_all`, `_get_runtime_metrics`) now resolve to live `bench.browser.*` symbols without ImportError. The Block 4 wrappers (`_record`, `_read_l1_ready_ts` → `bench.ledger.*`) still raise ImportError on call — expected; Block 4 ships those.

### G2: pytest passes ≥12 cases, total <30s

```
$ pytest e2e/bench/tests/test_browser.py -q --tb=line
....................                                                     [100%]
20 passed in 2.55s

$ pytest e2e/bench/tests/ -q --tb=line
........................................................................ [100%]
72 passed in 5.58s
```

**PASS** — 20 cases in test_browser.py (target ≥12), full suite 72 cases in 5.58s (target <30s).

### G3: ConvergenceTimeout via `pytest.raises` (acceptance (h))

```
$ grep -n "with pytest.raises(ConvergenceTimeout)" e2e/bench/tests/
e2e/bench/tests/test_browser.py:87:    with pytest.raises(ConvergenceTimeout) as exc_info:
```

**PASS** — one functional `with pytest.raises(ConvergenceTimeout)` block at test_browser.py:87 (`test_browser_measure_stage_raises_ConvergenceTimeout`). NO log-string assertion form. Per acceptance criterion (h) of plan §H, this is the canonical falsifier shape.

### G4: zero source-introspection

```
$ grep -rnE 'inspect\.getsource|sys\.modules\[__name__\]' e2e/bench/bench/ e2e/bench/tests/
e2e/bench/tests/test_browser.py:5:NO source-introspection (`inspect.getsource`, `sys.modules[__name__]`).
```

**PASS** — only match is the test file's own DOCSTRING stating the rule. No functional uses.

### G5: LOC band

| Component | LOC |
|---|---|
| `e2e/bench/bench/browser.py` (NEW) | 1,453 |
| `e2e/bench/tests/conftest.py` (+142 added) | +142 |
| **Code subtotal** | **1,595** |
| `e2e/bench/tests/test_browser.py` (NEW) | 568 |
| **Total Block 3 delta** | **2,163** |

Plan v1.2 band: **+2,800 to +3,200 LOC**. Actual: **+2,163**.

**UNDER-BAND** by 637 LOC. Per plan §H.0 — "Out-of-band actuals reopen the structural-cost analysis in §G.0."

Surfacing for PM gate review:

1. The under-band outcome reflects accuracy of the plan §A.5 hand-count ("~900 LOC" updated to "~1,400 LOC" in §G Block 3 v1.2). Actual `browser.py` is 1,453 LOC — almost exactly the v1.2 line-item estimate. The 200 MB bundle truncate-and-warn logic and the proof-validation re-runner are Block 4 surface; not Block 3 carry.
2. Deferred-import shims: ~50 LOC (slightly below the ~60 LOC §G.0 cost #1 estimate; browser.py needs fewer shim targets than ledger/cli will).
3. Tests at ~28 LOC/case × 20 cases = 568 LOC, tracking the §G.0 cost #2 per-case heuristic almost exactly. We over-delivered on case count (20 vs target ≥12) without inflating per-case LOC.
4. The v1.2 band was sized empirically against Block 1+2 overshoots; the structural costs that drove those overshoots are now visible and budgeted. Block 3 did not encounter a new fourth cost; the existing three costs were under-stressed by the surface this block touches.

**Recommendation**: do NOT widen Block 3's surface to hit the floor. Under-band is the correct outcome on the planned scope; widening would add LOC without acceptance value. Block 4 estimates (3,700–4,200) should stand unless we see Block 3 under-counting propagate via missed shims to Block 4.

### G6: smoke-gate option chosen — **option (b)**

**Chosen**: unit-test `test_smoke_video_pipeline` in `test_browser.py` (lines 522-565) exercises `make_browser_context` + `record_video_to_gif` end-to-end against a fake Playwright `BrowserContext` and a stubbed ffmpeg subprocess.

**Rationale**:

- **Decoupled from Block 4** — option (a) requires a `phases.py` stub which Block 4 will replace; the stub itself is throwaway LOC, and Block 4 dev would need to delete + replace it. Option (c) (adhoc `python -m bench.browser smoke`) would add a module-level entry point that no canonical CLI invocation actually uses. Option (b) keeps Block 3 self-contained.
- **No live-cluster dependency** — the smoke gate falsifier MUST work without `gke_neon-481711_us-central1-a_cluster-1` access. Option (a) would still require it for S1.
- **Fully unit-testable** — option (b) runs in <1s wall-clock, fits inside the pytest <30s budget, and exercises the exact two new helpers the plan calls out as Block 3 deliverables (`make_browser_context`, `record_video_to_gif`).
- **Block 4 hand-off contract preserved** — Block 4 dev's exit criterion in §G already calls for `python -m bench phase6 --tag 0.30.232 --to-stage S2` producing a real per-stage proof + ledger row. Block 4's smoke test is its own block's responsibility; Block 3's responsibility is the helper layer. Falsifier (h) (ConvergenceTimeout) is the load-bearing acceptance check, and it's covered via pytest.raises.

**PASS** — `test_smoke_video_pipeline` passes in pytest, asserts (a) `make_browser_context(record_video_dir=videos/)` creates the dir and forwards the kwarg, (b) `record_video_to_gif(webm, gif)` returns True with a <2 MB gif on disk under stubbed ffmpeg.

### G7: Block 2 deferred-import surface unblocked

```
$ pytest e2e/bench/tests/test_storm.py e2e/bench/tests/test_expected.py -q --tb=short
............                                                             [100%]
12 passed in 3.20s
```

**PASS** — Block 2 tests still pass. The `bench.storm` deferred-import wrappers (`_http_get`, `_login_all`) now resolve to live `bench.browser.*` symbols rather than raising ImportError; the storm tests stub them out at the wrapper layer (monkeypatched on `storm_mod._http_get`, etc.), so behaviour is unchanged. Block 4's wrappers (`_record`, `_read_l1_ready_ts`) still raise ImportError on actual call — that's expected; Block 4 ships `bench.ledger`.

### G8: worktree source UNTOUCHED

```
$ git diff bench-path-b-block-2-2026-06-02..HEAD -- .claude/worktrees/bench-harness-0.30-prep/
(empty)
```

**PASS** — `.claude/worktrees/bench-harness-0.30-prep/e2e/bench/snowplow_test.py` is byte-identical to Block 2 tip. Block 5 deletes it; Block 3 only moves functions into the new package.

## File summary

### NEW: `e2e/bench/bench/browser.py` (1,453 LOC)

Module surface (`__all__`):

- HTTP: `login`, `login_all`, `http_get`, `http_get_json`, `http_get_with_headers`, `cache_metrics`, `get_runtime_metrics`
- Constants: `WIDGET_ENDPOINTS`, `RESTACTION_ENDPOINTS`, `ALL_ENDPOINTS`, `BROWSER_PAGES`, `BROWSER_NAV_PAGES`, `BROWSER_SCALING_PAGES`, `USERS`
- Convergence: `ConvergenceTimeout`
- Browser drivers: `browser_login`, `browser_measure_navigation`, `browser_measure_stage`, `verify_composition_count_api`, `verify_composition_count_ui`, `make_browser_context`, `record_video_to_gif`
- L1 / composition waiters: `wait_for_compositions`, `wait_for_l1_ready`, `wait_for_l1_warmup`

Deferred-import shims (per plan §G.0 cost #1, ~50 LOC):

- `_count_compositions`, `_count_bench_ns`, `_list_composition_names`, `_pct` → `bench.cluster.*`
- `_expected_calls_lookup`, `_expected_calls_tolerance` → `bench.expected.*`
- `_log`, `_section` → `bench.cli.*` (with stdlib fallback for unit-test runs)

Critical behaviour change at the SOLE convergence-poll site (browser.py:`browser_measure_stage` line ~1230):

- When VERIFY poll deadline expires with `matched=False`, raises `ConvergenceTimeout(stage, user, api, ui, cluster, timeout_secs)` instead of logging `TIMEOUT/MISMATCH` and writing `convergence_ms=-1` into the result dict (worktree source 6465-6475 silent-skip bug).
- Block 4's stage runner catches this, persists a stage proof with `passed=False` AND `convergence_timeout=true`, then re-raises to abort the run with exit 4. For Block 3, only the `raise` ships; the catch is Block 4.

### NEW: `e2e/bench/tests/test_browser.py` (568 LOC, 20 cases)

| # | Case | Coverage |
|---|------|----------|
| 1 | `test_browser_measure_stage_raises_ConvergenceTimeout` | acceptance (h) — uses `with pytest.raises(ConvergenceTimeout)` |
| 2 | `test_browser_measure_stage_passes_when_api_equals_ui_equals_cluster` | happy path |
| 3 | `test_browser_measure_stage_cyber_uses_intra_user_consistency` | source 6454-6462 — RBAC-scoped consistency check |
| 4 | `test_record_video_to_gif_produces_under_2mb_gif` | ffmpeg stub end-to-end |
| 5 | `test_record_video_to_gif_returns_false_when_ffmpeg_missing` | missing-toolchain graceful skip |
| 6 | `test_record_video_to_gif_returns_false_on_oversize_and_unlinks` | R3.1 cap enforcement |
| 7 | `test_make_browser_context_passes_record_video_dir_to_playwright` | video dir kwarg + auto-mkdir |
| 8 | `test_make_browser_context_skips_video_when_none` | non-representative cells skip kwarg |
| 9 | `test_widget_terminal_state_passes_when_no_skeletons` | gate-1 PASS path |
| 10 | `test_widget_terminal_state_fails_when_skeletons_persist` | gate-1 FAIL path |
| 11 | `test_validate_widget_terminal_state_uses_scoped_selector_not_naked_ant_skeleton` | overlay-toast false-positive guard |
| 12 | `test_login_all_returns_dict_keyed_by_username` | login_all dict shape |
| 13 | `test_http_get_returns_response_data` | urllib.request happy path |
| 14 | `test_http_get_json_parses_response` | JSON-decoding wrapper |
| 15 | `test_wait_for_compositions_returns_true_when_count_at_target` | wait_for_compositions PASS |
| 16 | `test_wait_for_compositions_returns_false_on_timeout` | timeout-returns-False contract |
| 17 | `test_smoke_video_pipeline` | §6 option (b) smoke gate |
| 18 | `test_verify_composition_count_api_returns_minus_one_on_non_200` | error-path -1 sentinel |
| 19 | `test_verify_composition_count_api_returns_count_on_200` | happy path |
| 20 | `test_verify_composition_count_ui_reads_via_page_evaluate` | page.evaluate delegation |

### MODIFIED: `e2e/bench/tests/conftest.py` (+142 LOC)

Added `FakePage` class + `fake_page` fixture. Minimal duck-typed Playwright `Page` stand-in for unit tests: records `evaluate` / `goto` / `screenshot` calls; dispatches `evaluate(js)` by JS-string prefix to pre-configured DOM state. No Chromium launch; no live cluster.

The fixture exposes these mutable attributes for per-test scenario shaping: `_skeleton_scoped`, `_skeleton_raw`, `_skeleton_widget`, `_call_count`, `_errored_count`, `_ui_count`, `_has_token`, `_nav_timing`, `_waterfall`.

## Unresolved blockers

None.

## Branch + push state

```
$ git log --oneline -3
<HEAD>  refactor(bench): Path B Block 3 — browser.py + ConvergenceTimeout + video→gif
fd92264 refactor(bench): Path B Block 2 — storm.py + expected.py + preflight `bench check`
d6e856e refactor(bench): Path B Block 1 — skeleton + cluster.py + lifecycle.py + tests scaffolding
```

Ready for commit + push to `origin/bench-path-b-block-3-2026-06-02`.

## Sign-off

cache-developer
