"""Behavioural tests for bench/cluster.py.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.1. No source
introspection — only real input → real output assertions.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from unittest import mock

import pytest


# ─── Import the module under test ────────────────────────────────────────────

# conftest.py adds e2e/bench/ to sys.path AND sets BENCH_ALLOW_NON_GKE=1.

import bench.cluster as cluster_mod
from bench.cluster import (
    CANONICAL_GKE_CONTEXT,
    call_url,
    composition_yaml,
    gke_context_guard,
    k8s_split_gvr,
    kubectl,
    pct,
)


# ─── G6 fixture: optionally exercise the gate against the real cluster ──────

@pytest.fixture
def with_env(monkeypatch):
    """Set env vars for the duration of a test."""
    def _set(**kwargs):
        for k, v in kwargs.items():
            if v is None:
                monkeypatch.delenv(k, raising=False)
            else:
                monkeypatch.setenv(k, v)
    return _set


# ─── Case 1: gke_context_guard exits on wrong context ───────────────────────

def test_gke_context_guard_exits_on_wrong_context(monkeypatch, with_env):
    """When kubectl current-context != canonical GKE, guard exits with 3."""
    with_env(BENCH_ALLOW_NON_GKE=None)

    class _Fake:
        def __init__(self, rc, stdout):
            self.returncode = rc
            self.stdout = stdout.encode()

    def _run(argv, **kwargs):
        return _Fake(0, "kind-some-other-cluster\n")

    monkeypatch.setattr(subprocess, "run", _run)

    with pytest.raises(SystemExit) as exc_info:
        gke_context_guard()
    assert exc_info.value.code == 3


def test_gke_context_guard_passes_on_canonical_cluster(monkeypatch, with_env):
    """When kubectl current-context == canonical GKE, guard returns silently."""
    with_env(BENCH_ALLOW_NON_GKE=None)

    class _Fake:
        def __init__(self, rc, stdout):
            self.returncode = rc
            self.stdout = stdout.encode()

    def _run(argv, **kwargs):
        return _Fake(0, CANONICAL_GKE_CONTEXT + "\n")

    monkeypatch.setattr(subprocess, "run", _run)
    # Should NOT raise / exit.
    gke_context_guard()


def test_gke_context_guard_bypassed_by_env_flag(monkeypatch, with_env):
    """BENCH_ALLOW_NON_GKE=1 bypasses the gate even on a kind cluster."""
    with_env(BENCH_ALLOW_NON_GKE="1")

    def _run(argv, **kwargs):
        raise AssertionError("kubectl should not be invoked when env flag set")

    monkeypatch.setattr(subprocess, "run", _run)
    gke_context_guard()  # no exception, no kubectl invocation


# ─── Case 2/3: kubectl wrapper success + failure paths ──────────────────────

def test_kubectl_returns_tuple_on_success(monkeypatch):
    """kubectl(...) returns (rc, stdout_str, stderr_str)."""

    class _Fake:
        def __init__(self):
            self.returncode = 0
            self.stdout = b"hello\n"
            self.stderr = b""

    monkeypatch.setattr(subprocess, "run", lambda *a, **kw: _Fake())
    rc, out, err = kubectl("version")
    assert rc == 0
    assert out == "hello"  # stripped
    assert err == ""


def test_kubectl_returns_rc1_on_timeout(monkeypatch):
    """A subprocess timeout returns rc=1 + a 'timed out' stderr."""
    def _raise(*args, **kwargs):
        raise subprocess.TimeoutExpired(cmd=args[0], timeout=kwargs.get("timeout"))

    monkeypatch.setattr(subprocess, "run", _raise)
    rc, out, err = kubectl("get", "pods", timeout_secs=5)
    assert rc == 1
    assert out == ""
    assert "timed out" in err
    assert "5s" in err


# ─── Case 4/5: _k8s_init idempotence + one-shot retry semantics ─────────────

def test_k8s_init_short_circuits_when_lib_missing(reset_k8s_state):
    """When `_K8S_LIB_AVAILABLE=False`, `_k8s_init` returns False without retry."""
    cluster_mod = reset_k8s_state
    with mock.patch.object(cluster_mod, "_K8S_LIB_AVAILABLE", False):
        assert cluster_mod._k8s_init() is False
        # Second call also returns False (no side effects, no kubeconfig load).
        assert cluster_mod._k8s_init() is False


def test_k8s_init_one_shot_attempt_lock(reset_k8s_state, monkeypatch):
    """After a failed init attempt, `_k8s_init_attempted=True` short-circuits."""
    cluster_mod = reset_k8s_state
    # Simulate lib available but load_kube_config + load_incluster_config both fail.
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = False
    cluster_mod._k8s_init_attempted = False

    class _FakeConfig:
        @staticmethod
        def load_kube_config():
            raise RuntimeError("no kubeconfig")

        @staticmethod
        def load_incluster_config():
            raise RuntimeError("not in-cluster")

    monkeypatch.setattr(cluster_mod, "_k8s_config_mod", _FakeConfig)
    # First call attempts + fails.
    assert cluster_mod._k8s_init() is False
    assert cluster_mod._k8s_init_attempted is True
    # Second call short-circuits (without re-trying load_kube_config).
    call_count = [0]

    def _counting_load_kube_config():
        call_count[0] += 1
        raise RuntimeError("should not be called again")

    monkeypatch.setattr(_FakeConfig, "load_kube_config", _counting_load_kube_config)
    assert cluster_mod._k8s_init() is False
    assert call_count[0] == 0


# ─── Case 6/7: k8s_split_gvr parses and rejects bad input ────────────────────

def test_k8s_split_gvr_parses_canonical_strings():
    """`plural.group` parses into (group, plural)."""
    assert k8s_split_gvr("compositions.composition.krateo.io") == \
        ("composition.krateo.io", "compositions")
    assert k8s_split_gvr("applications.argoproj.io") == ("argoproj.io", "applications")


def test_k8s_split_gvr_rejects_bare_input():
    """A string with no '.' returns (None, original)."""
    g, p = k8s_split_gvr("nogroup")
    assert g is None
    assert p == "nogroup"


# ─── Case 8: k8s_list_cluster_custom returns [] when CRD missing ────────────

def test_k8s_list_cluster_custom_returns_empty_when_missing(
        reset_k8s_state, monkeypatch):
    """A 404 NotFound is swallowed and returns [] (not None or raise)."""
    cluster_mod = reset_k8s_state
    # Pretend k8s client lib available + init succeeded.
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            super().__init__(f"status={status}")
            self.status = status

    # Patch the kubernetes module's exception class so `_k8s_is_404` works.
    class _FakeExceptions:
        ApiException = _FakeAPIException

    class _FakeKubeClient:
        exceptions = _FakeExceptions

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeKubeClient)

    class _FakeCustom:
        def list_cluster_custom_object(self, group, version, plural, _request_timeout):
            raise _FakeAPIException(404)

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.k8s_list_cluster_custom("foo.example.com", "v1", "foos")
    assert out == []


# ─── Case 9: pct returns nearest-rank percentile ────────────────────────────

def test_pct_returns_nearest_rank_percentile():
    """pct([1..10], 50) = 5; pct([1..10], 99) = 10; pct([], 50) is None."""
    data = list(range(1, 11))
    assert pct(data, 50) == 5
    assert pct(data, 90) == 9
    assert pct(data, 99) == 10
    assert pct(data, 0) == 1
    assert pct([], 50) is None


# ─── Case 10: call_url constructs the bench composition GVR URL ─────────────

def test_call_url_constructs_authenticated_path():
    """call_url(ns) embeds GVR; call_url(ns, name) appends &name=."""
    url = call_url("bench-ns-01")
    assert url.startswith("/call?")
    assert "composition.krateo.io" in url
    assert "githubscaffoldingwithcompositionpages" in url
    assert "namespace=bench-ns-01" in url
    assert "name=" not in url

    url2 = call_url("bench-ns-01", "bench-app-01-01")
    assert "name=bench-app-01-01" in url2


# ─── Bonus: composition_yaml is YAML-shaped (sanity, not source-grep) ───────

def test_composition_yaml_contains_required_fields():
    """composition_yaml(ns, name) produces a parseable manifest with the
    bench composition GVR + correct ns/name.
    """
    body = composition_yaml("bench-ns-99", "bench-app-99-77")
    # YAML round-trip would require PyYAML; instead, assert key markers
    # (the manifest is a heredoc, not arbitrary user input — string
    # check is sufficient for the layering contract).
    assert "apiVersion: composition.krateo.io/v1-2-2" in body
    assert "kind: GithubScaffoldingWithCompositionPage" in body
    assert "name: bench-app-99-77" in body
    assert "namespace: bench-ns-99" in body
