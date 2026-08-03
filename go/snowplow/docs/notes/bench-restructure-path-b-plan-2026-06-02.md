# Bench Harness Restructure — Path B Detailed Implementation Plan

**Sign:** cache-architect
**Date:** 2026-06-02
**Parent audit:** cache-architect 2026-06-02 design audit (in-session deliverable; see §J for memory pointers)
**Source under restructure:** `/Users/diegobraga/krateo/snowplow-cache/snowplow/.claude/worktrees/bench-harness-0.30-prep/e2e/bench/snowplow_test.py` (8,691 LOC, ~380 KB)
**Destination tree:** `/Users/diegobraga/krateo/snowplow-cache/snowplow/e2e/bench/` (main worktree; the worktree fork is DROPPED after Block 5 acceptance)

All file:line citations point to the source file above. Citations are **TRACED** unless explicitly labelled **INFERRED**.

---

## A. Module-by-module file plan

Eight modules. Total budget 3,200–3,800 LOC after deletion (vs. 8,691 today; ≥56% reduction once self-tests + Phase 2/3 + dead helpers are removed). Each module gets a `__all__` so consumers see a curated public surface and dev can't accidentally reach into an internal.

### A.1 `e2e/bench/bench/__init__.py` (≤20 LOC)

Package marker. Exposes `__version__`, the stage registry, and `bench.cli.main` for `python -m bench`. Header docstring states "harness packaged 2026-06-02, see docs/bench-restructure-path-b-plan-2026-06-02.md."

### A.2 `e2e/bench/bench/cluster.py` (~450 LOC)

**Header docstring:** "Cluster I/O — kubectl wrapper + kubernetes-client helpers. Pure cluster mutators/readers; no harness state. GKE-context guard lives in `cli.py:preflight_check`."

**Functions moved (TRACED line range → new home):**

| Source line(s) | Function | Notes |
|---|---|---|
| 532–542 | `kubectl(*args, input_data, timeout_secs)` | Public |
| 545–633 | `_k8s_init`, `_k8s_is_404`, lazy-load block + module globals | Internal |
| 636–928 | All `k8s_list_*`, `k8s_delete_*`, `k8s_bulk_*`, `k8s_patch_*`, `k8s_split_gvr`, `k8s_create_namespace` (lines 636–928) | Public |
| 930–938 | `_k8s_gvr_for` | Internal |
| 1342–1385 | `_parse_ns_name`, `_count`, `_count_match`, `_crd_exists`, `_count_bench_argo` | Public |
| 2125–2150 | `deploy_compositiondefinition` (kubectl apply via stdin) | Public |
| 2233–2293 | `composition_yaml(ns, name)` | Public; pure string template |
| 2295–2342 | `ensure_composition_controller`, `delete_argo_apps_in_ns` | Public |
| 2344–2353 | `force_finalize_namespace` | Public |

**Public surface (`__all__`):**
`kubectl`, `k8s_list_clusterroles_by_prefix`, `k8s_list_clusterrolebindings_by_prefix`, `k8s_delete_clusterrole`, `k8s_delete_clusterrolebinding`, `k8s_list_roles_all_ns_by_prefix`, `k8s_list_rolebindings_all_ns_by_prefix`, `k8s_delete_role`, `k8s_delete_rolebinding`, `k8s_list_namespaces_by_prefix`, `k8s_delete_namespace`, `k8s_create_namespace`, `k8s_split_gvr`, `k8s_list_cluster_custom`, `k8s_patch_custom_finalizers_null`, `k8s_delete_custom`, `k8s_bulk_delete_clusterscope`, `k8s_bulk_delete_namespaced`, `k8s_bulk_patch_finalizers_null_custom`, `k8s_bulk_delete_custom`, `count_compositions`, `count_bench_ns`, `list_composition_names`, `deploy_compositiondefinition`, `composition_yaml`, `K8S_CLIENT_AVAILABLE` (read-only flag accessor).

**Internal helpers:** `_k8s_init`, `_k8s_is_404`, `_k8s_gvr_for`, `_parse_ns_name`, `_count`, `_count_match`, `_crd_exists`.

**Dependencies:** stdlib only (subprocess, threading, time, json); optional `kubernetes` SDK (lazy-loaded; falls back to kubectl per source 562–567).

---

### A.3 `e2e/bench/bench/lifecycle.py` (~700 LOC)

**Header docstring:** "Cluster lifecycle — namespace + composition + CRD CRUD, cleanup, cache toggle. Calls `cluster.py` for all kubectl/k8s-client IO. Holds NO harness state; functions are idempotent."

**Functions moved:**

| Source line(s) | Function |
|---|---|
| 1106–1280 | `_stop_port_forward`, `_start_port_forward`, `_wait_old_pods_gone`, `_chart_supports_cache_toggle`, `_set_cache_via_helm` |
| 1281–1340 | `enable_cache`, `disable_cache`, `cleanup_rogue_rbac` |
| 1387–1505 | `create_bench_namespaces`, `wait_for_bench_namespaces`, `count_compositions`, `list_composition_names`, `list_composition_names_from_cache` (the last via `http_get_json` — depends on `cluster.py`+`browser.py` HTTP helper) |
| 1510–1576 | `wait_for_restaction_steady_state` |
| 1577–2123 | `delete_all_compositions`, `delete_all_compositiondefinitions`, `cleanup_orphan_repoes`, `_drain_argo_apps`, `deep_clean_bench_namespace`, `delete_bench_namespaces` |
| 2153–2231 | `deploy_compositions`, `deploy_compositions_parallel` |
| 2329–2484 | Composition delete helpers `delete_one_composition`, `wait_for_composition_gone`, `wait_for_namespace_gone`, `wait_for_l1_key_gone`, `delete_one_bench_namespace` |
| 2485–2657 | `wait_for_crd`, `delete_all_clientconfigs`, `delete_bench_rbac`, `clean_environment` |
| 2748–2805 | `cluster_dirty_state`, `_destructive_clean_guard` |
| 4771–4814 | `assert_clean` |
| 7427–7563 | Synthetic user lifecycle `_scaleuser_name`, `_scaleuser_password`, `create_synthetic_users`, `delete_synthetic_users` |

**Public `__all__`:** lifecycle verbs the operator invokes: `clean_environment`, `assert_clean`, `cluster_dirty_state`, `enable_cache`, `disable_cache`, `create_bench_namespaces`, `wait_for_bench_namespaces`, `deploy_compositions_parallel`, `deploy_compositiondefinition`, `delete_all_compositions`, `delete_bench_namespaces`, `cleanup_rogue_rbac`, `wait_for_restaction_steady_state`, `create_synthetic_users`, `delete_synthetic_users`, `enable_cache`, `disable_cache`. Plus the predicates `cluster_dirty_state` and `_destructive_clean_guard` (renamed to `destructive_clean_guard` — drop underscore as it crosses module boundary).

**Internal:** `_start_port_forward`, `_stop_port_forward`, port-forward globals, `_chart_supports_cache_toggle`, `_set_cache_via_helm`, `_drain_argo_apps`, `_wait_old_pods_gone`.

**Dependencies:** `bench.cluster` (kubectl + k8s helpers), `bench.browser` (only `http_get_json` for `list_composition_names_from_cache`), stdlib.

---

### A.4 `e2e/bench/bench/storm.py` (~300 LOC)

**Header docstring:** "Storm scenarios — composition deploy storm + CRB-delete burst. Operator orchestration of `lifecycle.py` primitives for the disruptive workload used at Phase 6 S6 and the v5 CRB-delete reproduction."

**Functions moved:**

| Source line(s) | Function |
|---|---|
| 7567–7621 | `wait_for_active_users`, `measure_warmup_after_restart` |
| 7622–7696 | `measure_first_login_warmup` |
| 7697–7951 | `run_phase_user_scaling` (P7) — kept as `storm.run_user_scaling()` |
| 8225–8392 | `_pod_log_offset`, `_pod_logs_since`, `_crb_burst`, `_audit_uaf_emit_per_request_lint`, `crb_delete_burst` |

**Public `__all__`:** `run_user_scaling`, `crb_delete_burst`, `wait_for_active_users`, `measure_warmup_after_restart`, `measure_first_login_warmup`, `pod_logs_since` (rename `_pod_logs_since` and `_pod_log_offset` to public — operators tail logs across stage boundaries).

**Internal:** `_crb_burst`, `_audit_uaf_emit_per_request_lint` (UAF lint stays private).

**Dependencies:** `bench.cluster`, `bench.lifecycle`, `bench.browser` (for tokens), stdlib.

---

### A.5 `e2e/bench/bench/browser.py` (~900 LOC)

**Header docstring:** "Playwright driver — login, navigation, validation, convergence poll, video recording. The harness uses Playwright exclusively (no Chrome MCP); video → GIF post-processing via ffmpeg. NEVER uses kubectl-mediated paths in scored measurement (see feedback_no_kubectl_in_measurement.md)."

**Functions moved:**

| Source line(s) | Function |
|---|---|
| 421–525 | HTTP helpers used by browser flows: `login`, `login_all`, `_decompress`, `http_get`, `http_get_json`, `http_get_with_headers`, `cache_metrics` |
| 5448–5472 | `_browser_wait_for_call_stability` (legacy P4 path — kept but mark deprecated; `_browser_measure_navigation` is the production path) |
| 5474–5673 | `_browser_collect_metrics` (legacy P4) — **DELETE** (see §B) |
| 5674–5705 | `_browser_login` |
| 5708–5761 | `_WIDGET_SKELETON_COUNT_JS` JS literal |
| 5764–5781 | `_count_widget_skeletons` |
| 5784–5867 | `_validate_widget_terminal_state` |
| 5870–6041 | `_browser_measure_navigation` |
| 6241–6325 | `_verify_composition_count_api`, `_poll_piechart_progression`, `_verify_composition_count_ui` |
| 6327–6566 | `_browser_measure_stage` — the SOLE convergence-poll site (raise `ConvergenceTimeout` on the `matched=False` branch at source 6471–6475 instead of silently logging `MISMATCH`) |

**Public `__all__`:** `login`, `login_all`, `http_get`, `http_get_json`, `cache_metrics`, `browser_login`, `browser_measure_stage`, `browser_measure_navigation`, `verify_composition_count_api`, `verify_composition_count_ui`, `ConvergenceTimeout`, `record_video_to_gif` (new helper, ~30 LOC; see Block 3).

**Internal:** `_decompress`, `_count_widget_skeletons`, `_validate_widget_terminal_state`, `_WIDGET_SKELETON_COUNT_JS`, `_browser_wait_for_call_stability`, `_poll_piechart_progression`.

**New code (Block 3):**
- `class ConvergenceTimeout(Exception)`: raised when the VERIFY poll loop (source 6396–6463) hits its 300s deadline without `matched=True`. Replaces today's silent "TIMEOUT/MISMATCH" log + dict-only signal at 6465–6475. Callers cannot silently proceed.
- `record_video_to_gif(webm_path, gif_path, fps=4, max_seconds=60)`: ffmpeg one-shot (`-vf 'fps=4,scale=720:-1:flags=lanczos'`). Cap representative-only n=1 per cell per nav — see Risk Register §I.
- `make_browser_context(pw, *, video_dir=None, user_label=None)`: wrapper around `browser.new_context()` that conditionally enables `record_video_dir=video_dir / user_label`. Centralizes video flag so callers don't sprinkle the option.

**Dependencies:** `playwright.sync_api`, `urllib`, `bench.cluster` (only for `count_compositions` inside VERIFY poll at source 6391), `bench.expected` (for `_expected_calls`), stdlib.

---

### A.6 `e2e/bench/bench/expected.py` (~140 LOC)

**Header docstring:** "EXPECTED_CALLS calibration overlay. Tolerance = 0 per Diego 2026-06-02 (was 1 at source 212). Overlay freshness gate refuses to start when `/tmp/snowplow-runs/calibration/expected_calls.json` is older than 14 days; operator must re-run `bench calibrate` or `--allow-stale-overlay` (escape hatch with very loud log)."

**Functions moved:**

| Source line(s) | Function/constant |
|---|---|
| 200–213 | `EXPECTED_CALLS` dict literal + `EXPECTED_CALLS_DEFAULT_USER`, `EXPECTED_CALLS_TOLERANCE`, `EXPECTED_CALLS_OVERLAY_PATH` constants |
| 216–252 | `_load_expected_calls_overlay` |
| 255–268 | `_expected_calls` |
| 3092–3146 | `_gate_expected_calls_calibrated` — overlay-freshness gate (rename to `gate_overlay_freshness`) |
| 8394–8476 | `run_calibrate_expected_calls` |

**New code (Block 2):**
- Change `EXPECTED_CALLS_TOLERANCE` from `1` (source 212) to `0`. Per dispatch's hard constraint #5.
- `overlay_age_seconds() -> float`: stat the overlay; return `math.inf` when missing.
- `overlay_freshness_or_die(max_age_days=14)`: raises `OverlayStale` (new exception) with operator-facing instructions.

**Public `__all__`:** `EXPECTED_CALLS_OVERLAY_PATH`, `EXPECTED_CALLS_TOLERANCE`, `load_expected_calls_overlay`, `expected_calls`, `run_calibrate_expected_calls`, `overlay_freshness_or_die`, `OverlayStale`, `gate_overlay_freshness`.

**Dependencies:** `bench.browser` (for `_browser_login`, `_browser_measure_navigation` used by calibration at source 8434–8460), stdlib.

---

### A.7 `e2e/bench/bench/ledger.py` (~500 LOC)

**Header docstring:** "Canonical ledger row builder + report emission. Mix-weighted = 0.95·cyberjoker + 0.05·admin (feedback_north_star_is_frontend_ux). Schema is FROZEN — see source 7109–7133 docstring."

**Functions moved:**

| Source line(s) | Function |
|---|---|
| 411–415 | `record` |
| 411 | `test_results` global (rename to `_results` and expose via `add_result`/`get_results` accessors so the multi-process stage runs don't share a process-global at import time) |
| 7032–7064 | `_convergence_p99_for_stage` |
| 7065–7108 | `_aggregate_validation` |
| 7109–7338 | `_build_canonical_ledger_row` |
| 7340–7350 | `_load_per_mutation_metric` |
| 7351–7416 | `_compute_verdict` |
| 7417–7426 | `get_runtime_metrics` |
| 8164–8224 | `print_report` |
| 939–942 | `pct(data, p)` (percentile helper) |

**New code (Block 4):**
- `write_run_bundle(run_dir: Path, all_results, *, per_stage_proofs)`: serializes the §F directory tree. Replaces the inline `json.dump` at source 8663–8682.
- `compute_verdict_with_falsifier(...)`: thin wrapper around `_compute_verdict` that adds an explicit "would deleting THIS stage's proof change the verdict?" tag, supporting acceptance criterion (d).

**Public `__all__`:** `record`, `build_canonical_ledger_row`, `print_report`, `compute_verdict`, `aggregate_validation`, `convergence_p99_for_stage`, `pct`, `get_runtime_metrics`, `write_run_bundle`.

**Dependencies:** `bench.cluster` (for `kubectl` in the pod-restart probe at source 7274–7281 and uptime probe at 7286–7293), stdlib only otherwise.

---

### A.8 `e2e/bench/bench/phases.py` (~600 LOC)

**Header docstring:** "Stage runners + STAGE_REGISTRY. Phase 6 (browser scaling) is the ONLY scored phase post-restructure. Phase 7 (user-scaling) and Phase 8 (per-mutation) are retained as separate subcommands. Each stage is a callable that reads `state.json`, runs its work, writes a stage-proof JSON, and updates `state.json`. `--from-stage S5` resumes from disk."

**Functions moved:**

| Source line(s) | Function |
|---|---|
| 7954–8162 | All `_phase8_*` helpers + `run_phase_per_mutation` (kept; called from `bench phase8`) |
| 6086–6240 | `run_phase_browser_comparison` (P5) — **EVALUATE for deletion**; if cyberjoker mix-weighting is needed pre-Phase 6, fold its login-comparison flow into `phases.py:S1`; otherwise DELETE (see §B) |
| 6568–7031 | `run_phase_browser_scaling` (P6) — split into `stage_s1` through `stage_s8` each calling shared helpers `_setup_users`, `_measure_all_users`, `_snapshot_l1`. The current monolithic function (~464 LOC) breaks into ~8 stage functions of ~40–60 LOC each + a ~120 LOC `run_phase6` orchestrator. |

**New code (Block 4):**
- `STAGE_REGISTRY: dict[str, Callable[[StageContext], StageProof]]` — keys `S1`…`S8`, plus `S0_PREFLIGHT`, `S9_REPORT`. Operator selects via `--from-stage`/`--to-stage`.
- `@dataclass StageContext`: holds `tag`, `scale`, `tokens`, `user_pages`, `run_dir`, `state_path`, `cache_mode`.
- `@dataclass StageProof`: holds `stage_id`, `started_at`, `ended_at`, `passed`, `proof_dict` (what would be wrong if this stage is skipped — see §I), `artifacts: list[Path]`.
- `load_state(run_dir) / save_state(run_dir, state)` — JSON round-trip; see §E schema.

**Public `__all__`:** `STAGE_REGISTRY`, `StageContext`, `StageProof`, `run_phase6`, `run_phase7_user_scaling`, `run_phase8_per_mutation`, `load_state`, `save_state`.

**Dependencies:** `bench.cluster`, `bench.lifecycle`, `bench.browser`, `bench.expected`, `bench.ledger`, `bench.storm`.

---

### A.9 `e2e/bench/bench/cli.py` (~350 LOC)

**Header docstring:** "Command-line surface for the harness. Subcommands: check, calibrate, cleanup, storm, converge, measure, phase6, phase7, phase8, report. Every entrypoint goes through `_gke_context_guard()` first per feedback_kubectl_verify_gke_context."

**Functions:**

| New/Source | Function |
|---|---|
| NEW | `main()` — `argparse` subparser wiring; replaces source 8483–8688 |
| NEW | `_gke_context_guard()` — wraps `kubectl config current-context`; refuses on non-`gke_neon-481711_us-central1-a_cluster-1` unless `--allow-non-gke` (hidden flag for kind clusters). Hard exit code 3 on mismatch. |
| NEW | `cmd_check(args)` — the 7-item preflight gate (see §D + §I). 60s budget, NO mutations. |
| NEW | `cmd_calibrate(args)` — wraps `expected.run_calibrate_expected_calls`. |
| NEW | `cmd_cleanup(args)` — wraps `lifecycle.clean_environment` + `assert_clean`. |
| NEW | `cmd_storm(args)` — wraps `storm.crb_delete_burst` etc. |
| NEW | `cmd_converge(args)` — single-stage convergence (calls `phases.STAGE_REGISTRY[args.stage]`). |
| NEW | `cmd_measure(args)` — single-stage measurement without lifecycle mutations. |
| NEW | `cmd_phase6(args)` — runs P6 with `--from-stage`/`--to-stage`. |
| NEW | `cmd_phase7(args)` / `cmd_phase8(args)` |
| NEW | `cmd_report(args)` — reads `state.json` + per-stage proofs, builds ledger row, prints summary. |
| MOVED | `verify_deployed_image` (source 951–984) → `cli.py` (used by `cmd_check` and `cmd_phase6` entry). |

**Public `__all__`:** `main`. Everything else internal.

**Dependencies:** every other bench module.

---

## B. Delete list

Each entry: `<source_line_range> — <symbol or block> — <reason>`. All citations refer to `.claude/worktrees/bench-harness-0.30-prep/e2e/bench/snowplow_test.py`.

### B.1 The 19 self-test functions (delete in entirety; behavioral coverage moves to pytest §C)

**Note — count correction:** Dispatch said "21 `_self_test_*`". TRACED `grep -n "^def _self_test_"` returns **19** definitions (lines 2807, 2848, 2892, 2979, 3148, 3388, 3467, 3631, 3711, 3749, 3790, 3954, 4067, 4142, 4176, 4213, 4252, 4407, 4696). The remaining 20 `_self_test_*` string occurrences (≈39 total - 19 defs) are call sites in `main()` (source 8531–8551) and inspect.getsource probes inside other self-tests. The plan deletes the 19 defs + the dispatch block at 8531–8551 — net behavior identical to the dispatch's "21" intent.

| Lines | Symbol | Reason |
|---|---|---|
| 2807–2846 | `_self_test_destructive_clean_guard` | Use real `pytest.parametrize` table over state dicts |
| 2848–2890 | `_self_test_phase8_wired` | Replace with `test_main_dispatches_phase8` import-level check |
| 2892–2977 | `_self_test_canonical_ledger_row` | Replace with `test_canonical_row_schema_fields_frozen` |
| 2979–3090 | `_self_test_canonical_ledger_row_floor_shape` | Replace with `test_canonical_row_floor_shape_when_cache_unsupported` |
| 3148–3386 | `_self_test_widget_validation` (uses `inspect.getsource(sys.modules[__name__])` at 3106 — anti-pattern poster child) | Replace with direct behavioral test of `_validate_widget_terminal_state` on a mocked page |
| 3388–3465 | `_self_test_phase6_iterates_users` | Replace with `test_phase6_runs_admin_and_cyberjoker` calling a thin fake `_measure_all_users` |
| 3467–3629 | `_self_test_cell_aggregator_filters_zeros` | Replace with `test_cell_stats_drops_waterfall_zero_sentinels` |
| 3631–3709 | `_self_test_wait_for_restaction_steady_state_exists` | Existence check via `assert hasattr` — keep as 1-line pytest |
| 3711–3747 | `_self_test_phase5_runs_both_users` | Phase 5 deleted (see B.2); test deleted |
| 3749–3788 | `_self_test_cache_toggle_uses_helm` | Replace with `test_set_cache_uses_helm_not_kubectl_edit` using `monkeypatch` on `subprocess.run` |
| 3790–3952 | `_self_test_cache_toggle_chart_source_dispatch` | Same; behavioral test |
| 3954–4065 | `_self_test_cache_toggle_graceful_skip` | Same |
| 4067–4140 | `_self_test_chart_supports_cache_toggle` | Same |
| 4142–4174 | `_self_test_phase6_cache_supported_flag` | Replace with `test_phase6_skips_cache_toggle_when_unsupported` |
| 4176–4211 | `_self_test_assert_clean_guard_wiring` | Replace with `test_assert_clean_consults_destructive_guard` (call-trace test) |
| 4213–4250 | `_self_test_password_from_secret` | Replace with `test_read_password_decodes_secret` using `monkeypatch` on `subprocess.run` |
| 4252–4405 | `_self_test_namespace_cascade_delete` | Replace with `test_namespace_cascade_deletes_widget_kinds` (mocked kubectl call log) |
| 4407–4694 | `_self_test_k8s_client_helpers` | Replace with a parametrized `test_k8s_helper_falls_back_to_kubectl_when_client_unavailable` |
| 4696–4769 | `_self_test_hot_path_uses_k8s_client` | Same — behavioral, not source-text-grep |
| 8531–8551 | `--self-test` dispatch block in `main()` | The `--self-test` flag is replaced by `pytest e2e/bench/tests/`. The flag itself is removed; `bench --help` no longer advertises it. |

**Falsifier check (will be run in Block 5):** `grep -r 'inspect.getsource' e2e/bench/` returns 0 results.

### B.2 Pre-Phase-6 legacy phases (delete)

| Lines | Symbol | Reason |
|---|---|---|
| 5086–5108 | `_full_page_render` | Used only by `run_phase_latency` (B.2 deleted). Confirm zero other callers via `grep -n '_full_page_render' snowplow_test.py` (returns only def + 1 call). |
| 5109–5124 | `_bench_endpoints` | Used only by Phase 2; delete with the phase |
| 5125–5144 | `_print_comparison` | Phase-2-only print helper; delete |
| 5146–5354 | `run_phase_latency` | **Phase 2 — NOT on Phase 6 critical path.** Latency mechanism is now measured per-cell in the canonical ledger row (`cells.*.warm_p50_ms`). Keeping the standalone phase doubles the run time without producing a different number. |
| 5356–5383 | `_measure_stage` | Used only by `run_phase_scaling` (B.2 below). Delete with the phase. |
| 5385–5446 | `run_phase_scaling` | **Phase 3 — already skipped at SCALE≥10000 (source 8631–8635). At production scope (SCALE=50000) this code never runs.** Confirms zero customer signal; delete. |
| 5448–5472 | `_browser_wait_for_call_stability` | Phase-4 stability helper. `_browser_measure_navigation` has its own stability poll at source 5916–5934; this duplicate is unused after Phase 4 deletion. Verify via grep before deletion. |
| 5474–5595 | `_browser_collect_metrics` | Phase-4 metrics collector; not used by P5 or P6. Delete. |
| 5597–5672 | `run_phase_browser` | **Phase 4 — browser metrics phase superseded by `_browser_measure_navigation` (called from P6).** Delete. |
| 5580–5595 | `_print_browser_table` | Phase-4 print; delete. |
| 6044–6084 | `_browser_run_navigations` | Used only by `run_phase_browser_comparison` (deletion candidate; see B.3). |
| 6086–6240 | `run_phase_browser_comparison` | **Phase 5 — pre-Phase-6 stop-gap that ran cache OFF vs cache ON in admin+cyberjoker. Phase 6 already runs both subjects across both cache modes at S1-S8. Delete.** |

Phases 7 and 8 (`run_phase_user_scaling` 7697–7951, `run_phase_per_mutation` 8044–8162) **remain**; they exercise distinct mechanisms (warmup-after-restart, per-mutation convergence by class) not duplicated by P6.

### B.3 Half-converted subprocess→k8s-client paths

| Lines | Function | Reason |
|---|---|---|
| 1167–1191 | `_chart_supports_cache_toggle` — uses `kubectl get cm` then parses YAML by string (TRACED). | Already migrated; **keep** but mark as kubectl-only (chart manifest read; k8s client adds no win for a one-shot). No deletion. |
| 1577–1703 | `delete_all_compositions` — calls `k8s_bulk_*` AND falls back to kubectl when k8s lib unavailable | **Keep** — fallback is intentional (source 562–567). |
| 1705–1832 | `delete_all_compositiondefinitions` — same pattern | **Keep** — same rationale. |

No half-converted paths to actually delete. The audit's earlier "half-converted" claim was inaccurate per re-trace 2026-06-02; the fallback is by design.

### B.4 Convergence-concept duplication

| Lines | Symbol | Reason |
|---|---|---|
| 1074–1097 | `wait_for_l1_quiescent(stable_secs=15, timeout=180)` | Silent-skip on timeout (returns `False` and logs warning at 1096; no exception). The dispatch's hard constraint #8 requires ONE convergence concept. `_browser_measure_stage`'s VERIFY poll (6396–6463) is the SOLE convergence proof. **DELETE.** |
| 6678–6682 | `_stabilize(before_ts, quiesce=False, quiesce_secs=15)` nested helper | Replace with a 5s `time.sleep` inline; this nested function's `quiesce*` parameters are unused after the call-site rewrites below (the function body itself only uses `time.sleep(5)` regardless of args — verify TRACED at source 6681–6682). |
| 6691, 6697, 6710, 6724, 6745, 6837, 6864 | All `_stabilize(ts, ...)` call sites | Rewrite to `time.sleep(5)` (cache ON) or zero-sleep (cache OFF). The VERIFY poll handles convergence. |

### B.5 Other dead/stale code

| Lines | Symbol | Reason |
|---|---|---|
| 1041–1072 | `wait_for_l1_ready` | Used by Phase 1 only (`run_phase_functional`). Phase 1 is retained but Phase 1's L1 sentinel check is replaced by the VERIFY poll. Verify zero callers after Phase 4/5 deletion via grep; if zero, delete. INFERRED — call-graph not exhaustively traced yet; Block 1 will re-verify. |
| 1034–1039 | `_read_l1_ready_ts` | Same — verify call sites. |
| 1000–1032 | `wait_for_l1_warmup(timeout=300)` | Called from `main` at source 8623 and `run_phase_latency` at 5150. With Phase 2 deleted and P6 driving its own readiness via login+nav, this becomes obsolete. **DELETE** post-Block 4. |
| 7349–7350 | `_load_per_mutation_metric` reads `/tmp/snowplow_per_mutation_results.json` (hardcoded path) | Move to `bench.ledger`; replace with run-bundle-relative path (`run_dir / "phase8" / "per_mutation_results.json"`). No deletion; relocation. |
| 8663–8682 | inline `json.dump` to `/tmp/snowplow_test_results.json` and pod-log capture | Move into `ledger.write_run_bundle`. No deletion. |

**Final delete tally:** ~21 self-tests (≈2,000 LOC) + Phase 2 (≈220 LOC) + Phase 3 (≈90 LOC) + Phase 4 (≈220 LOC) + Phase 5 (≈350 LOC) + `wait_for_l1_quiescent` + stabilize sites (≈25 LOC) ≈ **2,905 LOC removed**. Source 8,691 → target ≈ 5,786 LOC pre-restructure; after modularization + dead-helper removal target is **3,200–3,800 LOC** across 8 modules.

---

## C. Pytest skeleton

Tests live in `e2e/bench/tests/`. Each module gets a peer test file. `pytest.ini` config: `testpaths = tests`, `addopts = -ra -q`, `python_files = test_*.py`. Goal: **all behavioral tests under 30s total**, no `inspect.getsource`, no live cluster contact (the harness's measurement IS the cluster contact; tests cover the harness itself).

### C.1 `tests/test_cluster.py` (≈10 cases)

- `test_kubectl_returns_tuple_on_success`
- `test_kubectl_timeout_propagates_as_returncode_1`
- `test_k8s_init_falls_back_when_lib_missing` — monkeypatch `_K8S_LIB_AVAILABLE = False`
- `test_k8s_init_lazy_one_shot_after_failure` — verifies `_k8s_init_attempted` short-circuit at source 605–608
- `test_k8s_bulk_delete_clusterscope_404_is_success` — assert NotFound swallowed
- `test_split_gvr_parses_apiversion`
- `test_count_compositions_uses_k8s_client_when_available` — call-trace via mock
- `test_count_compositions_falls_back_to_kubectl_when_client_unavailable`
- `test_force_finalize_namespace_uses_finalize_subresource`
- `test_composition_yaml_round_trips_through_yaml_safe_load` — sanity, not a self-grep

### C.2 `tests/test_lifecycle.py` (≈14 cases)

**6 highest-value behavioral cases (replacing the deleted self-tests; modelled on tester's `test_widget_validator_flags_stuck_skeleton`):**

1. `test_destructive_guard_blocks_at_phase6_baseline` — parametrized over (compositions, bench_ns) → guard verdict; matches source 2807–2846 intent but as data table, not inspect-getsource.
2. `test_destructive_guard_passes_with_allow_destructive_override` — same fn with `allow_destructive=True`.
3. `test_set_cache_uses_helm_not_kubectl_edit` — monkeypatch `subprocess.run`, assert the recorded argv contains `helm upgrade` and never `kubectl edit deploy`.
4. `test_cache_toggle_dispatches_to_correct_chart_source` — parametrized over chart source (braghettos vs upstream); replaces source 3790–3952.
5. `test_cache_toggle_graceful_skip_when_unsupported` — replaces source 3954–4065.
6. `test_namespace_cascade_deletes_all_widget_kinds` — mock the kubectl call log; assert every entry in `WIDGET_KINDS` (source 350–375) was deleted before the namespace force-finalize.

Plus:
- `test_assert_clean_consults_destructive_guard` — call-trace test
- `test_enable_cache_no_op_when_already_enabled` — monkeypatch helm reply
- `test_create_bench_namespaces_uses_k8s_client_at_scale`
- `test_wait_for_compositions_returns_true_on_steady_state`
- `test_deep_clean_bench_namespace_deletes_widget_crs_before_finalize`
- `test_synthetic_user_passwords_decode_from_secrets`

### C.3 `tests/test_storm.py` (≈4 cases)

- `test_crb_burst_emits_lint_for_uaf_per_request` — mock pod logs
- `test_user_scaling_phase_creates_then_logs_in_synthetic_users`
- `test_pod_logs_since_returns_window` — kubectl monkeypatch
- `test_crb_delete_burst_skips_when_no_targets`

### C.4 `tests/test_browser.py` (≈12 cases)

- `test_widget_validator_flags_stuck_skeleton` (tester's pattern; this is THE reference test)
- `test_widget_validator_passes_when_skeleton_count_zero`
- `test_widget_validator_excludes_drawer_skeletons` — covers source 5730–5759 overlay exclusion
- `test_widget_validator_call_count_within_tolerance_zero` — verifies new EXPECTED_CALLS_TOLERANCE=0
- `test_widget_validator_fails_on_call_count_mismatch`
- `test_widget_validator_uses_per_user_expected_table` — covers source 5837–5841
- `test_browser_measure_navigation_marks_incomplete_when_call_count_below_min` — source 6001–6003
- `test_browser_measure_navigation_marks_incomplete_on_terminal_state_fail` — source 6009–6011
- `test_browser_measure_stage_raises_ConvergenceTimeout` — NEW behavior; the deadline hit at source 6396 with `matched=False` must raise, not log. **Implementation MUST be `with pytest.raises(ConvergenceTimeout): browser_measure_stage(...)`** — log-string assertions are forbidden here (an upstream `except:` swallowing the exception would still let a log-string test pass). Per tightening #5.
- `test_browser_measure_stage_passes_when_api_equals_ui_equals_cluster`
- `test_browser_measure_stage_cyber_uses_intra_user_consistency` — source 6454–6462
- `test_record_video_to_gif_invokes_ffmpeg_once` — monkeypatch subprocess; assert single call

### C.5 `tests/test_expected.py` (≈5 cases)

- `test_overlay_freshness_or_die_passes_on_fresh_file`
- `test_overlay_freshness_or_die_raises_when_overlay_older_than_14_days`
- `test_overlay_freshness_or_die_raises_when_overlay_missing`
- `test_load_overlay_merges_over_hardcoded_defaults` — source 216–252
- `test_expected_calls_tolerance_is_zero` — guard against drift; the constant lives in this module and tests pin it

### C.6 `tests/test_ledger.py` (≈10 cases)

- `test_canonical_row_schema_fields_frozen` — replaces source 2892–2977; checks the literal key list
- `test_canonical_row_floor_shape_when_cache_unsupported` — replaces 2979–3090
- `test_cell_stats_drops_waterfall_zero_sentinels` — replaces 3467–3629
- `test_cell_stats_returns_none_when_all_invalid` — source 7185–7193
- `test_mix_weighted_is_095_cyber_plus_005_admin`
- `test_mix_weighted_returns_none_when_no_samples_in_either_cell`
- `test_verdict_is_INVALID_when_any_cell_all_invalid`
- `test_pod_restart_count_zero_when_kubectl_unavailable`
- `test_print_report_returns_true_when_all_passed`
- `test_per_mutation_metric_loads_from_run_dir_relative_path` — covers Block-4 path migration

### C.7 `tests/test_phases.py` (≈8 cases)

- `test_stage_registry_contains_S0_through_S9`
- `test_from_stage_S5_skips_S1_S2_S3_S4`
- `test_to_stage_S3_stops_after_S3`
- `test_stage_S2_writes_proof_with_ns_count_and_compdef_present`
- `test_stage_proof_records_what_would_be_wrong_if_skipped` — covers acceptance criterion (Risk Register §I block 4)
- `test_phase6_runs_admin_and_cyberjoker` — replaces source 3388–3465
- `test_phase6_skips_cache_toggle_when_unsupported` — replaces 4142–4174
- `test_run_state_resume_uses_disk_state_when_present`

### C.8 `tests/test_cli.py` (≈9 cases)

- `test_gke_context_guard_exits_3_on_non_gke_context`
- `test_gke_context_guard_passes_on_neon_481711_cluster`
- `test_gke_context_guard_allow_non_gke_flag_bypasses` — explicit escape hatch for kind clusters
- `test_check_command_exits_2_with_named_failures` — acceptance (e)
- `test_check_command_passes_all_7_gates`
- `test_calibrate_command_writes_overlay_then_exits_0`
- `test_phase6_from_stage_S5_resumes_from_disk_state` — acceptance (f)
- `test_phase6_aborts_when_overlay_stale` — acceptance (g)
- `test_overlay_stale_message_points_to_bench_calibrate`

### C.9 `conftest.py` (≈80 LOC)

Shared fixtures:
- `mock_kubectl(monkeypatch)` — installs a recording `subprocess.run` and yields the call log.
- `fake_page()` — minimal Playwright Page stand-in: `.evaluate`, `.goto`, `.locator(...).count`, `.url`, `.wait_for_timeout`. Implemented with `unittest.mock.MagicMock` plus a small `evaluate` dispatcher keyed on JS prefix.
- `tmp_run_dir(tmp_path)` — yields a clean `/tmp/snowplow-runs/{tag}/run-{ts}/`-shaped path.
- `fresh_overlay_file(tmp_path)` / `stale_overlay_file(tmp_path)` — controls overlay mtime.

---

## D. CLI surface

### D.1 `python -m bench --help`

```
usage: bench [-h] [--allow-non-gke] [--tag TAG] [--scale SCALE]
             {check,calibrate,cleanup,storm,converge,measure,phase6,phase7,phase8,report} ...

Snowplow bench harness (Path B, 2026-06-02). Drives Playwright + cluster
mutations + canonical ledger emission against the GKE benchmark cluster.

positional arguments:
  {check,calibrate,cleanup,storm,converge,measure,phase6,phase7,phase8,report}
    check       Preflight (7 gates, 60s, no mutations).
    calibrate   Refresh EXPECTED_CALLS overlay against the live cluster.
    cleanup     clean_environment + assert_clean (consults destructive guard).
    storm       Disruptive scenarios: crb-delete, deploy-burst.
    converge    Run a single Phase 6 stage with the VERIFY poll only.
    measure     Single-stage measurement (no lifecycle mutations).
    phase6      Browser scaling (S1-S8) with --from-stage / --to-stage.
    phase7      User scaling (synthetic users, warmup-after-restart).
    phase8      Per-mutation convergence (HOT/WARM/COLD classes).
    report      Read run-state + emit canonical ledger row + print summary.

options:
  -h, --help        show this help message and exit
  --allow-non-gke   bypass the GKE context guard (kind/minikube only).
  --tag TAG         image tag under test (default: $EXPECTED_IMAGE_TAG).
  --scale SCALE     target composition count (default: $SCALE or 5000).

ENV: EXPECTED_IMAGE_TAG, SCALE, FRONTEND_URL, SNOWPLOW_URL, AUTHN_URL.
```

### D.2 `python -m bench phase6 --help`

```
usage: bench phase6 [-h] [--from-stage S{0..9}] [--to-stage S{0..9}]
                    [--cache-mode {ON,OFF,BOTH}] [--users admin,cyberjoker]
                    [--video {none,representative,all}]
                    [--run-dir PATH] [--allow-stale-overlay]

Phase 6 — Browser Scaling (S1: zero state → S6: SCALE compositions →
S7: delete 1 comp → S8: delete 1 ns). Writes per-stage proofs to
$run_dir/proofs/SN.json and a canonical ledger row to
$run_dir/ledger_row.json.

options:
  -h, --help            show this help message and exit
  --from-stage S        resume from this stage; reads $run_dir/state.json.
                        (default: S0)
  --to-stage S          stop after this stage (inclusive). (default: S9)
  --cache-mode MODE     ON | OFF | BOTH (default: BOTH; runs OFF then ON).
  --users U,V           comma-separated subjects (default: admin,cyberjoker).
  --video MODE          none | representative | all
                        (default: representative — n=1 per cell per nav).
  --run-dir PATH        output bundle root (default:
                        /tmp/snowplow-runs/{tag}/run-{ts}/).
  --allow-stale-overlay bypass overlay-freshness gate (NOT recommended;
                        emits a LOUD warning to the ledger row).

EXIT CODES:
  0 ledger row PASS
  1 ledger row FAIL or INVALID
  2 preflight gate failed (named gates listed on stderr)
  3 GKE context mismatch
  4 ConvergenceTimeout raised by a stage
```

### D.3 Common operator flows

```
# 1. Pre-flight gate before any work
python -m bench check --tag 0.30.232

# 2. Refresh the EXPECTED_CALLS overlay (run when overlay >14d old)
python -m bench calibrate --tag 0.30.232

# 3. Full Phase 6 from a clean cluster
python -m bench phase6 --tag 0.30.232 --scale 50000

# 4. Resume Phase 6 from S5 after a crash mid-S6
python -m bench phase6 --tag 0.30.232 --scale 50000 \
    --from-stage S5 --run-dir /tmp/snowplow-runs/0.30.232/run-20260602-1530

# 5. Single-stage smoke (S1 only, no SCALE deploy)
python -m bench phase6 --tag 0.30.232 --to-stage S1 --video none
```

---

## E. `state.json` schema + stage registry

### E.1 `STAGE_REGISTRY`

```python
# bench/phases.py
STAGE_REGISTRY: dict[str, Callable[[StageContext], StageProof]] = {
    "S0": stage_preflight,            # GKE context, image, overlay freshness
    "S1": stage_s1_zero_state,        # baseline measurement
    "S2": stage_s2_one_ns_compdef,
    "S3": stage_s3_20_ns,
    "S4": stage_s4_20_compositions,
    "S5": stage_s5_scale_ns,          # 50 ns at SCALE≥10000, 500 ns at 5K
    "S6": stage_s6_scale_compositions,
    "S7": stage_s7_delete_one_comp,
    "S8": stage_s8_delete_one_ns,
    "S9": stage_report,               # build canonical ledger row + emit
}
```

### E.2 `state.json` schema

Lives at `$run_dir/state.json`. JSON-Schema fragment:

**Schema freeze marker (per tightening #8)**: `state.json` carries a `schema_version` field set to `"1.0.0"`. Frozen at Block 4 start. **Bumping the version requires a fresh PM gate** (any backward-incompatible field add/rename/remove is a 1.x → 2.0 bump). `bench.phases.load_state` refuses to read a state.json whose `schema_version` is missing OR whose major version exceeds the harness's supported version (`SCHEMA_MAJOR = 1`); raises `IncompatibleStateSchema`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["schema_version", "tag", "scale", "started_at",
               "stages_completed", "current_stage"],
  "properties": {
    "schema_version":    {"type": "string", "const": "1.0.0",
                          "description": "FROZEN at Block 4 start (2026-06-02). Bump requires PM gate."},
    "tag":               {"type": "string", "examples": ["0.30.232"]},
    "scale":             {"type": "integer", "minimum": 1},
    "started_at":        {"type": "string", "format": "date-time"},
    "last_updated_at":   {"type": "string", "format": "date-time"},
    "stages_completed":  {"type": "array", "items": {"type": "string",
                          "pattern": "^S[0-9]$"}},
    "current_stage":     {"type": "string", "pattern": "^S[0-9]$"},
    "cache_mode":        {"type": "string", "enum": ["ON", "OFF"]},
    "users":             {"type": "array", "items": {"type": "string"}},
    "cluster_snapshot": {
      "type": "object",
      "properties": {
        "bench_ns_count":    {"type": "integer"},
        "composition_count": {"type": "integer"},
        "snowplow_pod_uid":  {"type": "string"},
        "snowplow_restarts": {"type": "integer"}
      }
    },
    "stage_proofs": {
      "type": "object",
      "additionalProperties": {
        "$ref": "#/definitions/StageProof"
      }
    },
    "overlay_age_days_at_start": {"type": "number"}
  },
  "definitions": {
    "StageProof": {
      "type": "object",
      "required": ["stage_id", "started_at", "ended_at", "passed",
                   "proof", "artifacts", "what_breaks_if_skipped"],
      "properties": {
        "stage_id":  {"type": "string"},
        "started_at": {"type": "string"},
        "ended_at":  {"type": "string"},
        "passed":    {"type": "boolean"},
        "proof": {
          "type": "object",
          "description": "stage-specific assertions: composition_count, ns_count, convergence_ms, content_match, etc."
        },
        "artifacts": {
          "type": "array",
          "items": {"type": "string", "description": "path relative to $run_dir"}
        },
        "what_breaks_if_skipped": {
          "type": "string",
          "description": "free-text: what assumption later stages make that this proof underwrites. Required by Risk Register §I, Block 4."
        },
        "convergence_ms":           {"type": ["integer", "null"]},
        "navs": {
          "type": "array",
          "items": {"$ref": "#/definitions/NavEntry"}
        }
      }
    },
    "NavEntry": {
      "type": "object",
      "properties": {
        "user":         {"type": "string"},
        "page_path":    {"type": "string"},
        "nav_num":      {"type": "integer"},
        "cold_warm":    {"type": "string", "enum": ["COLD", "WARM"]},
        "waterfall_ms": {"type": "integer"},
        "call_count":   {"type": "integer"},
        "validation":   {"type": "object"}
      }
    }
  }
}
```

### E.3 `--from-stage` / `--to-stage` semantics

- `phases.py:run_phase6` calls `load_state(run_dir)`:
  - If `state.json` missing → fresh run; default `--from-stage S0`.
  - If present and `args.from_stage` set, validate that every stage in `state.json.stages_completed` precedes `args.from_stage` in the registry order. If not, abort: "state.json contains stages beyond requested --from-stage; pass a different --run-dir or remove the file."
- Each stage executes its callable, gets a `StageProof`, appends to `state.json.stages_completed`, writes the proof to `$run_dir/proofs/{stage_id}.json`, and updates `state.json.current_stage`.
- On exception (including `ConvergenceTimeout`): `state.json.current_stage` keeps the failing stage's ID, exit code 4 (convergence) or 1 (other). Operator can re-run `--from-stage <failing>` after diagnosing.

---

## F. Output bundle schema

```
/tmp/snowplow-runs/{tag}/run-{ts}/
├── state.json                          # see §E.2
├── ledger_row.json                     # see §F.1
├── per_stage.json                      # aggregated proofs (one row per stage)
├── expected_calls.json                 # overlay snapshot used by this run
├── overlay_freshness.json              # {age_days, source_mtime_utc, accepted_by}
├── pod_logs/
│   ├── full_run.txt                    # kubectl logs --since=$run_duration
│   ├── S6_around_deploy.txt            # captured per-stage when an anomaly fires
│   └── S8_around_delete.txt
├── proofs/
│   ├── S0_preflight.json
│   ├── S1_zero_state.json
│   ├── S2_one_ns_compdef.json
│   ├── S3_20_ns.json
│   ├── S4_20_compositions.json
│   ├── S5_scale_ns.json
│   ├── S6_scale_compositions.json
│   ├── S7_delete_one_comp.json
│   └── S8_delete_one_ns.json
├── videos/
│   ├── S1_admin_cold_dashboard.webm
│   ├── S1_admin_cold_dashboard.gif
│   ├── S6_cyber_warm_compositions.webm
│   └── ...                             # naming: S{N}_{user}_{cold|warm}_{page}.{webm,gif}
├── screenshots/                        # opt-in via $SCREENSHOTS=1
│   └── S{N}_{cache_mode}_{user}_poll{P}_api{A}_ui{U}_cluster{C}.png
└── summary.json                        # see §F.2
```

### F.1 `ledger_row.json` — full key list

Schema frozen at source 7109–7337. The full key list (TRACED):

```
tag
ship_date
scale                                       # [SCALE, 5000] or [5000]
uptime_at_capture_s
cells.admin_on   .{cold_ms, warm_p50_ms, warm_p99_ms,
                  valid_nav_count, invalid_nav_count, terminal_fail_rate}
cells.admin_off  .{...}
cells.cyber_on   .{...}
cells.cyber_off  .{...}
mix_weighted     .{cold_ms, warm_p50_ms, warm_p99_ms}
convergence_mass_s6_p99
convergence_mass_s7_p99
convergence_mass_s8_p99
convergence_per_mutation_p99_mix            # null when phase8 did not run
convergence_per_class_hot_p99               # null when phase8 did not run
convergence_per_class_warm_p99              # null when phase8 did not run
convergence_per_class_cold_p99              # null when phase8 did not run
tag_specific_verifications                  # dict, may be empty
pod_restart_count
validation.navs_terminal_pass
validation.navs_terminal_fail
validation.skeleton_failures
validation.errored_widgets_total
validation.call_count_mismatches
verdict                                     # PASS | FAIL | FLOOR | INVALID
```

### F.2 `summary.json` — PASS vs FAIL

Always present. Keys identical for PASS and FAIL; verdict + named-gate failures distinguish them.

```json
{
  "verdict": "PASS|FAIL|INVALID|FLOOR",
  "mix_weighted": {...},
  "tag": "0.30.232",
  "scale": 50000,
  "run_dir": "/tmp/snowplow-runs/0.30.232/run-20260602-1530",
  "stages_completed": ["S0","S1","S2","S3","S4","S5","S6","S7","S8","S9"],
  "duration_s": 5234,
  "pod_restarts": 0,
  "convergence_p99_s": {"s6": 67.2, "s7": 12.4, "s8": 9.1},
  "failed_gates": [],                  // populated only when verdict != PASS
  "convergence_timeout_stage": null,   // populated only when verdict==INVALID due to timeout
  "ledger_row_path": "ledger_row.json"
}
```

### F.3 `per_stage.json`

One JSON array — aggregated copy of every `proofs/SN.json` for grep convenience. Same schema as the individual stage proof.

### F.4 `expected_calls.json`

A byte-for-byte snapshot of `/tmp/snowplow-runs/calibration/expected_calls.json` as it was at run start. Lets the ledger row prove "this is the gate I was held to."

### F.5 `pod_logs/`

- `full_run.txt`: captured at S9. Single kubectl call. Replaces source 8670–8682.
- Per-stage logs: captured on demand by stages that detect anomalies (terminal_fail>0, convergence_ms>30s, pod_restart>0). Stage proof records the path it wrote.

### F.6 `videos/`

Playwright `record_video_dir=videos/`. Naming convention: each browser context is created with `make_browser_context(pw, video_dir=run_dir/'videos', user_label=f"S{n}_{user}_{cold_warm}_{page}")`. The .gif is created by `record_video_to_gif` from the resulting .webm; the .webm is kept (≤500 KB each) and the .gif is the shareable artifact.

---

## G. Implementation order

Five dev work-blocks. Each block's "exit criteria" is the gate dev hits before moving on.

### G.0 Preamble — three structural costs (added v1.2)

Block 1 actuals ran +142% over the v1.1 target; Block 2 actuals ran +183%. Two consecutive overshoots are a plan defect, not a dev defect. Block 2 dev's split-report identified three structural costs that the v1.1 estimates undercounted. **Block 3 and Block 4 LOC targets are scaled ×2 from v1.1 to absorb these costs empirically**; the costs themselves remain:

1. **Deferred-import shims** — Each future-block module that a current block calls into needs a forward-declaration shim to avoid circular imports during partial migration. Empirically: **~10 wrappers per module × ~6 LOC each = ~60 LOC per module touched by the block**. Block 3 touches `bench.browser` (uses cluster, expected); Block 4 touches `bench.phases`, `bench.ledger`, `bench.cli` (each consumes all earlier modules). Shims disappear in Block 5 when the worktree file is deleted and imports collapse.

2. **Test surface — realistic per-case cost** — §C lists bare case-name counts (e.g. "≥12 cases" for `tests/test_browser.py`). The realistic per-case implementation cost is **~25 LOC** (3–5 LOC test body + 15–20 LOC monkey-patch / fixture / parametrize / fake-page setup), not the ~10 LOC the v1.1 targets implicitly assumed. A 12-case test file is ~300 LOC, not ~120 LOC. §C case counts remain authoritative; this preamble documents the inflation factor so dev's LOC stays inside band.

3. **Operator-facing `(bool, str)` diagnostics** — Acceptance criterion (e) requires `bench check` to exit 2 with **named gate failures on stderr**. Every gate function therefore returns `(passed: bool, diag: str)` rather than `bool`, and every named-gate site has a short error-formatter. Empirically: **~8–12 LOC per gate × 7 gates = ~70 LOC** in Block 2; Block 4 adds the same pattern for stage-resume validation and convergence-timeout messages (~120 LOC). Not optional — without this, criterion (e) fails.

### G.0.1 Block 1+2 actuals — empirical anchor for Block 3+4 targets

| Block | v1.1 plan | Actual total | Overshoot | Code-LOC | Tests LOC |
|---|---|---|---|---|---|
| Block 1 | +1,250 | +3,024 | +142% | +1,620 | (folded into total; tests ≈ +1,404) |
| Block 2 | +750   | +2,120 | +183% | +1,557 | +556 |

Heuristic for Blocks 3 + 4: **code-LOC ×2 of v1.1**, **tests sized to §C case-count × 25 LOC realistic-per-case**. PM gate stricter: "within band" replaces "match target." If a block's actual code-LOC sits inside the refreshed band, the PM gate does NOT re-litigate — see §H PM-gate template question.

### Block 1 — Skeleton + cluster.py + lifecycle.py + tests/ scaffolding (Day 1-2)

**Files touched:**
- create `e2e/bench/bench/__init__.py`, `bench/cluster.py`, `bench/lifecycle.py`
- create `e2e/bench/tests/__init__.py`, `tests/conftest.py`, `tests/test_cluster.py`, `tests/test_lifecycle.py`
- update `e2e/bench/requirements.txt` (add `pytest`, `pytest-cov`, ensure `kubernetes`, `playwright` already present)

**Functions moved:** §A.2 + §A.3 line ranges. ~1,150 LOC moved (out of the worktree's `snowplow_test.py`), zero deleted yet.

**LOC delta target:** +1,250 (new module files + tests + scaffolding); the worktree file remains untouched in Block 1. Block 5 deletes it.

**Exit criteria:**
- `pytest e2e/bench/tests/test_cluster.py e2e/bench/tests/test_lifecycle.py` passes (≥24 cases per §C).
- `python -c "from bench.cluster import kubectl; from bench.lifecycle import clean_environment"` imports cleanly.
- No callers of `bench.cluster` or `bench.lifecycle` outside the new module tree.

### Block 2 — storm.py + expected.py + preflight check (Day 2-3)

**Files touched:**
- create `bench/storm.py`, `bench/expected.py`, partial `bench/cli.py` (only `cmd_check` and `_gke_context_guard`)
- create `tests/test_storm.py`, `tests/test_expected.py`, `tests/test_cli_check.py`

**Functions moved:** §A.4 + §A.6 line ranges + the `verify_deployed_image` (source 951–984) into `cli.py`. New: `_gke_context_guard`, `overlay_freshness_or_die`, `cmd_check` (7-item gate).

**LOC delta target:** +750 (modules + tests). EXPECTED_CALLS_TOLERANCE flips from `1` to `0` here.

**Exit criteria:**
- `python -m bench check --tag 0.30.232` returns 0 on the canonical GKE cluster within 60s, returns 2 with named gate failures otherwise, returns 3 on non-GKE context.
- All §C.3 + §C.5 + §C.8 (check-related) tests pass.
- `grep -r 'EXPECTED_CALLS_TOLERANCE' e2e/bench/bench/` returns exactly one definition, value `0`.

**The 7 preflight gates** (`cmd_check`):
1. **GKE context** == `gke_neon-481711_us-central1-a_cluster-1` (unless `--allow-non-gke`).
2. **snowplow pod ready** in `krateo-system` (1/1 containers ready, restart_count fresh).
3. **image tag match** — `verify_deployed_image()` succeeds (source 951–984 logic, exit 1 → gate fail).
4. **CRDs present** — `wait_for_crd(timeout=10)` succeeds on all `WIDGET_KINDS` + composition CRD.
5. **helm release lockstep** — `helm get values snowplow -n krateo-system -o json` returns the same image tag as the deployment (catches OBS-1 / 0.30.186-style drift; feedback_chart_release_lockstep).
6. **frontend LB reachable** — `urllib.request.urlopen(FRONTEND + "/login", timeout=5)` returns HTTP 200.
7. **overlay freshness** — `overlay_freshness_or_die(14)` succeeds or `--allow-stale-overlay` set (with loud-log).

### Block 3 — browser.py + verify-poll + video→gif (Day 3-4)

**Files touched:**
- create `bench/browser.py` with the `ConvergenceTimeout` exception, the `record_video_to_gif` helper, `make_browser_context`.
- create `tests/test_browser.py`.

**Functions moved:** §A.5 line ranges.

**Critical refactor at source 6465–6475:** today, when the VERIFY poll's deadline expires with `matched=False`, the code only logs `TIMEOUT/MISMATCH` and writes `convergence_ms=-1` into the result dict (source 6468). The caller (`_measure_all_users` at 6661–6672) appends the result and proceeds. **Replace this with `raise ConvergenceTimeout(stage=stage_num, user=user, api=api_count, ui=ui_count, cluster=fresh_comp_count)`.** The stage runner catches it, writes a stage proof with `passed=False`, and re-raises to abort the run with exit 4.

**LOC delta target (refreshed v1.2 against Block 1+2 anchor):**
- **code-LOC +2,200** (browser.py ~1,400 production + deferred-import shims to cluster/expected ~60 + `ConvergenceTimeout` + `record_video_to_gif` + `make_browser_context` + per-call response wiring)
- **tests +600** (≥12 cases × ~25 LOC realistic-per-case + Playwright fake-page fixture in `conftest.py` extension + `pytest.raises` integration scaffolding for criterion (h))
- **total +2,800 to +3,200**

If actual lands inside this band, PM gate does NOT re-litigate (see §H PM-gate template question). v1.1 said +1,100; that was the undercount that drove the Block 1+2 overshoots.

**Exit criteria (v1.3 — live smoke-gate descoped to Block 4):**
- `pytest e2e/bench/tests/test_browser.py` passes (≥12 cases per §C.4).
- Block 3 ships `make_browser_context` + `record_video_to_gif` exercised end-to-end via pytest (`test_smoke_video_pipeline` against fake Playwright + stubbed ffmpeg). The LIVE-cluster smoke run `python -m bench phase6 --to-stage S1 --video representative --tag 0.30.232` is **reassigned to Block 4 exit criteria** (phases.py + state.json + cli wired in Block 4). Reason: the live `phase6 --to-stage S1` invocation is structurally impossible pre-Block-4, because `bench.phases` and `bench.cli`'s `cmd_phase6` ship in Block 4.
- `ConvergenceTimeout` is raised cleanly on an intentionally-broken stage (test-side, not live).

### Block 4 — phases.py + state.json + --from-stage + ledger.py (Day 4-5)

**Files touched:**
- create `bench/phases.py`, `bench/ledger.py`, finish `bench/cli.py`.
- create `tests/test_phases.py`, `tests/test_ledger.py`, finish `tests/test_cli.py`.

**Functions moved:** §A.7 + §A.8 line ranges; deletion of source 6678–6682 `_stabilize` and rewrite of its 7 call sites (6691, 6697, 6710, 6724, 6745, 6837, 6864) to inline `time.sleep(5)`.

**LOC delta target (refreshed v1.2 against Block 1+2 anchor):**
- **code-LOC +2,800** (phases.py ~1,000 with 10 stage functions + STAGE_REGISTRY + StageContext/StageProof dataclasses + state.json round-trip + proof-validation re-runner per R4.1; ledger.py ~700 with builder + write_run_bundle + ledger_row.schema.json emitter for criterion (b) + baseline-comparator for criterion (c) + bundle-truncate logic per R3.1; cli.py ~600 finish covering all 10 subcommands + IncompatibleStateSchema + per-gate `(bool, str)` diagnostics; deferred-import shims across three modules ~180; _stabilize rewrite ~25 LOC delta)
- **tests +900** (test_phases ~8 cases, test_ledger ~10 cases, test_cli ~9 cases, plus state.json fixtures + ledger_row.schema.json round-trip test + mock north-star ledger reader for criterion (c) — all × ~25 LOC realistic-per-case)
- **total +3,700 to +4,200**

If actual lands inside this band, PM gate does NOT re-litigate (see §H PM-gate template question). v1.1 said +1,400; same undercount pattern as Block 3. `snowplow_test.py` shrinks proportionally as functions move; Block 5 will delete it entirely.

**v1.3 note**: Block 3 actuals (+2,163 / under-band by 637 LOC) tracked plan §A.5 to within 4% per line-item. Block 4 may follow the same precision profile; PM gate should accept under-band actuals when each §A.7 + §A.8 + §A.9 line-item is empirically tracked. Do NOT pre-emptively widen or narrow the band — let Block 4 dev produce empirical data and re-evaluate post-pre-commit.

**Exit criteria (v1.3 — inherits live smoke-gate from Block 3):**
- `pytest e2e/bench/tests/` passes — total < 30s wall-clock.
- **Inherited from Block 3 (per v1.3 descope)**: live-cluster smoke run `python -m bench phase6 --to-stage S1 --video representative --tag 0.30.232` produces:
  - `videos/S1_admin_cold_dashboard.webm` (any size)
  - `videos/S1_admin_cold_dashboard.gif` (≤2 MB at 4 fps × ≤60s)
- **Block 4 native**: `phases.py` wires the catch around `browser_measure_stage` → on `ConvergenceTimeout`, persists stage proof with `passed=False, convergence_timeout=true` → re-raises → process exits 4. Verified by an intentionally-broken stage smoke (e.g. `SCALE` set to a value the cluster cannot reach inside the verify deadline) returning exit code 4 with the proof on disk.
- `python -m bench phase6 --tag 0.30.232 --to-stage S2` produces:
  - `state.json` with `stages_completed: ["S0","S1","S2"]`
  - `proofs/S1_zero_state.json` and `proofs/S2_one_ns_compdef.json`
  - each proof has a non-empty `what_breaks_if_skipped` field
- `python -m bench phase6 --from-stage S2 --run-dir <above>` reads existing `state.json`, skips S0/S1, runs S2+.
- `grep -r 'wait_for_l1_quiescent\b' e2e/bench/` returns 0 hits.

### Block 5 — pytest sweep + delete old file + acceptance run + memory updates (Day 5-6)

**Files touched:**
- **CREATE rollback safety branch first (per tightening #7)**: before any delete,
  `git branch legacy-bench-2026-06-02 HEAD` — tag the current HEAD as the rollback point. The worktree file `e2e/bench/snowplow_test.py` is reachable from this branch even after the main-tree delete. **Recovery path if acceptance regresses**:
  `git checkout legacy-bench-2026-06-02 -- e2e/bench/snowplow_test.py` restores the legacy file at the main-tree path; the new package modules can coexist and the operator falls back to `python3 e2e/bench/snowplow_test.py` while diagnosing.
- **Git-tree safety gate (per tightening #9)**: before the delete commit, assert
  `git diff --stat -- ':!e2e/bench/' ':!docs/' ':!.claude/'` returns empty (only `e2e/bench/`, `docs/`, and `.claude/` paths touched by this restructure). Non-empty diff → abort the delete; surface to operator. This is a sibling of R5.3 (worktree-fork diff guard) but enforced on the main tree before the destructive commit.
- DELETE `e2e/bench/snowplow_test.py` (the worktree file). **Verify zero importers first** (§I block 5).
- DELETE `e2e/bench/page_load_test.py` (16 KB, last-touched 2026-05-11; superseded by `bench.browser`). **Verify zero importers.**
- update `e2e/bench/requirements.txt` final state.
- DROP the worktree at `.claude/worktrees/bench-harness-0.30-prep/` after confirming the main worktree harness produces a valid ledger row.
- update memory files §J.

**LOC delta target:** -8,691 (delete `snowplow_test.py`) and -16 KB (`page_load_test.py`). Final harness size ≈ 3,200–3,800 LOC across 8 modules + ~600 LOC tests.

**Exit criteria — the acceptance criteria in §H (a)–(h) must all pass.** If any fails, `git checkout legacy-bench-2026-06-02 -- e2e/bench/snowplow_test.py` restores the prior state immediately.

---

## H. Acceptance criteria (PM gate template)

Falsifier-style; each is a single `make`/`python -m`/`grep` invocation. Block 5's exit gate.

### H.0 PM-gate template question for per-block LOC delta (added v1.2, amended v1.3)

For Blocks 3 and 4 (LOC bands refreshed empirically against Block 1+2 actuals — see §G.0.1):

> **"Does actual code-LOC sit within the refreshed band (Block 3: 2,800–3,200; Block 4: 3,700–4,200)? OR is it UNDER-band with §A line-item accuracy demonstrated (each §A.x module size within 5% of the per-line-item estimate)? Yes / no — no re-litigation if within."**

A yes answer closes the LOC question for that block at gate time. Within-band overshoots vs. v1.1 numbers are NOT defects; the v1.1 numbers were the defect, corrected by v1.2. **Block 3 precedent (v1.3 amendment)**: Block 3 actuals came in UNDER-band by 637 LOC and PM ACK'd as plan-fidelity, not undercount, because each §A.5 line-item was tracked to within 4%. Under-band is acceptable when each §A.x line-item is empirically tracked to within 5%. Out-of-band actuals reopen the structural-cost analysis in §G.0 (which of the three costs grew? was there a new fourth cost?).

| ID | Criterion | How to verify |
|----|-----------|---------------|
| (a) | `pytest e2e/bench/tests/` runs all replaced self-tests in <30s with zero source-introspection escape-hatches | `pytest e2e/bench/tests/ -q --tb=line` (wall <30s) AND `grep -rE 'inspect\.getsource\|getsource\|sys\.modules\[__name__\]' e2e/bench/bench/ e2e/bench/tests/` returns 0. Extended grep blocks aliased `from inspect import getsource as gs` AND the `sys.modules[__name__]` reflection pattern (source 3106). Per tightening #1. |
| (b) | One full Phase 6 ledger row on 0.30.232 from main-worktree harness validates against a machine-readable schema | Dev MUST emit `e2e/bench/bench/ledger_row.schema.json` in Block 4 (JSON-Schema draft 2020-12 covering §F.1's frozen key list — every top-level key + cell sub-fields). Falsifier: `python -m jsonschema --instance /tmp/snowplow-runs/0.30.232/run-*/ledger_row.json --schema e2e/bench/bench/ledger_row.schema.json` returns 0. Plus `jq 'keys' ledger_row.json` returns the §F.1 literal list as a secondary cross-check. Per tightening #2. |
| (c) | Ledger row `mix_weighted.warm_p50_ms` within ±15% of the **most recent PASS row** in `project_north_star_ledger.md` at Block-5-start time | Step 1 at Block 5 start: read `memory/project_north_star_ledger.md`, pick the newest row whose `verdict == "PASS"`, capture its tag + `mix_weighted.warm_p50_ms` into `e2e/bench/.baseline.json` (tracked file). Step 2 after acceptance run: `summary.json` MUST contain `baseline_tag: "<that tag>"` and `baseline_warm_p50_ms: <value>` for reproducibility. Step 3: assert `abs(actual - baseline_warm_p50_ms) / baseline_warm_p50_ms ≤ 0.15`. Tolerance widened from ±5% to absorb cluster drift. Per tightening #3. |
| (d) | Deleting any 50 LOC across deleted self-tests changes nothing measurable | Tautology (self-tests are deleted). Verified by re-running (b) on a `git diff` showing only deletions of deleted-list lines |
| (e) | `bench check` exits 2 with named gates listed on any failure | Intentionally break gate 6 (e.g. `FRONTEND_URL=http://invalid`) → assert exit 2 + `"frontend_lb_reachable: FAIL"` on stderr |
| (f) | `bench phase6 --from-stage S5` resumes from `state.json` without re-running S1-S4 | Run to S4 then re-invoke with `--from-stage S5`; assert `state.json.stages_completed` already contains S0-S4 and stage timestamps for S0-S4 do NOT update |
| (g) | Overlay freshness gate refuses start when overlay >14 days old AND points to `bench calibrate` | **Source-of-truth for "age" = the overlay file's `os.stat().st_mtime`, NOT any `captured_at` JSON field inside the overlay** (hand-edits to a JSON field would forge freshness; mtime is set by the kernel when the file is rewritten). `bench/expected.py:overlay_freshness_or_die` docstring MUST state this explicitly. Falsifier: `touch -d '15 days ago' /tmp/snowplow-runs/calibration/expected_calls.json` → run `bench phase6` → assert exit 2 + stderr contains `"run: python -m bench calibrate"`. Per tightening #4. |
| (h) | `ConvergenceTimeout` raised on truly-not-converged; PASSes when `api == ui == cluster` | `tests/test_browser.py::test_browser_measure_stage_raises_ConvergenceTimeout` MUST use `with pytest.raises(ConvergenceTimeout): browser_measure_stage(...)` — NOT a log-string assertion (log-capture would still pass if the exception were swallowed by an `except:` upstream). Integration covered by (b) PASS implying converged. Per tightening #5. |

---

## I. Risk register

Risks ranked by impact × probability. Each entry: risk → trigger → mitigation (concrete file/line where the guardrail lives).

### Block 1

- **R1.1 (LOW × MEDIUM): k8s-client globals leak across module boundary.** Source 580–587 keeps `K8S_CLIENT_AVAILABLE`, `_k8s_core`, `_k8s_rbac`, `_k8s_custom`, `_k8s_apiext`, `_k8s_init_lock`, `_k8s_init_attempted` as module-level state. When this moves to `bench/cluster.py`, every other module reads through that module — fine — but tests must NOT share state. Mitigation: `conftest.py:reset_k8s_state` fixture (autouse=False, opt-in per test) that nulls the module-globals.

### Block 2

- **R2.1 (HIGH × LOW): EXPECTED_CALLS_TOLERANCE=0 + cluster jitter = false FAILs.** Today's tolerance=1 (source 212) absorbs retry jitter. Flipping to 0 is a Diego-ratified hard requirement (dispatch hard constraint #5) — but the operator MUST re-calibrate before the first scored run after the flip, or any prior overlay with a value off-by-one drops the gate. Mitigation: `cmd_check` reads the overlay's nav-by-nav values + cross-checks against `EXPECTED_CALLS` hardcoded defaults; if any value differs by >0 AND overlay age >0 days, prints a one-line "stale overlay vs defaults" warning. Lives in `bench/expected.py:gate_overlay_freshness`.

- **R2.2 (MEDIUM × MEDIUM): GKE context guard breaks kind/minikube local dev.** Mitigation: `--allow-non-gke` hidden flag (already in §D.1) + `BENCH_ALLOW_NON_GKE=1` env override. Tests pin both paths.

### Block 3

- **R3.1 (HIGH × HIGH): Playwright `record_video_dir` on every navigation could 10× the run's disk usage.** Each navigation in P6 across S1-S8 × 3 navs × 2 users × 2 pages × 2 cache modes = 192 navigations. If each .webm is ~5 MB that's ~960 MB just for videos. Mitigation:
  - `--video representative` (default) → only n=1 per `(stage, user, cache_mode, page)` cell gets recorded. Total: 64 navigations → ≤320 MB.
  - `--video none` for CI/smoke runs.
  - `--video all` only for diagnostic deep-dives.
  - **Soft cap on the run bundle with truncate-and-warn (per tightening #6)**: `ledger.write_run_bundle` walks `$run_dir/videos/` after stage S9, sums `webm + gif + screenshots + pod_logs`. If total > 200 MB, **drop oldest .webm/.gif pairs (by mtime) until under cap**, write `oversize_bundle.json` listing every trimmed file (name, size, mtime, reason), and emit a LOUD log line `BUNDLE TRUNCATED: dropped N files (M MB) to fit 200 MB cap`. **The ledger row STILL emits with its verdict intact** — truncation is an artifact-storage concern, not a measurement validity concern. Operator preference: never lose the scored row to a video-disk issue. The `summary.json` carries `bundle_truncated: bool` so reviewers see truncation without diving.

- **R3.2 (HIGH × MEDIUM): ConvergenceTimeout aborts the run mid-S6, losing partial data.** Today's silent-skip lets S7+S8 still measure and the ledger row records `convergence_mass_s6_p99: -1`. Raising hard could mask "S6 was slow but S7/S8 were fine." Mitigation: stage runner catches `ConvergenceTimeout` → writes the stage proof with `passed=False` and a `convergence_timeout: true` field → re-raises only AFTER persisting state.json. Operator resumes with `--from-stage S6` after diagnosing. The verdict logic in `ledger._compute_verdict` treats any stage with `convergence_timeout=true` as INVALID.

### Block 4

- **R4.1 (HIGH × HIGH): stage-resume masks incomplete state.** Operator runs `--from-stage S5` but `state.json` was written by an earlier S4 that silently failed (e.g. compositions did not finish reconciling). S5+ produce garbage. Mitigation:
  - Every stage proof MUST include a `what_breaks_if_skipped: str` field. Empty string is rejected by `phases.save_state` (`AssertionError`).
  - On `--from-stage SN` entry, `phases.run_phase6` re-runs the **proof-validation** subset of every prior stage's proof against the live cluster: e.g. S2 proof says "compdef present in bench-ns-01" → re-verify via a single `kubectl get compdef`. If any prior proof's assertion no longer holds, abort with "stale state.json; re-run from earlier stage."
  - The proof-validation step is bounded to ≤10s total (so resume stays fast). Stages whose proof cannot be validated in 10s opt out with `proof_validation: skipped` — the operator sees this and decides.

- **R4.2 (MEDIUM × LOW): `_stabilize` deletion + 7 call-site rewrites introduces races.** Source 6691/6697/6710/6724/6745/6837/6864 each call `_stabilize` after a cluster mutation. The function body today is `if cache_mode == "ON": time.sleep(5)` (source 6681–6682) — already a no-op for cache_mode==OFF. Mitigation: literal sed-equivalent rewrite to `if cache_mode == "ON": time.sleep(5)` at each site; no semantic change. Verify before merging by `git diff` showing only call-site rewrites + `_stabilize` deletion.

### Block 5

- **R5.1 (CRITICAL × LOW): Consumer outside `e2e/bench/` imports `snowplow_test`.** Mitigation — gating commands run before the delete:
  ```
  grep -rn "from snowplow_test" /Users/diegobraga/krateo/snowplow-cache/snowplow/ --include="*.py" \
      --exclude-dir=.venv --exclude-dir=.venv-bench --exclude-dir=__pycache__ \
      --exclude-dir=.claude 2>&1
  grep -rn "^import snowplow_test\b" /Users/diegobraga/krateo/snowplow-cache/snowplow/ --include="*.py" \
      --exclude-dir=.venv --exclude-dir=.venv-bench --exclude-dir=__pycache__ \
      --exclude-dir=.claude 2>&1
  ```
  TRACED 2026-06-02: both return zero hits in the main worktree. The file is a script (`if __name__ == "__main__":` at source 8690). The string `snowplow_test` does appear inside sibling worktrees `.claude/worktrees/distracted-einstein/` and `.claude/worktrees/q-con-1-multipod-driver/` (both as docstring/path strings, NOT module imports). Those worktrees are private to their own driver branches and out of scope for this restructure; the `--exclude-dir=.claude` filter scopes the gate to the main tree. Safe to delete in the main worktree. Block 5 re-runs the grep as the literal pre-delete gate.

- **R5.2 (HIGH × MEDIUM): Memory updates land late.** Mitigation: §J memory-update list is part of Block 5's exit criteria, not a footnote. The dev's git commit for Block 5 must include the three memory file rewrites listed in §J or the commit is rejected.

- **R5.3 (MEDIUM × LOW): Worktree fork drop loses uncommitted work.** Mitigation: before `git worktree remove .claude/worktrees/bench-harness-0.30-prep`, run `git diff --stat` inside that worktree; abort the drop on any non-empty diff and surface to operator.

---

## J. Memory updates required

Each Block 5 deliverable. The architect provides the file paths and Diego writes the contents (or dev drafts, Diego ratifies).

1. **REWRITE** `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_phase6_includes_gif_recording.md`
   - Current title implies Chrome MCP. New body: "Phase 6 records `.webm` per nav-cell via Playwright `record_video_dir`; `bench.browser.record_video_to_gif` post-processes to `.gif` via ffmpeg (`fps=4,scale=720:-1`). Default `--video representative` (n=1 per cell) to stay within 200 MB bundle cap. Chrome MCP is not used; see feedback_no_kubectl_in_measurement.md sister rule."

2. **CREATE** `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/bench-harness-package-layout.md`
   - One-liner: "Bench harness lives at `e2e/bench/bench/{cluster,lifecycle,storm,browser,expected,ledger,phases,cli}.py`. Operator entrypoint: `python -m bench <subcommand>`. See docs/bench-restructure-path-b-plan-2026-06-02.md §A for module surface."

3. **CREATE** `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/bench-state-json-protocol.md`
   - One-liner: "`$run_dir/state.json` records `stages_completed`, `stage_proofs.{S0..S9}`. Each proof carries a `what_breaks_if_skipped` field (asserted by phases.save_state). `--from-stage` re-validates prior proofs against the live cluster before resuming (≤10s budget). See docs/bench-restructure-path-b-plan-2026-06-02.md §E."

4. **AMEND** `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/MEMORY.md`
   - Add the new entries (#2, #3, #5) to the index, under ~200 chars per line per the existing warning. Order: alphabetical within their topical block.

5. **CREATE** `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_silent_skip_breaks_convergence_proof.md` (per tightening #10)
   - One-liner: "Helpers returning `False`/`-1`/`None` on timeout that callers don't check are SILENT SKIPS — they let downstream code measure against unverified state. Convergence helpers MUST raise (e.g. `ConvergenceTimeout`). `wait_for_l1_quiescent` (legacy source 1074–1097) deleted 2026-06-02 because it returned `False` on timeout and the caller proceeded. Sister rule to feedback_validate_content_not_just_status."

---

## Time-box check

This plan: 8 sections (A-J) × concrete file:line citations + acceptance falsifiers. No fresh-agent peer review at the plan stage (dispatch's instruction); fresh-agent review happens at the PM gate before dev dispatch.

Written by: **cache-architect**, 2026-06-02, ≤45 minutes.

---

## Revision history
- 2026-06-02 v1.0 cache-architect — initial plan
- 2026-06-02 v1.1 cache-architect — PM gate tightenings folded (10 items)
- 2026-06-02 v1.2 cache-architect — §G Block 3 + Block 4 LOC targets refreshed empirically (Block 1+2 anchor; ×2/×3 dev heuristic; PM gate stricter)
- 2026-06-02 v1.3 cache-architect — Block 3 PM-gate carry-forwards: §H.0 under-band amendment, §G Block 3 smoke-gate descoped to Block 4, §G Block 4 inherits live-cluster smoke run + ConvergenceTimeout catch wiring

