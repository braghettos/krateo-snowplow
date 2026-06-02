"""Playwright driver — login, navigation, validation, convergence poll,
video recording.

The harness uses Playwright exclusively (no Chrome MCP); video → GIF
post-processing via ffmpeg. NEVER uses kubectl-mediated paths in scored
measurement (see feedback_no_kubectl_in_measurement.md).

Per docs/bench-restructure-path-b-plan-2026-06-02.md §A.5 (Block 3).

Surface
-------
HTTP helpers (no Playwright dependency):
    login, login_all, http_get, http_get_json, http_get_with_headers,
    cache_metrics

Playwright-based helpers (require playwright.sync_api):
    browser_login (alias: _browser_login)
    browser_measure_navigation (alias: _browser_measure_navigation)
    browser_measure_stage (alias: _browser_measure_stage)
    verify_composition_count_api / verify_composition_count_ui
    make_browser_context, record_video_to_gif

Convergence:
    ConvergenceTimeout  (raised by browser_measure_stage on VERIFY poll
                         deadline — replaces the source-script's silent
                         TIMEOUT/MISMATCH log at worktree line 6465-6475)

Constants:
    WIDGET_ENDPOINTS, RESTACTION_ENDPOINTS, ALL_ENDPOINTS, BROWSER_PAGES,
    BROWSER_NAV_PAGES, BROWSER_SCALING_PAGES

Dependencies: playwright.sync_api (optional — only loaded when caller hits
a Playwright-based helper), urllib, gzip, json, base64, subprocess,
bench.cluster (count_compositions, list_composition_names + kubectl for
user-secret reads), bench.expected (_expected_calls + EXPECTED_CALLS_TOLERANCE
via deferred import to avoid circular import during partial migration).
Per plan §G.0: deferred-import shims absorb ~60 LOC.
"""

from __future__ import annotations

import base64
import gzip
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


__all__ = [
    # HTTP helpers
    "login",
    "login_all",
    "http_get",
    "http_get_json",
    "http_get_with_headers",
    "cache_metrics",
    "get_runtime_metrics",
    # Constants
    "WIDGET_ENDPOINTS",
    "RESTACTION_ENDPOINTS",
    "ALL_ENDPOINTS",
    "BROWSER_PAGES",
    "BROWSER_NAV_PAGES",
    "BROWSER_SCALING_PAGES",
    # User credentials
    "USERS",
    # Convergence-timeout
    "ConvergenceTimeout",
    # Playwright-based helpers
    "browser_login",
    "browser_measure_navigation",
    "browser_measure_stage",
    "verify_composition_count_api",
    "verify_composition_count_ui",
    "make_browser_context",
    "record_video_to_gif",
    # L1 / compositions waiters (browser-context callers)
    "wait_for_compositions",
    "wait_for_l1_ready",
    "wait_for_l1_warmup",
]


# ─── Environment / endpoints ────────────────────────────────────────────────

SNOWPLOW = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")
AUTHN = os.environ.get("AUTHN_URL", "http://34.136.84.51:8082")
FRONTEND = os.environ.get("FRONTEND_URL", "http://34.46.217.105:8080") or None
SCREENSHOTS = os.environ.get("SCREENSHOTS", "0") == "1"

# Iteration counts — kept at module level so callers / tests can monkeypatch.
ITERS = int(os.environ.get("ITERS", "10"))


# ─── Deferred-import shims (per plan §G.0 cost #1) ──────────────────────────


def _log(msg):
    """Local log shim — replaced by cli.py logger when present."""
    try:
        from bench.cli import log
        log(msg)
    except ImportError:
        ts = time.strftime("%H:%M:%S")
        print(f"  [{ts}] {msg}", flush=True)


def _section(title):
    """Local section shim — replaced by cli.py logger when present."""
    try:
        from bench.cli import section
        section(title)
    except ImportError:
        print(f"\n  === {title} ===", flush=True)


def _count_compositions():
    """Defer to bench.cluster (sole cluster-state probe used by VERIFY poll)."""
    from bench.cluster import count_compositions  # type: ignore
    return count_compositions()


def _count_bench_ns():
    """Defer to bench.cluster (used by browser_measure_stage header log)."""
    from bench.cluster import count_bench_ns  # type: ignore
    return count_bench_ns()


def _list_composition_names():
    """Defer to bench.cluster (CONTENT-correctness check post-VERIFY)."""
    from bench.cluster import list_composition_names  # type: ignore
    return list_composition_names()


def _pct(data, p):
    """Defer to bench.cluster (percentile helper used in waterfall stats)."""
    from bench.cluster import pct  # type: ignore
    return pct(data, p)


def _expected_calls_lookup(user, page_path):
    """Defer to bench.expected — lazy import to avoid pulling expected.py
    at module load (keeps the import surface lean for tests that only
    exercise the HTTP helpers).
    """
    from bench.expected import expected_calls  # type: ignore
    return expected_calls(user, page_path)


def _expected_calls_tolerance():
    """Defer to bench.expected (tolerance is a constant but threading
    through a getter keeps tests that monkeypatch the constant honest)."""
    from bench.expected import EXPECTED_CALLS_TOLERANCE  # type: ignore
    return EXPECTED_CALLS_TOLERANCE


# ─── User credentials — read from in-cluster secrets at first use ───────────


NS = os.environ.get("BENCH_NS", "krateo-system")


def _read_password_from_secret(secret_name, ns=NS):
    """Read a password from a Kubernetes Secret's `password` data key.

    Replaces hardcoded constants. Hardcoded values drift the moment chart
    helpers regenerate the secrets at install time, then every login()
    returns 401 and the bench produces structurally identical numbers
    that have nothing to do with cache state.

    Returns the decoded password string. Raises RuntimeError if the
    secret is missing or malformed -- the bench MUST not silently fall
    back to a stale literal.
    """
    proc = subprocess.run(
        ["kubectl", "get", "secret", secret_name, "-n", ns,
         "-o", "jsonpath={.data.password}"],
        capture_output=True, text=True, timeout=30,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"could not read secret {secret_name} in namespace {ns}: "
            f"{proc.stderr.strip()}")
    encoded = (proc.stdout or "").strip()
    if not encoded:
        raise RuntimeError(
            f"secret {secret_name} in namespace {ns} has empty data.password")
    try:
        return base64.b64decode(encoded).decode("utf-8").rstrip("\n")
    except Exception as e:
        raise RuntimeError(
            f"could not base64-decode data.password from secret "
            f"{secret_name}: {type(e).__name__}: {e}")


def _build_users_dict():
    """Build USERS from in-cluster secrets at first credential read.

    Lazy-evaluated so unit tests and `python -m bench --help` do not
    require kubectl access.
    """
    return {
        "admin": _read_password_from_secret("admin-password"),
        "cyberjoker": _read_password_from_secret("cyberjoker-password"),
    }


# USERS is populated lazily by _ensure_users() on first credential read.
USERS: dict[str, str] = {}


def _ensure_users():
    """Populate USERS on first credential read."""
    global USERS
    if not USERS:
        USERS = _build_users_dict()
    return USERS


# ─── HTTP helpers ───────────────────────────────────────────────────────────


def _decompress(body, headers=None):
    """Decompress gzip response body if needed."""
    if headers and headers.get("Content-Encoding", "").lower() == "gzip":
        try:
            return gzip.decompress(body)
        except Exception:
            pass
    # Also try if it looks like gzip magic bytes
    if body[:2] == b"\x1f\x8b":
        try:
            return gzip.decompress(body)
        except Exception:
            pass
    return body


def login(username, password):
    """POST basic-auth credentials to AUTHN, return the JWT accessToken.

    Raises urllib.error.URLError / RuntimeError on failure. Callers
    (login_all + bench calibrate) wrap with retries.
    """
    creds = base64.b64encode(f"{username}:{password}".encode()).decode()
    req = urllib.request.Request(
        AUTHN + "/basic/login",
        headers={"Authorization": "Basic " + creds},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["accessToken"]


def login_all():
    """Log in every USER and return {username: token}.

    Failed logins skip the user (with a logged WARNING). Returns the
    populated dict; if every login fails the dict is empty (caller must
    decide what to do).
    """
    tokens = {}
    for username, password in _ensure_users().items():
        for attempt in range(5):
            try:
                tokens[username] = login(username, password)
                _log(f"{username}: JWT acquired")
                break
            except Exception as e:
                if attempt < 4:
                    time.sleep(3)
                else:
                    _log(f"{username}: login FAILED — {e}")
    return tokens


def http_get(path, token, base_url=None, timeout=120, retries=3, trace_id=None):
    """GET `path` against snowplow with Bearer auth + gzip decode.

    Returns (elapsed_ms, http_code, body_bytes). Retries up to `retries`
    times on connection failure (code==0); HTTP errors return their
    status code without retry.

    `trace_id`, when set, is sent as the `X-Krateo-TraceId` header — per
    plumbing@v0.9.3/server/use/traceid.go:14-18 the server ADOPTS the
    client-supplied header, so callers can correlate logs via the same
    id (used by bench.verify_serve_stale).
    """
    url = (base_url or SNOWPLOW) + path
    headers = {"Authorization": "Bearer " + token, "Accept-Encoding": "gzip"}
    if trace_id:
        headers["X-Krateo-TraceId"] = trace_id
    req = urllib.request.Request(url, headers=headers)
    elapsed_ms = 0
    code = 0
    body = b""
    for attempt in range(retries):
        t0 = time.perf_counter()
        code, body = 0, b""
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                raw = r.read()
                code = r.status
                body = _decompress(raw, dict(r.headers))
        except urllib.error.HTTPError as e:
            code = e.code
            try:
                raw = e.read()
                body = _decompress(raw, dict(e.headers) if hasattr(e, "headers") else None)
            except Exception:
                pass
        except Exception:
            code = 0
        elapsed_ms = int((time.perf_counter() - t0) * 1000)
        if code != 0:
            return elapsed_ms, code, body
        if attempt < retries - 1:
            _log(f"    HTTP 0, retry {attempt + 2}/{retries} in 3s ...")
            time.sleep(3)
    return elapsed_ms, 0, body


def http_get_json(path, token, **kw):
    """Like http_get but JSON-decodes the body."""
    ms, code, body = http_get(path, token, **kw)
    try:
        return ms, code, json.loads(body)
    except Exception:
        return ms, code, None


def http_get_with_headers(path, token, base_url=None, timeout=120, trace_id=None):
    """Like http_get but also returns response headers as a dict.

    `trace_id`, when set, is sent as the `X-Krateo-TraceId` header (same
    semantics as http_get).
    """
    url = (base_url or SNOWPLOW) + path
    headers = {"Authorization": "Bearer " + token, "Accept-Encoding": "gzip"}
    if trace_id:
        headers["X-Krateo-TraceId"] = trace_id
    req = urllib.request.Request(url, headers=headers)
    t0 = time.perf_counter()
    code, body, hdrs = 0, b"", {}
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            code = r.status
            hdrs = dict(r.headers)
            body = _decompress(raw, hdrs)
    except urllib.error.HTTPError as e:
        code = e.code
        hdrs = dict(e.headers) if hasattr(e, "headers") else {}
    except Exception:
        pass
    elapsed_ms = int((time.perf_counter() - t0) * 1000)
    return elapsed_ms, code, body, hdrs


def cache_metrics(token):
    """Fetch /metrics/cache as JSON (used to estimate L1-ready sentinel)."""
    _, _, body = http_get_json("/metrics/cache", token)
    return body or {}


def get_runtime_metrics():
    """Fetch /metrics/runtime from snowplow. Returns dict or None on error.

    Unauthenticated probe (legacy snowplow_test.py behaviour).
    """
    try:
        req = urllib.request.Request(SNOWPLOW + "/metrics/runtime")
        with urllib.request.urlopen(req, timeout=10) as r:
            return json.loads(r.read())
    except Exception:
        return None


def _read_l1_ready_ts():
    """Read the L1-ready timestamp proxy. Uses /metrics/cache l1_hits as a
    monotonic freshness signal.
    """
    m = cache_metrics("")
    return int(time.time()) if m.get("l1_hits", 0) > 0 else 0


# ─── Endpoint catalogues ────────────────────────────────────────────────────


WIDGET_ENDPOINTS = [
    ("page/dashboard",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=dashboard-page&namespace=krateo-system"),
    ("page/blueprints",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=blueprints-page&namespace=krateo-system"),
    ("page/compositions",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=compositions-page&namespace=krateo-system"),
    ("navmenu/sidebar",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=navmenus&name=sidebar-nav-menu&namespace=krateo-system"),
    ("routes/loader",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=routesloaders&name=routes-loader&namespace=krateo-system"),
]


RESTACTION_ENDPOINTS = [
    ("restaction/all-routes",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=all-routes&namespace=krateo-system"),
    ("restaction/bp-list",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=blueprints-list&namespace=krateo-system"),
    ("restaction/comp-list",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-list&namespace=krateo-system"),
]


ALL_ENDPOINTS = WIDGET_ENDPOINTS + RESTACTION_ENDPOINTS


BROWSER_PAGES = [
    ("Dashboard", "/dashboard"),
    ("Compositions", "/compositions"),
]


BROWSER_NAV_PAGES = [
    ("Dashboard", "/dashboard"),
    ("Compositions", "/compositions"),
]


BROWSER_SCALING_PAGES = [
    ("Dashboard", "/dashboard"),
    ("Compositions", "/compositions"),
]


# ─── L1 + compositions waiters (used by browser-side callers) ───────────────


def wait_for_l1_warmup(timeout=300):
    """Poll snowplow /metrics/runtime + pod logs for L1 warmup completion.

    Returns True when warmup is detected, False on timeout. Behaviour
    matches the source script's `wait_for_l1_warmup` (worktree line 1000):
    primary signal is `cache_key_count > 0` on /metrics/runtime; fallback
    scans pod logs for the warmup-completed sentinel.
    """
    from bench.cluster import kubectl  # deferred — cluster is lifecycle-side

    _log("Waiting for L1 warmup ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        rt = get_runtime_metrics()
        if rt:
            keys = rt.get("cache_key_count", 0)
            if keys > 0:
                _log(f"L1 warmup detected ({keys} cache keys)")
                return True

        rc, out, _ = kubectl("logs", "deployment/snowplow", "-n", NS,
                             "-c", "snowplow", "--tail=500")
        if rc == 0:
            if "warmup: completed" in out or "warmup: pre-warm disabled" in out:
                _log("L1 warmup completed (log)")
                return True
            if "warmup: skipped" in out or "warmup: no users found" in out:
                _log("L1 warmup skipped (log)")
                return True

        time.sleep(5)
    _log("WARNING: L1 warmup not detected within timeout")
    return False


def wait_for_l1_ready(since_epoch=None, timeout=120):
    """Wait until the L1-ready sentinel is strictly > since_epoch.

    The sentinel is derived from /metrics/cache l1_hits (the legacy proxy
    used by the source script). When `since_epoch` is None, the current
    sentinel is snapshotted and we wait for it to change.
    """
    if since_epoch is None:
        since_epoch = _read_l1_ready_ts()
        _log(f"Waiting for L1 ready (current sentinel={since_epoch}) ...")
    else:
        _log(f"Waiting for L1 ready (since={since_epoch}) ...")

    deadline = time.time() + timeout
    while time.time() < deadline:
        ts = _read_l1_ready_ts()
        if ts > since_epoch:
            _log(f"L1 ready (sentinel={ts})")
            return True
        time.sleep(2)

    _log(f"WARNING: L1 not ready within {timeout}s "
         f"(sentinel={_read_l1_ready_ts()}, need >{since_epoch})")
    return False


def wait_for_compositions(expected, timeout=300, tolerance=5):
    """Wait until at least `expected - tolerance` compositions exist in
    the cluster.

    Returns True on success, False on timeout. NEVER raises — Block 3
    callers (Block 4's phases.py) check the return code.

    NOTE: this is the LEGACY helper retained for the run_user_scaling
    flow that lives in storm.py. The Block 4 stage runner's verify-step
    against partial state is the new ConvergenceTimeout-emitting helper
    inside `browser_measure_stage`.
    """
    _log(f"Waiting for {expected} compositions to exist (tolerance={tolerance}) ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        actual = _count_compositions()
        if actual >= expected:
            _log(f"All {actual} compositions exist")
            return True
        if actual >= expected - tolerance:
            _log(f"All {actual}/{expected} compositions exist (within tolerance)")
            return True
        elapsed = int(time.time() - (deadline - timeout))
        if elapsed % 30 < 5:
            _log(f"  {actual}/{expected} compositions exist ({elapsed}s elapsed)")
        time.sleep(5)
    actual = _count_compositions()
    _log(f"WARNING: only {actual}/{expected} compositions after {timeout}s")
    return False


def list_composition_names_from_cache(token):
    """Return the set of `ns/name` strings reported by compositions-list.

    Used by browser_measure_stage for the post-VERIFY CONTENT match. None
    on transport/parse failure (caller silently skips CONTENT check).
    """
    try:
        _ms, code, body = http_get_json(
            '/call?apiVersion=templates.krateo.io%2Fv1'
            '&resource=restactions'
            '&name=compositions-list'
            '&namespace=krateo-system',
            token,
        )
        if code != 200 or not isinstance(body, dict):
            return None
        items = (body.get("status") or {}).get("list") or []
        names = set()
        for item in items:
            if not isinstance(item, dict):
                continue
            ns = item.get("ns", "")
            name = item.get("name", "")
            if not ns or not name:
                md = item.get("metadata") or {}
                ns = ns or md.get("namespace", "")
                name = name or md.get("name", "")
            if ns and name:
                names.add(f"{ns}/{name}")
        return names
    except Exception:
        return None


# ─── ConvergenceTimeout exception (new — Block 3) ───────────────────────────


class ConvergenceTimeout(Exception):
    """Raised when VERIFY poll's deadline expires with matched=False.

    Replaces today's silent skip at source 6465-6475 (worktree
    snowplow_test.py). Caller MUST NOT silently swallow. Block 4's stage
    runner catches this, writes a stage proof with `passed=False` AND
    `convergence_timeout=true`, persists state.json, and re-raises to
    abort the run with exit 4.

    Fields enable post-mortem diagnostics:
      stage    — stage number that failed convergence (1..8)
      user     — which user's navigation timed out (admin/cyberjoker)
      api      — what /call piechart API saw last
      ui       — what the UI fetch saw last
      cluster  — what `kubectl count_compositions` reports as ground truth
      timeout_secs — the deadline that was exceeded (default 300s)
    """

    def __init__(self, *, stage, user, api, ui, cluster, timeout_secs=None):
        self.stage = stage
        self.user = user
        self.api = api
        self.ui = ui
        self.cluster = cluster
        self.timeout_secs = timeout_secs
        api_str = str(api) if api is not None and api >= 0 else "?"
        ui_str = str(ui) if ui is not None and ui >= 0 else "?"
        timeout_str = f"{timeout_secs}s" if timeout_secs is not None else "deadline"
        super().__init__(
            f"convergence timeout: stage=S{stage} user={user!r} "
            f"api={api_str} ui={ui_str} cluster={cluster} "
            f"(deadline {timeout_str} expired with matched=False)"
        )


# ─── Browser context + video → GIF (new — Block 3) ──────────────────────────


def make_browser_context(playwright_browser, *,
                         record_video_dir: Path | None = None,
                         viewport=(1280, 900),
                         ignore_https_errors: bool = True):
    """Construct a Playwright BrowserContext with optional video recording.

    `record_video_dir` is per cell representative only (n=1 per stage,
    user, cache_mode, page) — see plan §I R3.1. When set, the directory
    is created if missing and passed to `browser.new_context(...)`.
    Subsequent samples for the same cell should pass `record_video_dir=None`
    to keep the run bundle inside the 200 MB cap.

    `viewport` is a (width, height) tuple; converted to Playwright's
    {"width", "height"} dict at the call site so callers don't depend
    on Playwright's exact dict shape.

    Returns the new BrowserContext object.
    """
    kwargs = {
        "viewport": {"width": int(viewport[0]), "height": int(viewport[1])},
        "ignore_https_errors": ignore_https_errors,
    }
    if record_video_dir is not None:
        rvd = Path(record_video_dir)
        rvd.mkdir(parents=True, exist_ok=True)
        kwargs["record_video_dir"] = str(rvd)

    return playwright_browser.new_context(**kwargs)


def record_video_to_gif(webm_path: Path, gif_path: Path,
                        *, fps: int = 4, max_seconds: int = 60,
                        max_mb: int = 2) -> bool:
    """Convert a Playwright .webm video to .gif via ffmpeg.

    Returns True on success, False when:
      - ffmpeg is not on PATH (operator missing toolchain)
      - the source .webm does not exist
      - the produced .gif exceeds `max_mb` (caller deletes via R3.1 cap)
      - ffmpeg returns non-zero exit

    Per plan §I R3.1 — on bundle-cap exceedance the caller truncates
    oldest .webm/.gif pairs and emits a `BUNDLE TRUNCATED` log line.
    This helper itself NEVER raises; the run continues with the .webm
    only.

    Filter graph: `fps={fps},scale=720:-1:flags=lanczos`. 4 fps × 60s ≈
    240 frames at 720p ≈ ~1-1.5 MB per video on the canonical dashboard
    nav.
    """
    webm = Path(webm_path)
    gif = Path(gif_path)

    if shutil.which("ffmpeg") is None:
        _log(f"  record_video_to_gif: ffmpeg not on PATH — skipping {webm.name}")
        return False

    if not webm.exists():
        _log(f"  record_video_to_gif: source missing {webm} — skipping")
        return False

    gif.parent.mkdir(parents=True, exist_ok=True)
    # Cap clip length at max_seconds via `-t` so a runaway recording
    # doesn't produce a 50 MB .gif on a slow-render cell.
    cmd = [
        "ffmpeg",
        "-y",                       # overwrite without prompt
        "-i", str(webm),
        "-t", str(int(max_seconds)),
        "-vf", f"fps={int(fps)},scale=720:-1:flags=lanczos",
        "-loop", "0",
        str(gif),
    ]
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=120,
        )
    except subprocess.TimeoutExpired:
        _log(f"  record_video_to_gif: ffmpeg timeout on {webm.name}")
        return False
    except Exception as e:
        _log(f"  record_video_to_gif: ffmpeg invocation failed: "
             f"{type(e).__name__}: {e}")
        return False

    if proc.returncode != 0:
        _log(f"  record_video_to_gif: ffmpeg rc={proc.returncode} "
             f"on {webm.name}; stderr={proc.stderr[:200]!r}")
        return False

    if not gif.exists():
        _log(f"  record_video_to_gif: ffmpeg returned 0 but {gif} missing")
        return False

    size_mb = gif.stat().st_size / (1024.0 * 1024.0)
    if size_mb > max_mb:
        _log(f"  record_video_to_gif: {gif.name} is {size_mb:.1f}MB "
             f"(> {max_mb}MB cap) — deleting")
        try:
            gif.unlink()
        except Exception:
            pass
        return False

    return True


# ─── Widget skeleton + terminal-state validation ────────────────────────────


# JS that counts widget-scoped .ant-skeleton elements. We intentionally
# scope the count rather than match `.ant-skeleton` cluster-wide because
# the bare selector catches transient overlay surfaces (Drawer/
# Notification/Modal/Message/Popover/Dropdown/Tooltip) that have nothing
# to do with the main widget tree. Phase A 0.30.6 v2 attempt 4 surfaced
# this as false positives (e.g. a Notification toast firing during nav
# got logged as a "stuck widget skeleton").
#
# Two layers of scoping:
#
#   (preferred) [data-widget-renderer] .ant-skeleton — exact match
#     against the widget mount point. Becomes live the moment frontend
#     lands the `data-widget-renderer` wrapper on WidgetRenderer.tsx.
#     Today the wrapper does NOT exist, so this branch matches zero.
#   (fallback) all .ant-skeleton EXCLUDING those that have an ancestor
#     in the overlay-selector list.
#
# The JS returns both counts so logs can show whether the preferred
# wrapper has rolled out; once `widget_scoped` matches the fallback
# consistently, the fallback can be deleted.
_WIDGET_SKELETON_COUNT_JS = """
() => {
    const EXCLUDED_ANCESTORS = [
        '.ant-drawer',
        '.ant-notification',
        '.ant-notification-notice',
        '.ant-modal',
        '.ant-modal-root',
        '.ant-message',
        '.ant-popover',
        '.ant-dropdown',
        '.ant-tooltip',
        '.ant-select-dropdown'
    ];
    const all = Array.from(document.querySelectorAll('.ant-skeleton'));
    const widgetScoped = document.querySelectorAll(
        '[data-widget-renderer] .ant-skeleton').length;
    let scoped = 0;
    for (const el of all) {
        let excluded = false;
        for (const sel of EXCLUDED_ANCESTORS) {
            if (el.closest(sel)) { excluded = true; break; }
        }
        if (!excluded) scoped++;
    }
    return {raw: all.length, scoped: scoped, widget_scoped: widgetScoped};
}
"""


def _count_widget_skeletons(page):
    """Return (scoped, raw, widget_scoped) skeleton counts on the page.

    `scoped` is the gate input: skeletons OUTSIDE Drawer/Notification/
    Modal/Message/Popover/Dropdown/Tooltip surfaces. `raw` is the legacy
    unscoped `.ant-skeleton` count, kept for diagnostic visibility.
    `widget_scoped` matches `[data-widget-renderer] .ant-skeleton`; it
    is zero today (the frontend does not yet emit `data-widget-renderer`)
    and becomes the canonical metric once that wrapper rolls out.
    """
    result = page.evaluate(_WIDGET_SKELETON_COUNT_JS)
    return (int(result.get("scoped", 0)),
            int(result.get("raw", 0)),
            int(result.get("widget_scoped", 0)))


def _validate_widget_terminal_state(page, page_path, label, user="admin"):
    """Inspect the rendered page after the /call stability poll returns.

    The waterfall measurement only tells us when network activity stopped.
    It does not tell us whether the dashboard actually rendered piechart
    + table or whether every widget is still showing a Skeleton because
    the stability poll exited prematurely. This helper applies three
    gates and returns a structured result dict; callers decide whether
    to invalidate `waterfallMs` based on `terminal_state`.

    Gates:
      1. widget-scoped .ant-skeleton count == 0 — HARD FAIL.
      2. .ant-result-error count is recorded but NOT a failure: errored
         widgets are a valid terminal state.
      3. /call count must be within EXPECTED_CALLS_TOLERANCE of
         expected_calls(user, page_path) — HARD FAIL on deviation.
         Pages with no characterized expectation for the (user, page)
         pair are skipped silently.

    Returns a dict with keys:
        skeleton_count, skeleton_count_raw, skeleton_count_widget_scoped,
        errored_count, expected_calls, actual_calls,
        calls_within_tolerance, terminal_state ("pass" or "fail"), user
    """
    try:
        skeleton_count, skeleton_raw, skeleton_widget = _count_widget_skeletons(page)
    except Exception as e:
        _log(f"    [WARN] could not count widget-scoped .ant-skeleton at "
             f"{label}: {e}")
        skeleton_count = skeleton_raw = skeleton_widget = 0
    try:
        errored_count = page.locator(".ant-result-error").count()
    except Exception as e:
        _log(f"    [WARN] could not count .ant-result-error at {label}: {e}")
        errored_count = 0
    try:
        actual_calls = page.evaluate(
            "() => performance.getEntriesByType('resource')"
            ".filter(e => e.name.includes('/call')).length")
    except Exception as e:
        _log(f"    [WARN] could not read /call count at {label}: {e}")
        actual_calls = 0

    expected = _expected_calls_lookup(user, page_path)
    tolerance = _expected_calls_tolerance()
    if expected is None:
        calls_within_tolerance = True
    else:
        calls_within_tolerance = abs(actual_calls - expected) <= tolerance

    terminal_state = "pass"
    if skeleton_count > 0:
        _log(f"    [FAIL] stability_premature: {skeleton_count} skeletons "
             f"still visible at {label} (raw={skeleton_raw}, "
             f"widget_scoped={skeleton_widget})")
        terminal_state = "fail"
    if errored_count > 0:
        _log(f"    [WARN] errored_widgets={errored_count} at {label}")
    if expected is not None and not calls_within_tolerance:
        _log(f"    [FAIL] call_count_mismatch[{user}]: expected={expected}"
             f"±{tolerance} actual={actual_calls} at {label}")
        terminal_state = "fail"

    return {
        "skeleton_count": skeleton_count,
        "skeleton_count_raw": skeleton_raw,
        "skeleton_count_widget_scoped": skeleton_widget,
        "errored_count": errored_count,
        "expected_calls": expected,
        "actual_calls": actual_calls,
        "calls_within_tolerance": calls_within_tolerance,
        "terminal_state": terminal_state,
        "user": user,
    }


# ─── Browser login + navigation measurement ─────────────────────────────────


def browser_login(page, username, password, retries=3):
    """Login via the frontend UI; return True on success.

    Drives the form via Playwright. Verifies success by reading the
    `K_user` localStorage entry — pure URL redirects can lie.
    """
    if FRONTEND is None:
        _log("    browser: FRONTEND_URL not set — skipping login")
        return False
    for attempt in range(retries):
        try:
            page.goto(f"{FRONTEND}/login", wait_until="networkidle", timeout=300000)
            _log(f"    browser: login page loaded, URL={page.url}")
            page.click('#basic_username', timeout=10000)
            page.keyboard.type(username, delay=30)
            page.click('#basic_password', timeout=10000)
            page.keyboard.type(password, delay=30)
            page.click('button[type="submit"]', timeout=10000)
            page.wait_for_load_state("networkidle", timeout=30000)
            page.wait_for_timeout(5000)
            has_token = page.evaluate("""() => {
                try {
                    const u = JSON.parse(localStorage.getItem('K_user') || '{}');
                    return !!u.accessToken;
                } catch { return false; }
            }""")
            if has_token:
                _log(f"    browser: login OK — auth token in localStorage")
                return True
            _log(f"    browser: login attempt {attempt + 1}: no auth token, retrying")
        except Exception as e:
            current = getattr(page, "url", "?")
            _log(f"    browser: login attempt {attempt + 1} failed "
                 f"(URL={current}): {e}")
        if attempt < retries - 1:
            time.sleep(5)
    return False


# Backward-compat alias (callers that haven't migrated yet).
_browser_login = browser_login
_validate_widget_terminal_state_public = _validate_widget_terminal_state


def browser_measure_navigation(page, page_path, label, min_calls=0,
                               user="admin"):
    """Navigate to a page; measure the /call API waterfall + widget gates.

    Args:
        min_calls: Minimum number of /call requests to wait for before
            declaring stability. Set from the COLD navigation's call count
            so WARM navigations don't exit early.
        user: Subject the page is logged in as. Forwarded to
            _validate_widget_terminal_state for per-user EXPECTED_CALLS.

    Returns a dict with `waterfallMs`, `callCount`, `loadComplete`,
    `levels`, `httpOk/httpErr`, `validation`, and `incomplete` (True when
    the sample is unusable per widget-terminal-state or min_calls floor).
    """
    if FRONTEND is None:
        _log(f"    browser: FRONTEND_URL not set — skipping {label}")
        return {"label": label, "incomplete": True, "callCount": 0,
                "waterfallMs": 0, "loadComplete": 0, "domContentLoaded": 0,
                "calls": [], "levels": [], "httpOk": 0, "httpErr": 0,
                "validation": {"terminal_state": "fail"}}

    # Track /call HTTP response statuses via Playwright response listener.
    # Resource Timing API doesn't expose status codes, so we capture them here.
    _call_statuses = []

    def _on_response(response):
        if "/call" in response.url:
            _call_statuses.append({"url": response.url, "status": response.status})

    page.on("response", _on_response)

    # Clear previous performance entries and expand the resource timing buffer.
    # The default buffer (250 entries) includes ALL resources; at high
    # cardinality late /call entries are silently dropped.
    page.evaluate("""() => {
        performance.clearResourceTimings();
        performance.setResourceTimingBufferSize(2000);
    }""")

    # Navigate — use domcontentloaded instead of networkidle to avoid hanging
    # when the page has persistent connections (SSE, websockets, long-poll).
    try:
        page.goto(f"{FRONTEND}{page_path}", wait_until="domcontentloaded",
                  timeout=60_000)
    except Exception as e:
        _log(f"    WARNING: page.goto timeout ({e}), continuing with "
             f"stability poll")

    if "/login" in page.url:
        _log(f"    WARNING: redirected to login at {page.url}")

    # Wait for /call requests to stabilize.
    _stable_streak = 0
    _prev_count = -1
    for _ in range(120):
        _cur_count = page.evaluate(
            "() => performance.getEntriesByType('resource')"
            ".filter(e => e.name.includes('/call')).length")
        if _cur_count == _prev_count and _cur_count > 0 and _cur_count >= min_calls:
            _stable_streak += 1
            if _stable_streak >= 2:
                break
        else:
            _stable_streak = 0
        _prev_count = _cur_count
        page.wait_for_timeout(1000)

    validation = _validate_widget_terminal_state(page, page_path, label,
                                                 user=user)

    # Measure /call waterfall + cluster calls into progressive-rendering levels.
    result = page.evaluate("""() => {
        const GAP_MS = 150;
        const entries = performance.getEntriesByType('resource');
        const calls = entries
            .filter(e => e.name.includes('/call'))
            .sort((a, b) => a.startTime - b.startTime);
        if (calls.length === 0) return { callCount: 0, waterfallMs: 0, calls: [], levels: [] };
        const first = calls[0].startTime;
        const last = Math.max(...calls.map(c => c.startTime + c.duration));
        const callsRel = calls.map(c => ({
            name: new URL(c.name).searchParams.get('name') || c.name.split('/').pop(),
            duration: Math.round(c.duration),
            startTime: Math.round(c.startTime - first),
            endTime: Math.round(c.startTime + c.duration - first),
        }));
        const levels = [];
        let cur = null;
        let curMaxEnd = -Infinity;
        for (const c of callsRel) {
            if (cur === null || c.startTime > curMaxEnd + GAP_MS) {
                if (cur !== null) levels.push(cur);
                cur = { level: levels.length + 1, count: 0, startMs: c.startTime,
                        endMs: c.endTime, names: {} };
                curMaxEnd = c.endTime;
            }
            cur.count += 1;
            if (c.endTime > cur.endMs) cur.endMs = c.endTime;
            if (c.endTime > curMaxEnd) curMaxEnd = c.endTime;
            cur.names[c.name] = (cur.names[c.name] || 0) + 1;
        }
        if (cur !== null) levels.push(cur);
        for (const lv of levels) {
            lv.durationMs = lv.endMs - lv.startMs;
            lv.topNames = Object.entries(lv.names)
                .sort((a, b) => b[1] - a[1])
                .slice(0, 3)
                .map(([n, k]) => `${n}×${k}`);
            delete lv.names;
        }
        return {
            callCount: calls.length,
            waterfallMs: Math.round(last - first),
            calls: callsRel.map(c => ({ name: c.name, duration: c.duration, startTime: c.startTime })),
            levels: levels,
        };
    }""")

    if min_calls > 0 and result.get("callCount", 0) < min_calls:
        result["waterfallMs"] = 0
        result["incomplete"] = True

    if validation["terminal_state"] != "pass":
        result["waterfallMs"] = 0
        result["incomplete"] = True

    nav = page.evaluate("""() => {
        const t = performance.getEntriesByType('navigation')[0];
        if (!t) return {};
        return {
            domContentLoaded: Math.round(t.domContentLoadedEventEnd - t.startTime),
            loadComplete: Math.round(t.loadEventEnd - t.startTime),
        };
    }""")

    try:
        page.remove_listener("response", _on_response)
    except Exception:
        pass

    ok_count = sum(1 for s in _call_statuses if s["status"] == 200)
    err_count = len(_call_statuses) - ok_count

    return {
        "label": label,
        "callCount": result.get("callCount", 0),
        "waterfallMs": result.get("waterfallMs", 0),
        "domContentLoaded": (nav or {}).get("domContentLoaded", 0),
        "loadComplete": (nav or {}).get("loadComplete", 0),
        "calls": result.get("calls", []),
        "levels": result.get("levels", []),
        "httpOk": ok_count,
        "httpErr": err_count,
        "validation": validation,
        "incomplete": result.get("incomplete", False),
    }


# Backward-compat alias.
_browser_measure_navigation = browser_measure_navigation


def _browser_wait_for_call_stability(page, min_calls=0, timeout_s=120):
    """Wait for /call XHR requests to stabilize after a page navigation.

    DEPRECATED — kept for the legacy Phase 4 entry point. The production
    path is `browser_measure_navigation`, which embeds an inline stability
    loop with the same shape (3-consecutive-identical-counts + min_calls
    floor). This helper exists so callers in the worktree source that
    haven't migrated yet still resolve.

    Returns the final /call count observed.
    """
    stable_streak = 0
    prev_count = -1
    for _ in range(timeout_s):
        cur_count = page.evaluate(
            "() => performance.getEntriesByType('resource')"
            ".filter(e => e.name.includes('/call')).length")
        if cur_count == prev_count and cur_count > 0 and cur_count >= min_calls:
            stable_streak += 1
            if stable_streak >= 2:
                break
        else:
            stable_streak = 0
        prev_count = cur_count
        page.wait_for_timeout(1000)
    return prev_count if prev_count >= 0 else 0


# ─── Composition-count verifiers (used by VERIFY poll) ──────────────────────


def verify_composition_count_api(token):
    """Verify composition count by calling the piechart widget API.

    Calls dashboard-compositions-panel-row-piechart and reads
    `status.widgetData.title` (a string like "1200"). This is exactly
    what the dashboard piechart displays.

    Returns the observed count or -1 if verification was not possible.
    """
    try:
        _ms, code, body = http_get_json(
            '/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1'
            '&resource=piecharts'
            '&name=dashboard-compositions-panel-row-piechart'
            '&namespace=krateo-system',
            token,
        )
        if code != 200 or not isinstance(body, dict):
            return -1
        title = (body.get("status") or {}).get("widgetData", {}).get("title")
        if title is None:
            return -1
        return int(title)
    except Exception:
        return -1


# Backward-compat alias.
_verify_composition_count_api = verify_composition_count_api


def verify_composition_count_ui(page):
    """Verify composition count by fetching the piechart widget from the
    browser context.

    The piechart is rendered on a Canvas element, so the count cannot be
    read from the DOM. Instead, we fetch the piechart widget endpoint
    from the browser context using the browser's own auth token
    (localStorage K_user). This verifies the same data path the UI uses:
    browser -> snowplow -> cache.

    Returns the observed count or -1 if verification was not possible.
    """
    try:
        result = page.evaluate("""(snowplowUrl) => {
            return (async () => {
                try {
                    const userData = JSON.parse(localStorage.getItem('K_user') || '{}');
                    const token = userData.accessToken;
                    if (!token) return -1;
                    const resp = await fetch(
                        snowplowUrl + '/call?apiVersion=widgets.templates.krateo.io'
                        + '%2Fv1beta1&resource=piecharts'
                        + '&name=dashboard-compositions-panel-row-piechart'
                        + '&namespace=krateo-system',
                        { headers: { 'Authorization': 'Bearer ' + token } }
                    );
                    if (resp.status !== 200) return -1;
                    const body = await resp.json();
                    const title = (body.status || {}).widgetData?.title;
                    if (title === undefined || title === null) return -1;
                    return parseInt(title, 10);
                } catch (e) { return -1; }
            })();
        }""", SNOWPLOW)
        return result
    except Exception:
        return -1


# Backward-compat alias.
_verify_composition_count_ui = verify_composition_count_ui


def _poll_piechart_progression(token, stop_event, interval=5):
    """Background-thread helper: poll piechart API during S6 deploy+
    stabilize to log value progression.

    Logs each sample with elapsed time, piechart value (from cache), and
    cluster truth (kubectl count_compositions). Logs only on value change
    or every 30s to avoid noise.
    """
    start = time.time()
    sample = 0
    prev_api = None
    while not stop_event.is_set():
        sample += 1
        api_count = verify_composition_count_api(token) if token else -1
        cluster_count = _count_compositions()
        elapsed_s = int(time.time() - start)
        if api_count != prev_api or sample == 1 or elapsed_s % 30 < interval:
            api_str = f"{api_count}" if api_count >= 0 else "?"
            _log(f"    S6 PIECHART t={elapsed_s:>4d}s  piechart={api_str}  "
                 f"cluster={cluster_count}")
            prev_api = api_count
        stop_event.wait(interval)


# ─── Browser measure stage (the sole convergence-poll site) ─────────────────


def browser_measure_stage(page, stage_num, stage_desc, cache_mode,
                          token=None, num_navs=3, user="admin",
                          verify_against_cluster=True,
                          verify_timeout: int = 300,
                          verify_interval: int = 3,
                          screenshots_dir: Path | None = None):
    """Navigate browser to each page num_navs times, return timing data.

    Expects an already-logged-in page object and an admin JWT token.

    `user` tags every emitted navigation dict so the canonical-row
    exporter can bucket samples per cell (admin/cyberjoker × ON/OFF).
    The page must already be logged in as `user`; this argument is
    metadata only.

    `verify_against_cluster` controls the S6 piechart convergence check.
    For admin (cluster-wide RBAC) the piechart value must converge to
    the cluster's total composition count; for cyberjoker (RBAC scoped
    to a single namespace) the piechart will legitimately show fewer,
    so we fall back to an intra-user UI-vs-API consistency check
    (api_count == ui_count).

    On VERIFY-poll deadline expiry with `matched=False`, this function
    RAISES `ConvergenceTimeout(stage, user, api, ui, cluster, timeout_secs)`
    rather than silently logging TIMEOUT/MISMATCH (worktree source
    6465-6475's silent-skip bug). Block 4's stage runner catches it,
    persists a stage proof with passed=False + convergence_timeout=true,
    then re-raises to abort the run with exit 4.
    """
    ns_count, comp_count = _count_bench_ns(), _count_compositions()
    _log(f"Cluster: {ns_count} bench ns, {comp_count} compositions")

    if screenshots_dir is None:
        screenshots_dir = Path(__file__).parent / "screenshots"
    else:
        screenshots_dir = Path(screenshots_dir)

    pages_data = {}
    for page_name, page_path in BROWSER_SCALING_PAGES:
        navs = []
        cold_calls = 0
        for nav_num in range(1, num_navs + 1):
            m = browser_measure_navigation(
                page, page_path,
                f"S{stage_num} {cache_mode} nav#{nav_num} {page_name}",
                min_calls=cold_calls,
                user=user)
            cold_warm = 'COLD' if nav_num == 1 else 'WARM'
            m["nav_num"] = nav_num
            m["cold_warm"] = cold_warm
            m["user"] = user
            navs.append(m)
            if nav_num == 1:
                cold_calls = m["callCount"]
            http_info = (f"  http={m['httpOk']}ok"
                         if m.get("httpOk", 0) + m.get("httpErr", 0) > 0
                         else "")
            if m.get("httpErr", 0) > 0:
                http_info += f"/{m['httpErr']}err"
            _log(f"  {cold_warm} {page_name:<15s} "
                 f"waterfall={m['waterfallMs']:>5d}ms  "
                 f"load={m['loadComplete']:>5d}ms  "
                 f"calls={m['callCount']}{http_info}")

            if page_name == "Dashboard" and nav_num == num_navs:
                screenshots_dir.mkdir(parents=True, exist_ok=True)

                verify_start = time.time()
                deadline = verify_start + verify_timeout
                api_count = -1
                ui_count = -1
                fresh_comp_count = _count_compositions()
                last_cluster_check = time.time()
                matched = False

                poll_num = 0
                while time.time() < deadline:
                    poll_num += 1
                    if time.time() - last_cluster_check > 60:
                        fresh_comp_count = _count_compositions()
                        last_cluster_check = time.time()
                    api_count = (verify_composition_count_api(token)
                                 if token else -1)
                    ui_count = verify_composition_count_ui(page)
                    elapsed_ms = int((time.time() - verify_start) * 1000)
                    api_str_p = f"{api_count}" if api_count >= 0 else "?"
                    ui_str_p = f"{ui_count}" if ui_count >= 0 else "?"
                    _log(f"    VERIFY poll {poll_num}: api={api_str_p} "
                         f"ui={ui_str_p} cluster={fresh_comp_count} ({elapsed_ms}ms)")

                    try:
                        page.evaluate("""() => {
                            const walker = document.createTreeWalker(
                                document.body, NodeFilter.SHOW_TEXT, null);
                            let target = null;
                            while (walker.nextNode()) {
                                const node = walker.currentNode;
                                if (node.textContent.trim() === 'Compositions') {
                                    const el = node.parentElement;
                                    if (el.closest('nav, [class*="sidebar"], [class*="menu"], [class*="sider"]'))
                                        continue;
                                    target = el;
                                }
                            }
                            if (target) {
                                target.scrollIntoView({ block: 'start', behavior: 'instant' });
                                return true;
                            }
                            window.scrollTo(0, document.body.scrollHeight);
                            return false;
                        }""")
                        page.wait_for_timeout(500)
                    except Exception:
                        pass
                    if SCREENSHOTS:
                        ss_name = (f"S{stage_num}_{cache_mode}_{user}_poll"
                                   f"{poll_num}_api{api_str_p}_ui{ui_str_p}_"
                                   f"cluster{fresh_comp_count}.png")
                        try:
                            page.screenshot(path=str(screenshots_dir / ss_name))
                            _log(f"    screenshot: {ss_name}")
                        except Exception as e:
                            _log(f"    screenshot failed: {e}")

                    if verify_against_cluster:
                        api_ok = (api_count >= 0 and api_count == fresh_comp_count)
                        ui_ok = (ui_count >= 0 and ui_count == fresh_comp_count)
                        if api_ok and ui_ok:
                            matched = True
                            break
                    else:
                        if (api_count >= 0 and ui_count >= 0
                                and api_count == ui_count):
                            matched = True
                            break
                    time.sleep(verify_interval)

                convergence_ms = int((time.time() - verify_start) * 1000)
                m["verified_api"] = api_count
                m["verified_ui"] = ui_count
                m["convergence_ms"] = convergence_ms if matched else -1
                api_str = f"{api_count}" if api_count >= 0 else "?"
                ui_str = f"{ui_count}" if ui_count >= 0 else "?"

                if not matched:
                    # ConvergenceTimeout — replaces the source-script's
                    # silent TIMEOUT/MISMATCH log at worktree line
                    # 6465-6475. Block 4's stage runner catches this,
                    # writes a stage proof with passed=False, and
                    # re-raises to abort the run with exit 4.
                    _log(f"    VERIFY ✗ api={api_str} ui={ui_str} "
                         f"cluster={fresh_comp_count} converged=TIMEOUT — "
                         f"raising ConvergenceTimeout")
                    raise ConvergenceTimeout(
                        stage=stage_num,
                        user=user,
                        api=api_count,
                        ui=ui_count,
                        cluster=fresh_comp_count,
                        timeout_secs=verify_timeout,
                    )

                _log(f"    VERIFY ✓ api={api_str} ui={ui_str} "
                     f"cluster={fresh_comp_count} converged={convergence_ms}ms")

                # Content-level correctness check: compare composition NAMES,
                # not just counts. Catches silent cache corruption where the
                # count matches but the cached items are the wrong set
                # (stale entries present, recent deletes missing, etc.).
                if cache_mode == "ON" and token:
                    truth = _list_composition_names()
                    cached = list_composition_names_from_cache(token)
                    if truth is not None and cached is not None:
                        if truth == cached:
                            _log(f"    CONTENT ✓ {len(truth)} composition "
                                 f"names match")
                            m["content_match"] = True
                        else:
                            missing = truth - cached
                            extra = cached - truth
                            _log(f"    CONTENT ✗ DRIFT — "
                                 f"missing={len(missing)} extra={len(extra)}")
                            if missing:
                                _log(f"      missing: {sorted(missing)[:3]}...")
                            if extra:
                                _log(f"      extra: {sorted(extra)[:3]}...")
                            m["content_match"] = False
                            m["content_missing"] = len(missing)
                            m["content_extra"] = len(extra)

                # Final VERIFY screenshot — reload dashboard for fresh data.
                try:
                    page.goto(f"{FRONTEND}/dashboard",
                              wait_until="networkidle", timeout=30000)
                    page.wait_for_timeout(2000)
                except Exception:
                    pass
                try:
                    page.evaluate("""() => {
                        const walker = document.createTreeWalker(
                            document.body, NodeFilter.SHOW_TEXT, null);
                        let target = null;
                        while (walker.nextNode()) {
                            const node = walker.currentNode;
                            if (node.textContent.trim() === 'Compositions') {
                                const el = node.parentElement;
                                if (el.closest('nav, [class*="sidebar"], [class*="menu"], [class*="sider"]'))
                                    continue;
                                target = el;
                            }
                        }
                        if (target) {
                            target.scrollIntoView({ block: 'start', behavior: 'instant' });
                            return true;
                        }
                        window.scrollTo(0, document.body.scrollHeight);
                        return false;
                    }""")
                    page.wait_for_timeout(500)
                except Exception:
                    pass
                if SCREENSHOTS:
                    ss_final = (f"S{stage_num}_{cache_mode}_{user}_VERIFY_PASS_"
                                f"api{api_str}_ui{ui_str}_{convergence_ms}ms.png")
                    try:
                        page.screenshot(path=str(screenshots_dir / ss_final))
                        _log(f"    screenshot: {ss_final}")
                    except Exception as e:
                        _log(f"    screenshot failed: {e}")

        wf_vals = [n["waterfallMs"] for n in navs if n["waterfallMs"] > 0]
        lc_vals = [n["loadComplete"] for n in navs if n["loadComplete"] > 0]
        last_nav = navs[-1] if navs else {}
        pages_data[page_name] = {
            "navigations": navs,
            "waterfall_p50": _pct(wf_vals, 50) if wf_vals else 0,
            "waterfall_p90": _pct(wf_vals, 90) if wf_vals else 0,
            "waterfall_warm_last": last_nav.get("waterfallMs", 0),
            "loadComplete_p50": _pct(lc_vals, 50) if lc_vals else 0,
            "loadComplete_p90": _pct(lc_vals, 90) if lc_vals else 0,
            "callCount": navs[0]["callCount"] if navs else 0,
            "levels_warm_last": last_nav.get("levels", []),
            "levels_cold": (navs[0].get("levels", []) if navs else []),
        }

    return {
        "stage": stage_num, "desc": stage_desc, "cache": cache_mode,
        "bench_ns": ns_count, "compositions": comp_count,
        "pages": pages_data,
        # cache_supported defaults True for any actually-measured cell.
        # Block 4's stage runner overrides this to False for synthetic
        # N/A rows on stripped charts.
        "cache_supported": True,
    }


# Backward-compat alias.
_browser_measure_stage = browser_measure_stage
