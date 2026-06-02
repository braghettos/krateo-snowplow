"""Behavioural tests for cluster.user_visible_composition_count and
cluster.kubectl_auth_can_i (Ship 0.30.234 / #146 scope).

NO source-introspection. NO live cluster. Every test mocks
subprocess.run to return scripted kubectl outputs (SSAR yes/no, jsonpath
ns/name listings). Mechanism-uniform: no hardcoded "cyberjoker" or
"admin" decision branches — only SSAR-driven outcomes.
"""

from __future__ import annotations

import subprocess
from unittest import mock

import pytest


import bench.cluster as cluster_mod
from bench.cluster import (
    kubectl_auth_can_i,
    provision_narrow_rbac_role,
    user_visible_composition_count,
)


# ─── Fixture: scripted kubectl runner ───────────────────────────────────────


class _Scripted:
    """Drive subprocess.run via a reply queue keyed on the kubectl argv.

    Each entry: (predicate, rc, stdout, stderr). The predicate is a
    callable that receives the full argv list and returns True for a
    match. The first match in the queue wins; non-matched entries
    return rc=0, stdout="", stderr="".
    """

    def __init__(self, rules):
        self.rules = list(rules)
        self.calls = []

    def __call__(self, argv, **kwargs):
        self.calls.append(list(argv))
        for predicate, rc, stdout, stderr in self.rules:
            if predicate(argv):
                return _make_proc(rc, stdout, stderr)
        return _make_proc(0, "", "")


def _make_proc(rc, stdout, stderr):
    class _Proc:
        def __init__(self, rc, stdout, stderr):
            self.returncode = rc
            self.stdout = stdout.encode() if isinstance(stdout, str) else stdout
            self.stderr = stderr.encode() if isinstance(stderr, str) else stderr
    return _Proc(rc, stdout, stderr)


def _argv_has(argv, *needles):
    """True iff every needle appears in argv (positional + flag form)."""
    return all(any(n in str(a) for a in argv) for n in needles)


# ─── kubectl_auth_can_i: yes/no/error tri-state ─────────────────────────────


def test_kubectl_auth_can_i_returns_true_on_yes(monkeypatch):
    rules = [(lambda argv: _argv_has(argv, "auth", "can-i"),
              0, "yes\n", "")]
    monkeypatch.setattr(subprocess, "run", _Scripted(rules))
    result = kubectl_auth_can_i(
        "alice", "tok123", verb="list",
        resource="widgets", group="example.io",
        all_namespaces=True,
    )
    assert result is True


def test_kubectl_auth_can_i_returns_false_on_no(monkeypatch):
    rules = [(lambda argv: _argv_has(argv, "auth", "can-i"),
              1, "no\n", "")]
    monkeypatch.setattr(subprocess, "run", _Scripted(rules))
    result = kubectl_auth_can_i(
        "alice", "tok123", verb="list",
        resource="widgets", group="example.io",
        namespace="ns-1",
    )
    assert result is False


def test_kubectl_auth_can_i_returns_none_on_probe_error(monkeypatch):
    """A non-yes/no kubectl output (probe error) MUST yield None so
    the caller can distinguish 'denied' from 'couldn't measure'.
    """
    rules = [(lambda argv: _argv_has(argv, "auth", "can-i"),
              2, "", "Error: API connection refused")]
    monkeypatch.setattr(subprocess, "run", _Scripted(rules))
    result = kubectl_auth_can_i(
        "alice", "tok123", verb="list",
        resource="widgets", group="example.io",
    )
    assert result is None


def test_kubectl_auth_can_i_carries_user_as_impersonation(monkeypatch):
    """SSAR mechanism: --as=<user> must be present in the kubectl argv
    so the SSAR is evaluated against the user's bindings, not the
    kubeconfig default user. The bench kubeconfig has admin (impersonation)
    rights — this is the canonical "what would this user see" probe.
    """
    scripted = _Scripted([
        (lambda argv: _argv_has(argv, "auth", "can-i"), 0, "yes\n", ""),
    ])
    monkeypatch.setattr(subprocess, "run", scripted)
    kubectl_auth_can_i(
        "alice", None, verb="list",
        resource="widgets", group="example.io",
        all_namespaces=True,
    )
    assert any("--as=alice" in str(a) for a in scripted.calls[0]), \
        f"argv missing --as=alice: {scripted.calls[0]}"
    # Conversely, --token= MUST NOT be present (portal JWT is not
    # apiserver-recognised; --as= is the working mechanism).
    assert not any(str(a).startswith("--token=")
                   for a in scripted.calls[0]), \
        f"argv unexpectedly carries --token=: {scripted.calls[0]}"


def test_kubectl_auth_can_i_decodes_jwt_groups_into_as_group(monkeypatch):
    """When the token carries a `groups` claim, every group must be
    threaded as a --as-group=<g> flag so the SSAR sees the SAME
    (user, groups) identity snowplow's UserConfig context carries.

    Without group impersonation the bench would diverge from snowplow
    semantics on group-bound ClusterRoleBindings (e.g. `admins` group
    gets cluster-admin via cluster-admin-binding-krateo-system).
    """
    import base64
    import json
    # Build a minimal unsigned JWT: header.payload.signature
    payload = {"sub": "admin", "groups": ["admins", "platform-users"]}
    payload_b64 = base64.urlsafe_b64encode(
        json.dumps(payload).encode()).rstrip(b"=").decode()
    tok = f"hdr.{payload_b64}.sig"

    scripted = _Scripted([
        (lambda argv: _argv_has(argv, "auth", "can-i"), 0, "yes\n", ""),
    ])
    monkeypatch.setattr(subprocess, "run", scripted)
    kubectl_auth_can_i(
        "admin", tok, verb="list",
        resource="widgets", group="example.io",
        all_namespaces=True,
    )
    argv = scripted.calls[0]
    assert any("--as-group=admins" in str(a) for a in argv), \
        f"argv missing --as-group=admins: {argv}"
    assert any("--as-group=platform-users" in str(a) for a in argv), \
        f"argv missing --as-group=platform-users: {argv}"


def test_kubectl_auth_can_i_handles_token_without_groups_claim(monkeypatch):
    """Token without `groups` (or malformed token) → no --as-group=
    flags. Function still works via --as=<user>.
    """
    scripted = _Scripted([
        (lambda argv: _argv_has(argv, "auth", "can-i"), 0, "no\n", ""),
    ])
    monkeypatch.setattr(subprocess, "run", scripted)
    # Malformed token — no payload to decode.
    result = kubectl_auth_can_i(
        "alice", "not-a-jwt", verb="list",
        resource="widgets", group="example.io",
        all_namespaces=True,
    )
    assert result is False  # SSAR said no; function still returned a value
    argv = scripted.calls[0]
    assert not any(str(a).startswith("--as-group=") for a in argv), \
        f"argv unexpectedly carries --as-group=: {argv}"


# ─── user_visible_composition_count: 4 RBAC patterns ────────────────────────


def test_admin_short_circuits_via_can_i_all_namespaces(monkeypatch):
    """When SSAR --all-namespaces returns yes, function MUST return the
    cluster total without issuing any per-namespace SSAR.
    """
    # 5 ns × varying counts → cluster_total = 1+1+1+1+1 = 5
    jsonpath_out = "\n".join([
        "ns-1/comp-a",
        "ns-2/comp-b",
        "ns-3/comp-c",
        "ns-4/comp-d",
        "ns-5/comp-e",
    ])
    can_i_calls = []

    def _run(argv, **kwargs):
        if _argv_has(argv, "get", "githubscaffolding"):
            return _make_proc(0, jsonpath_out, "")
        if _argv_has(argv, "auth", "can-i"):
            can_i_calls.append(list(argv))
            # all-namespaces probe → yes
            if any("--all-namespaces" in str(a) for a in argv):
                return _make_proc(0, "yes\n", "")
            # per-ns probe MUST NOT be reached on admin path
            return _make_proc(1, "no\n", "")
        return _make_proc(0, "", "")

    monkeypatch.setattr(subprocess, "run", _run)
    result = user_visible_composition_count("admin", "tok-admin")
    assert result == 5
    # Exactly ONE SSAR call (the all-namespaces probe).
    assert len(can_i_calls) == 1
    assert any("--all-namespaces" in str(a) for a in can_i_calls[0])


def test_narrow_user_ssars_each_ns(monkeypatch):
    """Per-namespace SSAR returns mixed permit/deny → count = sum over
    permitted namespaces.
    """
    jsonpath_out = "\n".join([
        "ns-1/comp-a",  # 1 in ns-1
        "ns-2/comp-b",  # 1 in ns-2
        "ns-2/comp-c",  # 1 in ns-2 (so ns-2 has 2)
        "ns-3/comp-d",  # 1 in ns-3
    ])
    # mid-rbac: permits ns-1 + ns-3, denies ns-2.
    permitted = {"ns-1", "ns-3"}

    def _run(argv, **kwargs):
        if _argv_has(argv, "get", "githubscaffolding"):
            return _make_proc(0, jsonpath_out, "")
        if _argv_has(argv, "auth", "can-i"):
            if any("--all-namespaces" in str(a) for a in argv):
                return _make_proc(1, "no\n", "")
            # Per-ns: extract -n <ns> from argv
            ns_val = None
            for i, a in enumerate(argv):
                if a == "-n" and i + 1 < len(argv):
                    ns_val = argv[i + 1]
                    break
            if ns_val in permitted:
                return _make_proc(0, "yes\n", "")
            return _make_proc(1, "no\n", "")
        return _make_proc(0, "", "")

    monkeypatch.setattr(subprocess, "run", _run)
    # ns-1: 1 + ns-3: 1 = 2 visible compositions.
    assert user_visible_composition_count("alice", "tok-alice") == 2


def test_cyberjoker_zero_when_no_bindings(monkeypatch):
    """Empty per-ns permit set → returns 0.

    Models the cluster state the architect traced on 2026-06-02:
    cyberjoker has no ClusterRoleBindings AND no RoleBindings, so every
    SSAR returns 'no'. Expected = 0 — IS the correct value for that
    cluster state.
    """
    jsonpath_out = "\n".join([
        "ns-1/comp-a",
        "ns-2/comp-b",
    ])

    def _run(argv, **kwargs):
        if _argv_has(argv, "get", "githubscaffolding"):
            return _make_proc(0, jsonpath_out, "")
        if _argv_has(argv, "auth", "can-i"):
            return _make_proc(1, "no\n", "")
        return _make_proc(0, "", "")

    monkeypatch.setattr(subprocess, "run", _run)
    assert user_visible_composition_count("cyberjoker", "tok-cj") == 0


def test_mid_rbac_user_partial_visibility(monkeypatch):
    """Half-permit pattern (every other ns) → returns correct partial."""
    jsonpath_out = "\n".join([
        "ns-1/comp-a",
        "ns-2/comp-b",
        "ns-3/comp-c",
        "ns-4/comp-d",
        "ns-5/comp-e",
        "ns-6/comp-f",
    ])
    permitted = {"ns-1", "ns-3", "ns-5"}

    def _run(argv, **kwargs):
        if _argv_has(argv, "get", "githubscaffolding"):
            return _make_proc(0, jsonpath_out, "")
        if _argv_has(argv, "auth", "can-i"):
            if any("--all-namespaces" in str(a) for a in argv):
                return _make_proc(1, "no\n", "")
            ns_val = None
            for i, a in enumerate(argv):
                if a == "-n" and i + 1 < len(argv):
                    ns_val = argv[i + 1]
                    break
            if ns_val in permitted:
                return _make_proc(0, "yes\n", "")
            return _make_proc(1, "no\n", "")
        return _make_proc(0, "", "")

    monkeypatch.setattr(subprocess, "run", _run)
    # 3 permitted ns × 1 composition each = 3.
    assert user_visible_composition_count("midrbac", "tok-mid") == 3


# ─── Customer-shape: cj sees 1 in granted ns (Diego #146 follow-up) ─────────


def test_cyberjoker_sees_one_in_granted_ns(monkeypatch):
    """When cyberjoker has a Role on bench-ns-01 (customer narrow-RBAC
    shape), the SSAR for that one ns returns yes; ns-1 contains exactly
    one composition → count = 1.

    This is the proper falsifier per Diego clarification 2026-06-02:
    NOT cj==0 (zero-RBAC degenerate), but cj==1 (one Role granting
    visibility in one ns where one composition lives).
    """
    # 20 compositions across bench-ns-01..bench-ns-20 (1 each).
    lines = []
    for i in range(1, 21):
        lines.append(f"bench-ns-{i:02d}/comp-{i:02d}")
    jsonpath_out = "\n".join(lines)
    # cj's Role grants list on bench-ns-01 ONLY.
    permitted = {"bench-ns-01"}

    def _run(argv, **kwargs):
        if _argv_has(argv, "get", "githubscaffolding"):
            return _make_proc(0, jsonpath_out, "")
        if _argv_has(argv, "auth", "can-i"):
            if any("--all-namespaces" in str(a) for a in argv):
                return _make_proc(1, "no\n", "")
            ns_val = None
            for i, a in enumerate(argv):
                if a == "-n" and i + 1 < len(argv):
                    ns_val = argv[i + 1]
                    break
            if ns_val in permitted:
                return _make_proc(0, "yes\n", "")
            return _make_proc(1, "no\n", "")
        return _make_proc(0, "", "")

    monkeypatch.setattr(subprocess, "run", _run)
    assert user_visible_composition_count("cyberjoker", "tok-cj") == 1


# ─── Error propagation ─────────────────────────────────────────────────────


def test_returns_minus_one_on_jsonpath_probe_failure(monkeypatch):
    """When kubectl get -o jsonpath fails (cluster unreachable etc.),
    function returns -1 so the caller can distinguish 'measure failed'
    from 'legitimately zero'.
    """
    def _run(argv, **kwargs):
        if _argv_has(argv, "get", "githubscaffolding"):
            return _make_proc(1, "", "connection refused")
        return _make_proc(0, "", "")

    monkeypatch.setattr(subprocess, "run", _run)
    assert user_visible_composition_count("alice", "tok") == -1


# ─── provision_narrow_rbac_role: shape + mechanism-uniform ──────────────────


def test_provision_narrow_rbac_role_applies_parameterized_subject(monkeypatch):
    """Apply contract: kubectl apply -f - with a Role+RoleBinding body
    that carries the supplied user as subject. No hardcoded user name
    in the function — caller controls.
    """
    captured = {}

    def _run(argv, **kwargs):
        if _argv_has(argv, "apply", "-f", "-"):
            captured["input"] = kwargs.get("input", b"").decode() if isinstance(
                kwargs.get("input"), bytes) else kwargs.get("input", "")
            return _make_proc(0, "", "")
        return _make_proc(0, "", "")

    monkeypatch.setattr(subprocess, "run", _run)
    ok = provision_narrow_rbac_role("eve", "bench-ns-01")
    assert ok is True
    body = captured.get("input", "")
    assert "kind: Role" in body
    assert "kind: RoleBinding" in body
    assert "name: eve" in body  # subject is parameterized
    assert "namespace: bench-ns-01" in body
    # Mechanism-uniform: default role name = <user>-<resource>-reader
    # Wildcard "*" normalizes to "all" so kubectl names stay shell-clean.
    assert "eve-all-reader" in body  # default resources=("*",)


def test_provision_narrow_rbac_role_returns_false_on_apply_failure(monkeypatch):
    def _run(argv, **kwargs):
        if _argv_has(argv, "apply", "-f", "-"):
            return _make_proc(1, "", "apply failed")
        return _make_proc(0, "", "")

    monkeypatch.setattr(subprocess, "run", _run)
    assert provision_narrow_rbac_role("eve", "bench-ns-01") is False
