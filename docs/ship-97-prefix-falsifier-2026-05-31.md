# Ship #97 — Phase 1 Pre-flight Falsifier

Signed: cache-developer. Date: 2026-05-31. Status: PROCEED.

## Cluster context (verified)

- kubectl context: `gke_neon-481711_us-central1-a_cluster-1` (verified)
- Snowplow image at capture: `ghcr.io/braghettos/snowplow:0.30.212`
- Pod: `snowplow-594b5db8bf-57mrv`, restartCount=0
- Pod CPU at capture start: 5.3 cores; sustained 5.5–5.9 cores
- Refresher queue depth at capture start: 0 (steady-state, no enqueue spike)
- Refresher counters (pre-capture): completed=40,857, enqueue=130,634, cohort_memo_entries=99
- 50,000 compositions + 29,907 ArgoCD Applications (production scale per `project_argocd_apps_scale.md`)
- Bench process: absent

## Methodology

- Portal LB used for /call drive (NOT port-forward), per `feedback_no_kubectl_in_measurement`.
- kubectl port-forward used ONLY for `/debug/pprof` capture (diagnostic, allowed per narrow exemption).
- JWTs acquired via `AUTHN/basic/login` (passwords read from `admin-password` + `cyberjoker-password` secrets).
- 60s CPU profile captured during admin + cyberjoker /call drive on dashboard piechart RAs.
- Drive script: `/tmp/ship97-prefix-falsifier.sh`.

NOTE: The drive script's `wait` inside the loop stalled (curl backpressure on the snowplow LB under load); however the 60s CPU profile completed successfully with active drive PLUS refresher steady-state, which is the exact signal needed.

## Pre-fix CPU profile — top-cum

Profile: `/tmp/ship97-prefix/cpu-prefix.prof` (246 KB, 60.11s window, 328.78s total samples).

| Function | flat | cum | cum% |
|---|---|---|---|
| `encoding/json.Unmarshal` | 0 | 223.70s | **68.04%** |
| `restactions/api.Resolve.func5` | 0.01s | 164.62s | 50.07% |
| `restactions/api.apistageContentServe` | 0 | 151.28s | **46.01%** |
| `restactions/api.gateContentEnvelope` | 0 | 151.21s | 45.99% |
| `restactions/api.gateListEnvelope` | 0 | 151.21s | 45.99% |
| **`restactions/api.parseListEnvelope`** | **0.04s** | **149.11s** | **45.35%** |
| `dispatchers.resolveAndPopulateL1` (refresher) | 0 | 101.84s | 30.98% |
| `restactions/api.RefreshContentEntry` | 0 | 2.47s | 0.75% |

### `parseListEnvelope` caller fan-in (peek)

```
                                           149.11s   100% |   gateListEnvelope
     0.04s 0.012% 0.012%    149.11s 45.35%                | parseListEnvelope
                                           147.26s 98.76% |   encoding/json.Unmarshal
                                             1.70s  1.14% |   stripManagedFields (inline)
```

100% of `parseListEnvelope` CPU comes from `gateListEnvelope` (the FALLBACK path that fires when `entry.Items == nil`). The R3 fast path (`gateListItems` / `gateListItemsWithMemo`) consumed only 2.10s (0.6%).

### `gateListEnvelope` consumer split (peek)

```
                                           149.11s 98.61% |   parseListEnvelope
                                             2.10s  1.39% |   gateListItems
```

Confirms: on the production-scale workload, **99% of apistage gate work is spent re-parsing the LIST envelope on every cache hit** because refresher-Put entries carry no pre-parsed `Items`.

## Two-sided HALT band (PM condition #1 + #2) — verdict

| Metric | Architect-design (INFERRED) | Captured | Band check | Verdict |
|---|---|---|---|---|
| `parseListEnvelope` cum CPU% on dashboard hot-path | ≥25% | **45.35%** | 12.5% ≤ x ≤ 50% PROCEED band — **inside, near upper edge** | **PROCEED** |
| pre-fix piechart_correct mix-weighted warm | 2,103ms | 2,103ms (from `chrome-mcp-steady-state-2026-05-31`) | 1,000 ≤ x ≤ 4,200ms PROCEED band | **PROCEED** |
| per-call latency contribution | 200ms × 8 waves | 263ms per call (2103/8) | 100–400ms PROCEED band | **PROCEED** |

**Verdict: PROCEED on Phase 2 (build).**

### Risk note (upper-edge proximity)

`parseListEnvelope` cum CPU% at 45.35% is **0.65 pp below** the 50% upper-bound HALT trigger. This is unusual: it means the apistage-list workload is near-monopolizing CPU at steady-state. Two consequences worth flagging to architect+PM:

1. **Post-fix delta will be large but heterogeneous.** With 45% of CPU consumed by a code path the fix eliminates from the request goroutine, the post-fix CPU envelope will redistribute substantially. Other long-poles previously masked will surface (e.g., refresher cycle parse cost — which moves from `parseListEnvelope` in gateListEnvelope to `parseListEnvelope` in the new `ParseListEnvelopeForRefresh` helper). The TOTAL parseListEnvelope CPU will not collapse to 0 — but the request-goroutine share MUST collapse (AC-97.8).

2. **Refresher cycle CPU is already large** (30.98% cum on resolveAndPopulateL1). The architect's design adds one full `parseListEnvelope` per refresh cycle as the cost of populating Items. AC-97.9 (refresher CPU regression ≤ 10%) is the falsifier here — and it is at risk because the refresher is already a top consumer. Mitigation: the parse cost was being paid REPEATEDLY today on every Get-hit of refresh-Put entries, so refresher cost moving from "every hit" to "every Put" should be NET strictly less, even with this new explicit Put-time parse.

## Identity-propagation check (per `feedback_seed_inherits_nested_call_identity`)

The Phase 2 fix is in `resolve_populate.go:255` Put site + a new exported helper in `apistage.go`. Neither touches context propagation — the helper is pure `(inputs, raw) → (items, apiVersion, kind, ok)`. The refresher's WithUserInfo + SA-transport context is untouched. Identity propagation is unchanged.

## Two-sided HALT verdict matrix discharge

- LOWER bound (<10% mechanism wrong): **NO** — 45.35% ≫ 10%. Mechanism is firing on the dashboard hot path.
- UPPER bound (>50% magnitude wrong): **NO** — 45.35% < 50%. Magnitude is within design assumptions.
- PROCEED band (12.5–50% AND 100–400ms per-call): **YES** — both metrics inside.

## Artifacts attached to ledger row

- Pre-fix CPU profile: `/tmp/ship97-prefix/cpu-prefix.prof`
- Pre-state vars: `/tmp/ship97-prefix/vars-pre.json` (52 KB)
- Drive script: `/tmp/ship97-prefix-falsifier.sh`
- Top-cum summary: `/tmp/ship97-prefix/top-cum-summary.txt`

## Sign-off

PROCEED with Phase 2 (build). Two-sided band cleared. Pre-flight artifact attached for ledger row inclusion.

— cache-developer.
