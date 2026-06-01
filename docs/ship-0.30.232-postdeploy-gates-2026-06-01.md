# Ship 0.30.232 — post-deploy gate sequence (HARD)

date: 2026-06-01
ship: 0.30.232 — type-safety phase1Walker constructor (attempt #7)
predecessor: 0.30.231 (production helm rev 404 = 0.30.227 binary; lockstep chart at 0.30.231 was the prior dev iteration)
pm-conditional-authorize: 2026-06-01 final gate

This is the binding post-deploy gate sequence for 0.30.232. Each gate
is HARD: ANY of the 8 rollback triggers below fires an IMMEDIATE helm
rollback (no investigation, no "let me check one more thing").

## Pre-deploy baseline capture

Capture the error-log rate from the running 0.30.227 binary (helm rev
404) BEFORE the upgrade. The trigger-8 comparison needs a pre-deploy
5-min baseline.

```bash
POD_PRE=$(kubectl -n krateo-system get pod -l app.kubernetes.io/name=snowplow -o jsonpath='{.items[0].metadata.name}')
# Confirm GKE context (feedback_kubectl_verify_gke_context)
kubectl config current-context | grep -q 'gke_neon-481711_us-central1-a_cluster-1' \
  || { echo "WRONG CLUSTER — abort"; exit 1; }

# Pre-deploy 5-min error-log rate baseline
kubectl -n krateo-system logs "$POD_PRE" --tail=20000 --since=5m 2>/dev/null \
  | grep -iE '\b(error|err|panic|fatal)\b' | wc -l > /tmp/0.30.232-baseline-err-5min.txt
BASELINE_5MIN=$(cat /tmp/0.30.232-baseline-err-5min.txt)
echo "pre-deploy 5-min error rate: $BASELINE_5MIN"
# 2× threshold for trigger-8 (sustained over any 60s window)
TRIGGER8_60S_THRESHOLD=$(echo "$BASELINE_5MIN * 2 / 5" | bc)  # per-min equivalent
echo "trigger-8 60s window threshold: $TRIGGER8_60S_THRESHOLD events/60s"
```

## Helm upgrade

```bash
# Chart-side lockstep tag 0.30.232 must be pushed to braghettos FIRST
# (feedback_chart_repo_origin_is_upstream). Then helm upgrade with
# FULL --set per feedback_helm_no_reuse_values_on_chart_default_change.
# (Exact --set list captured in chart README at the lockstep tag.)
```

## The 8 rollback triggers (HARD — no investigation)

| # | trigger | check point |
|---|---|---|
| 1 | Pod not Ready within 90s of helm upgrade | `kubectl wait --for=condition=Ready --timeout=90s` |
| 2 | `rewalk_complete` event absent at 60s post-Ready | log grep |
| 3 | Any of {roots, rewalked, restactions, widgets} = 0 in rewalk_complete event | log JSON parse |
| 4 | `rewalked != len(roots)` in rewalk_complete event | log JSON parse |
| 5 | Any `plurals discovery: nil` log line in first 5 min | log grep |
| 6 | First cj /call shows `resolver-plurals-miss > 0` | /debug/vars probe |
| 7 | Pod restart at any point in 30-min soak | restartCount poll |
| 8 | Error-log rate >2× pre-deploy 5-min baseline sustained over any 60s window | rolling log-rate window |

Rollback path: `helm rollback portal <prev-revision>` (chart lockstep
per [feedback_chart_release_lockstep]). Do NOT `kubectl set image`.
Do NOT `kubectl apply` (feedback_never_kubectl_apply).

If rollback fires: append to `memory/project_regression_journal.md`
with the gate that tripped + artifact path BEFORE any further attempt.

## The 6-gate post-deploy sequence

### Step 1 — Pod Ready (90s budget)

```bash
POD=$(kubectl -n krateo-system get pod -l app.kubernetes.io/name=snowplow -o jsonpath='{.items[0].metadata.name}')
kubectl -n krateo-system wait --for=condition=Ready pod/"$POD" --timeout=90s
WAIT_EXIT=$?
RC=$(kubectl -n krateo-system get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')

if [ "$WAIT_EXIT" != "0" ] || [ "$RC" != "0" ]; then
  echo "FAIL trigger-1 — HARD ROLLBACK"
  helm rollback portal <prev-rev> -n krateo-system
  exit 1
fi
echo "Step 1 PASS — pod $POD Ready, restartCount=$RC"
```

### Step 2 — rewalk_complete event at 60s post-Ready

```bash
sleep 60
REWALK=$(kubectl -n krateo-system logs "$POD" | grep rewalk_complete | tail -1)
if [ -z "$REWALK" ]; then
  echo "FAIL trigger-2 — rewalk_complete absent — HARD ROLLBACK"
  helm rollback portal <prev-rev> -n krateo-system
  exit 1
fi
echo "Step 2 PASS — rewalk_complete: $REWALK"
```

### Step 3 — rewalk_complete field sanity (triggers 3 + 4)

```bash
# Parse roots, rewalked, restactions, widgets from the slog JSON line.
ROOTS=$(echo "$REWALK" | python3 -c "import sys,json,re; m=re.search(r'\{.*\}', sys.stdin.read()); d=json.loads(m.group()); print(d.get('roots',0))")
REWALKED=$(echo "$REWALK" | python3 -c "import sys,json,re; m=re.search(r'\{.*\}', sys.stdin.read()); d=json.loads(m.group()); print(d.get('rewalked',0))")
RESTACTIONS=$(echo "$REWALK" | python3 -c "import sys,json,re; m=re.search(r'\{.*\}', sys.stdin.read()); d=json.loads(m.group()); print(d.get('restactions',0))")
WIDGETS=$(echo "$REWALK" | python3 -c "import sys,json,re; m=re.search(r'\{.*\}', sys.stdin.read()); d=json.loads(m.group()); print(d.get('widgets',0))")

if [ "$ROOTS" = "0" ] || [ "$REWALKED" = "0" ] || [ "$RESTACTIONS" = "0" ] || [ "$WIDGETS" = "0" ]; then
  echo "FAIL trigger-3 — empty field in rewalk_complete (roots=$ROOTS rewalked=$REWALKED restactions=$RESTACTIONS widgets=$WIDGETS) — HARD ROLLBACK"
  helm rollback portal <prev-rev> -n krateo-system
  exit 1
fi

if [ "$REWALKED" != "$ROOTS" ]; then
  echo "FAIL trigger-4 — rewalked=$REWALKED != roots=$ROOTS — HARD ROLLBACK"
  helm rollback portal <prev-rev> -n krateo-system
  exit 1
fi
echo "Step 3 PASS — roots=$ROOTS rewalked=$REWALKED restactions=$RESTACTIONS widgets=$WIDGETS"
```

### Step 4 — zero plurals-discovery-nil log lines in first 5 min (trigger 5)

```bash
# Step 1+2 burned 90s+60s = 150s; sleep remainder to hit 5-min post-Ready.
sleep 150
NIL_HITS=$(kubectl -n krateo-system logs "$POD" --since=5m | grep -c "plurals discovery: nil" || true)
if [ "$NIL_HITS" != "0" ]; then
  echo "FAIL trigger-5 — $NIL_HITS plurals-discovery-nil log lines — HARD ROLLBACK"
  helm rollback portal <prev-rev> -n krateo-system
  exit 1
fi
echo "Step 4 PASS — zero plurals-discovery-nil log lines in 5 min"
```

### Step 5 — first cj /call PieChart resolver-plurals-miss=0 (trigger 6)

```bash
kubectl -n krateo-system port-forward "$POD" 8181:8081 >/dev/null 2>&1 &
PF=$!
sleep 3

JWT_CJ=$(cat /tmp/cyb-jwt-0.30.221.txt)
LB_EXT=http://34.135.50.203:8081
curl -s -H "Authorization: Bearer $JWT_CJ" \
  "$LB_EXT/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=piecharts&name=dashboard-compositions-panel-row-piechart&namespace=krateo-system" \
  > /dev/null 2>&1

sleep 5
MISS=$(curl -s http://localhost:8181/debug/vars | python3 -c "
import json, sys
d = json.load(sys.stdin)
fall = d.get('snowplow_apiserver_fallthrough_cells', {})
miss = [v for k,v in fall.items() if 'resolver-plurals-miss' in k and 'Kind=PieChart' in k]
print(sum(miss) if miss else 0)
")

kill $PF 2>/dev/null
if [ "$MISS" != "0" ]; then
  echo "FAIL trigger-6 — PieChart resolver-plurals-miss=$MISS — HARD ROLLBACK"
  helm rollback portal <prev-rev> -n krateo-system
  exit 1
fi
echo "Step 5 PASS — PieChart resolver-plurals-miss=$MISS"
```

### Step 6 — 30-min soak (PM CORRECTION 1 — verbatim contract)

> **Step 6 — 30-min soak**: zero pod restart, zero `plurals discovery: nil` log lines, zero resolver-plurals-miss>0 events on cj /call, no error-log spike vs pre-deploy baseline (>2× pre-deploy 5-min rate sustained over any 60s window).

```bash
# Soak from now (post-Step-5) until +30min total (~25 min of new soak after the ~5min that already elapsed).
SOAK_START=$(date +%s)
RC_START=$(kubectl -n krateo-system get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')

# Polling loop: every 60s check triggers 5, 6, 7, 8 over the trailing window.
# Phase B 0.30.185 amplification was visible at ~5 min, catastrophic at 15 min.
END_TS=$((SOAK_START + 1800))  # 30 min from soak start
while [ "$(date +%s)" -lt "$END_TS" ]; do
  sleep 60

  # trigger 7 — pod restart
  RC_NOW=$(kubectl -n krateo-system get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')
  if [ "$RC_NOW" != "$RC_START" ]; then
    echo "FAIL trigger-7 — pod restarted during soak (restartCount $RC_START → $RC_NOW) — HARD ROLLBACK"
    helm rollback portal <prev-rev> -n krateo-system
    exit 1
  fi

  # trigger 5 — plurals-discovery-nil over the last 60s
  NIL_60S=$(kubectl -n krateo-system logs "$POD" --since=60s | grep -c "plurals discovery: nil" || true)
  if [ "$NIL_60S" != "0" ]; then
    echo "FAIL trigger-5 (soak) — $NIL_60S plurals-discovery-nil in last 60s — HARD ROLLBACK"
    helm rollback portal <prev-rev> -n krateo-system
    exit 1
  fi

  # trigger 8 — error-log rate over last 60s window vs 2× baseline-per-minute
  ERR_60S=$(kubectl -n krateo-system logs "$POD" --since=60s 2>/dev/null \
    | grep -iE '\b(error|err|panic|fatal)\b' | wc -l)
  if [ "$ERR_60S" -gt "$TRIGGER8_60S_THRESHOLD" ]; then
    echo "FAIL trigger-8 — error-log rate $ERR_60S in 60s > threshold $TRIGGER8_60S_THRESHOLD (2× pre-deploy baseline-per-min) — HARD ROLLBACK"
    helm rollback portal <prev-rev> -n krateo-system
    exit 1
  fi

  # trigger 6 — periodic cj /call to verify resolver-plurals-miss stays 0
  # Probe every 5 minutes during the soak (not every 60s — avoids load on portal).
  ELAPSED=$(( $(date +%s) - SOAK_START ))
  if [ $((ELAPSED % 300)) -lt 60 ]; then
    kubectl -n krateo-system port-forward "$POD" 8181:8081 >/dev/null 2>&1 &
    PF=$!
    sleep 3
    curl -s -H "Authorization: Bearer $JWT_CJ" \
      "$LB_EXT/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=piecharts&name=dashboard-compositions-panel-row-piechart&namespace=krateo-system" \
      > /dev/null 2>&1
    sleep 3
    MISS_SOAK=$(curl -s http://localhost:8181/debug/vars | python3 -c "
import json, sys
d = json.load(sys.stdin)
fall = d.get('snowplow_apiserver_fallthrough_cells', {})
miss = [v for k,v in fall.items() if 'resolver-plurals-miss' in k and 'Kind=PieChart' in k]
print(sum(miss) if miss else 0)
")
    kill $PF 2>/dev/null
    if [ "$MISS_SOAK" != "0" ]; then
      echo "FAIL trigger-6 (soak) — resolver-plurals-miss=$MISS_SOAK — HARD ROLLBACK"
      helm rollback portal <prev-rev> -n krateo-system
      exit 1
    fi
    echo "  soak +${ELAPSED}s: trigger-6 PASS (miss=0)"
  fi

  echo "  soak +${ELAPSED}s: trigger-5/7/8 PASS"
done

echo "Step 6 PASS — 30-min soak clean"
```

## On soak completion (clean)

Append to `memory/project_feature_journal.md` per [feedback_maintain_feature_journal]:
- ship 0.30.232 — type-safety phase1Walker constructor
- expected: zero nil-rc surface; no perf delta
- test: 6-gate sequence
- actual: <results>
- delta vs expected: <delta>

## On rollback (any trigger)

Append to `memory/project_regression_journal.md` per [feedback_maintain_regression_journal]:
- date, ship, trigger that fired, artifact path
- how-found, root-cause, fix, prevention

BEFORE any further attempt.
