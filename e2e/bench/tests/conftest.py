"""Shared pytest fixtures for the bench harness (Block 1 scope).

Adds `e2e/bench/` to sys.path so `import bench.cluster` resolves without
requiring an editable install. Sets BENCH_ALLOW_NON_GKE=1 before any
test imports bench.cluster so the module-level gke_context_guard does
NOT exit during test collection.

Risk Register R1.1 mitigation: `reset_k8s_state` fixture (opt-in) nulls
the cluster.py module-level globals so tests don't share state.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

import pytest

# ─── Test isolation: env vars that MUST be set before bench.* imports ────────

# Bypass the GKE context guard — tests should never contact a real cluster.
os.environ.setdefault("BENCH_ALLOW_NON_GKE", "1")

# Add e2e/bench/ to sys.path so `import bench.cluster` works without install.
_E2E_BENCH_DIR = Path(__file__).resolve().parent.parent
if str(_E2E_BENCH_DIR) not in sys.path:
    sys.path.insert(0, str(_E2E_BENCH_DIR))


@pytest.fixture
def reset_k8s_state():
    """Reset bench.cluster k8s-client module globals between tests.

    Use this fixture in any test that monkey-patches `_k8s_init`,
    flips `K8S_CLIENT_AVAILABLE`, or sets the API client globals. Without
    it, mutations leak between cases.

    Risk Register §I R1.1 mitigation.
    """
    import bench.cluster as cluster_mod
    snapshot = {
        "K8S_CLIENT_AVAILABLE": cluster_mod.K8S_CLIENT_AVAILABLE,
        "_k8s_core": cluster_mod._k8s_core,
        "_k8s_rbac": cluster_mod._k8s_rbac,
        "_k8s_custom": cluster_mod._k8s_custom,
        "_k8s_apiext": cluster_mod._k8s_apiext,
        "_k8s_init_attempted": cluster_mod._k8s_init_attempted,
    }
    yield cluster_mod
    cluster_mod.K8S_CLIENT_AVAILABLE = snapshot["K8S_CLIENT_AVAILABLE"]
    cluster_mod._k8s_core = snapshot["_k8s_core"]
    cluster_mod._k8s_rbac = snapshot["_k8s_rbac"]
    cluster_mod._k8s_custom = snapshot["_k8s_custom"]
    cluster_mod._k8s_apiext = snapshot["_k8s_apiext"]
    cluster_mod._k8s_init_attempted = snapshot["_k8s_init_attempted"]


@pytest.fixture
def mock_kubectl(monkeypatch):
    """Install a recording subprocess.run; yield the call log.

    Each entry in the log is a dict {argv, input, rc, stdout, stderr}.
    By default every call returns rc=0 with empty stdout/stderr; the test
    can pre-load `log.replies` (a list of (rc, stdout, stderr) tuples) to
    return scripted outputs.
    """
    import subprocess as subprocess_mod

    class _Log:
        def __init__(self):
            self.calls: list[dict] = []
            self.replies: list[tuple[int, str, str]] = []

    log = _Log()

    def _fake_run(argv, **kwargs):
        rc, stdout, stderr = (0, "", "")
        if log.replies:
            rc, stdout, stderr = log.replies.pop(0)
        log.calls.append({
            "argv": list(argv),
            "input": kwargs.get("input"),
            "rc": rc,
            "stdout": stdout,
            "stderr": stderr,
        })

        class _FakeCompletedProcess:
            def __init__(self, rc, stdout, stderr):
                self.returncode = rc
                # subprocess.run with capture_output=True returns bytes
                # unless text=True; bench.cluster.kubectl uses bytes.
                if kwargs.get("text"):
                    self.stdout = stdout
                    self.stderr = stderr
                else:
                    self.stdout = stdout.encode() if isinstance(stdout, str) else stdout
                    self.stderr = stderr.encode() if isinstance(stderr, str) else stderr

        return _FakeCompletedProcess(rc, stdout, stderr)

    monkeypatch.setattr(subprocess_mod, "run", _fake_run)
    yield log


@pytest.fixture
def tmp_run_dir(tmp_path):
    """Yield a clean /tmp/snowplow-runs/{tag}/run-{ts}/-shaped path."""
    run_dir = tmp_path / "snowplow-runs" / "test-tag" / "run-20260602-test"
    run_dir.mkdir(parents=True, exist_ok=True)
    return run_dir
