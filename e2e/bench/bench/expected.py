"""EXPECTED_CALLS calibration overlay.

Tolerance = 0 per Diego 2026-06-02 (was 1 at the worktree source line 212).
Overlay freshness gate refuses to start when
`/tmp/snowplow-runs/calibration/expected_calls.json` is older than 14 days;
operator must re-run `python -m bench calibrate` or set
`--allow-stale-overlay` (escape hatch with a very loud log).

Per docs/bench-restructure-path-b-plan-2026-06-02.md §A.6.

Calibrated 2026-05-12.
"""

from __future__ import annotations

import json
import math
import os
import re
import sys
import time


__all__ = [
    "EXPECTED_CALLS",
    "EXPECTED_CALLS_DEFAULT_USER",
    "EXPECTED_CALLS_TOLERANCE",
    "EXPECTED_CALLS_OVERLAY_PATH",
    "OverlayStale",
    "load_expected_calls_overlay",
    "expected_calls",
    "overlay_age_seconds",
    "overlay_freshness_or_die",
    "gate_overlay_freshness",
    "run_calibrate_expected_calls",
]


# ─── Expected /call counts per (user, page) ─────────────────────────────────
#
# Calibrated 2026-05-12 against the GKE neon-481711 cluster. Both users sit
# at the structural ceiling (16 dashboard / 10 compositions) when the
# apiserver is not throttling cluster-scoped LISTs. Admin's overload-induced
# 9/6 floor at 50K compositions is a regression to detect, NOT the
# expectation. See plan §A.6.

EXPECTED_CALLS = {
    # user -> { page_path -> expected /call count at structural ceiling }
    "admin": {
        "/dashboard":    16,
        "/compositions": 10,
    },
    "cyberjoker": {
        "/dashboard":    16,
        "/compositions": 10,
    },
}
EXPECTED_CALLS_DEFAULT_USER = "admin"  # fallback for unknown subjects

# Tolerance flipped from 1 → 0 per Diego's hard constraint 2026-06-02.
# Any per-page actual /call count != expected is a hard gate failure now.
# The operator MUST re-calibrate before the first scored run after this flip,
# or any prior overlay carrying an off-by-one drops the gate.
EXPECTED_CALLS_TOLERANCE = 0

EXPECTED_CALLS_OVERLAY_PATH = "/tmp/snowplow-runs/calibration/expected_calls.json"


# ─── Overlay loading + lookup ───────────────────────────────────────────────


def load_expected_calls_overlay():
    """Merge expected_calls.json (if present) over the hardcoded dict.

    The overlay file has the same shape as EXPECTED_CALLS: a dict keyed by
    user, each value a dict keyed by page_path. Missing users / pages in
    the overlay fall through to the hardcoded defaults; unknown users are
    appended (forward-compat for new subjects).

    Returns the merged dict. Caller is responsible for assigning back to
    the module-level EXPECTED_CALLS if it wants the overlay live.
    """
    merged = {u: dict(v) for u, v in EXPECTED_CALLS.items()}
    try:
        with open(EXPECTED_CALLS_OVERLAY_PATH, "r") as f:
            overlay = json.load(f)
    except FileNotFoundError:
        return merged
    except Exception as e:
        # Bad JSON, permission denied, etc. — log to stderr and fall back
        # to defaults. Never let a stale overlay file silently break the
        # gate.
        sys.stderr.write(
            f"  [WARN] could not load expected-calls overlay "
            f"{EXPECTED_CALLS_OVERLAY_PATH}: {type(e).__name__}: {e}\n"
        )
        return merged
    if not isinstance(overlay, dict):
        return merged
    for user, paths in overlay.items():
        if not isinstance(paths, dict):
            continue
        if user not in merged:
            merged[user] = {}
        for path, count in paths.items():
            if isinstance(count, int) and count >= 0:
                merged[user][path] = count
    return merged


def expected_calls(user, page_path):
    """Return the calibrated /call count for a (user, page) pair.

    Looks up the user's row in EXPECTED_CALLS, falling back to
    EXPECTED_CALLS_DEFAULT_USER when the subject is unknown (the harness
    should not silently skip the gate just because a new user was added;
    admin's expectations are the safest default). Returns None when the
    page itself is unknown — callers treat None as 'page not characterized
    yet, skip the gate'.
    """
    table = EXPECTED_CALLS.get(user)
    if table is None:
        table = EXPECTED_CALLS.get(EXPECTED_CALLS_DEFAULT_USER, {})
    return table.get(page_path)


# ─── Overlay freshness gate ─────────────────────────────────────────────────


class OverlayStale(Exception):
    """Raised when the EXPECTED_CALLS overlay is older than the operator
    threshold.

    Carries the overlay path + age in seconds for the caller to surface
    in the user-facing error.
    """

    def __init__(self, path: str, age_seconds: float, max_age_days: int):
        self.path = path
        self.age_seconds = age_seconds
        self.max_age_days = max_age_days
        if math.isinf(age_seconds):
            age_human = "missing"
        else:
            age_human = f"{age_seconds / 86400.0:.1f} days"
        super().__init__(
            f"EXPECTED_CALLS overlay stale: {path} (age {age_human}, "
            f"max {max_age_days} days). "
            f"run: python -m bench calibrate "
            f"(or pass --allow-stale-overlay to bypass with a loud log)."
        )


def _resolve_overlay_path(path: str | None) -> str:
    # Resolve the path FRESHLY from the module at call-time so
    # monkeypatch.setattr(expected_mod, "EXPECTED_CALLS_OVERLAY_PATH", ...)
    # reaches the stat call. A default like
    # `path: str = EXPECTED_CALLS_OVERLAY_PATH` would capture the value
    # at def-time and the monkeypatch would be silently bypassed.
    if path is not None:
        return path
    import sys as _sys
    mod = _sys.modules[__name__]
    return mod.EXPECTED_CALLS_OVERLAY_PATH


def overlay_age_seconds(path: str | None = None) -> float:
    """Return the overlay file's age in seconds (`math.inf` when missing).

    Source-of-truth for 'age' is the overlay file's `os.stat().st_mtime`,
    NOT any `captured_at` JSON field. Hand-edits to a JSON field cannot
    forge freshness; mtime is set by the kernel when the file is rewritten.
    """
    resolved = _resolve_overlay_path(path)
    try:
        st = os.stat(resolved)
    except FileNotFoundError:
        return math.inf
    except OSError:
        return math.inf
    return max(0.0, time.time() - st.st_mtime)


def overlay_freshness_or_die(max_age_days: int = 14,
                             path: str | None = None) -> None:
    """Raise OverlayStale when the overlay file is older than max_age_days.

    Source-of-truth for 'age' is the overlay file's `os.stat().st_mtime`,
    NOT any `captured_at` JSON field. Hand-edits to a JSON field cannot
    forge freshness; mtime is set by the kernel when the file is rewritten.

    Per acceptance criterion (g) in plan §H + tightening #4. Operator-facing
    message in the OverlayStale exception points to `python -m bench
    calibrate` so the recovery path is one copy-paste away.
    """
    resolved = _resolve_overlay_path(path)
    age = overlay_age_seconds(resolved)
    max_age_seconds = max_age_days * 86400.0
    if age > max_age_seconds:
        raise OverlayStale(path=resolved, age_seconds=age,
                           max_age_days=max_age_days)


def gate_overlay_freshness():
    """Soft gate: warn when the overlay drifts from the hardcoded defaults.

    Today's tolerance=0 absorbs no jitter; flipping from the prior
    tolerance=1 means any prior overlay carrying an off-by-one drops the
    gate. This helper compares the overlay's nav-by-nav values against
    the hardcoded EXPECTED_CALLS defaults; if any value differs by >0 AND
    the overlay is older than zero days, prints a one-line "stale overlay
    vs defaults" warning to stderr.

    Returns True on no warning (defaults match overlay or overlay missing);
    False when a divergence was reported. Caller decides whether to block
    on the return value — current bench wiring treats this as advisory.

    Per plan §I R2.1 mitigation.
    """
    try:
        merged = load_expected_calls_overlay()
    except Exception as e:
        sys.stderr.write(
            f"  [WARN] gate_overlay_freshness: load failed "
            f"({type(e).__name__}: {e}); proceeding with hardcoded defaults\n"
        )
        return True

    any_drift = False
    for user, default_paths in EXPECTED_CALLS.items():
        merged_paths = merged.get(user, {})
        for path, default_count in default_paths.items():
            actual = merged_paths.get(path)
            if actual is None:
                continue
            if actual != default_count:
                sys.stderr.write(
                    f"  [WARN] overlay vs default drift: user={user!r} "
                    f"path={path!r} overlay={actual} default={default_count}\n"
                )
                any_drift = True
    return not any_drift


# ─── Calibration ────────────────────────────────────────────────────────────


def run_calibrate_expected_calls():
    """Probe the current cluster to refresh EXPECTED_CALLS.

    Logs in as each known user, performs a single cold navigation of
    /dashboard and /compositions, and records the resulting /call count.
    Writes the result to EXPECTED_CALLS_OVERLAY_PATH so subsequent bench
    runs pick up the calibrated values without a code change.

    Re-run this when:
      * The widget set changes (a new dashboard widget is added /
        removed). The structural ceiling shifts; the gate without
        re-calibration will produce false positives.
      * A new user subject is added. EXPECTED_CALLS already falls back
        to admin for unknown users, but calibrating the new subject
        directly gives narrower gates.
      * After a major frontend release that may change the widget set
        layout (NavMenu, RouteLoader, etc.).

    The function is intentionally non-destructive — it does NOT clean the
    cluster, deploy compositions, or modify any state. It expects the
    operator to bring the cluster into the desired calibration shape
    (typically: 0 compositions, 0 bench namespaces, snowplow pod
    restarted) before invoking.

    NOTE: depends on Block-3 browser.py helpers (`_browser_login`,
    `_browser_measure_navigation`). The imports are deferred so this
    module imports cleanly during Block 2.
    """
    # Deferred imports — these resolve in Block 3+.
    from bench.cli import log, section  # noqa
    try:
        # Block 3 will ship `bench.browser`. Until then, calibration is
        # not runnable, but the rest of expected.py works.
        from bench.browser import (  # type: ignore
            _browser_login,
            _browser_measure_navigation,
        )
    except ImportError as e:
        sys.stderr.write(
            f"  [ERROR] run_calibrate_expected_calls requires bench.browser "
            f"(Block 3): {type(e).__name__}: {e}\n"
        )
        return 2

    # FRONTEND / USERS come from the source script's globals. For now read
    # them from environment so this function can run pre-Block 3 if the
    # operator wires the browser helpers manually.
    frontend = os.environ.get("FRONTEND_URL", "").strip() or None
    if not frontend:
        sys.stderr.write("  FRONTEND_URL not set — calibration requires a browser\n")
        return 2
    try:
        from playwright.sync_api import sync_playwright  # type: ignore
    except ImportError as e:
        sys.stderr.write(
            f"  Playwright import failed: {type(e).__name__}: {e}\n"
        )
        return 2

    # Per-user credentials. Until browser.py ships its credential helper,
    # require the operator to set BENCH_CALIBRATE_USERS as a JSON map.
    creds_raw = os.environ.get("BENCH_CALIBRATE_USERS", "").strip()
    if not creds_raw:
        sys.stderr.write(
            "  BENCH_CALIBRATE_USERS not set — expected JSON map "
            "{user: password} (operator-only setting)\n"
        )
        return 2
    try:
        creds = json.loads(creds_raw)
    except json.JSONDecodeError as e:
        sys.stderr.write(
            f"  BENCH_CALIBRATE_USERS not valid JSON: {e}\n"
        )
        return 2

    pages_to_probe = [("/dashboard", "Dashboard"),
                      ("/compositions", "Compositions")]

    calibrated = {}
    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True)
        for user_name, password in creds.items():
            ctx = browser.new_context(viewport={"width": 1280, "height": 900},
                                      ignore_https_errors=True)
            page = ctx.new_page()
            if not _browser_login(page, user_name, password):
                sys.stderr.write(
                    f"  login failed for {user_name!r}; skipping calibration\n"
                )
                ctx.close()
                continue
            calibrated[user_name] = {}
            for path, label in pages_to_probe:
                # COLD navigation: clear timings, then nav with the same
                # stability poll the bench uses. Reuse the production
                # measure helper so calibration sees what the gate sees.
                m = _browser_measure_navigation(
                    page, path, f"calibrate {user_name} {label}",
                    min_calls=0, user=user_name)
                actual = (m.get("validation") or {}).get("actual_calls")
                if actual is None:
                    actual = m.get("callCount", 0)
                calibrated[user_name][path] = int(actual)
            ctx.close()
        browser.close()

    # Write the overlay file so the next bench run merges these values
    # over the hardcoded defaults. We keep the hardcoded dict as the
    # provenance-of-record (last known good); the overlay is a fast path
    # for refresh between code changes.
    try:
        os.makedirs(os.path.dirname(EXPECTED_CALLS_OVERLAY_PATH), exist_ok=True)
        with open(EXPECTED_CALLS_OVERLAY_PATH, "w") as f:
            json.dump(calibrated, f, indent=2, sort_keys=True)
    except Exception as e:
        sys.stderr.write(
            f"  [WARN] could not write overlay file: {type(e).__name__}: {e}\n"
        )
        return 1
    return 0
