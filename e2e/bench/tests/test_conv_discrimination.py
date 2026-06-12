"""Task #334a + #334 tests — convergence-cell discrimination tuple +
S6 first-poll settle. Both are REPORT-ONLY telemetry; these tests assert
field sourcing, None-safety, persistence into the stage proof, AND prove the
verdict path (compute_verdict / convergence_p99_for_stage) is untouched.

Mocking seams mirror the established bench idiom:
  - browser._snowplow_pod_log_window     (kubectl logs window; conftest nulls it)
  - browser.read_snowplow_expvar_int     (HTTP /debug/vars; conftest wrapper)
  - phases.browser.verify_composition_count_api / phases.cluster.count_compositions
"""

from __future__ import annotations

import json

import bench.browser as browser
import bench.ledger as ledger
import bench.phases as phases
from bench.phases import StageContext

# Capture the REAL transport helper at import time (before the autouse
# conftest fixture `_stub_conv_discrimination_probes` nulls the module
# attribute). The two transport tests below rebind it explicitly — the same
# idiom the conftest docstring prescribes for probe-seam tests.
_REAL_POD_LOG_WINDOW = browser._snowplow_pod_log_window


# ─── (a)+(c): pod-log parsers ───────────────────────────────────────────────


_SAMPLE_LOG = "\n".join([
    "ts resolved_cache.summary subsystem=cache entries=10 "
    "dep_dirty_mark_total=100 refresh_completed=5",
    "ts cache_event.consumed subsystem=cache type=ADD gvr=g ns=a name=b "
    "action=refresh l1_keys=3",
    "ts cache_event.consumed subsystem=cache type=UPDATE gvr=g ns=a name=c "
    "l1_keys=2",
    "ts cache_event.consumed subsystem=cache type=DELETE gvr=g ns=a name=d "
    "l1_keys=1",
    "ts cache_event.consumed subsystem=cache type=CRD_ADD gvr=g ns=a name=e "
    "l1_keys=4",
    "ts cache_event.consumed subsystem=cache type=CRD_DELETE gvr=g ns=a "
    "name=f l1_keys=1",
    "ts resolved_cache.summary subsystem=cache entries=11 "
    "dep_dirty_mark_total=160 refresh_completed=9",
])


def test_parse_last_dep_dirty_mark_total_takes_last_occurrence():
    # Two summary lines; the LAST (closest to window-close) wins.
    assert browser._parse_last_dep_dirty_mark_total(_SAMPLE_LOG) == 160


def test_parse_last_dep_dirty_mark_total_none_safe():
    assert browser._parse_last_dep_dirty_mark_total(None) is None
    assert browser._parse_last_dep_dirty_mark_total("") is None
    assert browser._parse_last_dep_dirty_mark_total("no token here") is None


def test_count_addupdate_events_counts_add_update_crdadd_not_delete():
    # ADD + UPDATE + CRD_ADD = 3; DELETE / CRD_DELETE excluded.
    assert browser._count_addupdate_events(_SAMPLE_LOG) == 3


def test_count_addupdate_events_none_vs_zero_are_distinguishable():
    # None log (transport failure) -> None; present-but-empty -> 0.
    assert browser._count_addupdate_events(None) is None
    assert browser._count_addupdate_events("nothing relevant\n") == 0


def test_count_addupdate_events_ignores_non_consumed_lines_with_type():
    # A `type=ADD` token on a NON-cache_event.consumed line must NOT count.
    log = "ts some.other.event type=ADD foo=bar\n"
    assert browser._count_addupdate_events(log) == 0


# ─── tuple assembly + (b) slope ─────────────────────────────────────────────


def test_capture_tuple_computes_slope_and_sources_log_fields():
    t = browser.capture_conv_discrimination(
        "2026-06-12T00:00:00Z", 10.0,
        refresher_completed_before=5,
        refresher_completed_after=95,
        log_text=_SAMPLE_LOG)
    assert t["dep_dirty_mark_total"] == 160
    assert t["addupdate_events_in_window"] == 3
    assert t["refresh_completed_before"] == 5
    assert t["refresh_completed_after"] == 95
    assert t["refresh_completed_slope_per_s"] == 9.0  # (95-5)/10
    assert t["window_seconds"] == 10.0


def test_capture_tuple_all_none_when_expvars_and_log_unavailable():
    # cache-off / unreachable: every field None-safe (no exception).
    t = browser.capture_conv_discrimination(
        None, None,
        refresher_completed_before=None,
        refresher_completed_after=None,
        log_text=None)
    assert t == {
        "dep_dirty_mark_total": None,
        "refresh_completed_before": None,
        "refresh_completed_after": None,
        "refresh_completed_slope_per_s": None,
        "addupdate_events_in_window": None,
        "window_seconds": None,
    }


def test_capture_tuple_slope_none_on_zero_window_or_missing_snapshot():
    # window<=0 -> slope None even with both snapshots present.
    z = browser.capture_conv_discrimination(
        "x", 0, 5, 95, log_text=_SAMPLE_LOG)
    assert z["refresh_completed_slope_per_s"] is None
    # one snapshot missing -> slope None.
    m = browser.capture_conv_discrimination(
        "x", 10.0, None, 95, log_text=_SAMPLE_LOG)
    assert m["refresh_completed_slope_per_s"] is None


def test_capture_tuple_fetches_log_when_not_prefetched(monkeypatch):
    # When log_text is None but a window start is given, the helper fetches
    # via _snowplow_pod_log_window (here stubbed to the sample log).
    monkeypatch.setattr(browser, "_snowplow_pod_log_window",
                        lambda *a, **k: _SAMPLE_LOG)
    t = browser.capture_conv_discrimination(
        "2026-06-12T00:00:00Z", 10.0, 5, 95)  # no log_text kwarg
    assert t["dep_dirty_mark_total"] == 160
    assert t["addupdate_events_in_window"] == 3


# ─── _snowplow_pod_log_window transport seam ────────────────────────────────


def test_pod_log_window_returns_none_on_nonzero_rc(monkeypatch):
    import bench.cluster as cluster_mod
    # Exercise the REAL helper (conftest nulls the module attribute).
    monkeypatch.setattr(browser, "_snowplow_pod_log_window",
                        _REAL_POD_LOG_WINDOW)
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **k: (1, "", "err"))
    assert browser._snowplow_pod_log_window(since_iso="x") is None


def test_pod_log_window_returns_text_on_success(monkeypatch):
    import bench.cluster as cluster_mod
    monkeypatch.setattr(browser, "_snowplow_pod_log_window",
                        _REAL_POD_LOG_WINDOW)
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **k: (0, "L0\nL1", ""))
    assert browser._snowplow_pod_log_window(since_iso="x") == "L0\nL1"


# ─── phases: _collect_conv_discrimination aggregation ───────────────────────


def test_collect_conv_discrimination_buckets_by_user():
    results = [
        {"user": "admin", "pages": {"Dashboard": {"navigations": [
            {"convergence_ms": 23000,
             "conv_discrimination": {"dep_dirty_mark_total": 100}},
            {"nav_num": 1}]}}},
        {"user": "cyberjoker", "pages": {"Dashboard": {"navigations": [
            {"convergence_ms": 24000,
             "conv_discrimination": {"dep_dirty_mark_total": None}}]}}},
    ]
    got = phases._collect_conv_discrimination(results)
    assert set(got) == {"admin", "cyberjoker"}
    assert len(got["admin"]) == 1
    assert got["admin"][0]["dep_dirty_mark_total"] == 100


def test_collect_conv_discrimination_empty_and_none_safe():
    assert phases._collect_conv_discrimination([]) == {}
    assert phases._collect_conv_discrimination(None) == {}
    # Entries with no conv_discrimination tuple contribute nothing.
    assert phases._collect_conv_discrimination(
        [{"user": "admin", "pages": {"Dashboard": {"navigations": [
            {"convergence_ms": 1}]}}}]) == {}


# ─── phases: _s6_settle_convergence ─────────────────────────────────────────


def _mk_ctx(**kw) -> StageContext:
    return StageContext(tag="t", scale=50000, admin_token="tok", **kw)


def test_s6_settle_records_median_discard_first_and_stability(monkeypatch):
    # api climbs 100,100,200,200,200 toward a CONSTANT cluster=200 → equal on
    # samples 2,3,4 (0-indexed). cluster_count constant → stable True.
    api_seq = iter([100, 100, 200, 200, 200])
    monkeypatch.setattr(phases.browser, "verify_composition_count_api",
                        lambda tok: next(api_seq))
    monkeypatch.setattr(phases.cluster, "count_compositions", lambda: 200)
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    r = phases._s6_settle_convergence(_mk_ctx(), samples=5, interval_s=0)
    assert r["sample_count"] == 5
    assert r["equal_count"] == 3
    assert r["median_ms"] is not None
    assert r["discard_first_ms"] is not None
    assert r["cluster_count_stable"] is True
    assert [s["equal"] for s in r["samples"]] == [False, False, True, True, True]


def test_s6_settle_flags_unstable_cluster_count(monkeypatch):
    # cluster count still climbing (install not done) → stable False, and no
    # sample reaches equality → median/discard_first None.
    monkeypatch.setattr(phases.browser, "verify_composition_count_api",
                        lambda tok: 100)
    cl_seq = iter([150, 160, 170])
    monkeypatch.setattr(phases.cluster, "count_compositions",
                        lambda: next(cl_seq))
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    r = phases._s6_settle_convergence(_mk_ctx(), samples=3, interval_s=0)
    assert r["cluster_count_stable"] is False
    assert r["equal_count"] == 0
    assert r["median_ms"] is None
    assert r["discard_first_ms"] is None


def test_s6_settle_no_admin_token_returns_none_filled(monkeypatch):
    r = phases._s6_settle_convergence(
        StageContext(tag="t", scale=1), samples=3)
    assert r["reason"] == "no_admin_token"
    assert r["sample_count"] == 0
    assert r["median_ms"] is None


# ─── VERDICT-UNTOUCHED PROOF (the hard gate) ────────────────────────────────


def test_conv_discrimination_does_not_feed_convergence_p99_for_stage():
    # convergence_p99_for_stage reads ONLY nav["convergence_ms"]; the sibling
    # conv_discrimination tuple must be invisible to it. Two identical nav
    # sets — one WITH a tuple, one WITHOUT — must yield the same p99.
    def _entry(with_tuple: bool):
        nav = {"convergence_ms": 23000}
        if with_tuple:
            nav["conv_discrimination"] = {
                "dep_dirty_mark_total": 4_780_000,
                "refresh_completed_slope_per_s": 0.0,  # a "dropped slope"
                "addupdate_events_in_window": 9999,
            }
        return {"stage": "8", "cache": "ON",
                "pages": {"Dashboard": {"navigations": [nav]}}}

    with_t = ledger.convergence_p99_for_stage([_entry(True)], "8")
    without_t = ledger.convergence_p99_for_stage([_entry(False)], "8")
    assert with_t == without_t == 23000


def test_compute_verdict_ignores_conv_discrimination_and_settle():
    # compute_verdict's signature takes (mix_weighted, restarts, conv_s8_p99,
    # cells) — there is no parameter for the tuple or the settle metric, so a
    # PASS scenario stays PASS regardless of any tuple/settle content.
    mix = {"warm_p50_ms": 900, "cold_ms": 2000}
    assert ledger.compute_verdict(mix, 0, 23000) == "PASS"
    # A "dropped slope" / huge churn tuple cannot be passed in — proves the
    # gating surface never reads it. (Same call, no tuple arg accepted.)
    assert ledger.compute_verdict(mix, 0, 35000) == "WEAK_PASS"  # conv tier


# ─── persistence: S6 proof carries BOTH telemetry fields into state.json ────


class _NullStreamer:
    """No-op per-stage log streamer (mirrors test_phases_revguard)."""
    def __init__(self, *a, **k):
        self._running = type("E", (), {"is_set": lambda s: False})()

    def start(self):
        pass

    def stop(self):
        pass


def test_s6_proof_persists_conv_discrimination_and_settle(tmp_path, monkeypatch):
    """Task #334a + #334: stage_s6_scale_compositions writes BOTH
    `conv_discrimination` (per-user tuple aggregate) and `conv_settle_s6`
    into the stage proof, persisted to state.json so post-run analysis reads
    them directly. All heavy seams stubbed; this asserts WIRING + persistence.
    """
    # Stub the per-stage log streamer + deploy fingerprint (the latter is
    # already conftest-stubbed; _NullStreamer keeps the stage hermetic).
    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)

    # Cluster + lifecycle no-ops (scale path uses these).
    monkeypatch.setattr(phases.cluster, "count_compositions", lambda: 50000)
    monkeypatch.setattr(phases.lifecycle, "deploy_compositions_parallel",
                        lambda *a, **k: None)
    monkeypatch.setattr(phases.lifecycle, "wait_for_restaction_steady_state",
                        lambda *a, **k: None)
    monkeypatch.setattr(phases.browser, "wait_for_compositions",
                        lambda *a, **k: None)
    monkeypatch.setattr(phases.browser, "_poll_piechart_progression",
                        lambda *a, **k: None)
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    # _measure_all_users returns one admin entry whose Dashboard nav carries a
    # discrimination tuple (as browser_measure_stage would attach it).
    def _fake_measure(c, stage_num, stage_desc, deleted_ns=None):
        entry = {
            "user": "admin",
            "pages": {"Dashboard": {"navigations": [{
                "convergence_ms": 23000,
                "conv_discrimination": {
                    "dep_dirty_mark_total": 4_780_000,
                    "refresh_completed_before": 100,
                    "refresh_completed_after": 190,
                    "refresh_completed_slope_per_s": 9.0,
                    "addupdate_events_in_window": 2026,
                    "window_seconds": 10.0,
                }}]}},
        }
        c.all_results.append(entry)
        return [entry]
    monkeypatch.setattr(phases, "_measure_all_users", _fake_measure)

    # S6 settle re-poll: admin converged + stable cluster count.
    monkeypatch.setattr(phases.browser, "verify_composition_count_api",
                        lambda tok: 50000)

    ctx = StageContext(
        tag="0.30.x", scale=50000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        admin_token="tok", cache_mode="ON")

    proof = phases.stage_s6_scale_compositions(ctx)
    assert proof.passed is True

    # The proof dict carries BOTH telemetry fields.
    pd = proof.proof
    assert "conv_discrimination" in pd
    assert pd["conv_discrimination"]["admin"][0]["dep_dirty_mark_total"] == 4_780_000
    assert "conv_settle_s6" in pd
    assert pd["conv_settle_s6"]["sample_count"] == phases._S6_SETTLE_SAMPLES
    assert pd["conv_settle_s6"]["cluster_count_stable"] is True

    # ... and they are persisted into state.json under stage_proofs.S6.proof.
    state = json.loads((tmp_path / "state.json").read_text())
    s6 = state["stage_proofs"]["S6"]["proof"]
    assert s6["conv_discrimination"]["admin"][0]["addupdate_events_in_window"] == 2026
    assert s6["conv_settle_s6"]["median_ms"] is not None
    # The convergence_ms field the verdict path reads is UNCHANGED (sibling).
    assert (state["stage_proofs"]["S6"]["proof"]["conv_discrimination"]
            ["admin"][0].get("convergence_ms") is None)  # tuple has no conv_ms

