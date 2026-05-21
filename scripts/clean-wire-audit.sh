#!/usr/bin/env bash
# clean-wire-audit.sh
#
# HG-6 falsifier: scan a content corpus directory for prohibited wire patterns.
# Trips on any match — captures the offending file:line:pattern for triage.
#
# Patterns scanned:
#   eyJ                    (JWT prefix — token leak in response body)
#   Authorization: Bearer  (header-style auth leak)
#   userAccessFilter       (internal RBAC narrowing leaked to wire)
#   _cacheKey              (NEW Ship G — _cacheKey leakage from L1 cache layer)
#
# Usage:
#   ./clean-wire-audit.sh [corpus_dir]
#   corpus_dir defaults to $PWD/content-corpus

set -euo pipefail

DIR=${1:-${OUT:-$PWD/content-corpus}}

if [[ ! -d "$DIR" ]]; then
  echo "ERROR: corpus dir not found: $DIR" >&2
  exit 2
fi

# Patterns are checked literally (fixed-string) to avoid regex false positives.
PATTERNS=(
  "eyJ"
  "Authorization: Bearer"
  "userAccessFilter"
  "_cacheKey"
)

total_hits=0
declare -a hits

for pat in "${PATTERNS[@]}"; do
  # grep -F fixed-string, -r recursive, -n line-number, -H file header.
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    hits+=("pattern=\"$pat\" $line")
    total_hits=$((total_hits + 1))
  done < <(grep -F -r -n -H "$pat" "$DIR" 2>/dev/null || true)
done

echo "=== clean-wire-audit (corpus=$DIR) ==="
echo "patterns: ${PATTERNS[*]}"
echo "files_scanned: $(find "$DIR" -type f | wc -l | tr -d ' ')"
echo "total_hits: $total_hits"

if [[ $total_hits -eq 0 ]]; then
  echo "HG-6 PASS  no prohibited patterns found in corpus"
  exit 0
fi

echo
echo "HG-6 FAIL  prohibited patterns detected:"
for h in "${hits[@]}"; do
  echo "  $h"
done
exit 1
