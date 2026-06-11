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


# ─── Run bundle: cap REMOVED, nothing dropped (Task #121) ───────────────────


def _compressible_log_bytes(n: int) -> bytes:
    """A semi-repetitive byte payload that gzip shrinks substantially
    (models real pod_logs, which are highly compressible text). Pure
    \\x00 compresses ~1000× which is unrealistic; a repeating line gives a
    realistic ~10-20× ratio."""
    line = b"2026-06-10T00:00:00Z INFO cache.event consumed gvr=panels ns=bench-ns-01\n"
    return (line * (n // len(line) + 1))[:n]


def test_cap_removed_retains_all_artifacts_over_200mb(tmp_path, monkeypatch):
    """Task #121: the 200 MB cap is REMOVED. A bundle whose videos +
    screenshots + pod_logs total well over 200 MB keeps EVERY artifact —
    nothing is dropped, bundle_truncated is False, oversize_bundle.json
    reports zero drops."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    # No RUN_BUNDLE_MAX_BYTES monkey-patch: the constant no longer exists.
    assert not hasattr(ledger, "RUN_BUNDLE_MAX_BYTES"), (
        "Task #121 removed the cap constant; nothing should reference it")

    import os as _os
    vdir = tmp_path / "videos"
    vdir.mkdir()
    sdir = tmp_path / "screenshots"
    sdir.mkdir()
    pdir = tmp_path / "pod_logs"
    pdir.mkdir()

    # 5 video pairs @ ~45 MB = ~225 MB of video alone (incompressible) —
    # well over the OLD 200 MB cap. Plus a 30 MB screenshot. All must stay.
    webms, gifs = [], []
    for i in range(5):
        webm = vdir / f"S{i + 1}_admin_cold.webm"
        gif = vdir / f"S{i + 1}_admin_cold.gif"
        webm.write_bytes(os.urandom(40_000_000))
        gif.write_bytes(os.urandom(5_000_000))
        _os.utime(webm, (1000 + i, 1000 + i))
        _os.utime(gif, (1000 + i, 1000 + i))
        webms.append(webm)
        gifs.append(gif)
    big_shot = sdir / "S6_admin.png"
    big_shot.write_bytes(os.urandom(30_000_000))
    # A compressible pod_log (gets gzipped, but NOT dropped).
    (pdir / "S6.txt").write_bytes(_compressible_log_bytes(20_000_000))

    total_before = ledger._bundle_size_bytes(tmp_path)
    assert total_before > 200 * 1024 * 1024, "fixture must exceed old cap"

    ledger.write_run_bundle(tmp_path, [], per_stage_proofs={},
                            tag="t", scale=50000)

    # EVERY video + the screenshot survive (nothing dropped).
    for p in webms + gifs:
        assert p.exists(), f"video {p.name} must be retained (cap removed)"
    assert big_shot.exists(), "screenshot must be retained (cap removed)"
    # Pod log gzipped (compress, not drop): .gz present, raw removed.
    assert (pdir / "S6.txt.gz").exists()
    assert not (pdir / "S6.txt").exists()

    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["bundle_truncated"] is False
    over = json.loads((tmp_path / "oversize_bundle.json").read_text())
    assert over["cap_removed"] is True
    assert over["dropped_count"] == 0
    reasons = [t["reason"] for t in over["trimmed"]]
    # The ONLY trimmed entry is the gzip summary — never a drop.
    assert "bundle_pod_logs_gzipped" in reasons
    assert not any(r.startswith("bundle_oversize_truncate") for r in reasons), (
        f"nothing may be dropped after the cap removal; got {reasons}")


def test_cap_removed_independent_of_video_mode(tmp_path, monkeypatch):
    """Task #121: the removal is UNCONDITIONAL — there is no per-video-mode
    branch. write_run_bundle takes no video arg and never drops, so a
    >200 MB video set is retained whatever the run's video mode was
    (representative or all). Verifies no mode-gated dropping remains."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    vdir = tmp_path / "videos"
    vdir.mkdir()
    # 6 video pairs @ ~40 MB = ~240 MB (over old cap), incompressible.
    pairs = []
    for i in range(6):
        webm = vdir / f"S{i + 1}_cyber_cold.webm"
        gif = vdir / f"S{i + 1}_cyber_cold.gif"
        webm.write_bytes(os.urandom(38_000_000))
        gif.write_bytes(os.urandom(2_000_000))
        pairs.extend([webm, gif])

    ledger.write_run_bundle(tmp_path, [], per_stage_proofs={},
                            tag="t", scale=50000)

    for p in pairs:
        assert p.exists(), f"{p.name} retained regardless of video mode"
    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["bundle_truncated"] is False


def test_gzip_pod_logs_still_runs_and_retains_stem(tmp_path, monkeypatch):
    """Task #121 keeps the #299 gzip (it drops nothing, only compresses):
    a pod_log is gzipped to S6.txt.gz, the raw .txt removed, the saving
    recorded, and zcat/zgrep-style discoverability preserved via the stem."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    pdir = tmp_path / "pod_logs"
    pdir.mkdir()
    raw = _compressible_log_bytes(5_000_000)
    (pdir / "S6.txt").write_bytes(raw)

    ledger.write_run_bundle(tmp_path, [], per_stage_proofs={},
                            tag="t", scale=5000)

    gz = pdir / "S6.txt.gz"
    assert gz.exists(), "pod log must be gzipped as S6.txt.gz"
    assert not (pdir / "S6.txt").exists(), "raw .txt removed after gzip"
    # Round-trip: the gzip is readable and byte-identical to the original.
    import gzip as _gzip
    assert _gzip.open(gz, "rb").read() == raw
    over = json.loads((tmp_path / "oversize_bundle.json").read_text())
    gzentry = next(t for t in over["trimmed"]
                   if t["reason"] == "bundle_pod_logs_gzipped")
    assert gzentry["saved_bytes"] > 0
    assert over["dropped_count"] == 0


def test_no_pod_logs_writes_no_oversize_report(tmp_path, monkeypatch):
    """With no pod_logs to gzip and nothing to drop, the finalize step is a
    no-op: oversize_bundle.json is NOT written and bundle_truncated is
    False. (full_run.txt is only written on a live kubectl; mocked here to
    return nothing.)"""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    vdir = tmp_path / "videos"
    vdir.mkdir()
    (vdir / "S6_admin_cold.webm").write_bytes(b"\x00" * 100_000)

    ledger.write_run_bundle(tmp_path, [], per_stage_proofs={},
                            tag="t", scale=5000)

    assert (vdir / "S6_admin_cold.webm").exists()
    assert not (tmp_path / "oversize_bundle.json").exists(), (
        "no gzip + no drop → no oversize report")
    summary = json.loads((tmp_path / "summary.json").read_text())
    assert summary["bundle_truncated"] is False


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
    """Task #121: warm_p50 tier 500 -> 1000 (~1.09× worst clean 50K run 914).
    Cold tier 1000 -> 1300 (#121), then RE-BASELINED 1300 -> 2200 (Diego
    2026-06-11) to the measured floor of the lazy-context two-window cold
    methodology (mix-weighted ~2043-2070 across the clean 0.30.255-257
    validations; warm flat at ~911 proves it is methodology, not a serve
    regression). The warm/cold tiers are structurally frontend-gated
    (docs/task-288 + docs/task-300); 500/1000 stay aspirational, documented,
    NOT deleted."""
    assert ledger.WARM_P50_TIER_MS == 1000
    assert ledger.COLD_TIER_MS == 2200


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


def test_cold_boundary_2200_exact_no_miss_2201_misses():
    """Boundary: cold == 2200 is NOT > 2200 → no miss (clean warm/conv →
    PASS); cold == 2201 IS a miss → one tier → WEAK_PASS."""
    assert ledger.compute_verdict(_mw(890, 2200), restarts=0,
                                  conv_s8_p99=23800, cells=None) == "PASS"
    assert ledger.compute_verdict(_mw(890, 2201), restarts=0,
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
    Task #121 revision + the 2026-06-11 cold re-baseline (warm>1000,
    cold>2200)."""
    monkeypatch.setattr(ledger, "kubectl", lambda *a, **k: (1, "", ""))
    # warm 1100>1000 AND cold 2500>2200 → 2 misses → FAIL.
    all_results = [{"stage": "6", "cache": "ON", "pages": {"Dashboard": {
        "navigations": [
            {"user": "cyberjoker", "nav_num": 1, "cold_warm": "COLD",
             "waterfallMs": 2500, "validation": {"terminal_state": "pass"}},
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
    assert any(g.startswith("tier:cold ") and g.endswith(">2200")
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
