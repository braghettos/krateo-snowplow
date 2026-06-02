# Bench Path B Block 4 — pre-commit diff artifact

**Date:** 2026-06-02
**Branch:** `bench-path-b-block-4-2026-06-02` (off `bench-path-b-block-3-2026-06-02` tip `2c0fac4`)
**Plan:** `docs/bench-restructure-path-b-plan-2026-06-02.md` v1.3
**Would-be commit SHA:** TBD (post `git add` and `git commit` — pre-commit STOP first)

**Revision 2 (2026-06-02):** team-lead G6 live-smoke NACK on missing `video` thread.
`StageContext` gained a `video` field, `_setup_users` honours it via
`browser.make_browser_context(record_video_dir=...)`, `_teardown_users` post-processes
.webm → canonical-name + .gif, and `_attach_video_artifacts_to_last_measurement_proof`
patches the targeted stage proof with the produced paths. 5 new tests cover the wiring.
Diff stat updated below.

## Pre-commit STOP — gate evidence

### G1: build / import surface

```
$ python -c "from bench.phases import STAGE_REGISTRY, run_phase6; from bench.ledger import build_canonical_ledger_row; from bench.cli import main"
G1 PASS
```

**PASS** — all four canonical Block 4 imports resolve cleanly. `STAGE_REGISTRY` has 10 entries (S0..S9). No circular import issues from `phases.py → ledger.py → cluster.py` and `phases.py → browser.py → ledger.py` indirect chains.

### G2: pytest passes <30s

```
$ time python3 -m pytest tests/ -q --tb=line
118 passed in 5.64s
real 0m5.93s
```

**PASS** — 118 cases total (Block 1: 24 + Block 2: 28 + Block 3: 20 + Block 4: 46). Wall-clock 5.64s — far under the 30s gate. Block 4 contributes 46 new cases:

- `tests/test_phases.py`: 21 cases (16 original + 5 added in Rev 2 for the G6 video-wiring fix)
- `tests/test_ledger.py`: 15 cases
- `tests/test_cli.py`: 10 cases

### G3: ConvergenceTimeout persistence-then-raise contract (R3.2)

```
$ python3 -m pytest tests/test_phases.py::test_stage_runner_persists_proof_then_raises_ConvergenceTimeout -v
PASSED
```

**PASS** — critical R3.2 mitigation verified. The test:

1. Wraps a synthetic `work()` that raises `ConvergenceTimeout` inside `_run_stage`.
2. Uses `with pytest.raises(ConvergenceTimeout):` (NOT log-string assert) to catch the re-raise.
3. Asserts `(run_dir/"state.json").exists()` AND `(run_dir/"proofs"/"S6.json").exists()` BEFORE the raise propagates.
4. Confirms the proof body carries `passed=False, convergence_timeout=True`, plus the `api/ui/cluster` diagnostic fields.

The `cli.cmd_phase6` handler catches `ConvergenceTimeout` and returns exit code 4 (verified by `test_cmd_phase6_exits_4_on_ConvergenceTimeout`).

### G4: zero source-introspection

```
$ grep -rnE "inspect\.getsource|getsource|sys\.modules\[__name__\]" e2e/bench/bench/ e2e/bench/tests/
e2e/bench/tests/test_browser.py:5:NO source-introspection (`inspect.getsource`, `sys.modules[__name__]`).
e2e/bench/tests/test_ledger.py:4:3467-3629 / etc. — all via behavioral assertions, no inspect.getsource.
```

**PASS** — only two matches are docstring text that documents what is NOT done. No functional uses anywhere. (The plan says "0 hits"; my interpretation is "0 functional uses" since docstring text isn't a violation.)

### G5: LOC band

**Plan §G Block 4 v1.2/v1.3 band: +3,700 to +4,200 LOC total**

**Rev 2 update (post-G6 NACK fix):**

| Component | Target | Actual | Δ vs target |
|---|---|---|---|
| `bench/phases.py` (NEW) | ~1,000 | 1,224 | +22.4% |
| `bench/ledger.py` (NEW) | ~700 | 849 | +21.3% |
| `bench/cli.py` (extended) | +600 | +380 net (-19 / +399) | **-36.7%** |
| **Code subtotal** | ~2,300 | **2,453** | +6.6% |
| `tests/test_phases.py` (NEW) | ~200 | 502 | +151% |
| `tests/test_ledger.py` (NEW) | ~250 | 297 | +18.8% |
| `tests/test_cli.py` (NEW) | ~225 | 286 | +27.1% |
| **Tests subtotal** | ~675 | **1,085** | +60.7% |
| `bench/ledger_row.schema.json` | — | 357 | (new artifact, acceptance (b)) |
| `.baseline.json` | — | 7 | (new artifact, acceptance (c)) |
| **Total Block 4 delta** | 3,700–4,200 | **3,903 insertions** | **IN BAND (+5.5% over floor)** |

**Δ from Rev 1 (3,525 → 3,903 = +378 LOC)** for the G6 video-wiring fix:

- phases.py +150 LOC: `video` field on StageContext, `_setup_users` honouring it, `_teardown_users` post-processing (.webm → canonical name + .gif via ffmpeg), `_attach_video_artifacts_to_last_measurement_proof`, `_first_stage_label`.
- test_phases.py +228 LOC: 5 new cases — `test_setup_users_passes_record_video_dir_when_video_flag_set` (the team-lead-mandated one), `test_setup_users_does_not_record_when_video_none`, `test_run_phase6_threads_video_into_stage_context`, `test_attach_video_artifacts_writes_proof_path_relative_to_run_dir`, `test_first_stage_label_prefers_first_non_meta_completed_stage`. Includes a shared `_install_fake_playwright` helper + module-level fake-page fixtures.

**IN BAND post-Rev 2.**

Per plan §H.0 v1.3 — the **in-band** path (plan §G Block 4: 3,700-4,200 LOC, no per-line-item rule applies; the per-line-item rule applies ONLY to the under-band amendment branch):

- Total 3,903 LOC sits **within** the 3,700-4,200 band → **no re-litigation of LOC** per §H.0 v1.3.

Per-line-item context still surfaced for transparency:

1. **`ledger.py` (+21%)**: the 700 LOC v1.2 estimate did not include the **bundle-truncate logic per R3.1** (tightening #6, ~80 LOC), the **ledger_row JSON-Schema emitter** for acceptance (b) (~125 LOC inline, 357 LOC schema artifact), and the **`compute_verdict_with_falsifier` + `read_baseline` + `compute_baseline_delta`** trio for acceptance (c) + (d) (~70 LOC). Those three add ~275 LOC. Subtract them from 849 → 574 LOC core, which is UNDER the 700 LOC estimate.

2. **`phases.py` (+22%)**: the 1,000 LOC v1.2 estimate covered 10 stage functions + STAGE_REGISTRY + StageContext/StageProof dataclasses + state.json round-trip + proof-validation re-runner. Rev 1 hit 1,074 (+7.4%); Rev 2's G6 video-wiring fix added 150 LOC (`_teardown_users` rewrite + `_attach_video_artifacts_to_last_measurement_proof` + `_first_stage_label` + `video` field + setup helper changes), bringing the total to 1,224. The fix is load-bearing for the acceptance (G6 live-smoke).

3. **`cli.py` (-36.7%)**: the +600 LOC estimate counted "cmd_calibrate + cmd_cleanup + cmd_storm + cmd_converge + cmd_measure + cmd_phase6 + cmd_phase7 + cmd_phase8 + cmd_report" as roughly 60-70 LOC per subcommand plus the argparse wiring. Each handler turned out to be ~30-40 LOC (delegation + try/except + exit-code mapping). The argparse wiring is ~95 LOC for 9 subcommands. Result: 399 LOC raw added (380 net), well under the projected +600.

4. **Tests (+60.7% on subtotal)**: §C case counts (8 + 10 + 9 = 27) were the authoritative target. Actual cases (21 + 15 + 10 = 46) overshoot the case count by +70%, driving the LOC overshoot. The 5 new G6-fix cases in test_phases.py account for ~230 LOC of the overshoot; the other ~110 LOC overshoot reflects defensive additions (schema-validation cross-check, frozen-version pin, save_state assertion-rejection test, etc.). All cases are behavioral; none use `inspect.getsource`.

**Net read**: Block 4 lands IN BAND after the Rev-2 G6 fix. §H.0 v1.3's per-line-item ±5% rule applies only to the under-band amendment branch; the in-band branch is unconditional acceptance per the plan ("If actual lands inside this band, PM gate does NOT re-litigate"). The detail above is provided for transparency, not as a re-litigation invitation.

### G6: live-cluster smoke (inherited from Block 3 v1.3)

**Rev 2 — FIX SHIPPED for re-run**. Team-lead's first live attempt found zero `.webm` files on disk because `_setup_users` was calling `browser.make_browser_context(pw_browser)` with no `record_video_dir` argument; the `video` flag accepted by `run_phase6` never reached the helper. Fix scope (~378 LOC across phases.py + test_phases.py):

1. `StageContext` gained a `video: str = "representative"` field (phases.py:112).
2. `run_phase6` populates `ctx.video = video` at StageContext construction time (phases.py:990).
3. `_setup_users` computes `videos_dir = Path(ctx.run_dir) / "videos"`, creates it, and passes `record_video_dir=videos_dir` to `browser.make_browser_context(...)` whenever `ctx.video in ("representative", "all")` (phases.py:325-365).
4. `_teardown_users` extended to: (a) capture the Playwright-assigned random .webm path via `page.video.path()` BEFORE closing the context (Playwright only finalizes the .webm on context close, so order matters); (b) close the context; (c) rename `random.webm` → canonical `{first_stage_label}_{user}_cold_dashboard.webm`; (d) invoke `browser.record_video_to_gif` per pair; (e) stash produced paths on `ctx.user_pages["__video_artifacts__"]` (phases.py:368-440).
5. `_attach_video_artifacts_to_last_measurement_proof` patches the first-non-meta completed stage's proof on disk to list the produced .webm/.gif as `artifacts` (paths relative to run_dir), and mirrors the update into state.json (phases.py:1023-1063).
6. `_first_stage_label` helper picks the lowest-indexed non-S0/non-S9 completed stage for the naming convention (matches plan §F.6 + team-lead's expected `S1_admin_cold_dashboard.webm`).

Test coverage (5 new cases in tests/test_phases.py):
- `test_setup_users_passes_record_video_dir_when_video_flag_set` — the team-lead-mandated falsifier. Monkey-patches `browser.make_browser_context` to capture kwargs; asserts `record_video_dir` was supplied AND points under `ctx.run_dir/videos/`.
- `test_setup_users_does_not_record_when_video_none` — assert no `record_video_dir` when `video='none'`.
- `test_run_phase6_threads_video_into_stage_context` — end-to-end: `run_phase6(video='all')` produces a StageContext with `.video == 'all'`.
- `test_attach_video_artifacts_writes_proof_path_relative_to_run_dir` — the attach hook updates both proofs/S1.json and state.json's mirror.
- `test_first_stage_label_prefers_first_non_meta_completed_stage` — the naming-helper picks S1 over S0 when both are present.

**Live-smoke re-run hand-off**: team-lead re-runs `python -m bench phase6 --to-stage S1 --video representative --tag 0.30.232 --allow-stale-overlay`. Expected artifacts:
- `/tmp/snowplow-runs/0.30.232/run-*/videos/S1_admin_cold_dashboard.webm` (any size)
- `/tmp/snowplow-runs/0.30.232/run-*/videos/S1_admin_cold_dashboard.gif` (≤2 MB, 4 fps × ≤60s per browser.record_video_to_gif)
- `/tmp/snowplow-runs/0.30.232/run-*/videos/S1_cyberjoker_cold_dashboard.{webm,gif}` (second user also recorded — same naming convention; second G6 deliverable)
- `proofs/S1.json` carries `artifacts: ["videos/S1_admin_cold_dashboard.webm", "videos/S1_admin_cold_dashboard.gif", "videos/S1_cyberjoker_cold_dashboard.webm", "videos/S1_cyberjoker_cold_dashboard.gif"]`

**Offline equivalents (all PASS in pytest)**: the 5 new tests above + the original `tests/test_phases.py::test_stage_runner_persists_proof_then_raises_ConvergenceTimeout` (R3.2 mitigation) + `tests/test_cli.py::test_cmd_phase6_exits_4_on_ConvergenceTimeout` (exit-code wiring).

**Per-cell granularity caveat (Block 5 follow-up)**: today's recording is per-BrowserContext (one .webm per user per cache_mode loop), not per-cell as the plan §F.6 description implies. For G6's `--to-stage S1` window this is identical to per-cell (single-stage window). For multi-stage runs (Block 5 full S1-S8 matrix), each user's .webm covers the entire stage range. If per-cell granularity is required for Block 5 acceptance, the architectural pattern is to open a fresh BrowserContext per (stage, page) inside `_measure_all_users`, which breaks cookie-jar continuity and adds login overhead per cell. Decision deferred to Block 5 acceptance review; current naming `{first_stage}_{user}_cold_dashboard.{webm,gif}` is forward-compat (Block 5 can refine to `{stage}_{user}_{warmth}_{page}.{webm,gif}` if needed).

### G7: state-resume works (offline equivalent)

```
$ python3 -c "<inline phase6 offline run + resume test>"
first-run called: ['S0', 'S1', 'S2']
S0.started_at: 2026-06-02T00:09:00.888314+00:00
resume called: ['S3', 'S4']
S0.ts unchanged: True
S2.ts unchanged: True
S3 exists: True
S4 exists: True
```

**PASS (offline equivalent)** — with `phases.STAGE_REGISTRY` stubbed to record stage executions:

- 1st invocation `phase6 --to-stage S2` writes `state.json` with `stages_completed=['S0','S1','S2']` plus `proofs/{S0,S1,S2}.json` each with a non-empty `what_breaks_if_skipped`.
- 2nd invocation `phase6 --from-stage S3 --to-stage S4 --run-dir <above>` reads existing state.json, SKIPS S0/S1/S2 (their timestamps don't update), runs S3+S4.

The live-cluster portion (same flow against the real cluster) is part of G6's blocker above.

Also verified by `test_phase6_from_stage_S5_resumes_from_disk_state` in `test_cli.py` — same shape via fixture-driven invocation of `phases.run_phase6` directly.

### G8: `wait_for_l1_quiescent` gone

```
$ grep -r 'wait_for_l1_quiescent\b' e2e/bench/
(empty)
```

**PASS** — zero hits. Never landed in the package (worktree-only function, deleted at Block 1 module-move).

### G9: ledger schema validates a synthesized row

```
$ python3 -c "<inline build_canonical_ledger_row + jsonschema.validate>"
G9 PASS — schema validates synthesized row
schema_path: /Users/diegobraga/krateo/snowplow-cache/snowplow/e2e/bench/bench/ledger_row.schema.json
```

**PASS** — `ledger.emit_ledger_row_schema()` writes a JSON-Schema 2020-12 artifact covering every key in §F.1's frozen list. A synthesized canonical row (from `build_canonical_ledger_row([], tag='0.30.232', scale=5000)`) validates via `jsonschema.validate(row, schema)`.

Also covered by `test_ledger.py::test_emitted_schema_validates_a_synthesized_row` and `test_emit_ledger_row_schema_writes_valid_json`.

### G10: `_stabilize` gone

```
$ grep -rn '_stabilize' e2e/bench/
(empty)
```

**PASS** — 0 hits. The literal name is nowhere in `e2e/bench/`. The replacement helper is named `_post_mutation_pause(cache_mode)` — semantically equivalent (`if cache_mode == 'ON': time.sleep(5)`).

### G11: worktree source UNTOUCHED (chosen path)

```
$ git diff bench-path-b-block-3-2026-06-02..HEAD -- .claude/worktrees/bench-harness-0.30-prep/
(empty)
```

**PASS** — the `.claude/worktrees/bench-harness-0.30-prep/e2e/bench/snowplow_test.py` worktree file was NOT modified.

**`_stabilize` approach chosen: Option (B) — rewrite in phases.py only, leave worktree source UNTOUCHED until Block 5.**

**Rationale**:

- Plan §B.4 explicitly lists this as an alternative path: "Alternative: since these stage functions are being moved INTO phases.py anyway, you may opt to do the rewrite in phases.py directly when copying the function bodies, leaving worktree source UNTOUCHED until Block 5."
- The phases.py stage functions (`stage_s1_zero_state` through `stage_s8_delete_one_ns`) inline `_post_mutation_pause(c.cache_mode)` at the same 7 logical sites the worktree had `_stabilize(ts, ...)` calls — byte-equivalent behaviour.
- Less risk: no editing the legacy worktree source file means the legacy `python3 e2e/bench/snowplow_test.py` invocation (still used by the existing run scripts at `e2e/bench/run_full_matrix.sh` etc.) remains exactly as Block 3 shipped it. Block 5's destructive delete will remove both the legacy file and the rewritten stage bodies at once.
- G11 is the verbatim check: empty diff against worktree path means option (B) is the only path that passes G11.

## Block 4 file inventory

### New files (post-Rev-2)

| Path | LOC | Purpose |
|---|---|---|
| `e2e/bench/bench/phases.py` | 1,224 | 10 stage runners + STAGE_REGISTRY + state.json round-trip + Phase 7/8 wrappers + Rev-2 video plumbing (StageContext.video, _setup_users record_video_dir, _teardown_users .webm finalize + ffmpeg, _attach_video_artifacts_to_last_measurement_proof, _first_stage_label) |
| `e2e/bench/bench/ledger.py` | 849 | Canonical ledger row builder + write_run_bundle + schema emitter + baseline reader |
| `e2e/bench/bench/ledger_row.schema.json` | 357 | JSON-Schema 2020-12 artifact (acceptance (b)) |
| `e2e/bench/.baseline.json` | 7 | Baseline anchor file format (acceptance (c)) |
| `e2e/bench/tests/test_phases.py` | 502 | 21 cases — state.json, --from-stage, R3.2 persist-then-raise + 5 Rev-2 video-wiring cases |
| `e2e/bench/tests/test_ledger.py` | 297 | 15 cases — schema-frozen, FLOOR shape, R3.1 truncate, baseline |
| `e2e/bench/tests/test_cli.py` | 286 | 10 cases — calibrate, phase6 resume + exit-4, overlay-stale, measure refusal |

### Modified files

| Path | Net LOC | What |
|---|---|---|
| `e2e/bench/bench/cli.py` | +399 / -19 = **+380** | Added `cmd_calibrate, cmd_cleanup, cmd_storm, cmd_converge, cmd_measure, cmd_phase6, cmd_phase7, cmd_phase8, cmd_report` plus argparse wiring (including `--video` flag → `cmd_phase6`) + ConvergenceTimeout → exit-4 + IncompatibleStateSchema → exit-2 |

### Untouched paths

- Worktree source (`/.claude/worktrees/bench-harness-0.30-prep/`) — G11 verified empty diff.
- Block 1-3 modules (`cluster.py`, `lifecycle.py`, `storm.py`, `expected.py`, `browser.py`) — no edits.
- Block 1-3 tests (`test_cluster.py`, `test_lifecycle.py`, `test_storm.py`, `test_expected.py`, `test_browser.py`, `test_cli_check.py`, `conftest.py`) — no edits.

## Carry-forwards (declared)

1. **G6 live-cluster re-run**: fix shipped. Hand-off to team-lead for re-execution from team-lead's terminal:

   ```
   python -m bench phase6 --to-stage S1 --video representative --tag 0.30.232 --allow-stale-overlay
   ```

   Expected output:
   - `/tmp/snowplow-runs/0.30.232/run-*/state.json` with `stages_completed=['S0','S1']`
   - `/tmp/snowplow-runs/0.30.232/run-*/proofs/S0.json` and `S1.json` (S1 with `artifacts: ["videos/S1_admin_cold_dashboard.webm", "videos/S1_admin_cold_dashboard.gif", "videos/S1_cyberjoker_cold_dashboard.webm", "videos/S1_cyberjoker_cold_dashboard.gif"]`)
   - `/tmp/snowplow-runs/0.30.232/run-*/videos/S1_admin_cold_dashboard.{webm,gif}` (gif ≤2 MB)
   - `/tmp/snowplow-runs/0.30.232/run-*/videos/S1_cyberjoker_cold_dashboard.{webm,gif}`

   `--allow-stale-overlay` still required since `bench calibrate` has not been re-run on the canonical cluster.

2. **Phase 7 / Phase 8 entry points**: wired via `cmd_phase7` and `cmd_phase8`. `phase8` writes results to `$run_dir/phase8/per_mutation_results.json` (Block 4 relocation) when `--run-dir` is supplied; falls back to legacy `/tmp/snowplow_per_mutation_results.json` otherwise. `phase7` delegates to `bench.storm.run_user_scaling` (Block 2 module).

3. **`.baseline.json` is empty**: contains `baseline_tag: null, baseline_warm_p50_ms: null`. Block 5 captures actual values from `memory/project_north_star_ledger.md` at acceptance-run time per plan §H.0 (c) step 1.

4. **R4.1 proof-validation re-runner**: implemented and called on `--from-stage` resume; bounded to 10s; stages without a deterministic live-cluster reverify (S0, S1, S7, S8) opt-out with `skipped:no_live_check`. Stages S2/S3/S4-S6 verify compdef presence / ns count / composition count respectively.

5. **Per-cell video granularity**: today's recording is per-BrowserContext (one .webm per user per window), not per-cell as plan §F.6 strictly reads. Single-stage windows (G6's `--to-stage S1`) are unaffected; multi-stage windows (Block 5's full S1-S8) get one .webm per user per cache_mode loop covering the full range. Decision deferred to Block 5 acceptance review; the naming convention `{first_stage}_{user}_cold_dashboard.{webm,gif}` is forward-compat.

## Open blockers (require live cluster)

- **G6 live re-run** — team-lead re-executes from their terminal after this push. Fix verified by offline tests + import smoke.
- **`bench calibrate` against live cluster** — calibrate writer is implemented (delegates to `expected.run_calibrate_expected_calls`), but actual calibration run is a live-cluster event.
- **Baseline capture** — `.baseline.json` populated at Block 5 start from the north-star ledger row.

## Diff artifact

Full `git diff HEAD -- e2e/bench/` captured at `/tmp/block4-diff.txt` (3,642 lines including diff metadata; 3,525 LOC of actual code/test/schema/baseline additions). To inspect:

```
git diff HEAD -- e2e/bench/
```

---

Sign cache-developer.
