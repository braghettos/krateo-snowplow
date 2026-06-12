"""Unit tests for bench verify-serve-stale v1.1 (Task #159).

Covers the three confirmed defects + the v1.1 warm-up scope:

  DEFECT 1 — API DRIFT: the probe attaches X-Krateo-TraceId via
             browser.http_get(..., headers=...) (the param added #159);
             the pre-#159 `trace_id=` kwarg did not exist.
  DEFECT 2 — MISTARGETED MARKER: the default target is `widget-echo`
             (a bench Button whose spec.widgetData.label echoes into the
             /call body). The two-marker model (setup_marker present in
             pre/mid, mutate_marker present only in post) is exercised
             across PASS + every FAIL/INDETERMINATE branch.
  v1.1     — WARM-UP PROBES: N unscored /calls fire before the scored
             vars_before snapshot; accounting is recorded on the bundle.

No real cluster: every I/O boundary is injected via the run_verify_serve_stale
`_*_fn` stub hooks (the established falsifier idiom) or monkeypatched on the
browser module. conftest autouse guards apply.
"""

from __future__ import annotations

import hashlib
import time
import urllib.request

import pytest

import bench.browser as browser_mod
import bench.verify_serve_stale as vss


# ─── shared stub builders ────────────────────────────────────────────────────


def _sha(body: bytes) -> str:
    return hashlib.sha256(body).hexdigest()


def _vars_with_cell(hit=100, miss=5, *, cache_on=True):
    """A /debug/vars snapshot publishing the widget-echo cell."""
    if not cache_on:
        return {"some_other_metric": 1}
    return {"snowplow_dispatch_l1_lookups": {
        vss._WIDGET_CELL_KEY: {"hit_total": hit, "miss_total": miss}}}


class _Harness:
    """Builds a coherent set of stub hooks for run_verify_serve_stale.

    Bodies are driven by a mutable `state` dict so a single probe_fn can
    return the OLD (setup) body before MUTATE and the NEW (mutate) body
    after — exactly the two-marker contract. Tweak the per-scenario knobs
    then call .run().
    """

    def __init__(self):
        self.state = {"mutated": False}
        # knobs the verdict branches flip:
        self.code = 200
        self.miss_total_after = 5    # == before(5) → miss_delta 0
        self.hit_total_after = 130
        self.ack = "ACKED"
        self.log_hit = True          # SECONDARY hit:true on every probe
        self.log_present = True      # SECONDARY log lines present
        self.setup_raises = False
        self.patch_rc = 0
        self.warmup_calls = []
        self.cleanup_called = []
        self.captured_markers = {}
        self._snap_calls = 0
        # When set, mid returns this body verbatim (scenario override).
        self.force_mid_body = None
        # When True, the post probe returns the SETUP body (no refresh).
        self.post_unchanged = False

    # -- hooks ----------------------------------------------------------------
    def _setup_body(self) -> bytes:
        # The served body literally embeds the widget label (the marker),
        # mirroring the live echo mechanism.
        m = self.captured_markers.get("setup", "vss-v1-unset")
        return f'{{"widgetData":{{"label":"{m}"}}}}'.encode()

    def _mutate_body(self) -> bytes:
        m = self.captured_markers.get("mutate", "vss-v2-unset")
        return f'{{"widgetData":{{"label":"{m}"}}}}'.encode()

    def probe_fn(self, path, token, tid):
        if "warmup" in tid:
            self.warmup_calls.append(tid)
            body = b"WARMUP-BODY"
            code = 200
        elif tid.endswith("-mid") and self.force_mid_body is not None:
            body = self.force_mid_body
            code = self.code
        elif tid.endswith("-post"):
            if self.post_unchanged or not self.state["mutated"]:
                body = self._setup_body()
            else:
                body = self._mutate_body()
            code = self.code
        else:  # pre / mid (steady state — OLD label)
            body = self._setup_body()
            code = self.code
        return {"trace_id": tid, "http_ms": 5, "code": code,
                "body_sha256": _sha(body), "body_bytes": body,
                "marker_present": False}

    def snapshot_fn(self):
        # Calls: 0=preflight, 1=scored-baseline(before), 2=vars_after.
        # miss_delta = after.miss - before.miss. before.miss is always 5;
        # after.miss = miss_total_after, so the cold-fill knob is honoured.
        self._snap_calls += 1
        if self._snap_calls <= 2:
            return _vars_with_cell(hit=100, miss=5)
        return _vars_with_cell(hit=self.hit_total_after,
                               miss=self.miss_total_after)

    def setup_fn(self, marker):
        if self.setup_raises:
            raise vss._VerifyError("setup boom")
        self.captured_markers["setup"] = marker
        return "bench-ns-01", "bench-vss-widget-probe"

    def mutate_fn(self, ns, name, marker):
        self.captured_markers["mutate"] = marker
        if self.patch_rc == 0:
            self.state["mutated"] = True
        return self.patch_rc, time.monotonic(), ("" if self.patch_rc == 0
                                                  else "patch boom")

    def cleanup_fn(self, ns, name):
        self.cleanup_called.append((ns, name))
        return True

    def ack_fn(self, ns, name, marker, t):
        return self.ack

    def grep_fn(self, tids):
        if not self.log_present:
            return {t: [] for t in tids}
        return {t: [{"hit": self.log_hit, "key_hash": "k",
                     "resident_bytes": 9}] for t in tids}

    # -- driver ---------------------------------------------------------------
    def run(self, **overrides):
        kw = dict(
            user="cyberjoker", target="widget-echo", tag="0.30.261",
            warmup=2,
            _probe_fn=self.probe_fn, _snapshot_fn=self.snapshot_fn,
            _grep_fn=self.grep_fn, _setup_fn=self.setup_fn,
            _patch_fn=self.mutate_fn, _cleanup_fn=self.cleanup_fn,
            _ack_fn=self.ack_fn, _sleep_fn=lambda s: None,
            log=lambda m: None, section=lambda m: None,
        )
        kw.update(overrides)
        return vss.run_verify_serve_stale(**kw)


@pytest.fixture(autouse=True)
def _stub_login(monkeypatch):
    """run_verify_serve_stale calls browser.login_all() — never hit authn."""
    monkeypatch.setattr(browser_mod, "login_all",
                        lambda: {"cyberjoker": "fake-jwt", "admin": "fake"})


# ─── DEFECT 1: http_get headers= attaches X-Krateo-TraceId ────────────────────


def test_http_get_attaches_custom_headers(monkeypatch):
    """browser.http_get(..., headers={...}) merges onto the request — the
    #159 fix for the dropped trace_id. The pre-fix `trace_id=` kwarg would
    have raised TypeError."""
    seen = {}

    class _Resp:
        status = 200
        headers = {"Content-Type": "application/json"}

        def read(self):
            return b"{}"

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=120):
        # urllib lowercases header keys in .headers; capture the dict.
        seen["headers"] = dict(req.headers)
        return _Resp()

    monkeypatch.setattr(urllib.request, "urlopen", fake_urlopen)

    ms, code, body = browser_mod.http_get(
        "/call?x", "tok", retries=1,
        headers={"X-Krateo-TraceId": "bench-vss-abc-pre"})
    assert code == 200
    # urllib capitalises to "X-krateo-traceid"; match case-insensitively.
    lowered = {k.lower(): val for k, val in seen["headers"].items()}
    assert lowered.get("x-krateo-traceid") == "bench-vss-abc-pre"
    # The always-present pair is still there.
    assert lowered.get("authorization") == "Bearer tok"
    assert lowered.get("accept-encoding") == "gzip"


def test_http_get_without_headers_unchanged(monkeypatch):
    """Omitting headers= leaves existing callers byte-for-byte unaffected
    (only Authorization + Accept-Encoding present)."""
    seen = {}

    class _Resp:
        status = 200
        headers = {}

        def read(self):
            return b"ok"

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    monkeypatch.setattr(urllib.request, "urlopen",
                        lambda req, timeout=120: (seen.update(
                            headers=dict(req.headers)) or _Resp()))
    browser_mod.http_get("/p", "t", retries=1)
    lowered = {k.lower() for k in seen["headers"]}
    assert lowered == {"authorization", "accept-encoding"}


def test_probe_passes_trace_id_as_header(monkeypatch):
    """_probe must call http_get with headers={'X-Krateo-TraceId': tid} —
    NOT a trace_id= kwarg (the drifted call)."""
    captured = {}

    def fake_http_get(path, token, timeout=120, retries=3, headers=None):
        captured["headers"] = headers
        captured["no_trace_id_kwarg"] = True
        return 7, 200, b"body"

    monkeypatch.setattr(browser_mod, "http_get", fake_http_get)
    out = vss._probe("/call?x", "tok", "bench-vss-xyz-mid")
    assert captured["headers"] == {"X-Krateo-TraceId": "bench-vss-xyz-mid"}
    assert out["code"] == 200
    assert out["trace_id"] == "bench-vss-xyz-mid"
    assert out["body_sha256"] == _sha(b"body")


# ─── DEFECT 2: target registry + widget fixture shape ─────────────────────────


def test_default_target_is_widget_echo():
    assert vss._TARGET_REGISTRY["widget-echo"]["path"] == vss._WIDGET_PROBE_PATH
    # the broken compositions-list target is gone (not parked behind a flag)
    assert "compositions-list" not in vss._TARGET_REGISTRY


def test_default_user_is_admin():
    """#159 C2: standalone default is admin (cyberjoker needs a phase6
    rolebinding into bench-ns-01). cyberjoker stays selectable."""
    import inspect
    sig = inspect.signature(vss.run_verify_serve_stale)
    assert sig.parameters["user"].default == "admin"


def test_widget_cell_key_and_gvr_string_shape():
    """The expvar cell key + ACK gvr_string must carry the ', Resource='
    prefix (apimachinery GVR.String()) or every live run misses the cell /
    forces INDETERMINATE_INFORMER_NOT_ACKED."""
    assert vss._WIDGET_CELL_KEY == (
        "widgets|widgets.templates.krateo.io/v1beta1, Resource=buttons")
    assert vss._WIDGET_GVR_STRING == (
        "widgets.templates.krateo.io/v1beta1, Resource=buttons")
    assert vss._TARGET_REGISTRY["widget-echo"]["gvr_string"] == \
        vss._WIDGET_GVR_STRING


def test_widget_call_path_url_encoded():
    p = vss._widget_call_path("my-btn", "my-ns")
    assert p == ("/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1"
                 "&resource=buttons&name=my-btn&namespace=my-ns")


def test_widget_fixture_yaml_embeds_label_as_marker():
    y = vss._widget_fixture_yaml("vss-v1-deadbeef")
    assert 'label: "vss-v1-deadbeef"' in y
    assert "kind: Button" in y
    assert "namespace: bench-ns-01" in y
    # plain widget — no apiRef / widgetDataTemplate (routes to widgets cell)
    assert "apiRef" not in y
    assert "widgetDataTemplate" not in y


def test_setup_widget_target_apply_failure_raises(monkeypatch):
    monkeypatch.setattr(vss.cluster, "k8s_apply_yaml",
                        lambda y: (False, "apply denied"))
    with pytest.raises(vss._VerifyError):
        vss._setup_widget_target("vss-v1-x")


def test_mutate_widget_label_maps_ok_to_rc0(monkeypatch):
    seen = {}

    def fake_patch(group, version, plural, ns, name, spec_patch):
        seen.update(spec_patch=spec_patch, plural=plural)
        return True, ""

    monkeypatch.setattr(vss.cluster, "k8s_patch_cr_spec", fake_patch)
    rc, t, err = vss._mutate_widget_label("bench-ns-01", "b", "vss-v2-x")
    assert rc == 0 and err == ""
    assert seen["spec_patch"] == {"widgetData": {"label": "vss-v2-x"}}
    assert seen["plural"] == "buttons"


def test_mutate_widget_label_maps_fail_to_rc1(monkeypatch):
    monkeypatch.setattr(vss.cluster, "k8s_patch_cr_spec",
                        lambda *a, **k: (False, "patch boom"))
    rc, t, err = vss._mutate_widget_label("ns", "b", "m")
    assert rc == 1 and err == "patch boom"


# ─── v1.1: warm-up probe accounting ──────────────────────────────────────────


def test_warmup_probes_fire_count_and_count_200():
    calls = []

    def probe_fn(path, token, tid):
        calls.append(tid)
        # one warm-up 404s — must NOT abort, just not count as ok
        code = 200 if "warmup-0" in tid else 404
        return {"code": code}

    ok = vss._warmup_probes("/p", "tok", 3, probe_fn, log=lambda m: None)
    assert len(calls) == 3
    assert all("warmup" in c for c in calls)
    assert ok == 1  # only warmup-0 returned 200


def test_warmup_zero_disabled():
    calls = []
    ok = vss._warmup_probes("/p", "tok", 0,
                            lambda *a: calls.append(1) or {"code": 200},
                            log=lambda m: None)
    assert calls == [] and ok == 0


def test_warmup_runs_before_scored_baseline_snapshot():
    """The warm-ups must fire BEFORE vars_before is snapshotted, so their
    cold-fill misses are excluded from the scored counter window. We assert
    ordering by recording the sequence of snapshot vs warm-up events."""
    h = _Harness()
    events = []

    real_snapshot = h.snapshot_fn

    def snap():
        events.append("snapshot")
        return real_snapshot()

    real_probe = h.probe_fn

    def probe(path, token, tid):
        if "warmup" in tid:
            events.append("warmup")
        return real_probe(path, token, tid)

    code, bundle = h.run(_snapshot_fn=snap, _probe_fn=probe, warmup=2)
    # sequence: preflight-snapshot, warmup, warmup, scored-baseline-snapshot, ...
    assert events[0] == "snapshot"           # preflight reachability
    assert events[1] == "warmup"
    assert events[2] == "warmup"
    assert events[3] == "snapshot"           # scored baseline AFTER warm-ups
    assert bundle["warmup"] == {"requested": 2, "http_200": 2}


# ─── verdict matrix: PASS + the two-marker contract ───────────────────────────


def test_pass_two_marker_contract():
    """The clean serve-stale path: mid == pre snapshot (no marker), post
    fresh. PASS with mid_observation == 'served_stale' (#159 C1)."""
    h = _Harness()
    code, bundle = h.run()
    assert bundle["verdict"] == "PASS"
    assert code == 0
    assert bundle["mid_observation"] == "served_stale"
    assert bundle["stale_served"] is True
    assert bundle["refresh_completed"] is True
    # pre/mid carry setup_marker (mutate_marker ABSENT) → marker_present False
    assert bundle["probes"][0]["marker_present"] is False
    assert bundle["probes"][1]["marker_present"] is False
    # post carries mutate_marker → present
    assert bundle["probes"][2]["marker_present"] is True
    # the two markers are distinct and stamped
    assert bundle["mutation"]["setup_marker"].startswith("vss-v1-")
    assert bundle["mutation"]["mutate_marker"].startswith("vss-v2-")
    assert (bundle["mutation"]["setup_marker"][6:]
            == bundle["mutation"]["mutate_marker"][6:])  # same stamp suffix
    # marker actually scanned is the mutate_marker (the NEW label)
    assert h.captured_markers["mutate"] == bundle["mutation"]["mutate_marker"]
    assert h.captured_markers["setup"] == bundle["mutation"]["setup_marker"]
    # cleanup fired
    assert h.cleanup_called == [("bench-ns-01", "bench-vss-widget-probe")]


def test_indeterminate_mid_body_unexpected():
    """#159 C1: a mid body that CHANGED but carries NEITHER the pre snapshot
    NOR the new marker is a genuine 'something else mutated the entry'
    anomaly → INDETERMINATE_MID_BODY_UNEXPECTED (NOT a serve-stale FAIL).
    Post is still fresh (bounded-freshness passes), so the mid-classifier
    is reached."""
    h = _Harness()
    h.force_mid_body = b'{"widgetData":{"label":"SOMETHING-ELSE"}}'
    code, bundle = h.run()
    assert bundle["verdict"] == "INDETERMINATE_MID_BODY_UNEXPECTED"
    assert code == 1
    assert bundle["mid_observation"] is None


def test_pass_served_fresh_early_at_mid():
    """#159 C1 — the EXACT idle-cluster smoke case: the refresh beat the
    probe RTT, so mid already carries the mutate_marker. miss_delta==0
    (no cold miss) + post fresh ⇒ PASS, mid_observation='served_fresh_early'
    (strictly-better than stale, NOT a FAIL). This is the verdict the live
    0.30.261 smoke must now produce."""
    h = _Harness()

    def probe(path, token, tid):
        if "warmup" in tid:
            return h.probe_fn(path, token, tid)
        if tid.endswith("-pre"):
            # pre = the OLD snapshot (no marker).
            body = h._setup_body()
        else:
            # mid AND post = the MUTATE body (marker present) — refresh
            # landed before the mid probe reached the dispatcher.
            body = h._mutate_body()
        return {"trace_id": tid, "http_ms": 5, "code": 200,
                "body_sha256": _sha(body), "body_bytes": body,
                "marker_present": False}

    code, bundle = h.run(_probe_fn=probe)
    assert bundle["verdict"] == "PASS"
    assert code == 0
    assert bundle["mid_observation"] == "served_fresh_early"
    # mid carries the marker; post fresh; no cold miss.
    assert bundle["probes"][1]["marker_present"] is True
    assert bundle["probes"][2]["marker_present"] is True
    assert bundle["l1_lookups"]["miss_delta"] == 0


def test_fail_refresh_not_completed_when_post_unchanged():
    """If post body == pre body (no refresh), FAIL."""
    h = _Harness()
    h.post_unchanged = True
    code, bundle = h.run()
    assert bundle["verdict"] == "FAIL_REFRESH_NOT_COMPLETED_BY_10S"
    assert code == 2


def test_fail_sync_cold_fill_when_miss_delta_nonzero():
    h = _Harness()
    h.miss_total_after = 9  # before miss=5 → miss_delta=4
    code, bundle = h.run()
    assert bundle["verdict"] == "FAIL_SYNC_COLD_FILL_ON_MID"
    assert code == 2
    assert bundle["l1_lookups"]["miss_delta"] == 4


def test_fail_http_non_200():
    h = _Harness()
    h.code = 503
    code, bundle = h.run()
    assert bundle["verdict"].startswith("FAIL_HTTP_503_ON_")
    assert code == 2


# ─── verdict matrix: INDETERMINATE branches ───────────────────────────────────


def test_indeterminate_cache_off():
    h = _Harness()
    code, bundle = h.run(_snapshot_fn=lambda: _vars_with_cell(cache_on=False))
    assert bundle["verdict"] == "INDETERMINATE_CACHE_OFF"
    assert code == 1


def test_indeterminate_setup_failed():
    h = _Harness()
    h.setup_raises = True
    code, bundle = h.run()
    assert bundle["verdict"] == "INDETERMINATE_SETUP_FAILED"
    assert code == 1


def test_indeterminate_informer_not_acked():
    h = _Harness()
    h.ack = "NOT_ACKED"
    code, bundle = h.run()
    assert bundle["verdict"] == "INDETERMINATE_INFORMER_NOT_ACKED"
    assert code == 1


def test_indeterminate_patch_failed_runs_cleanup():
    h = _Harness()
    h.patch_rc = 1
    code, bundle = h.run()
    assert bundle["verdict"] == "INDETERMINATE_PATCH_FAILED"
    assert code == 1
    # fixture must still be cleaned up on the patch-failure path
    assert h.cleanup_called == [("bench-ns-01", "bench-vss-widget-probe")]


def test_indeterminate_login_failed(monkeypatch):
    monkeypatch.setattr(browser_mod, "login_all", lambda: {})
    h = _Harness()
    code, bundle = h.run()
    assert bundle["verdict"] == "INDETERMINATE_LOGIN_FAILED"
    assert code == 1


def test_indeterminate_log_filter_unavailable():
    h = _Harness()
    h.log_present = False
    code, bundle = h.run()
    assert bundle["verdict"] == "INDETERMINATE_LOG_FILTER_UNAVAILABLE"
    assert code == 1


def test_fail_sources_disagree():
    """PRIMARY says no miss but SECONDARY log line has hit:false."""
    h = _Harness()
    h.log_hit = False
    code, bundle = h.run()
    assert bundle["verdict"] == "FAIL_SOURCES_DISAGREE"
    assert code == 2


def test_unknown_target_raises():
    with pytest.raises(vss._VerifyError):
        vss.run_verify_serve_stale(target="no-such-target",
                                   _sleep_fn=lambda s: None,
                                   log=lambda m: None, section=lambda m: None)
