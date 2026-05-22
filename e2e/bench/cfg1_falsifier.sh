#!/usr/bin/env bash
# cfg1_falsifier.sh — HG-321 (Ship CFG-1 / 0.30.163) integration
# falsifier. Spawns the cfg1_probe binary 4 times, once per env-value
# in the matrix {true, false, unset, invalid}, hits /debug/vars per
# spawn, and asserts the snowplow_* cache key set against the
# CFG-1 contract:
#
#   CACHE_ENABLED=true     → all 5 keys present
#   CACHE_ENABLED=false    → 0 keys present
#   CACHE_ENABLED (unset)  → 0 keys present
#   CACHE_ENABLED=invalid  → 0 keys present  (Disabled() default-deny)
#
# WHY a separate process per env value:
#   - The expvar registration is at package init() time.
#   - init() reads CACHE_ENABLED via cache.Disabled() ONCE per
#     process. Once a key is published, it cannot be unpublished
#     (expvar has no Unpublish). Therefore the matrix MUST be tested
#     by spawning N separate processes; an in-process unit test
#     cannot falsify this.
#
# This script is THE unit-test for the cache-off compliance contract
# (per `project_cache_off_is_transparent_fallback`); the in-process
# Go test (TestFallthroughScope_E2E_ExpvarHandler) only validates the
# cache-on side.
#
# Usage:
#   ./cfg1_falsifier.sh                # build + run all 4 cases
#   PORT_BASE=29000 ./cfg1_falsifier.sh
#
# Exit 0 = all 4 cases match contract; non-zero = at least one fails.

set -euo pipefail

# ─── config ────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." &>/dev/null && pwd)"
PROBE_BIN="${PROBE_BIN:-/tmp/cfg1_probe}"
PORT_BASE="${PORT_BASE:-28980}"
SETTLE_SEC="${SETTLE_SEC:-2}"

# The 5 cache expvar keys CFG-1 governs.
CACHE_KEYS=(
  "snowplow_apiserver_fallthrough_total"
  "snowplow_assertion_violations_total"
  "snowplow_apiserver_fallthrough_cells"
  "snowplow_upstream_controller_health"
  "snowplow_upstream_webhook_failurepolicy"
)

# ─── build ─────────────────────────────────────────────────────────────
echo "==> Building cfg1_probe → ${PROBE_BIN}"
( cd "${REPO_ROOT}" && go build -o "${PROBE_BIN}" ./e2e/bench/cfg1_probe/ )

# ─── helpers ───────────────────────────────────────────────────────────
# spawn_and_probe <label> <env-value-action> <port> <expect_keys_present>
#   label:                 human label for output
#   env-value-action:      one of "set:true", "set:false", "unset",
#                          "set:invalid"
#   port:                  port to bind /debug/vars
#   expect_keys_present:   "5" or "0"
#
# Returns 0 if observed key count matches expectation, 1 otherwise.
spawn_and_probe() {
  local label="$1" action="$2" port="$3" expect="$4"
  echo "─────────────────────────────────────────────────────────────────"
  echo "Case: ${label}  (port ${port}, expect ${expect} cache keys)"

  local pidfile
  pidfile="$(mktemp -t cfg1_probe_pid.XXXXXX)"

  # Spawn in subshell with the env arrangement.
  case "${action}" in
    set:true)
      ( CACHE_ENABLED=true "${PROBE_BIN}" -addr=":${port}" >"/tmp/cfg1_probe_${port}.log" 2>&1 ) &
      ;;
    set:false)
      ( CACHE_ENABLED=false "${PROBE_BIN}" -addr=":${port}" >"/tmp/cfg1_probe_${port}.log" 2>&1 ) &
      ;;
    unset)
      ( env -u CACHE_ENABLED "${PROBE_BIN}" -addr=":${port}" >"/tmp/cfg1_probe_${port}.log" 2>&1 ) &
      ;;
    set:invalid)
      ( CACHE_ENABLED=invalid "${PROBE_BIN}" -addr=":${port}" >"/tmp/cfg1_probe_${port}.log" 2>&1 ) &
      ;;
    *)
      echo "unknown action: ${action}" >&2
      return 2
      ;;
  esac
  local probe_pid=$!
  echo "${probe_pid}" >"${pidfile}"

  # Settle (port bind + init complete).
  sleep "${SETTLE_SEC}"

  # Probe /debug/vars.
  local body
  if ! body="$(curl --max-time 5 -sf "http://127.0.0.1:${port}/debug/vars")"; then
    echo "FAIL: curl /debug/vars failed for ${label}"
    kill "${probe_pid}" 2>/dev/null || true
    wait "${probe_pid}" 2>/dev/null || true
    return 1
  fi

  # Count which cache keys are present.
  local found=0 missing_keys=() present_keys=()
  for key in "${CACHE_KEYS[@]}"; do
    if echo "${body}" | grep -q "\"${key}\""; then
      found=$((found + 1))
      present_keys+=("${key}")
    else
      missing_keys+=("${key}")
    fi
  done

  echo "Observed: ${found}/5 cache keys present at /debug/vars"
  if [ "${#present_keys[@]}" -gt 0 ]; then
    echo "  present: ${present_keys[*]}"
  fi
  if [ "${#missing_keys[@]}" -gt 0 ] && [ "${found}" -lt 5 ]; then
    echo "  absent : ${missing_keys[*]}"
  fi

  # Cleanup.
  kill "${probe_pid}" 2>/dev/null || true
  wait "${probe_pid}" 2>/dev/null || true
  rm -f "${pidfile}"

  # Verdict.
  if [ "${found}" -ne "${expect}" ]; then
    echo "FAIL: ${label} — got ${found} keys, expected ${expect}"
    return 1
  fi
  echo "PASS: ${label}"
  return 0
}

# ─── matrix ────────────────────────────────────────────────────────────
fail_count=0

spawn_and_probe "CACHE_ENABLED=true"     "set:true"    "$((PORT_BASE + 0))" 5 || fail_count=$((fail_count + 1))
spawn_and_probe "CACHE_ENABLED=false"    "set:false"   "$((PORT_BASE + 1))" 0 || fail_count=$((fail_count + 1))
spawn_and_probe "CACHE_ENABLED (unset)"  "unset"       "$((PORT_BASE + 2))" 0 || fail_count=$((fail_count + 1))
spawn_and_probe "CACHE_ENABLED=invalid"  "set:invalid" "$((PORT_BASE + 3))" 0 || fail_count=$((fail_count + 1))

echo "─────────────────────────────────────────────────────────────────"
if [ "${fail_count}" -eq 0 ]; then
  echo "HG-321 PASS: all 4 env-value cases match CFG-1 contract"
  exit 0
fi
echo "HG-321 FAIL: ${fail_count}/4 cases violated CFG-1 contract"
exit 1
