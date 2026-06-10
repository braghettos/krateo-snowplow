"""Canonical ledger row builder + report emission (Block 4).

Mix-weighted = 0.95 * cyberjoker + 0.05 * admin (feedback_north_star_is_frontend_ux).
Schema is FROZEN — see source 7109-7133 docstring; the JSON-Schema artifact at
`bench/ledger_row.schema.json` is the machine-readable surface for acceptance (b).

Functions moved (per plan §A.7):
  411-415   record
  411       test_results global  -> _results + add_result/get_results
  7032-7064 _convergence_p99_for_stage  -> convergence_p99_for_stage
  7065-7108 _aggregate_validation       -> aggregate_validation
  7109-7338 _build_canonical_ledger_row -> build_canonical_ledger_row
  7340-7350 _load_per_mutation_metric   -> _load_per_mutation_metric (run_dir-relative now)
  7351-7416 _compute_verdict            -> compute_verdict
  7417-7426 get_runtime_metrics
  8164-8224 print_report
   939-942  pct(data, p)                -> re-exported from bench.cluster.pct

New (Block 4):
  write_run_bundle      — §F directory tree serializer (replaces source 8663-8682)
  compute_verdict_with_falsifier
  emit_ledger_row_schema — writes JSON-Schema 2020-12 covering §F.1 key list
  read_baseline          — read e2e/bench/.baseline.json (acceptance (c))
  compute_baseline_delta — ±15% gate helper (acceptance (c))

Bundle-truncate logic per R3.1 / tightening #6: ledger row STILL emits with verdict
intact when video bundle exceeds 200 MB; oldest .webm/.gif pairs by mtime drop until
under cap; `oversize_bundle.json` lists trimmed files; summary.json.bundle_truncated=True.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §A.7 + §F.
"""

from __future__ import annotations

import datetime
import json
import os
import time
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

# Deferred — cluster.pct is the same helper from worktree source 939-942.
from bench.cluster import kubectl, pct as _pct  # type: ignore

__all__ = [
    "record",
    "add_result",
    "get_results",
    "reset_results",
    "build_canonical_ledger_row",
    "compute_verdict",
    "compute_verdict_with_falsifier",
    "aggregate_validation",
    "convergence_p99_for_stage",
    "pct",
    "get_runtime_metrics",
    "print_report",
    "write_run_bundle",
    "emit_ledger_row_schema",
    "read_baseline",
    "compute_baseline_delta",
    "RUN_BUNDLE_MAX_BYTES",
    "BASELINE_PATH",
]


# ─── Module-level results buffer (per-process, rename of test_results) ──────


_results: list[dict] = []


def add_result(entry: dict) -> None:
    """Append a result row (stage-runner side); per-process buffer."""
    _results.append(entry)


def get_results() -> list[dict]:
    """Return the per-process results buffer (defensive copy)."""
    return list(_results)


def reset_results() -> None:
    """Clear the per-process results buffer (test fixture hook)."""
    _results.clear()


# ─── ANSI helpers (mirror cli.py constants so reports render uniformly) ─────


_GREEN = "\033[32m"
_RED = "\033[31m"
_BOLD = "\033[1m"
_RESET = "\033[0m"


# ─── record (worktree source 411-415) ───────────────────────────────────────


def record(name: str, passed: bool, ms: int = 0, code: int = 0,
           note: str = "") -> None:
    """Append a result row; matches worktree source 411-415 behaviour."""
    _results.append({
        "name": name, "passed": passed, "ms": ms, "code": code, "note": note,
    })
    tag = f"{_GREEN}PASS{_RESET}" if passed else f"{_RED}FAIL{_RESET}"
    print(f"  [{tag}] {name:<65s} {ms:>5d}ms  HTTP {code:<4}  {note}")


# ─── pct helper (worktree source 939-942) ───────────────────────────────────


def pct(data, p):
    """Nearest-rank percentile. Returns 0 for empty input (legacy behavior).

    The cluster.pct helper returns None for empty; ledger callers historically
    guarded with `if cold_samples else 0`, so we keep the 0 sentinel here
    while delegating the non-empty path to cluster.pct for code re-use.
    """
    if not data:
        return 0
    return _pct(data, p)


# ─── runtime metrics (worktree source 7417-7426) ────────────────────────────


def get_runtime_metrics():
    """Fetch /metrics/runtime from snowplow. Returns dict or None."""
    snowplow = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")
    try:
        req = urllib.request.Request(snowplow + "/metrics/runtime")
        with urllib.request.urlopen(req, timeout=10) as r:
            return json.loads(r.read())
    except Exception:
        return None


# ─── convergence_p99_for_stage (worktree source 7032-7064) ──────────────────


def convergence_p99_for_stage(all_results: Iterable[dict], stage_label: str) -> int:
    """p99 of convergence_ms across navigations for a given stage."""
    def _samples_for(mode):
        out = []
        for entry in all_results:
            if str(entry.get("stage")) != stage_label:
                continue
            if entry.get("cache") != mode:
                continue
            for _, pg in (entry.get("pages") or {}).items():
                for nav in (pg.get("navigations") or []):
                    cm = nav.get("convergence_ms")
                    if isinstance(cm, (int, float)) and cm >= 0:
                        out.append(int(cm))
        return out
    samples = _samples_for("ON") or _samples_for("OFF")
    if not samples:
        return -1
    return pct(samples, 99)


# ─── aggregate_validation (worktree source 7065-7108) ───────────────────────


def aggregate_validation(all_results: Iterable[dict]) -> dict:
    """Aggregate widget-terminal-state validation across every nav."""
    summary = {
        "navs_terminal_pass": 0,
        "navs_terminal_fail": 0,
        "skeleton_failures": [],
        "errored_widgets_total": 0,
        "call_count_mismatches": [],
    }
    for entry in all_results:
        for _, pg in (entry.get("pages") or {}).items():
            for nav in (pg.get("navigations") or []):
                v = nav.get("validation")
                if not isinstance(v, dict):
                    continue
                label = nav.get("label") or "<unlabeled>"
                if v.get("terminal_state") == "pass":
                    summary["navs_terminal_pass"] += 1
                else:
                    summary["navs_terminal_fail"] += 1
                # #289a: respect the efaf1a4 skeleton demotion. A skeleton
                # that was demoted to a benign WARN (terminal_state=='pass'
                # AND skeleton_materializing==True — the small-N
                # materialization race) is NOT a failure and must not be
                # reported as one. A skeleton that is still a HARD FAIL
                # (terminal_state=='fail', or any skeleton not carrying the
                # materializing demotion) IS still recorded. Telemetry-only:
                # this does not change the verdict (that keys off
                # navs_terminal_fail, which already reflects the demotion).
                demoted = (v.get("terminal_state") == "pass"
                           and v.get("skeleton_materializing") is True)
                if v.get("skeleton_count", 0) > 0 and not demoted:
                    summary["skeleton_failures"].append(label)
                summary["errored_widgets_total"] += int(
                    v.get("errored_count", 0) or 0)
                exp = v.get("expected_calls")
                if exp is not None and not v.get("calls_within_tolerance", True):
                    summary["call_count_mismatches"].append(
                        (label, exp, v.get("actual_calls", 0)))
    return summary


# ─── _load_per_mutation_metric (relocated; worktree source 7340-7350) ───────


def _load_per_mutation_metric(key: str, *, run_dir: Path | None = None):
    """Load a Phase-8 metric.

    Block 4 relocation: prefer `{run_dir}/phase8/per_mutation_results.json`
    when `run_dir` is provided; fall back to the legacy hardcoded path
    `/tmp/snowplow_per_mutation_results.json` so behaviour is identical
    when no run_dir is supplied.
    """
    candidates: list[str] = []
    if run_dir is not None:
        candidates.append(str(Path(run_dir) / "phase8" / "per_mutation_results.json"))
    candidates.append("/tmp/snowplow_per_mutation_results.json")

    for path in candidates:
        try:
            with open(path) as f:
                d = json.load(f)
            v = d.get(key)
            if v is not None:
                return v
        except Exception:
            continue
    return None


# ─── Latency tier thresholds (ms) ───────────────────────────────────────────
#
# warm_p50 (500) and cold (1000) are UNCHANGED — Diego explicitly kept them
# pending #288.
#
# CONV_TIER_MS = 30000 is the 50K-scale REVISED convergence tier (Task #289,
# Diego-ratified 2026-06-10). The prior 1000ms tier is structurally
# unreachable at 50K — proved, not assumed (per feedback_north_star_is_desiderata:
# "relax only when data proves structural limit"):
#   - docs/task-282-serve-stale-depth-trace-2026-06-10.md §1: a single
#     compositions-panels LIST re-resolve costs 11.5s p50 / 52.8s p99 at 50K
#     (krateo-system/blueprints-panels: n=57, p50=11,535ms, p99=52,753ms), and
#   - the VERIFY poll cadence imposes a ~22-24s floor observed across every
#     run: convergence_mass_s8_p99 ≈ 23.8s measured (run-20260609-221611,
#     run-20260609-234201, and the prior 0.30.248 run-20260605-114100 all sit
#     in the 22-24s band).
# 30000ms covers the measured ~23.8s p99 + margin. This is the eventually-
# consistent serve-stale contract at scale; #286 (fast-lane) may allow
# tightening this tier later.
CONV_TIER_MS = 30000
WARM_P50_TIER_MS = 500   # UNCHANGED pending #288
COLD_TIER_MS = 1000      # UNCHANGED pending #288


# ─── compute_verdict (worktree source 7351-7416) ────────────────────────────


def compute_verdict(mix_weighted, restarts, conv_s8_p99, cells=None):
    """Verdict per the architect's gates.

    PASS:        warm_p50 < 500ms, cold < 1000ms, conv < CONV_TIER_MS, 0 restarts
    WEAK_PASS:   one tier missed by <=20%
    FAIL:        2+ tiers missed OR restarts > 0
    FLOOR:       measurements present, but the deployed chart has no cache
                 toggle (cache_supported=false). Surfaces as structural N/A.
    REJECT:      pod crashed, no usable measurements

    Conv tier is CONV_TIER_MS (30000ms at 50K scale, Task #289) — see the
    constant's comment for the structural-limit derivation. warm_p50/cold
    tiers unchanged.
    """
    if not mix_weighted:
        return "REJECT"
    wp50 = mix_weighted.get("warm_p50_ms")
    cold = mix_weighted.get("cold_ms")
    if wp50 is None or wp50 <= 0:
        return "REJECT"
    if restarts > 0:
        return "FAIL"

    def _wp50(c):
        v = (c or {}).get("warm_p50_ms")
        return v if v is not None else 0

    if cells:
        on_zero = (_wp50(cells.get("admin_on")) <= 0 and
                   _wp50(cells.get("cyber_on")) <= 0)
        off_nonzero = (_wp50(cells.get("admin_off")) > 0 or
                       _wp50(cells.get("cyber_off")) > 0)
        if on_zero and off_nonzero:
            return "FLOOR"
    misses = 0
    if wp50 > WARM_P50_TIER_MS:
        misses += 1
    if cold is None or cold > COLD_TIER_MS:
        misses += 1
    if conv_s8_p99 is not None and conv_s8_p99 > CONV_TIER_MS:
        misses += 1
    if misses == 0:
        return "PASS"
    if misses == 1:
        return "WEAK_PASS"
    return "FAIL"


def compute_verdict_with_falsifier(mix_weighted, restarts, conv_s8_p99,
                                   *, cells=None,
                                   per_stage_proofs: dict | None = None) -> dict:
    """Wraps compute_verdict and answers the falsifier acceptance (d):
    "would deleting THIS stage's proof change the verdict?"

    Returns a dict with:
      verdict:       same as compute_verdict()
      proof_impact:  {stage_id: bool}  — True if dropping the proof flips
                     the verdict (or makes it INVALID).
    """
    verdict = compute_verdict(mix_weighted, restarts, conv_s8_p99, cells=cells)
    impact: dict[str, bool] = {}
    if per_stage_proofs:
        for sid, proof in per_stage_proofs.items():
            # The proofs underwrite the cell counts. Dropping a proof in
            # this verdict surface does NOT mutate the cell percentiles
            # (those come from all_results). Falsifier tag is therefore
            # `False` by default — if a future schema change ties verdict
            # to a proof field (e.g. content_match in S6), set True here.
            impact[sid] = bool(proof.get("convergence_timeout"))
    return {"verdict": verdict, "proof_impact": impact}


# ─── Canonical ledger row builder (worktree source 7109-7338) ───────────────


def build_canonical_ledger_row(all_results: list[dict], *,
                               run_dir: Path | None = None,
                               tag: str | None = None,
                               scale: int | None = None) -> dict:
    """Assemble the canonical ledger row from Phase-6 measurements.

    Schema FROZEN — do NOT add/rename keys without updating the architect's
    plan AND the tester's reader. Refer to docs/bench-restructure-path-b-plan-2026-06-02.md
    §F.1 for the full key list and bench/ledger_row.schema.json for the
    JSON-Schema artifact.
    """
    tag = tag or os.environ.get("EXPECTED_IMAGE_TAG", "unknown")
    ship_date = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d")
    if scale is None:
        scale = int(os.environ.get("SCALE", "5000"))

    # Per-cell waterfall stats. The validation framework marks
    # `waterfallMs == 0` as an "incomplete" sentinel; those MUST be
    # filtered before percentile computation. See worktree 7138-7152.
    def _cell_stats(user, cache_mode):
        cold_samples: list[int] = []
        warm_samples: list[int] = []
        valid_nav_count = 0
        invalid_nav_count = 0
        for entry in all_results:
            if entry.get("cache") != cache_mode:
                continue
            for _, pg in (entry.get("pages") or {}).items():
                for nav in (pg.get("navigations") or []):
                    if nav.get("user") and nav["user"] != user:
                        continue
                    wf = nav.get("waterfallMs", 0) or 0
                    if wf <= 0 or nav.get("incomplete"):
                        invalid_nav_count += 1
                        continue
                    valid_nav_count += 1
                    if nav.get("nav_num") == 1 or nav.get("cold_warm") == "COLD":
                        cold_samples.append(wf)
                    else:
                        warm_samples.append(wf)
        total_navs = valid_nav_count + invalid_nav_count
        terminal_fail_rate = (
            float(invalid_nav_count) / total_navs if total_navs else 0.0
        )
        if total_navs > 0 and valid_nav_count == 0:
            return {
                "cold_ms":              None,
                "warm_p50_ms":          None,
                "warm_p99_ms":          None,
                "valid_nav_count":      0,
                "invalid_nav_count":    invalid_nav_count,
                "terminal_fail_rate":   round(terminal_fail_rate, 4),
            }
        return {
            "cold_ms":            pct(cold_samples, 50) if cold_samples else 0,
            "warm_p50_ms":        pct(warm_samples, 50) if warm_samples else 0,
            "warm_p99_ms":        pct(warm_samples, 99) if warm_samples else 0,
            "valid_nav_count":    valid_nav_count,
            "invalid_nav_count":  invalid_nav_count,
            "terminal_fail_rate": round(terminal_fail_rate, 4),
        }

    cells = {
        "admin_on":   _cell_stats("admin",      "ON"),
        "admin_off":  _cell_stats("admin",      "OFF"),
        "cyber_on":   _cell_stats("cyberjoker", "ON"),
        "cyber_off":  _cell_stats("cyberjoker", "OFF"),
    }

    # Mirror admin -> cyber ONLY when no cyber samples exist (worktree 7210-7234).
    def _cell_has_navs(c):
        v = c.get("valid_nav_count") or 0
        iv = c.get("invalid_nav_count") or 0
        return (v + iv) > 0

    def _cell_has_data(c):
        cold = c.get("cold_ms")
        wp50 = c.get("warm_p50_ms")
        return (cold is not None and cold > 0) or (wp50 is not None and wp50 > 0)

    def _mirror(target, source):
        if _cell_has_navs(cells[target]):
            return
        if not _cell_has_data(cells[source]):
            return
        cells[target] = dict(cells[source])
        cells[target]["mirrored_from"] = source
    _mirror("cyber_on", "admin_on")
    _mirror("cyber_off", "admin_off")

    # Mix-weighted = 0.95 * cyber + 0.05 * admin (worktree 7236-7269).
    def _val(cell, field):
        v = cell.get(field)
        return v if (v is not None and v > 0) else None

    def _mw_pick(field):
        cyber = _val(cells["cyber_on"], field)
        if cyber is None:
            cyber = _val(cells["cyber_off"], field)
        admin = _val(cells["admin_on"], field)
        if admin is None:
            admin = _val(cells["admin_off"], field)
        if cyber is None and admin is None:
            return None
        if cyber is None:
            return int(round(admin))
        if admin is None:
            return int(round(cyber))
        return int(round(0.95 * cyber + 0.05 * admin))
    mix_weighted = {
        "cold_ms":     _mw_pick("cold_ms"),
        "warm_p50_ms": _mw_pick("warm_p50_ms"),
        "warm_p99_ms": _mw_pick("warm_p99_ms"),
    }

    # Pod restart count (worktree 7272-7281).
    restarts = 0
    try:
        rc, out, _ = kubectl(
            "get", "pods", "-n", "krateo-system",
            "-l", "app.kubernetes.io/name=snowplow",
            "-o",
            "jsonpath={.items[0].status.containerStatuses[0].restartCount}")
        if rc == 0 and out.strip():
            restarts = int(out.strip())
    except Exception:
        pass

    # Uptime at capture (worktree 7286-7293).
    uptime_s = 0
    try:
        rc, out, _ = kubectl(
            "get", "pods", "-n", "krateo-system",
            "-l", "app.kubernetes.io/name=snowplow",
            "-o", "jsonpath={.items[0].status.startTime}")
        if rc == 0 and out.strip():
            t0 = datetime.datetime.fromisoformat(
                out.strip().replace("Z", "+00:00"))
            uptime_s = int((datetime.datetime.now(datetime.timezone.utc) - t0)
                           .total_seconds())
    except Exception:
        pass

    validation = aggregate_validation(all_results)

    mix_has_null = any(v is None for v in mix_weighted.values())
    base_verdict = compute_verdict(
        mix_weighted, restarts,
        convergence_p99_for_stage(all_results, "8"),
        cells=cells)
    if validation["navs_terminal_fail"] > 0 or mix_has_null:
        verdict = "INVALID"
    else:
        verdict = base_verdict

    return {
        "tag": tag,
        "ship_date": ship_date,
        "scale": [scale, 5000] if scale != 5000 else [5000],
        "uptime_at_capture_s": uptime_s,
        "cells": cells,
        "mix_weighted": mix_weighted,
        "convergence_mass_s6_p99": convergence_p99_for_stage(all_results, "6"),
        "convergence_mass_s7_p99": convergence_p99_for_stage(all_results, "7"),
        "convergence_mass_s8_p99": convergence_p99_for_stage(all_results, "8"),
        "convergence_per_mutation_p99_mix": _load_per_mutation_metric(
            "p99_mix", run_dir=run_dir),
        "convergence_per_class_hot_p99":    _load_per_mutation_metric(
            "hot_p99", run_dir=run_dir),
        "convergence_per_class_warm_p99":   _load_per_mutation_metric(
            "warm_p99", run_dir=run_dir),
        "convergence_per_class_cold_p99":   _load_per_mutation_metric(
            "cold_p99", run_dir=run_dir),
        "tag_specific_verifications": {},
        "pod_restart_count": restarts,
        "validation": validation,
        "verdict": verdict,
    }


# ─── print_report (worktree source 8164-8174) ───────────────────────────────


def print_report() -> bool:
    """Print final report; True iff zero failures in `_results`."""
    print(f"\n  {_BOLD}=== FINAL REPORT ==={_RESET}", flush=True)
    passed = [r for r in _results if r["passed"]]
    failed = [r for r in _results if not r["passed"]]
    print(f"\n  Total: {len(_results)}   "
          f"{_GREEN}Passed: {len(passed)}{_RESET}   "
          f"{_RED}Failed: {len(failed)}{_RESET}\n")
    if failed:
        print(f"  {_RED}{_BOLD}FAILED TESTS:{_RESET}")
        for r in failed:
            print(f"    {_RED}x{_RESET} {r['name']:<65s}  "
                  f"HTTP {r['code']:<4}  {r['ms']}ms  {r['note']}")
        print()
    return len(failed) == 0


# ─── Run-bundle writer + truncate logic (per R3.1) ──────────────────────────


RUN_BUNDLE_MAX_BYTES = 200 * 1024 * 1024  # 200 MB cap
BASELINE_PATH = Path(__file__).resolve().parent.parent / ".baseline.json"


def _video_pairs(run_dir: Path) -> list[Path]:
    """Return .webm files under run_dir/videos/ ordered oldest first."""
    vdir = run_dir / "videos"
    if not vdir.is_dir():
        return []
    webms = [p for p in vdir.iterdir() if p.suffix.lower() == ".webm"]
    return sorted(webms, key=lambda p: p.stat().st_mtime)


def _bundle_size_bytes(run_dir: Path) -> int:
    total = 0
    for sub in ("videos", "screenshots", "pod_logs"):
        d = run_dir / sub
        if not d.is_dir():
            continue
        for p in d.rglob("*"):
            if p.is_file():
                try:
                    total += p.stat().st_size
                except OSError:
                    pass
    return total


def _truncate_bundle(run_dir: Path, *,
                     max_bytes: int | None = None) -> tuple[bool, list[dict]]:
    """Drop oldest .webm/.gif pairs until bundle <= max_bytes.

    `max_bytes` defaults to the module-level RUN_BUNDLE_MAX_BYTES at
    call time (resolved via the module attr, so tests can monkey-patch
    it without monkey-patching the function default).

    Returns (truncated, trimmed_list). trimmed_list has dicts with
    keys: name, size, mtime, reason. Writes `oversize_bundle.json`
    alongside ledger_row.json when truncation fired.
    """
    if max_bytes is None:
        max_bytes = RUN_BUNDLE_MAX_BYTES
    if _bundle_size_bytes(run_dir) <= max_bytes:
        return False, []
    trimmed: list[dict] = []
    for webm in _video_pairs(run_dir):
        if _bundle_size_bytes(run_dir) <= max_bytes:
            break
        gif = webm.with_suffix(".gif")
        for victim in (webm, gif):
            if victim.exists():
                size = victim.stat().st_size
                mtime = victim.stat().st_mtime
                try:
                    victim.unlink()
                except OSError:
                    continue
                trimmed.append({
                    "name": victim.name, "size": size,
                    "mtime": datetime.datetime.fromtimestamp(
                        mtime, datetime.timezone.utc).isoformat(),
                    "reason": "bundle_oversize_truncate",
                })
    if trimmed:
        (run_dir / "oversize_bundle.json").write_text(
            json.dumps({
                "max_bytes": max_bytes,
                "trimmed_count": len(trimmed),
                "trimmed": trimmed,
            }, indent=2))
        n = len(trimmed)
        mb = sum(t["size"] for t in trimmed) / (1024 * 1024)
        print(f"  BUNDLE TRUNCATED: dropped {n} files "
              f"({mb:.1f} MB) to fit "
              f"{max_bytes // (1024 * 1024)} MB cap", flush=True)
    return bool(trimmed), trimmed


def write_run_bundle(run_dir: Path,
                     all_results: list[dict],
                     *,
                     per_stage_proofs: dict | None = None,
                     tag: str | None = None,
                     scale: int | None = None,
                     duration_s: float | None = None,
                     overlay_age_days: float | None = None) -> dict:
    """Serialize the §F directory tree.

    Replaces inline json.dump at worktree source 8663-8682. Side-effect:
    writes ledger_row.json, summary.json, per_stage.json, expected_calls.json
    snapshot, and pod_logs/full_run.txt under `run_dir`.

    Returns the canonical ledger row dict for caller convenience.
    """
    run_dir = Path(run_dir)
    run_dir.mkdir(parents=True, exist_ok=True)
    (run_dir / "pod_logs").mkdir(exist_ok=True)

    # 1) Build canonical row.
    row = build_canonical_ledger_row(
        all_results, run_dir=run_dir, tag=tag, scale=scale)
    (run_dir / "ledger_row.json").write_text(json.dumps(row, indent=2,
                                                       default=str))

    # 2) Aggregated per-stage proofs.
    if per_stage_proofs:
        (run_dir / "per_stage.json").write_text(
            json.dumps(per_stage_proofs, indent=2, default=str))

    # 3) Expected-calls snapshot — verbatim copy of the overlay at run start.
    try:
        from bench.expected import EXPECTED_CALLS_OVERLAY_PATH
        if os.path.exists(EXPECTED_CALLS_OVERLAY_PATH):
            with open(EXPECTED_CALLS_OVERLAY_PATH, "rb") as f:
                (run_dir / "expected_calls.json").write_bytes(f.read())
    except Exception:
        pass

    # 4) Full-run pod logs (single kubectl call).
    try:
        rc, out, _ = kubectl(
            "logs", "deployment/snowplow",
            "-n", "krateo-system", "-c", "snowplow",
            "--tail=1000000", timeout_secs=120,
        )
        if rc == 0 and out:
            (run_dir / "pod_logs" / "full_run.txt").write_text(out)
    except Exception:
        pass

    # 5) Bundle-truncate (per R3.1).
    truncated, trimmed = _truncate_bundle(run_dir)

    # 6) summary.json — see §F.2.
    baseline_tag, baseline_warm = read_baseline()
    actual_warm = row.get("mix_weighted", {}).get("warm_p50_ms")
    baseline_delta = compute_baseline_delta(actual_warm, baseline_warm)

    failed_gates: list[str] = []
    if row["verdict"] != "PASS":
        if row["validation"]["navs_terminal_fail"] > 0:
            failed_gates.append("terminal_state")
        if row["pod_restart_count"] > 0:
            failed_gates.append("pod_restarts")
        if any(v is None for v in row["mix_weighted"].values()):
            failed_gates.append("mix_weighted_null")
        # #289b: enumerate latency-tier misses so a non-PASS verdict never
        # reads as a contradiction (verdict=FAIL with failed_gates=[]). Each
        # entry names the tier and the offending value, e.g.
        # "tier:warm_p50 914>500". Thresholds mirror compute_verdict's tier
        # logic exactly (same constants) so the two cannot drift.
        mw = row["mix_weighted"]
        wp50 = mw.get("warm_p50_ms")
        cold = mw.get("cold_ms")
        conv = row.get("convergence_mass_s8_p99")
        if wp50 is not None and wp50 > WARM_P50_TIER_MS:
            failed_gates.append(f"tier:warm_p50 {wp50}>{WARM_P50_TIER_MS}")
        if cold is None or (cold is not None and cold > COLD_TIER_MS):
            failed_gates.append(
                f"tier:cold {cold}>{COLD_TIER_MS}" if cold is not None
                else f"tier:cold null>{COLD_TIER_MS}")
        if conv is not None and conv not in (-1,) and conv > CONV_TIER_MS:
            failed_gates.append(f"tier:conv_s8_p99 {conv}>{CONV_TIER_MS}")

    summary = {
        "verdict": row["verdict"],
        "mix_weighted": row["mix_weighted"],
        "tag": row["tag"],
        "scale": scale or int(os.environ.get("SCALE", "5000")),
        "run_dir": str(run_dir.absolute()),
        "stages_completed": (
            list(per_stage_proofs.keys()) if per_stage_proofs else []),
        "duration_s": duration_s,
        "pod_restarts": row["pod_restart_count"],
        "convergence_p99_s": {
            "s6": row["convergence_mass_s6_p99"] / 1000.0
                  if row["convergence_mass_s6_p99"] not in (None, -1) else None,
            "s7": row["convergence_mass_s7_p99"] / 1000.0
                  if row["convergence_mass_s7_p99"] not in (None, -1) else None,
            "s8": row["convergence_mass_s8_p99"] / 1000.0
                  if row["convergence_mass_s8_p99"] not in (None, -1) else None,
        },
        "failed_gates": failed_gates,
        "convergence_timeout_stage": _find_convergence_timeout_stage(
            per_stage_proofs),
        "ledger_row_path": "ledger_row.json",
        "bundle_truncated": truncated,
        "baseline_tag": baseline_tag,
        "baseline_warm_p50_ms": baseline_warm,
        "baseline_delta_ratio": baseline_delta,
        "overlay_age_days_at_start": overlay_age_days,
    }
    (run_dir / "summary.json").write_text(json.dumps(summary, indent=2,
                                                    default=str))
    return row


def _find_convergence_timeout_stage(per_stage_proofs: dict | None) -> str | None:
    if not per_stage_proofs:
        return None
    for sid, proof in per_stage_proofs.items():
        if isinstance(proof, dict) and proof.get("convergence_timeout"):
            return sid
    return None


# ─── Baseline (acceptance (c)) ──────────────────────────────────────────────


def read_baseline() -> tuple[str | None, float | None]:
    """Read `e2e/bench/.baseline.json` if present.

    Returns (baseline_tag, baseline_warm_p50_ms). Both None when the
    file is missing or malformed — acceptance (c) treats absence as
    "no anchor; record but don't gate."
    """
    try:
        with open(BASELINE_PATH) as f:
            d = json.load(f)
        return d.get("baseline_tag"), d.get("baseline_warm_p50_ms")
    except Exception:
        return None, None


def compute_baseline_delta(actual_warm_p50_ms,
                           baseline_warm_p50_ms) -> float | None:
    """Signed relative delta: (actual - baseline) / baseline.

    Returns None when either input is missing/non-positive. Acceptance (c)
    gates |delta| <= 0.15 — but the gate ITSELF lives in Block 5's
    acceptance step; this helper is the math.
    """
    if not actual_warm_p50_ms or not baseline_warm_p50_ms:
        return None
    if baseline_warm_p50_ms <= 0:
        return None
    return (actual_warm_p50_ms - baseline_warm_p50_ms) / baseline_warm_p50_ms


# ─── JSON-Schema emitter (acceptance (b)) ───────────────────────────────────


def _ledger_row_schema_doc() -> dict:
    """Return the JSON-Schema 2020-12 covering §F.1 frozen key list."""
    cell_props = {
        "cold_ms":             {"type": ["integer", "null"]},
        "warm_p50_ms":         {"type": ["integer", "null"]},
        "warm_p99_ms":         {"type": ["integer", "null"]},
        "valid_nav_count":     {"type": "integer", "minimum": 0},
        "invalid_nav_count":   {"type": "integer", "minimum": 0},
        "terminal_fail_rate":  {"type": "number"},
        "mirrored_from":       {"type": "string"},
    }
    cell_schema = {
        "type": "object",
        "required": ["cold_ms", "warm_p50_ms", "warm_p99_ms",
                     "valid_nav_count", "invalid_nav_count",
                     "terminal_fail_rate"],
        "properties": cell_props,
        "additionalProperties": False,
    }
    mw_schema = {
        "type": "object",
        "required": ["cold_ms", "warm_p50_ms", "warm_p99_ms"],
        "properties": {
            "cold_ms":     {"type": ["integer", "null"]},
            "warm_p50_ms": {"type": ["integer", "null"]},
            "warm_p99_ms": {"type": ["integer", "null"]},
        },
        "additionalProperties": False,
    }
    return {
        "$schema": "https://json-schema.org/draft/2020-12/schema",
        "$id": "https://krateo.io/snowplow/bench/ledger_row.schema.json",
        "title": "Snowplow bench canonical ledger row",
        "description": (
            "Frozen schema covering docs/bench-restructure-path-b-plan-"
            "2026-06-02.md §F.1 key list. Bumping requires a fresh PM gate."
        ),
        "type": "object",
        "required": [
            "tag", "ship_date", "scale", "uptime_at_capture_s",
            "cells", "mix_weighted",
            "convergence_mass_s6_p99", "convergence_mass_s7_p99",
            "convergence_mass_s8_p99",
            "convergence_per_mutation_p99_mix",
            "convergence_per_class_hot_p99",
            "convergence_per_class_warm_p99",
            "convergence_per_class_cold_p99",
            "tag_specific_verifications", "pod_restart_count",
            "validation", "verdict",
        ],
        "properties": {
            "tag":                  {"type": "string"},
            "ship_date":            {"type": "string"},
            "scale": {
                "type": "array",
                "items": {"type": "integer", "minimum": 1},
                "minItems": 1, "maxItems": 2,
            },
            "uptime_at_capture_s":  {"type": "integer", "minimum": 0},
            "cells": {
                "type": "object",
                "required": ["admin_on", "admin_off", "cyber_on", "cyber_off"],
                "properties": {
                    "admin_on":  cell_schema,
                    "admin_off": cell_schema,
                    "cyber_on":  cell_schema,
                    "cyber_off": cell_schema,
                },
                "additionalProperties": False,
            },
            "mix_weighted": mw_schema,
            "convergence_mass_s6_p99": {"type": "integer"},
            "convergence_mass_s7_p99": {"type": "integer"},
            "convergence_mass_s8_p99": {"type": "integer"},
            "convergence_per_mutation_p99_mix": {"type": ["integer", "null"]},
            "convergence_per_class_hot_p99":    {"type": ["integer", "null"]},
            "convergence_per_class_warm_p99":   {"type": ["integer", "null"]},
            "convergence_per_class_cold_p99":   {"type": ["integer", "null"]},
            "tag_specific_verifications": {"type": "object"},
            "pod_restart_count": {"type": "integer", "minimum": 0},
            "validation": {
                "type": "object",
                "required": ["navs_terminal_pass", "navs_terminal_fail",
                             "skeleton_failures", "errored_widgets_total",
                             "call_count_mismatches"],
                "properties": {
                    "navs_terminal_pass": {"type": "integer", "minimum": 0},
                    "navs_terminal_fail": {"type": "integer", "minimum": 0},
                    "skeleton_failures":  {"type": "array",
                                           "items": {"type": "string"}},
                    "errored_widgets_total": {"type": "integer",
                                              "minimum": 0},
                    "call_count_mismatches": {"type": "array"},
                },
                "additionalProperties": True,
            },
            "verdict": {
                "type": "string",
                "enum": ["PASS", "WEAK_PASS", "FAIL", "FLOOR", "INVALID",
                         "REJECT"],
            },
        },
        "additionalProperties": True,
    }


def emit_ledger_row_schema(path: Path | None = None) -> Path:
    """Write the JSON-Schema artifact next to this module by default."""
    target = (Path(path) if path else
              Path(__file__).resolve().parent / "ledger_row.schema.json")
    target.write_text(json.dumps(_ledger_row_schema_doc(), indent=2))
    return target


# ─── Legacy aliases for back-compat (callers using the worktree names) ──────


_build_canonical_ledger_row = build_canonical_ledger_row
_compute_verdict = compute_verdict
_aggregate_validation = aggregate_validation
_convergence_p99_for_stage = convergence_p99_for_stage
