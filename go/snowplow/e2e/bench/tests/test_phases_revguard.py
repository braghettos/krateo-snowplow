"""Tests for the #320 per-stage deploy-revision guard (report-only).

From the #319 trace: a snowplow re-roll inside a measurement window must be
visible on the stage proof (deploy_revision_before/after +
pod_rerolled_mid_stage) instead of silently corrupting the sample. The
guard NEVER gates a stage — cache-toggle stages restart snowplow
intentionally.
"""

from __future__ import annotations

import json
import threading

from bench import phases

# Captured at collection time, BEFORE the conftest autouse fixture stubs the
# module attribute — this is the real implementation under test.
_REAL_FINGERPRINT = phases._snowplow_deploy_fingerprint


class _NullStreamer:
    def __init__(self, *a, **k):
        self._running = threading.Event()

    def start(self):
        self._running.set()

    def stop(self):
        self._running.clear()


def _ctx(tmp_path):
    return phases.StageContext(
        tag="0.30.320", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        tokens={"admin": "tok"}, user_pages={},
    )


def _deploy_json(revision, restarted_at=None):
    ann = {}
    if restarted_at:
        ann["kubectl.kubernetes.io/restartedAt"] = restarted_at
    return json.dumps({
        "metadata": {"annotations": {
            "deployment.kubernetes.io/revision": revision}},
        "spec": {"template": {"metadata": {"annotations": ann}}},
    })


def _stub_fingerprint_seq(monkeypatch, fingerprints):
    """Replace the (autouse-stubbed) fingerprint with a scripted sequence."""
    seq = iter(fingerprints)
    monkeypatch.setattr(phases, "_snowplow_deploy_fingerprint",
                        lambda: next(seq))


# ─── The real fingerprint reader (kubectl JSON parse) ────────────────────────


def test_fingerprint_parses_revision_and_restarted_at(monkeypatch):
    body = _deploy_json("2222", restarted_at="2026-06-11T13:25:58Z")
    monkeypatch.setattr(phases.cluster, "kubectl",
                        lambda *a, **k: (0, body, ""))
    fp = _REAL_FINGERPRINT()
    assert fp == {"revision": "2222",
                  "restarted_at": "2026-06-11T13:25:58Z"}


def test_fingerprint_none_on_kubectl_failure_or_garbage(monkeypatch):
    monkeypatch.setattr(phases.cluster, "kubectl",
                        lambda *a, **k: (1, "", "boom"))
    assert _REAL_FINGERPRINT() is None
    monkeypatch.setattr(phases.cluster, "kubectl",
                        lambda *a, **k: (0, "not-json", ""))
    assert _REAL_FINGERPRINT() is None


# ─── The guard through _run_stage (report-only semantics) ────────────────────


def test_stage_proof_carries_stable_revision_window(tmp_path, monkeypatch):
    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)
    _stub_fingerprint_seq(monkeypatch, [
        {"revision": "7", "restarted_at": None},
        {"revision": "7", "restarted_at": None},
    ])

    proof = phases._run_stage("SX", _ctx(tmp_path), lambda c: {"x": 1},
                              what_breaks_if_skipped="t")

    assert proof.proof["deploy_revision_before"] == "7"
    assert proof.proof["deploy_revision_after"] == "7"
    assert proof.proof["pod_rerolled_mid_stage"] is False
    assert proof.passed is True


def test_stage_proof_flags_mid_stage_reroll_but_still_passes(tmp_path,
                                                             monkeypatch):
    """Revision moved mid-stage → flagged + loud log, but REPORT-ONLY:
    the stage outcome is untouched (#320 decision — toggle stages restart
    snowplow intentionally; gating would false-positive there)."""
    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)
    _stub_fingerprint_seq(monkeypatch, [
        {"revision": "7", "restarted_at": None},
        {"revision": "8", "restarted_at": "2026-06-11T13:25:58Z"},
    ])
    warns = []
    monkeypatch.setattr(phases.lifecycle, "log", lambda m: warns.append(m))

    proof = phases._run_stage("SX", _ctx(tmp_path), lambda c: {"x": 1},
                              what_breaks_if_skipped="t")

    assert proof.proof["pod_rerolled_mid_stage"] is True
    assert proof.proof["deploy_revision_before"] == "7"
    assert proof.proof["deploy_revision_after"] == "8"
    assert proof.passed is True
    assert any("MID-STAGE" in w for w in warns)
    assert any("restartedAt=2026-06-11T13:25:58Z" in w for w in warns)


def test_unreadable_deployment_never_breaks_stage(tmp_path, monkeypatch):
    """Fingerprint unreadable on both sides → fields None, no flag, stage
    unaffected (telemetry must never fail a run)."""
    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)
    _stub_fingerprint_seq(monkeypatch, [None, None])

    proof = phases._run_stage("SX", _ctx(tmp_path), lambda c: {"x": 1},
                              what_breaks_if_skipped="t")

    assert proof.proof["deploy_revision_before"] is None
    assert proof.proof["deploy_revision_after"] is None
    assert proof.proof["pod_rerolled_mid_stage"] is False
    assert proof.passed is True
