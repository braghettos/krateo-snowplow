#!/usr/bin/env python3
"""F5 — Chrome customer-probe pre-ship baseline.

Ship 0.30.242 H.c-layered Phase 3 F5.

CAPTURES the customer-visible compositions count for admin + cyberjoker
across /dashboard + /compositions pages on the LIVE cluster, against
the CURRENT pre-ship cluster state (snowplow rev 419 / chart
snowplow-0.30.243 / image 0.30.235 + portal rev 33 / chart
portal-0.30.178). Emits a JSON baseline that Phase 5 reads to diff
against post-ship measurements.

PASS GATE (Phase 5 contract): for every (user, page) tuple, the
post-ship piechart composition count MUST EQUAL this baseline. Any
drift triggers HARD REVERT.

F5 MEASURES INTEGER COUNTS, NOT RAW WIRE RESPONSES
==================================================

F5 captures customer-visible integer counts only (piechart composition
count via API + UI). NOT raw wire responses. Wire-level diff would
false-positive on metadata churn (resourceVersion, creationTimestamp,
managedFields timestamps) on every Phase 5 run without a real
regression existing. The customer sees the integer count, not
resourceVersion.

F1-F8 + Gap 1 + Gap 3 are orthogonal safety nets. F5's integer-equality
+ F6 (cross-binding isolation) + F3 (seed↔dispatch convergence) +
F7 (Empty-UID invariant) + F2 (kubectl-truth equivalence) is the
customer-correctness contract.

BENCH HARNESS REUSE — F5 IS ~50 LOC OF ORCHESTRATION
====================================================

The `e2e/bench/bench/browser.py` Playwright module already provides
the FULL primitive stack:
  - `_ensure_users()` — reads admin/cyberjoker passwords from
    k8s Secrets (admin-password / cyberjoker-password in krateo-system).
    NEVER hardcoded.
  - `login(user, password)` — obtains JWT via portal LB.
  - `browser_login(page, user, password)` — Playwright UI login.
  - `browser_measure_navigation(page, page_path, label)` — page nav
    + network-stable waterfall.
  - `verify_composition_count_api(token)` — piechart count via direct
    snowplow API call (status.widgetData.title).
  - `verify_composition_count_ui(page)` — piechart count via the
    browser's own auth-token (validates UI's data path).
  - `BROWSER_PAGES` — [("Dashboard", "/dashboard"), ("Compositions",
    "/compositions")].

F5 just orchestrates them + emits JSON.

GUARDRAILS
==========

  - kubectl current-context MUST start with gke_neon-481711_ per
    `feedback_kubectl_verify_gke_context` (verified at script start).
  - Mode B only — runs only when invoked. Mirrors F2's ship-gate
    pattern (no env var to skip; the script is itself the gate).
  - Failure handling: ANY measurement failure (login error, count==-1,
    Playwright crash) → script EXITS non-zero with diagnostic. Does
    NOT emit a partial baseline.
  - All credentials read from k8s Secrets (no hardcoded passwords).

USAGE

    cd e2e/bench
    python3 f5_preship_baseline.py [--out=PATH]

Default output: e2e/bench/f5_preship_baseline_<YYYY-MM-DD>.json
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

# Ensure the bench module is importable when invoked from e2e/bench/.
SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

# Bench primitives — Playwright-based reuse.
from bench import browser  # noqa: E402

GKE_CONTEXT = "gke_neon-481711_us-central1-a_cluster-1"


def fatal(msg: str) -> None:
    print(f"F5 FATAL: {msg}", file=sys.stderr)
    sys.exit(1)


def verify_kubectl_context() -> str:
    """Hard rule per feedback_kubectl_verify_gke_context."""
    out = subprocess.run(
        ["kubectl", "config", "current-context"],
        capture_output=True, text=True, timeout=10,
    )
    if out.returncode != 0:
        fatal(f"kubectl config current-context failed: {out.stderr.strip()}")
    ctx = out.stdout.strip()
    if not ctx.startswith("gke_neon-481711_"):
        fatal(f"kubectl current-context={ctx!r} is NOT gke_neon-481711_*")
    return ctx


def capture_cluster_baseline(ctx_name: str) -> dict:
    """Capture pre-ship cluster metadata for the baseline header.

    Per `feedback_kubectl_verify_gke_context`, every kubectl/helm call
    here is explicit --context-flagged.
    """
    def _helm_rev(release: str) -> str:
        try:
            out = subprocess.run(
                ["helm", f"--kube-context={GKE_CONTEXT}", "-n", "krateo-system",
                 "history", release, "--max", "1", "-o", "json"],
                capture_output=True, text=True, timeout=30,
            )
            if out.returncode != 0:
                return f"<helm-history-err: {out.stderr.strip()}>"
            hist = json.loads(out.stdout or "[]")
            if not hist:
                return "<no-history>"
            last = hist[-1]
            return f"rev={last.get('revision', '?')} chart={last.get('chart', '?')}"
        except Exception as e:
            return f"<exc: {e}>"

    def _snowplow_image() -> str:
        try:
            out = subprocess.run(
                ["kubectl", f"--context={GKE_CONTEXT}", "-n", "krateo-system",
                 "get", "pod", "-l", "app.kubernetes.io/name=snowplow",
                 "-o", "jsonpath={.items[0].spec.containers[*].image}"],
                capture_output=True, text=True, timeout=30,
            )
            if out.returncode != 0:
                return f"<err: {out.stderr.strip()}>"
            return out.stdout.strip()
        except Exception as e:
            return f"<exc: {e}>"

    def _compositions_count() -> int:
        try:
            from bench.cluster import count_compositions  # type: ignore
            return count_compositions()
        except Exception as e:
            print(f"F5: count_compositions failed: {e}", file=sys.stderr)
            return -1

    return {
        "snowplow_helm": _helm_rev("snowplow"),
        "snowplow_image": _snowplow_image(),
        "portal_helm": _helm_rev("portal"),
        "kubectl_context": ctx_name,
        "compositions_cluster_truth": _compositions_count(),
    }


def measure_user(page, user: str, password: str) -> dict:
    """Measure one user across /dashboard + /compositions.

    Returns a dict shaped:
        {
          "login_ok": True,
          "auth_token_obtained": True,
          "dashboard": { ... },
          "compositions": { ... }
        }

    Raises RuntimeError on login failure or measurement gap.
    """
    out: dict = {"user": user}

    # Step 1: token via HTTP login (validates auth path independent
    # of the browser).
    try:
        token = browser.login(user, password)
    except Exception as e:
        raise RuntimeError(f"user={user} login(HTTP) raised: {e}")
    if not token:
        raise RuntimeError(f"user={user} login(HTTP) returned empty token")
    out["auth_token_obtained"] = True

    # Step 2: piechart count via API (independent of browser context).
    api_count = browser.verify_composition_count_api(token)
    if api_count < 0:
        raise RuntimeError(
            f"user={user} verify_composition_count_api returned {api_count} "
            f"(expected non-negative integer)")

    # Step 3: Playwright login + per-page navigation + UI piechart count.
    if not browser.browser_login(page, user, password):
        raise RuntimeError(f"user={user} browser_login failed")
    out["login_ok"] = True

    for label, path in browser.BROWSER_PAGES:
        page_key = label.lower()
        page_out: dict = {"path": path, "label": label}
        try:
            nav_result = browser.browser_measure_navigation(
                page, path, label, min_calls=0, user=user,
            )
            page_out["nav_call_count"] = nav_result.get("callCount", -1)
            page_out["load_complete"] = nav_result.get("loadComplete", False)
            page_out["validation"] = nav_result.get("validation", {})
            page_out["incomplete"] = nav_result.get("incomplete", False)
        except Exception as e:
            raise RuntimeError(
                f"user={user} page={label} browser_measure_navigation raised: {e}")

        # piechart counts AFTER navigation has settled.
        page_out["piechart_count_api"] = browser.verify_composition_count_api(token)
        page_out["piechart_count_ui"] = browser.verify_composition_count_ui(page)
        if page_out["piechart_count_api"] < 0 or page_out["piechart_count_ui"] < 0:
            raise RuntimeError(
                f"user={user} page={label} piechart count not captured "
                f"(api={page_out['piechart_count_api']}, "
                f"ui={page_out['piechart_count_ui']})")

        # API-vs-UI cross-check (within-snapshot consistency).
        if page_out["piechart_count_api"] != page_out["piechart_count_ui"]:
            print(f"F5 WARN: user={user} page={label} API/UI count mismatch — "
                  f"api={page_out['piechart_count_api']} "
                  f"ui={page_out['piechart_count_ui']}", file=sys.stderr)

        out[page_key] = page_out

    return out


def main() -> int:
    parser = argparse.ArgumentParser(
        description="F5 pre-ship baseline capture (Ship 0.30.242 H.c-layered)")
    parser.add_argument(
        "--out", default=None,
        help="output JSON path (default: f5_preship_baseline_<YYYY-MM-DD>.json "
             "in the script directory)")
    args = parser.parse_args()

    print("F5 — Chrome customer-probe pre-ship baseline (Ship 0.30.242 H.c-layered)")
    print()

    # Guardrail: GKE context.
    ctx_name = verify_kubectl_context()
    print(f"F5 cluster context: {ctx_name}")

    # Capture cluster baseline metadata.
    print("F5 capturing cluster baseline metadata ...")
    cluster_baseline = capture_cluster_baseline(ctx_name)
    print(f"F5 baseline: snowplow={cluster_baseline['snowplow_helm']} "
          f"portal={cluster_baseline['portal_helm']} "
          f"image={cluster_baseline['snowplow_image']} "
          f"compositions_cluster_truth={cluster_baseline['compositions_cluster_truth']}")

    # Load user credentials.
    users = browser._ensure_users()
    print(f"F5 users loaded: {sorted(users.keys())}")
    if set(users.keys()) != {"admin", "cyberjoker"}:
        fatal(f"unexpected users dict: {list(users.keys())} (expected admin + cyberjoker)")

    # Playwright session per user.
    from playwright.sync_api import sync_playwright

    measurements: dict = {}
    start = time.time()
    with sync_playwright() as p:
        b = p.chromium.launch(headless=True)
        try:
            for user in ("admin", "cyberjoker"):
                print(f"F5 measuring user={user} ...")
                ctx = b.new_context()
                try:
                    page = ctx.new_page()
                    measurements[user] = measure_user(page, user, users[user])
                    print(f"F5 user={user} OK: "
                          f"dashboard_piechart={measurements[user]['dashboard']['piechart_count_api']} "
                          f"compositions_piechart={measurements[user]['compositions']['piechart_count_api']}")
                except RuntimeError as e:
                    fatal(f"measurement failed for user={user}: {e}")
                finally:
                    ctx.close()
        finally:
            b.close()

    duration = time.time() - start
    print(f"F5 measurement duration: {duration:.1f}s")

    # Compose baseline JSON.
    captured_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    baseline = {
        "captured_at": captured_at,
        "captured_by": "f5_preship_baseline.py (Ship 0.30.242 H.c-layered Phase 3 F5)",
        "captured_against": "pre-ship cluster (snowplow image 0.30.235 + chart 0.30.243 + portal rev 33)",
        "cluster_baseline": cluster_baseline,
        "ship_plan_invariant": (
            "F5 measures customer-visible integer counts (piechart composition "
            "count via API + UI), NOT raw wire responses. Wire-level diff would "
            "false-positive on metadata churn (resourceVersion, timestamps). F5 "
            "integer-equality + F1-F8 orthogonal coverage is the customer-"
            "correctness contract. Post-ship Phase 5 MUST match these integer "
            "counts byte-equivalently per (user, page) tuple; any drift triggers "
            "HARD REVERT."),
        "measurements": measurements,
        "duration_seconds": round(duration, 2),
    }

    # Emit JSON.
    out_path = args.out
    if not out_path:
        out_path = SCRIPT_DIR / f"f5_preship_baseline_{captured_at[:10]}.json"
    else:
        out_path = Path(out_path)
    out_path.write_text(json.dumps(baseline, indent=2, sort_keys=True) + "\n")
    print(f"F5 baseline written: {out_path}")

    # Summary table.
    print()
    print("F5 SUMMARY:")
    for user, m in sorted(measurements.items()):
        for page in ("dashboard", "compositions"):
            p = m.get(page, {})
            print(f"  {user:11s} {page:14s}  piechart(api)={p.get('piechart_count_api', '?'):5} "
                  f"piechart(ui)={p.get('piechart_count_ui', '?'):5} "
                  f"nav_calls={p.get('nav_call_count', '?')}")
    print()
    print("F5 PASS GATE: baseline captured. Phase 5 MUST diff post-ship "
          "measurements against this JSON; any per-(user, page) integer "
          "drift triggers HARD REVERT.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
