#!/bin/bash
# check_resolved_entry_put_sites.sh — L-SCOPE-COMPLETENESS CI guard
# (docs/test-blindspot-analysis-2026-07-24.md, class 3 — enumeration
# incompleteness). Machine-enforces the #118(d) lesson: "stamp ALL sites of a
# class, or none — never a named subset."
#
# THE DEFECT CLASS (#118d, memory project_118d_seed_put_uaf_ttl_gap): a
# per-entry metadata field (the short UAF TTLOverride) was stamped at the fix's
# NAMED subset of `ResolvedEntry{` Put sites (dispatch + refresher) and MISSED
# the boot-seed Put — so boot-seeded UAF cells stayed on the long standard TTL.
# The in/out-of-scope site map is HAND-maintained (uaf_shortttl.go R-d-4 SITE
# MAP); a NEW `ResolvedEntry{` literal added later is silently absent from it.
# This gate turns that silent enumeration gap into a hard CI failure:
#
#   - Every `ResolvedEntry{...}` Put site must EITHER set the per-entry
#     metadata field TTLOverride, OR carry an inline `//scope-waiver:
#     TTLOverride: <reason>` annotation. A site that does neither is FLAGGED.
#   - The readiness-critical boot-prewarm scope stamps
#     (WithFallthroughScope(..., cache.ScopeBootPrewarm*)) are counted and the
#     count must stay at/above the committed floor (a dropped stamp silently
#     changes boot-path serve behaviour).
#
# It calls the AST-based checker in scripts/checkresolvedentrysites (go run
# resolves it against the repo's own go.mod — no extra deps, no build step for
# devs). Test files (_test.go) and testdata/ dirs are skipped.
#
# Run it locally before pushing:
#
#   bash scripts/check_resolved_entry_put_sites.sh
#
set -euo pipefail

cd "$(dirname "$0")/.."

# Scan the production resolver + dispatcher + cache trees — the only packages
# that construct ResolvedEntry literals and stamp boot-prewarm scopes.
# Scoping the walk keeps the gate fast and its surface explicit.
go run ./scripts/checkresolvedentrysites internal
