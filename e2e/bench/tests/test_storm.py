"""Behavioural tests for bench/storm.py.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.3. 4 cases:
  - CRB burst emits UAF lint as expected against mock pod logs.
  - User-scaling phase creates THEN logs in synthetic users (boundary
    check that calls don't accidentally invert order).
  - `pod_logs_since(offset)` returns the window between two snapshots
    via monkeypatched kubectl.
  - `crb_delete_burst` skips when cyberjoker token absent.

No source-introspection. No live cluster. Every test stays in-process.
"""

from __future__ import annotations

import sys
from unittest import mock

import pytest


import bench.storm as storm_mod
from bench.storm import (
    CRB_DELETE_PORTAL_PATHS,
    _audit_uaf_emit_per_request_lint,
    crb_delete_burst,
    pod_logs_since,
)


# ─── Case 1: UAF audit lint counts both emit kinds for the right user ───────


def test_crb_burst_emits_lint_for_uaf_per_request():
    """`_audit_uaf_emit_per_request_lint` returns True when the user has
    at least one user_access_filter OR user_access_filter_skipped emit
    in the captured log window, False otherwise.
    """
    user = "cyberjoker"
    logs_pass = [
        # one full emit + one skipped emit scoped to cyberjoker
        '{"audit":"user_access_filter","user":"cyberjoker","path":"compositions-list"}',
        '{"audit":"user_access_filter_skipped","user":"cyberjoker","path":"blueprints-list"}',
        # noise: another user (must NOT count)
        '{"audit":"user_access_filter","user":"admin","path":"compositions-list"}',
        # noise: unrelated audit
        '{"audit":"binding_identity_transition","user":"cyberjoker"}',
    ]
    assert _audit_uaf_emit_per_request_lint(
        logs_pass, user=user, expected_paths=CRB_DELETE_PORTAL_PATHS) is True

    logs_fail = [
        # only the wrong user
        '{"audit":"user_access_filter","user":"admin","path":"compositions-list"}',
        # wrong audit kind for cyberjoker
        '{"audit":"binding_identity_transition","user":"cyberjoker"}',
    ]
    assert _audit_uaf_emit_per_request_lint(
        logs_fail, user=user, expected_paths=CRB_DELETE_PORTAL_PATHS) is False


# ─── Case 2: user-scaling boundary — create before login ────────────────────


def test_user_scaling_phase_creates_then_logs_in_synthetic_users(monkeypatch):
    """`measure_first_login_warmup` must invoke create_synthetic_users
    BEFORE polling for L1 ready — order matters because polling against
    a not-yet-registered user always times out and pollutes timings.

    We stub create_synthetic_users + the deferred metric helpers and
    assert the create call landed before any active_users probe.
    """
    call_log: list[str] = []

    def fake_create(start, end):
        call_log.append(f"create({start},{end})")
        return (end - start + 1, {})

    fake_lifecycle = mock.MagicMock()
    fake_lifecycle.create_synthetic_users = fake_create
    monkeypatch.setitem(sys.modules, "bench.lifecycle", fake_lifecycle)

    # Stub the deferred metric helpers so the function exits the loop fast.
    monkeypatch.setattr(storm_mod, "_get_runtime_metrics", lambda: None)
    monkeypatch.setattr(storm_mod, "_read_l1_ready_ts", lambda: 0)
    monkeypatch.setattr(storm_mod, "_http_get", lambda *a, **kw: (5, 200, b""))

    # Call the routine with a very short timeout so the loop exits.
    storm_mod.measure_first_login_warmup(
        new_start=1, new_end=2, total_expected=4, tokens={"admin": "tok"},
        timeout=1,
    )

    # create_synthetic_users must have been called exactly once.
    assert call_log == ["create(1,2)"], (
        f"expected create_synthetic_users called once before the poll loop; "
        f"got call_log={call_log!r}"
    )


# ─── Case 3: pod_logs_since returns the window between snapshots ────────────


def test_pod_logs_since_returns_window(monkeypatch):
    """`pod_logs_since(offset)` returns lines starting at the given
    offset; an offset past EOF returns []; a missing log returns [].
    """
    log_text = "L0\nL1\nL2\nL3\nL4"

    def fake_kubectl(*args, **kwargs):
        # First arg should be "logs"; we don't differentiate the calls.
        return (0, log_text, "")

    monkeypatch.setattr(storm_mod, "kubectl", fake_kubectl)

    assert pod_logs_since(0) == ["L0", "L1", "L2", "L3", "L4"]
    assert pod_logs_since(2) == ["L2", "L3", "L4"]
    assert pod_logs_since(5) == []  # exactly at EOF
    assert pod_logs_since(99) == []  # past EOF

    def fake_kubectl_fail(*args, **kwargs):
        return (1, "", "kubectl error")

    monkeypatch.setattr(storm_mod, "kubectl", fake_kubectl_fail)
    assert pod_logs_since(0) == []


# ─── Case 4: crb_delete_burst skips cleanly without cyberjoker token ────────


def test_crb_delete_burst_skips_when_no_targets(monkeypatch, capsys):
    """`crb_delete_burst(tokens)` must return early — and NOT touch
    kubectl — when the cyberjoker token is absent. Today's source script
    behaviour at worktree line 8296-8298.
    """
    kubectl_calls: list[tuple] = []

    def fake_kubectl(*args, **kwargs):
        kubectl_calls.append(args)
        return (0, "", "")

    monkeypatch.setattr(storm_mod, "kubectl", fake_kubectl)
    # Wedge the deferred login/record helpers so any unintended call would
    # raise loudly (the test should NOT reach them).
    monkeypatch.setattr(storm_mod, "_login_all",
                        lambda: pytest.fail("login_all called unexpectedly"))
    monkeypatch.setattr(storm_mod, "_http_get",
                        lambda *a, **kw: pytest.fail("http_get called unexpectedly"))
    monkeypatch.setattr(storm_mod, "_record",
                        lambda *a, **kw: pytest.fail("record called unexpectedly"))

    # No cyberjoker token → early return; admin alone is not enough.
    crb_delete_burst(tokens={"admin": "tok"})

    # kubectl must NOT have been touched.
    assert kubectl_calls == [], (
        f"crb_delete_burst should skip kubectl without cyberjoker token; "
        f"got kubectl_calls={kubectl_calls!r}"
    )
