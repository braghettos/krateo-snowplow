#!/bin/bash
# check_resolveoptions_rc.sh — CI guard for the 0.30.230-class nil-rc defect.
#
# A ResolveOptions struct literal that omits its *rest.Config field (RC, or
# SArc on restactions.ResolveOptions) leaves it nil. That nil threads ~8 calls
# deep and breaks EVERY Kind=* widget /call at cache.GVRFor ->
# discoverPluralInfo ("plurals discovery: nil *rest.Config"). The same latent
# path caused four HARD REVERTs (0.30.226 / 0.30.228 / 0.30.229, fixed at root
# in 0.30.230). This gate turns a re-introduction into a hard CI failure
# instead of a production revert.
#
# It calls the AST-based checker in scripts/checkresolveopts (go run resolves
# it against the repo's own go.mod — no extra deps, no build step for devs).
# Only the four rest.Config-bearing ResolveOptions types are checked; the two
# that carry no rest.Config (widgetdatatemplate, resourcesrefstemplate) are
# never flagged. Test files (_test.go) and .claude/ worktrees are skipped.
#
# Run it locally before pushing:
#
#   bash scripts/check_resolveoptions_rc.sh
#
set -euo pipefail

cd "$(dirname "$0")/.."

# Scan the production resolver + dispatcher trees. These are the only packages
# that construct rest.Config-bearing ResolveOptions literals; scoping the walk
# keeps the gate fast and its surface explicit.
go run ./scripts/checkresolveopts internal
