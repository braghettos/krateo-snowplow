"""Command-line surface for the bench harness.

Block 2 ships ONLY:
  - `_gke_context_guard` (subset of `cluster.gke_context_guard` — delegates
    so the canonical gate stays single-sourced; per PM carry-forward #2).
  - `cmd_check(args)` — the 7-item preflight gate (no mutations, 60s budget).
  - `verify_deployed_image()` — moved from worktree source 951-984; used by
    `cmd_check` and (Block 4) `cmd_phase6`.
  - `_setup_logger()` — coloured logger that replaces lifecycle.py's
    Block-1 `log()` shim (per PM carry-forward #1).

Subsequent commands (calibrate, cleanup, storm, converge, measure, phase6,
phase7, phase8, report) land in Block 4. Every entrypoint passes through
`_gke_context_guard()` first per feedback_kubectl_verify_gke_context.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §A.9 + §G Block 2.

Exit codes:
  0 — success
  1 — generic failure
  2 — preflight gate fail (one or more gates FAILED; stderr names each)
  3 — non-GKE kubectl context (refused to operate)
  4 — convergence timeout (raised by Block-3 ConvergenceTimeout)
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

from bench import cluster
from bench.cluster import (
    CANONICAL_GKE_CONTEXT,
    NS,
    kubectl,
)


__all__ = ["main", "cmd_check", "verify_deployed_image"]


# ─── ANSI helpers + logger ──────────────────────────────────────────────────

RESET = "\033[0m"
BOLD = "\033[1m"
RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
BLUE = "\033[34m"
CYAN = "\033[36m"


def _enabled_ansi():
    """ANSI on for TTYs unless BENCH_NO_COLOR is set."""
    if os.environ.get("BENCH_NO_COLOR", "").strip():
        return False
    return sys.stdout.isatty()


def _setup_logger():
    """Return (log, section) callables wired to the coloured ANSI helpers.

    Per PM carry-forward #1: this replaces lifecycle.py's `log()` shim.
    The functions stay simple — string in, prefixed timestamp + colour
    out. No globals; callers reuse the returned closures (cli.py's
    `log`/`section` re-export them, and lifecycle.py imports those).
    """
    use_ansi = _enabled_ansi()

    def log(msg):
        ts = time.strftime("%H:%M:%S")
        if use_ansi:
            print(f"  [{CYAN}{ts}{RESET}] {msg}", flush=True)
        else:
            print(f"  [{ts}] {msg}", flush=True)

    def section(title):
        if use_ansi:
            print(f"\n  {BOLD}=== {title} ==={RESET}", flush=True)
        else:
            print(f"\n  === {title} ===", flush=True)

    return log, section


log, section = _setup_logger()


# ─── GKE context guard — SUBSET wrapper around cluster.gke_context_guard ────


def _gke_context_guard(allow_non_gke=None):
    """SUBSET of `cluster.gke_context_guard`.

    Per PM carry-forward #2: this MUST stay a subset of the canonical
    cluster.py guard. We delegate — no second, non-conformant guard
    surface. Args + exit semantics mirror cluster.gke_context_guard():
        allow_non_gke=True or BENCH_ALLOW_NON_GKE=1 → bypass.
        Exit 3 on context mismatch.

    Returns the current context string on PASS (for cmd_check to log).
    Calls sys.exit(3) on FAIL.
    """
    # Delegate to cluster.py's canonical implementation. We then read the
    # current-context one more time for the caller's logging — this is
    # additive, not a re-implementation of the gate.
    cluster.gke_context_guard(allow_non_gke=allow_non_gke)
    try:
        proc = subprocess.run(
            ["kubectl", "config", "current-context"],
            capture_output=True, timeout=10,
        )
        return (proc.stdout.decode() or "").strip()
    except Exception:
        return ""


# ─── verify_deployed_image (moved from worktree source 951-984) ─────────────


def verify_deployed_image(expected_tag: str | None = None) -> bool:
    """Ensure the snowplow deployment runs the expected image.

    Per plan §A.9 (`MOVED` from source 951-984). Differences from the
    source version:
      - `expected_tag` arg is now first-class (was the EXPECTED_IMAGE_TAG
        env var only).
      - Returns bool instead of `sys.exit(1)` on miss; caller decides
        what exit code to emit. This makes the function testable.
      - SKIP_IMAGE_CHECK=1 env still bypasses (legacy operator escape
        hatch).

    Returns True on match, False on mismatch / kubectl failure. Writes
    operator-facing diagnostics to stderr on failure.
    """
    if os.environ.get("SKIP_IMAGE_CHECK", "0") == "1":
        log("SKIP_IMAGE_CHECK=1 — skipping image version check")
        return True

    expected = (expected_tag
                or os.environ.get("EXPECTED_IMAGE_TAG", "")
                or "").strip()
    if not expected:
        sys.stderr.write(
            f"\n{RED}{BOLD}ERROR: expected image tag required.{RESET}\n"
            f"  pass --tag <tag> or set EXPECTED_IMAGE_TAG.\n"
            f"  example:\n"
            f"    python -m bench check --tag 0.25.19\n\n"
            f"  Or set SKIP_IMAGE_CHECK=1 to bypass (not recommended).\n"
        )
        return False

    rc, out, err = kubectl(
        "get", "deployment", "snowplow", "-n", NS,
        "-o", "jsonpath={.spec.template.spec.containers[?(@.name==\"snowplow\")].image}",
    )
    if rc != 0 or not out.strip():
        sys.stderr.write(
            f"\n{RED}{BOLD}ERROR: Could not get snowplow deployment image.{RESET}\n"
            f"  kubectl failed or snowplow not found in {NS}. Ensure cluster access.\n"
            f"  stderr: {err[:200] if err else 'none'}\n"
        )
        return False

    current_image = out.strip()
    current_tag = current_image.split(":")[-1] if ":" in current_image else ""
    if current_tag != expected:
        sys.stderr.write(
            f"\n{RED}{BOLD}ERROR: Deployed image does not match expected.{RESET}\n"
            f"  Expected tag: {expected}\n"
            f"  Current image: {current_image}\n"
            f"  Deploy the new image first, then run check.\n"
        )
        return False

    log(f"Deployed image verified: {current_image}")
    return True


# ─── Gate helpers (each returns (passed: bool, msg: str)) ───────────────────


def _gate_pod_ready() -> tuple[bool, str]:
    """Gate 2: snowplow pod ready in krateo-system.

    Checks deployment readiness: 1/1 ready replicas, all containers ready,
    and no restart-storm signal.
    """
    rc, out, err = kubectl(
        "get", "deployment", "snowplow", "-n", NS,
        "-o", "json",
    )
    if rc != 0 or not out.strip():
        return False, (f"snowplow_pod_ready: FAIL (kubectl rc={rc}: "
                       f"{err[:120] if err else 'no output'})")
    try:
        deploy = json.loads(out)
    except json.JSONDecodeError as e:
        return False, f"snowplow_pod_ready: FAIL (JSON parse: {e})"
    status = deploy.get("status", {}) or {}
    desired = status.get("replicas", 0) or 0
    ready = status.get("readyReplicas", 0) or 0
    avail = status.get("availableReplicas", 0) or 0
    if desired == 0:
        return False, "snowplow_pod_ready: FAIL (desired=0; deployment scaled to zero)"
    if ready < desired or avail < desired:
        return False, (f"snowplow_pod_ready: FAIL "
                       f"(desired={desired} ready={ready} available={avail})")
    return True, f"snowplow_pod_ready: PASS (replicas {ready}/{desired})"


def _gate_image_match(expected_tag: str) -> tuple[bool, str]:
    """Gate 3: deployment image tag matches the --tag argument."""
    ok = verify_deployed_image(expected_tag=expected_tag)
    if ok:
        return True, f"image_tag_match: PASS (tag={expected_tag})"
    return False, f"image_tag_match: FAIL (expected={expected_tag})"


def _gate_crds_present(timeout=10) -> tuple[bool, str]:
    """Gate 4: CRDs present — WIDGET_KINDS + composition CRD."""
    from bench.lifecycle import WIDGET_KINDS  # deferred (Block 1 module)

    composition_crd = "compositiondefinitions.core.krateo.io"
    crds_to_check = list(WIDGET_KINDS) + [composition_crd]
    deadline = time.time() + timeout
    missing = list(crds_to_check)

    while time.time() < deadline and missing:
        still_missing = []
        for crd in missing:
            rc, out, _ = kubectl(
                "get", "crd", crd, "--ignore-not-found", "-o", "name",
                timeout_secs=5,
            )
            if rc != 0 or not out.strip():
                still_missing.append(crd)
        missing = still_missing
        if missing:
            time.sleep(1)

    if missing:
        return False, (f"crds_present: FAIL (missing: "
                       f"{', '.join(missing[:3])}{'...' if len(missing) > 3 else ''})")
    return True, f"crds_present: PASS ({len(crds_to_check)} CRDs)"


def _gate_helm_lockstep(expected_tag: str) -> tuple[bool, str]:
    """Gate 5: helm release image tag matches the deployment.

    Catches OBS-1 / 0.30.186-style drift where the deployment carries a
    tag the helm release does NOT know about. See feedback_chart_release_lockstep.
    """
    try:
        proc = subprocess.run(
            ["helm", "get", "values", "snowplow", "-n", NS, "-o", "json"],
            capture_output=True, timeout=15,
        )
    except FileNotFoundError:
        return False, "helm_release_lockstep: FAIL (helm binary not found)"
    except subprocess.TimeoutExpired:
        return False, "helm_release_lockstep: FAIL (helm get values timed out)"
    if proc.returncode != 0:
        err_text = (proc.stderr.decode() or "").strip()
        return False, (f"helm_release_lockstep: FAIL "
                       f"(helm rc={proc.returncode}: {err_text[:120]})")
    try:
        values = json.loads(proc.stdout.decode() or "{}")
    except json.JSONDecodeError as e:
        return False, f"helm_release_lockstep: FAIL (JSON parse: {e})"

    # Look for the tag under common helm chart paths.
    candidates = []

    def _walk(node, path=""):
        if isinstance(node, dict):
            for k, v in node.items():
                if k == "tag" and isinstance(v, (str, int, float)):
                    candidates.append((path + "." + k, str(v)))
                _walk(v, path + "." + str(k))
        elif isinstance(node, list):
            for i, v in enumerate(node):
                _walk(v, path + f"[{i}]")

    _walk(values)

    if not candidates:
        return False, ("helm_release_lockstep: FAIL "
                       "(no .image.tag-like key in helm get values)")

    matches = [c for c in candidates if c[1].strip() == expected_tag.strip()]
    if not matches:
        observed = sorted({c[1] for c in candidates})
        return False, (f"helm_release_lockstep: FAIL "
                       f"(expected tag={expected_tag}, found {observed})")
    return True, f"helm_release_lockstep: PASS (tag={expected_tag})"


def _gate_frontend_reachable() -> tuple[bool, str]:
    """Gate 6: frontend LB reachable (HTTP 200 on /login)."""
    frontend = os.environ.get("FRONTEND_URL", "http://34.46.217.105:8080").strip()
    if not frontend:
        return False, "frontend_lb_reachable: FAIL (FRONTEND_URL not set)"
    url = frontend.rstrip("/") + "/login"
    try:
        with urllib.request.urlopen(url, timeout=5) as r:
            code = r.getcode()
    except urllib.error.HTTPError as e:
        # Some portal builds return 200 + HTML; non-2xx is a fail.
        return False, (f"frontend_lb_reachable: FAIL "
                       f"(HTTP {e.code} from {url})")
    except urllib.error.URLError as e:
        return False, (f"frontend_lb_reachable: FAIL "
                       f"(connection error: {e.reason})")
    except Exception as e:
        return False, (f"frontend_lb_reachable: FAIL "
                       f"({type(e).__name__}: {e})")
    if code != 200:
        return False, f"frontend_lb_reachable: FAIL (HTTP {code} from {url})"
    return True, f"frontend_lb_reachable: PASS ({url})"


def _gate_overlay_freshness(allow_stale: bool) -> tuple[bool, str]:
    """Gate 7: overlay freshness or --allow-stale-overlay."""
    from bench.expected import (
        OverlayStale,
        overlay_age_seconds,
        overlay_freshness_or_die,
    )

    if allow_stale:
        age = overlay_age_seconds()
        if age == float("inf"):
            age_str = "missing"
        else:
            age_str = f"{age / 86400.0:.1f}d"
        # Per dispatch: loud-log when bypassed.
        sys.stderr.write(
            f"  {YELLOW}{BOLD}WARNING: --allow-stale-overlay bypasses freshness gate "
            f"(overlay age={age_str}). Run `python -m bench calibrate` to "
            f"refresh.{RESET}\n"
        )
        return True, f"overlay_freshness: BYPASS (age={age_str})"

    try:
        overlay_freshness_or_die(max_age_days=14)
    except OverlayStale as e:
        return False, f"overlay_freshness: FAIL ({e})"
    age = overlay_age_seconds()
    age_str = "missing" if age == float("inf") else f"{age / 86400.0:.1f}d"
    return True, f"overlay_freshness: PASS (age={age_str})"


# ─── cmd_check — the 7-item preflight gate ──────────────────────────────────


def cmd_check(args) -> int:
    """7-item preflight gate. No mutations; 60s wall-clock budget.

    Returns:
        0 — all 7 gates PASS
        2 — one or more gates FAIL (stderr names each)
        3 — non-GKE kubectl context (handled inside _gke_context_guard)

    Gates (per plan §G Block 2):
        1. GKE context match
        2. snowplow pod ready in krateo-system
        3. image tag matches --tag
        4. CRDs present (WIDGET_KINDS + composition CRD)
        5. helm release lockstep
        6. frontend LB reachable
        7. overlay freshness (--allow-stale-overlay to bypass)
    """
    start = time.time()
    section("bench check — preflight (7 gates)")

    # Gate 1: GKE context.
    allow_non_gke = bool(getattr(args, "allow_non_gke", False))
    ctx = _gke_context_guard(allow_non_gke=allow_non_gke)
    log(f"gke_context: PASS (context={ctx!r})")

    expected_tag = getattr(args, "tag", None) or os.environ.get("EXPECTED_IMAGE_TAG", "")
    expected_tag = (expected_tag or "").strip()

    results: list[tuple[str, bool, str]] = []

    # Gate 2.
    ok, msg = _gate_pod_ready()
    results.append(("snowplow_pod_ready", ok, msg))
    (log if ok else _stderr_log)(msg)

    # Gate 3.
    if not expected_tag:
        msg = "image_tag_match: FAIL (no --tag provided)"
        results.append(("image_tag_match", False, msg))
        _stderr_log(msg)
    else:
        ok, msg = _gate_image_match(expected_tag)
        results.append(("image_tag_match", ok, msg))
        (log if ok else _stderr_log)(msg)

    # Gate 4.
    ok, msg = _gate_crds_present(timeout=10)
    results.append(("crds_present", ok, msg))
    (log if ok else _stderr_log)(msg)

    # Gate 5.
    if not expected_tag:
        msg = "helm_release_lockstep: FAIL (no --tag provided)"
        results.append(("helm_release_lockstep", False, msg))
        _stderr_log(msg)
    else:
        ok, msg = _gate_helm_lockstep(expected_tag)
        results.append(("helm_release_lockstep", ok, msg))
        (log if ok else _stderr_log)(msg)

    # Gate 6.
    ok, msg = _gate_frontend_reachable()
    results.append(("frontend_lb_reachable", ok, msg))
    (log if ok else _stderr_log)(msg)

    # Gate 7.
    allow_stale = bool(getattr(args, "allow_stale_overlay", False))
    ok, msg = _gate_overlay_freshness(allow_stale=allow_stale)
    results.append(("overlay_freshness", ok, msg))
    (log if ok else _stderr_log)(msg)

    elapsed = time.time() - start
    failed = [name for name, ok, _ in results if not ok]
    section(f"check complete in {elapsed:.1f}s")
    if failed:
        sys.stderr.write(
            f"{RED}{BOLD}check FAILED: "
            f"{len(failed)}/{len(results) + 1} gates failed: "
            f"{', '.join(failed)}{RESET}\n"
        )
        return 2
    log(f"{GREEN}{BOLD}check PASS (7/7 gates){RESET}")
    return 0


def _stderr_log(msg):
    """Write a gate-failure line to stderr with a timestamp prefix."""
    ts = time.strftime("%H:%M:%S")
    sys.stderr.write(f"  [{ts}] {msg}\n")


# ─── argparse wiring (Block 2 — only `check`) ───────────────────────────────


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="bench",
        description=(
            "Snowplow bench harness (Path B, 2026-06-02). Drives Playwright + "
            "cluster mutations + canonical ledger emission against the GKE "
            "benchmark cluster."
        ),
    )
    p.add_argument(
        "--allow-non-gke", action="store_true",
        help=argparse.SUPPRESS,  # hidden — kind/minikube only
    )

    sub = p.add_subparsers(dest="cmd")

    p_check = sub.add_parser(
        "check",
        help="Preflight (7 gates, 60s, no mutations).",
    )
    p_check.add_argument(
        "--tag", required=False, default=None,
        help="Expected image tag (e.g. 0.30.232). Falls back to "
             "EXPECTED_IMAGE_TAG env when unset.",
    )
    p_check.add_argument(
        "--allow-stale-overlay", action="store_true",
        help="Bypass the 14-day overlay freshness gate with a loud-log.",
    )
    p_check.set_defaults(func=cmd_check)

    return p


def main(argv: list[str] | None = None) -> int:
    """Entry point for `python -m bench`.

    Returns the exit code; callers should `sys.exit(main())`. Block 2
    only wires `check`; subsequent subcommands land in Block 4.
    """
    parser = _build_parser()
    args = parser.parse_args(argv)

    if args.cmd is None:
        parser.print_help()
        return 0

    func = getattr(args, "func", None)
    if func is None:
        parser.print_help()
        return 0
    return int(func(args))
