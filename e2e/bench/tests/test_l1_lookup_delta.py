"""Task #217 tests — per-stage L1 hit/miss delta (REPORT-ONLY telemetry).

_run_stage snapshots `snowplow_dispatch_l1_lookups` (map[handlerKind|gvr] ->
{hit_total, miss_total}, per dispatchers/l1_lookup_metrics.go) before/after a
stage and diffs it into the stage proof under `l1_lookup_delta`. This is
additive observability that NEVER feeds compute_verdict.

These tests assert, mirroring the established conv_discrimination idiom
(test_conv_discrimination.py / test_browser.py):

  1. read_snowplow_expvar_map — transport happy/None-safe paths (urlopen mock,
     same seam as read_snowplow_expvar_int's unit tests).
  2. compute_l1_lookup_delta — pure-function diff + full None-safety.
  3. _run_stage — the delta is persisted into the proof + state.json.
  4. compute_verdict source is untouched (grep-proof) — the hard gate.

Mocking seam: the autouse conftest `_stub_conv_discrimination_probes` nulls
`browser.read_snowplow_expvar_map` for base_url-less callers (the _run_stage
path), so a hermetic stage records l1_lookup_delta=None — the same path a
cache-off pod takes. Transport tests below pass an explicit base_url to drive
the REAL function through a mocked urlopen (the conftest delegates in that
case), exactly as the read_snowplow_expvar_int unit tests do.
"""

from __future__ import annotations

import json
import threading
import urllib.request

import bench.browser as browser
import bench.ledger as ledger
import bench.phases as phases


# ─── 1. read_snowplow_expvar_map — transport ───────────────────────────────


class _FakeResp:
    def __init__(self, body: bytes, status: int = 200):
        self._body = body
        self.status = status
        self.headers = {}

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def read(self):
        return self._body


def test_read_expvar_map_returns_dict_on_200(monkeypatch):
    captured = []
    body = json.dumps({
        "snowplow_dispatch_l1_lookups": {
            "widgets|widgets.krateo.io/v1, Resource=buttons":
                {"hit_total": 12, "miss_total": 3},
        },
        "other": 7,
    }).encode()

    def _fake_urlopen(req, timeout=None):
        captured.append(req.full_url)
        return _FakeResp(body)

    monkeypatch.setattr(browser.urllib.request, "urlopen", _fake_urlopen)
    out = browser.read_snowplow_expvar_map(
        "snowplow_dispatch_l1_lookups", base_url="http://fake:8081")
    assert out == {
        "widgets|widgets.krateo.io/v1, Resource=buttons":
            {"hit_total": 12, "miss_total": 3},
    }
    assert captured == ["http://fake:8081/debug/vars"]


def test_read_expvar_map_none_on_transport_failure(monkeypatch):
    def _fake_urlopen(req, timeout=None):
        raise OSError("connection refused")

    monkeypatch.setattr(browser.urllib.request, "urlopen", _fake_urlopen)
    assert browser.read_snowplow_expvar_map(
        "snowplow_dispatch_l1_lookups", base_url="http://fake:8081") is None


def test_read_expvar_map_none_on_missing_key(monkeypatch):
    body = json.dumps({"other_key": {"a": 1}}).encode()
    monkeypatch.setattr(browser.urllib.request, "urlopen",
                        lambda req, timeout=None: _FakeResp(body))
    assert browser.read_snowplow_expvar_map(
        "snowplow_dispatch_l1_lookups", base_url="http://fake:8081") is None


def test_read_expvar_map_none_on_non_dict_value(monkeypatch):
    # The key exists but is a scalar (wrong shape) → None, not a crash.
    body = json.dumps({"snowplow_dispatch_l1_lookups": 5}).encode()
    monkeypatch.setattr(browser.urllib.request, "urlopen",
                        lambda req, timeout=None: _FakeResp(body))
    assert browser.read_snowplow_expvar_map(
        "snowplow_dispatch_l1_lookups", base_url="http://fake:8081") is None


def test_read_expvar_map_none_on_non_200(monkeypatch):
    monkeypatch.setattr(browser.urllib.request, "urlopen",
                        lambda req, timeout=None: _FakeResp(b"{}", status=503))
    assert browser.read_snowplow_expvar_map(
        "snowplow_dispatch_l1_lookups", base_url="http://fake:8081") is None


# ─── 2. compute_l1_lookup_delta — pure diff + None-safety ───────────────────


def test_compute_delta_diffs_per_gvr_and_totals():
    before = {
        "widgets|g/v1, Resource=buttons": {"hit_total": 10, "miss_total": 2},
        "restactions|g/v1, Resource=ra":  {"hit_total": 5, "miss_total": 5},
    }
    after = {
        "widgets|g/v1, Resource=buttons": {"hit_total": 18, "miss_total": 3},
        "restactions|g/v1, Resource=ra":  {"hit_total": 5, "miss_total": 5},
    }
    d = browser.compute_l1_lookup_delta(before, after)
    # widgets moved +8 hits / +1 miss; restactions unchanged → omitted.
    assert d["per_gvr"] == {
        "widgets|g/v1, Resource=buttons": {"hits": 8, "misses": 1},
    }
    assert d["total"] == {"hits": 8, "misses": 1}


def test_compute_delta_counts_cell_new_in_after_as_full_value():
    before = {}
    after = {"widgets|g/v1, Resource=b": {"hit_total": 4, "miss_total": 1}}
    d = browser.compute_l1_lookup_delta(before, after)
    assert d["per_gvr"]["widgets|g/v1, Resource=b"] == {"hits": 4, "misses": 1}
    assert d["total"] == {"hits": 4, "misses": 1}


def test_compute_delta_zero_when_no_movement():
    same = {"widgets|g/v1, Resource=b": {"hit_total": 9, "miss_total": 9}}
    d = browser.compute_l1_lookup_delta(same, dict(same))
    assert d["per_gvr"] == {}
    assert d["total"] == {"hits": 0, "misses": 0}


def test_compute_delta_none_when_either_snapshot_none():
    # cache-off / unreachable on either end → None (caller records null).
    assert browser.compute_l1_lookup_delta(None, {}) is None
    assert browser.compute_l1_lookup_delta({}, None) is None
    assert browser.compute_l1_lookup_delta(None, None) is None


def test_compute_delta_tolerates_missing_and_garbage_subfields():
    # A cell missing a counter, or carrying a non-int, must not crash; the
    # missing/garbage counter is treated as 0.
    before = {"widgets|g/v1, Resource=b": {"hit_total": 3}}  # no miss_total
    after = {"widgets|g/v1, Resource=b": {"hit_total": 7, "miss_total": "x"}}
    d = browser.compute_l1_lookup_delta(before, after)
    assert d["per_gvr"]["widgets|g/v1, Resource=b"] == {"hits": 4, "misses": 0}
    assert d["total"] == {"hits": 4, "misses": 0}


# ─── 3. _run_stage persistence ──────────────────────────────────────────────


class _NullStreamer:
    def __init__(self, *a, **k):
        self._running = threading.Event()

    def start(self):
        self._running.set()

    def stop(self):
        self._running.clear()


def _ctx(tmp_path):
    return phases.StageContext(
        tag="0.30.x", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        tokens={"admin": "tok"}, user_pages={},
    )


def test_run_stage_persists_l1_lookup_delta_into_proof(tmp_path, monkeypatch):
    """The success-path proof carries `l1_lookup_delta`, computed from the
    before/after L1 snapshots, and it is persisted into state.json."""
    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)

    # Drive the REAL map reader through a scripted before/after pair. The
    # _run_stage call passes no base_url, so we override the module attribute
    # directly (overriding the autouse None-wrapper) to return the snapshots
    # in call order — the same explicit-rebind idiom test_phases_s8bc uses.
    snaps = iter([
        {"widgets|g/v1, Resource=b": {"hit_total": 1, "miss_total": 4}},   # before
        {"widgets|g/v1, Resource=b": {"hit_total": 9, "miss_total": 5}},   # after
    ])
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_map",
                        lambda key, **k: next(snaps))

    proof = phases._run_stage("SX", _ctx(tmp_path), lambda c: {"x": 1},
                              what_breaks_if_skipped="t")
    assert proof.passed is True
    assert proof.proof["l1_lookup_delta"] == {
        "per_gvr": {"widgets|g/v1, Resource=b": {"hits": 8, "misses": 1}},
        "total": {"hits": 8, "misses": 1},
    }

    state = json.loads((tmp_path / "state.json").read_text())
    assert (state["stage_proofs"]["SX"]["proof"]["l1_lookup_delta"]["total"]
            == {"hits": 8, "misses": 1})


def test_run_stage_l1_delta_is_none_when_expvars_unreachable(tmp_path,
                                                             monkeypatch):
    """cache-OFF / unreachable: the autouse conftest nulls the base_url-less
    map reader → l1_lookup_delta is None, and the stage still PASSES (never
    crashes). This is the canonical cache-off path."""
    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)
    # NOTE: no read_snowplow_expvar_map override here — the autouse
    # _stub_conv_discrimination_probes fixture returns None for the
    # base_url-less _run_stage caller.
    proof = phases._run_stage("SX", _ctx(tmp_path), lambda c: {"x": 1},
                              what_breaks_if_skipped="t")
    assert proof.passed is True
    assert proof.proof["l1_lookup_delta"] is None


def test_run_stage_l1_delta_none_when_only_one_snapshot_reachable(
        tmp_path, monkeypatch):
    """One snapshot present, the other None (pod restarted mid-stage / scrape
    blip) → compute_l1_lookup_delta returns None; the stage still passes."""
    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)
    snaps = iter([
        {"widgets|g/v1, Resource=b": {"hit_total": 1, "miss_total": 0}},  # before
        None,                                                              # after
    ])
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_map",
                        lambda key, **k: next(snaps))
    proof = phases._run_stage("SX", _ctx(tmp_path), lambda c: {"x": 1},
                              what_breaks_if_skipped="t")
    assert proof.passed is True
    assert proof.proof["l1_lookup_delta"] is None


# ─── 4. VERDICT-UNTOUCHED PROOF (the hard gate) ─────────────────────────────


def test_l1_lookup_delta_does_not_feed_compute_verdict():
    """compute_verdict's source must contain NO reference to the L1 delta
    field — the report-only telemetry is structurally invisible to scoring.
    Mirrors test_conv_discrimination's verdict-untouched proof."""
    import inspect
    src = inspect.getsource(ledger.compute_verdict)
    assert "l1_lookup" not in src
    assert "l1_lookups" not in src
    # And compute_verdict_with_falsifier (the wrapper) likewise.
    src_wrap = inspect.getsource(ledger.compute_verdict_with_falsifier)
    assert "l1_lookup" not in src_wrap


def test_compute_verdict_signature_unchanged_by_217():
    """The L1 delta lands ONLY in stage proofs; compute_verdict's parameters
    are unchanged (no new gating input added)."""
    import inspect
    params = list(inspect.signature(ledger.compute_verdict).parameters)
    # Canonical params as of #334-B (mix_weighted, restarts, conv_s8_p99,
    # cells, ...). #217 adds NONE of its own.
    assert "mix_weighted" in params
    assert "restarts" in params
    assert "conv_s8_p99" in params
    assert not any("l1" in p for p in params)
