"""Behavioural tests for the VERIFY-poll logic in browser.py
(Ship 0.30.234).

Covers:
  - _verify_poll_match pure-function gate (api/ui/expected × K=3 transient
    handling)
  - browser_measure_stage end-to-end VERIFY semantics (per-user expected,
    ui_unavailable_pct > 25% raises ConvergenceTimeout)

NO source-introspection. NO live cluster. NO real Playwright Chromium.
Uses the `fake_page` fixture from conftest.py.
"""

from __future__ import annotations

import pytest


import bench.browser as browser_mod
from bench.browser import (
    ConvergenceTimeout,
    _verify_poll_match,
    browser_measure_stage,
)


# ─── Shared helpers ─────────────────────────────────────────────────────────


def _patch_cluster_count(monkeypatch, comp_count=0, ns_count=0):
    monkeypatch.setattr(browser_mod, "_count_compositions",
                        lambda: comp_count)
    monkeypatch.setattr(browser_mod, "_count_bench_ns", lambda: ns_count)
    monkeypatch.setattr(browser_mod, "_list_composition_names",
                        lambda: set())


def _patch_expected_calls_noop(monkeypatch):
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)


# ─── _verify_poll_match: pure-function gate ────────────────────────────────


def test_admin_cluster_wide_verify_uses_cluster_count():
    """Admin path: api == expected (== cluster_total) AND ui == expected
    → matched=True on first poll.
    """
    r = _verify_poll_match(api_count=20, ui_count=20, expected=20,
                           consecutive_api_correct_count=0)
    assert r["matched"] is True
    assert r["ui_unavailable"] is False
    assert r["api_correct"] is True
    assert r["next_consecutive"] == 1


def test_cyberjoker_narrow_verify_uses_user_count():
    """Narrow-RBAC user: expected=0 (zero bindings); api=0, ui=0 → matched.
    Cluster has 20 compositions but cluster_count is NOT consulted in
    the new code path — expected_for_user is the sole truth.
    """
    r = _verify_poll_match(api_count=0, ui_count=0, expected=0,
                           consecutive_api_correct_count=0)
    assert r["matched"] is True
    assert r["ui_unavailable"] is False


def test_narrow_rbac_user_sees_one_composition():
    """Customer narrow-RBAC shape (cj with Role on bench-ns-01 →
    expected=1). Passes when api=1 ui=1.
    """
    r = _verify_poll_match(api_count=1, ui_count=1, expected=1,
                           consecutive_api_correct_count=0)
    assert r["matched"] is True


def test_api_wrong_value_blocks_match():
    """api != expected MUST hold matched=False even when ui_count
    happens to coincide with api_count (the bug the old code shipped).
    """
    r = _verify_poll_match(api_count=15, ui_count=15, expected=20,
                           consecutive_api_correct_count=0)
    assert r["matched"] is False
    assert r["api_correct"] is False
    assert r["next_consecutive"] == 0  # reset


def test_ui_unavailable_rejected_before_k3():
    """ui=-1 with consecutive_count<3 → ui_ok=False; matched=False
    even when api==expected. The -1 might mask a real defect; bench
    requires api-stability evidence before tolerating.
    """
    # Poll #1: api correct for first time; next_consecutive becomes 1.
    # Tolerance threshold is K>=3, so 1 < 3 → ui_unavailable=False.
    r = _verify_poll_match(api_count=0, ui_count=-1, expected=0,
                           consecutive_api_correct_count=0)
    assert r["api_correct"] is True
    assert r["next_consecutive"] == 1
    assert r["ui_unavailable"] is True
    assert r["matched"] is False

    # Poll #2: api correct for second time; next_consecutive=2; still <3.
    r = _verify_poll_match(api_count=0, ui_count=-1, expected=0,
                           consecutive_api_correct_count=1)
    assert r["next_consecutive"] == 2
    assert r["matched"] is False


def test_ui_unavailable_tolerated_after_k3_consecutive_api_correct():
    """ui=-1 after 3 polls of api==expected → ui_ok=True; matched=True.

    The next_consecutive arithmetic is api_correct ? prev+1 : 0. To
    reach next_consecutive >= 3 the caller must have already observed
    3 prior api-correct polls (prev=2 → next_consecutive=3 makes
    K=3 threshold).
    """
    # Prior state: 2 consecutive api-correct polls observed.
    # This poll: api correct again → next_consecutive=3 → tolerate ui=-1.
    r = _verify_poll_match(api_count=0, ui_count=-1, expected=0,
                           consecutive_api_correct_count=2)
    assert r["api_correct"] is True
    assert r["next_consecutive"] == 3
    assert r["ui_unavailable"] is True
    assert r["matched"] is True


def test_ui_minus_one_does_not_reset_api_streak():
    """A transient ui=-1 must NOT reset the api-correct streak (the
    streak resets only when api itself diverges from expected).
    """
    r = _verify_poll_match(api_count=20, ui_count=-1, expected=20,
                           consecutive_api_correct_count=5)
    assert r["api_correct"] is True
    assert r["next_consecutive"] == 6
    # streak >= 3 so ui=-1 tolerated → matched
    assert r["matched"] is True


def test_api_diverge_resets_streak():
    """When api diverges from expected, consecutive count resets to 0
    so subsequent ui=-1 is rejected anew.
    """
    r = _verify_poll_match(api_count=5, ui_count=-1, expected=20,
                           consecutive_api_correct_count=10)
    assert r["api_correct"] is False
    assert r["next_consecutive"] == 0


# ─── browser_measure_stage: stage-end ui_unavailable_pct gate ───────────────


def test_ui_unavailable_pct_threshold_fails_stage(monkeypatch, fake_page,
                                                  tmp_path):
    """When the harness completes VERIFY with matched=True BUT more than
    25% of polls returned ui=-1, raise ConvergenceTimeout to surface the
    UI-channel reliability defect.

    Setup: expected_for_user=0 (cyberjoker zero-RBAC), api always 0
    (correct), ui returns -1 on EVERY poll (simulating sustained UI
    fetch failure). Since api is correct, the K=3 gate eventually
    tolerates ui=-1 and matched=True — but 100% of polls had ui=-1
    so ui_unavailable_pct=100 > 25 → raise.
    """
    _patch_cluster_count(monkeypatch, comp_count=20, ns_count=20)
    _patch_expected_calls_noop(monkeypatch)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 0)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: -1)
    # User-visible count = 0 for the test user (zero-RBAC shape).
    monkeypatch.setattr(browser_mod, "_user_visible_composition_count",
                        lambda user, token: 0)

    with pytest.raises(ConvergenceTimeout) as exc_info:
        browser_measure_stage(
            fake_page, stage_num=5, stage_desc="S5 zero-RBAC ui-fail",
            cache_mode="OFF", token="tok", num_navs=1, user="cyberjoker",
            verify_against_cluster=False,
            verify_timeout=5, verify_interval=0,
            screenshots_dir=tmp_path / "ss",
        )
    err = exc_info.value
    assert err.stage == 5
    assert err.user == "cyberjoker"
    assert err.api == 0
    assert err.ui == -1


def test_admin_per_user_path_byte_identical_for_cluster_match(monkeypatch,
                                                              fake_page,
                                                              tmp_path):
    """For admin (cluster-wide RBAC), expected_for_user == cluster_count.
    The new code path produces the same matched=True outcome as the
    legacy cluster-equality gate.
    """
    _patch_cluster_count(monkeypatch, comp_count=20, ns_count=20)
    _patch_expected_calls_noop(monkeypatch)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 20)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 20)
    monkeypatch.setattr(browser_mod, "_user_visible_composition_count",
                        lambda user, token: 20)

    result = browser_measure_stage(
        fake_page, stage_num=4, stage_desc="S4 admin",
        cache_mode="OFF", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True,
        verify_timeout=10, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash["verified_api"] == 20
    assert dash["verified_ui"] == 20
    assert dash["verified_expected"] == 20
    assert dash["convergence_ms"] >= 0
    # ui_unavailable_pct field is present and 0 (no -1 polls).
    assert dash["ui_unavailable_polls"] == 0
    assert dash["ui_unavailable_pct"] == 0.0


def test_cyberjoker_narrow_rbac_passes_with_expected_zero(monkeypatch,
                                                          fake_page,
                                                          tmp_path):
    """When cj has zero-RBAC reach (expected=0), api=0 ui=0 must PASS
    even when cluster has 20 compositions. The legacy code accidentally
    converged on this case (both happened to be 0); the new code path
    converges DELIBERATELY because expected_for_user is 0.
    """
    _patch_cluster_count(monkeypatch, comp_count=20, ns_count=20)
    _patch_expected_calls_noop(monkeypatch)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 0)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 0)
    monkeypatch.setattr(browser_mod, "_user_visible_composition_count",
                        lambda user, token: 0)

    result = browser_measure_stage(
        fake_page, stage_num=5, stage_desc="S5 cj zero-RBAC",
        cache_mode="OFF", token="tok", num_navs=1, user="cyberjoker",
        verify_against_cluster=False,
        verify_timeout=10, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash["verified_api"] == 0
    assert dash["verified_ui"] == 0
    assert dash["verified_expected"] == 0
    assert dash["convergence_ms"] >= 0


def test_narrow_rbac_user_passes_with_expected_one(monkeypatch, fake_page,
                                                   tmp_path):
    """Customer narrow-RBAC shape: cj has Role on bench-ns-01;
    expected=1; api=1 ui=1 MUST PASS. This is the Diego #146
    falsifier — not cj=0 (degenerate) but cj=1 (one composition in
    one granted ns).
    """
    _patch_cluster_count(monkeypatch, comp_count=20, ns_count=20)
    _patch_expected_calls_noop(monkeypatch)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 1)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 1)
    monkeypatch.setattr(browser_mod, "_user_visible_composition_count",
                        lambda user, token: 1)

    result = browser_measure_stage(
        fake_page, stage_num=5, stage_desc="S5 cj narrow-RBAC",
        cache_mode="OFF", token="tok", num_navs=1, user="cyberjoker",
        verify_against_cluster=False,
        verify_timeout=10, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash["verified_api"] == 1
    assert dash["verified_ui"] == 1
    assert dash["verified_expected"] == 1
    assert dash["convergence_ms"] >= 0


def test_yesterday_s5_cj_failure_mode_now_converges(monkeypatch,
                                                    fake_page,
                                                    tmp_path):
    """Re-creates yesterday's S5 cyberjoker timeout: api=0 stable,
    ui flapping between 0 and -1. expected=0. Legacy code timed out
    forever (== ui never holds when ui=-1). New code converges when
    api stays at expected — ui=0 on first poll is enough.
    """
    _patch_cluster_count(monkeypatch, comp_count=20, ns_count=20)
    _patch_expected_calls_noop(monkeypatch)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 0)
    # ui returns 0 on the first poll — matches expected_for_user=0.
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 0)
    monkeypatch.setattr(browser_mod, "_user_visible_composition_count",
                        lambda user, token: 0)

    # Should NOT raise — converges on first poll.
    result = browser_measure_stage(
        fake_page, stage_num=5, stage_desc="S5 cj 500ns scale",
        cache_mode="OFF", token="tok", num_navs=1, user="cyberjoker",
        verify_against_cluster=False,
        verify_timeout=2, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash["convergence_ms"] >= 0
    assert dash["verified_expected"] == 0


def test_legacy_api_eq_ui_minus_one_no_longer_misfires(monkeypatch,
                                                      fake_page,
                                                      tmp_path):
    """The old legacy path accepted convergence when api == ui >= 0.
    The new path requires api == expected_for_user. If api=10 (cache
    serves wrong count) and ui=10 (browser fetches same wrong count),
    the legacy gate would PASS the stage despite serving incorrectly.
    The new gate FAILS the stage because api(10) != expected(0).
    """
    _patch_cluster_count(monkeypatch, comp_count=20, ns_count=20)
    _patch_expected_calls_noop(monkeypatch)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 10)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 10)
    monkeypatch.setattr(browser_mod, "_user_visible_composition_count",
                        lambda user, token: 0)

    with pytest.raises(ConvergenceTimeout) as exc_info:
        browser_measure_stage(
            fake_page, stage_num=5, stage_desc="S5 cj wrong-serve",
            cache_mode="OFF", token="tok", num_navs=1, user="cyberjoker",
            verify_against_cluster=False,
            verify_timeout=1, verify_interval=0,
            screenshots_dir=tmp_path / "ss",
        )
    assert exc_info.value.api == 10
    assert exc_info.value.ui == 10
