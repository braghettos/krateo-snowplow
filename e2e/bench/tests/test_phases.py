"""Tests for bench.phases — stage runner, state.json, --from-stage semantics.

Covers §C.7 (~8 cases) PLUS the critical R3.2 mitigation test that proves
ConvergenceTimeout-from-stage persists state.json BEFORE the re-raise
(see plan §G Block 4 G3).

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.7 + §G Block 4.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from bench import phases
from bench.browser import ConvergenceTimeout
from bench.phases import (
    StageContext,
    StageProof,
    STAGE_REGISTRY,
    STAGE_ORDER,
    load_state,
    save_state,
    _run_stage,
    _stages_in_window,
    _validate_resume,
    IncompatibleStateSchema,
    SCHEMA_VERSION,
)


# ─── STAGE_REGISTRY shape ───────────────────────────────────────────────────


def test_stage_registry_contains_S0_through_S11():
    # Task #250 Block 2 / SCHEMA_VERSION 1.1.0: new S8 (RB-add) +
    # S9 (RB-remove) inserted after S7; old S8/S9 renamed to S10/S11.
    assert set(STAGE_REGISTRY.keys()) == {
        "S0", "S1", "S2", "S3", "S4", "S5", "S6", "S7",
        "S8", "S9", "S10", "S11",
    }
    assert STAGE_ORDER == ["S0", "S1", "S2", "S3", "S4",
                           "S5", "S6", "S7", "S8", "S9", "S10", "S11"]


# ─── --from-stage / --to-stage semantics ────────────────────────────────────


def test_from_stage_S5_skips_S1_S2_S3_S4():
    window = _stages_in_window("S5", "S8")
    assert window == ["S5", "S6", "S7", "S8"]
    assert "S1" not in window and "S4" not in window


def test_to_stage_S11_includes_S0_through_S11_inclusive():
    # SCHEMA 1.1.0 — the report stage is now S11. Operators selecting
    # the full window via --to-stage S11 must see all 12 stages.
    window = _stages_in_window(None, "S11")
    assert window == ["S0", "S1", "S2", "S3", "S4",
                      "S5", "S6", "S7", "S8", "S9", "S10", "S11"]


def test_to_stage_S3_stops_after_S3():
    window = _stages_in_window(None, "S3")
    assert window == ["S0", "S1", "S2", "S3"]


def test_to_stage_S1_window_is_S0_S1():
    window = _stages_in_window(None, "S1")
    assert window == ["S0", "S1"]


# ─── state.json schema + assertion (R4.1) ──────────────────────────────────


def test_save_state_rejects_empty_what_breaks_if_skipped(tmp_path):
    state = {
        "schema_version": SCHEMA_VERSION,
        "tag": "t", "scale": 5000,
        "stages_completed": ["S0"],
        "stage_proofs": {
            "S0": {
                "stage_id": "S0", "passed": True,
                "what_breaks_if_skipped": "",  # <- forbidden empty
                "started_at": "x", "ended_at": "y",
                "proof": {}, "artifacts": [],
            },
        },
    }
    with pytest.raises(AssertionError):
        save_state(tmp_path, state)


def test_save_state_accepts_non_empty_what_breaks(tmp_path):
    state = {
        "schema_version": SCHEMA_VERSION,
        "tag": "t", "scale": 5000,
        "stages_completed": ["S0"],
        "stage_proofs": {
            "S0": {
                "stage_id": "S0", "passed": True,
                "what_breaks_if_skipped": "non-empty",
                "started_at": "x", "ended_at": "y",
                "proof": {}, "artifacts": [],
            },
        },
    }
    save_state(tmp_path, state)
    assert (tmp_path / "state.json").exists()


def test_load_state_returns_empty_when_missing(tmp_path):
    assert load_state(tmp_path) == {}


def test_load_state_rejects_missing_schema_version(tmp_path):
    (tmp_path / "state.json").write_text(json.dumps({"tag": "t"}))
    with pytest.raises(IncompatibleStateSchema):
        load_state(tmp_path)


def test_load_state_rejects_future_major(tmp_path):
    (tmp_path / "state.json").write_text(json.dumps({
        "schema_version": "2.0.0", "tag": "t",
    }))
    with pytest.raises(IncompatibleStateSchema):
        load_state(tmp_path)


# ─── _validate_resume ──────────────────────────────────────────────────────


def test_validate_resume_passes_when_only_prior_stages_present():
    state = {"stages_completed": ["S0", "S1"]}
    ok, _ = _validate_resume(state, "S2")
    assert ok


def test_validate_resume_blocks_when_later_stages_present():
    state = {"stages_completed": ["S0", "S1", "S5"]}
    ok, diag = _validate_resume(state, "S2")
    assert not ok
    assert "S5" in diag


# ─── _run_stage persist-before-raise contract (G3 / R3.2) ──────────────────


def test_stage_runner_persists_proof_then_raises_ConvergenceTimeout(tmp_path):
    """CRITICAL Block 4 acceptance G3 + R3.2.

    Proves that when a stage body raises ConvergenceTimeout, _run_stage:
      1. Builds a StageProof with passed=False, convergence_timeout=True
      2. Persists $run_dir/state.json + $run_dir/proofs/Sx.json BEFORE the raise
      3. Re-raises ConvergenceTimeout so the CLI can exit 4
    """
    ctx = StageContext(
        tag="0.30.232", scale=5000,
        run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        cache_mode="ON",
    )

    def _bad_work(c):
        raise ConvergenceTimeout(
            stage=6, user="cyberjoker",
            api=99, ui=100, cluster=200, timeout_secs=300,
        )

    with pytest.raises(ConvergenceTimeout):
        _run_stage("S6", ctx, _bad_work,
                   what_breaks_if_skipped="S6 SCALE anchor")

    # Proof file persisted before the raise propagated.
    assert (tmp_path / "state.json").exists()
    assert (tmp_path / "proofs" / "S6.json").exists()
    proof = json.loads((tmp_path / "proofs" / "S6.json").read_text())
    assert proof["passed"] is False
    assert proof["convergence_timeout"] is True
    assert proof["proof"]["api"] == 99
    assert proof["proof"]["user"] == "cyberjoker"

    state = json.loads((tmp_path / "state.json").read_text())
    assert state["current_stage"] == "S6"
    # The failing stage MUST NOT be in stages_completed (passed=False).
    assert "S6" not in state.get("stages_completed", [])


# ─── _run_stage success path ────────────────────────────────────────────────


def test_stage_runner_writes_proof_on_success(tmp_path):
    ctx = StageContext(
        tag="0.30.232", scale=5000,
        run_dir=tmp_path,
        state_path=tmp_path / "state.json",
    )

    def _good_work(c):
        return {"ns_count": 1, "compdef_ready": True}

    proof = _run_stage("S2", ctx, _good_work,
                       what_breaks_if_skipped="S4 needs the CRD")
    assert proof.passed is True
    assert proof.convergence_timeout is False
    state = json.loads((tmp_path / "state.json").read_text())
    assert state["stages_completed"] == ["S2"]
    assert state["stage_proofs"]["S2"]["proof"]["ns_count"] == 1


# ─── what_breaks_if_skipped is non-empty for every registry stage ──────────


def test_stage_proof_records_what_would_be_wrong_if_skipped(tmp_path):
    """Every stage in STAGE_REGISTRY MUST set a non-empty
    what_breaks_if_skipped via the _run_stage wrapper.

    Acceptance criterion §I Block 4 R4.1: empty rejected by save_state.
    Here we sanity-check by inspecting the SOURCE TEXT for the registry
    callable — looking for the kwarg value at the _run_stage call site.
    A simpler proxy: monkey-patch _run_stage to capture the kwarg.
    """
    seen: dict[str, str] = {}

    real_run_stage = phases._run_stage

    def _capture(stage_id, ctx, work, what_breaks_if_skipped, *,
                 artifacts=None):
        seen[stage_id] = what_breaks_if_skipped
        # Don't run the work — only capture the kwarg.
        return StageProof(
            stage_id=stage_id, started_at="", ended_at="", passed=True,
            proof={}, artifacts=[],
            what_breaks_if_skipped=what_breaks_if_skipped)

    phases._run_stage = _capture
    try:
        ctx = StageContext(
            tag="t", scale=5000, run_dir=tmp_path,
            state_path=tmp_path / "state.json",
        )
        for sid, fn in STAGE_REGISTRY.items():
            fn(ctx)
        for sid in STAGE_REGISTRY:
            assert seen.get(sid), \
                f"stage {sid} did not call _run_stage with what_breaks_if_skipped"
            assert len(seen[sid]) > 10, \
                f"stage {sid} has trivially short what_breaks_if_skipped"
    finally:
        phases._run_stage = real_run_stage


# ─── Resume reads disk state ────────────────────────────────────────────────


def test_run_state_resume_uses_disk_state_when_present(tmp_path):
    """A pre-existing state.json with stages_completed=['S0'] must be
    visible to load_state."""
    state = {
        "schema_version": SCHEMA_VERSION,
        "tag": "0.30.232", "scale": 5000,
        "started_at": "2026-06-02T00:00:00+00:00",
        "stages_completed": ["S0"],
        "stage_proofs": {
            "S0": {
                "stage_id": "S0", "started_at": "x", "ended_at": "y",
                "passed": True, "proof": {}, "artifacts": [],
                "what_breaks_if_skipped": "preflight underwrites everything",
            },
        },
    }
    save_state(tmp_path, state)
    reloaded = load_state(tmp_path)
    assert reloaded["stages_completed"] == ["S0"]
    assert "S0" in reloaded["stage_proofs"]


# ─── Schema constant freeze ─────────────────────────────────────────────────


def test_schema_version_is_frozen_at_1_1_0():
    # Task #250 Block 2 minor bump from 1.0.0: additive S8/S9 RBAC
    # stages + N-aware EXPECTED_CALLS. SCHEMA_MAJOR unchanged at 1,
    # so resume from 1.0.0 state.json still works (additive fields
    # are read-by-key, no structural break).
    assert SCHEMA_VERSION == "1.1.0"
    assert phases.SCHEMA_MAJOR == 1


# ─── G6 wiring fix: _setup_users honours ctx.video ─────────────────────────


class _FakePage:
    """Minimal Playwright Page stand-in for _setup_users tests."""
    url = "http://fake/login"
    def goto(self, *a, **k): pass
    def click(self, *a, **k): pass
    def wait_for_load_state(self, *a, **k): pass
    def wait_for_timeout(self, *a, **k): pass
    def evaluate(self, *a, **k): return True

    @property
    def keyboard(self):
        class _KB:
            def type(self, *a, **k): pass
        return _KB()


class _FakeCtx:
    def new_page(self): return _FakePage()
    def close(self): pass


class _FakeBrowser:
    def new_context(self, **kwargs): return _FakeCtx()
    def close(self): pass


class _FakePW:
    def __init__(self):
        self.chromium = type("_C", (), {
            "launch": lambda self_inner, headless=True: _FakeBrowser(),
        })()

    def start(self): return self
    def stop(self): pass


def _install_fake_playwright(monkeypatch):
    """Insert a fake playwright.sync_api so _setup_users imports succeed."""
    import sys as _sys
    fake_mod = type(_sys)("playwright.sync_api")
    fake_mod.sync_playwright = lambda: _FakePW()
    monkeypatch.setitem(_sys.modules, "playwright",
                        type(_sys)("playwright"))
    monkeypatch.setitem(_sys.modules, "playwright.sync_api", fake_mod)


def test_setup_users_passes_record_video_dir_when_video_flag_set(
        tmp_path, monkeypatch):
    """Per team-lead G6 NACK: _setup_users MUST thread ctx.video into
    browser.make_browser_context(record_video_dir=...) when video !=
    "none". Without this, Playwright never writes .webm files to disk.

    Falsifier: monkey-patch browser.make_browser_context to capture
    kwargs; assert `record_video_dir` was supplied AND points under
    ctx.run_dir/videos/.
    """
    _install_fake_playwright(monkeypatch)
    captured: list[dict] = []

    def _capture(pw_browser, **kwargs):
        captured.append(dict(kwargs))
        return _FakeCtx()

    monkeypatch.setattr(phases.browser, "make_browser_context", _capture)
    monkeypatch.setattr(phases.browser, "_ensure_users",
                        lambda: {"admin": "x", "cyberjoker": "y"})
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)

    ctx = phases.StageContext(
        tag="t", scale=5000,
        run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        video="representative",
        user_pages={"__subjects__": ["admin", "cyberjoker"]},
    )
    phases._setup_users(ctx)

    assert len(captured) == 2, (
        f"expected 2 make_browser_context calls (admin + cyberjoker); "
        f"got {len(captured)}")
    for cap in captured:
        assert "record_video_dir" in cap, (
            f"make_browser_context called WITHOUT record_video_dir: {cap}")
        rvd = cap["record_video_dir"]
        assert Path(rvd) == tmp_path / "videos", (
            f"record_video_dir={rvd!r} not under {tmp_path / 'videos'}")
    assert (tmp_path / "videos").is_dir()


def test_setup_users_does_not_record_when_video_none(tmp_path, monkeypatch):
    """When ctx.video == "none", _setup_users MUST NOT supply
    record_video_dir (Playwright defaults: no recording)."""
    _install_fake_playwright(monkeypatch)
    captured: list[dict] = []

    def _capture(pw_browser, **kwargs):
        captured.append(dict(kwargs))
        return _FakeCtx()

    monkeypatch.setattr(phases.browser, "make_browser_context", _capture)
    monkeypatch.setattr(phases.browser, "_ensure_users",
                        lambda: {"admin": "x"})
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)

    ctx = phases.StageContext(
        tag="t", scale=5000,
        run_dir=tmp_path, state_path=tmp_path / "state.json",
        video="none",
        user_pages={"__subjects__": ["admin"]},
    )
    phases._setup_users(ctx)
    assert captured
    for cap in captured:
        assert cap.get("record_video_dir") is None, (
            f"record_video_dir should be None for video='none'; got {cap}")


def test_run_phase6_threads_video_into_stage_context(tmp_path, monkeypatch):
    """End-to-end: run_phase6(video='all') must construct a StageContext
    whose .video == 'all', as observed by _setup_users."""
    seen_video: list[str] = []

    def _spy_setup(ctx):
        seen_video.append(ctx.video)

    monkeypatch.setattr(phases, "_setup_users", _spy_setup)
    monkeypatch.setattr(phases, "_teardown_users", lambda ctx: None)
    monkeypatch.setattr(
        phases, "_attach_video_artifacts_to_last_measurement_proof",
        lambda ctx: None)
    monkeypatch.setattr(phases.browser, "FRONTEND", "http://fake")
    monkeypatch.setattr(phases.browser, "login_all", lambda: {})

    def _fake_stage(sid):
        def _f(ctx):
            return phases.StageProof(
                stage_id=sid, started_at="", ended_at="",
                passed=True, proof={}, artifacts=[],
                what_breaks_if_skipped=f"{sid} fake")
        return _f

    fake_reg = {sid: _fake_stage(sid) for sid in phases.STAGE_ORDER}
    monkeypatch.setattr(phases, "STAGE_REGISTRY", fake_reg)

    phases.run_phase6("t", 5000, to_stage="S1",
                      video="all", run_dir=tmp_path)
    assert seen_video == ["all"], (
        f"video flag did not reach _setup_users; saw {seen_video}")


def test_attach_video_artifacts_writes_proof_path_relative_to_run_dir(
        tmp_path):
    """The video-artifact attach hook MUST update the targeted stage's
    proof to list .webm/.gif paths relative to run_dir, AND must also
    update state.json's mirror copy."""
    # Pre-seed state.json + proofs/S1.json (the "first measurement" stage)
    state = {
        "schema_version": "1.0.0",
        "tag": "t", "scale": 5000,
        "stages_completed": ["S0", "S1"],
        "stage_proofs": {
            "S0": {
                "stage_id": "S0", "started_at": "x", "ended_at": "y",
                "passed": True, "proof": {}, "artifacts": [],
                "what_breaks_if_skipped": "S0 underwrites preflight",
            },
            "S1": {
                "stage_id": "S1", "started_at": "x", "ended_at": "y",
                "passed": True, "proof": {}, "artifacts": [],
                "what_breaks_if_skipped": "S1 zero-state baseline",
            },
        },
    }
    phases.save_state(tmp_path, state)
    (tmp_path / "proofs").mkdir(exist_ok=True)
    (tmp_path / "proofs" / "S1.json").write_text(json.dumps(
        state["stage_proofs"]["S1"], indent=2))

    # Place fake .webm + .gif under videos/.
    (tmp_path / "videos").mkdir()
    webm = tmp_path / "videos" / "S1_admin_cold_dashboard.webm"
    gif = tmp_path / "videos" / "S1_admin_cold_dashboard.gif"
    webm.write_bytes(b"\x00")
    gif.write_bytes(b"\x00")

    ctx = phases.StageContext(
        tag="t", scale=5000,
        run_dir=tmp_path, state_path=tmp_path / "state.json",
        video="representative",
        user_pages={"__video_artifacts__": [str(webm), str(gif)]},  # type: ignore[arg-type]
    )
    phases._attach_video_artifacts_to_last_measurement_proof(ctx)

    proof = json.loads((tmp_path / "proofs" / "S1.json").read_text())
    assert "videos/S1_admin_cold_dashboard.webm" in proof["artifacts"]
    assert "videos/S1_admin_cold_dashboard.gif" in proof["artifacts"]
    # state.json mirror updated too
    state_after = json.loads((tmp_path / "state.json").read_text())
    assert "videos/S1_admin_cold_dashboard.webm" in \
        state_after["stage_proofs"]["S1"]["artifacts"]


def test_first_stage_label_prefers_first_non_meta_completed_stage(tmp_path):
    """`_first_stage_label` picks the lowest-indexed completed stage that
    is NOT S0/S9 (those don't produce nav samples)."""
    state = {
        "schema_version": "1.0.0",
        "tag": "t", "scale": 5000,
        "stages_completed": ["S0", "S1", "S2"],
        "stage_proofs": {
            sid: {
                "stage_id": sid, "started_at": "x", "ended_at": "y",
                "passed": True, "proof": {}, "artifacts": [],
                "what_breaks_if_skipped": f"{sid} fake",
            } for sid in ("S0", "S1", "S2")
        },
    }
    phases.save_state(tmp_path, state)
    ctx = phases.StageContext(tag="t", scale=5000, run_dir=tmp_path,
                              state_path=tmp_path / "state.json")
    assert phases._first_stage_label(ctx) == "S1"


# ─── Task #250 Block 2 — _user_group lookup table ──────────────────────────


def test_user_group_returns_devs_for_cyberjoker():
    """The 2-entry lookup mirrors portal-chart provisioning. cyberjoker
    is in `devs`; admin is provisioned via CRB (Group lookup unused).
    """
    from bench.phases import _user_group
    assert _user_group("cyberjoker") == "devs"
    assert _user_group("admin") == ""
    # Unknown user → empty string (caller treats as 'no Group').
    assert _user_group("mystery") == ""


# ─── Task #250 Block 2 — _count_widget_errors aggregation ──────────────────


def test_count_widget_errors_sums_validation_errored_count():
    """`_count_widget_errors` walks (user, page) results and sums
    `validation.errored_count` across all navigations. Catches the
    #186-class 'card visible but per-card widget 403' failure mode
    that R4 mitigation requires `cj_widget_error_count == 0`.
    """
    from bench.phases import _count_widget_errors

    # Synthetic results matching the browser_measure_stage output shape.
    results = [
        {
            "user": "cyberjoker",
            "pages": {
                "Compositions": {
                    "navigations": [
                        {"validation": {"errored_count": 2}},
                        {"validation": {"errored_count": 1}},
                        {"validation": {"errored_count": 0}},
                    ],
                },
                "Dashboard": {
                    "navigations": [
                        {"validation": {"errored_count": 99}},
                    ],
                },
            },
        },
        {
            "user": "admin",
            "pages": {
                "Compositions": {
                    "navigations": [
                        {"validation": {"errored_count": 50}},
                    ],
                },
            },
        },
    ]
    # cj + Compositions → 2 + 1 + 0 = 3
    assert _count_widget_errors(results, user="cyberjoker",
                                page="Compositions") == 3
    # cj + Dashboard → 99 (does not bleed across pages)
    assert _count_widget_errors(results, user="cyberjoker",
                                page="Dashboard") == 99
    # admin + Compositions → 50 (does not bleed across users)
    assert _count_widget_errors(results, user="admin",
                                page="Compositions") == 50
    # Unknown user → 0
    assert _count_widget_errors(results, user="ghost",
                                page="Compositions") == 0


def test_count_widget_errors_empty_results_returns_zero():
    """An empty results list returns 0 — no FAIL surface from absence."""
    from bench.phases import _count_widget_errors
    assert _count_widget_errors([], user="cyberjoker",
                                page="Compositions") == 0
    assert _count_widget_errors(None, user="cyberjoker",
                                page="Compositions") == 0


def test_count_widget_errors_tolerates_missing_validation():
    """Nav entries without a `validation` key contribute 0 (no crash)."""
    from bench.phases import _count_widget_errors
    results = [
        {
            "user": "cyberjoker",
            "pages": {
                "Compositions": {
                    "navigations": [
                        {},  # missing validation
                        {"validation": {}},  # missing errored_count
                        {"validation": {"errored_count": "garbage"}},
                        {"validation": {"errored_count": 3}},
                    ],
                },
            },
        },
    ]
    # Only the valid integer 3 contributes.
    assert _count_widget_errors(results, user="cyberjoker",
                                page="Compositions") == 3


# ─── Task #250 Block 2 — _wait_rbac_propagation_to_snowplow ────────────────
#
# Hermetic-isolation contract: ALL probe paths must monkeypatch the
# browser-side reads so the test fails fast against a real GKE context.
# We patch `browser.read_snowplow_expvar_int` AND
# `browser.count_user_compositions_in_ns`, plus inject the subject's
# token through the ctx.


def test_wait_rbac_propagation_succeeds_when_both_probes_agree(
        tmp_path, monkeypatch):
    from bench import phases as phases_mod
    from bench import browser as browser_mod

    # First expvar call (the seq_before snapshot) returns 100; ALL
    # subsequent calls return 101 (incremented — Probe A passes).
    # Visible-count returns 999 forever (Probe B passes immediately
    # after the first poll inside the loop).
    expvar_call_count = [0]

    def _fake_expvar(key, **kw):
        expvar_call_count[0] += 1
        return 100 if expvar_call_count[0] == 1 else 101

    def _fake_count(user, token, ns):
        return 999

    monkeypatch.setattr(browser_mod, "read_snowplow_expvar_int",
                        _fake_expvar)
    monkeypatch.setattr(browser_mod, "count_user_compositions_in_ns",
                        _fake_count)

    ctx = phases_mod.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        tokens={"cyberjoker": "fake-token"},
    )
    ok, ms, diag = phases_mod._wait_rbac_propagation_to_snowplow(
        ctx, "cyberjoker", "bench-ns-01",
        expected_visible=999, timeout_secs=5)
    assert ok is True
    assert ms >= 0
    assert diag["rbac_publish_seq_before"] == 100
    assert diag["rbac_publish_seq_after"] == 101
    assert diag["user_visible_count"] == 999
    assert diag["probe_a_pass"] is True
    assert diag["probe_b_pass"] is True


def test_wait_rbac_propagation_fails_closed_when_expvar_unreadable(
        tmp_path, monkeypatch):
    """Probe A reads return None (expvar key absent or transport
    failure). MUST fail closed — never assume propagation on a missing
    read.
    """
    from bench import phases as phases_mod
    from bench import browser as browser_mod

    monkeypatch.setattr(browser_mod, "read_snowplow_expvar_int",
                        lambda key, **kw: None)
    monkeypatch.setattr(browser_mod, "count_user_compositions_in_ns",
                        lambda u, t, n: 999)

    ctx = phases_mod.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        tokens={"cyberjoker": "fake-token"},
    )
    ok, ms, diag = phases_mod._wait_rbac_propagation_to_snowplow(
        ctx, "cyberjoker", "bench-ns-01",
        expected_visible=999, timeout_secs=2)
    assert ok is False
    assert diag.get("error") == "expvar_unreadable"


def test_wait_rbac_propagation_fails_when_probe_b_disagrees(
        tmp_path, monkeypatch):
    """Probe A passes (seq increments) but Probe B never matches —
    classic #149 evaluator-side regression. propagation_ok = False with
    probe_a_pass=True, probe_b_pass=False in diag.
    """
    from bench import phases as phases_mod
    from bench import browser as browser_mod

    # seq_before=100; subsequent calls return 101 (incremented), so
    # Probe A passes. count_user_compositions_in_ns returns 0 forever
    # though expected_visible=999, so Probe B never matches.
    seq_calls = [0]

    def _fake_expvar(key, **kw):
        seq_calls[0] += 1
        return 100 if seq_calls[0] == 1 else 101

    monkeypatch.setattr(browser_mod, "read_snowplow_expvar_int",
                        _fake_expvar)
    monkeypatch.setattr(browser_mod, "count_user_compositions_in_ns",
                        lambda u, t, n: 0)

    ctx = phases_mod.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        tokens={"cyberjoker": "fake-token"},
    )
    ok, ms, diag = phases_mod._wait_rbac_propagation_to_snowplow(
        ctx, "cyberjoker", "bench-ns-01",
        expected_visible=999, timeout_secs=2)
    assert ok is False
    assert diag["probe_a_pass"] is True   # seq incremented
    assert diag["probe_b_pass"] is False  # view never converged
    assert diag["user_visible_count"] == 0
    assert diag["expected_visible"] == 999


def test_wait_rbac_propagation_fails_closed_when_no_token(tmp_path):
    """Missing token in ctx.tokens → fail closed (no_token diag)."""
    from bench import phases as phases_mod
    ctx = phases_mod.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        tokens={},  # no cj token
    )
    ok, ms, diag = phases_mod._wait_rbac_propagation_to_snowplow(
        ctx, "cyberjoker", "bench-ns-01",
        expected_visible=999, timeout_secs=2)
    assert ok is False
    assert diag.get("error") == "no_token"


# ─── _pick_visible_composition_name fallback path ──────────────────────────


def test_pick_visible_composition_name_returns_conventional_on_kubectl_fail(
        monkeypatch):
    """When kubectl returns non-zero, fall back to the conventional
    `bench-app-01-02` name.
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **kw: (1, "", "err"))
    assert phases_mod._pick_visible_composition_name("bench-ns-01") == \
        "bench-app-01-02"


def test_pick_visible_composition_name_uses_lex_sort(monkeypatch):
    """When kubectl returns names, sort lex and return `bench-app-01-02`
    if present (typically true after S7 deletes -01-01).
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0,
                          "bench-app-01-05\nbench-app-01-03\n"
                          "bench-app-01-02\nbench-app-01-04",
                          ""))
    out = phases_mod._pick_visible_composition_name("bench-ns-01")
    assert out == "bench-app-01-02"


def test_pick_visible_composition_name_fallback_when_conventional_absent(
        monkeypatch):
    """If `bench-app-01-02` is NOT in the live list (e.g. S7 already
    deleted that one too), pick the lex-first available name.
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0, "bench-app-01-07\nbench-app-01-05", ""))
    out = phases_mod._pick_visible_composition_name("bench-ns-01")
    assert out == "bench-app-01-05"
