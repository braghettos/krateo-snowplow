"""Behavioural tests for bench/expected.py.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.5. 5 cases plus
the two tightening-#4 freshness sanity probes (mtime is source-of-truth,
NOT any `captured_at` JSON field).

No source-introspection. No live cluster.
"""

from __future__ import annotations

import json
import os
import time

import pytest


from bench.expected import (
    EXPECTED_CALLS,
    EXPECTED_CALLS_OVERLAY_PATH,
    EXPECTED_CALLS_TOLERANCE,
    OverlayStale,
    expected_calls,
    load_expected_calls_overlay,
    overlay_age_seconds,
    overlay_freshness_or_die,
)


# ─── Case 1: tolerance is 0 (Diego 2026-06-02 hard flip) ────────────────────


def test_expected_calls_tolerance_is_zero():
    """`EXPECTED_CALLS_TOLERANCE` is exactly 0 — pin against drift.

    The flip from 1 to 0 is a hard constraint of this restructure. Any
    accidental revert (e.g. in a merge conflict) drops the gate's
    discriminating power.
    """
    assert EXPECTED_CALLS_TOLERANCE == 0, (
        f"EXPECTED_CALLS_TOLERANCE must be 0 (was {EXPECTED_CALLS_TOLERANCE}); "
        f"see dispatch hard constraint #5."
    )


# ─── Case 2: overlay loader merges over hardcoded defaults ──────────────────


def test_load_overlay_merges_over_hardcoded_defaults(tmp_path, monkeypatch):
    """Overlay JSON values override the hardcoded EXPECTED_CALLS values;
    missing entries fall through.
    """
    overlay_path = tmp_path / "expected_calls.json"
    overlay_path.write_text(json.dumps({
        # admin: bump dashboard count up, leave compositions alone
        "admin": {"/dashboard": 17},
        # cyberjoker: not in overlay → unchanged
        # new user: forward-compat
        "newbie": {"/dashboard": 5},
    }))
    monkeypatch.setattr(
        "bench.expected.EXPECTED_CALLS_OVERLAY_PATH", str(overlay_path),
    )

    merged = load_expected_calls_overlay()
    assert merged["admin"]["/dashboard"] == 17           # overlay wins
    assert merged["admin"]["/compositions"] == EXPECTED_CALLS["admin"]["/compositions"]
    # cyberjoker untouched by the overlay → hardcoded values survive
    assert merged["cyberjoker"] == EXPECTED_CALLS["cyberjoker"]
    # newbie added (forward-compat)
    assert merged["newbie"] == {"/dashboard": 5}


def test_expected_calls_falls_back_to_default_user():
    """`expected_calls(unknown_user, page)` returns the admin row's
    value (fallback per source-script invariant)."""
    # Known: admin /dashboard
    assert expected_calls("admin", "/dashboard") == EXPECTED_CALLS["admin"]["/dashboard"]
    # Unknown user → fall back to admin row
    assert (expected_calls("ghost", "/dashboard")
            == EXPECTED_CALLS["admin"]["/dashboard"])
    # Unknown page → None
    assert expected_calls("admin", "/nonexistent") is None


# ─── Case 3: overlay_freshness_or_die — mtime IS the source of truth ────────


def test_overlay_freshness_or_die_passes_on_fresh_file(tmp_path):
    """Freshly-written overlay → freshness gate PASSES."""
    overlay_path = tmp_path / "fresh.json"
    overlay_path.write_text(json.dumps({"admin": {"/dashboard": 16}}))
    # mtime is now (no-op os.utime). overlay_freshness_or_die expects 14d.
    overlay_freshness_or_die(max_age_days=14, path=str(overlay_path))
    # No exception → PASS.


def test_overlay_freshness_uses_mtime_not_json_field(tmp_path):
    """Source-of-truth for 'age' is `os.stat().st_mtime`, NOT any
    `captured_at` JSON field inside the overlay. Per PM tightening #4
    to falsifier (g).

    Write an overlay carrying a stale `captured_at` (1970-01-01 epoch 0)
    but `os.utime` the file to NOW; assert the gate PASSES — only mtime
    counts.
    """
    overlay_path = tmp_path / "spoofed.json"
    overlay_path.write_text(json.dumps({
        # Hand-edited 1970 timestamp inside the file would forge freshness
        # under a JSON-field check. The mtime gate ignores it.
        "captured_at": "1970-01-01T00:00:00Z",
        "admin": {"/dashboard": 16},
    }))
    # Force mtime to NOW.
    now = time.time()
    os.utime(str(overlay_path), (now, now))

    overlay_freshness_or_die(max_age_days=14, path=str(overlay_path))
    # No raise → PASS. The JSON `captured_at` field was IGNORED.


def test_overlay_freshness_raises_when_mtime_stale(tmp_path):
    """Overlay file with mtime >14 days old → raises OverlayStale, and
    the error message points the operator at `python -m bench calibrate`.
    """
    overlay_path = tmp_path / "stale.json"
    overlay_path.write_text(json.dumps({"admin": {"/dashboard": 16}}))
    # Push mtime to 15 days ago.
    fifteen_days_ago = time.time() - (15 * 86400.0)
    os.utime(str(overlay_path), (fifteen_days_ago, fifteen_days_ago))

    with pytest.raises(OverlayStale) as exc_info:
        overlay_freshness_or_die(max_age_days=14, path=str(overlay_path))
    msg = str(exc_info.value)
    assert "python -m bench calibrate" in msg, (
        f"OverlayStale message must point operator to the calibrate command; "
        f"got: {msg!r}"
    )


def test_overlay_freshness_raises_when_overlay_missing(tmp_path):
    """Missing overlay file → raises OverlayStale (age==inf)."""
    missing = tmp_path / "nonexistent.json"
    with pytest.raises(OverlayStale) as exc_info:
        overlay_freshness_or_die(max_age_days=14, path=str(missing))
    assert "python -m bench calibrate" in str(exc_info.value)
    # age_seconds attribute carries the inf marker for callers that need it
    assert exc_info.value.age_seconds == float("inf")


# ─── Bonus: overlay_age_seconds returns inf when file missing ───────────────


def test_overlay_age_seconds_returns_inf_when_missing(tmp_path):
    age = overlay_age_seconds(path=str(tmp_path / "nope.json"))
    assert age == float("inf")
