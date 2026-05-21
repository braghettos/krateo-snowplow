#!/usr/bin/env bash
# capture-content-corpus.sh
#
# Re-capture the 12-item content corpus matching the shape of
# /tmp/snowplow-runs/0.30.152/before/content-corpus/:
#
#   apis-admin.bin               apis-cj.bin
#   nav-admin.bin                nav-cj.bin
#   page-blueprints-admin.bin    page-blueprints-cj.bin
#   page-compositions-admin.bin  page-compositions-cj.bin
#   page-dashboard-admin.bin     page-dashboard-cj.bin
#   panel-comp-admin.bin         panel-comp-cj.bin
#   rl-admin.bin                 rl-cj.bin
#
# All captures hit kube-apiserver directly via `kubectl get --raw`, exercising
# the wire shape upstream of snowplow's /call? resolver. The corpus is then
# scanned by clean-wire-audit.sh for token leaks / RBAC-internal fields /
# (new this ship) _cacheKey leakage.
#
# `admin` rows use the operator's kubeconfig (cluster-admin); `cj` rows use a
# kubeconfig contextualised to ServiceAccount krateo-system/cyberjoker — this
# replicates the 0.30.152 admin-vs-cj asymmetry visible in the original fixture
# (e.g. panel-comp-admin.bin == 7825 bytes content vs panel-comp-cj.bin == 353
# bytes Forbidden Status).
#
# Optional env:
#   OUT           output dir (default: $PWD/content-corpus)
#   PANEL_NS      bench namespace for panel-comp probe (default: bench-ns-03)
#   PANEL_NAME    panel name (default matches existing 0.30.152 fixture)
#   CJ_SA_NAME    cyberjoker SA name (default: cyberjoker)
#   CJ_SA_NS      cyberjoker SA namespace (default: krateo-system)

set -euo pipefail

: "${OUT:=$PWD/content-corpus}"
: "${PANEL_NS:=bench-ns-03}"
: "${PANEL_NAME:=githubscaffoldingwithcompositionpage-bench-app-03-980-composition-panel}"
: "${CJ_KUBECONFIG:?must set CJ_KUBECONFIG (kubeconfig with cyberjoker mTLS identity)}"

mkdir -p "$OUT"

get() {
  local fname="$1" user="$2" path="$3"
  local kconf raw
  if [[ "$user" = admin ]]; then
    kconf="${KUBECONFIG:-$HOME/.kube/config}"
  else
    kconf="$CJ_KUBECONFIG"
  fi
  raw=$(kubectl --kubeconfig="$kconf" get --raw "$path" 2>&1 || true)
  # 0.30.152 fixture is 2-space pretty-printed JSON; non-JSON errors are stored
  # verbatim (e.g. "Error from server (Forbidden): ..." → minted Status object).
  if [[ "$raw" == "{"* ]]; then
    # JSON response — pretty-print via jq for byte-for-byte fixture parity.
    printf '%s' "$raw" | jq . > "$OUT/$fname"
  elif [[ "$raw" == *"Forbidden"* ]]; then
    # Mint a Status JSON matching the 0.30.152 panel-comp-cj.bin shape.
    printf '%s' "$raw" \
      | python3 -c '
import sys,json,re
msg = sys.stdin.read().strip()
# Extract user/resource/ns from the kubectl error text.
m = re.search(r"\"([^\"]+)\" is forbidden: User \"([^\"]+)\" cannot get resource \"([^\"]+)\" in API group \"([^\"]*)\" in the namespace \"([^\"]+)\"", msg)
if m:
  obj = {"kind":"Status","apiVersion":"v1","status":"Failure",
         "message": f"{m.group(3)}.{m.group(4)} \"{m.group(1)}\" is forbidden: User \"{m.group(2)}\" cannot get resource \"{m.group(3)}\" in API group \"{m.group(4)}\" in the namespace \"{m.group(5)}\"",
         "reason":"Forbidden","code":403}
else:
  obj = {"kind":"Status","apiVersion":"v1","status":"Failure","message":msg,"reason":"Forbidden","code":403}
print(json.dumps(obj))' > "$OUT/$fname"
  else
    # 404 path-not-found or other plain-text error
    printf '%s\n' "$raw" > "$OUT/$fname"
  fi
  echo "$fname size=$(wc -c < "$OUT/$fname")"
}

# 1. apis (snowplow root /apis — Go stdlib default 404 page; kept for fixture
#    shape parity with 0.30.152). Captured directly via curl against the
#    snowplow LB rather than kubectl --raw.
: "${SNOWPLOW_BASE:=http://34.135.50.203:8081}"
: "${ADMIN_TOKEN:?must set ADMIN_TOKEN for snowplow /apis probe}"
: "${CJ_TOKEN:?must set CJ_TOKEN for snowplow /apis probe}"
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "$SNOWPLOW_BASE/apis" > "$OUT/apis-admin.bin"
echo "apis-admin.bin size=$(wc -c < "$OUT/apis-admin.bin")"
curl -s -H "Authorization: Bearer $CJ_TOKEN" "$SNOWPLOW_BASE/apis" > "$OUT/apis-cj.bin"
echo "apis-cj.bin size=$(wc -c < "$OUT/apis-cj.bin")"

# 2. NavMenu
get nav-admin.bin admin "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/navmenus/sidebar-nav-menu"
get nav-cj.bin    cj    "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/navmenus/sidebar-nav-menu"

# 3. Page: blueprints
get page-blueprints-admin.bin admin "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/pages/blueprints-page"
get page-blueprints-cj.bin    cj    "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/pages/blueprints-page"

# 4. Page: compositions
get page-compositions-admin.bin admin "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/pages/compositions-page"
get page-compositions-cj.bin    cj    "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/pages/compositions-page"

# 5. Page: dashboard
get page-dashboard-admin.bin admin "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/pages/dashboard-page"
get page-dashboard-cj.bin    cj    "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/pages/dashboard-page"

# 6. Panel (composition panel — admin should fetch, cj should 403)
get panel-comp-admin.bin admin "/apis/widgets.templates.krateo.io/v1beta1/namespaces/${PANEL_NS}/panels/${PANEL_NAME}"
get panel-comp-cj.bin    cj    "/apis/widgets.templates.krateo.io/v1beta1/namespaces/${PANEL_NS}/panels/${PANEL_NAME}"

# 7. RoutesLoader
get rl-admin.bin admin "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/routesloaders/routes-loader"
get rl-cj.bin    cj    "/apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/routesloaders/routes-loader"

# SHA256 manifest (raw)
(
  cd "$OUT"
  sha256sum *.bin | sort > SHA256SUMS_raw.txt
)
echo
echo "captured $(ls "$OUT"/*.bin | wc -l | tr -d ' ') corpus files → $OUT"
echo "manifest: $OUT/SHA256SUMS_raw.txt"
