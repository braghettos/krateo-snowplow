"""Tests for bench.probe_determinism — meta-falsifier per dispatch
2026-06-02 (#161 disambiguation harness).

Every I/O surface is stubbed (HTTP probe, kubectl logs grep, login).
ZERO cluster contact. Each case asserts the harness emits the named
verdict + correct exit code.

Verdict matrix coverage:
    DETERMINISTIC      — all hit + body_sha equal (exit 0)
    NON_DETERMINISTIC  — all hit + body_sha differs (exit 2)
    MIXED              — at least one miss (exit 3)
    INDETERMINATE_*    — log gap / HTTP error (exit 1)
"""

from __future__ import annotations

import hashlib

import pytest

from bench import probe_determinism as pd


# ─── Helpers ────────────────────────────────────────────────────────────────


def _make_probe(body: bytes, code: int = 200, http_ms: int = 50,
                trace_id: str = ""):
    return {
        "trace_id": trace_id,
        "http_ms": http_ms,
        "code": code,
        "body_sha256": hashlib.sha256(body).hexdigest(),
        "body_len": len(body),
    }


def _probe_seq_constant(body_bytes: bytes):
    """Return a probe_fn that always emits the SAME body (deterministic)."""
    def fn(target_path, token, trace_id, timeout=30):
        out = _make_probe(body_bytes, trace_id=trace_id)
        return out
    return fn


def _probe_seq_per_call_stamp():
    """Return a probe_fn whose body changes on every call (non-deterministic)."""
    counter = {"n": 0}

    def fn(target_path, token, trace_id, timeout=30):
        counter["n"] += 1
        body = f"body-{counter['n']}".encode()
        return _make_probe(body, trace_id=trace_id)
    return fn


def _grep_all_hit(trace_ids, since="60s", tail=4000, retries=3):
    return {tid: [{"hit": True,
                   "key_hash": "k-same",
                   "resident_bytes": 100}]
            for tid in trace_ids}


def _grep_first_miss_rest_hit(trace_ids, since="60s", tail=4000, retries=3):
    out = {}
    for i, tid in enumerate(trace_ids):
        out[tid] = [{"hit": (i != 0),
                     "key_hash": "k-same",
                     "resident_bytes": 100 if i != 0 else 0}]
    return out


def _grep_empty(trace_ids, since="60s", tail=4000, retries=3):
    return {tid: [] for tid in trace_ids}


def _no_sleep(s):
    return None


def _login_ok(user="cyberjoker"):
    def fn():
        return {user: "fake-jwt"}
    return fn


# ─── Test 1: deterministic — all hit + body equal → exit 0 ──────────────────


def test_deterministic_when_all_hit_and_body_stable():
    body = b"stable-body"
    exit_code, bundle = pd.run_probe_determinism(
        user="cyberjoker",
        target_path="/call?x=1",
        probes_n=5,
        tag="0.30.235",
        run_dir=None,
        _probe_fn=_probe_seq_constant(body),
        _grep_fn=_grep_all_hit,
        _login_fn=_login_ok(),
        _sleep_fn=_no_sleep,
        log=lambda *a, **k: None,
        section=lambda *a, **k: None,
    )
    assert bundle["verdict"] == "DETERMINISTIC"
    assert exit_code == 0
    assert bundle["all_hits"] is True
    assert len(bundle["unique_body_sha256"]) == 1


# ─── Test 2: non-deterministic — all hit + body differs → exit 2 ───────────


def test_non_deterministic_when_body_differs_under_all_hit():
    exit_code, bundle = pd.run_probe_determinism(
        user="cyberjoker",
        target_path="/call?x=1",
        probes_n=5,
        tag="0.30.235",
        run_dir=None,
        _probe_fn=_probe_seq_per_call_stamp(),
        _grep_fn=_grep_all_hit,
        _login_fn=_login_ok(),
        _sleep_fn=_no_sleep,
        log=lambda *a, **k: None,
        section=lambda *a, **k: None,
    )
    assert bundle["verdict"] == "NON_DETERMINISTIC"
    assert exit_code == 2
    assert bundle["all_hits"] is True
    assert len(bundle["unique_body_sha256"]) == 5


# ─── Test 3: mixed — at least one miss → exit 3 ─────────────────────────────


def test_mixed_when_any_probe_missed():
    body = b"stable-body"
    exit_code, bundle = pd.run_probe_determinism(
        user="cyberjoker",
        target_path="/call?x=1",
        probes_n=3,
        tag="0.30.235",
        run_dir=None,
        _probe_fn=_probe_seq_constant(body),
        _grep_fn=_grep_first_miss_rest_hit,
        _login_fn=_login_ok(),
        _sleep_fn=_no_sleep,
        log=lambda *a, **k: None,
        section=lambda *a, **k: None,
    )
    assert bundle["verdict"] == "MIXED"
    assert exit_code == 3
    assert bundle["all_hits"] is False


# ─── Test 4: pod logs empty → INDETERMINATE_LOG_FILTER_UNAVAILABLE ──────────


def test_indeterminate_when_logs_unavailable():
    body = b"stable-body"
    exit_code, bundle = pd.run_probe_determinism(
        user="cyberjoker",
        target_path="/call?x=1",
        probes_n=3,
        tag="0.30.235",
        run_dir=None,
        _probe_fn=_probe_seq_constant(body),
        _grep_fn=_grep_empty,
        _login_fn=_login_ok(),
        _sleep_fn=_no_sleep,
        log=lambda *a, **k: None,
        section=lambda *a, **k: None,
    )
    assert bundle["verdict"] == "INDETERMINATE_LOG_FILTER_UNAVAILABLE"
    assert exit_code == 1


# ─── Test 5: HTTP error on a single probe → INDETERMINATE_HTTP_* ────────────


def test_indeterminate_when_http_error_on_any_probe():
    counter = {"n": 0}

    def probe_fn(target_path, token, trace_id, timeout=30):
        counter["n"] += 1
        if counter["n"] == 2:
            return {
                "trace_id": trace_id,
                "http_ms": 5000,
                "code": 503,
                "body_sha256": hashlib.sha256(b"").hexdigest(),
                "body_len": 0,
            }
        return _make_probe(b"stable", trace_id=trace_id)

    exit_code, bundle = pd.run_probe_determinism(
        user="cyberjoker",
        target_path="/call?x=1",
        probes_n=3,
        tag="0.30.235",
        run_dir=None,
        _probe_fn=probe_fn,
        _grep_fn=_grep_all_hit,
        _login_fn=_login_ok(),
        _sleep_fn=_no_sleep,
        log=lambda *a, **k: None,
        section=lambda *a, **k: None,
    )
    assert bundle["verdict"].startswith("INDETERMINATE_HTTP_503_ON_PROBE_")
    assert exit_code == 1


# ─── Test 6: input validation — probes_n must be >= 2 ──────────────────────


def test_rejects_probes_n_below_2():
    with pytest.raises(pd._ProbeError):
        pd.run_probe_determinism(
            user="cyberjoker",
            target_path="/call?x=1",
            probes_n=1,
            _probe_fn=_probe_seq_constant(b"x"),
            _grep_fn=_grep_all_hit,
            _login_fn=_login_ok(),
            _sleep_fn=_no_sleep,
            log=lambda *a, **k: None,
            section=lambda *a, **k: None,
        )


# ─── Test 7: input validation — path must start with '/' ───────────────────


def test_rejects_path_without_leading_slash():
    with pytest.raises(pd._ProbeError):
        pd.run_probe_determinism(
            user="cyberjoker",
            target_path="call?x=1",   # missing leading slash
            probes_n=3,
            _probe_fn=_probe_seq_constant(b"x"),
            _grep_fn=_grep_all_hit,
            _login_fn=_login_ok(),
            _sleep_fn=_no_sleep,
            log=lambda *a, **k: None,
            section=lambda *a, **k: None,
        )
