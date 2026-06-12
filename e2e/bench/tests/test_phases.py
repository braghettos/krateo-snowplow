"""Tests for bench.phases — stage runner, state.json, --from-stage semantics.

Covers §C.7 (~8 cases) PLUS the critical R3.2 mitigation test that proves
ConvergenceTimeout-from-stage persists state.json BEFORE the re-raise
(see plan §G Block 4 G3).

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.7 + §G Block 4.
"""

from __future__ import annotations

import json
import time
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
    # Task #314: additive lettered sub-stages S8b (widget-mod reconcile) +
    # S8c (RA-mod reconcile) inserted after S9, before S10 — no renumber of
    # S10/S11 (additive ids, no SCHEMA_MAJOR bump).
    assert set(STAGE_REGISTRY.keys()) == {
        "S0", "S1", "S2", "S3", "S4", "S5", "S6", "S7",
        "S8", "S9", "S8b", "S8c", "S10", "S11",
    }
    assert STAGE_ORDER == ["S0", "S1", "S2", "S3", "S4",
                           "S5", "S6", "S7", "S8", "S9",
                           "S8b", "S8c", "S10", "S11"]


# ─── --from-stage / --to-stage semantics ────────────────────────────────────


def test_from_stage_S5_skips_S1_S2_S3_S4():
    window = _stages_in_window("S5", "S8")
    assert window == ["S5", "S6", "S7", "S8"]
    assert "S1" not in window and "S4" not in window


def test_to_stage_S11_includes_S0_through_S11_inclusive():
    # SCHEMA 1.1.0 — the report stage is now S11. Operators selecting
    # the full window via --to-stage S11 must see all stages, now including
    # the Task #314 S8b/S8c reconcile sub-stages (after S9, before S10).
    window = _stages_in_window(None, "S11")
    assert window == ["S0", "S1", "S2", "S3", "S4",
                      "S5", "S6", "S7", "S8", "S9",
                      "S8b", "S8c", "S10", "S11"]


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


# ─── S8 Role rules freeze (Task #262 / #272) ────────────────────────────────


def test_s8_role_rules_grants_restactions_get_list():
    """Task #262 / 2026-06-09 v5 re-gate regression guard.

    The S8 cj_widget_error_count = 15 defect (architect doc
    docs/task-262-s8-cj-tablist-trace-2026-06-09.md) was caused by a
    missing `templates.krateo.io: restactions` rule in the bench S8
    Role: cj's per-card Panel `spec.apiRef` resolution fell through to
    the apiserver which returned 403, and the apiref boundary
    surfaced that as `.ant-result-error` on the SPA (5 cards × 3
    navs = 15 errors).

    This test pins the v5 rule in place. If anyone strips it again
    the gate fails BEFORE the bench runs Phase 6, surfacing the
    regression at near-zero cost.

    Rule shape matches what cluster.k8s_create_namespaced_role
    accepts: (api_groups, resources, verbs).
    """
    assert hasattr(phases, "S8_ROLE_RULES"), (
        "phases.S8_ROLE_RULES must be a module-level constant so the "
        "bench Role rules are testable without spinning up Phase 6"
    )
    rules = phases.S8_ROLE_RULES

    # The restactions rule MUST be present — pre-v5 it was missing.
    target = (["templates.krateo.io"], ["restactions"], ["get", "list"])
    assert target in rules, (
        f"S8_ROLE_RULES missing the templates.krateo.io:restactions grant. "
        f"This is the Task #262 v5 re-gate fix; removing it re-introduces "
        f"the cj_widget_error_count=15 defect at S8.\n"
        f"Current rules: {rules}"
    )


def test_s8_role_rules_keeps_v4_grants_intact():
    """v5 is additive over v4 — composition + the four widget kinds
    (incl. tablists) MUST still be present so click-nav + datagrid
    fan-out work as designed."""
    rules = phases.S8_ROLE_RULES

    # v3 grant (composition CRs cj's datagrid iterates).
    assert (["composition.krateo.io"], ["*"], ["get", "list"]) in rules

    # v4 grant (the four widget kinds — panels, markdowns, buttons,
    # tablists). Same shape the prior re-gates use.
    widget_rule = (
        ["widgets.templates.krateo.io"],
        ["panels", "markdowns", "buttons", "tablists"],
        ["get", "list"],
    )
    assert widget_rule in rules


def test_s8_role_rules_shape_is_cluster_helper_compatible():
    """Every S8_ROLE_RULES entry must be a (groups, resources, verbs)
    3-tuple of list[str] so cluster.k8s_create_namespaced_role can
    consume it verbatim. Guards against a future refactor accidentally
    introducing a dict / object shape that the helper would not
    recognise."""
    for entry in phases.S8_ROLE_RULES:
        assert isinstance(entry, tuple), \
            f"S8 role rule entry not a tuple: {entry!r}"
        assert len(entry) == 3, \
            f"S8 role rule entry must be (groups, resources, verbs): {entry!r}"
        groups, resources, verbs = entry
        assert isinstance(groups, list) and all(
            isinstance(g, str) for g in groups), \
            f"S8 role rule groups must be list[str]: {groups!r}"
        assert isinstance(resources, list) and all(
            isinstance(r, str) for r in resources), \
            f"S8 role rule resources must be list[str]: {resources!r}"
        assert isinstance(verbs, list) and all(
            isinstance(v, str) for v in verbs), \
            f"S8 role rule verbs must be list[str]: {verbs!r}"


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


def test_setup_users_records_credentials_only_when_video_flag_set(
        tmp_path, monkeypatch):
    """Task #267 PER-STAGE correction (Diego 2026-06-10): in a recording
    mode, _setup_users must NOT open a long-lived recording context (that
    would span the whole run → ONE giant .webm). It stores credentials only
    (record_video=True, pwd, ctx=None) + creates videos/; the per-stage
    recording contexts are opened later by _measure_all_users so each stage
    yields its own files.

    Falsifier: monkey-patch browser.make_browser_context to capture calls;
    assert ZERO calls at setup time, videos/ exists, and each user carries
    creds for per-stage re-login.
    """
    _install_fake_playwright(monkeypatch)
    captured: list[dict] = []

    def _capture(pw_browser, **kwargs):
        captured.append(dict(kwargs))
        return _FakeCtx()

    monkeypatch.setattr(phases.browser, "make_browser_context", _capture)
    monkeypatch.setattr(phases.browser, "_ensure_users",
                        lambda: {"admin": "pw-a", "cyberjoker": "pw-c"})
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

    assert captured == [], (
        f"recording _setup_users must NOT open contexts at setup (per-stage "
        f"now); got {len(captured)} make_browser_context calls")
    assert (tmp_path / "videos").is_dir(), "videos/ dir must be created"
    assert "__stage_videos__" in ctx.user_pages
    for u, pw_expected in (("admin", "pw-a"), ("cyberjoker", "pw-c")):
        u_state = ctx.user_pages[u]
        assert u_state["record_video"] is True
        assert u_state["pwd"] == pw_expected, (
            f"{u} must carry its password for per-stage re-login")
        assert u_state["ctx"] is None and u_state["page"] is None, (
            f"{u} must hold NO long-lived recording context at setup")


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


@pytest.mark.parametrize("video_mode", ["representative", "all"])
def test_setup_users_credentials_only_in_both_record_modes(
        tmp_path, monkeypatch, video_mode):
    """Per-stage recording applies to BOTH `representative` AND `all` (they
    differ only in intended frequency). In either mode _setup_users opens NO
    context (per-stage in _measure_all_users) and stores creds-only.
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
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json", video=video_mode,
        user_pages={"__subjects__": ["admin", "cyberjoker"]},
    )
    phases._setup_users(ctx)

    assert captured == [], (
        f"video={video_mode!r}: _setup_users must open NO context at setup "
        f"(per-stage recording); got {len(captured)}")
    assert (tmp_path / "videos").is_dir()
    for u in ("admin", "cyberjoker"):
        assert ctx.user_pages[u]["record_video"] is True
        assert ctx.user_pages[u]["pwd"]  # creds present for per-stage re-login
        assert ctx.user_pages[u]["ctx"] is None


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


def test_attach_video_artifacts_to_last_measurement_proof_is_noop_now(
        tmp_path):
    """Task #267 PER-STAGE correction: the old run-end re-attribution hook is
    now a DEPRECATED no-op (per-stage attachment in _run_stage supersedes it).
    It must NOT mutate any proof — doing so would double-attach every stage's
    video to the first stage.
    """
    state = {
        "schema_version": "1.0.0", "tag": "t", "scale": 5000,
        "stages_completed": ["S0", "S1"],
        "stage_proofs": {
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

    ctx = phases.StageContext(
        tag="t", scale=5000,
        run_dir=tmp_path, state_path=tmp_path / "state.json",
        video="representative",
        user_pages={"__video_artifacts__": ["videos/whatever.webm"]},  # type: ignore[arg-type]
    )
    # No-op: must not raise and must not touch the proof.
    phases._attach_video_artifacts_to_last_measurement_proof(ctx)

    proof = json.loads((tmp_path / "proofs" / "S1.json").read_text())
    assert proof["artifacts"] == [], (
        f"deprecated hook must be a no-op; proof.artifacts mutated to "
        f"{proof['artifacts']!r}")


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


# ─── Task #267 FIX 2 — film BOTH dashboard AND /compositions per cell ───────
#
# Playwright records one .webm per BrowserContext. _setup_users now opens one
# recording context per (user, page); _teardown_users renames each to
# {stage}_{user}_{page_slug}.webm and renders a .gif. These cases prove a
# /compositions video artifact is produced (not just dashboard), that the
# hardcoded `_cold_dashboard` suffix is gone, and that the #299 truncation
# behaviour (record_video_to_gif's max_mb/oversize-delete) is preserved.


class _FakeVideo:
    """Playwright page.video stand-in: .path() returns the raw .webm path."""
    def __init__(self, raw_path):
        self._raw = str(raw_path)

    def path(self):
        return self._raw


from tests.conftest import FakePage


class _FakePageWithVideo(FakePage):
    """conftest.FakePage + `.video.path()` + the content-gate surface
    (`present_names`-backed `locator`, reload-counting `goto`) so the SAME
    page that 'filmed' the nav is the one the S8/S9 content asserts inspect
    (architect Option A) — and so it can drive the REAL
    `browser.browser_measure_stage` (incl. the Dashboard VERIFY poll) in the
    F-B′ end-to-end discrimination test. All JS-probe dispatch (verify UI
    count, waterfall, nav-timing, scroll, skeleton) is inherited from
    FakePage — one dispatch table to keep in sync with browser.py, not two.
    """
    def __init__(self, raw_webm=None, present_names=None, ui_count=42,
                 call_count=16):
        super().__init__(ui_count=ui_count, call_count=call_count,
                         url="http://fake/compositions")
        # Non-recording pages (raw_webm None) expose video=None, as Playwright
        # does for a context created without record_video_dir.
        self.video = _FakeVideo(raw_webm) if raw_webm is not None else None
        self.present_names = set(present_names or [])
        self.reloaded = 0

    def locator(self, selector):
        name = (selector[len("text="):] if selector.startswith("text=")
                else selector)
        return _CardLocator(1 if name in self.present_names else 0)

    def goto(self, url, **kw):
        self.reloaded += 1
        super().goto(url, **kw)


class _FakeRecordingCtx:
    """BrowserContext stand-in. On close(), the raw .webm 'finalizes' (we
    write a small byte blob to disk so record_video_to_gif sees a source).
    `raw_webm=None` models a NON-recording context (e.g. the throwaway login
    context): close() writes nothing and the page exposes video=None.
    `present_names` is forwarded to the page so the content gate can inspect
    the live (deferred) Compositions page."""
    def __init__(self, raw_webm=None, present_names=None):
        self._raw = Path(raw_webm) if raw_webm is not None else None
        self.closed = False
        self.page = _FakePageWithVideo(raw_webm, present_names=present_names)

    def new_page(self):
        return self.page

    def storage_state(self, **kw):
        # Task #307 / A1-full: the throwaway login context captures auth here.
        return {"cookies": [], "origins": []}

    def close(self):
        self.closed = True
        if self._raw is not None:
            self._raw.parent.mkdir(parents=True, exist_ok=True)
            self._raw.write_bytes(b"FAKE_WEBM" * 8)


def _stub_video_to_gif(monkeypatch):
    """Stub record_video_to_gif WITHOUT ffmpeg. Per the Task #267 correction
    the gif is ALWAYS produced + kept (no size-based drop)."""
    def _fake(webm_path, gif_path, *, fps=4, max_seconds=60, max_mb=None):
        webm = Path(webm_path)
        gif = Path(gif_path)
        if not webm.exists():
            return False
        gif.write_bytes(b"GIF89a" + b"\x00" * 256)
        return True

    monkeypatch.setattr(phases.browser, "record_video_to_gif", _fake)


class _StageRecordingBrowser:
    """Fake pw_browser whose new_context() returns a fresh recording context
    each call, mimicking Playwright assigning a unique random .webm path per
    context under the passed record_video_dir. `present_names` (optional) is
    forwarded to every page so the content gate can find rendered cards on the
    subject's deferred Compositions page."""
    def __init__(self, present_names=None):
        self._n = 0
        self.contexts: list[_FakeRecordingCtx] = []
        self._present_names = present_names

    def new_context(self, **kwargs):
        self._n += 1
        rvd_arg = kwargs.get("record_video_dir")
        if rvd_arg is None:
            # Task #307 / A1-full: the one-time throwaway login context is
            # NON-recording (no record_video_dir → raw=None, writes nothing).
            # It is not appended to `contexts` (it never produces a stage
            # .webm) so the per-stage video-count assertions still see exactly
            # the recording contexts.
            return _FakeRecordingCtx(present_names=self._present_names)
        raw = Path(rvd_arg) / f"raw-{self._n}.webm"
        c = _FakeRecordingCtx(raw, present_names=self._present_names)
        self.contexts.append(c)
        return c


def _hermetic_make_ctx(pw_browser, *, record_video_dir=None, **kw):
    """Hermetic make_browser_context stand-in: forwards record_video_dir to
    the fake pw_browser and swallows storage_state + any future kwargs — so a
    make_browser_context signature change is a zero-touch edit here."""
    return pw_browser.new_context(record_video_dir=record_video_dir)


def _materialise(page_factories):
    """Invoke every lazy slot factory — mirrors the real browser_measure_stage
    materialising each page at its own loop iteration (Task #307)."""
    for _pn, make in (page_factories or {}).items():
        make()


def _install_stage_recording_fakes(monkeypatch, *, measure_raises=None):
    """Wire browser.make_browser_context / browser_login / browser_measure_stage
    so _measure_all_users' per-stage recording path runs hermetically.

    Returns the list capturing (stage_num, user, pages_by_name keys) per
    browser_measure_stage call. `measure_raises` (an exception or None) lets a
    test force a ConvergenceTimeout-shaped failure to prove the partial video
    is still finalized.
    """
    monkeypatch.setattr(phases.browser, "make_browser_context",
                        _hermetic_make_ctx)
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)
    _stub_video_to_gif(monkeypatch)

    calls: list[dict] = []

    def _fake_measure(page, stage_num, stage_desc, cache_mode, *,
                      token=None, user="admin", verify_against_cluster=True,
                      deleted_ns=None, pages_by_name=None,
                      page_factories=None):
        # Task #307 / A1-full: mirror real browser_measure_stage — materialise
        # any lazy page by invoking its factory, so the deferred-finalize +
        # content-gate path sees the live page (the real function does this
        # at the page's loop iteration, after VERIFY).
        materialised_keys = sorted(pages_by_name) if pages_by_name else None
        if page_factories:
            _materialise(page_factories)
            if pages_by_name is not None:
                materialised_keys = sorted(
                    set(pages_by_name) | set(page_factories))
        calls.append({"stage_num": stage_num, "user": user,
                      "pages_by_name_keys": materialised_keys})
        if measure_raises is not None:
            raise measure_raises
        return {"stage": stage_num, "pages": {}}

    monkeypatch.setattr(phases.browser, "browser_measure_stage", _fake_measure)
    return calls


def _stub_measure_materializing(stage_num):
    """A `browser_measure_stage` stub that — like the real function under
    Task #307 / A1-full — materialises any LAZY page by invoking its factory
    at measure time. Tests whose content gate inspects the subject's deferred
    Compositions page MUST use this (a bare no-op stub leaves the lazy slot's
    page None → the gate would falsely see an empty/None page)."""
    def _measure(page, *a, page_factories=None, **k):
        _materialise(page_factories)
        return {"stage": stage_num, "pages": {}}
    return _measure


def _recording_ctx(tmp_path, users=("admin",)):
    """Build a recording-mode StageContext with creds-only user states and a
    _StageRecordingBrowser, mirroring post-_setup_users recording shape."""
    pw_browser = _StageRecordingBrowser()
    (tmp_path / "videos").mkdir(parents=True, exist_ok=True)
    up: dict = {"__browser__": pw_browser, "__pw__": None,
                "__stage_videos__": {}}
    for u in users:
        up[u] = {"ctx": None, "page": None, "pwd": f"pw-{u}",
                 "token": f"tok-{u}", "record_video": True}
    ctx = phases.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json", video="representative",
        user_pages=up)
    return ctx, pw_browser


def test_video_page_slug_maps_names():
    """`_video_page_slug` lowercases + slugifies the page name."""
    assert phases._video_page_slug("Dashboard") == "dashboard"
    assert phases._video_page_slug("Compositions") == "compositions"
    assert phases._video_page_slug("My Page") == "my-page"
    assert phases._video_page_slug("") == "page"


def test_open_stage_recording_pages_all_slots_lazy_no_eager_recording_ctx(
        tmp_path, monkeypatch):
    """Falsifier F-open (Task #307 / ArchLazyDash): EVERY slot returned by
    `_open_stage_recording_pages` is LAZY — page is None AND make is callable —
    and NO recording context (record_video_dir set) is created at open time.

    Pre-fix the Dashboard slot was EAGER: `_open_stage_recording_pages` created
    a recording BrowserContext (record_video_dir=videos_dir) + new_page for it
    right here, so its `.webm` clock started during the in-frame
    count_compositions() header probe + cold-nav poll idle. Post-fix the ONLY
    context created at open time is the throwaway NON-recording login context
    (record_video_dir=None) used for storage_state capture; every recording
    context is deferred to its page's loop iteration in browser_measure_stage.

    Discriminator: we record every make_browser_context call's record_video_dir.
    With the fix, the recording-dir count is 0 at open time and BOTH slots
    (Dashboard + Compositions) are lazy. Pre-fix the Dashboard slot is eager so
    a recording context is created here and its slot's page is non-None.
    """
    rvd_calls: list = []  # record_video_dir per make_browser_context call

    def _fake_make_ctx(pw, *, record_video_dir=None, storage_state=None, **kw):
        rvd_calls.append(record_video_dir)
        # Throwaway login context (rvd=None) supports storage_state capture.
        return _FakeRecordingCtx()

    monkeypatch.setattr(phases.browser, "make_browser_context", _fake_make_ctx)
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)

    pages = phases._open_stage_recording_pages(
        _StageRecordingBrowser(), tmp_path / "videos", "cyberjoker", "pw")

    # Exactly one make_browser_context call at open time — the throwaway login
    # context — and it is NON-recording (record_video_dir is None). NO recording
    # context (record_video_dir set) is created eagerly for ANY page.
    assert rvd_calls == [None], (
        f"open time must create exactly one NON-recording (storage_state) "
        f"context; recording-dir calls leaked: {rvd_calls!r}")
    recording_dir_calls = [r for r in rvd_calls if r is not None]
    assert recording_dir_calls == [], (
        f"eager record_video_dir context(s) created at open time: "
        f"{recording_dir_calls!r}")

    # BOTH slots present and LAZY: page is None + a callable make factory.
    assert set(pages) == {"Dashboard", "Compositions"}, sorted(pages)
    for page_name, slot in pages.items():
        assert slot.get("page") is None, (
            f"{page_name} slot must be lazy (page=None at open); got "
            f"{slot.get('page')!r}")
        assert slot.get("ctx") is None, (
            f"{page_name} slot must be lazy (ctx=None at open)")
        assert callable(slot.get("make")), (
            f"{page_name} slot must carry a make factory")


def test_open_stage_lazy_factory_creates_recording_context_when_invoked(
        tmp_path, monkeypatch):
    """The deferred factory (for EVERY page now) creates a RECORDING context
    (record_video_dir set) + materialises ctx/page into its slot when invoked —
    and is idempotent (a second call returns the same page without a 2nd ctx).

    This locks the lazy materialise contract that browser_measure_stage relies
    on (it invokes each slot's `make` at the page's loop iteration). It replaces
    the old eager-Dashboard orphan-leak guard, since there is no longer an eager
    new_page() at open time; the factory failure path is the B1 fallback in
    browser_measure_stage (covered by test_lazy_factory_failure_falls_back_*).
    """
    rvd_calls: list = []

    def _fake_make_ctx(pw, *, record_video_dir=None, storage_state=None, **kw):
        rvd_calls.append(record_video_dir)
        raw = (Path(record_video_dir) / f"raw-{len(rvd_calls)}.webm"
               if record_video_dir is not None else None)
        return _FakeRecordingCtx(raw)

    monkeypatch.setattr(phases.browser, "make_browser_context", _fake_make_ctx)
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)

    pages = phases._open_stage_recording_pages(
        _StageRecordingBrowser(), tmp_path / "videos", "cyberjoker", "pw")

    # Only the throwaway login context so far.
    assert rvd_calls == [None]

    dash = pages["Dashboard"]
    pg = dash["make"]()
    # The factory created a RECORDING context (record_video_dir set) and wired
    # the slot.
    assert rvd_calls[-1] == tmp_path / "videos", rvd_calls
    assert dash["page"] is pg and dash["ctx"] is not None
    # Idempotent: a second call returns the same page, no new context.
    before = len(rvd_calls)
    assert dash["make"]() is pg
    assert len(rvd_calls) == before, "factory created a second context"


def test_open_stage_login_fires_at_most_once_per_user_across_run(
        tmp_path, monkeypatch):
    """Task #310a falsifier: across a SIMULATED multi-stage recording run the
    throwaway login factory (`browser.browser_login`) fires ≤1× per user when a
    per-RUN memo is threaded — NOT once per stage.

    Pre-fix `_open_stage_recording_pages` did a fresh throwaway login every time
    it was called (once per STAGE per user) → ~stages×users redundant logins per
    run. With the per-RUN `storage_memo` it logs in once per user and every
    later stage reuses the cached storage_state with NO login.

    Discriminator: count browser_login(user) calls keyed by user across N stage
    opens that share ONE memo dict (exactly what _measure_all_users threads from
    ctx.user_pages["__storage_state_memo__"]). Assert each user's count is ≤1
    (and == 1, since each user does appear). Pre-fix this count == N_STAGES."""
    login_calls: list[str] = []

    def _counting_login(page, user, pwd):
        login_calls.append(user)
        return True

    monkeypatch.setattr(phases.browser, "browser_login", _counting_login)
    monkeypatch.setattr(phases.browser, "make_browser_context",
                        lambda pw, **kw: _FakeRecordingCtx())

    pw_browser = _StageRecordingBrowser()
    storage_memo: dict = {}            # the per-RUN memo (shared across stages)
    users = ("admin", "cyberjoker")
    n_stages = 6                       # S0,S1,S6,S8,... — a realistic run window

    for _stage in range(n_stages):
        for user in users:
            pages = phases._open_stage_recording_pages(
                pw_browser, tmp_path / "videos", user, f"pw-{user}",
                storage_memo=storage_memo)
            # Each open still yields the full lazy page set (auth came from the
            # memo on stages > 0) — proving reuse did not break the contract.
            assert set(pages) == {"Dashboard", "Compositions"}, sorted(pages)

    from collections import Counter
    per_user = Counter(login_calls)
    for user in users:
        assert per_user[user] <= 1, (
            f"{user} logged in {per_user[user]}× across {n_stages} stages — "
            f"memo did not collapse per-stage logins; calls={login_calls!r}")
        assert per_user[user] == 1, (
            f"{user} never logged in (expected exactly one per-run capture); "
            f"calls={login_calls!r}")
    # Total logins == #users, NOT #users × #stages.
    assert len(login_calls) == len(users), (
        f"expected exactly {len(users)} logins for the whole run, got "
        f"{len(login_calls)}: {login_calls!r}")
    # The memo holds one entry per user with a real storage_state.
    assert set(storage_memo) == set(users)
    for user in users:
        assert storage_memo[user]["storage_state"] is not None


def test_storage_memo_refreshes_after_age_guard_expiry(tmp_path, monkeypatch):
    """Task #310a fallback-on-expiry: a memo entry older than
    `_STORAGE_STATE_MAX_AGE_S` is treated as a miss and re-captured with a fresh
    login, rather than serving a (potentially expired-JWT) stale storage_state
    that would let recording pages silently 401.

    The age guard is defense-in-depth (AUTHN TTL is 24h, a run ~2.5h), but it
    MUST force a fresh capture once an entry ages past the threshold. We seed a
    memo entry whose `captured_at` is older than the guard, then assert the next
    open performs a fresh login and overwrites the entry's timestamp."""
    login_calls: list[str] = []

    def _counting_login(page, user, pwd):
        login_calls.append(user)
        return True

    monkeypatch.setattr(phases.browser, "browser_login", _counting_login)
    monkeypatch.setattr(phases.browser, "make_browser_context",
                        lambda pw, **kw: _FakeRecordingCtx())

    pw_browser = _StageRecordingBrowser()
    # Seed a STALE entry: captured_at far enough in the past to exceed the guard.
    stale_ss = {"cookies": [{"name": "stale"}], "origins": []}
    stale_at = time.monotonic() - (phases._STORAGE_STATE_MAX_AGE_S + 60)
    storage_memo = {"admin": {"storage_state": stale_ss,
                              "captured_at": stale_at}}

    pages = phases._open_stage_recording_pages(
        pw_browser, tmp_path / "videos", "admin", "pw-admin",
        storage_memo=storage_memo)

    assert set(pages) == {"Dashboard", "Compositions"}, sorted(pages)
    # Aged-out → a FRESH login fired (the stale storage_state was NOT reused).
    assert login_calls == ["admin"], (
        f"aged-out memo entry must trigger exactly one fresh login; "
        f"calls={login_calls!r}")
    # The memo entry was refreshed: new (non-stale) storage_state + newer ts.
    refreshed = storage_memo["admin"]
    assert refreshed["storage_state"] is not stale_ss, (
        "stale storage_state was served instead of re-captured")
    assert refreshed["captured_at"] > stale_at, (
        "memo timestamp was not refreshed after re-capture")

    # A SECOND open within the window now reuses the fresh entry — no 2nd login.
    pages2 = phases._open_stage_recording_pages(
        pw_browser, tmp_path / "videos", "admin", "pw-admin",
        storage_memo=storage_memo)
    assert set(pages2) == {"Dashboard", "Compositions"}, sorted(pages2)
    assert login_calls == ["admin"], (
        f"fresh memo entry must be reused without a 2nd login; "
        f"calls={login_calls!r}")


def test_failed_login_is_not_memoised_and_next_stage_retries(
        tmp_path, monkeypatch):
    """Task #310a constraint (c): a FAILED capture must NOT be cached — the next
    stage retries with a fresh login rather than serving a cached failure.

    Pre-fix (and a naive memo that cached failures) a transient AUTHN blip on
    one stage would suppress recording for the rest of the run. We make the
    first login fail and the second succeed, then assert: (1) the failed capture
    left the memo EMPTY (no entry written), (2) the second stage re-drove the
    login (a 2nd browser_login call), (3) only the successful capture is
    memoised, and (4) a third stage then reuses it with no further login."""
    outcomes = iter([False, True])   # stage 0 login fails, stage 1 succeeds

    login_calls: list[str] = []

    def _flaky_login(page, user, pwd):
        login_calls.append(user)
        return next(outcomes)

    monkeypatch.setattr(phases.browser, "browser_login", _flaky_login)
    monkeypatch.setattr(phases.browser, "make_browser_context",
                        lambda pw, **kw: _FakeRecordingCtx())

    pw_browser = _StageRecordingBrowser()
    storage_memo: dict = {}

    # Stage 0: login fails → no recording pages, and NOTHING memoised.
    pages0 = phases._open_stage_recording_pages(
        pw_browser, tmp_path / "videos", "admin", "pw-admin",
        storage_memo=storage_memo)
    assert pages0 == {}, (
        f"a failed login must yield no recording pages; got {sorted(pages0)}")
    assert "admin" not in storage_memo, (
        f"a FAILED capture must not be cached; memo={storage_memo!r}")
    assert login_calls == ["admin"]

    # Stage 1: must RETRY a fresh login (not serve a cached failure) → succeeds.
    pages1 = phases._open_stage_recording_pages(
        pw_browser, tmp_path / "videos", "admin", "pw-admin",
        storage_memo=storage_memo)
    assert set(pages1) == {"Dashboard", "Compositions"}, sorted(pages1)
    assert login_calls == ["admin", "admin"], (
        f"stage after a failed login must re-drive the login; "
        f"calls={login_calls!r}")
    assert storage_memo["admin"]["storage_state"] is not None

    # Stage 2: now the successful capture is reused — no further login.
    pages2 = phases._open_stage_recording_pages(
        pw_browser, tmp_path / "videos", "admin", "pw-admin",
        storage_memo=storage_memo)
    assert set(pages2) == {"Dashboard", "Compositions"}, sorted(pages2)
    assert login_calls == ["admin", "admin"], (
        f"successful capture must be reused without a 3rd login; "
        f"calls={login_calls!r}")


def test_measure_all_users_produces_both_pages_named_by_stage(
        tmp_path, monkeypatch):
    """Per-STAGE FIX 2 acceptance: one _measure_all_users(stage=6) run films
    BOTH pages on their OWN per-page recording contexts and finalizes
    `S6_admin_dashboard.webm` + `S6_admin_compositions.webm` (named by the
    LIVE stage number, not _first_stage_label). The /compositions video is
    produced, the old `_cold_dashboard` suffix is gone, and gifs are kept.
    """
    _install_stage_recording_fakes(monkeypatch)
    ctx, pw_browser = _recording_ctx(tmp_path, users=("admin",))

    phases._measure_all_users(ctx, 6, "S6 desc")

    produced = ctx.user_pages["__stage_videos__"]["S6"]
    names = {Path(p).name for p in produced}
    assert "S6_admin_dashboard.webm" in names, names
    assert "S6_admin_compositions.webm" in names, (
        f"/compositions video NOT produced per-stage; got {names}")
    assert "S6_admin_dashboard.gif" in names
    assert "S6_admin_compositions.gif" in names
    assert not any("cold_dashboard" in n for n in names)
    # Every per-page recording context was closed (Playwright finalizes .webm).
    assert all(c.closed for c in pw_browser.contexts)
    assert len(pw_browser.contexts) == len(phases.browser.BROWSER_SCALING_PAGES)


def test_two_stages_produce_distinctly_named_videos(tmp_path, monkeypatch):
    """LOAD-BEARING (Diego): separate watchable video per stage. Two
    _measure_all_users runs at DIFFERENT stage numbers (S1, then S6) must
    yield DISTINCTLY-named files — S1_admin_dashboard.webm AND
    S6_admin_dashboard.webm both exist, not one giant whole-run file.

    Falsifier: with whole-run recording (the pre-correction design) only one
    stage-labeled file would ever appear.
    """
    _install_stage_recording_fakes(monkeypatch)
    ctx, pw_browser = _recording_ctx(tmp_path, users=("admin",))

    phases._measure_all_users(ctx, 1, "S1 zero state")
    phases._measure_all_users(ctx, 6, "S6 scale")

    sv = ctx.user_pages["__stage_videos__"]
    assert set(sv.keys()) == {"S1", "S6"}, (
        f"expected per-stage keys S1 + S6; got {sorted(sv)}")
    s1_names = {Path(p).name for p in sv["S1"]}
    s6_names = {Path(p).name for p in sv["S6"]}
    assert "S1_admin_dashboard.webm" in s1_names
    assert "S1_admin_compositions.webm" in s1_names
    assert "S6_admin_dashboard.webm" in s6_names
    assert "S6_admin_compositions.webm" in s6_names
    # Distinct stages → disjoint, distinctly-named webm files on disk.
    videos_dir = tmp_path / "videos"
    for n in ("S1_admin_dashboard.webm", "S6_admin_dashboard.webm",
              "S1_admin_compositions.webm", "S6_admin_compositions.webm"):
        assert (videos_dir / n).exists(), f"{n} not on disk"


def test_measure_all_users_per_stage_films_every_user_and_page(
        tmp_path, monkeypatch):
    """Production shape (2 users × 2 pages): one stage yields 4 distinctly
    named videos — admin + cyberjoker each get their own dashboard +
    compositions .webm, all prefixed by the stage number.

    Architect Option A: the SUBJECT (cyberjoker) Compositions video is
    DEFERRED past the content asserts, so the full 4-file set is complete only
    AFTER `_drain_pending_video_finalize` runs (which `_run_stage` does after
    `_work`). We drain explicitly here to mimic that.
    """
    _install_stage_recording_fakes(monkeypatch)
    ctx, pw_browser = _recording_ctx(tmp_path, users=("admin", "cyberjoker"))

    phases._measure_all_users(ctx, 8, "S8 RB-add")

    # Before drain: subject (cyberjoker) Compositions is still OPEN/deferred
    # so its context is NOT yet closed and its video not yet stashed.
    pre = {Path(p).name for p in ctx.user_pages["__stage_videos__"]["S8"]
           if p.endswith(".webm")}
    assert "S8_cyberjoker_compositions.webm" not in pre, (
        f"subject Compositions must be DEFERRED, not finalized in-loop; {pre}")
    assert ctx.user_pages["cyberjoker"]["page"] is not None, (
        "subject Compositions live page must be exposed for the content gate")

    # Drain (what _run_stage does after _work) → the 4th video lands.
    phases._drain_pending_video_finalize(ctx)

    names = {Path(p).name for p in ctx.user_pages["__stage_videos__"]["S8"]
             if p.endswith(".webm")}
    assert names == {
        "S8_admin_dashboard.webm", "S8_admin_compositions.webm",
        "S8_cyberjoker_dashboard.webm", "S8_cyberjoker_compositions.webm",
    }, f"expected 4 per-(user,page) videos for S8 after drain; got {names}"
    # 2 users × 2 pages = 4 recording contexts opened + closed this stage.
    assert len(pw_browser.contexts) == 4
    assert all(c.closed for c in pw_browser.contexts)
    # After drain the deferred live page is cleared back to None.
    assert ctx.user_pages["cyberjoker"]["page"] is None


def test_measure_all_users_finalizes_video_even_on_convergence_timeout(
        tmp_path, monkeypatch):
    """The partial stage video must be finalized even when
    browser_measure_stage raises ConvergenceTimeout (it still shows real work
    up to the failure). The exception must still propagate so the stage fails.
    """
    ct = ConvergenceTimeout(stage=6, user="admin", api=0, ui=0,
                            cluster=1200, timeout_secs=1)
    _install_stage_recording_fakes(monkeypatch, measure_raises=ct)
    ctx, pw_browser = _recording_ctx(tmp_path, users=("admin",))

    with pytest.raises(ConvergenceTimeout):
        phases._measure_all_users(ctx, 6, "S6 desc")

    # Despite the raise, the stage's videos were finalized + stashed.
    produced = ctx.user_pages["__stage_videos__"].get("S6") or []
    names = {Path(p).name for p in produced}
    assert "S6_admin_dashboard.webm" in names, (
        f"partial video must be finalized on ConvergenceTimeout; got {names}")
    assert all(c.closed for c in pw_browser.contexts)


def test_subject_deferred_video_finalized_on_convergence_timeout(
        tmp_path, monkeypatch):
    """Architect's optional test — the only untested combination: SUBJECT
    user (deferred Compositions) + ConvergenceTimeout. On raise, the deferred
    Compositions context must STILL be CLOSED and its partial video land in
    __stage_videos__["S6"] once `_run_stage`'s drain runs — and the CT must
    still propagate. Mirrors _run_stage's `try: _work finally: drain` so the
    deferred-finalize-on-failure path is exercised end-to-end for the subject.
    """
    ct = ConvergenceTimeout(stage=6, user="cyberjoker", api=0, ui=0,
                            cluster=1200, timeout_secs=1)
    _install_stage_recording_fakes(monkeypatch, measure_raises=ct)
    ctx, pw_browser = _recording_ctx(tmp_path, users=("cyberjoker",))

    # Mirror _run_stage: drain pending finalize in a finally even when the
    # measurement raises, then confirm the CT still propagated.
    with pytest.raises(ConvergenceTimeout):
        try:
            phases._measure_all_users(ctx, 6, "S6 desc")
        finally:
            phases._drain_pending_video_finalize(ctx)

    # Both the subject's Dashboard (immediate) AND Compositions (deferred)
    # partial videos are finalized + stashed under S6.
    names = {Path(p).name
             for p in ctx.user_pages["__stage_videos__"].get("S6") or []}
    assert "S6_cyberjoker_dashboard.webm" in names, names
    assert "S6_cyberjoker_compositions.webm" in names, (
        f"deferred subject Compositions partial video must be finalized on "
        f"ConvergenceTimeout; got {names}")
    # Every recording context (incl. the deferred Compositions one) is closed.
    assert all(c.closed for c in pw_browser.contexts), (
        "deferred subject Compositions context must be CLOSED on raise")
    # Deferred live page cleared after drain.
    assert ctx.user_pages["cyberjoker"]["page"] is None


def test_measure_all_users_non_recording_uses_single_page(tmp_path, monkeypatch):
    """Non-recording run: NO per-stage contexts opened, pages_by_name=None,
    measurement runs on the single persistent page (zero behavioural change).
    """
    calls = []

    def _fake_measure(page, stage_num, stage_desc, cache_mode, *,
                      token=None, user="admin", verify_against_cluster=True,
                      deleted_ns=None, pages_by_name=None, page_factories=None):
        calls.append({"user": user, "pages_by_name": pages_by_name,
                      "page": page, "page_factories": page_factories})
        return {"stage": stage_num, "pages": {}}

    def _boom_ctx(*a, **k):
        raise AssertionError("non-recording must NOT open recording contexts")
    monkeypatch.setattr(phases.browser, "make_browser_context", _boom_ctx)
    monkeypatch.setattr(phases.browser, "browser_measure_stage", _fake_measure)

    persistent_page = object()
    ctx = phases.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json", video="none",
        user_pages={"__browser__": None,
                    "admin": {"ctx": object(), "page": persistent_page,
                              "token": "tok", "record_video": False}},
    )
    phases._measure_all_users(ctx, 6, "S6 desc")
    assert calls[0]["pages_by_name"] is None
    assert calls[0]["page"] is persistent_page
    assert "__stage_videos__" not in ctx.user_pages


def test_measure_all_users_all_lazy_selects_persistent_page_and_both_factories(
        tmp_path, monkeypatch):
    """Falsifier F-measure (Task #307 / ArchLazyDash): with EVERY recording
    slot now lazy (page=None), `_measure_all_users` selects
    `measure_page == u_state["page"]` (the persistent non-recording page) as the
    `page` arg to browser_measure_stage — NOT an eager slot page — and passes a
    `page_factories` mapping that now includes BOTH Dashboard AND Compositions,
    with `pages_by_name` mapping both to None.

    Pre-fix the Dashboard slot was EAGER (page non-None), so the `next(...)`
    selection picked that live Dashboard page as `measure_page` and
    page_factories carried ONLY Compositions. Post-fix the selection falls
    through to the persistent page (None for a recording user, which is correct:
    every page is materialised on its own lazy context inside
    browser_measure_stage, so the page arg is only the no-recording fallback).

    Discriminators that flip on revert: (a) "Dashboard" in page_factories;
    (b) Dashboard maps to None in pages_by_name; (c) the `page` arg is the
    persistent u_state["page"] fallback, not a live Dashboard page object.
    """
    captured: dict = {}

    def _fake_measure(page, stage_num, stage_desc, cache_mode, *,
                      token=None, user="admin", verify_against_cluster=True,
                      deleted_ns=None, pages_by_name=None, page_factories=None):
        captured["page"] = page
        captured["pages_by_name"] = pages_by_name
        captured["factory_keys"] = (sorted(page_factories)
                                    if page_factories else None)
        # Mirror the real function: materialise every lazy page at measure.
        _materialise(page_factories)
        return {"stage": stage_num, "pages": {}}

    # Real _open_stage_recording_pages (hermetic ctx factory), real lazy slots.
    monkeypatch.setattr(phases.browser, "make_browser_context",
                        _hermetic_make_ctx)
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)
    _stub_video_to_gif(monkeypatch)
    monkeypatch.setattr(phases.browser, "browser_measure_stage", _fake_measure)

    ctx, _pw = _recording_ctx(tmp_path, users=("admin",))
    # Production recording-user shape: persistent page is None (fresh per-stage
    # contexts are used instead). Capture it as the expected fallback.
    persistent = ctx.user_pages["admin"].get("page")
    assert persistent is None  # sanity: recording user holds no persistent page

    phases._measure_all_users(ctx, 6, "S6 desc")

    # (a) page_factories now carries BOTH pages (Dashboard is lazy too).
    assert captured["factory_keys"] == ["Compositions", "Dashboard"], (
        f"page_factories must include BOTH Dashboard + Compositions (all lazy); "
        f"got {captured['factory_keys']!r}")
    # (b) pages_by_name maps every page (incl. Dashboard) to None at selection.
    assert captured["pages_by_name"] == {"Dashboard": None,
                                         "Compositions": None}, (
        f"all slots lazy → pages_by_name all-None; got "
        f"{captured['pages_by_name']!r}")
    # (c) the page arg is the persistent non-recording fallback (u_state["page"],
    # None for a recording user), NOT a live eager Dashboard page object.
    assert captured["page"] is persistent, (
        f"measure_page must fall through to the persistent non-recording page "
        f"({persistent!r}); got a non-fallback page {captured['page']!r}")


def test_FB_prime_dashboard_lazy_end_to_end_verify_on_materialized_page(
        tmp_path, monkeypatch):
    """Falsifier F-B′ end-to-end (Task #307 / ArchLazyDash) — the TRUE
    prod-revert discriminator for the Dashboard-lazy behaviour, driven through
    the REAL `_open_stage_recording_pages` → `_measure_all_users` →
    `browser.browser_measure_stage` path (no measure stub).

    The Dashboard recording context is created LAZILY at its measure-time loop
    iteration (via the slot factory), NOT eagerly at open time, and the
    Dashboard VERIFY poll's `verify_composition_count_ui` runs on that
    lazily-materialised page.

    Discriminators that flip RED on prod revert (HEAD's eager Dashboard):
      (a) NO recording context (record_video_dir set) is created during
          `_open_stage_recording_pages`; the Dashboard recording context is
          created during `_measure_all_users` instead. Pre-fix the Dashboard
          recording context is created at open time.
      (b) the page `verify_composition_count_ui` inspected carries the
          factory-stamped `_lazy_materialised` marker — i.e. it came from the
          lazy slot factory. Pre-fix the Dashboard page is the eager open-time
          page (no marker) → RED.
    """
    # Phase tracking: count recording-context creations during open vs measure.
    phase = {"name": "open"}
    rec_ctx_creates = {"open": 0, "measure": 0}

    real_browser = phases.browser

    def _counting_make_ctx(pw, *, record_video_dir=None, storage_state=None,
                           **kw):
        if record_video_dir is not None:
            rec_ctx_creates[phase["name"]] += 1
        return pw.new_context(record_video_dir=record_video_dir)

    monkeypatch.setattr(real_browser, "make_browser_context", _counting_make_ctx)
    monkeypatch.setattr(real_browser, "browser_login", lambda page, u, p: True)
    _stub_video_to_gif(monkeypatch)

    # Module-level probes the REAL browser_measure_stage / VERIFY poll call.
    monkeypatch.setattr(real_browser, "FRONTEND", "http://fake")
    monkeypatch.setattr(real_browser, "_count_compositions", lambda: 42)
    monkeypatch.setattr(real_browser, "_count_bench_ns", lambda: 2)
    monkeypatch.setattr(real_browser, "verify_composition_count_api",
                        lambda token: 42)
    monkeypatch.setattr(real_browser, "_validate_widget_terminal_state",
                        lambda page, path, label, **kw: {"terminal_state": "pass"})

    ui_called_on: list = []

    def _verify_ui(page):
        ui_called_on.append(page)
        return 42
    monkeypatch.setattr(real_browser, "verify_composition_count_ui", _verify_ui)

    # Build the recording context, then run open + measure as the real pipeline
    # would, flipping the phase marker so we know WHEN each recording ctx is
    # created. `_measure_all_users` itself calls `_open_stage_recording_pages`,
    # so we wrap that to stamp the factory-materialised Dashboard page + record
    # the phase boundary.
    pw_browser = _StageRecordingBrowser(present_names={"comp-0"})
    (tmp_path / "videos").mkdir(parents=True, exist_ok=True)
    ctx = phases.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json", video="representative",
        user_pages={"__browser__": pw_browser, "__pw__": None,
                    "__stage_videos__": {},
                    "admin": {"ctx": None, "page": None, "pwd": "pw",
                              "token": "tok", "record_video": True}})

    real_open = phases._open_stage_recording_pages

    def _wrapped_open(*a, **k):
        phase["name"] = "open"
        pages = real_open(*a, **k)
        # Stamp every slot's factory so the page it materialises is marked.
        for _pn, slot in pages.items():
            inner = slot.get("make")
            if inner is None:
                continue

            def _stamping(_inner=inner):
                pg = _inner()
                try:
                    pg._lazy_materialised = True
                except Exception:
                    pass
                return pg
            slot["make"] = _stamping
        phase["name"] = "measure"
        return pages
    monkeypatch.setattr(phases, "_open_stage_recording_pages", _wrapped_open)

    phases._measure_all_users(ctx, 6, "S6 F-B' e2e")

    # (a) The Dashboard recording context was created at MEASURE time, not open.
    assert rec_ctx_creates["open"] == 0, (
        f"recording context(s) created EAGERLY at open time: "
        f"{rec_ctx_creates['open']} (Dashboard must be lazy)")
    assert rec_ctx_creates["measure"] >= 1, (
        "no recording context created at measure time — lazy factory never ran")
    # (b) The Dashboard VERIFY poll ran on a factory-materialised page.
    assert ui_called_on, "verify_composition_count_ui was never called"
    assert all(getattr(p, "_lazy_materialised", False) for p in ui_called_on), (
        "Dashboard VERIFY ran on a NON-lazily-materialised page (the eager "
        f"open-time page) — Dashboard was not lazy; pages={ui_called_on!r}")


def test_run_stage_attaches_this_stages_videos_to_its_own_proof(
        tmp_path, monkeypatch):
    """Per-stage attachment: _run_stage attaches the videos
    _measure_all_users stashed under __stage_videos__[stage_id] to THAT
    stage's proof (relative to run_dir), mirroring the per-stage log attach.
    """
    monkeypatch.setattr(phases, "_PerStageLogStreamer",
                        _NullStreamer)  # defined below
    videos_dir = tmp_path / "videos"
    videos_dir.mkdir(parents=True, exist_ok=True)
    webm = videos_dir / "S6_admin_dashboard.webm"
    gif = videos_dir / "S6_admin_dashboard.gif"
    webm.write_bytes(b"\x00")
    gif.write_bytes(b"\x00")

    ctx = phases.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json", video="representative",
        user_pages={"__stage_videos__": {"S6": [str(webm), str(gif)]}},
    )

    def _work(c):
        return {"__passed__": True, "ok": 1}

    proof = phases._run_stage("S6", ctx, _work,
                              what_breaks_if_skipped="S6 fake")
    assert "videos/S6_admin_dashboard.webm" in proof.artifacts, proof.artifacts
    assert "videos/S6_admin_dashboard.gif" in proof.artifacts
    # Persisted proof on disk carries them too.
    on_disk = json.loads((tmp_path / "proofs" / "S6.json").read_text())
    assert "videos/S6_admin_dashboard.webm" in on_disk["artifacts"]


class _NullStreamer:
    """No-op _PerStageLogStreamer stand-in for _run_stage unit tests."""
    def __init__(self, *a, **k):
        import threading as _t
        self._running = _t.Event()

    def start(self):
        self._running.set()

    def stop(self):
        self._running.clear()


# ─── Task #267 architect REWORK — recording-mode S8/S9 content gate ─────────
#
# THE missing gate (architect): the per-stage rebuild left recording users
# with u_state["page"]=None, so the S8/S9 CONTENT asserts (which run in `_work`
# AFTER _measure_all_users returns) inspected a None page → S8 false-FAILed
# (#149 broken) and S9 false-PASSed (blind to revocation). These tests build
# the recording-mode shape (page=None at setup, fake recording browser) and
# drive the REAL S8/S9 stage `_work` end-to-end through _run_stage, proving
# the content gate reads the LIVE filmed Compositions page (Option A).


def _stub_s8_s9_cluster(monkeypatch, *, comps_in_ns=5):
    """Stub the cluster/lifecycle/propagation calls S8/S9 `_work` makes so the
    stage reaches its CONTENT-assert section without a live apiserver."""
    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)
    monkeypatch.setattr(phases.browser, "FRONTEND", "http://fake")
    # Cluster mutation helpers all succeed.
    monkeypatch.setattr(phases.cluster, "count_compositions_in_ns",
                        lambda ns: comps_in_ns)
    monkeypatch.setattr(phases.cluster, "k8s_can_i_create_rolebinding",
                        lambda ns: (True, "ok"))
    monkeypatch.setattr(phases.cluster, "k8s_create_namespaced_role",
                        lambda *a, **k: (True, "ok"))
    monkeypatch.setattr(phases.cluster, "k8s_create_namespaced_role_binding",
                        lambda *a, **k: (True, "ok"))
    monkeypatch.setattr(phases.cluster, "k8s_delete_rolebinding",
                        lambda *a, **k: True)
    monkeypatch.setattr(phases.cluster, "k8s_delete_role",
                        lambda *a, **k: True)
    # Propagation gate passes immediately.
    monkeypatch.setattr(phases, "_wait_rbac_propagation_to_snowplow",
                        lambda *a, **k: (True, 10, {"ok": True}))
    monkeypatch.setattr(phases, "_post_mutation_pause", lambda mode: None)
    monkeypatch.setattr(phases, "_snapshot_l1_ready_ts", lambda c: 0)


def test_recording_mode_s8_content_gate_true_against_live_page(
        tmp_path, monkeypatch):
    """ARCHITECT FALSIFIER (stands in for proofs/S8.json under --video): in
    recording mode (subject page=None at setup), S8's real `_work` must report
    `content_card_present: true` because Option A exposes the LIVE filmed
    Compositions page for the gate. Pre-fix this was always false (page=None)
    → S8 false-FAIL.

    The card the pick returns IS present on the subject's (deferred)
    Compositions page, so prop_ok + card present + 0 widget errors → S8 PASSES.
    """
    picked = ["bench-app-01-03", "bench-app-01-04", "bench-app-01-05"]
    _stub_s8_s9_cluster(monkeypatch, comps_in_ns=5)
    monkeypatch.setattr(phases, "_pick_visible_composition_names",
                        lambda ns, k=8: list(picked))
    # browser_measure_stage stub: materialises the lazy Compositions page
    # (Task #307 / A1-full) like the real function so the content gate reads it.
    monkeypatch.setattr(
        phases.browser, "browser_measure_stage",
        _stub_measure_materializing(8))
    monkeypatch.setattr(phases.browser, "make_browser_context",
                        _hermetic_make_ctx)
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)
    _stub_video_to_gif(monkeypatch)

    # The subject's Compositions page renders the picked card.
    pw_browser = _StageRecordingBrowser(present_names={"bench-app-01-04"})
    (tmp_path / "videos").mkdir(parents=True, exist_ok=True)
    ctx = phases.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json", video="representative",
        user_pages={
            "__browser__": pw_browser, "__pw__": None, "__stage_videos__": {},
            # RECORDING shape: page=None at setup (the bug's trigger).
            "admin": {"ctx": None, "page": None, "pwd": "pw-a",
                      "token": "tok-a", "record_video": True},
            "cyberjoker": {"ctx": None, "page": None, "pwd": "pw-c",
                           "token": "tok-c", "record_video": True},
        },
    )

    proof = phases.stage_s8_add_rb_to_populated_ns(ctx)

    body = proof.proof
    assert body["content_card_present"] is True, (
        f"recording-mode S8 content gate must read the LIVE filmed page; "
        f"got content_card_present={body.get('content_card_present')!r}, "
        f"matched={body.get('matched_card_name')!r}")
    assert body["matched_card_name"] == "bench-app-01-04"
    assert proof.passed is True, "S8 must PASS in recording mode"
    # Persisted proof on disk agrees (the --video falsifier surface).
    on_disk = json.loads((tmp_path / "proofs" / "S8.json").read_text())
    assert on_disk["proof"]["content_card_present"] is True
    # Subject Compositions video was finalized (deferred → drained by _run_stage).
    s8_vids = {Path(p).name
               for p in ctx.user_pages.get("__stage_videos__", {}).get("S8", [])}
    assert "S8_cyberjoker_compositions.webm" in s8_vids, s8_vids
    # Deferred live page cleared after drain.
    assert ctx.user_pages["cyberjoker"]["page"] is None


def test_recording_mode_s8_content_gate_false_when_no_card_renders(
        tmp_path, monkeypatch):
    """REGRESSION GUARD: the recording-mode gate must still FAIL (not silently
    pass) when RBAC propagated but NO picked card renders on the live page —
    the #149/#186 breakage the gate exists to catch. Proves Option A did not
    weaken the gate into an always-true.
    """
    picked = ["bench-app-01-03", "bench-app-01-04", "bench-app-01-05"]
    _stub_s8_s9_cluster(monkeypatch, comps_in_ns=5)
    monkeypatch.setattr(phases, "_pick_visible_composition_names",
                        lambda ns, k=8: list(picked))
    monkeypatch.setattr(
        phases.browser, "browser_measure_stage",
        _stub_measure_materializing(8))
    monkeypatch.setattr(phases.browser, "make_browser_context",
                        _hermetic_make_ctx)
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)
    _stub_video_to_gif(monkeypatch)

    # EMPTY DOM — no picked card renders.
    pw_browser = _StageRecordingBrowser(present_names=set())
    (tmp_path / "videos").mkdir(parents=True, exist_ok=True)
    ctx = phases.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json", video="representative",
        user_pages={
            "__browser__": pw_browser, "__pw__": None, "__stage_videos__": {},
            "admin": {"ctx": None, "page": None, "pwd": "pw-a",
                      "token": "tok-a", "record_video": True},
            "cyberjoker": {"ctx": None, "page": None, "pwd": "pw-c",
                           "token": "tok-c", "record_video": True},
        },
    )

    proof = phases.stage_s8_add_rb_to_populated_ns(ctx)
    assert proof.proof["content_card_present"] is False
    assert proof.passed is False, (
        "S8 must still FAIL when no card renders (gate not weakened)")


def test_recording_mode_s9_detects_still_present_card_not_blind(
        tmp_path, monkeypatch):
    """ARCHITECT FALSIFIER (S9 half): in recording mode, S9 must NOT be blind.
    With a card from S8's recorded newest-K STILL present on the live page
    (a revocation that didn't take), S9 must report content_card_absent=False
    and FAIL — pre-fix page=None forced content_card_absent=True (false PASS).
    """
    s8_cards = ["bench-app-01-03", "bench-app-01-04", "bench-app-01-05"]
    _stub_s8_s9_cluster(monkeypatch)
    monkeypatch.setattr(
        phases.browser, "browser_measure_stage",
        _stub_measure_materializing(9))
    monkeypatch.setattr(phases.browser, "make_browser_context",
                        _hermetic_make_ctx)
    monkeypatch.setattr(phases.browser, "browser_login",
                        lambda page, u, p: True)
    _stub_video_to_gif(monkeypatch)

    # Seed S8's proof on disk (S9 reads target_ns/role/rb + expected_card_names).
    s8_state = {
        "schema_version": "1.0.0", "tag": "t", "scale": 5000,
        "stages_completed": ["S8"],
        "stage_proofs": {"S8": {
            "stage_id": "S8", "started_at": "x", "ended_at": "y",
            "passed": True, "artifacts": [],
            "what_breaks_if_skipped": "S8 fake",
            "proof": {
                "subject_user": "cyberjoker", "target_ns": "bench-ns-01",
                "role_name": "r", "rb_name": "rb",
                "expected_card_names": s8_cards,
            },
        }},
    }
    phases.save_state(tmp_path, s8_state)

    # A card from S8's K-list is STILL present → revocation incomplete.
    pw_browser = _StageRecordingBrowser(present_names={"bench-app-01-04"})
    (tmp_path / "videos").mkdir(parents=True, exist_ok=True)
    ctx = phases.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json", video="representative",
        user_pages={
            "__browser__": pw_browser, "__pw__": None, "__stage_videos__": {},
            "admin": {"ctx": None, "page": None, "pwd": "pw-a",
                      "token": "tok-a", "record_video": True},
            "cyberjoker": {"ctx": None, "page": None, "pwd": "pw-c",
                           "token": "tok-c", "record_video": True},
        },
    )

    proof = phases.stage_s9_remove_rb_from_populated_ns(ctx)
    assert proof.proof["content_card_absent"] is False, (
        "recording-mode S9 must DETECT the still-present card (not be blind); "
        f"got content_card_absent={proof.proof.get('content_card_absent')!r}")
    assert proof.proof["still_present_card_name"] == "bench-app-01-04"
    assert proof.passed is False, "S9 must FAIL when a card is still present"


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


# ─── Task #296 — S10 churn-ghost demotion threading + proof telemetry ───────


def test_s10_passes_deleted_ns_and_surfaces_churn_telemetry(tmp_path,
                                                            monkeypatch):
    """#296: stage_s10_delete_one_ns threads the deleted ns into
    validation and surfaces the demotion in its proof. We mock
    _measure_all_users to (a) assert it receives deleted_ns and (b) return
    synthetic navs carrying churn-demoted validation; the proof must then
    count the demoted navs + collect their churn-error detail."""
    from bench import phases as phases_mod

    # Mock the cluster-mutating side effects to no-ops.
    monkeypatch.setattr(phases_mod.lifecycle, "delete_one_bench_namespace",
                        lambda ns: None)
    monkeypatch.setattr(phases_mod.lifecycle, "wait_for_namespace_gone",
                        lambda ns: None)
    monkeypatch.setattr(phases_mod, "_post_mutation_pause", lambda mode: None)
    monkeypatch.setattr(phases_mod, "_snapshot_l1_ready_ts", lambda c: 0)

    captured = {}

    def _fake_measure(ctx, stage_num, stage_desc, deleted_ns=None):
        captured["deleted_ns"] = deleted_ns
        return [{
            "user": "admin",
            "pages": {
                "Compositions": {
                    "navigations": [
                        {"label": "S10 admin Compositions",
                         "validation": {
                             "terminal_state": "pass",
                             "s10_churn_demoted": True,
                             "s10_churn_errors": {
                                 "errored_count": 5,
                                 "errored_namespaces": ["bench-ns-16"],
                                 "deleted_ns": deleted_ns,
                                 "expected_calls": 30,
                                 "actual_calls": 35,
                             }}},
                        {"label": "S10 admin Compositions nav2",
                         "validation": {"terminal_state": "pass",
                                        "s10_churn_demoted": False}},
                    ],
                },
            },
        }]

    monkeypatch.setattr(phases_mod, "_measure_all_users", _fake_measure)

    ctx = StageContext(
        tag="t", scale=50000, run_dir=tmp_path, cache_mode="ON",
    )
    proof = phases_mod.stage_s10_delete_one_ns(ctx)

    # SCALE=50000 → deleted ns is bench-ns-50 (per the stage's formula).
    assert captured["deleted_ns"] == "bench-ns-50"
    # StageProof.proof holds the _work() return dict.
    work = proof.proof
    assert work.get("ns") == "bench-ns-50"
    assert work.get("s10_churn_demoted_navs") == 1
    assert len(work.get("s10_churn_errors") or []) == 1
    assert work["s10_churn_errors"][0]["errored_count"] == 5
    assert work["s10_churn_errors"][0]["deleted_ns"] == "bench-ns-50"


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


def test_pick_visible_composition_name_returns_newest_fallback_on_kubectl_fail(
        monkeypatch):
    """When kubectl returns non-zero, fall back to `bench-app-01-39`
    (a name from the architect-confirmed newest-page range; see
    docs/task-273-s8-second-defect-trace-2026-06-09.md §5.2).
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **kw: (1, "", "err"))
    assert phases_mod._pick_visible_composition_name("bench-ns-01") == \
        "bench-app-01-39"


def test_pick_visible_composition_name_picks_newest_by_timestamp(monkeypatch):
    """The datagrid sorts PANELS by creationTimestamp DESC. We invoke
    kubectl on panels.widgets.templates.krateo.io with the portal-page
    label, --sort-by=.metadata.creationTimestamp (ascending output),
    then take the LAST line's composition-name label as the newest.

    Replaces both prior behaviours: the alphabetical sort that picked
    bench-app-01-02 (row ~997) AND the composition-timestamp sort that
    picked bench-app-01-999 (whose PANEL isn't on the first page —
    panel order diverges from composition order under workers=32
    parallel deploy; empirically observed in run-20260609-200710).
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    # kubectl --sort-by yields ascending; the newest panel is the last line.
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0,
                          "bench-app-01-03\nbench-app-01-05\n"
                          "bench-app-01-39\nbench-app-01-42",
                          ""))
    out = phases_mod._pick_visible_composition_name("bench-ns-01")
    assert out == "bench-app-01-42"


def test_pick_visible_composition_name_queries_panels_not_compositions(
        monkeypatch):
    """Regression guard for the #273 implementation error: the kubectl
    query MUST target panels.widgets.templates.krateo.io with the
    portal-page=compositions label selector and the composition-name
    label column. Querying compositions sorted by THEIR timestamp picks
    a name whose panel may not be on the datagrid first page.
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    captured = {}

    def fake_kubectl(*args, **kwargs):
        captured["args"] = list(args)
        return (0, "bench-app-01-40", "")

    monkeypatch.setattr(cluster_mod, "kubectl", fake_kubectl)
    phases_mod._pick_visible_composition_name("bench-ns-01")
    joined = " ".join(captured["args"])
    assert "panels.widgets.templates.krateo.io" in joined
    assert "krateo.io/portal-page=compositions" in joined
    assert "composition-name" in joined


def test_pick_visible_composition_name_skips_none_labels(monkeypatch):
    """Panels lacking the composition-name label render as `<none>` in
    custom-columns output — they must be skipped, not returned.
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0, "bench-app-01-39\n<none>", ""))
    out = phases_mod._pick_visible_composition_name("bench-ns-01")
    assert out == "bench-app-01-39"


def test_pick_visible_composition_name_falls_back_when_output_empty(
        monkeypatch):
    """kubectl rc=0 but empty stdout (no compositions yet) → fall back
    to the architect-confirmed default newest-range name.
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0, "", ""))
    out = phases_mod._pick_visible_composition_name("bench-ns-01")
    assert out == "bench-app-01-39"


def test_pick_visible_composition_name_invokes_sort_by_creationtimestamp(
        monkeypatch):
    """Regression guard: the kubectl call MUST pass
    --sort-by=.metadata.creationTimestamp. Without that flag, the
    output order is undefined and we'd silently regress to an arbitrary
    name (which is what caused the run-20260609-175816 S8 FAIL).
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    captured = {}

    def fake_kubectl(*args, **kwargs):
        captured["args"] = list(args)
        return (0, "bench-app-01-99", "")

    monkeypatch.setattr(cluster_mod, "kubectl", fake_kubectl)
    phases_mod._pick_visible_composition_name("bench-ns-01")
    assert "--sort-by=.metadata.creationTimestamp" in captured["args"]


# ─── Task #280 / 2026-06-09: any-of-newest-K picker + S8 gate semantics ─────
#
# Rationale: docs/task-280-s8-card-absent-trace-2026-06-09.md §6 Option A1.
# The single-newest assert was structurally racy against snowplow's ratified
# serve-stale window (dirty-mark-not-evict + customer-priority refresher).
# The any-of-newest-K gate tolerates that freshness window while STILL
# failing when ZERO of the K cards render (the #149/#186 breakage classes).


def test_pick_visible_composition_names_returns_newest_k_in_ascending_order(
        monkeypatch):
    """kubectl --sort-by yields ASCENDING order; the newest K are the
    LAST k lines, returned newest-LAST (tail slice) for parity with the
    single-name helper which returns lines[-1].
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0,
                          "bench-app-01-01\nbench-app-01-02\n"
                          "bench-app-01-03\nbench-app-01-04\n"
                          "bench-app-01-05\nbench-app-01-06\n"
                          "bench-app-01-07",
                          ""))
    out = phases_mod._pick_visible_composition_names("bench-ns-01", k=5)
    # Newest 5, newest-LAST: 03,04,05,06,07.
    assert out == [
        "bench-app-01-03", "bench-app-01-04", "bench-app-01-05",
        "bench-app-01-06", "bench-app-01-07",
    ]
    # The single-name wrapper still returns THE newest (last) element.
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0,
                          "bench-app-01-03\nbench-app-01-07",
                          ""))
    assert phases_mod._pick_visible_composition_name("bench-ns-01") == \
        "bench-app-01-07"


def test_pick_visible_composition_names_returns_newest_8_for_s8_k(monkeypatch):
    """Task #282 Option C: the S8/S9 pick site uses k=8. With >=8 panels
    present the picker returns exactly the newest 8 (newest-LAST). This is
    the depth-margin the gate relies on (measured worst-case depth 4 +
    p99 variance => margin >=3).
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    rows = "\n".join(f"bench-app-01-{i:02d}" for i in range(1, 13))  # 01..12
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **kw: (0, rows, ""))
    out = phases_mod._pick_visible_composition_names("bench-ns-01", k=8)
    # Newest 8, newest-LAST: 05..12.
    assert out == [f"bench-app-01-{i:02d}" for i in range(5, 13)]
    assert len(out) == 8


def test_s8_pick_k_is_at_least_per_page():
    """K invariant (Task #282): the S8/S9 newest-K MUST stay >= the
    datagrid per_page so the K-list always covers the full first page.
    Assert the pick-site k (8) is >= COMP_DATAGRID_PER_PAGE (5).
    """
    from bench.expected import COMP_DATAGRID_PER_PAGE
    S8_PICK_K = 8  # the k passed at the stage_s8/stage_s9 pick site
    assert S8_PICK_K >= COMP_DATAGRID_PER_PAGE, (
        f"S8/S9 newest-K ({S8_PICK_K}) must stay >= datagrid per_page "
        f"({COMP_DATAGRID_PER_PAGE}) so the K-list covers the full first page")


def test_pick_visible_composition_names_skips_none_labels(monkeypatch):
    """Panels lacking the composition-name label render `<none>` — they
    must be skipped, not counted toward K.
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0,
                          "bench-app-01-10\n<none>\nbench-app-01-11\n"
                          "<none>\nbench-app-01-12",
                          ""))
    out = phases_mod._pick_visible_composition_names("bench-ns-01", k=5)
    assert out == ["bench-app-01-10", "bench-app-01-11", "bench-app-01-12"]
    assert "<none>" not in out


def test_pick_visible_composition_names_fewer_than_k(monkeypatch):
    """When fewer than K panels exist, return all of them (no padding)."""
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(
        cluster_mod, "kubectl",
        lambda *a, **kw: (0, "bench-app-01-01\nbench-app-01-02", ""))
    out = phases_mod._pick_visible_composition_names("bench-ns-01", k=5)
    assert out == ["bench-app-01-01", "bench-app-01-02"]


def test_pick_visible_composition_names_fallback_on_kubectl_fail(monkeypatch):
    """kubectl non-zero → single-element fallback list so callers always
    have a deterministic non-empty input.
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **kw: (1, "", "err"))
    assert phases_mod._pick_visible_composition_names("bench-ns-01", k=5) == \
        ["bench-app-01-39"]


def test_pick_visible_composition_names_fallback_on_empty_output(monkeypatch):
    """kubectl rc=0 but empty stdout → same single-element fallback."""
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **kw: (0, "   \n  ", ""))
    assert phases_mod._pick_visible_composition_names("bench-ns-01", k=5) == \
        ["bench-app-01-39"]


def test_pick_visible_composition_names_queries_panels_with_label(monkeypatch):
    """Same kubectl query contract as the single-name helper: panels GVR,
    portal-page label, composition-name column, sort-by creationTimestamp.
    """
    from bench import phases as phases_mod
    from bench import cluster as cluster_mod
    captured = {}

    def fake_kubectl(*args, **kwargs):
        captured["args"] = list(args)
        return (0, "bench-app-01-40", "")

    monkeypatch.setattr(cluster_mod, "kubectl", fake_kubectl)
    phases_mod._pick_visible_composition_names("bench-ns-01", k=5)
    joined = " ".join(captured["args"])
    assert "panels.widgets.templates.krateo.io" in joined
    assert "krateo.io/portal-page=compositions" in joined
    assert "composition-name" in joined
    assert "--sort-by=.metadata.creationTimestamp" in captured["args"]


# ─── Task #280 — any-present helper + S8 gate pass/fail semantics ───────────


class _CardLocator:
    def __init__(self, n):
        self._n = n

    def count(self):
        return self._n


class _CardPage:
    """Minimal Playwright Page stand-in: `text=NAME` locators present in
    `present_names` report count>=1; all others count 0. Records reloads.
    """

    def __init__(self, present_names):
        self._present = set(present_names)
        self.reloaded = 0
        self.url = "http://fake/compositions"

    def locator(self, selector):
        name = selector[len("text="):] if selector.startswith("text=") \
            else selector
        return _CardLocator(1 if name in self._present else 0)

    def goto(self, url, **kw):
        self.reloaded += 1

    def wait_for_timeout(self, ms):
        pass


def _ctx_with_page(tmp_path, present_names):
    from bench import phases as phases_mod
    page = _CardPage(present_names)
    ctx = phases_mod.StageContext(
        tag="t", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        tokens={"cyberjoker": "fake-token"},
        user_pages={"cyberjoker": {"page": page}},
    )
    return ctx, page


def test_user_card_any_present_matches_first_present(tmp_path):
    from bench import phases as phases_mod
    # Only the 3rd candidate renders.
    ctx, _ = _ctx_with_page(tmp_path, {"bench-app-01-05"})
    ok, matched = phases_mod._user_card_any_present(
        ctx, "cyberjoker",
        ["bench-app-01-03", "bench-app-01-04", "bench-app-01-05"])
    assert ok is True
    assert matched == "bench-app-01-05"


def test_user_card_any_present_returns_none_when_no_match(tmp_path):
    """The #149/#186 detection guard: when NONE of the K cards render,
    the gate MUST report (False, None) — preserving the gate's power to
    fail when RBAC propagates but no card is in the DOM.
    """
    from bench import phases as phases_mod
    ctx, _ = _ctx_with_page(tmp_path, set())  # empty DOM
    ok, matched = phases_mod._user_card_any_present(
        ctx, "cyberjoker",
        ["bench-app-01-03", "bench-app-01-04", "bench-app-01-05"])
    assert ok is False
    assert matched is None


def test_reload_user_compositions_page_reloads(tmp_path, monkeypatch):
    from bench import phases as phases_mod
    from bench import browser as browser_mod
    monkeypatch.setattr(browser_mod, "FRONTEND", "http://fake")
    ctx, page = _ctx_with_page(tmp_path, set())
    phases_mod._reload_user_compositions_page(ctx, "cyberjoker")
    assert page.reloaded == 1


def test_s8_gate_passes_when_any_of_k_present(tmp_path, monkeypatch):
    """S8 __passed__ is True when propagation_ok, ANY-of-K renders, and
    zero widget errors — even though the single-newest (last) name is
    absent (inside the serve-stale window).
    """
    from bench import phases as phases_mod
    from bench import browser as browser_mod
    monkeypatch.setattr(browser_mod, "FRONTEND", "http://fake")

    # Newest-K (newest last): 07 is newest but stale-absent; 05 renders.
    k_names = ["bench-app-01-03", "bench-app-01-04", "bench-app-01-05",
               "bench-app-01-06", "bench-app-01-07"]
    monkeypatch.setattr(phases_mod, "_pick_visible_composition_names",
                        lambda ns, k=5: list(k_names))

    ctx, _ = _ctx_with_page(tmp_path, {"bench-app-01-05"})
    expected_card_names = phases_mod._pick_visible_composition_names(
        "bench-ns-01", k=5)
    phases_mod._reload_user_compositions_page(ctx, "cyberjoker")
    present, matched = phases_mod._user_card_any_present(
        ctx, "cyberjoker", expected_card_names)
    # Mirror the S8 __passed__ predicate (prop_ok and present and errors==0).
    passed = (True and present and 0 == 0)
    assert present is True
    assert matched == "bench-app-01-05"
    assert passed is True


def test_s8_gate_fails_when_none_of_k_present(tmp_path, monkeypatch):
    """REGRESSION GUARD (#149/#186): when RBAC propagation works but NO
    card from the newest-K renders, the gate MUST fail. This is the whole
    reason the content gate exists — any-of-K must not weaken it to a
    no-op.
    """
    from bench import phases as phases_mod
    from bench import browser as browser_mod
    monkeypatch.setattr(browser_mod, "FRONTEND", "http://fake")

    k_names = ["bench-app-01-03", "bench-app-01-04", "bench-app-01-05"]
    monkeypatch.setattr(phases_mod, "_pick_visible_composition_names",
                        lambda ns, k=5: list(k_names))

    ctx, _ = _ctx_with_page(tmp_path, set())  # empty DOM, no cards at all
    expected_card_names = phases_mod._pick_visible_composition_names(
        "bench-ns-01", k=5)
    phases_mod._reload_user_compositions_page(ctx, "cyberjoker")
    present, matched = phases_mod._user_card_any_present(
        ctx, "cyberjoker", expected_card_names)
    passed = (True and present and 0 == 0)
    assert present is False
    assert matched is None
    assert passed is False


def test_s9_absence_requires_all_k_absent(tmp_path, monkeypatch):
    """S9 contract (Task #280): revocation removes ALL access, so the
    whole grid disappears. With any-of-K semantics the absence gate must
    assert NONE of the K names is present — a revocation that drops only
    SOME rows still leaves one present and FAILS S9 (stronger, not
    weaker, than the prior single-name check).
    """
    from bench import phases as phases_mod
    from bench import browser as browser_mod
    monkeypatch.setattr(browser_mod, "FRONTEND", "http://fake")

    s8_cards = ["bench-app-01-03", "bench-app-01-04", "bench-app-01-05"]

    # Case 1: revocation incomplete — 04 still renders → S9 must FAIL.
    ctx, _ = _ctx_with_page(tmp_path, {"bench-app-01-04"})
    phases_mod._reload_user_compositions_page(ctx, "cyberjoker")
    still_present, present_card = phases_mod._user_card_any_present(
        ctx, "cyberjoker", s8_cards)
    cj_card_absent = (not s8_cards) or (not still_present)
    assert still_present is True
    assert present_card == "bench-app-01-04"
    assert cj_card_absent is False  # gate fails — correct

    # Case 2: full revocation — none render → S9 PASSES.
    ctx2, _ = _ctx_with_page(tmp_path, set())
    phases_mod._reload_user_compositions_page(ctx2, "cyberjoker")
    still_present2, _ = phases_mod._user_card_any_present(
        ctx2, "cyberjoker", s8_cards)
    cj_card_absent2 = (not s8_cards) or (not still_present2)
    assert cj_card_absent2 is True


# ─── Task #251 / 2026-06-09: per-stage pod log capture ──────────────────────
#
# Rationale: agent a16e4da1a29434f24 TRACE on run-20260609-004834-cache-on
# found S8 measurement window (23:30:39 -> 23:35:48 UTC) had ZERO log
# evidence — full_run.txt covered 23:59:11 -> 00:04:09 UTC across a pod
# restart, so all 4 cj allCompositions UAF hypotheses were unfalsifiable.
# These tests pin the per-stage capture contract: stream opens BEFORE
# work, file is on disk + non-empty BEFORE proof persists, file path is
# recorded under proof.artifacts, opt-out via BENCH_NO_PER_STAGE_LOGS,
# reconnect-on-EOF tolerates pod restart.


def test_per_stage_streamer_opt_out_via_env(tmp_path, monkeypatch):
    """BENCH_NO_PER_STAGE_LOGS=1 disables the streamer.

    start() and stop() are no-ops; no file is created; no thread spawned.
    """
    from bench.phases import _PerStageLogStreamer

    monkeypatch.setenv("BENCH_NO_PER_STAGE_LOGS", "1")
    out = tmp_path / "pod_logs" / "S0.txt"
    s = _PerStageLogStreamer(
        stage_id="S0",
        stage_started_utc="2026-06-09T00:00:00+00:00",
        out_path=out,
    )
    assert _PerStageLogStreamer.disabled() is True
    s.start()
    assert s._thread is None
    assert s._fh is None
    s.stop()
    assert not out.exists()


def test_per_stage_streamer_creates_file_on_start(tmp_path, monkeypatch):
    """start() opens the output file in append mode BEFORE the
    supervisor thread reads anything from the subprocess.

    Monkey-patches subprocess.Popen so we don't actually fork kubectl.
    """
    import subprocess as subprocess_mod
    import threading
    from bench import phases as phases_mod
    from bench.phases import _PerStageLogStreamer

    monkeypatch.delenv("BENCH_NO_PER_STAGE_LOGS", raising=False)

    # Fake Popen that produces controllable output then exits.
    class _FakePopen:
        def __init__(self, *args, **kwargs):
            self.args = args
            self._exited = threading.Event()
            self.stdout = self
            self.returncode = 0

        def read(self, n):
            # Block until terminate() fires so the supervisor doesn't
            # spin-respawn during the test.
            self._exited.wait()
            return b""

        def close(self): pass

        def terminate(self): self._exited.set()
        def kill(self): self._exited.set()
        def wait(self, timeout=None): return 0

    monkeypatch.setattr(phases_mod.subprocess, "Popen", _FakePopen)

    out = tmp_path / "pod_logs" / "S5.txt"
    s = _PerStageLogStreamer(
        stage_id="S5",
        stage_started_utc="2026-06-09T00:00:00+00:00",
        out_path=out,
    )
    s.start()
    # File opened in start(), thread running.
    assert out.exists()
    assert s._thread is not None and s._thread.is_alive()
    s.stop()
    # Thread cleaned up.
    assert not s._thread.is_alive()


def test_per_stage_streamer_writes_chunks_then_stops(tmp_path, monkeypatch):
    """Supervisor pipes subprocess stdout into the per-stage file.

    Drives the supervisor with a fake Popen that delivers one chunk
    then EOFs; checks the chunk lands in the file before stop().
    """
    import threading
    from bench import phases as phases_mod
    from bench.phases import _PerStageLogStreamer

    monkeypatch.delenv("BENCH_NO_PER_STAGE_LOGS", raising=False)

    chunk_payload = b'{"msg":"stage_event","stage":"S6"}\n'
    delivered = threading.Event()

    class _FakeStdout:
        def __init__(self):
            self._delivered = False

        def read(self, n):
            if not self._delivered:
                self._delivered = True
                delivered.set()
                return chunk_payload
            return b""  # EOF — triggers respawn-decision branch

        def close(self): pass

    class _FakePopen:
        def __init__(self, *args, **kwargs):
            self.stdout = _FakeStdout()
            self.returncode = 0

        def terminate(self): pass
        def kill(self): pass
        def wait(self, timeout=None): return 0

    monkeypatch.setattr(phases_mod.subprocess, "Popen", _FakePopen)
    # Speed up the test — don't wait 1.5s between respawn iterations.
    monkeypatch.setattr(_PerStageLogStreamer, "_RESPAWN_BACKOFF_S", 0.05)

    out = tmp_path / "pod_logs" / "S6.txt"
    s = _PerStageLogStreamer(
        stage_id="S6",
        stage_started_utc="2026-06-09T00:00:00+00:00",
        out_path=out,
    )
    s.start()
    # Wait for the first chunk to be delivered.
    assert delivered.wait(timeout=5.0), "supervisor never read first chunk"
    # Give the supervisor a beat to write + emit reconnect marker.
    import time as time_mod
    time_mod.sleep(0.2)
    s.stop()
    # Chunk + at least one reconnect marker should be in the file.
    content = out.read_bytes()
    assert chunk_payload in content, \
        f"chunk not in file; got {content!r}"
    assert b"STREAM RECONNECT" in content, \
        f"reconnect marker missing; got {content!r}"


def test_per_stage_streamer_appends_on_reconnect(tmp_path, monkeypatch):
    """Pod-restart simulation: subprocess A produces chunk1 + EOFs,
    supervisor writes reconnect marker, subprocess B produces chunk2.

    Both chunks must land in the same per-stage file (append mode +
    same --since-time on respawn).
    """
    import threading
    from bench import phases as phases_mod
    from bench.phases import _PerStageLogStreamer

    monkeypatch.delenv("BENCH_NO_PER_STAGE_LOGS", raising=False)

    chunk_a = b"chunk-A: pre-restart event\n"
    chunk_b = b"chunk-B: post-restart event\n"
    spawn_counter = [0]
    chunk_b_delivered = threading.Event()

    class _FakeStdout:
        def __init__(self, which):
            self._which = which
            self._delivered = False

        def read(self, n):
            if not self._delivered:
                self._delivered = True
                if self._which == 1:
                    return chunk_a
                else:
                    chunk_b_delivered.set()
                    return chunk_b
            return b""  # EOF

        def close(self): pass

    class _FakePopen:
        def __init__(self, *args, **kwargs):
            spawn_counter[0] += 1
            self.stdout = _FakeStdout(spawn_counter[0])
            self.returncode = 0

        def terminate(self): pass
        def kill(self): pass
        def wait(self, timeout=None): return 0

    monkeypatch.setattr(phases_mod.subprocess, "Popen", _FakePopen)
    monkeypatch.setattr(_PerStageLogStreamer, "_RESPAWN_BACKOFF_S", 0.05)

    out = tmp_path / "pod_logs" / "S7.txt"
    s = _PerStageLogStreamer(
        stage_id="S7",
        stage_started_utc="2026-06-09T00:00:00+00:00",
        out_path=out,
    )
    s.start()
    assert chunk_b_delivered.wait(timeout=5.0), \
        "supervisor never respawned after first subprocess EOF"
    # Give the supervisor a beat to write chunk_b + the second EOF
    # detection + the second reconnect marker.
    import time as time_mod
    time_mod.sleep(0.2)
    s.stop()

    content = out.read_bytes()
    assert chunk_a in content, \
        f"chunk_a (pre-restart) missing from file: {content!r}"
    assert chunk_b in content, \
        f"chunk_b (post-restart) missing from file: {content!r}"
    assert content.index(chunk_a) < content.index(chunk_b), \
        "chunks out of order — append-mode broken?"
    # Both reconnects (after chunk_a EOF, after chunk_b EOF + stop) should
    # have produced markers. Require at least one between the chunks.
    between = content[
        content.index(chunk_a) + len(chunk_a):content.index(chunk_b)]
    assert b"STREAM RECONNECT" in between, \
        "no reconnect marker between chunk_a and chunk_b"


def test_run_stage_attaches_per_stage_log_to_proof_artifacts(
        tmp_path, monkeypatch):
    """_run_stage should attach the per-stage log file path to
    proof.artifacts on the success path.
    """
    import threading
    from bench import phases as phases_mod
    from bench.phases import _PerStageLogStreamer, _run_stage

    monkeypatch.delenv("BENCH_NO_PER_STAGE_LOGS", raising=False)

    chunk_payload = b"S2-event-log\n"
    delivered = threading.Event()

    class _FakeStdout:
        def __init__(self):
            self._delivered = False

        def read(self, n):
            if not self._delivered:
                self._delivered = True
                delivered.set()
                return chunk_payload
            return b""

        def close(self): pass

    class _FakePopen:
        def __init__(self, *args, **kwargs):
            self.stdout = _FakeStdout()
            self.returncode = 0

        def terminate(self): pass
        def kill(self): pass
        def wait(self, timeout=None): return 0

    monkeypatch.setattr(phases_mod.subprocess, "Popen", _FakePopen)
    monkeypatch.setattr(_PerStageLogStreamer, "_RESPAWN_BACKOFF_S", 0.05)

    ctx = StageContext(
        tag="0.30.248", scale=5000,
        run_dir=tmp_path,
        state_path=tmp_path / "state.json",
    )

    def _work(c):
        # Block until we know the supervisor has at least one chunk in
        # flight; otherwise the file may be empty and the artifact
        # attach intentionally skips it.
        delivered.wait(timeout=5.0)
        return {"ok": True}

    proof = _run_stage("S2", ctx, _work,
                       what_breaks_if_skipped="per-stage log capture test")
    assert proof.passed is True
    # The proof.artifacts list must include the per-stage log path
    # (relative to run_dir).
    assert any(a.endswith("pod_logs/S2.txt") for a in proof.artifacts), \
        f"per-stage log not attached; artifacts={proof.artifacts}"
    log_path = tmp_path / "pod_logs" / "S2.txt"
    assert log_path.exists()
    assert log_path.stat().st_size > 0


def test_run_stage_attaches_per_stage_log_on_convergence_timeout(
        tmp_path, monkeypatch):
    """When the stage raises ConvergenceTimeout, the per-stage log
    must still be flushed + attached BEFORE the proof is persisted.

    This is the failure-mode the agent a16e4da1a29434f24 TRACE
    specifically needs: S8 timed out with zero log evidence captured.
    """
    import threading
    from bench import phases as phases_mod
    from bench.phases import _PerStageLogStreamer, _run_stage

    monkeypatch.delenv("BENCH_NO_PER_STAGE_LOGS", raising=False)

    delivered = threading.Event()
    chunk = b"S8-timeout-evidence\n"

    class _FakeStdout:
        def __init__(self):
            self._delivered = False

        def read(self, n):
            if not self._delivered:
                self._delivered = True
                delivered.set()
                return chunk
            return b""

        def close(self): pass

    class _FakePopen:
        def __init__(self, *args, **kwargs):
            self.stdout = _FakeStdout()
            self.returncode = 0

        def terminate(self): pass
        def kill(self): pass
        def wait(self, timeout=None): return 0

    monkeypatch.setattr(phases_mod.subprocess, "Popen", _FakePopen)
    monkeypatch.setattr(_PerStageLogStreamer, "_RESPAWN_BACKOFF_S", 0.05)

    ctx = StageContext(
        tag="0.30.248", scale=5000,
        run_dir=tmp_path,
        state_path=tmp_path / "state.json",
    )

    def _timing_out_work(c):
        delivered.wait(timeout=5.0)
        raise ConvergenceTimeout(
            stage=8, user="cyberjoker",
            api=10, ui=15, cluster=20, timeout_secs=300,
        )

    with pytest.raises(ConvergenceTimeout):
        _run_stage("S8", ctx, _timing_out_work,
                   what_breaks_if_skipped="S8 timeout-capture test")

    # Proof on disk; artifact attached; file non-empty.
    proof_path = tmp_path / "proofs" / "S8.json"
    assert proof_path.exists()
    proof_d = json.loads(proof_path.read_text())
    assert proof_d["passed"] is False
    assert proof_d["convergence_timeout"] is True
    assert any(a.endswith("pod_logs/S8.txt")
               for a in proof_d.get("artifacts", [])), \
        f"per-stage log NOT attached to ConvergenceTimeout proof: " \
        f"artifacts={proof_d.get('artifacts')}"
    log_path = tmp_path / "pod_logs" / "S8.txt"
    assert log_path.exists()
    assert log_path.stat().st_size > 0


def test_run_stage_skips_log_attach_when_streamer_disabled(
        tmp_path, monkeypatch):
    """When BENCH_NO_PER_STAGE_LOGS=1, no per-stage file is created
    and no artifact is recorded — but the stage proof itself is still
    persisted normally.
    """
    from bench.phases import _run_stage

    monkeypatch.setenv("BENCH_NO_PER_STAGE_LOGS", "1")

    ctx = StageContext(
        tag="0.30.248", scale=5000,
        run_dir=tmp_path,
        state_path=tmp_path / "state.json",
    )
    proof = _run_stage("S3", ctx, lambda c: {"ok": True},
                       what_breaks_if_skipped="opt-out test")
    assert proof.passed is True
    # No per-stage log file should have been created.
    assert not (tmp_path / "pod_logs" / "S3.txt").exists()
    # No artifact entry referencing pod_logs/.
    assert not any("pod_logs" in a for a in proof.artifacts), \
        f"unexpected pod_logs artifact when opted out: {proof.artifacts}"
