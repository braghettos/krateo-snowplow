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


def _patch_cluster_count(monkeypatch, comp_count=0, ns_count=0):
    """Wire the deferred cluster.* probes used by browser_measure_stage.

    The browser module's _count_compositions / _count_bench_ns /
    _list_composition_names defer to bench.cluster.* via a lazy import
    inside the helper. We patch the wrapper, not bench.cluster, so the
    test never reaches the real kubectl path.
    """
    monkeypatch.setattr(browser_mod, "_count_compositions", lambda: comp_count)
    monkeypatch.setattr(browser_mod, "_count_bench_ns", lambda: ns_count)
    monkeypatch.setattr(browser_mod, "_list_composition_names", lambda: set())


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
                        lambda u, p: None)
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
                        lambda u, p: None)
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
                        lambda u, p: None)
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
                        lambda u, p: 16)
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
                        lambda u, p: 16)
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
                        lambda u, p: 16)
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
