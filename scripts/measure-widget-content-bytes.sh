#!/usr/bin/env bash
# measure-widget-content-bytes.sh
#
# AC-G.14 bound: for every widget the F2 walker reaches, capture the raw
# kube-apiserver response byte count. One line per widget:
#
#   <group>/<version>:<namespace>/<resource>/<name>: <bytes>
#
# This is the empirical per-entry cost feed for the capacity-cap derivation
# (per feedback_capacity_caps_empirical_per_entry_cost: derive caps from
# observed costs × count × safety multiplier, never from design-time guesses).
#
# Scope: widgets reached by the F2 walker = NavMenu + RoutesLoader root frontier
# expanded to their NavMenuItems / Routes children + Page (dashboard/compositions/
# blueprints) + Panels under bench namespaces.
#
# Output: one stdout line per widget; final line is the running total.
#
# Optional env:
#   PANEL_NS              bench namespace pattern (default: bench-ns-)
#   PANEL_COUNT           how many bench namespaces to probe (default: 5)
#   PER_NS_PANEL_LIMIT    cap panels-per-namespace (default: 5; -1 = no cap).
#                         At production scale a single bench-ns can carry 1k+
#                         panels; the byte-size distribution is well-sampled
#                         long before exhausting them.
#   OUT_FILE              where to mirror the output (default: $PWD/widget_entry_byte_sizes.txt)
#   KUBECONFIG            kubeconfig with cluster-admin (default: ~/.kube/config)

set -euo pipefail

: "${PANEL_NS:=bench-ns-}"
: "${PANEL_COUNT:=5}"
: "${PER_NS_PANEL_LIMIT:=5}"
: "${OUT_FILE:=$PWD/widget_entry_byte_sizes.txt}"

probe() {
  # probe <group/version> <namespace> <resource> <name>
  local gv="$1" ns="$2" res="$3" name="$4"
  local path="/apis/${gv}/namespaces/${ns}/${res}/${name}"
  local bytes
  bytes=$(kubectl get --raw "$path" 2>/dev/null | wc -c | tr -d ' ')
  echo "${gv}:${ns}/${res}/${name}: ${bytes}"
}

list_resource() {
  # list_resource <group/version> <namespace> <resource>
  # Returns one "name" per line for that GVR in that namespace.
  local gv="$1" ns="$2" res="$3"
  kubectl get --raw "/apis/${gv}/namespaces/${ns}/${res}" 2>/dev/null \
    | python3 -c 'import sys,json,signal
signal.signal(signal.SIGPIPE, signal.SIG_DFL)
try:
  d=json.load(sys.stdin)
  for it in d.get("items",[]):
    n=it.get("metadata",{}).get("name")
    if n: print(n)
except Exception:
  pass' 2>/dev/null || true
}

WIDGETS_GV="widgets.templates.krateo.io/v1beta1"

{
  echo "# widget_entry_byte_sizes — captured $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# format: <group/version>:<namespace>/<resource>/<name>: <bytes>"

  # NavMenu root + items
  probe "$WIDGETS_GV" krateo-system navmenus sidebar-nav-menu
  for nmi in $(list_resource "$WIDGETS_GV" krateo-system navmenuitems); do
    probe "$WIDGETS_GV" krateo-system navmenuitems "$nmi"
  done

  # RoutesLoader root + routes children
  probe "$WIDGETS_GV" krateo-system routesloaders routes-loader
  for r in $(list_resource "$WIDGETS_GV" krateo-system routes); do
    probe "$WIDGETS_GV" krateo-system routes "$r"
  done

  # Pages (dashboard / compositions / blueprints)
  for pg in dashboard-page compositions-page blueprints-page; do
    probe "$WIDGETS_GV" krateo-system pages "$pg"
  done

  # Panels in bench namespaces (sample PANEL_COUNT namespaces, capped per-ns).
  for i in $(seq -f "%02g" 1 "$PANEL_COUNT"); do
    ns="${PANEL_NS}${i}"
    if [[ "$PER_NS_PANEL_LIMIT" -lt 0 ]]; then
      panels=$(list_resource "$WIDGETS_GV" "$ns" panels)
    else
      panels=$(list_resource "$WIDGETS_GV" "$ns" panels | head -n "$PER_NS_PANEL_LIMIT")
    fi
    for p in $panels; do
      probe "$WIDGETS_GV" "$ns" panels "$p"
    done
  done
} | tee "$OUT_FILE"

# Summary line
total=$(awk -F': ' '/^[^#]/ {gsub(/[^0-9]/,"",$NF); s += $NF} END {print s}' "$OUT_FILE")
count=$(grep -c -v '^#' "$OUT_FILE" || true)
echo "# TOTAL widgets=${count} bytes=${total}" | tee -a "$OUT_FILE"
