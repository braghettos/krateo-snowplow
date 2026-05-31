# Ship #97 — PM Gate Verdict

Signed: cache-pm. Date: 2026-05-31. Status: gating, not building. Read-only audit.

## Decision: **CONDITIONAL ACCEPT**

The architect's design is structurally sound, the scope correction is defensible, and the falsifier (§5 Path 1) explicitly targets the north-star /dashboard path with an unambiguous HALT criterion. **Conditions below MUST be discharged before dev's first commit** — primarily because the design's piechart_correct delta projection (−65%, to ~600–800ms) is INFERRED-not-TRACED, and the same misframing pattern took down Ship S.2 / 0.30.213 less than 24 hours ago.

---

## Scope correction verification (6 → 1)

Independently walked all **10 production `ResolvedEntry{` Put sites** (`grep -rn 'ResolvedEntry{' internal/ | grep -v _test.go`). The architect's enumeration is correct minus a count off-by-one (the architect listed 9 alternates + the target; my grep counts the same 10 sites including ra_full_list_store.go:63 + :84):

| # | File:line | Class written | In R3 read scope? | Notes |
|---|---|---|---|---|
| 1 | `internal/resolvers/restactions/api/apistage.go:522` | `apistage` (MISS) | Yes, **already populates Items** (lines 530–538). Out of fix scope. |
| 2 | `internal/resolvers/restactions/api/cluster_list.go:357` | `apistage` (cluster-list collapse) | Yes, **already populates Items + ItemsAPIVersion + ItemsKind** (lines 360–362). Out of fix scope. |
| 3 | **`internal/handlers/dispatchers/resolve_populate.go:255`** | **polymorphic** — `apistage` when `inputs.CacheEntryClass == cache.CacheEntryClassApistage` (resolve_populate.go:302 hits `resolveContentEntryForRefresh` → `RefreshContentEntry` → returns raw envelope for the refresher Put) | **YES — the single in-scope defect.** Items not populated. |
| 4 | `internal/handlers/dispatchers/restactions.go:234` | `restactions` (dispatcher L1) | No. R3 predicate at `apistage.go:487` reads only entries fetched via the apistage content cache key (`contentKeyInputs(gvr, ns, name)` at apistage.go:524). |
| 5 | `internal/handlers/dispatchers/widgets.go:264` | `widgets` (dispatcher L1) | No. Different class, different key shape. |
| 6 | `internal/handlers/dispatchers/widget_content.go:254` | `widgetContent` | No. Consumed by `gateWidgetEnvelope`, not the apistage R3 path. |
| 7 | `internal/handlers/dispatchers/phase1_pip_seed.go:831` | `restactions` (PIP seed) | No. |
| 8 | `internal/handlers/dispatchers/phase1_pip_seed.go:949` | `widgets` (PIP seed) | No. |
| 9 | `internal/cache/ra_full_list_store.go:63` | `raFullList` | No. Separate slice-tier path; predicate at `resolved.go:893` keys on `CacheEntryClassRAFullList`. |
| 10 | `internal/cache/ra_full_list_store.go:84` | `raFullList` (pinned) | No. |

**R3 predicate confirmed** at `apistage.go:483–494`:
```go
if entry, hit := store.Get(contentKey); hit && entry != nil {
    envelope = entry.RawJSON
    entryRef = entry
    if isList && len(entry.Items) > 0 {                    // ← R3 fast-path
        parsed = parsedListEnvelope{ items: entry.Items, ... }
        haveParsed = true
    }
    ...
}
```

`store` here is the apistage content store; `contentKey` is computed from `contentKeyInputs(gvr, ns, name)` (apistage.go:524). Only Puts under that exact key shape are read by this predicate. Sites #4–#10 write under different key shapes (per-user dispatcher keys, widget keys, ra-full-list keys).

**Verdict: scope correction is defensible. 1 Put site (resolve_populate.go:255) is the correct surface.** The architect's table at §1 is accurate; the brief's "6 sites" was over-counted.

---

## Falsifier verification — is it tied to /dashboard?

**YES — load-bearing on /dashboard, NOT compositions-panels.**

Design §5 Path 1 capture script (apistage.go pprof at port 18081) drives "60s of admin + cyberjoker **/dashboard** warm reloads via Chrome MCP" and analyses `parseListEnvelope` cum CPU% on that probe. **Pass criterion (unambiguous):** ≥10% cum CPU → proceed; <10% → HALT, escalate.

§3 explicitly flags the scope-mismatch risk in plain language: *"the pprof identifies the long-pole on a 35 MB LIST envelope; the brief targets /dashboard piechart_correct = 2,103ms. Whether the dashboard piechart chain hits apistage-class entries dominated by parseListEnvelope is NOT TRACED."*

§3 also encodes the `feedback_empirical_baseline_gate` ±2× tolerance bands as a falsifiable table (parseListEnvelope CPU% lower bound 12.5%, piechart_correct pre-fix band 1,000–4,200ms, per-wave latency band 100–400ms; outside → HALT).

**Verdict: falsifier IS gated on the right path (/dashboard, the north-star scoring path) and the HALT criterion is unambiguous.**

---

## Acceptance criteria (≥5, falsifiable, anchored to north-star)

All criteria anchored to the 0.30.212 steady-state Chrome-MCP baseline (`docs/chrome-mcp-steady-state-2026-05-31.md`) and AC-97 numbering from design §6, plus PM-added gaps:

| # | Criterion | Measurement | 0.30.212 baseline | Pass threshold |
|---|---|---|---|---|
| **AC-97.1** | piechart_visible_correct_ms mix-weighted warm | Chrome MCP /dashboard warm (0.95×cj + 0.05×admin) | **2,103ms** | **≤ 1,500ms** (GREEN-band acceptable; target ≤800ms; stretch ≤500ms) |
| **AC-97.2** | piechart_visible_correct_ms admin warm | Chrome MCP /dashboard warm, admin only | **2,245ms** | ≤ 1,500ms |
| **AC-97.3** | admin /compositions warm lastCallEnd | Chrome MCP /compositions warm, admin | **9,573ms** | ≤ 7,000ms (modest improvement; full attack on this metric requires long-poles #1 + #2 too, out of #97 scope) |
| **AC-97.4** | `parseListEnvelope` cum CPU% on /dashboard burst | 60s pprof during admin + cj /dashboard warm reload | TBD by dev (§5 Path 1 falsifier) | post-fix < 10% (target < 5%) |
| **AC-97.5** | R3 fast-path fires on refresher-populated apistage entries | `apistage.content_hit` slog `preparsed=true` rate | 0% (R3-inert by mechanism) | ≥ 90% of post-refresh content-hits |
| **AC-97.6** | Output content-equivalence | `dispatch_delta` on cj + admin compositions-panels and dashboard piechart cells (pre vs post-fix) | n/a | zero non-noise diff (modulo per-request `status.traceId`) |
| **AC-97.7** | RBAC symmetry preserved (`feedback_predicate_subject_kind_symmetry`) | Group-only RoleBinding cohort serves correct items | n/a | served items count matches admin/cj per cohort |
| **AC-97.8** | Per-goroutine attribution — no customer-tax | post-fix `parseListEnvelope` callers in pprof | request-path stacks 100% today | request-path stacks 0% post-fix (refresher only) |
| **AC-97.9** | Refresher cycle CPU does not regress > 10% | `process.cpu_seconds_total` / `snowplow_refresher_completed_total` rate | TBD by dev | ≤ 110% of pre-fix |
| **AC-97.10** | -race test on concurrent Get over refresher-Put entry passes | `go test -race ./internal/handlers/dispatchers/...` | n/a | zero race detector hits |
| **AC-97.11** | Pod restartCount = 0 over 30-min sustained burst | kubectl get pod observation window | 0 (today's burst) | 0 |
| **AC-97.12** | LOC envelope honoured | git diff line counts, prod + tests | n/a | ≤ +250 LOC total; pause + review if exceeded |

AC-97.7 added per `feedback_predicate_subject_kind_symmetry` (Ship 0.30.183 ζ over-filter HARD REVERT lesson). AC-97.11 added per the "pod crash on dirty restart = P0" success metric. AC-97.12 explicit LOC pause threshold per Ship S.2 lesson (180 → 473, 2.6× overshoot).

---

## PM gate question discharge

**1. Would the fix make the symptom disappear?**

Architect labels the /dashboard piechart_correct delta as **INFERRED, not TRACED**. The mechanism — populate `entry.Items` at `resolve_populate.go:255` so `apistage.go:487 len(entry.Items) > 0` evaluates true on subsequent Gets — does collapse `parseListEnvelope` on the apistage content cache path. **What is NOT proven**: that the /dashboard piechart_correct path's wall-clock is dominated by `parseListEnvelope` on apistage Gets. The 200ms-per-wave / 8 waves × 200ms ≈ 1.6s of 2.1s INFERRED breakdown is plausible (the piechart RA does aggregate a 50K-composition LIST), but it is a hypothesis until dev's §5 Path 1 falsifier proves it. The falsifier IS load-bearing on the right path (/dashboard, not compositions-panels) and has an explicit HALT at <10%. **Symptom-disappear: probable but not proven** — gate hinges on dev's pre-flight measurement.

**2. Is the scope correction (6 → 1 Put site) defensible?**

YES — independently verified by walking 10 production Put sites (table above). R3 read-path predicate at `apistage.go:483–494` reads ONLY the apistage content store under `contentKeyInputs(gvr, ns, name)`. Sites #1, #2 already populate Items. Sites #4–#10 write entries of other classes under different key shapes that the R3 predicate never queries. Site #3 (`resolve_populate.go:255`, polymorphic) is the single defect when `inputs.CacheEntryClass == cache.CacheEntryClassApistage` (resolve_populate.go:302 hits `resolveContentEntryForRefresh`).

**3. Does the design honor `feedback_empirical_baseline_gate`?**

YES — §3 codifies the rule by name, §5 Path 1 makes the dev's first task an empirical pre-fix pprof on the **north-star path (/dashboard)**, and §3 defines a ±2× tolerance band table (parseListEnvelope CPU% must be ≥12.5%; piechart_correct must be 1,000–4,200ms; per-wave contribution ≥100ms). HALT criterion is unambiguous. **One gap**: the rule says ">2× off", §3 implements "<2× lower bound" but does not explicitly state what to do at ">2× upper bound" (e.g. parseListEnvelope at 80%). Adding "if parseListEnvelope >50% AND/OR per-wave >400ms also HALT and escalate" would close the symmetry; minor but worth tightening.

**4. Risk register completeness**

§7 enumerates seven risks: R-decode-on-Put, R-Items-shape-mismatch, R-content-correctness, R-race, R-identity-propagation, R-special-cases, R-empirical-baseline-gate. Each has a stated mechanism, a quantification or trace citation, and a bound or test mapping to an AC. **R-race specifically follows `feedback_shared_vs_copy_is_a_concurrency_change`** (mandatory `-race` test, AC-97.10). **R-identity-propagation honors `feedback_seed_inherits_nested_call_identity`** (verifies the refresher's WithUserInfo/SA-transport context is untouched by the new helper). **Coverage: complete.**

**5. LOC envelope honesty**

Architect estimates +25 production / +100 tests / +125 total, with a self-imposed pause at +250 (2×, design §8). Ship S.2 overran 180 → 473 (2.6×). The +25 production figure is plausibly correct (1 helper export in apistage.go + 1 apistage-class branch in resolve_populate.go). **Risk**: the 3 tests (unit, integration, race) may need significant scaffolding — the integration test needs a fake refresher → fake apistage Put → fake content-Get-hit harness, which doesn't exist standalone today. If scaffolding pushes total over +250, pause is mandatory per the design's own gate.

---

## Risk register validation

| Risk | Confidence | PM verdict |
|---|---|---|
| **R-empirical-baseline-gate** (architect-flagged as biggest risk) | The 200ms-per-wave INFERRED savings on /dashboard piechart could be wrong | Mitigation via §5 Path 1 + ±2× tolerance band is the right shape. **HARD CONDITION**: dev must capture + post the pre-fix dashboard pprof BEFORE writing code, attach to the ship ledger row, and HALT if outside band. |
| **R-decode-on-Put** (refresher cycle CPU regression) | Bound: ≤ +10% (AC-97.9). Mechanism: parse moves from per-Get-hit to per-Put-cycle, strictly lower rate. | Acceptable bound. Per `feedback_customer_priority_over_refresher` — refresher pollution is acceptable IF customer hot-path benefits. AC-97.9 is the right falsifier. |
| **R-Items-shape-mismatch** (R3 sees different items shape) | Same `parseListEnvelope` function as MISS branch; same fields populated; same gate consumer | Verified by code citation. Unit test in design's test plan covers byte-equivalence with MISS branch. Low risk. |
| **R-content-correctness** (dispatch_delta on cj + admin) | AC-97.6 + comment at apistage.go:266–275 ("Output is byte-identical between the two") | Acceptable, but **CONDITION**: dev must capture pre-fix + post-fix dispatch_delta on **all four north-star cells** (admin /dashboard, admin /compositions, cj /dashboard, cj /compositions). |
| **R-race** (concurrent Get over refresher-Put entry) | `Items` slice now populated by a goroutine other than the request-path goroutine; this IS a concurrency-change per `feedback_shared_vs_copy_is_a_concurrency_change` | AC-97.10 covers. **CONDITION**: race test must include 4+ concurrent reader goroutines per design §7 R-race example, NOT a single-reader trivial test. |
| **R-identity-propagation** (refresher's identity context untouched) | `ParseListEnvelopeForRefresh` is pure over (inputs, raw); no context. Refresher SA-transport / WithUserInfo path unchanged | Acceptable by code review per `feedback_seed_inherits_nested_call_identity`. No runtime check needed. |
| **R-special-cases** (no GVR/path/user literals) | Branch is on `inputs.CacheEntryClass == cache.CacheEntryClassApistage` (data-driven, already used at resolve_populate.go:302) and `inputs.Name == ""` (LIST vs GET, already used at apistage.go:115, :462) | Acceptable per `feedback_no_special_cases`. |

**No new flag introduced.** §9 explicitly states "No values.yaml changes. No new env vars" (honors `project_single_cache_flag_direction` and `feedback_no_park_broken_behind_flag`). **Revert is tag rollback to 0.30.212, not a flag toggle** (§10). Verified.

**One gap PM is adding**: per `feedback_predicate_subject_kind_symmetry` (Ship 0.30.183 ζ HARD REVERT lesson), the fix touches RBAC-adjacent code paths and the design does not explicitly state that the Group-kind subject path is unaffected. The fix doesn't touch any RBAC predicate, but AC-97.7 forces dev to verify Group-only RoleBindings still serve correctly. CONDITION below.

---

## Conditions for acceptance (CONDITIONAL)

1. **Pre-flight falsifier first, code never** (`feedback_empirical_baseline_gate`, `feedback_falsifier_first_before_ship`): dev MUST run §5 Path 1 (60s `parseListEnvelope` CPU% pprof on /dashboard with admin + cj warm reloads driven via Chrome MCP through the portal LB — NOT port-forward — per `feedback_no_kubectl_in_measurement`) BEFORE any commit. The pre-fix number gets attached to the ship ledger row. If the number is <10% cum CPU OR outside the ±2× tolerance band (12.5–50%), HALT and re-engage architect; do NOT write production code under a misframed baseline.

2. **Two-sided HALT band**: extend §3's table so the upper bound (>50% parseListEnvelope cum CPU OR >400ms per-wave contribution) ALSO triggers HALT (today only the lower bound is encoded). Symmetric tolerance per `feedback_empirical_baseline_gate`.

3. **dispatch_delta on all four north-star cells**: AC-97.6 must explicitly include all four (admin /dashboard, admin /compositions, cj /dashboard, cj /compositions) pre-fix + post-fix, not just compositions-panels.

4. **AC-97.7 RBAC symmetry probe** (added by PM): per `feedback_predicate_subject_kind_symmetry`, dev MUST verify a Group-only RoleBinding cohort serves the correct items both pre-fix and post-fix. The fix doesn't touch RBAC predicates, but the ζ HARD REVERT lesson costs nothing to discharge.

5. **`-race` test must use 4+ concurrent readers** (design §7 R-race example): single-reader trivial test does NOT satisfy `feedback_shared_vs_copy_is_a_concurrency_change`. AC-97.10 gates.

6. **LOC pause at +250** (design §8): hard gate. If test scaffolding pushes over +250 total, dev pauses and reviews with architect BEFORE continuing. Ship S.2's 2.6× overshoot is the reference lesson.

7. **Three-way pre-commit review** (`feedback_dev_review_with_architect_pm_before_commit`): dev MUST share the diff with architect (design soundness) + PM (acceptance + falsifier discharge) BEFORE commit/tag/push.

8. **Ledger row attached at deploy** (`feedback_falsifier_first_before_ship`, `feedback_maintain_feature_journal`): dev attaches pre-fix pprof artifact + falsifier output + post-fix pprof artifact to `project_feature_journal` ship row. Mix-weighted piechart_correct measurement must use Chrome MCP via the portal LB, not curl, not port-forward.

9. **Chart lockstep, explicit push to braghettos** (`feedback_chart_release_lockstep`, `feedback_chart_repo_origin_is_upstream`): chart repo's `origin` is UPSTREAM. Tag `snowplow-0.30.214` MUST be pushed to `braghettos` remote explicitly. Helm upgrade via `helm upgrade snowplow braghettos/snowplow --version 0.30.214` (NOT `kubectl apply`, NOT `kubectl set image`, per `feedback_chart_only_for_snowplow` and `feedback_never_kubectl_apply`).

10. **Per-goroutine post-fix evidence** (`feedback_per_goroutine_evidence_beats_cpu_pprof`): AC-97.8 — dev posts goroutine-debug=2 dump confirming `parseListEnvelope` call stacks come ONLY from refresher goroutines (`RegisterRefreshHandlers.func1` lineage), ZERO request-path stacks (`restActionHandler.ServeHTTP` lineage). CPU-pprof alone insufficient.

---

## Rollout sign-off

| Item | Verdict |
|---|---|
| Single binary `ghcr.io/braghettos/snowplow:0.30.214` | OK. 0.30.213 is the S.2 reverted candidate; 0.30.214 is the next clean slot. |
| Chart lockstep tag `snowplow-0.30.214` on `braghettos/snowplow-chart` | OK. **Reminder**: chart repo `origin`=upstream — push to `braghettos` explicitly (`feedback_chart_repo_origin_is_upstream`). |
| No values.yaml changes; no new env vars | OK. Honors `project_single_cache_flag_direction`. |
| Single CACHE_ENABLED flag end-state preserved | OK. |
| Revert plan: tag rollback to 0.30.212 (not flag toggle) | OK. Honors `feedback_no_park_broken_behind_flag`. §10 spells out helm rollback sequence; no state to roll back beyond image (Items field TTLs within 3600s; 0.30.212 read path is forward-compatible). |
| Deploy gate: /readyz 200 + phase1Done=true + post-fix pprof `parseListEnvelope` <10% within 5 min | OK. |

---

## Final verdict

**CONDITIONAL ACCEPT** — 10 conditions above; condition #1 (pre-flight falsifier on /dashboard with two-sided HALT band) is the load-bearing one. Discharge in order; do not commit production code before #1 is logged. Mechanism is correct, scope is correct, falsifier is correct path. The only open hypothesis is the magnitude of the piechart_correct improvement, and that is exactly what condition #1 is designed to test.

— cache-pm. Read-only. No commits.
