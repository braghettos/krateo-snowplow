"""bench probe-determinism — body_sha256 stability under repeated reads.

Fires N back-to-back probes against the same snowplow /call path with
NO mutation between them, captures body_sha256 + key_hash + L1 hit per
probe, and emits a verdict:

    DETERMINISTIC      — all probes hit AND all body_sha256 identical
                         (verify-serve-stale's body-content check is
                         trustable; #161 root cause = refresher rewrite).
    NON_DETERMINISTIC  — all probes hit BUT body_sha256 differs
                         (response stamping injects per-call bytes after
                         L1 read; #161 root cause = (2); verify-serve-
                         stale needs a body normalizer before Stage 2).
    MIXED              — at least one probe was a miss (cohort wasn't
                         warm); re-run.

Spec — dispatch 2026-06-02 issue #161 disambiguation, follow-up to
verify-serve-stale's body_sha pre/mid/post mismatch with identical
key_hash. Single-file subcommand, <300 LOC, no cluster mutation.

Exit codes:
    0 — DETERMINISTIC (verdict body_sha == 1 unique value)
    2 — NON_DETERMINISTIC (verdict body_sha > 1 unique value, all hit)
    3 — MIXED (at least one probe missed; re-run)
    1 — INDETERMINATE (HTTP errors, /debug/vars unreachable, no logs)
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

from bench import browser
from bench.cluster import NS, kubectl


class _ProbeError(Exception):
    """Raised for hard-fail conditions that short-circuit the verdict."""


# ─── Probe primitive ────────────────────────────────────────────────────────


def _probe(target_path: str, token: str, trace_id: str,
           timeout: int = 30) -> dict:
    """Fire one /call request with the bench-supplied trace_id header.

    Returns:
        {
            "trace_id": str,
            "http_ms": int,
            "code": int,
            "body_sha256": hex str,
            "body_len": int,
        }

    Mirrors verify_serve_stale._probe shape so the two subcommands feed
    the same downstream tooling.
    """
    ms, code, body = browser.http_get(
        target_path, token, timeout=timeout, retries=1, trace_id=trace_id)
    body = body or b""
    return {
        "trace_id": trace_id,
        "http_ms": ms,
        "code": code,
        "body_sha256": hashlib.sha256(body).hexdigest(),
        "body_len": len(body),
    }


# ─── Pod-log post-hoc filter — same shape as verify_serve_stale ─────────────


def _grep_pod_logs_for_traces(trace_ids: list[str],
                              since: str = "60s",
                              tail: int = 4000,
                              retries: int = 3) -> dict:
    """Filter `kubectl logs deployment/snowplow` for the bench's traceIds.

    Returns {trace_id: [parsed_event, ...]}. Each event is a
    `resolved_cache.lookup` JSON line whose `traceId` matches. Missing
    trace_ids appear with an empty list.
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
            raise _ProbeError(
                f"kubectl logs rc={rc}: {err[:200] if err else 'no output'}")
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


# ─── Verdict logic ──────────────────────────────────────────────────────────


def _decide_verdict(probes: list[dict],
                    log_events: dict) -> tuple[str, int]:
    """Apply the determinism matrix.

    Returns (verdict_string, exit_code). Matrix:
        all hit + all body_sha256 equal → DETERMINISTIC (0)
        all hit + body_sha256 differs   → NON_DETERMINISTIC (2)
        at least one miss               → MIXED (3)
        HTTP error / log gap            → INDETERMINATE (1)
    """
    # Hard-fail HTTP codes first.
    for i, p in enumerate(probes):
        if p["code"] != 200:
            return (f"INDETERMINATE_HTTP_{p['code']}_ON_PROBE_{i}", 1)

    # Pull per-probe hit booleans from the secondary log source. The
    # log filter is the authority for hit/miss here because /debug/vars
    # counters are an aggregate delta — we need the per-trace verdict
    # to detect a single miss in a multi-probe burst.
    per_probe_hits: list[bool | None] = []
    missing_log = []
    for p in probes:
        events = log_events.get(p["trace_id"], [])
        if not events:
            per_probe_hits.append(None)
            missing_log.append(p["trace_id"])
            continue
        # Any event with hit=true → probe hit. If ALL events hit=false
        # for this tid, probe was a miss.
        any_hit = any(ev.get("hit") is True for ev in events)
        per_probe_hits.append(any_hit)

    if missing_log:
        return ("INDETERMINATE_LOG_FILTER_UNAVAILABLE", 1)

    if any(h is False for h in per_probe_hits):
        return ("MIXED", 3)

    unique_shas = {p["body_sha256"] for p in probes}
    if len(unique_shas) == 1:
        return ("DETERMINISTIC", 0)
    return ("NON_DETERMINISTIC", 2)


# ─── Public entry point ─────────────────────────────────────────────────────


def run_probe_determinism(user: str = "cyberjoker",
                          target_path: str = "",
                          probes_n: int = 5,
                          tag: str | None = None,
                          run_dir: Path | None = None,
                          inter_probe_delay_s: float = 0.0,
                          log=print,
                          section=print,
                          # I/O stub hooks for the falsifier:
                          _probe_fn=None,
                          _grep_fn=None,
                          _login_fn=None,
                          _sleep_fn=None) -> tuple[int, dict]:
    """Run N back-to-back probes, return (exit_code, bundle_dict).

    All I/O is parameterised via the `_*_fn` hooks (defaults wired to
    the real helpers). The meta-tests inject deterministic stubs.
    """
    if not target_path or not target_path.startswith("/"):
        raise _ProbeError(
            f"--path must be a snowplow /call query starting with "
            f"'/'; got {target_path!r}")
    if probes_n < 2:
        raise _ProbeError(
            f"--probes must be >= 2 (single probe cannot test "
            f"determinism); got {probes_n}")

    probe_fn = _probe_fn or _probe
    grep_fn = _grep_fn or _grep_pod_logs_for_traces
    login_fn = _login_fn or browser.login_all
    sleep_fn = _sleep_fn or time.sleep

    tokens = login_fn()
    token = tokens.get(user)
    if not token:
        return 1, {"verdict": "INDETERMINATE_LOGIN_FAILED",
                   "user": user, "exit_code": 1}

    run_id = uuid.uuid4().hex[:6]
    section(f"probe-determinism ({user} / {probes_n} probes) — "
            f"tag={tag or 'unset'}")
    log(f"target path: {target_path}")

    started_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    probes: list[dict] = []
    for i in range(probes_n):
        tid = f"bench-pd-{run_id}-{i:02d}"
        if i > 0 and inter_probe_delay_s > 0:
            sleep_fn(inter_probe_delay_s)
        p = probe_fn(target_path, token, tid)
        p["index"] = i
        probes.append(p)
        log(f"probe {i:02d}      ... code={p['code']} "
            f"body_sha={p['body_sha256'][:8]}... body_len={p['body_len']} "
            f"trace={tid} ({p['http_ms']}ms)")

    # Brief wait so the log lines flush before we grep.
    sleep_fn(2.0)

    trace_ids = [p["trace_id"] for p in probes]
    try:
        log_events = grep_fn(trace_ids)
    except _ProbeError as e:
        log(f"pod log filter failed: {e}")
        log_events = {tid: [] for tid in trace_ids}

    # Annotate each probe with its log-evidence (hit + key_hash).
    for p in probes:
        events = log_events.get(p["trace_id"], [])
        if events:
            p["l1_hit"] = any(ev.get("hit") is True for ev in events)
            # Take the FIRST event's key_hash — for a single /call the
            # dispatcher emits ONE lookup line per (handlerKind, gvr)
            # cohort cell. If multiple cells fire we still surface the
            # first; the verdict treats key_hash as evidence not verdict.
            p["key_hash"] = events[0].get("key_hash", "")
        else:
            p["l1_hit"] = None
            p["key_hash"] = ""

    verdict, exit_code = _decide_verdict(probes, log_events)
    section(f"VERDICT: {verdict}")

    # Surface the unique-sha set + per-probe summary so the operator
    # can see WHY at a glance.
    unique_shas = sorted({p["body_sha256"] for p in probes})
    unique_key_hashes = sorted({p.get("key_hash", "") for p in probes
                                 if p.get("key_hash")})
    log(f"unique body_sha256 count = {len(unique_shas)}")
    log(f"unique key_hash count    = {len(unique_key_hashes)}")
    log(f"all hits                 = {all(p.get('l1_hit') is True for p in probes)}")

    bundle = {
        "verdict": verdict,
        "exit_code": exit_code,
        "started_at": started_at,
        "user": user,
        "target_path": target_path,
        "probes_n": probes_n,
        "tag": tag,
        "probes": [{k: v for k, v in p.items() if k != "body_bytes"}
                   for p in probes],
        "unique_body_sha256": unique_shas,
        "unique_key_hash": unique_key_hashes,
        "all_hits": all(p.get("l1_hit") is True for p in probes),
    }

    if run_dir is not None:
        try:
            Path(run_dir).mkdir(parents=True, exist_ok=True)
            ts = time.strftime("%Y%m%d-%H%M%S")
            out_path = Path(run_dir) / f"probe-determinism-{ts}.json"
            out_path.write_text(json.dumps(bundle, indent=2, default=str))
            log(f"bundle written to {out_path}")
        except Exception as e:
            log(f"bundle write failed: {e}")

    return exit_code, bundle


__all__ = ["run_probe_determinism"]
