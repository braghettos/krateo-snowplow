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


# ─── Task #250 Block 2 — new RBAC primitives ────────────────────────────────
#
# Hermetic-isolation contract (feedback_bench_tests_hermetic_isolation_all_
# cluster_paths): every test that touches `k8s_create_*` MUST monkeypatch
# `_k8s_init` so the test FAILS FAST against a real GKE context. The
# reset_k8s_state fixture + monkeypatch of _k8s_init satisfies the
# contract end-to-end (the apiserver client globals are nulled at fixture
# entry; the test installs fakes; the fixture restores at exit).


def test_k8s_create_namespaced_role_returns_false_when_init_fails(
        reset_k8s_state, monkeypatch):
    """When `_k8s_init` returns False (lib missing / cluster unreachable),
    `k8s_create_namespaced_role` returns False — does NOT crash, does NOT
    contact a real cluster.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False
    result = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        api_groups=["composition.krateo.io"],
        resources=["*"],
        verbs=["get"],
    )
    assert result is False


def test_k8s_create_namespaced_role_uses_client_when_available(
        reset_k8s_state, monkeypatch):
    """The create call uses the rbac client's `create_namespaced_role`
    method with a V1Role body — single REST call, idempotent on 409.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeRbacAPI:
        def create_namespaced_role(self, namespace, body, _request_timeout):
            captured["namespace"] = namespace
            captured["body"] = body
            captured["timeout"] = _request_timeout

    class _FakeV1Role:
        def __init__(self, metadata=None, rules=None):
            self.metadata = metadata
            self.rules = rules

    class _FakeV1ObjectMeta:
        def __init__(self, name=None):
            self.name = name

    class _FakeV1PolicyRule:
        def __init__(self, api_groups=None, resources=None, verbs=None):
            self.api_groups = api_groups
            self.resources = resources
            self.verbs = verbs

    class _FakeClientMod:
        V1Role = _FakeV1Role
        V1ObjectMeta = _FakeV1ObjectMeta
        V1PolicyRule = _FakeV1PolicyRule

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    cluster_mod._k8s_rbac = _FakeRbacAPI()

    ok = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        api_groups=["composition.krateo.io"],
        resources=["*"],
        verbs=["get", "list"],
    )
    assert ok is True
    assert captured["namespace"] == "bench-ns-01"
    body = captured["body"]
    assert body.metadata.name == "test-role"
    assert len(body.rules) == 1
    assert body.rules[0].api_groups == ["composition.krateo.io"]
    assert body.rules[0].resources == ["*"]
    assert body.rules[0].verbs == ["get", "list"]


def test_k8s_create_namespaced_role_swallows_409(
        reset_k8s_state, monkeypatch):
    """AlreadyExists (HTTP 409) is treated as success — idempotent on
    re-runs.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            self.status = status

    class _FakeClientMod:
        V1Role = type("V1Role", (), {"__init__": lambda s, **kw: None})
        V1ObjectMeta = type("V1ObjectMeta", (),
                            {"__init__": lambda s, **kw: None})
        V1PolicyRule = type("V1PolicyRule", (),
                            {"__init__": lambda s, **kw: None})

        class exceptions:
            ApiException = _FakeAPIException

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)

    class _FakeRbacAPI:
        def create_namespaced_role(self, namespace, body, _request_timeout):
            raise _FakeAPIException(status=409)

    cluster_mod._k8s_rbac = _FakeRbacAPI()
    ok = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        api_groups=["composition.krateo.io"],
        resources=["*"],
        verbs=["get"],
    )
    assert ok is True


def test_k8s_create_namespaced_role_binding_constructs_subject_list(
        reset_k8s_state, monkeypatch):
    """The RoleBinding body carries the role_ref + subjects exactly as
    threaded — no hardcoded cyberjoker / devs identity participates.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeRbacAPI:
        def create_namespaced_role_binding(self, namespace, body,
                                           _request_timeout):
            captured["namespace"] = namespace
            captured["body"] = body

    class _FakeV1RoleBinding:
        def __init__(self, metadata=None, role_ref=None, subjects=None):
            self.metadata = metadata
            self.role_ref = role_ref
            self.subjects = subjects

    class _FakeV1RoleRef:
        def __init__(self, api_group=None, kind=None, name=None):
            self.api_group = api_group
            self.kind = kind
            self.name = name

    class _FakeV1Subject:
        def __init__(self, kind=None, name=None, api_group=None,
                     namespace=None):
            self.kind = kind
            self.name = name
            self.api_group = api_group
            self.namespace = namespace

    class _FakeV1ObjectMeta:
        def __init__(self, name=None):
            self.name = name

    class _FakeClientMod:
        V1RoleBinding = _FakeV1RoleBinding
        V1RoleRef = _FakeV1RoleRef
        V1Subject = _FakeV1Subject
        V1ObjectMeta = _FakeV1ObjectMeta

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    cluster_mod._k8s_rbac = _FakeRbacAPI()

    ok = cluster_mod.k8s_create_namespaced_role_binding(
        "bench-ns-01", "test-rb",
        role_ref=("Role", "test-role"),
        subjects=[
            {"kind": "Group", "name": "devs"},
            {"kind": "ServiceAccount", "name": "sa1",
             "namespace": "krateo-system"},
        ],
    )
    assert ok is True
    body = captured["body"]
    assert body.metadata.name == "test-rb"
    assert body.role_ref.kind == "Role"
    assert body.role_ref.name == "test-role"
    assert body.role_ref.api_group == "rbac.authorization.k8s.io"
    assert len(body.subjects) == 2
    # Group subject: default api_group filled in.
    assert body.subjects[0].kind == "Group"
    assert body.subjects[0].name == "devs"
    assert body.subjects[0].api_group == "rbac.authorization.k8s.io"
    # SA subject: api_group empty, namespace honoured.
    assert body.subjects[1].kind == "ServiceAccount"
    assert body.subjects[1].name == "sa1"
    assert body.subjects[1].api_group == ""
    assert body.subjects[1].namespace == "krateo-system"


def test_k8s_read_namespaced_role_binding_returns_none_on_404(
        reset_k8s_state, monkeypatch):
    """A 404 NotFound is swallowed and returns None (not raise)."""
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            self.status = status

    class _FakeClientMod:
        class exceptions:
            ApiException = _FakeAPIException

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)

    class _FakeRbacAPI:
        def read_namespaced_role_binding(self, name, namespace,
                                         _request_timeout):
            raise _FakeAPIException(status=404)

    cluster_mod._k8s_rbac = _FakeRbacAPI()
    out = cluster_mod.k8s_read_namespaced_role_binding(
        "bench-ns-01", "missing-rb")
    assert out is None


def test_k8s_read_namespaced_role_binding_returns_object_on_200(
        reset_k8s_state, monkeypatch):
    """A successful read returns the rb object (not None, not raise)."""
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    sentinel = {"metadata": {"name": "test-rb"}}

    class _FakeRbacAPI:
        def read_namespaced_role_binding(self, name, namespace,
                                         _request_timeout):
            return sentinel

    cluster_mod._k8s_rbac = _FakeRbacAPI()
    out = cluster_mod.k8s_read_namespaced_role_binding(
        "bench-ns-01", "test-rb")
    assert out is sentinel


def test_k8s_create_helpers_never_touch_real_cluster_when_lib_unavailable(
        reset_k8s_state, monkeypatch):
    """Hermetic-isolation contract: with `_K8S_LIB_AVAILABLE=False`, no
    create/read helper should make any subprocess call OR raise. Returns
    False (create) / None (read) so the caller falls back deterministically.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False

    # subprocess.run must NOT be called by these helpers (they go through
    # the k8s client lib only). Monkeypatch subprocess.run to a tripwire.
    def _tripwire(*a, **kw):
        raise AssertionError(
            "k8s_* helper escaped to subprocess.run — hermetic-isolation "
            "contract violated")

    monkeypatch.setattr(subprocess, "run", _tripwire)

    assert cluster_mod.k8s_create_namespaced_role(
        "ns", "name", api_groups=[], resources=[], verbs=[]) is False
    assert cluster_mod.k8s_create_namespaced_role_binding(
        "ns", "name", role_ref=("Role", "r"), subjects=[]) is False
    assert cluster_mod.k8s_read_namespaced_role_binding(
        "ns", "name") is None


# ─── Task #250 Block 2b re-gate fix — count_compositions_with_panels_ready ──


def test_count_comp_panels_returns_none_when_init_fails(
        reset_k8s_state, monkeypatch):
    """When `_k8s_init` returns False, the helper returns None so the
    caller falls back to BASE expected (does NOT crash, does NOT hit a
    real cluster).
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False
    assert cluster_mod.count_compositions_with_panels_ready(
        target_ns=None) is None
    assert cluster_mod.count_compositions_with_panels_ready(
        target_ns="bench-ns-01") is None


def test_count_comp_panels_cluster_wide_uses_list_cluster_custom_object(
        reset_k8s_state, monkeypatch):
    """target_ns=None → list_cluster_custom_object with the comp-panel
    label_selector. Asserts the GVR + label match the empirical
    ground-truth (probed against gke_neon-481711 at 2026-06-05).
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeCustom:
        def list_cluster_custom_object(self, group, version, plural,
                                       label_selector, _request_timeout):
            captured["group"] = group
            captured["version"] = version
            captured["plural"] = plural
            captured["label_selector"] = label_selector
            return {"items": [{"i": i} for i in range(4423)]}

        def list_namespaced_custom_object(self, **kw):
            raise AssertionError(
                "list_namespaced_custom_object called when target_ns=None")

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.count_compositions_with_panels_ready(target_ns=None)
    assert out == 4423
    # Empirical GVR + label match Task #250 design + 2026-06-05 probe.
    assert captured["group"] == "widgets.templates.krateo.io"
    assert captured["version"] == "v1beta1"
    assert captured["plural"] == "panels"
    assert captured["label_selector"] == "krateo.io/portal-page=compositions"


def test_count_comp_panels_namespaced_uses_list_namespaced_custom_object(
        reset_k8s_state, monkeypatch):
    """target_ns="bench-ns-01" → list_namespaced_custom_object with same
    label_selector + namespace. cyberjoker S8 case.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeCustom:
        def list_namespaced_custom_object(self, group, version, namespace,
                                          plural, label_selector,
                                          _request_timeout):
            captured["group"] = group
            captured["version"] = version
            captured["namespace"] = namespace
            captured["plural"] = plural
            captured["label_selector"] = label_selector
            # Empirical 2026-06-05 probe: bench-ns-01 had 143 comp-panels.
            return {"items": [{"i": i} for i in range(143)]}

        def list_cluster_custom_object(self, **kw):
            raise AssertionError(
                "list_cluster_custom_object called when target_ns is set")

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.count_compositions_with_panels_ready(
        target_ns="bench-ns-01")
    assert out == 143
    assert captured["namespace"] == "bench-ns-01"
    assert captured["plural"] == "panels"
    assert captured["label_selector"] == "krateo.io/portal-page=compositions"


def test_count_comp_panels_returns_zero_on_404(
        reset_k8s_state, monkeypatch):
    """A 404 NotFound (panel CRD not installed) → 0 (not None). Caller
    treats 0 as 'cluster has no comp-panels' which collapses the
    formula to BASE.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            self.status = status

    class _FakeClientMod:
        class exceptions:
            ApiException = _FakeAPIException

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)

    class _FakeCustom:
        def list_cluster_custom_object(self, group, version, plural,
                                       label_selector, _request_timeout):
            raise _FakeAPIException(status=404)

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.count_compositions_with_panels_ready(target_ns=None)
    assert out == 0


def test_count_comp_panels_returns_none_on_transport_failure(
        reset_k8s_state, monkeypatch):
    """Non-404 exception (transport / 5xx) → None so caller falls back
    to BASE. Mirrors the convention of all other k8s_* helpers in this
    module.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            self.status = status

    class _FakeClientMod:
        class exceptions:
            ApiException = _FakeAPIException

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)

    class _FakeCustom:
        def list_cluster_custom_object(self, group, version, plural,
                                       label_selector, _request_timeout):
            raise _FakeAPIException(status=503)

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.count_compositions_with_panels_ready(target_ns=None)
    assert out is None


def test_count_comp_panels_never_touches_subprocess(
        reset_k8s_state, monkeypatch):
    """Tripwire: the k8s-client path NEVER escapes to subprocess.run.
    Mirrors the contract for create/read primitives.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False

    def _tripwire(*a, **kw):
        raise AssertionError(
            "count_compositions_with_panels_ready escaped to "
            "subprocess.run — hermetic-isolation contract violated")

    monkeypatch.setattr(subprocess, "run", _tripwire)
    assert cluster_mod.count_compositions_with_panels_ready(
        target_ns=None) is None
    assert cluster_mod.count_compositions_with_panels_ready(
        target_ns="bench-ns-01") is None
