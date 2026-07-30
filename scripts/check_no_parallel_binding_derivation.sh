#!/bin/bash
# check_no_parallel_binding_derivation.sh — H6 CI guard that WIRES the
# previously-orphaned AST lint scripts/lint/no_parallel_binding_derivation.go
# into a runnable, exit-code-bearing step.
#
# THE GAP (H6): the lint program is a //go:build ignore standalone (so
# `go build ./...` / `go vet ./...` skip it) and was invoked by NO workflow.
# A BindingUID derivation reintroduced OUTSIDE the match_subject.go single
# source of truth — the exact v3-baseline defect class the lint was written
# to catch — would therefore NOT fail CI. This wrapper closes that gap by
# running the lint over the production tree and propagating its exit code,
# mirroring the wiring style of scripts/check_resolveoptions_rc.sh.
#
# The lint enforces internal/cache/match_subject.go as the SINGLE SOURCE OF
# TRUTH for BindingUID derivation: any non-allowlisted production file that
# iterates snap.CRBsBy* / snap.RBsBy* / snap.CRBsCatchAll / snap.RBsCatchAllByNS
# (an identity->binding projection outside the SOT) is a hard failure.
#
# Run it locally before pushing:
#
#   bash scripts/check_no_parallel_binding_derivation.sh
#
set -euo pipefail

cd "$(dirname "$0")/.."

# The lint defaults --root to the go.mod-bearing project root and walks
# <root>/internal. Invoke via `go run` on the //go:build ignore program —
# no build step, no extra deps, resolves against the repo's own go.mod.
go run scripts/lint/no_parallel_binding_derivation.go
