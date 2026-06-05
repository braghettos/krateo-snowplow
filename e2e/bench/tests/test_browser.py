"""Behavioural tests for bench/browser.py.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.4. 13 cases.

NO source-introspection (`inspect.getsource`, `sys.modules[__name__]`).
NO live cluster. NO real Playwright Chromium launch — every test uses the
`fake_page` fixture (see conftest.py).

The ConvergenceTimeout test (acceptance criterion (h)) MUST use
`with pytest.raises(ConvergenceTimeout): browser_measure_stage(...)` —
log-string assertions are forbidden because an upstream `except:` could
swallow the exception while still letting the log message pass.
"""

from __future__ import annotations

import io
import json
import shutil
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path
from unittest import mock

import pytest


import bench.browser as browser_mod
from bench.browser import (
    BROWSER_SCALING_PAGES,
    ConvergenceTimeout,
    browser_measure_stage,
    http_get,
    http_get_json,
    login_all,
    make_browser_context,
    record_video_to_gif,
    verify_composition_count_api,
    verify_composition_count_ui,
    wait_for_compositions,
    _validate_widget_terminal_state,
)


# ─── helpers ────────────────────────────────────────────────────────────────


def _patch_cluster_count(monkeypatch, comp_count=0, ns_count=0,
                         panel_count=None):
    """Wire the deferred cluster.* probes used by browser_measure_stage.

    The browser module's _count_compositions / _count_bench_ns /
    _list_composition_names defer to bench.cluster.* via a lazy import
    inside the helper. We patch the wrapper, not bench.cluster, so the
    test never reaches the real kubectl path.

    Task #250 Block 2b: _validate_widget_terminal_state now also calls
    `_user_visible_composition_count` which in turn imports
    `bench.cluster.count_compositions_with_panels_ready`. Patch the
    cluster module entry so the new admin/cj path stays hermetic.
    `panel_count` defaults to None (the helper returns None → BASE
    expected fallback, matching pre-Block-2b behaviour for tests that
    don't care about the new gate).
    """
    monkeypatch.setattr(browser_mod, "_count_compositions", lambda: comp_count)
    monkeypatch.setattr(browser_mod, "_count_bench_ns", lambda: ns_count)
    monkeypatch.setattr(browser_mod, "_list_composition_names", lambda: set())
    # Hermetic isolation: prevent _user_visible_composition_count from
    # escaping to a real apiserver via the new cluster.* probe.
    from bench import cluster as _cluster_mod
    monkeypatch.setattr(_cluster_mod, "count_compositions_with_panels_ready",
                        lambda target_ns=None: panel_count)


# ─── Case 1: ConvergenceTimeout via pytest.raises (acceptance (h)) ──────────


def test_browser_measure_stage_raises_ConvergenceTimeout(monkeypatch, fake_page,
                                                         tmp_path):
    """VERIFY-poll deadline expiry with matched=False MUST raise
    ConvergenceTimeout (NOT log + silently set convergence_ms=-1).

    This is acceptance criterion (h). The assertion MUST use
    `with pytest.raises(ConvergenceTimeout)` — log-string assertions
    would still pass if an upstream `except:` swallowed the exception.
    """
    # Cluster has 1200 compositions; piechart returns 0 (cache stale).
    _patch_cluster_count(monkeypatch, comp_count=1200, ns_count=20)
    # Both verify probes return 0, never matching cluster=1200.
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 0)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 0)
    # _expected_calls_lookup returns None so no widget-validation failure.
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    with pytest.raises(ConvergenceTimeout) as exc_info:
        browser_measure_stage(
            fake_page, stage_num=6, stage_desc="S6 deploy 1200",
            cache_mode="ON", token="tok", num_navs=1, user="admin",
            verify_against_cluster=True,
            verify_timeout=1,  # 1-second deadline — guaranteed to expire
            verify_interval=0,  # no inter-poll sleep
            screenshots_dir=tmp_path / "ss",
        )

    err = exc_info.value
    assert err.stage == 6
    assert err.user == "admin"
    assert err.cluster == 1200
    assert err.api == 0
    assert err.ui == 0
    assert err.timeout_secs == 1


# ─── Case 2: PASS when api == ui == cluster ─────────────────────────────────


def test_browser_measure_stage_passes_when_api_equals_ui_equals_cluster(
        monkeypatch, fake_page, tmp_path):
    """Happy path: VERIFY poll matches on the first iteration → returns
    the stage dict with convergence_ms >= 0, no exception."""
    _patch_cluster_count(monkeypatch, comp_count=42, ns_count=2)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 42)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 42)
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    result = browser_measure_stage(
        fake_page, stage_num=2, stage_desc="S2 1ns + compdef",
        cache_mode="OFF", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=10, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    assert result["stage"] == 2
    assert result["compositions"] == 42
    # Dashboard nav must carry a non-negative convergence_ms.
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash["convergence_ms"] >= 0
    assert dash["verified_api"] == 42
    assert dash["verified_ui"] == 42


# ─── Case 3: cyberjoker uses intra-user consistency (worktree 6454-6462) ────


def test_browser_measure_stage_cyber_uses_intra_user_consistency(
        monkeypatch, fake_page, tmp_path):
    """When `verify_against_cluster=False` (cyberjoker), the gate is
    api == ui, NOT cluster equality. Cluster has 50K but cyberjoker
    sees 10 (RBAC scoped to one namespace); api == ui == 10 must PASS.
    """
    _patch_cluster_count(monkeypatch, comp_count=50_000, ns_count=50)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 10)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 10)
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    # NO pytest.raises — must PASS despite cluster=50K vs api/ui=10.
    result = browser_measure_stage(
        fake_page, stage_num=6, stage_desc="cj S6",
        cache_mode="ON", token="tok", num_navs=1, user="cyberjoker",
        verify_against_cluster=False,
        verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash["verified_api"] == 10
    assert dash["verified_ui"] == 10
    assert dash["convergence_ms"] >= 0


# ─── Case 4: record_video_to_gif produces a <=2 MB .gif ─────────────────────


def test_record_video_to_gif_produces_under_2mb_gif(monkeypatch, tmp_path):
    """Stub ffmpeg invocation; produce a 1 KB .gif on disk and assert
    the helper returns True + the file exists + size <= 2 MB."""
    webm = tmp_path / "S1_admin_cold_dashboard.webm"
    webm.write_bytes(b"FAKE_WEBM_HEADER" * 4)
    gif = tmp_path / "S1_admin_cold_dashboard.gif"

    monkeypatch.setattr(shutil, "which", lambda name: "/opt/homebrew/bin/ffmpeg"
                        if name == "ffmpeg" else None)

    def fake_ffmpeg(cmd, **kwargs):
        # cmd is the argv list; the last arg is the output gif path.
        out = Path(cmd[-1])
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_bytes(b"GIF89a" + b"\x00" * 1024)  # ~1 KB fake gif

        class _Proc:
            returncode = 0
            stderr = ""
            stdout = ""
        return _Proc()

    monkeypatch.setattr(subprocess, "run", fake_ffmpeg)

    assert record_video_to_gif(webm, gif, fps=4, max_seconds=60, max_mb=2) is True
    assert gif.exists()
    assert gif.stat().st_size < 2 * 1024 * 1024


# ─── Case 5: record_video_to_gif returns False when ffmpeg missing ──────────


def test_record_video_to_gif_returns_false_when_ffmpeg_missing(
        monkeypatch, tmp_path):
    """shutil.which('ffmpeg') is None → returns False, NEVER raises,
    leaves the cluster/run alone (gif not created)."""
    webm = tmp_path / "v.webm"
    webm.write_bytes(b"WEBM")
    gif = tmp_path / "v.gif"

    monkeypatch.setattr(shutil, "which", lambda name: None)

    assert record_video_to_gif(webm, gif) is False
    assert not gif.exists()


# ─── Case 6: record_video_to_gif rejects oversize output ────────────────────


def test_record_video_to_gif_returns_false_on_oversize_and_unlinks(
        monkeypatch, tmp_path):
    """When the produced .gif exceeds max_mb, the helper unlinks it
    and returns False (per R3.1 bundle-cap discipline)."""
    webm = tmp_path / "big.webm"
    webm.write_bytes(b"WEBM")
    gif = tmp_path / "big.gif"

    monkeypatch.setattr(shutil, "which", lambda name: "/usr/bin/ffmpeg"
                        if name == "ffmpeg" else None)

    def fake_ffmpeg(cmd, **kwargs):
        out = Path(cmd[-1])
        out.write_bytes(b"X" * (3 * 1024 * 1024))  # 3 MB

        class _Proc:
            returncode = 0
            stderr = ""
            stdout = ""
        return _Proc()

    monkeypatch.setattr(subprocess, "run", fake_ffmpeg)

    assert record_video_to_gif(webm, gif, max_mb=2) is False
    # Must have been deleted by the helper.
    assert not gif.exists()


# ─── Case 7: make_browser_context passes record_video_dir to Playwright ─────


def test_make_browser_context_passes_record_video_dir_to_playwright(tmp_path):
    """`record_video_dir=tmp_path/ "videos"` → forwarded to
    browser.new_context(record_video_dir=...). The directory is created
    if missing."""
    captured: dict = {}

    class FakeBrowser:
        def new_context(self, **kwargs):
            captured.update(kwargs)
            return mock.MagicMock(name="BrowserContext")

    videos = tmp_path / "videos" / "S1_admin"
    ctx = make_browser_context(FakeBrowser(), record_video_dir=videos)

    assert ctx is not None
    assert "record_video_dir" in captured, (
        f"new_context() did not receive record_video_dir; kwargs={captured!r}"
    )
    assert captured["record_video_dir"] == str(videos)
    assert videos.exists() and videos.is_dir(), (
        f"make_browser_context must create the video dir; "
        f"{videos} exists={videos.exists()}"
    )
    assert captured["viewport"] == {"width": 1280, "height": 900}
    assert captured["ignore_https_errors"] is True


def test_make_browser_context_skips_video_when_none(tmp_path):
    """`record_video_dir=None` → new_context() called WITHOUT the kwarg.

    Subsequent samples for the same cell pass None to stay inside R3.1's
    200 MB bundle cap.
    """
    captured: dict = {}

    class FakeBrowser:
        def new_context(self, **kwargs):
            captured.update(kwargs)
            return mock.MagicMock(name="BrowserContext")

    make_browser_context(FakeBrowser(), record_video_dir=None)
    assert "record_video_dir" not in captured, (
        f"new_context() received record_video_dir despite None; "
        f"kwargs={captured!r}"
    )


# ─── Case 8: terminal-state PASSes with zero skeletons ──────────────────────


def test_widget_terminal_state_passes_when_no_skeletons(monkeypatch,
                                                        fake_page):
    """`_validate_widget_terminal_state` returns terminal_state='pass'
    when skeleton_count == 0 AND /call count matches expectation."""
    fake_page._skeleton_scoped = 0
    fake_page._skeleton_raw = 0
    fake_page._skeleton_widget = 0
    fake_page._call_count = 16
    fake_page._errored_count = 0

    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: 16)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/dashboard",
                                        "label-cold", user="admin")
    assert v["terminal_state"] == "pass"
    assert v["skeleton_count"] == 0
    assert v["actual_calls"] == 16
    assert v["calls_within_tolerance"] is True


# ─── Case 9: terminal-state FAILs when skeletons persist ────────────────────


def test_widget_terminal_state_fails_when_skeletons_persist(monkeypatch,
                                                            fake_page):
    """skeleton_count > 0 → terminal_state='fail' (the stability poll
    exited prematurely; the page is still rendering)."""
    fake_page._skeleton_scoped = 3
    fake_page._skeleton_raw = 5
    fake_page._call_count = 16

    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: 16)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/dashboard", "label",
                                        user="admin")
    assert v["terminal_state"] == "fail"
    assert v["skeleton_count"] == 3


# ─── Case 10: scoped selector excludes Drawer/Notification false positives ──


def test_validate_widget_terminal_state_uses_scoped_selector_not_naked_ant_skeleton(
        monkeypatch, fake_page):
    """The widget validator counts SCOPED skeletons (excluding Drawer/
    Notification/Modal/Message/Popover/Dropdown/Tooltip surfaces). A
    page with raw=10 (Notification toast) but scoped=0 must PASS — a
    toast firing during nav must NOT produce a false positive.
    """
    fake_page._skeleton_scoped = 0    # NO widget-tree skeletons
    fake_page._skeleton_raw = 10      # raw includes overlay toasts
    fake_page._skeleton_widget = 0
    fake_page._call_count = 16

    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: 16)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/dashboard", "label",
                                        user="admin")
    assert v["terminal_state"] == "pass", (
        f"scoped=0 + raw=10 (overlay toasts) must PASS — "
        f"got terminal_state={v['terminal_state']!r}, validation={v!r}"
    )
    assert v["skeleton_count"] == 0
    assert v["skeleton_count_raw"] == 10


# ─── Case 11: login_all returns dict keyed by username ──────────────────────


def test_login_all_returns_dict_keyed_by_username(monkeypatch):
    """`login_all()` populates a {username: token} dict from USERS."""
    monkeypatch.setattr(browser_mod, "_ensure_users",
                        lambda: {"admin": "pw1", "cyberjoker": "pw2"})

    def fake_login(user, pw):
        return f"jwt-for-{user}"

    monkeypatch.setattr(browser_mod, "login", fake_login)

    tokens = login_all()
    assert tokens == {"admin": "jwt-for-admin",
                      "cyberjoker": "jwt-for-cyberjoker"}


# ─── Case 12: http_get returns response data via monkeypatched urlopen ──────


def test_http_get_returns_response_data(monkeypatch):
    """`http_get(path, token)` reads body via urllib.request.urlopen
    and returns (ms, code, body)."""
    fake_body = b'{"ok": true, "data": [1, 2, 3]}'

    class FakeResponse:
        status = 200
        headers = {"Content-Type": "application/json"}

        def __init__(self, body):
            self._body = body

        def read(self):
            return self._body

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=120):
        return FakeResponse(fake_body)

    monkeypatch.setattr(urllib.request, "urlopen", fake_urlopen)

    ms, code, body = http_get("/call?foo", "fake-token")
    assert code == 200
    assert body == fake_body
    assert ms >= 0


def test_http_get_json_parses_response(monkeypatch):
    """`http_get_json` decodes the body as JSON."""
    fake_body = b'{"items": [{"name": "comp-1"}, {"name": "comp-2"}]}'

    class FakeResponse:
        status = 200
        headers = {}

        def read(self):
            return fake_body

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    monkeypatch.setattr(urllib.request, "urlopen",
                        lambda req, timeout=120: FakeResponse())

    ms, code, parsed = http_get_json("/call?foo", "tok")
    assert code == 200
    assert isinstance(parsed, dict)
    assert parsed["items"][0]["name"] == "comp-1"


# ─── Case 13: wait_for_compositions / wait_for_compositions timeout ─────────


def test_wait_for_compositions_returns_true_when_count_at_target(monkeypatch):
    """When `count_compositions()` reaches `expected` immediately,
    `wait_for_compositions` returns True without waiting."""
    monkeypatch.setattr(browser_mod, "_count_compositions", lambda: 100)
    assert wait_for_compositions(100, timeout=5, tolerance=0) is True


def test_wait_for_compositions_returns_false_on_timeout(monkeypatch):
    """When `count_compositions()` never reaches `expected`,
    `wait_for_compositions` returns False after the deadline.

    Per `feedback_silent_skip_breaks_convergence_proof.md` (Block 5 memory
    file), helpers returning False on timeout MUST be checked by callers.
    This test pins the return type contract so the storm.run_user_scaling
    deferred-import wrapper continues to receive a bool.
    """
    monkeypatch.setattr(browser_mod, "_count_compositions", lambda: 0)
    # Skip the 5s inter-poll sleep so the test wall-clock stays ≤2s.
    monkeypatch.setattr(browser_mod.time, "sleep", lambda s: None)
    assert wait_for_compositions(100, timeout=1, tolerance=0) is False


# ─── Case 14: smoke video pipeline (per chosen smoke-gate option (b)) ───────


def test_smoke_video_pipeline(monkeypatch, tmp_path):
    """Smoke-gate replacement for `python -m bench phase6 --to-stage S1
    --video representative --tag 0.30.232` (per §G Block 3).

    Per the Block 3 dispatch §6 option (b), the smoke gate runs as an
    in-process unit test against fake Playwright + fake ffmpeg. Exercises:

      1. `make_browser_context(..., record_video_dir=videos/)` creates
         the directory and forwards the kwarg.
      2. `record_video_to_gif(webm, gif)` is invoked with a 0-rc ffmpeg
         stub and produces a small (<2 MB) gif on disk.

    Replacing the live smoke run with this test keeps Block 3 fully
    unit-testable + decoupled from Block 4's phases.py / state.json.
    Block 4 will replace this with a real S1 smoke as its own gate;
    until then, this is the falsifier for the criterion.
    """
    captured: dict = {}

    class FakeBrowser:
        def new_context(self, **kwargs):
            captured.update(kwargs)
            return mock.MagicMock(name="BrowserContext")

    # Step 1: make_browser_context with record_video_dir
    videos_dir = tmp_path / "videos"
    ctx = make_browser_context(FakeBrowser(), record_video_dir=videos_dir)
    assert ctx is not None
    assert captured.get("record_video_dir") == str(videos_dir)
    assert videos_dir.exists()

    # Step 2: simulate a recorded .webm landing on disk
    webm = videos_dir / "S1_admin_cold_dashboard.webm"
    webm.write_bytes(b"FAKE_WEBM" * 32)
    gif = videos_dir / "S1_admin_cold_dashboard.gif"

    # ffmpeg stub
    monkeypatch.setattr(shutil, "which", lambda n: "/opt/homebrew/bin/ffmpeg"
                        if n == "ffmpeg" else None)

    def fake_ffmpeg(cmd, **kwargs):
        out = Path(cmd[-1])
        out.write_bytes(b"GIF89a" + b"\x00" * 1024)

        class _Proc:
            returncode = 0
            stderr = ""
            stdout = ""
        return _Proc()

    monkeypatch.setattr(subprocess, "run", fake_ffmpeg)

    ok = record_video_to_gif(webm, gif, fps=4, max_seconds=60, max_mb=2)
    assert ok is True, "smoke video pipeline must produce a gif"
    assert gif.exists()
    assert gif.stat().st_size < 2 * 1024 * 1024


# ─── verify_composition_count_api error paths ───────────────────────────────


def test_verify_composition_count_api_returns_minus_one_on_non_200(
        monkeypatch):
    """Non-200 response → returns -1 (used to express "indeterminate")."""
    monkeypatch.setattr(browser_mod, "http_get_json",
                        lambda path, token: (50, 500, None))
    assert verify_composition_count_api("tok") == -1


def test_verify_composition_count_api_returns_count_on_200(monkeypatch):
    """200 with status.widgetData.title='1200' → returns 1200."""
    body = {"status": {"widgetData": {"title": "1200"}}}
    monkeypatch.setattr(browser_mod, "http_get_json",
                        lambda path, token: (12, 200, body))
    assert verify_composition_count_api("tok") == 1200


# ─── verify_composition_count_ui delegates to page.evaluate ─────────────────


def test_verify_composition_count_ui_reads_via_page_evaluate(fake_page):
    """`verify_composition_count_ui(page)` invokes page.evaluate with a
    JS string that fetches the piechart endpoint, and returns the JS
    result directly. FakePage's `evaluate` dispatches on the JS prefix
    `(async () => {` and returns `_ui_count`.
    """
    fake_page._ui_count = 1234
    assert verify_composition_count_ui(fake_page) == 1234


# ─── Task #181: CONTENT-DRIFT is RBAC-aware ─────────────────────────────────


def test_content_drift_uses_cluster_truth_for_admin(
        monkeypatch, fake_page, tmp_path):
    """When `verify_against_cluster=True` (admin), CONTENT-DRIFT compares
    `list_composition_names_from_cache(token)` against the full cluster
    via `_list_composition_names()` and sets `content_truth_source =
    "cluster"` on the navigation result.

    Admin's RBAC permits the cluster-wide LIST so cluster-truth is the
    authoritative comparison set.
    """
    truth_names = {f"ns{i}/comp{i}" for i in range(5)}
    _patch_cluster_count(monkeypatch, comp_count=len(truth_names), ns_count=2)
    monkeypatch.setattr(browser_mod, "_list_composition_names",
                        lambda: truth_names)
    monkeypatch.setattr(browser_mod, "list_composition_names_from_cache",
                        lambda token: set(truth_names))
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: len(truth_names))
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: len(truth_names))
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    result = browser_measure_stage(
        fake_page, stage_num=6, stage_desc="admin S6",
        cache_mode="ON", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash.get("content_match") is True, (
        f"admin CONTENT must PASS when cache == cluster-truth; got "
        f"{dash.get('content_match')!r}")
    assert dash.get("content_truth_source") == "cluster", (
        f"admin must compare against cluster-truth, got "
        f"{dash.get('content_truth_source')!r}")


def test_content_drift_uses_intra_user_api_for_cyber(
        monkeypatch, fake_page, tmp_path):
    """When `verify_against_cluster=False` (cyberjoker), CONTENT-DRIFT
    compares `len(list_composition_names_from_cache(token))` against the
    intra-user api_count (the same VERIFY-poll value) and sets
    `content_truth_source = "intra_user_api"`.

    Cluster has 50,000 compositions but cj sees 1,000 (RBAC scoped). The
    cluster-truth diff would spuriously report `missing=49000`; the
    RBAC-aware behaviour MUST instead PASS when cached_names == api_count.
    This is the task #181 regression — pre-fix, cj's CONTENT check always
    failed at SCALE=50K regardless of cache health.
    """
    cluster_truth = {f"ns{i}/comp{i}" for i in range(50_000)}
    cj_view = {f"bench-ns-1/bench-app-1-{i}" for i in range(1_000)}
    _patch_cluster_count(monkeypatch, comp_count=len(cluster_truth), ns_count=500)
    monkeypatch.setattr(browser_mod, "_list_composition_names",
                        lambda: cluster_truth)
    monkeypatch.setattr(browser_mod, "list_composition_names_from_cache",
                        lambda token: cj_view)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: len(cj_view))
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: len(cj_view))
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    result = browser_measure_stage(
        fake_page, stage_num=6, stage_desc="cj S6",
        cache_mode="ON", token="tok", num_navs=1, user="cyberjoker",
        verify_against_cluster=False, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash.get("content_match") is True, (
        f"cj CONTENT must PASS when cached_names == api_count even though "
        f"cluster-truth has 50K names; got content_match="
        f"{dash.get('content_match')!r}")
    assert dash.get("content_truth_source") == "intra_user_api", (
        f"cj must compare against intra-user api_count, got "
        f"{dash.get('content_truth_source')!r}")
    # No cluster-truth diff fields should appear for cj (regression
    # guard: pre-fix the symptom was `content_missing=49000` on every cj
    # CONTENT check).
    assert "content_missing" not in dash, (
        "cj must NOT emit `content_missing` (cluster-truth diff field) — "
        "task #181 regression guard")
    assert "content_extra" not in dash, (
        "cj must NOT emit `content_extra` (cluster-truth diff field) — "
        "task #181 regression guard")


def test_content_drift_cyber_flags_drift_when_cache_diverges_from_api(
        monkeypatch, fake_page, tmp_path):
    """Even with the RBAC-aware fix, real cache corruption MUST still
    flag CONTENT ✗ for cj. Construct a scenario where the cached name
    set has the WRONG count vs api/ui (the only intra-user signal we
    can compare against) and assert `content_match=False`.

    Note: api_count and ui_count must still match (otherwise VERIFY
    raises ConvergenceTimeout BEFORE the CONTENT block runs). Only the
    cached-name count diverges. This is the residual-defect case the
    fix MUST still catch.
    """
    cj_view_cached = {f"bench-ns-1/bench-app-1-{i}" for i in range(900)}  # cache says 900
    _patch_cluster_count(monkeypatch, comp_count=50_000, ns_count=500)
    monkeypatch.setattr(browser_mod, "_list_composition_names",
                        lambda: {f"ns/c{i}" for i in range(50_000)})
    monkeypatch.setattr(browser_mod, "list_composition_names_from_cache",
                        lambda token: cj_view_cached)
    # api == ui == 1000 (VERIFY passes), but the names endpoint reports 900 → drift
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 1000)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 1000)
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    result = browser_measure_stage(
        fake_page, stage_num=6, stage_desc="cj S6 cache drift",
        cache_mode="ON", token="tok", num_navs=1, user="cyberjoker",
        verify_against_cluster=False, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash.get("content_match") is False
    assert dash.get("content_truth_source") == "intra_user_api"
    assert dash.get("content_cached_count") == 900
    assert dash.get("content_api_count") == 1000


# ─── Task #250 Block 2 — Phase 6 S8/S9 probes ──────────────────────────────


def test_read_snowplow_expvar_int_returns_int_on_200(monkeypatch):
    """Happy path: /debug/vars returns 200 + JSON dict with int value
    → integer returned (not float, not None).
    """
    import bench.browser as browser_mod

    captured_urls = []

    class _FakeResp:
        status = 200
        headers = {}

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def read(self):
            return b'{"snowplow_rbac_publish_seq": 414, "other": "x"}'

    def _fake_urlopen(req, timeout=None):
        captured_urls.append(req.full_url)
        return _FakeResp()

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        _fake_urlopen)
    out = browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081")
    assert out == 414
    assert captured_urls == ["http://fake:8081/debug/vars"]


def test_read_snowplow_expvar_int_returns_none_on_transport_failure(
        monkeypatch):
    """Network failure / unreachable host → None (fail closed). Caller
    interprets None as 'cannot read', NOT as 'value is 0'.
    """
    import bench.browser as browser_mod

    def _fake_urlopen(req, timeout=None):
        raise OSError("connection refused")

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        _fake_urlopen)
    out = browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081")
    assert out is None


def test_read_snowplow_expvar_int_returns_none_on_missing_key(monkeypatch):
    """200 + JSON dict without the requested key → None (caller fails
    closed; do not confuse 'absent' with '0').
    """
    import bench.browser as browser_mod

    class _FakeResp:
        status = 200
        headers = {}

        def __enter__(self): return self
        def __exit__(self, *a): return False
        def read(self):
            return b'{"other_key": 1}'

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        lambda req, timeout=None: _FakeResp())
    out = browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081")
    assert out is None


def test_read_snowplow_expvar_int_rejects_bool_value(monkeypatch):
    """True/False at the key (which int() would coerce to 1/0) returns
    None — a wrong-shape value on the snowplow side surfaces explicitly.
    """
    import bench.browser as browser_mod

    class _FakeResp:
        status = 200
        headers = {}

        def __enter__(self): return self
        def __exit__(self, *a): return False
        def read(self):
            return b'{"snowplow_rbac_publish_seq": true}'

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        lambda req, timeout=None: _FakeResp())
    assert browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081") is None


def test_count_user_compositions_in_ns_returns_minus1_when_no_token():
    """Missing token → -1 sentinel (re-gate v3: ns argument no longer
    in the URL, so empty ns is no longer rejected — caller may pass
    "" as a label-only field).
    """
    import bench.browser as browser_mod
    assert browser_mod.count_user_compositions_in_ns(
        "cyberjoker", None, "bench-ns-01") == -1


def test_count_user_compositions_in_ns_uses_piechart_widget_url(monkeypatch):
    """Re-gate v3 (2026-06-05): Probe B calls the piechart-widget
    endpoint `resource=piecharts&name=dashboard-compositions-panel-
    row-piechart` (NOT the raw composition GVR list which lacks
    `name=` and returns 400). The shape mirrors
    verify_composition_count_api so both probes share the same
    /call path the dashboard piechart already uses.

    Asserts:
      1. URL includes `name=dashboard-compositions-panel-row-piechart`.
      2. URL includes `resource=piecharts`.
      3. URL does NOT include the raw composition GVR (no
         resource=githubscaffoldingwithcompositionpages).
      4. Returns int(title) parsed from
         body.status.widgetData.title (piechart contract).
    """
    import bench.browser as browser_mod

    captured = {}

    def _fake_http_get_json(path, token, **kw):
        captured["path"] = path
        captured["token"] = token
        return (0, 200, {
            "status": {"widgetData": {"title": "999"}},
        })

    monkeypatch.setattr(browser_mod, "http_get_json", _fake_http_get_json)
    out = browser_mod.count_user_compositions_in_ns(
        "cyberjoker", "tok", "bench-ns-01")
    assert out == 999
    assert captured["token"] == "tok"
    # Falsifier #1: the piechart widget name MUST be in the URL.
    assert "name=dashboard-compositions-panel-row-piechart" \
        in captured["path"], \
        f"Probe B URL must hit the piechart RESTAction by name; " \
        f"got: {captured['path']!r}"
    # Falsifier #2: resource is piecharts (widget endpoint), NOT the
    # raw composition GVR which would 400.
    assert "resource=piecharts" in captured["path"]
    assert "githubscaffoldingwithcompositionpages" not in captured["path"]


def test_count_user_compositions_in_ns_returns_minus1_on_non200(monkeypatch):
    """Non-200 response → -1 sentinel (poll continues)."""
    import bench.browser as browser_mod
    monkeypatch.setattr(browser_mod, "http_get_json",
                        lambda path, token, **kw: (0, 401, {}))
    assert browser_mod.count_user_compositions_in_ns(
        "cyberjoker", "tok", "bench-ns-01") == -1


def test_count_user_compositions_in_ns_returns_minus1_on_missing_title(
        monkeypatch):
    """200 but body lacks status.widgetData.title → -1 (caller falls
    back / poll continues). Guards against snowplow shape changes.
    """
    import bench.browser as browser_mod
    monkeypatch.setattr(browser_mod, "http_get_json",
                        lambda path, token, **kw: (0, 200, {"status": {}}))
    assert browser_mod.count_user_compositions_in_ns(
        "cyberjoker", "tok", "bench-ns-01") == -1


def test_user_visible_composition_count_returns_none_for_non_compositions(
        monkeypatch):
    """The N-aware formula only applies to /compositions; other paths
    return None so the lookup falls back to BASE.
    """
    import bench.browser as browser_mod
    out = browser_mod._user_visible_composition_count(
        "admin", "/dashboard", token=None)
    assert out is None


def test_user_visible_composition_count_admin_uses_panel_count(monkeypatch):
    """admin path goes through cluster.count_compositions_with_panels_ready
    (Task #250 Block 2b re-gate fix 2026-06-05). The empirical source
    for n_visible on /compositions is the COMP-PANEL count, not the
    raw composition CR count — comp-panels are what the /compositions
    datagrid iterates over (TRACED via the compositions-panels
    RESTAction at krateo-system). Hermetic isolation: monkeypatch the
    cluster.* helper so this test NEVER hits a real apiserver.
    """
    import bench.browser as browser_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(cluster_mod, "count_compositions_with_panels_ready",
                        lambda target_ns=None: 4423)
    out = browser_mod._user_visible_composition_count(
        "admin", "/compositions", token=None)
    assert out == 4423


def test_user_visible_composition_count_admin_returns_none_on_transport_fail(
        monkeypatch):
    """Transport failure inside count_compositions_with_panels_ready →
    None at this layer → expected_calls falls back to BASE."""
    import bench.browser as browser_mod
    from bench import cluster as cluster_mod

    def _raise(target_ns=None):
        raise RuntimeError("apiserver unreachable")

    monkeypatch.setattr(cluster_mod, "count_compositions_with_panels_ready",
                        _raise)
    out = browser_mod._user_visible_composition_count(
        "admin", "/compositions", token=None)
    assert out is None


def test_user_visible_composition_count_non_admin_with_target_ns(monkeypatch):
    """Non-admin path WITH target_ns goes through
    count_compositions_with_panels_ready(target_ns=...) — admin and
    narrow-RBAC users use the SAME mechanism for /compositions.
    """
    import bench.browser as browser_mod
    from bench import cluster as cluster_mod

    captured = {}

    def _fake_count(target_ns=None):
        captured["target_ns"] = target_ns
        return 143  # bench-ns-01 comp-panel count from 2026-06-05 probe

    monkeypatch.setattr(cluster_mod, "count_compositions_with_panels_ready",
                        _fake_count)
    out = browser_mod._user_visible_composition_count(
        "cyberjoker", "/compositions", token="tok",
        target_ns="bench-ns-01")
    assert out == 143
    assert captured["target_ns"] == "bench-ns-01"


def test_user_visible_composition_count_non_admin_falls_back_to_piechart(
        monkeypatch):
    """Non-admin WITHOUT target_ns falls back to
    verify_composition_count_api (pre-Block-2b shape; still correct
    for cj at S1-S7 where no grant means piechart=0 → BASE).
    """
    import bench.browser as browser_mod
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 999)
    out = browser_mod._user_visible_composition_count(
        "cyberjoker", "/compositions", token="tok")
    assert out == 999

    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: -1)
    assert browser_mod._user_visible_composition_count(
        "cyberjoker", "/compositions", token="tok") is None


def test_user_visible_composition_count_non_admin_without_token(monkeypatch):
    """Non-admin without token AND without target_ns → None (legacy
    BASE behaviour preserved)."""
    import bench.browser as browser_mod
    assert browser_mod._user_visible_composition_count(
        "cyberjoker", "/compositions", token=None) is None
