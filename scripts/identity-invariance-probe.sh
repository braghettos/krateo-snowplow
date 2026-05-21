#!/usr/bin/env bash
# identity-invariance-probe.sh
#
# Ship G OQ-2 falsifier: prove that snowplow's resolved widget /call output for
# nav + RoutesLoader is identical under admin vs cyberjoker, after normalising
# non-identity fields (status.traceId, items[].allowed).
#
# Two distinct widget /call endpoints, each probed under admin AND cyberjoker:
#   1. NavMenu          /call?resource=navmenus&name=sidebar-nav-menu&namespace=krateo-system
#   2. RoutesLoader     /call?resource=routesloaders&name=routes-loader&namespace=krateo-system
#
# Zero diff across both endpoints (after normalisation) == Branch A confirmed.
# Any residue == Branch B re-gate required.
#
# Required env:
#   ADMIN_TOKEN  Bearer JWT for admin
#   CJ_TOKEN     Bearer JWT for cyberjoker
# Optional env:
#   BASE         snowplow base URL (default: http://34.135.50.203:8081)
#   OUT          output dir for normalised dumps + diff transcripts
#
# Usage:
#   ADMIN_TOKEN=... CJ_TOKEN=... ./identity-invariance-probe.sh
#   OUT=/tmp/snowplow-runs/0.30.160/before ADMIN_TOKEN=... CJ_TOKEN=... ./identity-invariance-probe.sh

set -euo pipefail

: "${ADMIN_TOKEN:?must set ADMIN_TOKEN}"
: "${CJ_TOKEN:?must set CJ_TOKEN}"
: "${BASE:=http://34.135.50.203:8081}"
: "${OUT:=/tmp/snowplow-runs/0.30.160/before}"

CURL=${CURL:-/usr/bin/curl}
JQ=${JQ:-jq}

mkdir -p "$OUT/identity-probe"

# Normalise: drop status.traceId and null-out every items[].allowed so identity
# (user-specific permission) cannot show up as a content diff.
NORMALISE_FILTER='del(.status.traceId) | (.status.resourcesRefs.items[]?.allowed) |= null'

# Endpoint table:  label  resource  name                namespace
endpoints=(
  "nav         navmenus       sidebar-nav-menu  krateo-system"
  "rl          routesloaders  routes-loader     krateo-system"
)

pass=0
fail=0
declare -a fail_details

for row in "${endpoints[@]}"; do
  # shellcheck disable=SC2086
  set -- $row
  label="$1"; resource="$2"; name="$3"; namespace="$4"

  path="/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=${resource}&name=${name}&namespace=${namespace}"

  admin_raw="$OUT/identity-probe/${label}-admin.raw.json"
  cj_raw="$OUT/identity-probe/${label}-cj.raw.json"
  admin_norm="$OUT/identity-probe/${label}-admin.normalised.json"
  cj_norm="$OUT/identity-probe/${label}-cj.normalised.json"
  diff_file="$OUT/identity-probe/${label}.diff"

  $CURL -s --max-time 60 -H "Authorization: Bearer $ADMIN_TOKEN" "$BASE$path" > "$admin_raw"
  $CURL -s --max-time 60 -H "Authorization: Bearer $CJ_TOKEN"    "$BASE$path" > "$cj_raw"

  $JQ -S "$NORMALISE_FILTER" < "$admin_raw" > "$admin_norm"
  $JQ -S "$NORMALISE_FILTER" < "$cj_raw"    > "$cj_norm"

  if diff -u "$admin_norm" "$cj_norm" > "$diff_file"; then
    echo "PASS  ${label}-admin vs ${label}-cj  (0 bytes diff after normalisation)"
    pass=$((pass+1))
  else
    bytes=$(wc -c < "$diff_file" | tr -d ' ')
    # Extract first JSON path that differs (best-effort).
    first_path=$($JQ -S -n --slurpfile a "$admin_norm" --slurpfile b "$cj_norm" '
      def paths_of(x): [paths(scalars)];
      [paths_of($a[0])] as $pa
      | [paths_of($b[0])] as $pb
      | reduce ($pa[0][] + $pb[0][] | unique[]) as $p (null;
          if . == null and ($a[0] | getpath($p)) != ($b[0] | getpath($p))
          then $p else . end)
      | if . == null then "<structural>" else ($p|join(".")) end
    ' 2>/dev/null || echo "<unknown>")
    sample=$(head -c 200 "$diff_file" | tr '\n' ' ')
    echo "FAIL  ${label}-admin vs ${label}-cj  bytes=${bytes}  first_path=${first_path}"
    fail=$((fail+1))
    fail_details+=("endpoint=${label} bytes=${bytes} first_path=${first_path} sample=${sample}")
  fi
done

echo
echo "=== OQ-2 SUMMARY ==="
echo "endpoints_probed=2 (nav, rl) × 2 users (admin, cj) = 4 captures"
echo "pairs_compared=2 (nav-admin↔nav-cj, rl-admin↔rl-cj)"
echo "pass=${pass} fail=${fail}"
echo

if [[ $fail -eq 0 ]]; then
  echo "OQ-2 PASS: 0 bytes diff across {nav-admin↔nav-cj, rl-admin↔rl-cj} after normalising status.traceId + items[].allowed. Branch A confirmed. Dev unblocked."
  exit 0
else
  for d in "${fail_details[@]}"; do
    echo "DETAIL  $d"
  done
  echo
  echo "OQ-2 FAIL: ${fail} pair(s) diverged after normalisation. Branch B re-gate required."
  exit 1
fi
