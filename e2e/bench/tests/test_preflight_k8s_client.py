"""Tests for the #320-follow-up k8s-client preflight (0.30.258 INVALID-run
lesson): a venv missing the `kubernetes` lib must fail `bench check` (gate 8)
and abort `bench phase6` at minute 0 — never degrade S8/S9/S8b/S8c silently
hours into a run. Also pins the in-process client's kubeconfig context to the
canonical GKE context (gcloud rewrites current-context).
"""

from __future__ import annotations

import argparse

import pytest

import bench.cli as cli_mod
import bench.cluster as cluster_mod
from bench.cli import _gate_k8s_client, cmd_check, cmd_phase6


# ─── The gate function itself ────────────────────────────────────────────────


def test_gate_fails_when_lib_missing(monkeypatch):
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    ok, msg = _gate_k8s_client()
    assert ok is False
    assert "pip install" in msg and "kubernetes" in msg


def test_gate_fails_when_init_fails(monkeypatch):
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    monkeypatch.setattr(cluster_mod, "_k8s_init", lambda: False)
    ok, msg = _gate_k8s_client()
    assert ok is False
    assert "init failed" in msg
    # The message names the canonical context for the operator.
    assert cluster_mod.CANONICAL_GKE_CONTEXT in msg


def test_gate_passes_when_client_initializes(monkeypatch):
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    monkeypatch.setattr(cluster_mod, "_k8s_init", lambda: True)
    ok, msg = _gate_k8s_client()
    assert ok is True
    assert "PASS" in msg


# ─── cmd_check wiring (gate 8) ──────────────────────────────────────────────


def _stub_other_gates(monkeypatch):
    monkeypatch.setattr(cli_mod, "_gate_pod_ready",
                        lambda: (True, "snowplow_pod_ready: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_image_match",
                        lambda tag: (True, "image_tag_match: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_crds_present",
                        lambda timeout=10: (True, "crds_present: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_helm_lockstep",
                        lambda tag: (True, "helm_release_lockstep: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_frontend_reachable",
                        lambda: (True, "frontend_lb_reachable: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_overlay_freshness",
                        lambda allow_stale: (True, "overlay_freshness: PASS"))
    monkeypatch.setattr(cli_mod, "_gke_context_guard",
                        lambda allow_non_gke=False: "stub-context")


def test_check_fails_on_k8s_client_gate(monkeypatch, capsys):
    """Gate 8 FAIL alone must fail the whole check (exit 2, named)."""
    monkeypatch.setenv("BENCH_ALLOW_NON_GKE", "1")
    _stub_other_gates(monkeypatch)
    monkeypatch.setattr(cli_mod, "_gate_k8s_client",
                        lambda: (False, "k8s_client: FAIL (stub)"))

    args = argparse.Namespace(
        cmd="check", tag="0.30.258",
        allow_non_gke=True, allow_stale_overlay=False,
    )
    rc = cmd_check(args)
    assert rc == 2
    err = capsys.readouterr().err
    assert "k8s_client" in err


def test_check_passes_with_k8s_client_gate_green(monkeypatch, capsys):
    monkeypatch.setenv("BENCH_ALLOW_NON_GKE", "1")
    _stub_other_gates(monkeypatch)
    monkeypatch.setattr(cli_mod, "_gate_k8s_client",
                        lambda: (True, "k8s_client: PASS"))

    args = argparse.Namespace(
        cmd="check", tag="0.30.258",
        allow_non_gke=True, allow_stale_overlay=False,
    )
    rc = cmd_check(args)
    assert rc == 0
    out = capsys.readouterr().out
    assert "8/8 gates" in out


# ─── cmd_phase6 minute-0 abort ──────────────────────────────────────────────


def test_phase6_aborts_before_lock_when_client_unavailable(monkeypatch,
                                                           capsys):
    """phase6 must exit 2 BEFORE acquiring the run lock or running any
    stage when the in-process client is unavailable."""
    monkeypatch.setattr(cli_mod, "_gate_overlay_freshness",
                        lambda allow_stale: (True, "overlay_freshness: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_k8s_client",
                        lambda: (False, "k8s_client: FAIL (stub)"))

    def _boom_lock():
        raise AssertionError("run lock must not be acquired on gate FAIL")

    monkeypatch.setattr(cli_mod, "_acquire_run_lock", _boom_lock)

    args = argparse.Namespace(
        cmd="phase6", tag="0.30.258", scale=50000,
        from_stage=None, to_stage=None, cache_mode="ON",
        video="representative", allow_stale_overlay=False, run_dir=None,
    )
    rc = cmd_phase6(args)
    assert rc == 2
    assert "k8s_client" in capsys.readouterr().err


# ─── _k8s_init context pin ──────────────────────────────────────────────────


class _RecorderConfig:
    """Fake kubernetes.config module recording load_kube_config contexts."""

    def __init__(self):
        self.contexts = []

    def load_kube_config(self, context=None):
        self.contexts.append(context)

    def load_incluster_config(self):
        raise AssertionError("must not fall back to in-cluster config")


class _FakeClientMod:
    class _API:
        pass

    CoreV1Api = _API
    RbacAuthorizationV1Api = _API
    CustomObjectsApi = _API
    ApiextensionsV1Api = _API


def _fresh_init_state(monkeypatch):
    monkeypatch.setattr(cluster_mod, "K8S_CLIENT_AVAILABLE", False)
    monkeypatch.setattr(cluster_mod, "_k8s_init_attempted", False)
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)


def test_k8s_init_pins_canonical_context_on_gke(monkeypatch, reset_k8s_state):
    _fresh_init_state(monkeypatch)
    rec = _RecorderConfig()
    monkeypatch.setattr(cluster_mod, "_k8s_config_mod", rec)
    monkeypatch.delenv("BENCH_ALLOW_NON_GKE", raising=False)

    assert cluster_mod._k8s_init() is True
    assert rec.contexts == [cluster_mod.CANONICAL_GKE_CONTEXT]


def test_k8s_init_default_context_when_non_gke_allowed(monkeypatch,
                                                       reset_k8s_state):
    """kind/minikube escape hatch: BENCH_ALLOW_NON_GKE=1 → no pin (None =
    kubeconfig current-context), preserving local-cluster use."""
    _fresh_init_state(monkeypatch)
    rec = _RecorderConfig()
    monkeypatch.setattr(cluster_mod, "_k8s_config_mod", rec)
    monkeypatch.setenv("BENCH_ALLOW_NON_GKE", "1")

    assert cluster_mod._k8s_init() is True
    assert rec.contexts == [None]
