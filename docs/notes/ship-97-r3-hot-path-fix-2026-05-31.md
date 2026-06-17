# Ship #97 — R3 hot-path inert at apistage.go:140 — design

Signed: cache-architect. Date: 2026-05-31. Status: design-only (no code, no commits).
Successor binary slot: **0.30.214**. Chart lockstep: `snowplow-0.30.214` on `braghettos/snowplow-chart`.

> NOTE on scope correction. The dispatch brief enumerates "6 Put sites" (`restactions.go:234`, `widget_content.go:255`, + 4 others). On code trace, **exactly ONE Put site is the R3-relevant defect**: `internal/handlers/dispatchers/resolve_populate.go:255` (the refresher Put, polymorphic over class). The other 5 sites cited by the brief write entries of class `restactions`, `widgets`, `widgetContent`, or `raFullList` — none of which are consumed by the R3 fast path at `internal/resolvers/restactions/api/apistage.go:140` (which only reads entries of class `apistage`). The pprof report at line 144 lists those 5 sites correctly as "do not populate Items", but the R3 read path does not depend on them populating Items. The fix surface is therefore **1 site**, not 6. The brief's contract (≤60 LOC, single-call surface) holds; the count was over-stated.

---

## §1 Problem statement (TRACED)

**Symptom (TRACED, from `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/admin-ra-empirical-pprof-2026-05-31.md`):**

> Long-pole #3 — **Per-call LIST-envelope re-decode (`parseListEnvelope`) — `entry.Items` is never populated for refresh-Put entries**, so every content-cache hit takes the fallback `gateListEnvelope` path. 34.27% cum CPU (`gateListEnvelope` 6.56s of 19.14s sample); 557 GB alloc_space over 30s (38% of total). The pre-parsed Items hot-path (`gateListItemsWithMemo`, apistage.go:587) is invoked but the entry's `Items` field is nil for entries that came from refresher Puts.

**Read-path invariant (TRACED, `apistage.go:485-494`):**

```go
if entry, hit := store.Get(contentKey); hit && entry != nil {
    envelope = entry.RawJSON
    entryRef = entry
    if isList && len(entry.Items) > 0 {                          // ← R3 fast-path predicate
        parsed = parsedListEnvelope{
            items:      entry.Items,
            apiVersion: entry.ItemsAPIVersion,
            kind:       entry.ItemsKind,
        }
        haveParsed = true
    }
    ...
}
...
if haveParsed {
    gated, gateOK = gateListItemsWithMemo(ctx, memoStore, gvr, parsed)   // ← fast (sub-µs cohort memo)
} else {
    gated, gateOK = gateContentEnvelope(ctx, ..., envelope)              // ← falls back to parseListEnvelope (line 140)
}
```

R3 fires **only** when `entry.Items` is non-empty on Get. The MISS branch (`apistage.go:530-538`) populates Items + ItemsAPIVersion + ItemsKind via `parseListEnvelope(gvr, dispatched)`. The cluster-list collapse path (`cluster_list.go:357-363`) also populates Items via `validateClusterListShape`'s parsed envelope.

**Defect site (TRACED, `resolve_populate.go:255`):**

```go
c.Put(key, &cache.ResolvedEntry{
    RawJSON: encoded,
    Inputs:  &inputs,
    Pinned:  prePinned,
})
```

This Put is invoked by `resolveAndPopulateL1` (the refresher's single populate path, refresh-handler registered at `dispatchers.go:92` for `CacheEntryClassApistage`). For an apistage-class entry, `encoded` is the raw LIST envelope returned by `RefreshContentEntry → dispatchViaInformer` (`apistage.go:633`). The Put writes `RawJSON: encoded` with `Items: nil`. On the next content-Get-hit, R3 fast-path predicate `len(entry.Items) > 0` is **false**; the gate falls through to `gateListEnvelope → parseListEnvelope`, paying the full ~35 MB / 9k-item json.Unmarshal + 9k unstructured-wrap on every cache hit.

**Invariant violated:** All apistage content-class Put paths MUST populate `entry.Items` (+ ItemsAPIVersion + ItemsKind) for LIST envelopes so the R3 fast-path fires on subsequent Gets. The MISS branch and the cluster-list branch honor this. The refresher Put does not.

**Enumeration of Put sites by class (TRACED, `grep -rn 'ResolvedEntry{' internal/`):**

| # | File:line | Class written | Populates Items? | In R3 scope? |
|---|---|---|---|---|
| 1 | `internal/resolvers/restactions/api/apistage.go:522` | `apistage` (MISS branch) | YES (lines 530-538) | n/a — already fixed |
| 2 | `internal/resolvers/restactions/api/cluster_list.go:357` | `apistage` (cluster-list collapse) | YES (lines 360-362) | n/a — already fixed; dead post-S.2 revert |
| 3 | **`internal/handlers/dispatchers/resolve_populate.go:255`** | **polymorphic — `apistage` for refresher of apistage class** | **NO** | **YES — fix target** |
| 4 | `internal/handlers/dispatchers/restactions.go:234` | `restactions` (dispatcher L1) | NO | NO — different class |
| 5 | `internal/handlers/dispatchers/widgets.go:264` | `widgets` (dispatcher L1) | NO | NO — different class |
| 6 | `internal/handlers/dispatchers/widget_content.go:254` | `widgetContent` (identity-free widget shell) | NO | NO — different class (`gateWidgetEnvelope` consumes RawJSON directly) |
| 7 | `internal/handlers/dispatchers/phase1_pip_seed.go:831` | `restactions` (PIP seed) | NO | NO — different class |
| 8 | `internal/handlers/dispatchers/phase1_pip_seed.go:949` | `widgets` (PIP seed) | NO | NO — different class |
| 9 | `internal/cache/ra_full_list_store.go:63` | `raFullList` | NO | NO — different class |
| 10 | `internal/cache/ra_full_list_store.go:84` | `raFullList` | NO | NO — different class |

**Conclusion: 1 Put site, ~10-15 LOC, plus 1 helper export, plus tests.**

---

## §2 Fix mechanism

**Chosen approach: decode-on-Put inside `resolveContentEntryForRefresh` (apistage class only, LIST shape only).**

### Why this approach (vs alternatives)

| Option | Where parse runs | Cost amortisation | Hot-path effect | Notes |
|---|---|---|---|---|
| **A. Decode on Put in `RefreshContentEntry`** | Once per refresher cycle (background goroutine) | 1 parse per entry per refresh cycle | Hot-path: zero parse (R3 fires) | **PICKED** — minimal surface, cost stays in refresher budget |
| B. Decode on Put in `resolveAndPopulateL1` (after `resolveOnceFn`) | Once per refresher cycle | Same as A | Same as A | Requires resolve_populate to know apistage parsing — leaks knowledge across packages |
| C. Carry parsed Items alongside encoded bytes through the resolve chain | Once per refresher cycle | Same as A | Same as A | Requires changing the `resolveOnceFn` signature ([]byte → ([]byte, parsedListEnvelope, bool)) — broader blast radius |
| D. Lazy parse on first Get-hit | Once per entry-lifetime (deferred) | First-Get pays | Sub-ms after first hit | Adds branching to hot path; cost is paid by customer request, not refresher |

**Option A's cost stays in the refresher budget** (per `feedback_customer_priority_over_refresher`: refresher pollution is acceptable, customer hot-path must be fast). Customers benefit from the fast Get; refresher pays the same parse cost it pays today via `gateListEnvelope` falling back.

### Code surface (INFERRED — exact diff is dev's call)

**File:** `internal/resolvers/restactions/api/apistage.go`

Add a new exported helper, OR extend `RefreshContentEntry` to return the parsed form alongside raw bytes. The lowest-blast-radius approach is to add a separate exported helper that the refresher calls instead of `RefreshContentEntry`:

```go
// RefreshContentEntryWithParsed — Ship #97. Same as RefreshContentEntry but
// also returns the pre-parsed LIST envelope when the call is a LIST shape
// (name=="") and the dispatched envelope parses cleanly. Hands the parsed
// items off so the refresher's Put can populate entry.Items, restoring R3
// fast-path firing on subsequent Gets (apistage.go:487).
//
// Returns (raw, parsed, haveParsed, err). haveParsed=false for GET-by-name,
// or for a LIST whose envelope failed to parse (caller stores RawJSON only —
// the gate then takes the unmarshal fallback, byte-identical to today).
func RefreshContentEntryWithParsed(ctx context.Context, inputs cache.ResolvedKeyInputs) (
    []byte, parsedListEnvelope, bool, error,
) {
    gvr := schema.GroupVersionResource{Group: inputs.Group, Version: inputs.Version, Resource: inputs.Resource}
    raw, err := RefreshContentEntry(ctx, inputs)
    if err != nil || raw == nil {
        return raw, parsedListEnvelope{}, false, err
    }
    isList := inputs.Name == ""
    if !isList {
        return raw, parsedListEnvelope{}, false, nil
    }
    parsed, ok := parseListEnvelope(gvr, raw)
    if !ok {
        return raw, parsedListEnvelope{}, false, nil
    }
    return raw, parsed, true, nil
}
```

But this requires exporting `parsedListEnvelope` (currently unexported) — undesirable for a 1-site fix. **Better surface: keep parse internal to the api package, expose only the result fields.**

**Preferred surface:**

```go
// ParsedListEnvelopeFields — Ship #97. The minimal R3 hot-path inputs
// (Items + ItemsAPIVersion + ItemsKind) the refresher needs to populate
// on a content-entry Put so subsequent Gets fire the fast path. Returns
// (nil, "", "", false) for GET-by-name OR a LIST whose envelope failed
// to parse (caller stores RawJSON only — byte-identical fallback).
func ParseListEnvelopeForRefresh(inputs cache.ResolvedKeyInputs, raw []byte) (
    items []*unstructured.Unstructured, apiVersion, kind string, ok bool,
) {
    if inputs.Name != "" {
        return nil, "", "", false
    }
    gvr := schema.GroupVersionResource{Group: inputs.Group, Version: inputs.Version, Resource: inputs.Resource}
    parsed, parseOK := parseListEnvelope(gvr, raw)
    if !parseOK {
        return nil, "", "", false
    }
    return parsed.items, parsed.apiVersion, parsed.kind, true
}
```

**File:** `internal/handlers/dispatchers/resolve_populate.go`

Modify the apistage-class branch in `resolveOnceProd` to return the parsed fields, OR (less invasive) modify the Put site:

```go
c.Put(key, &cache.ResolvedEntry{
    RawJSON: encoded,
    Inputs:  &inputs,
    Pinned:  prePinned,
})
// Ship #97 — restore R3 fast-path on refresher Puts for apistage LIST entries.
// Without this, refresher-populated entries serve every subsequent content-Get
// via parseListEnvelope re-decode (34% cum CPU, 557 GB alloc / 30s — pprof
// 2026-05-31). The parse is byte-identical to the MISS-branch parse at
// apistage.go:530-538; the cost is paid ONCE per refresher cycle (background
// goroutine) instead of per-hit (customer hot path).
```

Best diff is to compute parsed fields BEFORE the Put and set them on the entry. Skeleton:

```go
// resolve_populate.go, in resolveAndPopulateL1, just before c.Put:
entry := &cache.ResolvedEntry{
    RawJSON: encoded,
    Inputs:  &inputs,
    Pinned:  prePinned,
}
if inputs.CacheEntryClass == cache.CacheEntryClassApistage {
    if items, apiVer, kind, ok := restactionsapi.ParseListEnvelopeForRefresh(inputs, encoded); ok {
        entry.Items = items
        entry.ItemsAPIVersion = apiVer
        entry.ItemsKind = kind
    }
}
c.Put(key, entry)
```

**LOC envelope:**
- `apistage.go`: +15 LOC (new exported helper + doc comment)
- `resolve_populate.go`: +10 LOC (apistage-class branch around Put)
- Tests: +60-100 LOC (1 unit test for `ParseListEnvelopeForRefresh`; 1 integration test for refresher → Put → Get-hit-fires-R3; 1 `-race` test for concurrent Get over a refresher-Put entry)

**Total: ~25 LOC production + ~80 LOC tests = ~105 LOC.**

---

## §3 Empirical pre-fix baseline (REQUIRED — `feedback_empirical_baseline_gate`)

### Architect-captured numbers (TRACED)

**(a) Widget-L1 hit ratio for refresh-populated entries** — TRACED from `/tmp/snowplow-internal-state-2026-05-31/debugvars.json` (via `docs/snowplow-internal-state-snapshot-2026-05-31.md`):

| Cell | Hit | Miss | Hit ratio |
|---|---|---|---|
| `widgetContent` (all GVRs) | 100% across 7 GVRs | — | 100% |
| `widgets` cells | 36–83% across 8 GVRs | varies | mid |
| `restactions` | 91 | 0 | 100% |

**These metrics are NOT direct measurements of the R3 fast-path firing.** The dispatcher-L1 cells above are not apistage-class. There is no current expvar exposing apistage-class content-cache hit ratio. Closest signal: `apistage-get-partial-shape` = 23 fall-throughs (a partial-shape guard, not a hit/miss counter).

**Direct apistage hit/miss counter is missing.** Dev's first task should be to confirm whether the existing `cache_lookups_total` expvar separates apistage from other classes. If not, the falsifier in §5 (pprof CPU share of `parseListEnvelope`) is the **only** TRACED pre-fix baseline available.

**(b) `parseListEnvelope` cum CPU share under admin compositions-panels burst** — TRACED from `/tmp/admin-ra-pprof-2026-05-31/cpu.prof`:

> `gateContentEnvelope → gateListEnvelope → parseListEnvelope → json.Unmarshal` = **33.59% cum CPU** of a 45s sample window during 20-concurrent admin compositions-panels burst.

**(c) Architect-INFERRED expected pre-fix per-wave latency: 200ms**, cited from the brief's "8-wave serial widget chain, each ~200ms steady-state warm."

**This is the load-bearing INFERRED claim that requires dev validation.** Per the pprof + Chrome-MCP cross-reference (admin-ra-empirical-pprof-2026-05-31.md §"Chrome MCP cross-reference"), `parseListEnvelope` dominates **admin compositions-panels admin-tail** (35 MB / 9k items), **NOT** the /dashboard piechart_correct path (1.7 KB piechart envelope, narrow-RBAC, structurally invariant under R3).

There is a **scope-mismatch risk** here: the pprof identifies the long-pole on a 35 MB LIST envelope; the brief targets /dashboard piechart_correct = 2,103ms. Whether the dashboard piechart chain hits apistage-class entries dominated by parseListEnvelope is **NOT TRACED**. The brief's "200ms per wave saved → 50ms per wave" projection assumes the dashboard chain's apistage Gets are bottlenecked on `parseListEnvelope` — which is plausible for the 50K-composition aggregate the piechart RA computes, but not directly measured.

### Dev's first task BEFORE coding

Capture an empirical pre-fix baseline of:

1. `parseListEnvelope` CPU% share via 60s pprof during **admin /dashboard warm reload + cyberjoker /dashboard warm reload** (NOT compositions-panels burst). This is the path the north-star metric piechart_correct measures.
2. piechart_correct mix-weighted warm = current measured 2,103ms (steady-state Chrome-MCP 2026-05-31).
3. If the captured `parseListEnvelope` CPU% on the dashboard path is < ~10% (i.e. R3 is NOT the dashboard bottleneck), **HALT**. Per `feedback_empirical_baseline_gate`, escalate to architect: the fix mechanism is sound but targets the wrong long-pole for the dashboard north-star.

### ±2× tolerance gate

| Metric | Architect design value (INFERRED) | Tolerance band (±2×) | Action if outside |
|---|---|---|---|
| `parseListEnvelope` CPU% during dashboard warm reload | ≥25% (cited from compositions-panels pprof) | ≥12.5% acceptable; ≥50% acceptable | If <12.5%: HALT, escalate. Fix mechanism is sound but does not move the dashboard north-star. |
| Pre-fix piechart_correct mix-weighted warm | 2,103ms | 1,000ms – 4,200ms | If outside: re-baseline before code. |
| Per-wave latency contribution from parseListEnvelope | 200ms (8 waves × 200ms ≈ 1,600ms of the 2,103ms) | 100-400ms acceptable | If <100ms: fix won't close to north-star; escalate. |

---

## §4 Post-fix expected delta (INFERRED — pre-flight falsifier is dev's empirical check)

| Metric | Pre-fix | Post-fix (INFERRED) | Delta |
|---|---|---|---|
| `parseListEnvelope` cum CPU share (admin compositions-panels burst) | 34% | **<5%** | -29 pp |
| `parseListEnvelope` cum CPU share (dashboard warm reload) | ≥25% (INFERRED, NOT TRACED) | **<5%** | -20 pp |
| Per-wave latency on dashboard piechart chain | ~200ms (INFERRED) | **~50ms** | 4× improvement per wave |
| piechart_correct mix-weighted warm | 2,103ms (TRACED 2026-05-31) | **~600-800ms** (INFERRED) | -65% |
| Refresher cycle CPU (per-cycle) | Today | +parse cost (sub-ms per cycle, INFERRED) | small refresher tax |
| 35 MB admin compositions-panels admin-tail wall-clock under burst | 38.7s p50 | **<25s p50** (INFERRED — only #3 long-pole closes; #1 wire backpressure, #2 rate-limiter contention, #5 marshal heap live remain) | partial |

**Confidence labels:**
- Admin compositions-panels burst CPU share — **HIGH** (direct pprof measurement; mechanism removes the exact code path).
- Dashboard piechart_correct delta — **INFERRED, not TRACED**. Whether R3 firing on the dashboard chain saves 200ms × 8 waves is a hypothesis until the dev's pre-fix falsifier proves it.

---

## §5 Pre-flight falsifier (HARD GATE per `feedback_falsifier_first_before_ship`)

**Definition:** EMPIRICAL pprof probe the dev runs BEFORE writing code.

### Capture instructions

**Path 1: parseListEnvelope CPU% on the dashboard hot-path (north-star scoring path)**

```bash
# Verify GKE context (HARD per feedback_kubectl_verify_gke_context)
kubectl config current-context
# expect: gke_neon-481711_us-central1-a_cluster-1

# Open snowplow debug port
kubectl -n krateo-system port-forward deploy/snowplow 18081:8081 >/dev/null 2>&1 &
PF_PID=$!
sleep 2

# Start CPU profile capture (60s window)
curl -s "http://localhost:18081/debug/pprof/profile?seconds=60" \
  -o /tmp/ship97-prefix-dashboard-cpu.prof &
PROF_PID=$!

# Drive 60s of admin + cyberjoker /dashboard warm reloads via Chrome MCP
# (per feedback_no_kubectl_in_measurement — public LB, not port-forward).
# Tester invokes Chrome MCP cells: admin/dashboard/warm × 6, cj/dashboard/warm × 6
# spaced at ~5s intervals = 60s window.

wait $PROF_PID
kill $PF_PID

# Analyse
go tool pprof -top -cum /tmp/ship97-prefix-dashboard-cpu.prof | grep -A2 -i 'parseListEnvelope\|gateListEnvelope\|json.Unmarshal' | head -30
```

**Expected pre-fix output:**

```
flat   flat%   cum     cum%   function
0      0%      <X>s    >=10%  parseListEnvelope
                              gateListEnvelope
```

**Pass criteria for proceeding with the fix:**
- `parseListEnvelope` cum CPU% ≥ 10% on dashboard hot-path → proceed.
- `parseListEnvelope` cum CPU% < 10% on dashboard hot-path → **HALT, escalate** (fix is correct but does not move north-star).

**Path 2: confirm refresher Puts dominate apistage class entries**

```bash
# /debug/vars dump — confirm apistage-class entries are being refreshed
curl -s http://localhost:18081/debug/vars | jq '{
  refresher_completed_total: .snowplow_refresher_completed_total,
  refresher_enqueue_total:   .snowplow_refresher_enqueue_total,
  cohort_memo_entries_total: .snowplow_cohort_memo_entries_total
}'
```

**Expected:** refresher_completed_total >> 0 (e.g. 14,160 over 61 min per the 2026-05-31 internal snapshot). Confirms refresher cycles are running and apistage entries are being re-Put without Items.

### Post-fix falsifier (post-deploy gate)

Same capture command, after 0.30.214 deploys and snowplow has been up ≥5 min (one refresher cycle for the dashboard's apistage entries):

```bash
go tool pprof -top -cum /tmp/ship97-postfix-dashboard-cpu.prof | grep parseListEnvelope
```

**Post-fix expected:** `parseListEnvelope` cum CPU% < 10% (target < 5%). If post-fix is still ≥ 25%, mechanism failed — investigate whether `ParseListEnvelopeForRefresh` is being called (slog at debug, dispatch count expvar) AND whether the entries fed to it are LIST-shape (Name=="").

---

## §6 Acceptance criteria (≥5 falsifiable, each maps to a measurement)

| # | Criterion | Measurement | Pass threshold |
|---|---|---|---|
| **AC-97.1** | R3 fast-path fires on refresher-populated apistage entries | apistage.go:483 hit-path debug log `preparsed=true` rate (post-fix vs pre-fix) | ≥90% of post-refresh hits log `preparsed=true` (pre-fix is 0% by mechanism) |
| **AC-97.2** | `parseListEnvelope` CPU share collapses on the north-star path | 60s pprof during dashboard warm reload | post-fix `parseListEnvelope` cum CPU% < 10% (target <5%) |
| **AC-97.3** | piechart_correct mix-weighted warm improves | Chrome MCP /dashboard warm cells (admin × 1 + cj × 1, mix-weighted 0.95 cj + 0.05 admin) | post-fix mix-weighted warm < 1,500ms (vs 2,103ms pre-fix) — target <800ms but 1,500ms is GREEN-band |
| **AC-97.4** | Output content-equivalence — served bytes are byte-identical for any user on any apistage LIST cell | Diff dispatch_delta on cyberjoker + admin compositions-panels and dashboard piechart cells (pre-fix vs post-fix) | zero non-noise diff (modulo status.traceId which is per-request) |
| **AC-97.5** | No customer-tax — request-path goroutines do NOT acquire the parse cost on the hot path | Verify `parseListEnvelope` callers in pprof: post-fix the call MUST come from `RefreshContentEntry → ParseListEnvelopeForRefresh` (refresher goroutines, not request goroutines) | Per-goroutine evidence: zero request-path stacks in `parseListEnvelope` post-fix |
| **AC-97.6** | Refresher cycle CPU does not regress by >10% | `snowplow_refresher_completed_total` rate vs `process.cpu_seconds_total` rate, pre-fix vs post-fix steady-state | refresher CPU% per cycle ≤ 110% of pre-fix |
| **AC-97.7** | -race test on concurrent Get over a refresher-Put entry passes | `go test -race ./internal/handlers/dispatchers/...` with a new test that Puts an apistage LIST entry via the refresher path, then concurrently reads it from 4 goroutines | zero race detector hits |

**Mix-weighted scoring (per `project_north_star_ledger`):**
- piechart_correct warm mix-weighted = 0.95 × cyberjoker + 0.05 × admin
- pre-fix: 0.95 × 2,095 + 0.05 × 2,245 = **2,103ms** (RED)
- post-fix target: < 1,000ms (GREEN); stretch < 500ms (NORTH-STAR)

---

## §7 Risk register

### R-decode-on-Put — refresher cycle CPU regression

**Mechanism:** `ParseListEnvelopeForRefresh` runs the same `parseListEnvelope` the MISS branch runs today, but inside the refresher goroutine. For each refresh cycle (~3.9/s steady-state per internal snapshot), this adds one full json.Unmarshal + N stripManagedFields + N unstructured-wrap allocs.

**Quantification (INFERRED):** Pre-fix, this parse runs in the request goroutine on every content-Get-hit (potentially many hits per refresh cycle). Post-fix, it runs once per refresh cycle. **Net should be a strict win** — the parse cost moves from per-hit to per-Put, which is a strictly lower rate (Puts << Gets in steady-state).

**Bound:** If refresher CPU% (`process.cpu_seconds_total` / `snowplow_refresher_completed_total`) regresses by >10% post-fix, **escalate**. Possible if the dispatched envelopes have many GET-by-name entries (where the parse is skipped, so no regression risk) vs many LIST entries (where parse cost is paid).

### R-Items-shape-mismatch — R3 fast-path sees a different items shape than today

**Mechanism:** `ParseListEnvelopeForRefresh` calls the SAME `parseListEnvelope` function the MISS branch (apistage.go:531) and the cluster-list collapse (cluster_list.go validateClusterListShape internals) call. The returned `parsedListEnvelope` struct has the same Items/ItemsAPIVersion/ItemsKind fields populated identically. `gateListItemsWithMemo` at apistage.go:587 consumes the same shape.

**Verification (TRACED):** apistage.go:140-170 is the single source. There is no shape branch.

**Bound:** unit test `TestParseListEnvelopeForRefresh_ItemsByteEquivalentToMissBranch` — given the same raw envelope bytes, ParseListEnvelopeForRefresh and the MISS branch produce identical `Items` slices (element-wise object equality after stripManagedFields).

### R-content-correctness — dispatch_delta on cyberjoker + admin

**Mechanism:** the served bytes flow through `gateListItemsWithMemo → listEnvelopeValue → jsonHandlerValue`. Pre-fix the same bytes flow through `gateContentEnvelope → gateListEnvelope → parseListEnvelope → gateListItems → listEnvelopeValue → jsonHandlerValue`. The two paths produce identical envelope shape per the doc at apistage.go:266-275 ("Output is byte-identical between the two").

**Verification:** AC-97.4. Run dispatch_delta on:
- cyberjoker compositions-panels (0 items expected)
- cyberjoker dashboard piechart (0 items)
- admin compositions-panels (~9000 items)
- admin dashboard piechart (50,000 count)

### R-race — concurrent Get over a refresher-Put entry

**Mechanism:** `ResolvedEntry.Items` is `[]*unstructured.Unstructured`. Once written by Put, the slice header is read by Get-hit goroutines without a lock (RawJSON immutability contract). The items pointed to are stripped + populated inside `parseListEnvelope` BEFORE Put, then shared by Get-hits via `gateListItemsWithMemo`'s SHALLOW envelope (apistage.go:249 listEnvelopeValue).

**Today's invariant (TRACED, apistage.go:243-263):** "Ship 2a removes that last writer: the gojq fork's deleteEmpty is now allocator-aware … the served tree is read-only and per-request."

**Per `feedback_shared_vs_copy_is_a_concurrency_change`:** the refresher Put is changing from `Items: nil` to `Items: <shared slice>`. This is **NOT** a swap of an existing shared reference — it is an initial population by a goroutine other than the one that produced the bytes today. Mandatory: `-race` test (AC-97.7).

**Specific scenario to falsify:**
```go
// pseudo-test
entry := refresherPut(apistageInputs, rawEnvelope)  // populates Items
var wg sync.WaitGroup
for i := 0; i < 4; i++ {
    wg.Add(1)
    go func() {
        defer wg.Done()
        for j := 0; j < 1000; j++ {
            _, _ = apistageContentServe(ctxWithUser, store, callOpts)
        }
    }()
}
wg.Wait()
// pass: zero race detector hits
```

### R-identity-propagation — the seed/refresh path identity check (per `feedback_seed_inherits_nested_call_identity`)

**Trace:** `resolveAndPopulateL1` builds `rctx` with `WithUserInfo` (refreshUser/refreshGroups — SA-canonical for identity-free classes, representative tuple otherwise). `resolveOnceFn` → `resolveContentEntryForRefresh` → `RefreshContentEntry` → `dispatchViaInformer(cache.WithApistageContentResolve(ctx), call)`. The marker `WithApistageContentResolve` is the SAME marker the MISS branch uses (apistage.go:516). The bytes returned are identity-free SA-maximal envelopes by construction.

`ParseListEnvelopeForRefresh` does NOT touch context — pure function over `(inputs, raw)`. Identity propagation is **unchanged** by the fix.

**Verification:** code review (no runtime check needed).

### R-special-cases — fix must not introduce hardcoded path/GVR/user logic (per `feedback_no_special_cases`)

**Trace:** the fix branches on `inputs.CacheEntryClass == cache.CacheEntryClassApistage`. This is a data-driven check on the cache contract (the same class predicate `resolveOnceProd` already uses at resolve_populate.go:302). No GVR/path/user literal.

`ParseListEnvelopeForRefresh` branches on `inputs.Name == ""` (LIST vs GET). Same predicate `apistage.go:115` and `apistage.go:462` use today.

**Verification:** code review.

### R-empirical-baseline-gate — design's piechart_correct projection may be wrong

**Mechanism:** §3 flags that the 200ms-per-wave INFERRED savings is not directly measured for the /dashboard piechart path. If dev's pre-flight falsifier (§5 path 1) shows `parseListEnvelope` < 10% CPU on the dashboard path, **the design is targeting the wrong long-pole** — the mechanism still works, but it won't close piechart_correct to north-star.

**Mitigation:** dev HALTS per `feedback_empirical_baseline_gate` if pre-fix falsifier baseline is >2× off design expectation.

---

## §8 LOC envelope

| Component | Lines |
|---|---|
| `internal/resolvers/restactions/api/apistage.go` (new exported helper + doc) | +15 |
| `internal/handlers/dispatchers/resolve_populate.go` (apistage-class branch at Put) | +10 |
| **Production total** | **+25** |
| `internal/resolvers/restactions/api/apistage_test.go` (`TestParseListEnvelopeForRefresh_*`) | +30 |
| `internal/handlers/dispatchers/resolve_populate_test.go` (`TestRefresherPopulatesItems_*`) | +30 |
| `internal/handlers/dispatchers/refresher_apistage_race_test.go` (concurrent Get with -race) | +40 |
| **Test total** | **+100** |
| **Grand total** | **+125** |

**Overshoot risk (per Ship S.2 lesson, 180 → 473):** moderate. The 25-LOC production envelope is tight; if the test harness for refresher-Put → apistage-Get-hit requires more scaffolding than expected, the test LOC could expand to 200. **Dev's gate:** if test scaffolding pushes over +250 total LOC, pause and review with architect before continuing.

---

## §9 Rollout

**Single binary:** `ghcr.io/braghettos/snowplow:0.30.214`.

**Lockstep chart tag** (per `feedback_chart_release_lockstep` and `feedback_chart_repo_origin_is_upstream`):
- Tag `0.30.214` on `braghettos/snowplow-chart` (NOT origin upstream — chart repo's `origin` is the upstream).
- Push to `braghettos` remote explicitly.
- Chart `appVersion: 0.30.214` + `version: 0.30.214`.

**No values.yaml changes. No new env vars** (per `project_single_cache_flag_direction` — end state is one CACHE_ENABLED flag).

**Helm upgrade** (NOT kubectl apply, per `feedback_never_kubectl_apply`):

```bash
helm upgrade snowplow braghettos/snowplow \
  -n krateo-system \
  --version 0.30.214 \
  --reuse-values
```

Or pass full --set override if any default changed (per `feedback_helm_no_reuse_values_on_chart_default_change`) — but the design does not change any chart default.

**Deploy gate:** `/readyz` returns 200 with phase1Done=true; first chrome-mcp dashboard cell shows `parseListEnvelope` cum CPU < 10% in a 60s pprof capture within 5 min of new pod becoming Ready (allows one refresher cycle for the dashboard's apistage entries).

---

## §10 Revert plan

**Mechanism:** clean tag rollback to 0.30.212 — **NOT a flag toggle** (per `feedback_no_park_broken_behind_flag`).

Note: 0.30.213 is the S.2 candidate that was hard-reverted at 2026-05-31 09:01 UTC; 0.30.214 is the next clean slot.

**Revert sequence:**

```bash
# Revert image
helm rollback snowplow -n krateo-system <prior-rev-of-0.30.212>
# Verify
helm history snowplow -n krateo-system | head -5
kubectl -n krateo-system get pod -l app.kubernetes.io/name=snowplow \
  -o jsonpath='{.items[*].spec.containers[*].image}'
# expect: ghcr.io/braghettos/snowplow:0.30.212
```

**No state to roll back beyond image** — the change is code-only; the L1 cache contents (Items field) are immutable once Put + the entries TTL out within `defaultResolvedCacheTTLSeconds` = 3600s = 1hr. A 0.30.214 → 0.30.212 rollback leaves at most 1hr of stale `Items`-populated entries in the cache; the 0.30.212 read path reads `len(entry.Items) > 0` and gates via `gateListItemsWithMemo` correctly (the field was added in 0.30.121 R3 — preserved across this revert).

**Chart-side:** rollback to `snowplow-0.30.212` chart tag on `braghettos/snowplow-chart` if 0.30.214 chart was deployed.

---

## Sign-off

cache-architect. Design-only — no commits. The fix surface is **single-site** (resolve_populate.go:255, with one helper export from apistage.go), ~25 LOC production, ~100 LOC tests.

**Critical pre-flight gate (§5 path 1):** dev MUST capture `parseListEnvelope` CPU% on the dashboard warm reload path BEFORE coding. If < 10%, halt + escalate — the fix mechanism is sound but does not move piechart_correct. If ≥ 10%, proceed.

Lessons folded in from today's session:
- `feedback_empirical_baseline_gate` (NEW) — §3 codifies the ±2× tolerance gate; §5 makes dev's first task an empirical pre-flight falsifier on the **actual north-star path** (dashboard), not the path the pprof was captured on (compositions-panels).
- Per-goroutine ground truth (`feedback_per_goroutine_evidence_beats_cpu_pprof`) — AC-97.5 mandates per-goroutine attribution post-fix (parseListEnvelope stacks come from refresher goroutines, not request goroutines).
- The brief's "6 Put sites" enumeration was wrong; only 1 Put site is in R3 scope. Logged at the top of this doc.
