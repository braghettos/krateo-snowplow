"""Tests for the Task #314 S8b/S8c CR-definition-modification reconcile stages.

Hermetic — no cluster. The HTTP seam (`phases._s8bc_call_body`) and the
kubectl/k8s seams (`phases.cluster.*`, `phases.browser.read_snowplow_expvar_int`)
are stubbed exactly as the design directs ("stub the http + kubectl seams like
`_phase8_poll_via_snowplow` tests do"). These stages poll via urllib (no Chrome),
so no FakePage is needed.

Coverage (per the dispatch brief):
  - stage registration / order (S8b/S8c in STAGE_ORDER + the window logic incl.
    `--from-stage S8b --to-stage S8c`).
  - content predicate + negative-control logic (fake /call bodies: marker flips
    at poll k → conv_ms computed; control changed → FAIL).
  - timeout → -1 + stage FAIL.
  - revert/teardown called on BOTH the success and failure paths.
  - ledger by-key read of conv_widget_mod_ms / conv_ra_mod_ms (report-only).
  - fixture-YAML echo-field shape (the resolver-confirmed echo fields).
"""

from __future__ import annotations

import json
import threading

import pytest

from bench import phases, ledger
from bench.phases import (
    STAGE_REGISTRY,
    STAGE_ORDER,
    StageContext,
    _stages_in_window,
)


# ─── A null per-stage log streamer (avoids real kubectl in _run_stage) ───────


class _NullStreamer:
    def __init__(self, *a, **k):
        self._running = threading.Event()

    def start(self):
        self._running.set()

    def stop(self):
        self._running.clear()


# ─── A scripted /call body seam ──────────────────────────────────────────────


class _ScriptedCall:
    """Stand-in for phases._s8bc_call_body.

    `script[(name)]` is a list of bodies returned on successive GETs of that
    fixture name; the LAST element repeats once exhausted. A None element
    simulates a transport failure for that GET.
    """

    def __init__(self, script: dict[str, list]):
        self.script = {k: list(v) for k, v in script.items()}
        self.calls: list[tuple] = []

    def __call__(self, gvr, name, ns, token):
        self.calls.append((gvr["plural"], name, ns, token))
        seq = self.script.get(name, [None])
        if len(seq) > 1:
            return seq.pop(0)
        return seq[0] if seq else None


def _stub_cluster(monkeypatch, *, apply_ok=True, patch_ok=True):
    """All cluster mutation/teardown helpers succeed (record their calls)."""
    log: dict[str, list] = {
        "apply": [], "patch": [], "delete": []}

    def _apply(yaml_str):
        log["apply"].append(yaml_str)
        return (apply_ok, "" if apply_ok else "apply boom")

    def _patch(group, version, plural, ns, name, spec_patch):
        log["patch"].append((plural, name, spec_patch))
        return (patch_ok, "" if patch_ok else "patch boom")

    def _delete(group, version, plural, ns, name):
        log["delete"].append((plural, name))
        return True

    monkeypatch.setattr(phases, "_PerStageLogStreamer", _NullStreamer)
    monkeypatch.setattr(phases.cluster, "k8s_apply_yaml", _apply)
    monkeypatch.setattr(phases.cluster, "k8s_patch_cr_spec", _patch)
    monkeypatch.setattr(phases.cluster, "k8s_delete_custom", _delete)
    return log


def _ctx(tmp_path, *, token="tok-admin"):
    return StageContext(
        tag="0.30.314", scale=5000, run_dir=tmp_path,
        state_path=tmp_path / "state.json",
        tokens=({"admin": token} if token else {}),
        user_pages={},
    )


# ─── Registration / order / window ──────────────────────────────────────────


def test_s8b_s8c_registered_in_registry_and_order():
    assert "S8b" in STAGE_REGISTRY and "S8c" in STAGE_REGISTRY
    assert STAGE_REGISTRY["S8b"] is phases.stage_s8b_widget_mod_reconcile
    assert STAGE_REGISTRY["S8c"] is phases.stage_s8c_ra_mod_reconcile
    # Placement: after S9, before S10.
    assert STAGE_ORDER.index("S9") < STAGE_ORDER.index("S8b")
    assert STAGE_ORDER.index("S8b") + 1 == STAGE_ORDER.index("S8c")
    assert STAGE_ORDER.index("S8c") < STAGE_ORDER.index("S10")


def test_window_from_s8b_to_s8c_is_exactly_the_two_substages():
    window = _stages_in_window("S8b", "S8c")
    assert window == ["S8b", "S8c"]


def test_window_from_s9_to_s10_includes_the_substages_inline():
    window = _stages_in_window("S9", "S10")
    assert window == ["S9", "S8b", "S8c", "S10"]


def test_full_window_keeps_s8bc_after_s9_before_s10():
    window = _stages_in_window(None, "S11")
    assert window[window.index("S9") + 1:window.index("S10")] == ["S8b", "S8c"]


# ─── Fixture-YAML echo-field shape (resolver-confirmed) ─────────────────────


def test_widget_fixture_yaml_uses_widgetdata_label_and_no_apiref():
    y = phases._widget_fixture_yaml("v1", "ctrl")
    # Both fixtures present, correct GVR + kind.
    assert "apiVersion: widgets.templates.krateo.io/v1beta1" in y
    assert y.count("kind: Button") == 2
    assert "name: bench-widget-mod-probe" in y
    assert "name: bench-widget-mod-control" in y
    # Echo field is spec.widgetData.label; probe="v1", control="ctrl".
    assert 'label: "v1"' in y
    assert 'label: "ctrl"' in y
    # NOT RBAC-sensitive: no apiRef / widgetDataTemplate that would route it
    # away from the per-cohort `widgets` cell.
    assert "apiRef" not in y
    assert "widgetDataTemplate" not in y


def test_ra_fixture_yaml_uses_top_level_filter_jq_literal_and_no_api():
    y = phases._ra_fixture_yaml("v1", "ctrl")
    assert "apiVersion: templates.krateo.io/v1" in y
    assert y.count("kind: RESTAction") == 2
    assert "name: bench-ra-mod-probe" in y
    assert "name: bench-ra-mod-control" in y
    # spec.filter jq literal — probe emits {"probe":"v1"}, control "ctrl".
    assert 'filter: \'{ probe: "v1" }\'' in y
    assert 'filter: \'{ probe: "ctrl" }\'' in y
    # No api stage (would conflate with the new-GVR rePrewarm path).
    assert "api:" not in y


def test_call_url_encodes_group_slash_version():
    url = phases._call_url_for(
        phases._WIDGET_FIXTURE_GVR, "bench-widget-mod-probe", "bench-ns-01")
    assert url == ("/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1"
                   "&resource=buttons&name=bench-widget-mod-probe"
                   "&namespace=bench-ns-01")


# ─── Content predicate: marker flips at poll k → conv_ms computed ───────────


def test_widget_stage_converges_when_marker_flips(tmp_path, monkeypatch):
    """probe body returns "v1" for the first 2 polls then "v2" — the stage
    must compute a non-negative conv_widget_mod_ms, the control stays "ctrl",
    Probe-A refresher delta is positive → S8b PASSES.
    """
    _stub_cluster(monkeypatch)
    scripted = _ScriptedCall({
        # PRIME(probe)="v1", then 2 polls show old "v1", then "v2".
        "bench-widget-mod-probe": [
            '{"status":{"widgetData":{"label":"v1"}}}',   # PRIME
            '{"status":{"widgetData":{"label":"v1"}}}',   # poll 1 (stale)
            '{"status":{"widgetData":{"label":"v1"}}}',   # poll 2 (stale)
            '{"status":{"widgetData":{"label":"v2"}}}',   # poll 3 — converged
        ],
        # PRIME(control)="ctrl"; negative-control GET still "ctrl".
        "bench-widget-mod-control": ['{"status":{"widgetData":{"label":"ctrl"}}}'],
    })
    monkeypatch.setattr(phases, "_s8bc_call_body", scripted)
    # Probe A: refresher_completed_total increments across the window.
    seq = iter([100, 103])
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_int",
                        lambda key: next(seq))
    # Don't actually sleep between polls.
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    proof = phases.stage_s8b_widget_mod_reconcile(_ctx(tmp_path))
    body = proof.proof

    assert body["conv_widget_mod_ms"] >= 0
    assert body["control_unchanged"] is True
    assert body["refresher_completed_delta"] == 3
    assert body["refresher_probe_ok"] is True
    assert proof.passed is True
    # The metric is stamped under the resolver-confirmed key.
    assert "conv_widget_mod_ms" in body
    # Proof persisted on disk under proofs/S8b.json.
    on_disk = json.loads((tmp_path / "proofs" / "S8b.json").read_text())
    assert on_disk["proof"]["conv_widget_mod_ms"] >= 0


def test_ra_stage_converges_when_probe_field_flips(tmp_path, monkeypatch):
    _stub_cluster(monkeypatch)
    scripted = _ScriptedCall({
        "bench-ra-mod-probe": [
            '{"probe":"v1"}',   # PRIME
            '{"probe":"v1"}',   # poll 1
            '{"probe":"v2"}',   # poll 2 — converged
        ],
        "bench-ra-mod-control": ['{"probe":"ctrl"}'],
    })
    monkeypatch.setattr(phases, "_s8bc_call_body", scripted)
    seq = iter([7, 9])
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_int",
                        lambda key: next(seq))
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    proof = phases.stage_s8c_ra_mod_reconcile(_ctx(tmp_path))
    body = proof.proof

    assert body["conv_ra_mod_ms"] >= 0
    assert body["control_unchanged"] is True
    assert body["refresher_completed_delta"] == 2
    assert proof.passed is True


# ─── Negative control: control flipped → FAIL ───────────────────────────────


def test_stage_fails_when_negative_control_also_flipped(tmp_path, monkeypatch):
    """If the control body ALSO shows "v2" at convergence, the measurement is
    a global flush, not a targeted dirty-mark → control_unchanged=False → FAIL
    even though the probe converged.
    """
    _stub_cluster(monkeypatch)
    scripted = _ScriptedCall({
        "bench-widget-mod-probe": [
            '{"status":{"widgetData":{"label":"v1"}}}',   # PRIME
            '{"status":{"widgetData":{"label":"v2"}}}',   # poll 1 — converged
        ],
        # Control body shows the NEW value too — a global flush.
        "bench-widget-mod-control": [
            '{"status":{"widgetData":{"label":"ctrl"}}}',  # PRIME ("ctrl")
            '{"status":{"widgetData":{"label":"v2"}}}',    # neg-control GET
        ],
    })
    monkeypatch.setattr(phases, "_s8bc_call_body", scripted)
    seq = iter([10, 12])
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_int",
                        lambda key: next(seq))
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    proof = phases.stage_s8b_widget_mod_reconcile(_ctx(tmp_path))
    body = proof.proof

    assert body["conv_widget_mod_ms"] >= 0  # probe DID converge
    assert body["control_unchanged"] is False
    assert proof.passed is False  # but the control flip fails the stage


# ─── Timeout → -1 + stage FAIL ──────────────────────────────────────────────


def test_stage_fails_with_minus_one_on_poll_timeout(tmp_path, monkeypatch):
    """The probe body NEVER shows "v2" → poll times out → conv = -1 → FAIL."""
    _stub_cluster(monkeypatch)
    # Shrink the budget so the test does not spin for 60 wall-clock seconds.
    monkeypatch.setattr(phases, "_S8BC_POLL_BUDGET_S", 2)
    monkeypatch.setattr(phases, "_S8BC_POLL_INTERVAL_S", 0.0)
    scripted = _ScriptedCall({
        # Always stale "v1" — never converges.
        "bench-widget-mod-probe": ['{"status":{"widgetData":{"label":"v1"}}}'],
        "bench-widget-mod-control": ['{"status":{"widgetData":{"label":"ctrl"}}}'],
    })
    monkeypatch.setattr(phases, "_s8bc_call_body", scripted)
    seq = iter([1, 1])  # refresher did NOT move either
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_int",
                        lambda key: next(seq))
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    proof = phases.stage_s8b_widget_mod_reconcile(_ctx(tmp_path))
    body = proof.proof

    assert body["conv_widget_mod_ms"] == -1
    assert proof.passed is False
    assert body["poll_samples"] >= 1


# ─── Revert + teardown on BOTH success and failure paths ────────────────────


def test_teardown_runs_on_success_path(tmp_path, monkeypatch):
    log = _stub_cluster(monkeypatch)
    scripted = _ScriptedCall({
        "bench-widget-mod-probe": [
            '{"status":{"widgetData":{"label":"v1"}}}',   # PRIME
            '{"status":{"widgetData":{"label":"v2"}}}',   # converged
        ],
        "bench-widget-mod-control": ['{"status":{"widgetData":{"label":"ctrl"}}}'],
    })
    monkeypatch.setattr(phases, "_s8bc_call_body", scripted)
    seq = iter([5, 6])
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_int",
                        lambda key: next(seq))
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    proof = phases.stage_s8b_widget_mod_reconcile(_ctx(tmp_path))

    assert proof.passed is True
    # Revert patch back to v1 happened.
    assert ("buttons", "bench-widget-mod-probe", {"widgetData": {"label": "v1"}}) \
        in log["patch"]
    # Both fixtures deleted.
    deleted = set(log["delete"])
    assert ("buttons", "bench-widget-mod-probe") in deleted
    assert ("buttons", "bench-widget-mod-control") in deleted
    # Teardown diag stamped on the proof.
    td = proof.proof["teardown"]
    assert td["revert_ok"] is True
    assert td["probe_deleted"] is True and td["control_deleted"] is True


def test_teardown_runs_on_failure_path_apply_failed(tmp_path, monkeypatch):
    """When fixture apply FAILS, the stage exits early — but the `finally`
    teardown still fires (idempotent: revert + delete attempted)."""
    log = _stub_cluster(monkeypatch, apply_ok=False)
    # The HTTP seam must never be needed on this path, but stub it defensively.
    monkeypatch.setattr(phases, "_s8bc_call_body",
                        _ScriptedCall({}))
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_int",
                        lambda key: None)
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    proof = phases.stage_s8c_ra_mod_reconcile(_ctx(tmp_path))

    assert proof.passed is False
    assert proof.proof["error"] == "fixture_apply_failed"
    assert proof.proof["conv_ra_mod_ms"] == -1
    # Teardown still ran (delete attempted on both fixtures).
    deleted = set(log["delete"])
    assert ("restactions", "bench-ra-mod-probe") in deleted
    assert ("restactions", "bench-ra-mod-control") in deleted
    assert "teardown" in proof.proof


def test_teardown_runs_on_failure_path_work_raises(tmp_path, monkeypatch):
    """If the stage body raises mid-flight (e.g. the expvar reader blows up),
    the `finally` teardown still reverts + deletes before _run_stage records
    the error proof.
    """
    log = _stub_cluster(monkeypatch)
    scripted = _ScriptedCall({
        "bench-widget-mod-probe": ['{"status":{"widgetData":{"label":"v1"}}}'],
        "bench-widget-mod-control": ['{"status":{"widgetData":{"label":"ctrl"}}}'],
    })
    monkeypatch.setattr(phases, "_s8bc_call_body", scripted)

    def _boom(key):
        raise RuntimeError("expvar transport exploded")

    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_int", _boom)
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    with pytest.raises(RuntimeError):
        phases.stage_s8b_widget_mod_reconcile(_ctx(tmp_path))

    # Even though _work raised, the finally-block teardown deleted the fixtures.
    deleted = set(log["delete"])
    assert ("buttons", "bench-widget-mod-probe") in deleted
    assert ("buttons", "bench-widget-mod-control") in deleted


# ─── Precheck: missing admin token → FAIL, no fixtures touched ──────────────


def test_stage_fails_when_no_admin_token(tmp_path, monkeypatch):
    log = _stub_cluster(monkeypatch)
    monkeypatch.setattr(phases, "_s8bc_call_body", _ScriptedCall({}))
    monkeypatch.setattr(phases.browser, "read_snowplow_expvar_int",
                        lambda key: None)
    monkeypatch.setattr(phases.time, "sleep", lambda s: None)

    proof = phases.stage_s8b_widget_mod_reconcile(_ctx(tmp_path, token=None))
    body = proof.proof

    assert body["error"] == "no_admin_token"
    assert body["conv_widget_mod_ms"] == -1
    assert proof.passed is False
    # No fixtures created (precheck short-circuits before apply/teardown).
    assert log["apply"] == []
    assert log["delete"] == []


# ─── Ledger by-key read (report-only) ───────────────────────────────────────


def test_ledger_reads_conv_mod_metrics_by_key_from_proofs(tmp_path):
    """build_canonical_ledger_row surfaces conv_widget_mod_ms / conv_ra_mod_ms
    by-key from the S8b/S8c proofs on disk — REPORT-ONLY (not in verdict).
    """
    proofs = tmp_path / "proofs"
    proofs.mkdir(parents=True)
    (proofs / "S8b.json").write_text(json.dumps(
        {"stage_id": "S8b", "proof": {"conv_widget_mod_ms": 2345}}))
    (proofs / "S8c.json").write_text(json.dumps(
        {"stage_id": "S8c", "proof": {"conv_ra_mod_ms": 1789}}))

    assert ledger._load_stage_proof_metric(
        "S8b", "conv_widget_mod_ms", run_dir=tmp_path) == 2345
    assert ledger._load_stage_proof_metric(
        "S8c", "conv_ra_mod_ms", run_dir=tmp_path) == 1789

    row = ledger.build_canonical_ledger_row([], run_dir=tmp_path, tag="t")
    assert row["conv_widget_mod_ms"] == 2345
    assert row["conv_ra_mod_ms"] == 1789


def test_ledger_conv_mod_metrics_null_when_proofs_absent(tmp_path):
    """No S8b/S8c proofs (pre-#314 run or window excluded them) → null, never
    a crash. Confirms forward/backward compatibility."""
    row = ledger.build_canonical_ledger_row([], run_dir=tmp_path, tag="t")
    assert row["conv_widget_mod_ms"] is None
    assert row["conv_ra_mod_ms"] is None


def test_ledger_conv_mod_metrics_do_not_affect_verdict(tmp_path):
    """A timed-out reconcile (-1) is surfaced but MUST NOT flip the verdict —
    report-only this ship (the #121 pattern). compute_verdict ignores them.
    """
    proofs = tmp_path / "proofs"
    proofs.mkdir(parents=True)
    (proofs / "S8b.json").write_text(json.dumps(
        {"stage_id": "S8b", "proof": {"conv_widget_mod_ms": -1}}))
    row = ledger.build_canonical_ledger_row([], run_dir=tmp_path, tag="t")
    # The -1 is reported …
    assert row["conv_widget_mod_ms"] == -1
    # … but the verdict is driven only by mix_weighted/restarts/conv_s8_p99
    # (compute_verdict's signature takes none of the conv-mod metrics). With
    # empty all_results the existing logic yields INVALID (mix_weighted null),
    # NOT a conv-mod-driven verdict — proving the -1 has zero gate impact.
    assert row["verdict"] == "INVALID"
    # Sanity: compute_verdict itself never sees the conv-mod values.
    assert "conv_widget_mod_ms" not in ledger.compute_verdict.__code__.co_varnames
