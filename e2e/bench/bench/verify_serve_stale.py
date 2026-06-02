"""bench verify-serve-stale — explicit serve-stale-while-refresh contract test.

Implements the design at
docs/bench-verify-serve-stale-design-2026-06-02.md (cache-architect)
under PM AUTHORIZE-with-tightening 2026-06-02 (Diego's
feedback_l1_hit_miss_via_metrics_not_timing +
feedback_mutation_serves_stale_while_refresh).

The harness fires THREE probes around a single `kubectl patch` annotation
mutation on a randomly chosen composition:

    T-1s   probe   (steady-state, pre-mutation)
    MUTATE         (kubectl patch — adds an annotation marker)
    T+50ms probe   (refresh-in-flight: cache MUST serve STALE)
    T+5s   probe   (post-refresh: cache MUST carry the new marker)

Two independent sources confirm L1 hit/miss verdict per probe:

    PRIMARY   — /debug/vars snowplow_dispatch_l1_lookups counters
                (miss_delta == 0 across all 3 probes is the load-bearing
                assertion).
    SECONDARY — kubectl logs deployment/snowplow filtered on the bench's
                client-supplied X-Krateo-TraceId — verifies each probe's
                `resolved_cache.lookup` log emits `hit:true`.

If PRIMARY and SECONDARY disagree → SOURCES_DISAGREE (snowplow defect).

Plus PM tightening (Option A): the harness verifies the informer
dirty-marked the patched object's L1 entry BEFORE the T+50ms probe fires
— a streaming kubectl logs tail scans `cache_event.consumed` lines for
the patched (gvr, ns, name). If absent at T+50ms → emit
INDETERMINATE_INFORMER_NOT_ACKED.

Output: JSON bundle at /tmp/snowplow-runs/<tag>/verify-serve-stale-<ts>.json
plus an ANSI-coloured stdout summary that mirrors cli.py:_setup_logger.

Exit codes:
    0 — PASS (all probes hit; T-1s body == T+50ms body; T+5s carries
        marker; miss_delta == 0; informer ACK'd)
    1 — INDETERMINATE (window slip / informer not ACK'd / kubectl logs
        unavailable / sources disagree / cache OFF)
    2 — FAIL_<reason> (any probe shows miss / sync cold-fill spike /
        refresh did not complete)
    3 — non-GKE context (inherits cli.py:_gke_context_guard)

Spec — design doc §3 Q1-Q6 + §4 LOC budget + §5 falsifier + §6 risks.
"""

from __future__ import annotations

import hashlib
import json
import os
import queue
import random
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

from bench import browser, cluster
from bench.cluster import COMP_GVR, COMP_RES, NS, kubectl

# ─── Constants ──────────────────────────────────────────────────────────────

# /call URL builder for compositions-list — identical to
# phases.py:_phase8_poll_via_snowplow so we re-use the proven path
# (design Q2).
_COMPOSITIONS_LIST_PATH = (
    "/call?apiVersion=templates.krateo.io%2Fv1"
    "&resource=restactions&name=compositions-list"
    "&namespace=krateo-system"
)

# expvar /debug/vars cell key for restactions/compositions-list class —
# format is "<handlerKind>|<gvrString>" per
# dispatchers/l1_lookup_metrics.go:74-75. GVR.String() emits
# "<group>/<version>, Resource=<resource>" verbatim per apimachinery
# v0.35.3 pkg/runtime/schema/group_version.go:114-116:
#   strings.Join([]string{Group, "/", Version, ", Resource=", Resource}, "")
# (PM tightening #1, 2026-06-02 — original draft used the wrong shape
# "<group>/<version>, <resource>" without the "Resource=" prefix, which
# would have made every live run miss the cell.)
_RESTACTIONS_CELL_KEY = (
    "restactions|templates.krateo.io/v1, Resource=restactions")

# When the mid-probe overshoots this many ms past T_mutate we emit
# INDETERMINATE_MID_WINDOW_SLIPPED per design Q4 + risk register R2.
_MID_WINDOW_HARD_LIMIT_MS = 2000

# Targets enumerated by --target. Today only compositions-list is wired;
# the enum keeps future expansion (dashboard-piechart, compositions-page)
# additive per feedback_no_special_cases.
_TARGET_REGISTRY = {
    "compositions-list": {
        "path": _COMPOSITIONS_LIST_PATH,
        "cell_key": _RESTACTIONS_CELL_KEY,
    },
}


class _VerifyError(Exception):
    """Raised for hard-fail conditions that short-circuit the verdict."""


# ─── /debug/vars snapshot ───────────────────────────────────────────────────


def _snapshot_debug_vars(base_url: str | None = None,
                         timeout: int = 10) -> dict:
    """Fetch /debug/vars from snowplow and parse the JSON envelope.

    Returns a dict (the whole expvar map). Raises _VerifyError on
    transport failure or non-200.

    Used as the PRIMARY hit/miss source per design Q5-A — atomic
    counters bumped in emitResolvedCacheLookup (helpers.go:241-250).
    """
    url = (base_url or browser.SNOWPLOW) + "/debug/vars"
    req = urllib.request.Request(url)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            if r.status != 200:
                raise _VerifyError(
                    f"/debug/vars HTTP {r.status}")
            return json.loads(r.read())
    except urllib.error.URLError as e:
        raise _VerifyError(f"/debug/vars unreachable: {e}") from e


def _cell_delta(vars_before: dict, vars_after: dict,
                cell_key: str) -> tuple[int, int, bool]:
    """Compute (miss_delta, hit_delta, cell_key_inexact) from two
    /debug/vars snapshots for the given cell.

    Risk register R5 fallback: if the exact cell_key is not present
    (snowplow version may use a different separator), sum across all
    cells with the same handlerKind prefix. Operator sees the inexact
    flag in the JSON bundle.
    """
    map_before = (vars_before or {}).get("snowplow_dispatch_l1_lookups") or {}
    map_after = (vars_after or {}).get("snowplow_dispatch_l1_lookups") or {}

    inexact = False
    if cell_key in map_after or cell_key in map_before:
        before_cell = map_before.get(cell_key) or {}
        after_cell = map_after.get(cell_key) or {}
    else:
        # Fallback: sum across all "<handlerKind>|*" cells.
        handler_kind = cell_key.split("|", 1)[0]
        before_cell = {"hit_total": 0, "miss_total": 0}
        after_cell = {"hit_total": 0, "miss_total": 0}
        for k, v in map_before.items():
            if k.startswith(handler_kind + "|"):
                before_cell["hit_total"] += v.get("hit_total", 0) or 0
                before_cell["miss_total"] += v.get("miss_total", 0) or 0
        for k, v in map_after.items():
            if k.startswith(handler_kind + "|"):
                after_cell["hit_total"] += v.get("hit_total", 0) or 0
                after_cell["miss_total"] += v.get("miss_total", 0) or 0
        inexact = True

    miss_delta = int(after_cell.get("miss_total", 0) or 0) \
        - int(before_cell.get("miss_total", 0) or 0)
    hit_delta = int(after_cell.get("hit_total", 0) or 0) \
        - int(before_cell.get("hit_total", 0) or 0)
    return miss_delta, hit_delta, inexact


# ─── Probe primitive ────────────────────────────────────────────────────────


def _probe(target_path: str, token: str, trace_id: str,
           timeout: int = 30) -> dict:
    """Fire one /call request, return a probe-result dict.

    Returns:
        {
            "trace_id": "...",
            "http_ms": int,
            "code": int,
            "body_sha256": "...",  # SHA-256 of response bytes
            "marker_present": False,  # set later by harness
            "body_bytes": bytes,  # retained for marker scan
        }

    Per design Q3: direct urllib HTTP (no Playwright), bench-managed JWT,
    X-Krateo-TraceId header for log correlation.
    """
    ms, code, body = browser.http_get(
        target_path, token, timeout=timeout, retries=1, trace_id=trace_id)
    sha = hashlib.sha256(body or b"").hexdigest()
    return {
        "trace_id": trace_id,
        "http_ms": ms,
        "code": code,
        "body_sha256": sha,
        "body_bytes": body or b"",
        "marker_present": False,
    }


# ─── Target picker (random composition) ─────────────────────────────────────


def _pick_target_composition(rng: random.Random) -> tuple[str, str]:
    """Choose one random composition for the mutation.

    Returns (namespace, name). Raises _VerifyError if none exist.

    Per design Q1: a composition annotation patch dirty-marks the
    compositions-list LIST-dep — ANY composition in the cluster works;
    a random pick avoids hot-spotting one object across runs.
    """
    rc, out, _ = kubectl(
        "get", f"{COMP_RES}.{COMP_GVR}",
        "-A", "--no-headers",
        "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    if rc != 0 or not out.strip():
        raise _VerifyError(
            "no compositions found cluster-wide — cannot mutate")
    rows = []
    for line in out.splitlines():
        parts = line.split(None, 1)
        if len(parts) >= 2:
            rows.append((parts[0], parts[1]))
    if not rows:
        raise _VerifyError("composition list parsed empty")
    return rng.choice(rows)


def _patch_composition_marker(ns: str, name: str,
                              marker: str) -> tuple[int, float, str]:
    """Add annotation `snowplow-bench/verify-serve-stale-marker=<marker>`.

    Returns (rc, t_mutate_monotonic, stderr). t_mutate is captured AFTER
    the kubectl call returns — i.e. apiserver wrote-and-acknowledged.

    Per design Q1: ADD-on-existing-object annotation flows through
    deps_watch.go OnAdd/OnUpdate → dirty-marks compositions-list
    LIST-scope dependents.
    """
    patch = json.dumps({"metadata": {"annotations": {
        "snowplow-bench/verify-serve-stale-marker": marker,
    }}})
    rc, _, err = kubectl(
        "patch", f"{COMP_RES}.{COMP_GVR}", name,
        "-n", ns, "--type=merge", "-p", patch)
    return rc, time.monotonic(), err


def _remove_composition_marker(ns: str, name: str) -> bool:
    """Cleanup: set the marker annotation to null (removes it).

    Returns True on rc=0. Not load-bearing for verdict — operators can
    re-run safely with the marker left in place.
    """
    patch = json.dumps({"metadata": {"annotations": {
        "snowplow-bench/verify-serve-stale-marker": None,
    }}})
    rc, _, _ = kubectl(
        "patch", f"{COMP_RES}.{COMP_GVR}", name,
        "-n", ns, "--type=merge", "-p", patch)
    return rc == 0


# ─── Pod log filter (post-hoc SECONDARY) ────────────────────────────────────


def _grep_pod_logs_for_traces(trace_ids: list[str],
                              since: str = "30s",
                              tail: int = 4000,
                              retries: int = 3) -> dict:
    """Fetch deployment/snowplow logs and filter by trace_id.

    Returns {trace_id: [parsed_event, ...]}. Each parsed_event is the
    raw JSON dict of a `resolved_cache.lookup` log line whose `traceId`
    matches. Missing trace_ids are present in the dict with an empty
    list.

    Risk register R7 mitigation — retry up to `retries` with 1s back-off
    if any trace_id yields 0 events.
    """
    out_per_tid: dict[str, list] = {tid: [] for tid in trace_ids}
    wanted = set(trace_ids)
    for attempt in range(retries):
        rc, out, err = kubectl(
            "logs", "-n", NS, "deployment/snowplow",
            f"--since={since}", f"--tail={tail}")
        if rc != 0:
            if attempt < retries - 1:
                time.sleep(1.0)
                continue
            raise _VerifyError(
                f"kubectl logs rc={rc}: {err[:200] if err else 'no output'}")
        # Reset accumulator each retry — only the latest fetch counts.
        out_per_tid = {tid: [] for tid in trace_ids}
        for line in out.splitlines():
            if not line or not line.startswith("{"):
                continue
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                continue
            if ev.get("msg") != "resolved_cache.lookup":
                continue
            tid = ev.get("traceId") or ev.get("trace_id")
            if tid in wanted:
                out_per_tid[tid].append(ev)
        if all(out_per_tid[tid] for tid in trace_ids):
            return out_per_tid
        if attempt < retries - 1:
            time.sleep(1.0)
    return out_per_tid


# ─── Informer ACK watcher (PM Q3 — Option A) ────────────────────────────────


class _InformerAckWatcher(threading.Thread):
    """Tails `kubectl logs --since=10s -f deployment/snowplow` and reports
    when a `cache_event.consumed` line with the patched (gvr, ns, name)
    arrives.

    Caller flow:
        watcher = _InformerAckWatcher(gvr, ns, name)
        watcher.start()      # subscribes BEFORE the mutation
        ...                  # run mutation
        seen = watcher.seen_by(deadline_monotonic)  # blocks until deadline
        watcher.stop()

    PM-required (Option A): if the informer has NOT logged the
    dirty-mark by the time the mid-probe fires, the harness emits
    INDETERMINATE_INFORMER_NOT_ACKED — caller decides what to do.

    Risk register R7 — pod log stream may lag. We mitigate by starting
    the stream BEFORE the mutation (subscribed → kubectl tail buffers
    events; we never poll-after-the-fact).
    """

    def __init__(self, gvr_string: str, namespace: str, name: str):
        super().__init__(daemon=True)
        self._gvr_string = gvr_string
        self._namespace = namespace
        self._name = name
        self._proc: subprocess.Popen | None = None
        self._seen_event = threading.Event()
        self._stop = threading.Event()
        self._err_queue: queue.Queue[str] = queue.Queue()

    def run(self) -> None:
        try:
            self._proc = subprocess.Popen(
                ["kubectl", "logs", "-n", NS,
                 "deployment/snowplow",
                 "--since=10s", "-f", "--tail=500"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True, bufsize=1)
        except Exception as e:
            self._err_queue.put(f"popen failed: {e}")
            return
        try:
            self._parse_stream(self._proc.stdout)
        except Exception as e:
            self._err_queue.put(f"stdout read failed: {e}")

    def _parse_stream(self, stream) -> None:
        """Parse slog JSON lines from `stream` until a matching
        `cache_event.consumed` line lands OR self._stop fires.

        Extracted as its own method so the meta-falsifier can drive it
        with a fake stdin stream (PM tightening #2, 2026-06-02) — no
        subprocess + no thread required for the test.

        Matching contract (TRACED to internal/cache/deps.go:549-557 for
        OnAdd/OnUpdate and 634-644 for OnDelete):
            ev["msg"] == "cache_event.consumed"
            ev["gvr"] == self._gvr_string   (verbatim GVR.String() shape)
            ev["ns"]  == self._namespace
            ev["name"] == self._name
        """
        for line in stream:
            if self._stop.is_set():
                return
            if not line or not line.startswith("{"):
                continue
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                continue
            if ev.get("msg") != "cache_event.consumed":
                continue
            if ev.get("gvr") != self._gvr_string:
                continue
            if ev.get("ns") != self._namespace:
                continue
            if ev.get("name") != self._name:
                continue
            self._seen_event.set()
            return

    def seen_by(self, deadline_monotonic: float) -> bool:
        """Block until either the dirty-mark line is observed OR the
        deadline passes. Returns True on observation, False otherwise.
        """
        timeout = max(0.0, deadline_monotonic - time.monotonic())
        return self._seen_event.wait(timeout=timeout)

    def stop(self) -> None:
        """Best-effort termination of the kubectl logs subprocess.

        PM tightening #3 (2026-06-02): terminate() + wait(timeout=2.0)
        + kill() fallback so a hung kubectl never orphans on the bench
        laptop after the harness exits.
        """
        self._stop.set()
        if self._proc is None:
            return
        try:
            self._proc.terminate()
        except Exception:
            pass
        try:
            self._proc.wait(timeout=2.0)
        except subprocess.TimeoutExpired:
            try:
                self._proc.kill()
            except Exception:
                pass
            try:
                self._proc.wait(timeout=1.0)
            except Exception:
                pass
        except Exception:
            pass


# ─── Marker presence in body ────────────────────────────────────────────────


def _marker_in_body(body: bytes, marker: str) -> bool:
    """True if `marker` appears anywhere in `body` (UTF-8 decoded).

    Per design §3 Q1: the annotation value is the marker string itself
    and surfaces in the compositions-list response body.
    """
    if not body or not marker:
        return False
    try:
        return marker in body.decode("utf-8", errors="replace")
    except Exception:
        return False


# ─── Verdict logic ──────────────────────────────────────────────────────────


def _decide_verdict(probes: list[dict],
                    miss_delta: int,
                    hit_delta: int,
                    log_events: dict,
                    informer_ack: str,
                    cell_inexact: bool) -> tuple[str, int]:
    """Apply the verdict matrix from design §2 + §5.

    Returns (verdict_string, exit_code). Verdict strings match the
    falsifier table cases exactly so the meta-tests can grep on them.
    """
    pre, mid, post = probes

    # Hard-fail HTTP codes first.
    for p in probes:
        if p["code"] != 200:
            return (f"FAIL_HTTP_{p['code']}_ON_{p['window']}", 2)

    # PM Q3 — Option A informer-ack gate (INDETERMINATE if mid fires
    # before deps_watch logged the dirty-mark for the patched object).
    if informer_ack == "NOT_ACKED":
        return ("INDETERMINATE_INFORMER_NOT_ACKED", 1)

    # Window slip: design Q4 + risk register R2.
    if abs(mid["offset_ms"]) > _MID_WINDOW_HARD_LIMIT_MS:
        return ("INDETERMINATE_MID_WINDOW_SLIPPED", 1)

    # PRIMARY counter assertion: miss_delta must be 0 across all probes.
    if miss_delta != 0:
        return ("FAIL_SYNC_COLD_FILL_ON_MID", 2)

    # SECONDARY log presence per probe — primary already passed.
    missing = [p["window"] for p in probes
               if not log_events.get(p["trace_id"])]
    if missing:
        return (f"INDETERMINATE_LOG_FILTER_UNAVAILABLE", 1)

    # Sources-disagree: PRIMARY says no miss but a per-probe log line
    # has hit:false.
    for p in probes:
        for ev in log_events.get(p["trace_id"], []):
            if ev.get("hit") is False:
                return ("FAIL_SOURCES_DISAGREE", 2)

    # Refresh contract: pre body must equal mid body, and post must
    # differ from pre AND carry the marker.
    if pre["body_sha256"] != mid["body_sha256"]:
        return ("FAIL_STALE_NOT_SERVED_ON_MID", 2)
    if mid["marker_present"]:
        # Stale window should NOT carry the just-written marker.
        return ("FAIL_STALE_NOT_SERVED_ON_MID", 2)
    if post["body_sha256"] == pre["body_sha256"]:
        return ("FAIL_REFRESH_NOT_COMPLETED_BY_5S", 2)
    if not post["marker_present"]:
        return ("FAIL_REFRESH_NOT_COMPLETED_BY_5S", 2)

    # All checks pass.
    return ("PASS", 0)


# ─── Public entry point ─────────────────────────────────────────────────────


def run_verify_serve_stale(user: str = "cyberjoker",
                           target: str = "compositions-list",
                           tag: str | None = None,
                           run_dir: Path | None = None,
                           rng_seed: int | None = None,
                           log=print,
                           section=print,
                           # I/O stub hooks for the falsifier:
                           _probe_fn=None,
                           _snapshot_fn=None,
                           _grep_fn=None,
                           _pick_target_fn=None,
                           _patch_fn=None,
                           _ack_fn=None,
                           _sleep_fn=None) -> tuple[int, dict]:
    """Run the 3-probe serve-stale-while-refresh verifier.

    Returns (exit_code, bundle_dict). Caller writes the bundle and
    returns the exit code; cli.cmd_verify_serve_stale is the only
    production caller.

    All I/O is parameterised via the `_*_fn` stub hooks (defaults wired
    to the real helpers). The falsifier meta-tests inject deterministic
    stubs — no cluster contact.
    """
    target_cfg = _TARGET_REGISTRY.get(target)
    if target_cfg is None:
        raise _VerifyError(
            f"unknown --target {target!r}; known: "
            f"{sorted(_TARGET_REGISTRY)}")

    probe_fn = _probe_fn or _probe
    snapshot_fn = _snapshot_fn or _snapshot_debug_vars
    grep_fn = _grep_fn or _grep_pod_logs_for_traces
    pick_fn = _pick_target_fn or _pick_target_composition
    patch_fn = _patch_fn or _patch_composition_marker
    sleep_fn = _sleep_fn or time.sleep
    ack_fn = _ack_fn  # None → real _InformerAckWatcher

    # Pre-flight: /debug/vars reachable AND publishes the cell?
    try:
        vars_before = snapshot_fn()
    except _VerifyError as e:
        return 1, {"verdict": "INDETERMINATE_DEBUG_VARS_UNREACHABLE",
                   "error": str(e), "exit_code": 1}
    if "snowplow_dispatch_l1_lookups" not in (vars_before or {}):
        # Risk register R8 — CACHE_ENABLED=false.
        return 1, {"verdict": "INDETERMINATE_CACHE_OFF",
                   "exit_code": 1,
                   "reason": "snowplow_dispatch_l1_lookups not published"}

    # Pick the mutation target.
    rng = random.Random(rng_seed)
    try:
        ns, name = pick_fn(rng)
    except _VerifyError as e:
        return 1, {"verdict": "INDETERMINATE_NO_TARGET_FOUND",
                   "error": str(e), "exit_code": 1}

    # JWT for the chosen user.
    tokens = browser.login_all()
    token = tokens.get(user)
    if not token:
        return 1, {"verdict": "INDETERMINATE_LOGIN_FAILED",
                   "user": user, "exit_code": 1}

    marker = f"verify-serve-stale-{int(time.time() * 1000)}-{uuid.uuid4().hex[:8]}"
    run_id = uuid.uuid4().hex[:6]
    tid_pre = f"bench-vss-{run_id}-pre"
    tid_mid = f"bench-vss-{run_id}-mid"
    tid_post = f"bench-vss-{run_id}-post"

    target_path = target_cfg["path"]
    cell_key = target_cfg["cell_key"]

    section(f"verify-serve-stale ({user} / {target}) — tag={tag or 'unset'}")
    log(f"target composition: ns={ns} name={name} marker={marker}")

    # ── Subscribe to deps_watch log stream BEFORE the mutation ─────────
    # PM Option A (mandatory): start `kubectl logs --since=10s -f` BEFORE
    # the mutation so the dirty-mark `cache_event.consumed` line cannot
    # land before we're listening. The 10s --since gives a wide backfill
    # window so the kubectl-side stream-attach race is non-load-bearing.
    watcher = None
    if ack_fn is None:
        # gvr_string format matches what snowplow's slog emits, which is
        # schema.GroupVersionResource.String() VERBATIM. Per apimachinery
        # v0.35.3 pkg/runtime/schema/group_version.go:114-116:
        #   strings.Join([]string{Group, "/", Version,
        #                         ", Resource=", Resource}, "")
        # producing "<group>/<version>, Resource=<resource>".
        # COMP_GVR = "composition.krateo.io"; bench compositions live
        # under composition_yaml's "v1-2-2" version (cluster.py:574-578).
        # PM tightening #1 (2026-06-02): the original ", <resource>" shape
        # (no "Resource=" prefix) would NOT match any real log line and
        # would force INDETERMINATE_INFORMER_NOT_ACKED on every live run.
        comp_gvr_string = (
            f"{COMP_GVR}/v1-2-2, Resource={COMP_RES}")
        watcher = _InformerAckWatcher(comp_gvr_string, ns, name)
        watcher.start()
        # Grace for the kubectl logs -f subprocess to attach.
        sleep_fn(0.30)

    # ── T-1s probe (steady-state, pre-mutation) ────────────────────────
    t0 = time.monotonic()
    started_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    pre = probe_fn(target_path, token, tid_pre)
    pre["window"] = "pre"
    pre["offset_ms"] = -1000  # nominal; pre fires at t0
    pre["marker_present"] = _marker_in_body(pre["body_bytes"], marker)
    log(f"T-1s probe   ... code={pre['code']} body_sha={pre['body_sha256'][:8]}"
        f"... trace={tid_pre} ({pre['http_ms']}ms)")

    # Wait the remaining time to T_mutate (t0 + 1.0s).
    target_mutate = t0 + 1.000
    sleep_fn(max(0.0, target_mutate - time.monotonic()))

    # ── MUTATE ─────────────────────────────────────────────────────────
    rc, t_mutate, err = patch_fn(ns, name, marker)
    if rc != 0:
        if watcher is not None:
            watcher.stop()
        return 1, {"verdict": "INDETERMINATE_PATCH_FAILED",
                   "error": err, "exit_code": 1}
    log(f"MUTATE       ... rc={rc} t_mutate=+{(t_mutate - t0):.3f}s")

    # ── T+50ms probe ───────────────────────────────────────────────────
    mid_target = t_mutate + 0.050
    sleep_fn(max(0.0, mid_target - time.monotonic()))

    # PM Option A: verify informer ACK BEFORE firing the mid-probe.
    # ACK is a hard gate — NOT_ACKED → INDETERMINATE_INFORMER_NOT_ACKED.
    if ack_fn is not None:
        # Stub injection path (used by the meta-falsifier).
        informer_ack = ack_fn(ns, name, marker, t_mutate)
    elif watcher is not None:
        # seen_by blocks until either the dirty-mark line is observed or
        # the deadline (time.monotonic() — i.e. NOW, since we already
        # slept to mid_target) passes.
        seen = watcher.seen_by(time.monotonic())
        informer_ack = "ACKED" if seen else "NOT_ACKED"
        watcher.stop()
    else:
        # Defensive — both ack_fn and watcher None should be impossible.
        informer_ack = "NOT_ACKED"

    actual_mid_offset_ms = int((time.monotonic() - t_mutate) * 1000)
    mid = probe_fn(target_path, token, tid_mid)
    mid["window"] = "mid"
    mid["offset_ms"] = actual_mid_offset_ms
    mid["marker_present"] = _marker_in_body(mid["body_bytes"], marker)
    log(f"T+50ms probe ... code={mid['code']} body_sha={mid['body_sha256'][:8]}"
        f"... trace={tid_mid} (offset=+{actual_mid_offset_ms}ms, "
        f"informer={informer_ack})")

    # ── T+5s probe ─────────────────────────────────────────────────────
    post_target = t_mutate + 5.000
    sleep_fn(max(0.0, post_target - time.monotonic()))
    actual_post_offset_ms = int((time.monotonic() - t_mutate) * 1000)
    post = probe_fn(target_path, token, tid_post)
    post["window"] = "post"
    post["offset_ms"] = actual_post_offset_ms
    post["marker_present"] = _marker_in_body(post["body_bytes"], marker)
    log(f"T+5s probe   ... code={post['code']} body_sha={post['body_sha256'][:8]}"
        f"... trace={tid_post} (offset=+{actual_post_offset_ms}ms, "
        f"marker={'present' if post['marker_present'] else 'ABSENT'})")

    # ── PRIMARY: /debug/vars snapshot AFTER all probes ─────────────────
    try:
        vars_after = snapshot_fn()
    except _VerifyError as e:
        return 1, {"verdict": "INDETERMINATE_DEBUG_VARS_UNREACHABLE",
                   "error": str(e), "exit_code": 1}
    miss_delta, hit_delta, cell_inexact = _cell_delta(
        vars_before, vars_after, cell_key)
    log(f"L1 counters  ... miss_delta={miss_delta} hit_delta={hit_delta}"
        f"{' (cell_key_inexact)' if cell_inexact else ''}")

    # ── SECONDARY: pod log post-hoc filter ─────────────────────────────
    log_events: dict
    try:
        # Brief wait so the post-probe log line has time to flush.
        sleep_fn(2.0)
        log_events = grep_fn([tid_pre, tid_mid, tid_post])
    except _VerifyError as e:
        log(f"pod log filter failed: {e}")
        log_events = {tid_pre: [], tid_mid: [], tid_post: []}

    # ── Cleanup ────────────────────────────────────────────────────────
    cleanup_ok = _remove_composition_marker(ns, name)
    log(f"CLEANUP      ... annotation removed={cleanup_ok}")

    # ── Verdict ────────────────────────────────────────────────────────
    verdict, exit_code = _decide_verdict(
        [pre, mid, post], miss_delta, hit_delta,
        log_events, informer_ack, cell_inexact)
    section(f"VERDICT: {verdict}")

    # ── Bundle ─────────────────────────────────────────────────────────
    def _strip_body(p: dict) -> dict:
        return {k: v for k, v in p.items() if k != "body_bytes"}

    bundle = {
        "verdict": verdict,
        "exit_code": exit_code,
        "started_at": started_at,
        "user": user,
        "target": target,
        "tag": tag,
        "mutation": {
            "namespace": ns,
            "name": name,
            "marker": marker,
            "patch_rc": rc,
        },
        "probes": [_strip_body(pre), _strip_body(mid), _strip_body(post)],
        "l1_lookups": {
            "class": cell_key,
            "miss_delta": miss_delta,
            "hit_delta": hit_delta,
            "cell_key_inexact": cell_inexact,
        },
        "informer_ack_check": informer_ack,
        "log_events": {
            "pre":  [{"hit": e.get("hit"),
                      "key_hash": e.get("key_hash"),
                      "resident_bytes": e.get("resident_bytes")}
                     for e in log_events.get(tid_pre, [])],
            "mid":  [{"hit": e.get("hit"),
                      "key_hash": e.get("key_hash"),
                      "resident_bytes": e.get("resident_bytes")}
                     for e in log_events.get(tid_mid, [])],
            "post": [{"hit": e.get("hit"),
                      "key_hash": e.get("key_hash"),
                      "resident_bytes": e.get("resident_bytes")}
                     for e in log_events.get(tid_post, [])],
        },
        "stale_served": pre["body_sha256"] == mid["body_sha256"],
        "refresh_completed": post["body_sha256"] != pre["body_sha256"]
            and post["marker_present"],
        "sources_agree": all(
            ev.get("hit") is True
            for tid in (tid_pre, tid_mid, tid_post)
            for ev in log_events.get(tid, [])
        ),
        "cleanup": {"annotation_removed": cleanup_ok},
    }

    if run_dir is not None:
        try:
            Path(run_dir).mkdir(parents=True, exist_ok=True)
            ts = time.strftime("%Y%m%d-%H%M%S")
            out_path = Path(run_dir) / f"verify-serve-stale-{ts}.json"
            out_path.write_text(json.dumps(bundle, indent=2, default=str))
            log(f"bundle written to {out_path}")
        except Exception as e:
            log(f"bundle write failed: {e}")

    return exit_code, bundle


__all__ = ["run_verify_serve_stale"]
