"""Snowplow bench harness — packaged 2026-06-02.

See docs/bench-restructure-path-b-plan-2026-06-02.md for the module-by-module
layout. Operator entrypoint: `python -m bench <subcommand>` (CLI lands in
Block 2). Block 1 ships cluster.py + lifecycle.py + tests scaffolding only.

Modules (full plan at §A):
  cluster.py   — kubectl wrapper + kubernetes-client helpers (Block 1)
  lifecycle.py — cluster lifecycle orchestration (Block 1)
  storm.py     — disruptive scenarios (Block 2)
  expected.py  — EXPECTED_CALLS overlay (Block 2)
  cli.py       — argparse subcommands (Block 2+)
  browser.py   — Playwright driver + verify-poll (Block 3)
  phases.py    — stage registry + state.json (Block 4)
  ledger.py    — canonical ledger row (Block 4)
"""

__version__ = "0.1.0-block1"

__all__ = ["__version__"]
