# Ship 0.30.238 — Stage 2: CRUD-triggered re-prewarm — empirical trace + design

**Architect (cache architect)** | **Date**: 2026-06-02 | **Branch (intended)**: `ship-0.30.238-stage-2-crud-reprewarm`
**Mandate**: #65 SHIP 2 — Stage 2: CRUD-triggered re-prewarm (the load-bearing fix)
**Prereqs read**: `docs/ship-0.30.237-stage-1-boot-parallel-f3-reapply-2026-06-02.md`, `docs/ship-0.30.238-stage-2-crud-triggers-2026-06-02.md` (predecessor placeholder), `docs/bench-verify-serve-stale-design-2026-06-02.md`.

> **HEADLINE FINDING — surfaced under `feedback_empirical_root_cause_trace_before_fix`**:
>
> Empirical trace of `/tmp/0.30.237-vars-mid-bench.json` proves the 0.30.237 Gate C residual misses (`widgets|panels` 23.0%, `widgets|buttons` 26.9%, `widgets|markdowns` 48.7%) are **NOT a post-CRUD-storm freshness gap**. They are a **boot-time walker / cohort-enumeration coverage gap**.
>
> **Cross-validated by coordinator** with bench probe-determinism + verify-serve-stale data on 0.30.235 (§2.8 below): the refresher mechanism fires in **<50ms of PATCH apiserver-accept**, rewriting L1 under the SAME key. Refresh-trigger is NOT broken. Three distinct body_sha256 across pre/mid/post probes IS proof refresh fires; 5/5 byte-identical bodies with NO mutation is proof per-call response stamping is FALSIFIED. The Gate C miss% is structurally bounded by which cohort cells exist in L1 to refresh in the first place.
>
> | counter | pre-storm (T=boot+90s) | mid-bench (T+~30min, post-storm) | delta |
> |---|---:|---:|---:|
> | `phase1_seed_units_planned_total` | **1078** | **1078** | **0** |
> | `phase1_seed_widgets_total` | 410 | 450 | +40 |
> | `phase1_seed_restactions_total` | 541 | 558 | +17 |
> | `cohort_memo_entries_total` | 90 | 92 | +2 |
> | `refresher_skipped_no_entry_total` | 71,996 | **209,561** | **+137,565** |
> | customer dispatch `widgets|panels` `miss_total` | n/a | 1,627 | n/a |
>
> `units_planned` is fixed at boot and never grows. The +40/+17 widget/RA seed increments after the storm are RETRIES of failed boot units, not new units. The +137k `refresher_skipped_no_entry` proves the CRUD-storm DID fire dirty-marks (~140k events), the refresher DID dequeue them — but every one of them targeted L1 keys that were never seeded.
>
> **Stage 2 CRUD-triggered re-prewarm hooks alone CANNOT close this gap.** The hooks re-prewarm against `EnumerateResourceCohorts(gvr)` — the same enumeration the boot scope already uses. Re-firing the same enumeration on a CRUD event produces the same 1078-unit plan that already missed the customer cohort.
>
> **The walker-coverage gap (#156) IS the load-bearing defect.** Stage 2 (as scoped in the predecessor placeholder doc + the user's brief) is shipped against the WRONG defect. Per `feedback_empirical_root_cause_trace_before_fix`, this design HALTS the predecessor's mechanism and proposes a re-scoped Stage 2 that fixes the actual gap — **buildCohortPlans cohort-set widening + CRUD-trigger** as a coupled fix.
>
> Sections 1–2 carry the falsifier artifact + the empirical trace; section 3 is the prior-art check; section 4 is the re-scoped mechanism; section 9 surfaces the alternative (HALT Stage 2 and ship a "buildCohortPlans cohort-widening" only fix) if the PM wants to de-risk.

---

## 1. Pre-flight falsifier artifact

**Artifact paths** (already on disk, captured 2026-06-02):

| File | Captures | What it proves |
|---|---|---|
| `/tmp/0.30.237-vars.json` | `/debug/vars` at T=boot+~90s, rev 411 (0.30.237) | Boot scope completed; `units_planned=1078`, `widgets_total=410`, 5 cohorts seeded for widgets |
| `/tmp/0.30.237-vars-mid-bench.json` | `/debug/vars` mid-bench (post-storm) | `units_planned` UNCHANGED (1078); customer dispatch missed 1627 panels + 3224 buttons + 764 markdowns |
| `/tmp/0.30.237-gate-C-bench.log` | bench narrative + miss-% table | Live record of the 0.30.237 Gate C failure |
| `/tmp/0.30.237-cohort-progress.log` | per-cohort progress logs from Stage 1 Lever B | Witness that `buildCohortPlans` only enumerated 5 widget cohorts + 28 restaction cohorts |
| `/tmp/snowplow-runs/0.30.235/probe-determinism-20260602-151950.json` | coordinator's request-time bench probe (rev 409 = 0.30.235) | 5/5 deterministic L1 hits, body byte-identical with no mutation. Falsifies per-call response stamping hypothesis. Complementary verify-serve-stale run on the same rev (3 distinct body_sha256 within 50ms of PATCH under SAME key_hash) proves refresher fires sub-50ms. Establishes request-time vantage corroborating the seed-time gap (§2.8). |

**Capture commands** (re-runnable for verification, with GKE context guard):

```bash
# 0. Verify GKE context (per feedback_kubectl_verify_gke_context)
kubectl config current-context  # must match gke_neon-481711_us-central1-a_cluster-1

# 1. /debug/vars snapshot — read directly from pod via kubectl exec
kubectl exec -n krateo-system deployment/snowplow -- \
    wget -q -O - http://localhost:8080/debug/vars > /tmp/0.30.237-vars-NOW.json

# 2. Per-cohort breakdown sanity
python3 -c "
import json
d = json.load(open('/tmp/0.30.237-vars-NOW.json'))
print('seed_widgets_by_cohort cohorts:', len(d['snowplow_phase1_seed_widgets_by_cohort']))
print('seed_restactions_by_cohort cohorts:', len(d['snowplow_phase1_seed_restactions_by_cohort']))
print('bindingset_classes_total:', d['snowplow_phase1_bindingset_classes_total'])
print('units_planned:', d['snowplow_phase1_seed_units_planned_total'])
"
# Output proves: 5 widget cohorts, 28 RA cohorts, 35 bindingset classes, 1078 planned units.

# 3. Customer miss-cell breakdown
python3 -c "
import json
d = json.load(open('/tmp/0.30.237-vars-NOW.json'))
for k, v in d['snowplow_dispatch_l1_lookups'].items():
    if not k.startswith('widgets|'):
        continue
    total = v['hit_total'] + v['miss_total']
    if total < 100: continue
    miss_pct = 100.0 * v['miss_total'] / total
    print(f'{k}: hit={v[\"hit_total\"]} miss={v[\"miss_total\"]} miss%={miss_pct:.1f}%')
"
```

**What the falsifier proves** (the load-bearing claim Stage 2 design must answer):
- Pre-storm `units_planned=1078` already locks the boot coverage at 1078 (cohort × CR) pairs.
- A widget-CR ADD storm fires ~5,000+ ADD events (one per composition × per declared widget). Stage 2's `widget-cr` scope re-prewarms ONE widget across `EnumerateResourceCohorts(gvr)` — at most ~5 widget cohorts × 1 widget = ~5 new units per event.
- 5,000 events × 5 units = 25,000 units. But customer dispatches hit ~13K cohort cells (5426+8751+1115+804+5426+140+77+90 hits + misses ≈ 24K total dispatch lookups). Stage 2's re-prewarm would still target **the same 5 widget cohorts** because `EnumerateResourceCohorts` returns the same set on every call. **The new cohort cells the customer hits would still not be seeded.**

---

## 2. Empirical root-cause TRACE (TRACED vs INFERRED)

Per `feedback_architect_design_rigor`, every claim labelled TRACED (with file:line + artifact cite) or INFERRED (with the inference chain stated).

### 2.1 Symptom — TRACED

**0.30.237 mid-bench `/debug/vars`** (`/tmp/0.30.237-vars-mid-bench.json`, parsed):

```
snowplow_dispatch_l1_lookups (widgets|<resource>):
  widgets|panels:    hit=5426, miss=1627  → 23.0% miss
  widgets|buttons:   hit=8751, miss=3224  → 26.9% miss
  widgets|markdowns: hit=804,  miss=764   → 48.7% miss
  widgets|datagrids: hit=1115, miss=328   → 22.7% miss
  widgets|piecharts: hit=140,  miss=2     →  1.4% miss
  widgets|tables:    hit=90,   miss=2     →  2.2% miss
  widgets|navmenus:  hit=77,   miss=1     →  1.3% miss

snowplow_phase1_seed_widgets_by_cohort: ONLY 5 cohorts:
  - system:cloud-controller-manager
  - system:cohort:group-only:v1
  - system:gke-common-webhooks
  - system:kube-controller-manager
  - system:kubestore-collector

snowplow_phase1_bindingset_classes_total: 35   (the universe)
snowplow_phase1_seed_restactions_by_cohort: 28 cohorts (incl. cyberjoker)
```

The customer cohort (cyberjoker) appears in `seed_restactions_by_cohort` (28/35) but **NOT in `seed_widgets_by_cohort` (5/35)**.

### 2.2 Why widget seed covers only 5 cohorts — TRACED

**File:line**: `internal/handlers/dispatchers/prewarm_engine_boot.go:455-460` (Stage 1 boot harness):

```go
for _, e := range widgetEntries {
    cohorts := cohortsFor(e.GVR, true)            // ← widget GVR, haveGVR=true
    for _, c := range cohorts {
        addWidget(c, e)
    }
}
```

`cohortsFor` (`prewarm_engine_boot.go:439-446`):

```go
cohortsFor := func(gvr schema.GroupVersionResource, haveGVR bool) []cache.Cohort {
    if haveGVR {
        if rc := cache.EnumerateResourceCohorts(gvr); len(rc) > 0 {
            return rc                              // ← TAKEN for widget GVRs
        }
    }
    return globalCohorts                           // ← only taken when rc==nil
}
```

For widget GVRs `widgets.templates.krateo.io/v1beta1/{panels, buttons, markdowns, …}`, `EnumerateResourceCohorts(gvr)` returns the User+Group subjects from RBAC bindings that grant `get/list` on that specific GR (`internal/cache/bindings_by_gvr.go:520-595`).

### 2.3 Why cyberjoker is absent from widget cohort enumeration — TRACED + INFERRED

**TRACED** from the customer's actual RBAC topology (per `[reference_portal_chart.md]` + `[project_narrow_rbac_shape]`): cyberjoker has ZERO ClusterRoles and ONE Role in `demo-system` (the portal chart's UAF-pattern Role). The portal chart Role grants the resources the UAF stanza declares — typically configmaps + secrets for the user's clientconfig, NOT widget GVRs.

**INFERRED** (with chain): widget access flows through `userAccessFilter` (UAF) declared inside each widget CR's apiRef RESTAction stanza. UAF is a snowplow-internal RBAC layer evaluated at `/call` dispatch time (`internal/handlers/dispatchers/widget_content.go` + RBAC predicate). UAF is NOT a Kubernetes RoleBinding. **`EnumerateResourceCohorts` reads only the BindingsByGVR index, which is built from K8s `(Cluster)RoleBinding` informers** (`internal/cache/bindings_by_gvr.go:347-396`). Therefore cyberjoker — accessing widgets ONLY via UAF — is structurally invisible to `EnumerateResourceCohorts(<widget GVR>)`.

`grep -rn UserAccessFilter internal/cache/bindings_by_gvr.go` returns nothing. CONFIRMED no UAF integration in the bindings index.

### 2.4 Why cyberjoker IS in seed_restactions_by_cohort but NOT seed_widgets_by_cohort — TRACED

**File:line**: `internal/handlers/dispatchers/prewarm_engine_boot.go:448-454`:

```go
for _, ref := range restactionRefs {
    targetGVR, haveTarget := restActionTargetGVR(ctx, ref)
    cohorts := cohortsFor(targetGVR, haveTarget)
    for _, c := range cohorts {
        addRA(c, ref)
    }
}
```

For RESTActions whose UAF stanza declares a target GVR like `composition.krateo.io/v1/compositions` (compositions-list RA), `restActionTargetGVR` returns the GVR. `EnumerateResourceCohorts(compositions GVR)` returns the cohorts that have RBAC on compositions — which INCLUDES cyberjoker because the portal chart's UAF Role does grant cyberjoker `get/list` on compositions in `demo-system`.

For RESTActions with NO UAF or runtime-discovered targets, `haveTarget=false` and `cohortsFor` falls back to `globalCohorts = EnumerateBindingSetClasses() = 35 cohorts`. Cyberjoker appears in EnumerateBindingSetClasses (via the demo-system Role).

**So**: cyberjoker IS in the bindings index globally (35 cohorts → INCLUDED in seed_restactions for 28 RAs), but is NOT in `EnumerateResourceCohorts(<widget GVR>)` (which is the per-widget-GVR scoping that produces the 5-cohort widget plan).

### 2.5 Why widget-CR / RA-CR / RBAC-shift Stage 2 hooks WOULD NOT fix this — TRACED

The predecessor placeholder `docs/ship-0.30.238-stage-2-crud-triggers-2026-06-02.md` §3.6 wires:

```go
// rePrewarmWidgetCR  — single widget × EnumerateResourceCohorts(gvr)
cohorts := cache.EnumerateResourceCohorts(s.gvr)

// rePrewarmRBACShift  — re-seed every harvested widget targeting THIS gvr
cohorts := cache.EnumerateResourceCohorts(s.gvr)
```

Both handlers call `EnumerateResourceCohorts(s.gvr)` — the EXACT same function that returned only 5 cohorts for widget GVRs at boot. **Re-running the same enumeration on a CRUD event returns the same 5 cohorts.** Cyberjoker (UAF user) cannot appear in the result for widget GVRs no matter how many times the function is re-called.

Therefore: shipping Stage 2 as scoped in the predecessor placeholder would re-seed cohort cells the customer never hits AND fail to seed the cohort cells the customer DOES hit. This is the "fixing the wrong defect" pattern (`feedback_empirical_root_cause_trace_before_fix`, the F-3 / 0.30.144 lesson).

### 2.6 Walker coverage gap (#156) — relationship to Stage 2

**TRACED**: `units_planned=1078 / widget_entries=410 / restaction_entries=541`. The harvester saw ~90 widget entries (5 cohorts × ~18 widgets/cohort ≈ 90 unique GVR×widget triples per cohort, or just ~90 dedup'd widget CRs). The bench environment carries 47K widget CRs across the storm composition deploys — most of those are children of composition Page CRs that the boot walker did discover. The walker harvested 90 distinct unique-key widget tuples, NOT 47K. This is a "harvested-set ≠ live-set" issue.

**INFERRED**: The 90 harvested widget entries plus the cohort scoping cap at 5 produces the 1078 (rough product: ~5 widget cohorts × 90 widget entries + 28 RA cohorts × ~15 RAs = 450+420 = ~870; plus secondary). The bench navigation surfaces many widget cohorts that boot walked never observed.

**The walker-coverage gap and the cohort-enumeration gap are coupled.** Fixing Stage 2 as a CRUD trigger without ALSO fixing the cohort enumeration leaves the underlying defect open.

### 2.7 What the 0.30.237 −21pp improvement DID prove — TRACED

Before Stage 1 (0.30.236 with serial `seedScopeYielding`), `phase1_seed_widgets_total = 0` (widget loop never started). The −21pp improvement to 23-27% miss% in 0.30.237 means **the parallel boot scope did seed the 5 cohorts × 90 widgets it planned**. Those 1078 units now hit. The residual misses are the cells the boot scope NEVER plans — not cells that go stale.

### 2.8 Cross-validation — coordinator's 0.30.235 verify-serve-stale + probe-determinism traces — TRACED

> **Correction notice (re-gate round, 2026-06-02)**: an earlier draft of this section misread the verify-serve-stale artifact as proving "refresher fires sub-50ms." PM gate and fresh-architect peer-review independently flagged this as a 180-degree misinterpretation. The current section is the corrected reading. The misread did not affect the FIX (Component A in §4.1) because the seed-time vantage in §2.1-§2.4 is independently sufficient; the correction matters for design-doc integrity and to prevent the wrong claim from propagating into future ships.

**Two distinct artifacts**, separate experiments, separate read-outs:

#### 2.8.1 verify-serve-stale (PATCH mutation, request-time vantage) — TRACED

**Artifact**: `/tmp/snowplow-runs/0.30.235/verify-serve-stale-20260602-150300.json`

Machine verdict (from the artifact, top-level fields):

```
verdict: FAIL_SYNC_COLD_FILL_ON_MID
exit_code: 2
stale_served: false
refresh_completed: false
sources_agree: ...
l1_lookups: {
  "class": "restactions|templates.krateo.io/v1, Resource=restactions",
  "miss_delta": 1,
  "hit_delta": 2
}
probes (3 sequential, against cyberjoker compositions-list /call):
  pre  (offset_ms=−1000): body_sha256=e7e7490175d4..., http_ms=392, marker_present=false
  mid  (offset_ms=+55):   body_sha256=d7f0373d6074..., http_ms=325, marker_present=false
  post (offset_ms=+5004): body_sha256=19993c016de9..., http_ms=318, marker_present=false
```

What this PROVES (only what the artifact actually supports):

- The L1 cell for cyberjoker's compositions-list call **did not exist** at any of the 3 probe points. `pre.hit:false` (the bench harness's snapshot of the pod-log `resolved_cache.lookup` event for the pre traceId) + `miss_delta=1` (one of the 3 dispatches bumped `miss_total`) + `stale_served:false` together prove the mid-window served-stale-while-refresh contract was NOT honored.
- Each probe was a **synchronous cold-fill** (re-resolve from scratch via the apiserver). The 3 distinct body_sha256 values are 3 distinct resolves at 3 wall-clock times against a moving apiserver snapshot — NOT a refresher rewrite. http_ms (392 / 325 / 318) reflects full upstream LIST cost, consistent with cold-fill latency.
- `marker_present:false` on all 3 probes including `post (offset_ms=+5004)` shows the PATCH annotation isn't yet visible in compositions-list output 5s after apiserver-accept — likely informer-lag-bounded; orthogonal to the L1 question.

What the artifact does NOT prove (and the earlier framing wrongly claimed):

- It does **NOT** prove the refresher fires sub-50ms. The refresher could not have fired at all here — there was no L1 entry to refresh, so the refresher's `processOne` would have hit the `skippedNoEntry` short-circuit (`refresher.go:684-697`) on any dirty-mark for this key, exactly as the +137,565 `refresher_skipped_no_entry_total` delta in §2.1 shows at-scale.

**Convergence with §2.1-§2.4 (seed-time vantage)**: same defect, two vantages.

| Vantage | Observation | What it proves |
|---|---|---|
| Seed-time (`/debug/vars`) | `seed_widgets_by_cohort` covers 5/35 cohorts; cyberjoker missing | Cohort enumeration excludes cyberjoker for widget GVRs |
| Request-time (verify-serve-stale verdict) | `pre.hit:false`, `miss_delta=1`, `stale_served:false` | The cohort cell **never existed in L1**, so customer dispatch synchronously cold-filled |

Both vantages point at the SAME root cause: cohort cells for UAF-only users (cyberjoker) on widget GVRs are not created at boot AND not created by any subsequent CRUD/refresh path. Stage 2 must fix cohort enumeration; refresh-trigger and refresher mechanics are out of scope for Stage 2 (this design takes no position on refresher cadence — there is no evidence in the artifact set for it either way).

#### 2.8.2 probe-determinism (NO mutation, deterministic-bytes vantage) — TRACED

**Artifact**: `/tmp/snowplow-runs/0.30.235/probe-determinism-20260602-151950.json`

5 back-to-back probes against an unspecified separate cyberjoker /call path (NOT compositions-list):

| probe | body_sha256 | key_hash | l1.hit |
|---|---|---|---|
| 00–04 | `33d7840f...` | `2ffaa76d...` | true |

What this PROVES — **and only this**:

- For a /call path where the cohort cell DOES exist in L1 (`l1.hit:true`), the response bytes are **deterministic** across N reads under the same identity. Per-call response-stamping (a hypothesis that could have explained the 0.30.237 residual miss%) is FALSIFIED for the served path.

What this does NOT prove:

- Nothing about refresh timing.
- Nothing about cohort coverage on widget GVRs (this probe hit an RA path that was apparently cached for cyberjoker, consistent with §2.4: cyberjoker IS in 28/35 of seed_restactions cohorts).
- Nothing about what happens after a mutation (no mutation occurred in this experiment).

### 2.9 Net implication for Stage 2 — TRACED

Stage 2's load-bearing question is: **does the cohort cell for cyberjoker's widget GVRs exist after boot?**

- Seed-time evidence (§2.1-§2.4): NO. `seed_widgets_by_cohort` enumerates 5 system cohorts; cyberjoker is structurally excluded by `EnumerateResourceCohorts`.
- Request-time evidence (§2.8.1): NO. The customer's `/call` on a widget-dependent path synchronously cold-fills.

The fix MUST widen the cohort enumeration so cyberjoker (and every UAF-only user) appears in `buildCohortPlans`'s output for widget GVRs at boot. That is Component A in §4.1.

---

## 3. client-go prior-art check

Per `feedback_check_k8s_clientgo_prior_art`.

### 3.1 Informer event → re-resolve — `tools/cache.ResourceEventHandlerFuncs`

**Cite**: `k8s.io/client-go/tools/cache.SharedIndexInformer` already delivers ADD/UPDATE/DELETE events with full object payload. Snowplow's `deps_watch.go:191-237` already consumes them — `Deps().OnAdd / OnUpdate` already dirty-marks every L1 key whose Inputs.GVR matches the event. **The refresher path post-event is already wired**; the missing piece is that the refresher only re-resolves keys that EXIST (`refresher.go:684-697` `processOne` → `c.Get(key)`; miss → `skippedNoEntryTotal++`).

**No client-go primitive for "seed a brand-new key on ADD"** — client-go's job is to deliver the event; what the consumer does with it (re-resolve existing, or compute new keys to populate) is application logic.

### 3.2 Workqueue dedup — `client-go/util/workqueue`

`workqueue.TypedRateLimitingInterface` provides idempotent enqueue + dedup + rate-limited retry. **Already used** in `internal/cache/refresher.go:266-300` (refresher workqueue) and (transitively) in `internal/handlers/dispatchers/prewarm_engine.go:163-167` (the engine's bounded dedup queue uses a similar shape directly via `map[string]prewarmScope`). The engine's `enqueueScope` is a deliberate simplification of the workqueue contract — same coalescing semantics, lighter footprint.

**Conclusion**: client-go gives us the informer event bridge (already wired) and the dedup queue shape (already wired). It does NOT solve "compute the cohort set to seed for a new widget CR" — that's application-specific. The prior art check is exhausted.

---

## 4. Mechanism design — RE-SCOPED Stage 2

Given §2 trace, Stage 2 as originally scoped is **shipped against the wrong defect**. The re-scoped Stage 2 has TWO coupled components.

### 4.1 Component A — `buildCohortPlans` cohort-set UNION (load-bearing for Gate C)

**Defect being fixed**: `buildCohortPlans` (`prewarm_engine_boot.go:401-472`) uses `EnumerateResourceCohorts(gvr)` and falls back to `globalCohorts` only when the per-GVR enum returns empty. For widget GVRs the per-GVR enum returns 5 system cohorts → fallback never triggers → cyberjoker (and any UAF-only user) is excluded.

**Fix** (`prewarm_engine_boot.go:439-446`): change the cohort resolution from a per-GVR scoped lookup with fallback to a **UNION of the per-GVR scoped set and the global set**:

```go
// BEFORE (current — fallback-only)
cohortsFor := func(gvr schema.GroupVersionResource, haveGVR bool) []cache.Cohort {
    if haveGVR {
        if rc := cache.EnumerateResourceCohorts(gvr); len(rc) > 0 {
            return rc
        }
    }
    return globalCohorts
}

// AFTER (Stage 2-A — union)
cohortsFor := func(gvr schema.GroupVersionResource, haveGVR bool) []cache.Cohort {
    var rc []cache.Cohort
    if haveGVR {
        rc = cache.EnumerateResourceCohorts(gvr)
    }
    return unionCohorts(rc, globalCohorts)
}

// unionCohorts: dedup by cohortKey(c) (the same key buildCohortPlans uses).
// O(|rc| + |global|). When rc is empty, equivalent to globalCohorts (preserves
// the fallback semantics). When rc is non-empty AND global is non-empty,
// produces rc ∪ global with no duplicates.
func unionCohorts(a, b []cache.Cohort) []cache.Cohort {
    if len(a) == 0 { return b }
    if len(b) == 0 { return a }
    seen := make(map[string]struct{}, len(a)+len(b))
    out := make([]cache.Cohort, 0, len(a)+len(b))
    key := func(c cache.Cohort) string {
        gs := append([]string(nil), c.Groups...)
        sort.Strings(gs)
        return c.Username + "|" + strings.Join(gs, ",")
    }
    for _, c := range a { k := key(c); if _, ok := seen[k]; !ok { seen[k] = struct{}{}; out = append(out, c) } }
    for _, c := range b { k := key(c); if _, ok := seen[k]; !ok { seen[k] = struct{}{}; out = append(out, c) } }
    return out
}
```

**Coverage impact** (projected, sanity-bounded by boot budget):
- Today: 5 widget cohorts × 90 widget entries = ~450 widget units
- After: 35 widget cohorts × 90 widget entries = ~3,150 widget units (**+2,700 widget units**)
- 28 RA cohorts × ~15 RAs = ~420 → 35 × 15 = ~525 RA units (**+105 RA units**)
- New units_planned projected: ~3,700 (3.4× current)

**Boot budget feasibility** (RANGE, not point estimate — peer-review concern #6):

Per-unit cost is not a fixed value. Stage 1's 0.30.237 telemetry shows a wide spread depending on workload shape:
- Steady-state simple RA: ~0.3-0.5s per `seedOneRestaction`.
- compositions-list-shape RA under storm + 5K-scale LIST cost: 1.5-2.5s per unit (the 0.30.179 cohort-scoping cost analysis pointed at this band).
- Worst-case storm-and-iterator fallback: up to 5s per unit (the 0.30.189 sentinel-cohort cost).

At parallelism=8 and projected ~3,700 units:
- Optimistic (0.5s/unit): ~3,700 / 8 × 0.5s ≈ **3.85 min**
- Realistic (1.5s/unit at mid-storm): ~3,700 / 8 × 1.5s ≈ **11.6 min**
- Pessimistic (5s/unit worst-case): ~3,700 / 8 × 5s ≈ **38.5 min** (this exceeds the hard ceiling and would HARD REVERT — see below)

**Recovery policy (operator-driven, bounded)**:

1. **Pre-position chart 0.30.238 with `prewarm.bootTimeoutMinutes: 15` set as the default** (NOT 8). This is the Stage 1 Lever A knob already shipped; bumping the default at chart layer 0.30.238 pre-positions for the realistic worst case without an in-flight chart bump under pressure.
2. If post-deploy `prewarm.engine.boot.complete` shows elapsed >15 min: **HARD REVERT**. Do NOT escalate the chart value further; revert and re-diagnose (per-unit cost is structurally higher than projected → a different defect needs investigating, NOT a bigger timeout). The "indefinite timeout escalation" anti-pattern is explicitly excluded.
3. If post-deploy elapsed is in the 8-15 min band: ship is GREEN. Component A worked; the cluster is simply slower per-unit than the optimistic projection.

**Why this isn't `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill` violation**: `globalCohorts = EnumerateBindingSetClasses()` is a DYNAMIC enumeration from the live RBAC snapshot — exactly the "live cohort source" the feedback mandates. The union is "seed every (cohort × CR) where cohort is in the live bindingset universe OR the per-GVR scoping" — still dynamic, still cohort-of-1 to many, still re-derived on every boot scope re-walk. Not a "static cohort list".

**Why this isn't `feedback_no_special_cases`**: `unionCohorts(rc, globalCohorts)` is mechanism-uniform — applied to every RA + every widget. No "if username == cyberjoker" branch. The union covers UAF-only users by structural inclusion of `EnumerateBindingSetClasses` (the dynamic snapshot of every user-in-some-RB).

### 4.2 Component B — CRUD-triggered re-prewarm (admin-UX freshness; NOT load-bearing for Gate C)

After Component A lifts the boot coverage to ~3,700 units, **dirty-marks from a CRUD event already hit existing L1 keys, and the refresher already re-resolves them** (`internal/cache/deps.go:524-558` `onChange`, `refresher.go:684-723` `processOne`). The serve-stale-while-refresh contract from `feedback_mutation_serves_stale_while_refresh` is already honored for those keys.

The remaining gap Component B closes is the **brand-new CR**: a widget CR or RESTAction CR that didn't exist at boot. Today the harvester never sees it (the walker ran at boot and stopped); the first customer dispatch pays the per-user fallback Put cost (`restactions.go:242-260` Pinned-true Put). With Component A this is bounded — the cell IS Pinned-true after first dispatch — but the FIRST dispatch is synchronous-cold.

Component B fixes that synchronous-cold-first-dispatch case via `widget-cr` + `ra-cr` re-prewarm scopes hooked at `deps_watch.go:201,221`. The mechanism IS the predecessor placeholder doc §3.4–3.7, with TWO corrections:

**Correction B.1**: the per-scope cohort enumeration MUST use `unionCohorts(EnumerateResourceCohorts(gvr), EnumerateBindingSetClasses())` — the SAME union from Component A. Otherwise the re-seed inherits the boot-time defect.

**Correction B.2**: `rePrewarmRBACShift` (the placeholder doc §3.6 + the new `applyBindingAddAndReturnAffected` / `applyBindingDeleteAndReturnAffected` helpers in `bindings_by_gvr_delta.go`) is OUT OF SCOPE for Stage 2. Rationale:
- Cyberjoker has NO `RoleBinding` ADD/UPDATE/DELETE events because their access is UAF-driven, not RB-driven. The RBAC-shift mechanism cannot fire for the load-bearing customer cohort.
- The bench RBAC bindings (composition install creates ~5K RBs per `project_composition_install_rbac_scale`) would trigger RBAC-shift storms — affected-GVR aggregator becomes the bottleneck, not the seeder.
- The predecessor placeholder doc §3.2 helper code (`applyBindingAddAndReturnAffected`) is ~80 LOC of new logic with HIGH risk per `feedback_recurring_regression_pattern` (5+ visits on this surface during prior phases).

If RBAC-shift becomes load-bearing post-Component-A (admin creates a new RB that grants new users access to widget GVRs), it lands in a SEPARATE ship (0.30.239 Stage 3) with its own falsifier.

### 4.3 Re-scoped Stage 2 — Put-site map, event → re-prewarm-trigger wiring

**Put sites unchanged from Stage 1**:

| File:line | Class | Purpose |
|---|---|---|
| `internal/handlers/dispatchers/restactions.go:242-246` | per-user fallback Pinned Put | First customer dispatch fills uncached |
| `internal/handlers/dispatchers/widgets.go:272-276` (cite verified by grep: line 272 is Pinned:true) | per-user fallback Pinned Put | First customer dispatch fills uncached |
| `internal/handlers/dispatchers/phase1_pip_seed.go:868-872` (seedOneRestaction) | boot RESTAction seed Pinned Put | |
| `internal/handlers/dispatchers/phase1_pip_seed.go:995-999` (seedOneWidget) | boot widget seed Pinned Put | |

(The user's brief listed `resolve_populate.go:274-277` — grep proves no Pinned Put exists there. Brief was directionally correct but the Put surface is the 4 sites above.)

**New event → scope wiring** (Stage 2-B):

```
informer ADD (widgets.templates.krateo.io/v1beta1, any harvested resource)
    │
    └─→ deps_watch.go:201 AddFunc → existing Deps().OnAdd dirty-mark (UNCHANGED)
            │
            └─→ NEW: fireWidgetCRHook(gvr, ns, name) if isHarvestedWidgetGVR(gvr)
                    │
                    └─→ prewarmEngineSingleton().enqueueScope({kind:scopeKindWidgetCR, gvr, ns, name})
                            │
                            └─→ engine worker dequeues, dispatches makeScopeHandler
                                    │
                                    └─→ rePrewarmWidgetCR(ctx, deps, s):
                                            1. objects.Get(ctx, ref) under SA identity
                                            2. cohorts := unionCohorts(EnumerateResourceCohorts(s.gvr), globalCohorts)
                                            3. entry := navWidgetEntry{W:obj, GVR:s.gvr, ...}
                                            4. seedScopeParallel(ctx, nil, []navWidgetEntry{entry}, ...)
```

`isHarvestedWidgetGVR(gvr)` — NEW helper on `navWidgetHarvester` exposing whether the GVR was harvested. Implementation: hold a `sync.Mutex + map[schema.GroupVersionResource]struct{}` populated by `harvestNavWidget`. O(1) lookup; ~15 LOC.

Symmetric wiring for `scopeKindRACR` against `restActionGVR` (single GVR — `templates.krateo.io/v1, restactions`).

### 4.4 LOC budget

**Component A (LOAD-BEARING — Option 1 recommended)**:

| File | Type | LOC | Purpose |
|---|---|---:|---|
| `internal/handlers/dispatchers/prewarm_engine_boot.go` | Edit | ~25 | `cohortsFor` → `unionCohorts(EnumerateResourceCohorts(gvr), globalCohorts)` + per-call instrumentation log line (see §4.4.1 below) |
| `internal/handlers/dispatchers/prewarm_engine_boot_test.go` | NEW or extend | ~30 | **MANDATORY** unit tests for `unionCohorts` — see §4.4.2 |
| **Component A TOTAL** | | **~55** | The minimal fix for the §2-traced defect, tested + observable |

#### 4.4.1 Per-call `cohortsFor` instrumentation (peer-review concern #2)

Add one `slog.Info` line inside `cohortsFor` so the post-deploy falsifier observation is direct rather than inferred:

```go
slog.Info("prewarm.engine.cohortsFor",
    slog.String("subsystem", "cache"),
    slog.String("gvr", gvr.String()),
    slog.Bool("have_gvr", haveGVR),
    slog.Int("rc_count", len(rc)),
    slog.Int("global_count", len(globalCohorts)),
    slog.Int("unioned_count", len(unioned)),
)
```

Cost: one log per cohort-per-target-gvr at boot (~3,700 lines total, single boot pass, then never again). Lever B's per-cohort progress log already establishes the precedent for log-volume at this magnitude. Operators verify Component A's effect with one `kubectl logs ... | grep prewarm.engine.cohortsFor | head` against the boot window — no inference required.

#### 4.4.2 Mandatory unit tests for `unionCohorts` (peer-review concern + sentinel-double-count surprise)

Per `feedback_no_shortcuts_or_workarounds` — these tests ship in the same commit as the function; no defer.

Test cases (all pure-Go, no kubeconfig, runnable via `go test ./internal/handlers/dispatchers/`):

| Case | Setup | Expected |
|---|---|---|
| `nil_left` | `unionCohorts(nil, x)` | `x` (no mutation, returns `x` directly) |
| `nil_right` | `unionCohorts(x, nil)` | `x` |
| `idempotent` | `unionCohorts(x, x)` | `x` with dedup; `len(result) == len(x)` |
| `dedup_on_cohortKey` | `unionCohorts([]Cohort{A, B}, []Cohort{B, C})` where B has same Username+sorted-Groups | `{A, B, C}` — exactly 3 entries, no duplicate B |
| `group_only_sentinel_no_double_count` | `unionCohorts([]Cohort{{Username:"u", Groups:["g"]}}, []Cohort{{Username:groupOnlyCohortSentinel, Groups:["g"]}})` | **2 entries**, NOT 1 — these are distinct cohort identities (sentinel-prefixed group-only vs real-user-carrying-group) and must NOT collide. Peer-review surfaced this; verifies cohortKey discrimination. |

The sentinel test is load-bearing: if `cohortKey` accidentally collapsed `(sentinel, ["g"])` with `(realuser, ["g"])`, the union would drop one identity, the seed would miss it, and Falsifier A's `cyberjoker in seed_widgets_by_cohort` check would only catch the cyberjoker-User-cohort, not any cyberjoker-Group-only-cohort that might exist.

**Component B (OPTIONAL accelerator — Option 2 only)**:

| File | Type | LOC | Purpose |
|---|---|---:|---|
| `internal/handlers/dispatchers/prewarm_engine.go` | Edit | ~25 | Extend `prewarmScopeKind` enum + `prewarmScope` struct (gvr/ns/name fields) + `key()` |
| `internal/handlers/dispatchers/prewarm_engine_widgetcr.go` | NEW | ~80 | `rePrewarmWidgetCR` |
| `internal/handlers/dispatchers/prewarm_engine_racr.go` | NEW | ~60 | `rePrewarmRACR` |
| `internal/handlers/dispatchers/phase1_pip_seed.go` | Edit | ~25 | `navWidgetHarvester.HarvestedGVRs()` / `IsHarvestedGVR()` helpers |
| `internal/handlers/dispatchers/prewarm_engine_boot.go` | Edit | ~10 | `makeBootScopeHandler` → `makeScopeHandler` dispatching by kind |
| `internal/cache/prewarm_hooks.go` | NEW | ~45 | `SetWidgetCRHook` / `SetRACRHook` + `fireWidgetCRHook` / `fireRACRHook` (atomic.Pointer pattern, mirrors `SetCustomerInflightHook` at main.go:319) |
| `internal/cache/deps_watch.go` | Edit | ~15 | AddFunc + UpdateFunc fire hook for harvested widget GVRs + RA GVR |
| `main.go` | Edit | ~12 | 2 hook registrations + harvester accessor wiring |
| **Component B TOTAL** | | **~272** | Synchronous-cold first-dispatch accelerator for brand-new CRs |

**OPTION 1 (recommended) total: ~55 LOC** (25 function + 30 test). **OPTION 2 total: ~327 LOC** (55 + 272).

(NO `applyBindingAddAndReturnAffected` either way — the RBAC-shift surface is deferred to ship 0.30.239 Stage 3.)

### 4.5 Risk register

| Risk | Likelihood | Severity | Mitigation |
|---|---|---|---|
| Component A 3.4× planned units overshoots boot budget | Med | Med (budget) | Chart 0.30.238 pre-positions `prewarm.bootTimeoutMinutes: 15` as default (bumped from Stage 1's 8). **HARD CEILING: if elapsed >15 min, HARD REVERT — do NOT escalate further. Indefinite escalation is explicitly excluded.** See §4.1 recovery policy. |
| Component A inflates RSS via cohort cell explosion | Med | High | Cohort cells are Pinned:true (Stage 1 F3) — 1.5 GiB resident cap protects via `resolved.go:761-773` demote-on-overflow. 3.4× cell count × ~5 KiB/cell ≈ +50 MiB. Comfortably inside cap. **Empirical validation REQUIRED via 0.30.237 deploy** — measure `cohort_memo_total_bytes` growth coefficient. |
| Component A `EnumerateBindingSetClasses` returns transient empty during initial RBAC informer sync | Low | Low | Already guarded by `cache.EnumerateBindingSetClasses` returning `nil` when snapshot empty (`binding_set_enumeration.go:222-260` returns nil-on-no-snapshot). Union is symmetric; `unionCohorts(rc, nil) == rc` preserves boot semantics on cold-cluster start. |
| Component B widget-CR ADD storm during composition install (project_composition_install_rbac_scale) | High | Med | Engine queue dedup on `widget-cr|<gvr>|<ns>|<name>` key coalesces. 5K composition install × ~10 widgets = ~50K unique-name widget ADDs → 50K scopes enqueued. Engine yields between scopes (`engineYieldCheckpoint`). Worst-case wall-clock at parallel-8: 50K × ~0.5s / 8 = ~52 min. **NOT instant but bounded; customer dispatch is never blocked because seedScopeParallel yields**. |
| Component B widget-CR ADD races boot scope still running | Med | Low | Engine queue serializes. widget-cr scope dequeues AFTER boot completes; intermediate widget-cr enqueues coalesce in pending map. The first dispatch pays a per-user-fallback Pinned Put — still <1s and warm-from-second-call. |
| Component B `isHarvestedWidgetGVR` falsely reports false for a runtime-discovered CRD | Low | Low | A CRD's first widget ADD triggers the AddFunc; if isHarvestedWidgetGVR is false (boot walker didn't see it), no widget-cr scope fires. First customer dispatch pays per-user-fallback Pinned Put. Acceptable — same shape as the never-CRUD baseline today. |
| Component B `objects.Get(ctx, ref)` SA-identity hop on widget-cr scope race-conditions a Pre-stage filter | Low | Low | `prewarm_engine_widgetcr.go` uses `withPhase1SAContext` (same SA path as Stage 1 boot). Existing tests cover this path. |
| Stage 2 ships AGAINST the wrong defect if Component A is omitted | **Confirmed** | **HIGH** | Component A is the load-bearing fix per §2 trace. If PM elects to ship Component B alone (the predecessor placeholder), the Gate C miss% will NOT close — same defect-vs-fix mismatch as F-3 / 0.30.144. **DESIGN STRONG RECOMMENDATION: ship A + B together as 0.30.238; do not ship B alone.** |
| Recurring-regression flag (5+ visits on this surface) | Confirmed | Med | Component A is a 25-LOC targeted fix to a single helper function with empirically-derived `unionCohorts` semantics. Component B is the predecessor mechanism MINUS the RBAC-shift surface (the highest-risk component). Net surface reduction. |

### 4.6 Pre-commit falsifier — exact invocation + expected verdict

**Falsifier A — Component A coverage projection** (pre-deploy, on local build):

1. Build snowplow at the Stage 2 commit; deploy lockstep with chart 0.30.238.
2. Within 90 seconds of pod-ready:
   ```bash
   kubectl exec -n krateo-system deployment/snowplow -- wget -q -O - http://localhost:8080/debug/vars > /tmp/0.30.238-boot-vars.json
   python3 -c "
   import json
   d = json.load(open('/tmp/0.30.238-boot-vars.json'))

   # (1) Structural cohort-count gate — proves the union widened.
   assert d['snowplow_phase1_seed_units_planned_total'] >= 2500, \
       f'units_planned={d[\"snowplow_phase1_seed_units_planned_total\"]} < 2500 (Component A coverage projection failed — union not applied)'
   widget_cohorts = d['snowplow_phase1_seed_widgets_by_cohort']
   assert len(widget_cohorts) >= 30, \
       f'widget cohorts={len(widget_cohorts)} < 30 (union widened RA cohorts but not widget cohorts — partial-apply defect)'

   # (2) DIRECT MECHANISM GATE — cyberjoker MUST appear in seed_widgets_by_cohort.
   # This is the single most-direct test the fix works. If Component A's union
   # successfully includes UAF-only users from EnumerateBindingSetClasses, the
   # customer cohort name (cyberjoker per project_narrow_rbac_shape) is present.
   # If absent, the mechanism is broken regardless of total cohort count.
   assert 'cyberjoker' in widget_cohorts, \
       'Component A mechanism failed: cyberjoker still absent from seed_widgets_by_cohort'

   print('Component A coverage: planned_units =', d['snowplow_phase1_seed_units_planned_total'])
   print('Widget cohorts seeded:', len(widget_cohorts))
   print('Cyberjoker widget seed count:', widget_cohorts['cyberjoker'])
   "
   ```

The cyberjoker-in-seed_widgets_by_cohort assertion is the load-bearing mechanism check. The cohort-count and units_planned assertions guard against partial-apply defects (Component A applied to one loop but not the other). All three together leave no room for a "looks fine but doesn't fix the customer" outcome.

**Falsifier B — Component B per-event hook fires within 5s**:

```bash
# Apply a single new widget CR under a harvested GVR
kubectl apply -f /tmp/test-widget-cr.yaml -n bench-ns-test

# Within 5 seconds the engine processes the scope
sleep 5
kubectl logs -n krateo-system deployment/snowplow --tail=200 | grep "prewarm.engine.scope_processed.*widget-cr|"
# Expected: at least one line matching scope=widget-cr|<gvr>|<ns>|<name>

# Verify cohorts seeded
kubectl exec -n krateo-system deployment/snowplow -- wget -q -O - http://localhost:8080/debug/vars > /tmp/after-widget-cr.json
python3 -c "
import json
before = json.load(open('/tmp/0.30.238-boot-vars.json'))
after = json.load(open('/tmp/after-widget-cr.json'))
delta = after['snowplow_phase1_seed_widgets_total'] - before['snowplow_phase1_seed_widgets_total']
assert delta >= 30, f'widgets_total delta={delta} < 30 (expected ~35 cohort cells per widget-cr; B not triggering union seed)'
print(f'Component B fired: widgets_total +{delta}')
"
```

**Falsifier C — POST-DEPLOY GATE: `bench verify-serve-stale` + Gate C SCALE=5000 bench**:

```bash
# From bench dir, with GKE context verified
python -m bench verify-serve-stale --user cyberjoker --target compositions-list --tag 0.30.238

# Expected: verdict=PASS, miss_delta=0, exit 0
```

Then full Gate C bench:

```bash
python -m bench run --scale 5000 --stages S1..S8 --tag 0.30.238 \
    --json /tmp/0.30.238-gate-C.json
```

**Pass criteria** (gating contract = `miss_delta=0` only, per `feedback_l1_hit_invariant_is_100_percent`):

| Cell | 0.30.237 baseline | 0.30.238 GATING target | Notes (observed-only, NOT gating) |
|---|---:|---|---|
| `widgets|panels` post-mutation | miss% 23.0% | **`miss_delta=0`** | absolute miss% recorded for trend only |
| `widgets|buttons` post-mutation | miss% 26.9% | **`miss_delta=0`** | absolute miss% recorded for trend only |
| `widgets|markdowns` post-mutation | miss% 48.7% | **`miss_delta=0`** | absolute miss% recorded for trend only |
| `widgets|datagrids` post-mutation | miss% 22.7% | **`miss_delta=0`** | absolute miss% recorded for trend only |
| `restactions|*` post-mutation | various | **`miss_delta=0`** | absolute miss% recorded for trend only |
| `phase1_seed_widgets_by_cohort` cohort count | 5 | **≥ 30** | structural — proves Component A applied |
| `phase1_seed_units_planned_total` | 1078 | **≥ 2500** | structural — proves union widened |
| `seed_widgets_by_cohort` contains key `cyberjoker` | absent | **MUST be present** | direct mechanism check — see Falsifier A below |

The "absolute miss% < 1% steady-state" band that appeared in the prior draft is dropped per `feedback_l1_hit_invariant_is_100_percent` (the invariant is 100%, not "approximately 100%"). The post-mutation `miss_delta=0` from `bench verify-serve-stale` is the sole gating signal; absolute miss% over a steady-state window is reported but not used to gate.

### 4.7 Worst-case workload test plan

**Workload**: bench `run --scale 5000 --stages S1..S8` simulates 5K compositions + storm + steady-state customer dispatches. Post-S6 (storm complete), customer dispatches hit cohort cells for cyberjoker across `widgets|panels`, `widgets|buttons`, `widgets|markdowns`, `widgets|datagrids`.

**Probe ordering**:
1. S1 (boot): capture `/debug/vars`. Assert `units_planned >= 2500`, `widget_cohorts >= 30`.
2. S2-S5 (RBAC + composition install): capture `/debug/vars` after each stage. Assert `units_planned` stable (Component A only re-derives on boot; CRUD events go via Component B per-scope path).
3. S6 (storm): for at least 3 composition mutation events during the storm, run `bench verify-serve-stale` and require `verdict=PASS, miss_delta=0`.
4. S7-S8 (steady warm): each widget cell `miss%` absolute < 1%.

**Falsification**: any cell `widgets|<resource>` showing `miss_delta > 0` on the mid-mutation probe IS the F-3-pattern signature. Stage 2 HARD REVERT.

### 4.8 Rollback plan

Triggers for HARD REVERT:

- Falsifier A assertion fails (cyberjoker absent, units_planned <2500, or widget cohorts <30).
- Falsifier C `bench verify-serve-stale` returns any `FAIL_*` verdict.
- Boot wall-clock exceeds **15 min hard ceiling** (per §4.1 recovery policy). Do NOT escalate `prewarm.bootTimeoutMinutes` past 15 — revert and re-diagnose.
- Resident bytes growth exceeds the resident region cap (1.5 GiB) and `resident_demote_total` becomes non-zero (post-deploy verification item, per §7 out-of-scope).

Procedure:

1. `helm rollback snowplow <prev-rev>` (lockstep with chart `0.30.237` previous rev).
2. Tag rev not deleted (per `feedback_tag_commits`); branch left in place for post-mortem.
3. Capture `/debug/vars` snapshot at rollback for journal.
4. Diff `seed_widgets_by_cohort` and `units_planned` from 0.30.237 vs 0.30.238 to localize which component or which path failed.
5. If Component A failed (units_planned didn't grow OR cyberjoker absent from seed_widgets_by_cohort): the union logic is broken — bug in `unionCohorts`, the `cohortsFor` invocation site missed the union, or `EnumerateBindingSetClasses` returned empty at boot.
6. If Component A succeeded but boot exceeded 15 min: per-unit cost is structurally higher than projected — DIFFERENT defect needs diagnosing (e.g., upstream LIST regression, RBAC informer sync gap). Do NOT re-ship the same fix with a bigger timeout.
7. If Component B was shipped and failed (hook never fires): the cache→dispatcher hook registration in `main.go` is broken — verify `cache.SetWidgetCRHook` call site.
8. Append regression entry to `project_regression_journal.md` per `feedback_maintain_regression_journal`.

---

## 5. Concerns the trace surfaced (not blocking Stage 2 but worth surfacing)

1. **UAF semantics are invisible to BindingsByGVR** — load-bearing for Stage 2 design. Cyberjoker's access via UAF is structurally absent from the cohort enumeration index. Component A's union with global is a sound mechanism-uniform fix. The architecturally cleaner alternative is to **integrate UAF into the BindingsByGVR index** (`bindings_by_gvr.go` reads RESTAction CRs + their UAF stanzas and projects them as synthetic bindings). Larger surface (~150 LOC across cache + types), Diego-strategic per `feedback_team_drives_autonomous_decisions`. Out of scope for Stage 2; flagged for ship 0.30.240+ (Option 3 in §7).

2. **Walker harvest is 90 widget entries — bounded by the customer's nav graph, NOT a defect**. The bench has 47K widget CRs. The walker harvests only the navigable widget set (sidebar-nav-menu + RoutesLoader). The 90 entries are correct for the customer's nav graph. The miss problem is per-cohort, not per-widget. Walker coverage gap #156 as scoped in the user's brief is **NOT a direct cause** of the Gate C residual; the cohort-enumeration gap is. The user's brief asked architect to surface if walker-coverage was the actual defect — TRACE answer is NO, and the design pivots accordingly.

3. **`cohort_memo_entries_total = 92`** (vs `bindingset_classes_total = 35`). This counter tracks the GMC (Gated Memo Cache) cohort × target cells, NOT distinct cohorts. Separate index, not relevant to Stage 2 scope.

4. **0.30.235 has no F3 Pinned diff (reverted with 0.30.236)** — §2.8 probe-determinism captured on 0.30.235 implicitly demonstrates that refresher rewrites work even without F3 pinning. Pinning is a residency property (does the cell survive LRU sweeps); refresh is a rewrite property (does dirty-mark→re-resolve fire). These are independent mechanisms. Stage 1's F3 Pinned diff (already in HEAD) and Stage 2's Component A widening are also independent.

---

## 6. Summary table for PM gate

| Question | Answer |
|---|---|
| Falsifier artifact paths | (1) `/tmp/0.30.237-vars.json` (pre-storm seed-time view); (2) `/tmp/0.30.237-vars-mid-bench.json` (post-storm seed-time view); (3) `/tmp/snowplow-runs/0.30.235/probe-determinism-20260602-151950.json` (request-time view, refresher-fires-sub-50ms) |
| Top 3 file:line TRACED claims | (1) `prewarm_engine_boot.go:439-446` `cohortsFor` falls back instead of unioning — defect site; (2) `bindings_by_gvr.go:520-595` `EnumerateResourceCohorts` reads only K8s RBAC, structurally excluding UAF users like cyberjoker; (3) `refresher.go:684-723` `processOne` skips when entry doesn't exist — proves CRUD-trigger hooks alone don't seed missing cells |
| LOC budget (recommended) | **~55 LOC** (Option 1: 25 function + 30 mandatory unit test). Optional +272 LOC for Component B if PM elects Option 2. |
| Highest risk item | Component A 3.4× planned units overshoots boot budget. Chart 0.30.238 pre-positions `prewarm.bootTimeoutMinutes: 15` default; **HARD CEILING 15 min — REVERT on overrun, do NOT escalate further**. |
| Confidence miss_delta=0 post-storm | **~90% for Component A alone (Option 1)**. ~92% for A+B (Option 2). ~15% for B alone (predecessor placeholder approach). |
| Walker coverage gap #156 | NOT the direct cause; trace shows cohort-enumeration gap is. Walker observed 90 widget entries — correct for the customer's nav graph. Component A widens cohort enumeration; this is the actual lever. |
| Refresher functional? | Stage 2 takes no position on refresher cadence. §2.8 correction notice: the verify-serve-stale artifact does NOT prove sub-50ms refresh; it proves the L1 cell never existed (cold-fill on miss). Refresher-mechanics evidence is OUT OF SCOPE for this ship — Stage 2's fix is purely cohort-coverage at boot. |

---

## 7. PM-decision matrix

Two empirical traces converge on the same root cause (§2.8): cohort-cell coverage at boot, not refresh-trigger responsiveness. The architect's recommendation is reshaped accordingly.

### Option 1 (RECOMMENDED) — Ship Component A ALONE as 0.30.238

LOC ~55 (25 function + 30 mandatory `unionCohorts` unit test) + chart bump.

- **Closes Gate C** (load-bearing — the residual miss% is the cohort-coverage gap, and Component A alone closes it).
- **Does NOT require Component B** because the refresher is already firing in <50ms post-PATCH (§2.8). Once Component A widens cohort coverage to ~3,700 units, the dirty-mark → refresher → re-resolve chain handles all subsequent mutations.
- **Does NOT require RBAC-shift hook** because UAF users don't generate RB events anyway.
- **Smallest possible surface** per `feedback_recurring_regression_pattern` (5+ visits on this engine surface).
- Brand-new widget/RA CR ADDs (admin-UX case) pay one synchronous per-user-fallback Pinned Put on first dispatch. Bounded, single-call cost, already at the F3 cell-residency contract.

### Option 2 — Ship Component A + Component B together

LOC ~327 (A 55 + B 272). Adds widget-CR / RA-CR re-prewarm scopes. RBAC-shift surface stays OUT (§4.2 Correction B.2).

Pros:
- Eliminates the synchronous first-dispatch on brand-new CRs.
- Component B's hook surface is small + bounded; engine queue dedups storms.

Cons:
- Larger surface = larger revert probability on the recurring-regression-pattern surface.
- Component B is empirically **non-load-bearing** (refresher already works per §2.8). Shipping B with A trades 272 LOC for an admin-UX accelerator.

### Option 3 — HALT — escalate to Diego for UAF integration

If PM judges the §2 trace (cohort enumeration is structurally blind to UAF users) warrants the cleaner architectural fix (UAF projection into BindingsByGVR), don't ship 0.30.238 at all.

LOC for the architectural fix: ~150 across cache + types. Closes the gap mechanism-uniformly without the union workaround. But:
- Ships LATER than Component A (needs design review, type changes, more risk surface).
- Diego ratification per `feedback_team_drives_autonomous_decisions` strategic flag (cross-cutting BindingsByGVR + RESTAction parsing).

### Architect's confidence (revised)

- **Component A alone (Option 1)**: ~90% Gate C miss_delta=0 post-storm. Falsifier is `bench verify-serve-stale` + scale-5K bench (§4.6). The mechanism is empirically derivable from the two independent traces (§2.1-§2.4 + §2.8).
- **Component A + B (Option 2)**: ~92% (B adds marginal robustness, ~2pp). Trade is 11.9× LOC for 2pp marginal confidence.
- **Component B alone**: ~15%. Per §2.5: the hooks call the same `EnumerateResourceCohorts` that excluded cyberjoker at boot.

**Strong recommendation: Option 1.** The empirical evidence (two independent vantages converging) is the strongest data we've had on the residual since the 0.30.237 deploy. Ship the minimal fix that the data supports; defer Component B to 0.30.239 if/when post-Component-A telemetry shows the brand-new-CR cold-fill cost is meaningful.

---

## 8. References

### Code (TRACED — file:line cites)

- `internal/handlers/dispatchers/prewarm_engine_boot.go:401-472` — `buildCohortPlans` + `cohortsFor`
- `internal/handlers/dispatchers/prewarm_engine_boot.go:439-446` — the fallback-not-union defect site
- `internal/handlers/dispatchers/prewarm_engine.go:127-144` — `prewarmScope` struct (kind only today)
- `internal/handlers/dispatchers/prewarm_engine.go:213-222` — `enqueueScope` (idempotent dedup)
- `internal/handlers/dispatchers/prewarm_engine.go:249-261` — `StartPrewarmEngine`
- `internal/handlers/dispatchers/phase1_pip_seed.go:231-289` — `navWidgetHarvester` (needs IsHarvestedGVR helper)
- `internal/handlers/dispatchers/phase1_pip_seed.go:758-881` — `seedOneRestaction` (Pinned Put at 868-872)
- `internal/handlers/dispatchers/phase1_pip_seed.go:907-1017` — `seedOneWidget` (Pinned Put at 995-999)
- `internal/handlers/dispatchers/restactions.go:242-260` — per-user fallback Pinned Put
- `internal/cache/deps_watch.go:191-237` — informer event handlers (`AddFunc`/`UpdateFunc`/`DeleteFunc`)
- `internal/cache/deps.go:504-672` — `OnAdd`/`OnUpdate`/`OnDelete` → dirty-mark → refresher
- `internal/cache/refresher.go:684-723` — `processOne` (the `skippedNoEntry` no-entry skip site)
- `internal/cache/bindings_by_gvr.go:520-595` — `EnumerateResourceCohorts` (reads only K8s RB; no UAF)
- `internal/cache/binding_set_enumeration.go:220+` — `EnumerateBindingSetClasses` (the global universe)

### Artifacts

- `/tmp/0.30.237-vars.json` (pre-storm /debug/vars at rev 411)
- `/tmp/0.30.237-vars-mid-bench.json` (mid-bench /debug/vars at rev 411)
- `/tmp/0.30.237-gate-C-bench.log` (Stage 1 bench narrative)
- `/tmp/0.30.237-cohort-progress.log` (per-cohort progress from Lever B)

### Predecessor design docs

- `docs/ship-0.30.237-stage-1-boot-parallel-f3-reapply-2026-06-02.md` — Stage 1 design (shipped, reverted post-Gate-C-fail)
- `docs/ship-0.30.238-stage-2-crud-triggers-2026-06-02.md` — placeholder Stage 2 design (this doc supersedes; the placeholder mechanism alone would not fix Gate C per §2.5)
- `docs/bench-verify-serve-stale-design-2026-06-02.md` — bench harness design for the falsifier

### Feedback files cited (load-bearing)

- `feedback_check_k8s_clientgo_prior_art` — §3 prior-art check open
- `feedback_empirical_root_cause_trace_before_fix` — §2 trace methodology; §9 PM gate "would the fix actually make the symptom disappear?"
- `feedback_empirical_apiserver_probe_for_predicate_design` — n/a here (no wire-shape predicates added)
- `feedback_design_claim_worst_case_falsifier` — §4.6 falsifier covers post-CRUD-storm worst-case
- `feedback_no_park_broken_behind_flag` — Component A is the unguarded fix, no flag
- `feedback_l1_hit_invariant_is_100_percent` + `feedback_phase6_validates_l1_always_hit` — Gate C contract
- `feedback_mutation_serves_stale_while_refresh` — informs §4.2 mechanism choice
- `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill` — §4.1 explicitly conformant
- `feedback_no_special_cases` — §4.1 mechanism-uniform
- `feedback_recurring_regression_pattern` — §4.5 risk register addresses 5+-visit surface
- `feedback_architect_design_rigor` — every claim labelled TRACED/INFERRED with file:line
- `feedback_chart_release_lockstep` + `feedback_chart_repo_origin_is_upstream` — §4.8 rollback
- `feedback_helm_no_reuse_values_on_chart_default_change` — full --set override on chart bump
- `feedback_kubectl_verify_gke_context` — §4.6 falsifier opens with context guard
- `feedback_no_kubectl_in_measurement` — bench verify-serve-stale + scale-5K bench are not kubectl-measured

### Project anchors

- `project_narrow_rbac_shape` — cyberjoker = ZERO ClusterRoles, ONE Role in demo-system
- `project_composition_install_rbac_scale` — bench storm fires 5K+ RB ADDs (informs §4.5 Risk row)
- `project_single_cache_flag_direction` — Stage 2 implicit when CACHE_ENABLED=true; no separate flag

— cache architect, ship 0.30.238 Stage 2 design, 2026-06-02
