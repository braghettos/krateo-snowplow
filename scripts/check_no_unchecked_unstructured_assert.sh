#!/bin/bash
# check_no_unchecked_unstructured_assert.sh — H6 CI guard that WIRES the
# previously-orphaned AST lint scripts/lint/no_unchecked_unstructured_assert.go
# into a runnable, exit-code-bearing step.
#
# THE GAP (H6): the lint program is a //go:build ignore standalone (so
# `go build ./...` / `go vet ./...` skip it) and was invoked by NO workflow.
# A raw obj.(*unstructured.Unstructured) content-assert reintroduced inside a
# literal informer event-handler body — the Ship 0.30.233 defect class that
# silently drops every CRD event post-H5 (delivery shape is *bytesObject) —
# would therefore NOT fail CI. This wrapper closes that gap by running the
# lint over internal/cache and propagating its exit code, mirroring the
# wiring style of scripts/check_resolveoptions_rc.sh.
#
# Run it locally before pushing:
#
#   bash scripts/check_no_unchecked_unstructured_assert.sh
#
set -euo pipefail

cd "$(dirname "$0")/.."

# The lint defaults --root to <project-root>/internal/cache (the only tree
# that installs dynamic-informer event handlers). Invoke via `go run` on the
# //go:build ignore program — no build step, no extra deps.
go run scripts/lint/no_unchecked_unstructured_assert.go --root="$(pwd)/internal/cache"
