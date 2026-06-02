# Ship 0.30.236 — L1-miss-after-mutation TRACE

**Architect** | **Date**: 2026-06-02 | **Scope**: highest-priority defect per Diego 2026-06-02

> Reframed contract (Diego, in-flight): "Every mutation MUST use serve-stale-while-refresh. COLD waterfall == WARM waterfall is the L1-hit invariant. If COLD > WARM, customer paid synchronous work — DEFECT."

## 1. Symptom

Bench `run-20260602-094024` at 5K-scale on 0.30.235. Stage S6 (post-deploy of 5,000 compositions) shows:

| Page | COLD ms | WARM ms | Ratio | Verdict |
|---|---|---|---|---|
| Dashboard | 2269 | 1125 | 2.02× | L1 MISS |
| Compositions | 1279 | 600 | 2.13× | L1 MISS |

Same pattern at S7 (post-delete-1) and S8 (post-delete-1-ns). **Only S1 (zero-state) shows COLD == WARM** (L1 HIT). Every measurement stage after ANY mutation shows the customer paying synchronous re-resolve cost — violating the serve-stale-while-refresh contract.

## 2. Live cluster probe (`snowplow-5d696f64c4-s5skg`, 0.30.235, helm rev 407)

`/debug/vars` and pod logs gathered 2026-06-02 ~11:43 CEST (54min after pod start; bench-test cluster state preserved: 499 bench-ns, 4989 compositions, cj-RB live).

### 2.1 Dispatch L1 lookup metric (`snowplow_dispatch_l1_lookups`)

The smoking-gun signal — per `(handlerKind, gvrString)`:

```
widgetContent|widgets...buttons             hit=    30  miss=     0
widgetContent|widgets...markdowns           hit=   139  miss=     0
widgetContent|widgets...navmenuitems        hit=   222  miss=     0
widgetContent|widgets...pages               hit=    74  miss=     0
widgetContent|widgets...panels              hit=    88  miss=     0
widgetContent|widgets...routes              hit=   148  miss=     0
widgetContent|widgets...rows                hit=    88  miss=     0
widgets|widgets...buttons                   hit=   240  miss=    62
widgets|widgets...datagrids                 hit=    84  miss=     6
widgets|widgets...markdowns                 hit=     1  miss=    11
widgets|widgets...navmenus                  hit=    72  miss=     2
widgets|widgets...panels                    hit=   250  miss=    50
widgets|widgets...piecharts                 hit=   119  miss=     4
widgets|widgets...routesloaders             hit=    72  miss=     2
widgets|widgets...tables                    hit=    84  miss=     4
TOTAL: hit=1711  miss=141
```

Two distinct cache classes observed:

1. **`widgetContent` (identity-free shell)** — **0 misses, 789 hits**. The shared apistage-content shell cache is fully effective.
2. **`widgets` (per-cohort)** — **141 misses out of 1,852 lookups (7.6%)**. The per-RBAC-cohort widget envelope cache has cold-resolve events on the customer path.

The `widgets|markdowns` cell shows 11/12 = 92% miss rate — the RBAC-sensitive widget cohort entries are virtually absent.

### 2.2 Refresher backlog metric

```
snowplow_refresher_enqueue_total: 615,737
snowplow_refresher_completed_total: 135,937
snowplow_refresher_failed_total: 946
snowplow_refresher_dropped_total: 157
snowplow_refresher_skipped_no_entry_total: 80,494
snowplow_refresher_queue_depth: 115   (live, while probe ran)
```

- 615K dirty-marks have enqueued, only 135K have been processed (~22% throughput) in ~54 minutes.
- **80,494 `skipped_no_entry`** — refresher dequeued a key whose L1 entry had vanished by the time the worker picked it up. **This is direct evidence that L1 entries are being EVICTED between dirty-mark and refresh.** They are not being preserved as stale-and-served.
- 115 queue depth at probe time, even though bench has stopped.

### 2.3 Cohort memo + walk metrics

```
snowplow_cohort_memo_entries_total:  16
snowplow_cohort_memo_total_bytes:    299,874
snowplow_phase1_walk_observations_total: 273
snowplow_phase1_bindingset_classes_total: 35
snowplow_phase1_bindingset_seed_resolves_total: 0
snowplow_phase1_seed_restactions_total: 0
snowplow_phase1_seed_widgets_total:    0
```

- Walk ran (273 observations).
- 35 bindingSet classes built.
- **PIP seed totals = 0 BUT THIS IS A METRIC NOT WIRED on the engine path**, not a "seed didn't run" — see §5.3.

### 2.4 Pod log evidence — composition CRUD storm

Sample (one per `bench-ns-NNN` ADD/UPDATE event during S6):

```
"cache_event.consumed", type=ADD, gvr=widgets.../panels, l1_keys=11
"cache_event.consumed", type=ADD, gvr=/v1.../configmaps, l1_keys=193
"cache_event.consumed", type=UPDATE, gvr=composition.krateo.io.../githubscaffoldingwithcompositionpages, l1_keys=11
```

Each composition deploy fires roughly: 1 CR ADD + ~5 panel ADDs + 1 configmap ADD + ~3 UPDATEs. At 5K compositions ≈ 50K CR events × 11–193 dirty-marks/event ≈ 1.1M – 9.6M dirty-mark enqueues. Refresher capacity has fallen behind by ~3-5× — and the entries it tries to refresh have been evicted from L1 (`skipped_no_entry: 80K`).

## 3. Reframed hypotheses falsification

Diego's mid-flight reframe: "WHERE in the request path is cold-fill happening synchronously instead of serve-stale-then-refresh?" Original H1–H5 collapse to:

### F1: Dispatcher serves stale L1 entry on `Get()` hit — REFUTED (mechanism works as designed)

TRACED `internal/handlers/dispatchers/restactions.go:135-150` and `widgets.go:183-196`:

```go
if cacheHandle != nil {
    if entry, ok := cacheHandle.Get(cacheKey); ok {
        emitResolvedCacheLookup(...true...)
        writeResolvedJSON(wri, entry.RawJSON)
        return                       // L1 HIT → serve
    }
    emitResolvedCacheLookup(...false...) // L1 MISS → fall through
}
// Synchronous Resolve(...) on customer goroutine
```

`ResolvedCacheStore.Get` (`internal/cache/resolved.go:700-723`) returns the entry whenever the key is present and TTL is not expired. **No dirty-flag check.** A dirty-marked entry IS served. So a cold-fill on the customer path proves the entry was *absent* (evicted) — not "dirty + re-resolved synchronously".

### F2: Dirty-mark evicts the entry — REFUTED (dirty-mark is enqueue-only)

TRACED `internal/cache/deps.go:524-558` (`onChange`):

```go
for l1Key := range matched {
    if enqueue != nil {
        enqueue(l1Key)        // refresher enqueue ONLY
    }
    marked++
}
```

ADD/UPDATE never evict. DELETE only evicts the entry whose Inputs identify the deleted object (self-representation, `deps.go:611-623`). Cross-cohort entries dirty-mark.

### F3: Entry is dropped between dirty-mark and refresh due to LRU eviction — TRACED + CONFIRMED

Two TRACEs combine to prove the entry-loss path:

**F3.a — production code never sets `Pinned: true`** for the per-cohort dispatcher cells:

```
$ grep -rn "Pinned:.*true" internal/
internal/cache/ra_full_list_slice_test.go:341, 389, 417  (TESTS ONLY)
```

ZERO production callers. The seed path Put (`phase1_pip_seed.go:836`) writes an unpinned entry; the per-user fallback Put on cache miss (`restactions.go:235`, `widgets.go:265`) writes unpinned; the refresher re-Put (`resolve_populate.go:255-263`) writes `Pinned: prePinned` where `prePinned` is computed only for `CacheEntryClassRAFullList` (`resolve_populate.go:107-112`) and is `false` for every other class — **including the customer-facing `widgets` and `restactions` cohort cells**.

**F3.b — resident region exists but is unused**: configmap has `RESOLVED_CACHE_MAX_RESIDENT_BYTES=1.610612736e+09` (1.5 GiB pinned budget) and `RESOLVED_CACHE_MAX_BYTES=3.221225472e+09` (3 GiB total). The pin-honour logic at `resolved.go:761-773` works correctly, but no production entry ever requests `Pinned: true` for the dispatcher classes, so the 1.5 GiB resident budget protects nothing customer-facing.

Cascade: bench creates 5K compositions → `cache_event.consumed l1_keys=11–193` per event → tens of thousands of dirty-mark enqueues; each refresher cycle Put-replaces the entry as **un-pinned**, eligible for LRU eviction. Meanwhile new widget/restaction Puts from refresh waves of OTHER cohorts fill the LRU. The 1.5 GiB resident region sits empty; the 3 GiB transient region churns. Customer reads an evicted cohort cell → MISS → synchronous cold-fill.

**This is also the source of the 80,494 `refresher_skipped_no_entry_total`**: by the time the refresher worker drains the queue, the entry it queued to refresh has been LRU-evicted by a later refresher Put under a different key.

### F4: Composition CRUD events trigger NO prewarm re-fire — TRACED + CONFIRMED (architectural gap)

The dynamic-prewarm engine has the scaffolding but is **wired only for `scopeKindBoot`**. TRACED `internal/handlers/dispatchers/prewarm_engine.go:129-139`:

```go
const (
    scopeKindBoot prewarmScopeKind = "boot"
    // Ship 2 (NOT wired this ship): scopeKindWidgetCR (a widget/RESTAction
    // CR add/update/delete re-walks that object's subtree) and
    // scopeKindRBACShift (an RBAC binding shift re-seeds the affected GVRs'
    // cohorts). The engine queue + rePrewarm core are built to accept these
    // with no refactor.
)
```

And `phase1_walk.go:358-360`:

```go
// Ship 1 enqueues only the BOOT scope. Ship 2 wires runtime
// triggers (widget/RESTAction CR + RBAC shift) to enqueueScope.
prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindBoot})
```

The comment is verbatim explicit: composition CR ADD/UPDATE/DELETE and binding shifts **do not** re-enqueue the prewarm engine. The dirty-mark path is the only response to CRUD; the prewarm engine only fires at boot. This contradicts `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill` (Diego 2026-05-28): "Widget/RESTAction/binding CRUD re-runs SAME prewarm logic."

For 0.30.236 this is not the **proximate** cause of the COLD > WARM symptom (that is F3), but it is the architectural reason the customer-facing cohort cells are exposed to LRU churn in the first place: there is no mechanism to **re-pin** a cohort's cells after a CRUD-induced refresh cycle, and there is no scheduled re-seed.

### F5: Bench measures BEFORE prewarm completes (race) — REFUTED

The bench window straddles S6, S7, S8 — each separated by deploy waits. Refresher backlog at 115 queue-depth on a quiesced cluster (no bench traffic since the run ended ~50 min ago) refutes the "prewarm not done yet" framing. The lag is **steady-state**, not a startup transient.

## 4. Root cause

The L1 dispatcher cache for the `widgets` and `restactions` classes is **fully transient under LRU pressure**:

- Cohort entries are written `Pinned: false` by both the boot PIP seed and the refresher re-populate.
- The 1.5 GiB resident region is enabled in production config but never used by these classes.
- The composition-CRUD storm produces a multi-million dirty-mark enqueue burst that swamps the refresher; the refresher's own re-Put churn evicts other entries via LRU.
- By the time the customer reads a cohort cell, it has been evicted (refuted-and-vanished, not stale-and-served).
- Dispatcher fallback is synchronous re-resolve — the customer pays the cold-fill cost.

This is a **direct violation of serve-stale-while-revalidate**: the cache should retain the stale entry, serve it, AND schedule background refresh. Today it retains the entry only at the mercy of LRU.

## 5. Fix design — pin cohort cells; eliminate customer-path cold-fill

### 5.1 Scope (Ship 0.30.236)

ONE change, mechanism-uniform: **the boot PIP seed and the refresher re-populate MUST write `Pinned: true` for every `widgets`/`restactions` entry**, so cohort cells live in the resident region and are not LRU-evictable.

This makes the existing 1.5 GiB resident budget perform its designed job. The pin-honour logic already degrades gracefully on overflow (`resolved.go:761-772` — overflow demotes the new pin to transient rather than evicting another pinned cell; cap is enforced).

### 5.2 Concrete code changes

**File: `internal/handlers/dispatchers/phase1_pip_seed.go`** — `seedOneRestaction` line 836:

```go
// before:
handle.Put(key, &cache.ResolvedEntry{
    RawJSON: encoded,
    Inputs:  inputs,
})

// after (0.30.236):
handle.Put(key, &cache.ResolvedEntry{
    RawJSON: encoded,
    Inputs:  inputs,
    Pinned:  true,        // cohort cell — resident region
})
```

Same change at `seedOneWidget` (similar Put around line ~990 — locate and edit symmetrically).

**File: `internal/handlers/dispatchers/resolve_populate.go`** — `resolveAndPopulateL1` line 255-263:

```go
// before:
entry := &cache.ResolvedEntry{
    RawJSON: encoded,
    Inputs:  &inputs,
    Pinned:  prePinned,   // only set for RAFullList
}

// after (0.30.236):
pinThis := prePinned
switch inputs.CacheEntryClass {
case cache.CacheEntryClassRAFullList:
    // unchanged — prePinned from prior entry
case "widgets", "restactions":
    pinThis = true        // cohort cell — refresher preserves residency
}
entry := &cache.ResolvedEntry{
    RawJSON: encoded,
    Inputs:  &inputs,
    Pinned:  pinThis,
}
```

**File: `internal/handlers/dispatchers/restactions.go`** — `cacheHandle.Put` line 235 (per-user fallback after cold resolve):

```go
cacheHandle.Put(cacheKey, &cache.ResolvedEntry{
    RawJSON: encoded,
    Inputs:  cacheInputs,
    Pinned:  true,        // 0.30.236 — protect freshly resolved cohort cell
})
```

**File: `internal/handlers/dispatchers/widgets.go`** — symmetric change at `cacheHandle.Put` line 265.

Total LOC: ~12 lines across 4 sites. No new types, no new functions.

### 5.3 Telemetry fix (zero-bug, but unblocks gate)

The engine path does **not** bump `pipSeedRestactionsTotal` / `pipSeedWidgetsTotal`. Add the bumps inside `prewarm_engine_boot.go:seedScopeYielding`:

```go
// after seedOneRestaction success at line ~286:
pipSeedRestactionsTotal.Add(1)
// after seedOneWidget success at line ~318:
pipSeedWidgetsTotal.Add(1)
```

This is a metric-wiring fix only; behaviour unchanged. Without it, the post-deploy gate cannot read "did the seed run?" out of /debug/vars.

### 5.4 What 0.30.236 does NOT do

Not in this ship (intentionally deferred):

- **Wire `scopeKindWidgetCR` / `scopeKindRBACShift`** on the prewarm engine (F4). The current dirty-mark + refresher path is sufficient *once cohort cells are pinned*; the customer never sees a cold-fill because the cell stays resident across CRUD storms. Re-walk on every CRUD event is expensive and should be deferred until measured.
- **Refresher backlog cap / yield re-tuning**. The 615K-enqueue backlog is real but does not affect customer correctness once cells are pinned (refresher being late means stale-but-served, which the contract permits).
- **Coverage of `apistage` / `widgetContent` classes**. Those already have 0 misses; no change needed.

### 5.5 Effort + risk

- LOC: ~14 changed lines across 5 files (4 production + 1 telemetry).
- Compile risk: negligible (additive boolean field already exists on `ResolvedEntry`).
- Memory risk: pinned cohort cells flow into the 1.5 GiB resident region. Per the live snapshot, 16 cohort memo entries × ~18 KB avg = ~300 KB total. Even at the production cohort fan-out (35 bindingSet classes × ~150 widgets × ~5 KB avg envelope) ≈ 25 MiB — well under 1.5 GiB.
- Pin-overflow safety: `resolved.go:766-772` already degrades to transient on resident overflow; no OOM path.
- Refresher backlog: not eliminated by 0.30.236, but the customer is no longer exposed to it.

## 6. Pre-commit falsifier

Live cluster (`gke_neon-481711_us-central1-a_cluster-1`, helm rev 407, 0.30.235) IS the reproducer. **Do not redeploy** until 0.30.236 binary is ready.

**FALSIFIER A — 0.30.235 binary fails** (reproduce current MISS):

1. Pick a `widgets|markdowns` cohort lookup that has miss > 0 in `snowplow_dispatch_l1_lookups` (current snapshot: hit=1, miss=11).
2. With cluster in current bench-tail state (5K compositions, post-S8), issue 5 `GET /call` requests from `cyberjoker` against a fresh markdown widget (one that was bench-deployed) — record p50 latency.
3. EXPECTED on 0.30.235: at least one miss recorded in the metric delta (`miss_total++`), p50 > 200ms reflecting synchronous re-resolve.

**FALSIFIER B — 0.30.236 binary passes** (after deploy):

1. Same 5 requests against the same widget after 0.30.236 is up + boot completes (`prewarm.engine.boot.complete` log line present).
2. EXPECTED on 0.30.236: every request `miss_total` increment = 0; all 5 are hits; p50 < 80ms.
3. Issue 100 CR ADD events from the bench script (`update_storm_generator.py`), wait 5s, issue the same 5 requests.
4. EXPECTED on 0.30.236: still 0 misses, p50 < 80ms — cohort cell survived the dirty-mark wave because it is now pinned.

## 7. Post-deploy gate

Re-run `e2e/bench/snowplow_test.py` at SCALE=5000 (Diego: `feedback_test_scale_50k` permits sub-scale for invariant gates; 5K replicates the 0.30.235 result). Gate criterion per the L1-always-hit invariant Diego ratified 2026-06-02 (`feedback_phase6_validates_l1_always_hit`):

| Stage | Page | Pass criterion |
|---|---|---|
| S1 | All | COLD ≤ 1.05× WARM (already passes) |
| S6 | Dashboard | **COLD ≤ 1.05× WARM** (was 2.02× — FAILS today) |
| S6 | Compositions | **COLD ≤ 1.05× WARM** (was 2.13× — FAILS today) |
| S7 | All | **COLD ≤ 1.05× WARM** |
| S8 | All | **COLD ≤ 1.05× WARM** |

Secondary signals on /debug/vars after the gate run:

- `snowplow_dispatch_l1_lookups` — every `widgets|*` cell must show `miss_total == 0` in the delta over the bench window.
- `snowplow_refresher_skipped_no_entry_total` — delta must not grow disproportionally to enqueue rate (target: < 5% of new enqueues).
- `snowplow_cohort_memo_total_bytes` — must rise but stay under `RESOLVED_CACHE_MAX_RESIDENT_BYTES` (1.5 GiB).

## 8. Risk register

| Risk | Likelihood | Severity | Mitigation |
|---|---|---|---|
| Resident region overflow → silent demotion of new entries | Low | Med | Existing demote path counts `residentDemoteTotal`. Add expvar publication (1 line) so the gate can observe it. |
| Pinned cells outlive their staleness window (TTL still applies — `resolved.go:713`); a stuck entry could mis-serve | Low | Low | TTL=3600s. The refresher continues to re-Put; staleness window matches today. |
| Refresher backlog still grows | Confirmed | Low | Customer no longer exposed to it. Document as separate follow-up. |
| Other entry classes (apistage, widget-content) regress | None | — | Those classes use `prePinned` from the prior entry; the change branches narrowly on `widgets` / `restactions`. |
| Recurring regression on the prewarm surface (`feedback_recurring_regression_pattern` Change A) | Low | Med | This ship narrows to a single boolean per-entry; not a structural re-walk or re-shape. Pre-commit falsifier verifies on the live cluster before commit. |

## 9. What this ship does NOT close

- **F4** — `scopeKindWidgetCR` / `scopeKindRBACShift` engine wiring (the `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill` contract). Out of scope; deferred to Ship 2 once 0.30.236 confirms F3 is the dominant defect. If post-fix bench still shows COLD > WARM at S6 specifically for *new compositions* the customer hasn't seen before, F4 becomes the next target.
- Refresher throughput tuning (615K enqueue backlog at quiesced cluster). Customer impact eliminated by 0.30.236, but the underlying amplification (configmap ADD → 193 dirty-marks) is its own ship.
- The `pip_seed` telemetry hole — the 5.3 telemetry-only fix above is sufficient; deeper refactor of cohort visibility is its own ship.

## 10. References

- `feedback_phase6_validates_l1_always_hit` (Diego 2026-06-02) — the L1-hit-coverage invariant.
- `feedback_mutation_serves_stale_while_refresh` (Diego 2026-06-02) — serve-stale contract.
- `feedback_l1_invalidation_delete_only` — DELETE-only eviction; ADD/UPDATE dirty-mark only.
- `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill` (Diego 2026-05-28) — the long-term direction (F4 ship).
- `internal/cache/resolved.go:746-805` — the pin-honour LRU logic, already correct.
- `internal/handlers/dispatchers/prewarm_engine.go:129-139` — the explicit "Ship 2 NOT wired" comment.
- Live cluster `/debug/vars` snapshot 2026-06-02 11:43 CEST (pod `snowplow-5d696f64c4-s5skg`).

— architect, 0.30.236, 2026-06-02
