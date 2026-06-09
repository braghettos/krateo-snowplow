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


# ─── Task #250 Block 2 — N-aware EXPECTED_CALLS formula ─────────────────────


def test_expected_calls_returns_base_when_n_visible_is_none():
    """Backward-compat: pre-Block-2 callers (no n_visible kwarg) get the
    structural BASE value, unchanged from SCHEMA 1.0.0 behaviour.
    """
    from bench.expected import expected_calls
    assert expected_calls("admin", "/compositions") == 10
    assert expected_calls("cyberjoker", "/compositions") == 10
    assert expected_calls("admin", "/dashboard") == 16


def test_expected_calls_returns_base_when_n_visible_is_zero():
    """N=0 → no visible cards → no per-card-widget fan-out → BASE.
    This preserves S1/S2/S3 PASS shape from the empirical 0.30.248 run.
    """
    from bench.expected import expected_calls
    assert expected_calls("admin", "/compositions", n_visible=0) == 10
    assert expected_calls("cyberjoker", "/compositions", n_visible=0) == 10


def test_expected_calls_adds_per_card_widgets_term_below_per_page_cap():
    """N=1 visible card. admin fires 4 widgets/card (10 + 4×1 = 14).
    cj fires 2 widgets/card (10 + 2×1 = 12) — the 2 Buttons are filtered
    by snowplow's allowed=false / SPA's WidgetRenderer. See
    docs/task-273-s8-second-defect-trace-2026-06-09.md.
    """
    from bench.expected import expected_calls
    assert expected_calls("admin", "/compositions", n_visible=1) == 14
    assert expected_calls("cyberjoker", "/compositions", n_visible=1) == 12


def test_expected_calls_caps_at_per_page_above_cap():
    """N=5 → 5 cards visible → 10 + 4×5 = 30.
    N=50K → cards still capped at per_page=5 → 30 (matches S6 empirical).
    """
    from bench.expected import expected_calls
    assert expected_calls("admin", "/compositions", n_visible=5) == 30
    assert expected_calls("admin", "/compositions", n_visible=20) == 30
    assert expected_calls("admin", "/compositions", n_visible=50000) == 30


def test_expected_calls_ignores_n_visible_for_non_compositions_page():
    """Only /compositions has the per-card fan-out term. /dashboard ignores
    n_visible (no datagrid).
    """
    from bench.expected import expected_calls
    assert expected_calls("admin", "/dashboard", n_visible=999) == 16
    assert expected_calls("cyberjoker", "/dashboard", n_visible=999) == 16


def test_expected_calls_unknown_user_falls_back_to_admin_row():
    """Unknown user → use admin's BASE+formula (safe default).
    Same fallback applies under the N-aware path.
    """
    from bench.expected import expected_calls
    assert expected_calls("mystery", "/compositions") == 10
    assert expected_calls("mystery", "/compositions", n_visible=5) == 30


def test_expected_calls_unknown_page_returns_none():
    """Caller treats None as 'skip the gate' — unchanged behaviour."""
    from bench.expected import expected_calls
    assert expected_calls("admin", "/unknown") is None
    assert expected_calls("admin", "/unknown", n_visible=100) is None


def test_expected_calls_constants_match_spec():
    """The N-aware formula constants are named for traceability per
    Task #250 design §5.4. Operators should be able to grep
    `COMP_DATAGRID_PER_PAGE` and see 5 (frontend datagrid page size).
    """
    from bench.expected import (
        COMP_DATAGRID_PER_PAGE,
        COMP_PER_CARD_WIDGETS,
        COMP_BASE_CALLS_STRUCTURAL,
        DASH_BASE_CALLS_STRUCTURAL,
    )
    assert COMP_DATAGRID_PER_PAGE == 5
    assert COMP_PER_CARD_WIDGETS == 4
    assert COMP_BASE_CALLS_STRUCTURAL == 10
    assert DASH_BASE_CALLS_STRUCTURAL == 16


def test_expected_calls_base_dict_alias_preserves_overlay_path():
    """`EXPECTED_CALLS` remains an alias for `EXPECTED_CALLS_BASE` so the
    overlay-merge path at `load_expected_calls_overlay` and the
    `gate_overlay_freshness` divergence detector keep working without
    structural changes.
    """
    from bench.expected import EXPECTED_CALLS, EXPECTED_CALLS_BASE
    assert EXPECTED_CALLS is EXPECTED_CALLS_BASE


def test_expected_calls_negative_n_visible_returns_base():
    """A -1 sentinel from a failed verify_composition_count_api call must
    NOT bias the gate — fall back to BASE.
    """
    from bench.expected import expected_calls
    assert expected_calls("admin", "/compositions", n_visible=-1) == 10


# ─── Per-user per-card widget count (Task #273) ─────────────────────────────


def test_expected_calls_per_user_widget_count_cyberjoker():
    """cj fires 2 widgets per card (Panel + Markdown). The 2 Buttons get
    allowed=false from snowplow because cj's S8 Role only grants get/list
    on compositions.composition.krateo.io. SPA filters allowed=false
    and Panel's FooterItem short-circuits — no /call fired for buttons.

    n_visible=999 → 5 cards capped → base 10 + 2 widgets × 5 cards = 20.
    See docs/task-273-s8-second-defect-trace-2026-06-09.md §5.1.
    """
    from bench.expected import expected_calls
    assert expected_calls("cyberjoker", "/compositions", n_visible=999) == 20


def test_expected_calls_per_user_widget_count_admin_still_four():
    """admin's broader RBAC means all 4 widgets per card fire.
    n_visible=50000 → 5 cards capped → 10 + 4 × 5 = 30. Unchanged behaviour.
    """
    from bench.expected import expected_calls
    assert expected_calls("admin", "/compositions", n_visible=50000) == 30


def test_expected_calls_per_user_widget_count_dict_present_and_correct():
    """Regression guard: the per-user dict must contain admin=4 and
    cyberjoker=2. A future role-grant change for cj would update cj=4;
    test will flag the constant update so docs stay in sync.
    """
    from bench.expected import COMP_PER_CARD_WIDGETS_BY_USER
    assert COMP_PER_CARD_WIDGETS_BY_USER["admin"] == 4
    assert COMP_PER_CARD_WIDGETS_BY_USER["cyberjoker"] == 2


def test_expected_calls_unknown_user_widget_count_falls_back_to_admin():
    """Unknown user → falls back to admin row + admin widget count.
    /compositions(unknown, N=10) = 10 + 4 × 5 = 30 (same as admin).
    """
    from bench.expected import expected_calls
    assert expected_calls("ghost-user", "/compositions", n_visible=10) == 30
