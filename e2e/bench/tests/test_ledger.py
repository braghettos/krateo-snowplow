"""Tests for bench.ledger.

Covers §C.6 (~10 cases per plan). Replaces source 2892-2977 / 2979-3090 /
3467-3629 / etc. — all via behavioral assertions, no inspect.getsource.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.6.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from bench import ledger


# ─── Frozen-schema gate (replaces source 2892-2977) ─────────────────────────


def test_canonical_row_schema_fields_frozen(monkeypatch):
    """The §F.1 frozen key list MUST appear in every emitted row."""
    # Synthesize a minimum nav set so build_canonical_ledger_row has shape.
    all_results = []
    monkeypatch.setattr(
        ledger, "kubectl",
        lambda *a, **k: (1, "", "no cluster"))
    row = ledger.build_canonical_ledger_row(all_results, tag="0.30.232",
                                            scale=5000)
    expected_keys = {
        "tag", "ship_date", "scale", "uptime_at_capture_s",
        "cells", "mix_weighted",
        "convergence_mass_s6_p99", "convergence_mass_s7_p99",
        "convergence_mass_s8_p99",
        "convergence_per_mutation_p99_mix",
        "convergence_per_class_hot_p99",
        "convergence_per_class_warm_p99",
        "convergence_per_class_cold_p99",
        "tag_specific_verifications",
        "pod_restart_count", "validation", "verdict",
    }
    assert expected_keys.issubset(set(row.keys())), (
        f"missing keys: {expected_keys - set(row.keys())}")
    assert set(row["cells"]) == {"admin_on", "admin_off",
                                 "cyber_on", "cyber_off"}


# ─── Floor-shape (replaces source 2979-3090) ────────────────────────────────


def test_canonical_row_floor_shape_when_cache_unsupported(monkeypatch):
    """When cache is unsupported, ON cells are zero; FLOOR verdict surfaces."""
    monkeypatch.setattr(
        ledger, "kubectl", lambda *a, **k: (1, "", ""))
    all_results = [
        {
            "stage": "6", "cache": "OFF",
            "cache_supported": False,
            "pages": {
                "Dashboard": {
                    "navigations": [
                        {"user": "admin", "nav_num": 1, "waterfallMs": 800},
                        {"user": "admin", "nav_num": 2, "waterfallMs": 400},
                        {"user": "cyberjoker", "nav_num": 1,
                         "waterfallMs": 900},
                        {"user": "cyberjoker", "nav_num": 2,
                         "waterfallMs": 400},
                    ],
                },
            },
        },
    ]
    row = ledger.build_canonical_ledger_row(all_results, tag="floor-tag",
                                            scale=5000)
    assert row["cells"]["admin_on"]["warm_p50_ms"] == 0
    assert row["cells"]["cyber_on"]["warm_p50_ms"] == 0
    assert row["cells"]["admin_off"]["warm_p50_ms"] == 400
    # FLOOR verdict surfaces when ON cells are zero but OFF cells carry data.
    assert row["verdict"] == "FLOOR"


# ─── Sentinel filter (replaces source 3467-3629) ────────────────────────────


def test_cell_stats_drops_waterfall_zero_sentinels(monkeypatch):
    """waterfallMs == 0 navigations are excluded from percentile compute."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    all_results = [
        {"stage": "6", "cache": "ON", "pages": {"Dashboard": {
            "navigations": [
                # Mixed valid + invalid samples; only the 400+500 must count
                {"user": "admin", "nav_num": 1, "waterfallMs": 0},
                {"user": "admin", "nav_num": 2, "waterfallMs": 400},
                {"user": "admin", "nav_num": 3, "waterfallMs": 500},
            ],
        }}},
    ]
    row = ledger.build_canonical_ledger_row(all_results, tag="t", scale=5000)
    cell = row["cells"]["admin_on"]
    # waterfallMs=0 is invalid; nav#2 (400) and nav#3 (500) are WARM.
    assert cell["warm_p50_ms"] == 400
    assert cell["invalid_nav_count"] == 1
    assert cell["valid_nav_count"] == 2


def test_cell_stats_returns_none_when_all_invalid(monkeypatch):
    """All-invalid cells report None percentiles (not 0)."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    all_results = [
        {"stage": "6", "cache": "ON", "pages": {"Dashboard": {
            "navigations": [
                {"user": "admin", "nav_num": 1, "waterfallMs": 0,
                 "incomplete": True},
                {"user": "admin", "nav_num": 2, "incomplete": True,
                 "waterfallMs": 0},
            ],
        }}},
    ]
    row = ledger.build_canonical_ledger_row(all_results, tag="t", scale=5000)
    cell = row["cells"]["admin_on"]
    assert cell["warm_p50_ms"] is None
    assert cell["valid_nav_count"] == 0


# ─── Mix-weighted formula ────────────────────────────────────────────────────


def test_mix_weighted_is_095_cyber_plus_005_admin(monkeypatch):
    """mix_weighted = 0.95*cyber + 0.05*admin per feedback_north_star_is_frontend_ux."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    all_results = [
        {"stage": "6", "cache": "ON", "pages": {"Dashboard": {
            "navigations": [
                {"user": "admin", "nav_num": 2, "waterfallMs": 1000},
                {"user": "cyberjoker", "nav_num": 2, "waterfallMs": 200},
            ],
        }}},
    ]
    row = ledger.build_canonical_ledger_row(all_results, tag="t", scale=5000)
    # 0.95*200 + 0.05*1000 = 190 + 50 = 240
    assert row["mix_weighted"]["warm_p50_ms"] == 240


def test_mix_weighted_returns_none_when_no_samples_in_either_cell(monkeypatch):
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    row = ledger.build_canonical_ledger_row([], tag="t", scale=5000)
    assert row["mix_weighted"]["warm_p50_ms"] is None
    assert row["mix_weighted"]["cold_ms"] is None


# ─── Verdict gates ──────────────────────────────────────────────────────────


def test_verdict_is_INVALID_when_any_cell_all_invalid(monkeypatch):
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    all_results = [
        {"stage": "6", "cache": "ON", "pages": {"Dashboard": {
            "navigations": [
                # admin all invalid
                {"user": "admin", "nav_num": 1, "waterfallMs": 0,
                 "incomplete": True,
                 "validation": {"terminal_state": "fail"}},
                # cyber valid
                {"user": "cyberjoker", "nav_num": 2, "waterfallMs": 200,
                 "validation": {"terminal_state": "pass"}},
            ],
        }}},
    ]
    row = ledger.build_canonical_ledger_row(all_results, tag="t", scale=5000)
    assert row["verdict"] == "INVALID"


# ─── Pod-restart fallback ───────────────────────────────────────────────────


def test_pod_restart_count_zero_when_kubectl_unavailable(monkeypatch):
    """kubectl-returns-1 path should leave pod_restart_count at 0."""
    monkeypatch.setattr(ledger, "kubectl",
                        lambda *a, **k: (1, "", "no cluster"))
    row = ledger.build_canonical_ledger_row([], tag="t", scale=5000)
    assert row["pod_restart_count"] == 0


# ─── print_report ───────────────────────────────────────────────────────────


def test_print_report_returns_true_when_all_passed(capsys):
    ledger.reset_results()
    ledger.add_result({"name": "x", "passed": True, "ms": 1, "code": 0,
                       "note": ""})
    assert ledger.print_report() is True
    ledger.reset_results()
    ledger.add_result({"name": "y", "passed": False, "ms": 1, "code": 500,
                       "note": "boom"})
    assert ledger.print_report() is False
    ledger.reset_results()


# ─── Per-mutation metric loader (Block-4 path migration) ────────────────────


def test_per_mutation_metric_loads_from_run_dir_relative_path(tmp_path):
    """The Block-4 relocation: read from $run_dir/phase8/per_mutation_results.json."""
    p8 = tmp_path / "phase8"
    p8.mkdir()
    (p8 / "per_mutation_results.json").write_text(
        json.dumps({"p99_mix": 425, "hot_p99": 100,
                    "warm_p99": 200, "cold_p99": 700}))
    val = ledger._load_per_mutation_metric("p99_mix", run_dir=tmp_path)
    assert val == 425
    val2 = ledger._load_per_mutation_metric("cold_p99", run_dir=tmp_path)
    assert val2 == 700


# ─── Schema artifact (acceptance (b)) ───────────────────────────────────────


def test_emit_ledger_row_schema_writes_valid_json(tmp_path):
    out = tmp_path / "ledger_row.schema.json"
    ledger.emit_ledger_row_schema(out)
    assert out.exists()
    d = json.loads(out.read_text())
    assert d["$schema"] == \
        "https://json-schema.org/draft/2020-12/schema"
    # Every §F.1 top-level key listed.
    required = set(d["required"])
    assert "tag" in required and "verdict" in required
    assert "mix_weighted" in required


def test_emitted_schema_validates_a_synthesized_row(tmp_path, monkeypatch):
    """jsonschema.validate(row, schema) must accept a synthesized row."""
    jsonschema = pytest.importorskip("jsonschema")
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    row = ledger.build_canonical_ledger_row([], tag="0.30.232", scale=5000)
    schema = ledger._ledger_row_schema_doc()
    # The synthesized row has mix_weighted={null, null, null} (empty input).
    # That's valid per the schema (cold_ms/warm_p50_ms/warm_p99_ms allow null).
    jsonschema.validate(row, schema)


# ─── Baseline (acceptance (c)) ──────────────────────────────────────────────


def test_compute_baseline_delta_returns_signed_ratio():
    # +10% drift
    assert abs(ledger.compute_baseline_delta(110, 100) - 0.1) < 1e-9
    # -20% drift
    assert abs(ledger.compute_baseline_delta(80, 100) - (-0.2)) < 1e-9
    # missing inputs → None (no anchor)
    assert ledger.compute_baseline_delta(None, 100) is None
    assert ledger.compute_baseline_delta(100, None) is None
    assert ledger.compute_baseline_delta(100, 0) is None


def test_read_baseline_returns_tag_and_warm_p50(tmp_path, monkeypatch):
    fake = tmp_path / ".baseline.json"
    fake.write_text(json.dumps({
        "baseline_tag": "0.30.227",
        "baseline_warm_p50_ms": 512,
    }))
    monkeypatch.setattr(ledger, "BASELINE_PATH", fake)
    tag, ms = ledger.read_baseline()
    assert tag == "0.30.227"
    assert ms == 512


# ─── Run bundle truncate logic (per R3.1) ───────────────────────────────────


def test_write_run_bundle_truncates_oversize_video_bundle(tmp_path,
                                                         monkeypatch):
    """When videos+gifs+logs exceed 200 MB, oldest pairs drop and the
    BUNDLE TRUNCATED log + oversize_bundle.json file appear."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    monkeypatch.setattr(ledger, "RUN_BUNDLE_MAX_BYTES", 1_000_000)  # 1 MB cap

    vdir = tmp_path / "videos"
    vdir.mkdir()
    # Write 3 webm/gif pairs of ~500 KB each → 3 MB total
    for i in range(3):
        webm = vdir / f"S{i + 1}_admin_cold.webm"
        gif = vdir / f"S{i + 1}_admin_cold.gif"
        webm.write_bytes(b"\x00" * 400_000)
        gif.write_bytes(b"\x00" * 100_000)
        # Stagger mtimes so the truncator picks oldest first
        import os as _os
        _os.utime(webm, (1000 + i, 1000 + i))
        _os.utime(gif, (1000 + i, 1000 + i))

    ledger.write_run_bundle(tmp_path, [], per_stage_proofs={},
                            tag="t", scale=5000)
    # After truncate, the oversize_bundle.json must list trimmed entries
    assert (tmp_path / "oversize_bundle.json").exists()
    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["bundle_truncated"] is True


def _compressible_log_bytes(n: int) -> bytes:
    """A semi-repetitive byte payload that gzip shrinks substantially
    (models real pod_logs, which are highly compressible text). Pure
    \\x00 compresses ~1000× which is unrealistic; a repeating line gives a
    realistic ~10-20× ratio for the drop-order test sizing."""
    line = b"2026-06-10T00:00:00Z INFO cache.event consumed gvr=panels ns=bench-ns-01\n"
    return (line * (n // len(line) + 1))[:n]


def test_truncate_gzips_pod_logs_retaining_videos_and_logs(tmp_path,
                                                           monkeypatch):
    """Task #299 (reworked): pod_logs are gzipped at bundle time, after
    which BOTH videos AND logs fit the cap — nothing is dropped. The .gz
    keeps the .txt stem (S6.txt.gz) for discoverability; the original .txt
    is removed; the gzip saving is recorded in oversize_bundle.json."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    monkeypatch.setattr(ledger, "RUN_BUNDLE_MAX_BYTES", 1_000_000)  # 1 MB cap

    vdir = tmp_path / "videos"
    vdir.mkdir()
    # 1 video pair = 500 KB (retained).
    (vdir / "S6_admin_cold.webm").write_bytes(b"\x00" * 400_000)
    (vdir / "S6_admin_cold.gif").write_bytes(b"\x00" * 100_000)

    # Raw pod_logs ~5 MB (over the 1 MB cap) but ~10-20× compressible →
    # after gzip the bundle (videos 0.5 MB + tiny .gz) fits.
    pdir = tmp_path / "pod_logs"
    pdir.mkdir()
    (pdir / "S6.txt").write_bytes(_compressible_log_bytes(5_000_000))

    ledger.write_run_bundle(tmp_path, [], per_stage_proofs={},
                            tag="t", scale=5000)

    # Logs are now gzipped with the .txt stem preserved; raw removed.
    assert (pdir / "S6.txt.gz").exists(), "pod log must be gzipped as S6.txt.gz"
    assert not (pdir / "S6.txt").exists(), "raw .txt must be removed after gzip"
    # Videos retained.
    assert (vdir / "S6_admin_cold.webm").exists()
    assert (vdir / "S6_admin_cold.gif").exists()
    # gzip alone fit the cap → NO files dropped → bundle_truncated False.
    over = json.loads((tmp_path / "oversize_bundle.json").read_text())
    reasons = [t["reason"] for t in over["trimmed"]]
    assert "bundle_pod_logs_gzipped" in reasons
    assert not any(r.startswith("bundle_oversize_truncate") for r in reasons), (
        f"nothing should be dropped when gzip fits the cap; got {reasons}")
    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["bundle_truncated"] is False
    assert ledger._bundle_size_bytes(tmp_path) <= 1_000_000


def test_truncate_drop_order_screenshots_then_videos_then_gz_logs_last(
        tmp_path, monkeypatch):
    """Task #299 (reworked): when gzip alone is NOT enough, the drop order
    is screenshots → oldest videos → gzipped pod_logs LAST. Gzipped logs
    (irreplaceable trace inputs) are the very last to go and only if the
    cap still cannot be met. The cap stays a hard ceiling."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    monkeypatch.setattr(ledger, "RUN_BUNDLE_MAX_BYTES", 1_000_000)  # 1 MB cap

    import os as _os
    # Screenshots 600 KB (dropped first).
    sdir = tmp_path / "screenshots"
    sdir.mkdir()
    (sdir / "S6_admin.png").write_bytes(b"\x00" * 600_000)
    # 3 video pairs of 500 KB = 1.5 MB incompressible (dropped second,
    # oldest-first).
    vdir = tmp_path / "videos"
    vdir.mkdir()
    for i in range(3):
        webm = vdir / f"S{i + 1}_admin_cold.webm"
        gif = vdir / f"S{i + 1}_admin_cold.gif"
        webm.write_bytes(b"\x00" * 400_000)
        gif.write_bytes(b"\x00" * 100_000)
        _os.utime(webm, (1000 + i, 1000 + i))
        _os.utime(gif, (1000 + i, 1000 + i))
    # A pod_log that stays large even gzipped (incompressible \x00 → gzip
    # is tiny, so to force a log-drop we use random-ish incompressible
    # bytes large enough to exceed the cap by itself post-gzip).
    pdir = tmp_path / "pod_logs"
    pdir.mkdir()
    (pdir / "S6.txt").write_bytes(os.urandom(2_000_000))  # ~incompressible

    ledger.write_run_bundle(tmp_path, [], per_stage_proofs={},
                            tag="t", scale=5000)

    over = json.loads((tmp_path / "oversize_bundle.json").read_text())
    # Order of drop reasons (gzip summary first, then drops in phase order).
    drop_reasons = [t["reason"] for t in over["trimmed"]
                    if t["reason"].startswith("bundle_oversize_truncate")]
    assert "bundle_pod_logs_gzipped" in [t["reason"] for t in over["trimmed"]]
    # Screenshots dropped before videos before gz logs.
    def _first_idx(reason):
        for i, r in enumerate(drop_reasons):
            if r == reason:
                return i
        return None
    ss = _first_idx("bundle_oversize_truncate_screenshot")
    vid = _first_idx("bundle_oversize_truncate_video")
    gz = _first_idx("bundle_oversize_truncate_podlog_gz")
    assert ss is not None and ss == 0, (
        f"screenshots must drop first; drop order={drop_reasons}")
    if vid is not None and gz is not None:
        assert vid < gz, "videos must drop before gzipped pod_logs"
    if ss is not None and vid is not None:
        assert ss < vid, "screenshots must drop before videos"
    # Cap honoured as a hard ceiling.
    assert ledger._bundle_size_bytes(tmp_path) <= 1_000_000


def test_truncate_gz_logs_dropped_only_as_last_resort(tmp_path, monkeypatch):
    """Task #299: gzipped pod_logs are dropped ONLY when screenshots +
    videos are already gone and the cap still cannot be met. Here a single
    incompressible 2 MB log alone exceeds the 1 MB cap with no other
    artifacts → the gz log must be dropped (last resort), cap honoured."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    monkeypatch.setattr(ledger, "RUN_BUNDLE_MAX_BYTES", 1_000_000)

    pdir = tmp_path / "pod_logs"
    pdir.mkdir()
    (pdir / "S6.txt").write_bytes(os.urandom(2_000_000))  # gzip ≈ 2 MB still

    ledger.write_run_bundle(tmp_path, [], per_stage_proofs={},
                            tag="t", scale=5000)

    over = json.loads((tmp_path / "oversize_bundle.json").read_text())
    reasons = [t["reason"] for t in over["trimmed"]]
    assert "bundle_pod_logs_gzipped" in reasons
    assert "bundle_oversize_truncate_podlog_gz" in reasons, (
        "the gz log must be dropped as a last resort when it alone exceeds "
        "the cap")
    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["bundle_truncated"] is True  # a file WAS dropped
    assert ledger._bundle_size_bytes(tmp_path) <= 1_000_000


# ─── Task #289: conv tier revision + reporting-clarity fixes ────────────────


def _mw(warm, cold):
    return {"warm_p50_ms": warm, "cold_ms": cold}


def test_conv_tier_30s_measured_p99_does_not_miss():
    """Conv tier revised to 30000ms (Task #289). The measured worst-case
    s8 p99 ≈ 23.8s (23800ms) is WITHIN tier → no conv miss → PASS when
    warm/cold are clean."""
    assert ledger.CONV_TIER_MS == 30000
    v = ledger.compute_verdict(_mw(400, 900), restarts=0,
                               conv_s8_p99=23800, cells=None)
    assert v == "PASS"


def test_conv_tier_31s_exceeds_revised_tier_is_a_miss():
    """31000ms > 30000ms tier → one tier missed → WEAK_PASS (warm/cold
    clean). Confirms the tier still discriminates above the revised bound."""
    v = ledger.compute_verdict(_mw(400, 900), restarts=0,
                               conv_s8_p99=31000, cells=None)
    assert v == "WEAK_PASS"


def test_conv_tier_boundary_exactly_30s_passes():
    """Boundary: conv == 30000 is NOT > 30000 → not a miss."""
    v = ledger.compute_verdict(_mw(400, 900), restarts=0,
                               conv_s8_p99=30000, cells=None)
    assert v == "PASS"


def test_warm_cold_tiers_revised_to_measured_floor_task_121():
    """Task #121: warm_p50 tier 500 -> 1000, cold tier 1000 -> 1300, set to
    the measured-achievable floor (~1.09× worst clean 50K run: warm 914,
    cold 1196). The warm/cold tiers are structurally frontend-gated
    (docs/task-288 + docs/task-300); 500/1000 stay aspirational, documented,
    NOT deleted."""
    assert ledger.WARM_P50_TIER_MS == 1000
    assert ledger.COLD_TIER_MS == 1300


def test_measured_clean_50k_run_scores_no_tier_miss_task_121():
    """A measured clean 50K run (warm_p50=890 / cold=1175, the 0.30.254
    figures) now scores NO tier miss under the revised tiers → PASS when
    conv is within tier (acceptance assumes clean validation)."""
    v = ledger.compute_verdict(_mw(890, 1175), restarts=0,
                               conv_s8_p99=23800, cells=None)
    assert v == "PASS"


def test_warm_p50_boundary_1000_exact_no_miss_1001_misses():
    """Boundary: warm_p50 == 1000 is NOT > 1000 → no miss (clean cold/conv
    → PASS); warm_p50 == 1001 IS a miss → one tier → WEAK_PASS."""
    assert ledger.compute_verdict(_mw(1000, 1175), restarts=0,
                                  conv_s8_p99=23800, cells=None) == "PASS"
    assert ledger.compute_verdict(_mw(1001, 1175), restarts=0,
                                  conv_s8_p99=23800, cells=None) == "WEAK_PASS"


def test_cold_boundary_1300_exact_no_miss_1301_misses():
    """Boundary: cold == 1300 is NOT > 1300 → no miss (clean warm/conv →
    PASS); cold == 1301 IS a miss → one tier → WEAK_PASS."""
    assert ledger.compute_verdict(_mw(890, 1300), restarts=0,
                                  conv_s8_p99=23800, cells=None) == "PASS"
    assert ledger.compute_verdict(_mw(890, 1301), restarts=0,
                                  conv_s8_p99=23800, cells=None) == "WEAK_PASS"


def test_conv_tier_unchanged_at_30000_under_task_121():
    """Task #121 moves ONLY warm/cold; the conv tier stays 30000. Boundary
    30000 = no miss, 30001 = miss (regression guard that #121 did not touch
    the conv gate)."""
    assert ledger.CONV_TIER_MS == 30000
    assert ledger.compute_verdict(_mw(890, 1175), restarts=0,
                                  conv_s8_p99=30000, cells=None) == "PASS"
    assert ledger.compute_verdict(_mw(890, 1175), restarts=0,
                                  conv_s8_p99=30001, cells=None) == "WEAK_PASS"


# ── #289a: skeleton_failures respects the efaf1a4 demotion ──────────────────


def _nav(label, *, terminal_state, skeleton_count, skeleton_materializing,
         waterfall=200, user="cyberjoker"):
    return {
        "user": user, "nav_num": 2, "waterfallMs": waterfall,
        "label": label,
        "validation": {
            "terminal_state": terminal_state,
            "skeleton_count": skeleton_count,
            "skeleton_materializing": skeleton_materializing,
            "errored_count": 0,
        },
    }


def test_skeleton_failures_excludes_demoted_materializing_nav():
    """A nav that PASSED with skeleton_materializing=True (the S4 small-N
    race demoted at efaf1a4) is NOT a skeleton failure — telemetry must
    not report it."""
    all_results = [{"stage": "4", "cache": "ON", "pages": {"Compositions": {
        "navigations": [_nav("S4 ON nav#1 Compositions",
                              terminal_state="pass", skeleton_count=2,
                              skeleton_materializing=True)],
    }}}]
    summary = ledger.aggregate_validation(all_results)
    assert summary["skeleton_failures"] == []
    assert summary["navs_terminal_pass"] == 1


def test_skeleton_failures_still_includes_hard_fail_skeleton():
    """A genuine stuck-widget skeleton (terminal_state=fail, NOT demoted)
    is STILL recorded — the demotion must not weaken hard-fail detection."""
    all_results = [{"stage": "6", "cache": "ON", "pages": {"Dashboard": {
        "navigations": [_nav("S6 ON nav#1 Dashboard",
                              terminal_state="fail", skeleton_count=1,
                              skeleton_materializing=False)],
    }}}]
    summary = ledger.aggregate_validation(all_results)
    assert summary["skeleton_failures"] == ["S6 ON nav#1 Dashboard"]
    assert summary["navs_terminal_fail"] == 1


def test_skeleton_failures_includes_skeleton_pass_without_materializing_flag():
    """Defensive: a skeleton on a nav that did NOT carry the materializing
    demotion (flag absent/False) is still reported even if terminal_state
    happens to be 'pass' — only the explicit demotion is excluded."""
    all_results = [{"stage": "6", "cache": "ON", "pages": {"Dashboard": {
        "navigations": [_nav("S6 ON nav#2 Dashboard",
                              terminal_state="pass", skeleton_count=3,
                              skeleton_materializing=False)],
    }}}]
    summary = ledger.aggregate_validation(all_results)
    assert summary["skeleton_failures"] == ["S6 ON nav#2 Dashboard"]


# ── #296: S10 churn-demoted navs excluded from call_count_mismatches ────────


def _churn_nav(label, *, expected, actual, s10_churn_demoted):
    """A /compositions nav whose call-count mismatched (actual != expected)
    but terminal_state passed because the S10 churn demotion fired."""
    return {
        "user": "admin", "nav_num": 1, "waterfallMs": 200, "label": label,
        "validation": {
            "terminal_state": "pass",
            "skeleton_count": 0,
            "errored_count": 5,
            "expected_calls": expected,
            "actual_calls": actual,
            "calls_within_tolerance": False,   # the demotion does NOT flip this
            "s10_churn_demoted": s10_churn_demoted,
        },
    }


def test_call_count_mismatches_excludes_s10_churn_demoted_nav():
    """#296 telemetry fix: a nav whose mismatch was demoted by the S10
    controller-churn ghost rule (terminal_state='pass' +
    s10_churn_demoted=True) must NOT appear in call_count_mismatches —
    otherwise the ledger self-contradicts (navs_terminal_fail==0 alongside
    a mismatch tuple). Mirrors the #289a skeleton exclusion."""
    all_results = [{"stage": "10", "cache": "ON", "pages": {"Compositions": {
        "navigations": [_churn_nav("S10 admin Compositions",
                                   expected=30, actual=35,
                                   s10_churn_demoted=True)],
    }}}]
    summary = ledger.aggregate_validation(all_results)
    assert summary["navs_terminal_fail"] == 0
    assert summary["call_count_mismatches"] == [], (
        "a churn-demoted nav must not be reported as a call_count_mismatch "
        "(self-contradictory ledger otherwise)")


def test_call_count_mismatches_still_records_genuine_mismatch():
    """Regression guard: a real mismatch (NOT churn-demoted — e.g.
    terminal_state='fail') is STILL recorded. The exclusion must not
    weaken genuine under/over-call detection."""
    all_results = [{"stage": "6", "cache": "ON", "pages": {"Compositions": {
        "navigations": [_churn_nav("S6 admin Compositions",
                                   expected=30, actual=10,
                                   s10_churn_demoted=False)],
    }}}]
    # terminal_state is 'pass' in _churn_nav; force the genuine-fail shape.
    all_results[0]["pages"]["Compositions"]["navigations"][0][
        "validation"]["terminal_state"] = "fail"
    summary = ledger.aggregate_validation(all_results)
    assert summary["navs_terminal_fail"] == 1
    assert summary["call_count_mismatches"] == [("S6 admin Compositions", 30, 10)]


# ── #289b: failed_gates enumerates latency-tier misses on non-PASS ──────────


def test_failed_gates_carries_tier_entries_on_fail(tmp_path, monkeypatch):
    """A FAIL verdict (2 tiers missed) must enumerate the tier misses so
    failed_gates is never empty on a non-PASS verdict. Thresholds track the
    Task #121 revision (warm>1000, cold>1300)."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    # warm 1100>1000 AND cold 1500>1300 → 2 misses → FAIL.
    all_results = [{"stage": "6", "cache": "ON", "pages": {"Dashboard": {
        "navigations": [
            {"user": "cyberjoker", "nav_num": 1, "cold_warm": "COLD",
             "waterfallMs": 1500, "validation": {"terminal_state": "pass"}},
            {"user": "cyberjoker", "nav_num": 2, "waterfallMs": 1100,
             "validation": {"terminal_state": "pass"}},
        ],
    }}}]
    ledger.write_run_bundle(tmp_path, all_results, per_stage_proofs={},
                            tag="t", scale=50000)
    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["verdict"] == "FAIL"
    fg = summary["failed_gates"]
    assert any(g.startswith("tier:warm_p50 ") and g.endswith(">1000")
               for g in fg), fg
    assert any(g.startswith("tier:cold ") and g.endswith(">1300")
               for g in fg), fg
    # Sanity: the contradiction the fix targets (FAIL with [] gates) is gone.
    assert fg != []


def test_failed_gates_enumerates_conv_tier_miss(tmp_path, monkeypatch):
    """conv_s8_p99 above the 30s tier appears as a tier:conv_s8_p99 entry."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    # warm/cold clean; conv 31000>30000 → 1 miss → WEAK_PASS (non-PASS).
    all_results = [{"stage": "8", "cache": "ON", "pages": {"Compositions": {
        "navigations": [
            {"user": "cyberjoker", "nav_num": 1, "cold_warm": "COLD",
             "waterfallMs": 800, "convergence_ms": 31000,
             "validation": {"terminal_state": "pass"}},
            {"user": "cyberjoker", "nav_num": 2, "waterfallMs": 300,
             "convergence_ms": 31000,
             "validation": {"terminal_state": "pass"}},
        ],
    }}}]
    ledger.write_run_bundle(tmp_path, all_results, per_stage_proofs={},
                            tag="t", scale=50000)
    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["verdict"] in ("WEAK_PASS", "FAIL")
    assert any(g.startswith("tier:conv_s8_p99 ") and g.endswith(">30000")
               for g in summary["failed_gates"]), summary["failed_gates"]


def test_failed_gates_empty_on_valid_pass(tmp_path, monkeypatch):
    """A clean PASS leaves failed_gates == [] (no tier entries, no
    contradiction in the other direction)."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    all_results = [{"stage": "8", "cache": "ON", "pages": {"Compositions": {
        "navigations": [
            {"user": "cyberjoker", "nav_num": 1, "cold_warm": "COLD",
             "waterfallMs": 800, "convergence_ms": 10000,
             "validation": {"terminal_state": "pass"}},
            {"user": "cyberjoker", "nav_num": 2, "waterfallMs": 300,
             "convergence_ms": 10000,
             "validation": {"terminal_state": "pass"}},
        ],
    }}}]
    ledger.write_run_bundle(tmp_path, all_results, per_stage_proofs={},
                            tag="t", scale=50000)
    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["verdict"] == "PASS"
    assert summary["failed_gates"] == []
