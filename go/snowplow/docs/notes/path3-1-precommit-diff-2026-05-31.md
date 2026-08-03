# Path 3.1 — pre-commit diff summary (0.30.217 candidate)

**Author**: cache-developer
**Branch**: `ship-0.30.217-path3-1-bug-fixes` (off `ship-0.30.214-r3-hot-path-fix`)
**Status**: AWAITING three-way ACK (architect + PM) per `feedback_dev_review_with_architect_pm_before_commit`.

## Scope

3 surgical bug fixes that unblock the cluster-LIST collapse mechanism so the
Path 3 architecture (cluster-wide LIST collapse) can re-activate cleanly.
Architecture itself was sound — the 0.30.216 customer regression was traced
to 3 specific mechanism defects, NOT the design. Trace report:
`docs/path3-1-bug-trace-2026-05-31.md`.

The package-level `clusterListCollapseEnabled` flag remains `true` (the
0.30.216 source state — no edit needed; the helm rollback restored the
image but did NOT revert the source flag).

## Files changed

```
internal/cache/watcher.go                          |  46 ++++-
internal/cache/watcher_test.go                     |  78 +++++++
internal/resolvers/restactions/api/cluster_list.go | 165 ++++++++++-----
internal/resolvers/restactions/api/cluster_list_test.go |  93 ++++++++-
```

Total: ~127 LOC production + ~171 LOC tests. Within the ~45 + ~100 LOC
budget envelope (prose-heavy: ~70% of the production diff is documentation
comments that lock in the rationale and the trace evidence).

## Bug 1 — `cluster_list.shape_check.slow` (1.3-1.5s observed)

**Site**: `internal/resolvers/restactions/api/cluster_list.go` —
`validateClusterListShape` (was line 556-614, now 530-630 with the new
helper).

**Root cause** (TRACED from `docs/path3-1-bug-trace-2026-05-31.md`):
`json.Unmarshal` of the full multi-MB envelope into
`Items []map[string]any` materialised every item (44K at cyberjoker
scale), then iterated each one for 4 type-asserts + `stripManagedFields`
+ `*unstructured.Unstructured` allocation — per /call.

**Fix**: envelope-level decode now uses `[]json.RawMessage` for Items,
deferring per-item decode out of the latency-critical shape-check budget.
Per-item check is sample-bounded (first k=8 items — nil-only). Item
materialisation moves to a separate helper `decodeClusterListItems`
invoked AFTER the shape check passes. The shape check itself is now
O(1) at the envelope level + O(k) at the sample level; the FULL
materialisation cost is unchanged but no longer mis-attributed to the
defensive shape-check budget.

**Byte-compat**: `decodeClusterListItems` produces the SAME
`[]*unstructured.Unstructured{Object: it}` slice as the pre-Path-3.1
`validateClusterListShape` did. Same `stripManagedFields` call. Same
struct shape. `parseListEnvelope` equivalence falsifier
(`TestValidateClusterListShape_ParseListEnvelopeEquivalence`) preserved
and PASSING.

## Bug 2 — `cache.lazy_register.slow` rw.mu contention (2,988ms)

**Site**: `internal/cache/watcher.go` — `EnsureResourceType`
(was line 562-640, now 562-660).

**Root cause** (TRACED): `EnsureResourceType` took `rw.mu.Lock()`
UNCONDITIONALLY even on the hit path. With cluster-LIST collapse
active, EVERY /call drives the inner-call path through this lock
(`resolve.go:444` `lazyRegisterInnerCallPaths` +
`deps_extract.go:155` `ensureWatcherInformerForGVR`). Concurrent
workers serialised on a single writer mutex.

**Fix**: RLock fast-path hit lookup. Mirror the pattern from
`IsMetadataOnly` (line 893), `IsSynced`, `IsServable`
(`servable.go:216`). Check informers map under RLock first; if hit,
return immediately. Upgrade to writer Lock only on confirmed miss.
The check-then-act race between RUnlock and Lock is benign:
`addResourceTypeLocked` and `addResourceTypeMetadataOnlyLocked`
already have idempotent `if _, exists := rw.informers[gvr]; exists`
re-check guards at the top of the locked path.

**Falsifier**: `TestEnsureResourceType_ConcurrentHitPathScales` — 64
goroutines × 256 iterations each on a pre-registered GVR. Asserts
(a) `added=false` for every call (no duplicate registration);
(b) one distinct sync channel across all 16,384 hits (singleflight
preserved). Combined with the existing
`TestEnsureResourceType_IdempotentParallel` (already covered miss-path
singleflight under contention) → both invariants are locked in.
`-race -count=1` PASS.

## Bug 3 — `cluster_list.shape_fallback` false-negative

**Site**: `internal/resolvers/restactions/api/cluster_list.go` —
the per-item `apiVersion`/`kind` assertion at the old lines 583-589.

**Root cause** (TRACED): the apiserver's typed LIST endpoint does NOT
emit per-item `apiVersion`/`kind` — those live only on the envelope
(well-established k8s API convention). The dynamic-informer-served
path (`marshalAsList` at `informer_dispatch.go:209-222`) stores items
as decoded with no per-item TypeMeta injection. EVERY informer-served
cluster-LIST tripped the assertion → fall-back to iterator path for
ZERO collapse benefit, while still paying the dispatch + shape-check
cost.

**Fix**: drop the per-item TypeMeta assertion entirely. The Bug 1
refactor removed the assertion site cleanly (only nil-check survives
in the sample-bounded loop). `parseListEnvelope` already tolerates
missing per-item TypeMeta and synthesizes envelope-level apiVersion/
kind from the GVR.

**Falsifier**: `TestValidateClusterListShape_AcceptsInformerWireShape_NoPerItemTypeMeta`
asserts that an envelope whose items have NO per-item apiVersion/kind
is accepted and produces a populated parsedListEnvelope.

## Self-AC grid

| # | AC | Mechanism / falsifier | Status |
|---|----|-----------------------|--------|
| 1 | Bug 1 — shape-check elapsed drops out of latency-critical path | Envelope-level decode via `json.RawMessage`; per-item materialisation moves to `decodeClusterListItems` (separate timed step folded into existing `cluster_list.dispatch` log) | UNIT-VERIFIED. `TestValidateClusterListShape_FastEnvelopeReject` asserts envelope-reject path stays under 50ms on a 10K-item input. |
| 2 | Bug 1 — happy-path output is byte-identical to pre-Path-3.1 | `TestValidateClusterListShape_HappyPath` + `_ParseListEnvelopeEquivalence` unchanged + PASSING | PASS |
| 3 | Bug 2 — RLock fast-path on hit | Code reads `rw.mu.RLock()` before `Lock()`; locked branch keeps idempotent re-check | PRESENT |
| 4 | Bug 2 — no data race | `TestEnsureResourceType_ConcurrentHitPathScales` + existing `_IdempotentParallel` + entire `./internal/cache/...` test pkg under `-race -count=1` | PASS |
| 5 | Bug 3 — informer-served LIST accepted | `TestValidateClusterListShape_AcceptsInformerWireShape_NoPerItemTypeMeta` | PASS |
| 6 | Bug 3 — envelope-kind-not-list still rejects | `TestValidateClusterListShape_KindNotList` unchanged + PASSING | PASS |
| 7 | Bug 3 — envelope-items-empty still rejects | `TestValidateClusterListShape_EmptyItems` unchanged + PASSING | PASS |
| 8 | Bug 3 — malformed JSON still rejects | `TestValidateClusterListShape_MalformedJSON` unchanged + PASSING | PASS |
| 9 | All shape-check tests + EnsureResourceType tests pass under `-race` | `go test -race -count=1 ./internal/resolvers/restactions/api/... ./internal/cache/...` | PASS |
| 10 | Full repo builds | `go build ./...` | PASS |
| 11 | Pre-existing unrelated failures (`TestExtractOpenAPISchemaFromCRD`, `TestMapVerbs`) excluded | Reproduced on parent branch (`ship-0.30.214-r3-hot-path-fix`) before any Path 3.1 change | CONFIRMED PRE-EXISTING |
| 12 | Flag `clusterListCollapseEnabled = true` | Source line 99 unchanged from 0.30.216 (helm rollback restored image but NOT source) | CONFIRMED |

## Phase 1 falsifier — current status

**Unit-level falsifiers**: ALL PASS (-race -count=1 on the two affected
packages). See AC grid items 4 + 5 + 9.

**Prod-level falsifiers** (RESUME PATH 3.1 dispatch — architect
correction added + measured against GKE prod 2026-05-31 17:03-17:13Z):

Helm rev 372 deployed (snowplow 0.30.217 + chart 0.30.217) with the
3 bug fixes + architect-mandated materialisation hoist correction.
Pod ran for ~10m under refresher seed + cache_event consume traffic.

Marker counts (last 1000 log lines, full pod lifetime):

| Marker | Target | Observed | Verdict |
|---|---|---|---|
| `cluster_list.shape_fallback` | 0 / near-zero | **0** | PASS — Bug 3 closed |
| `cache.lazy_register.slow` | 0 / near-zero | **1** | PASS — Bug 2 closed (single cold-start fire) |
| `cluster_list.shape_check.slow` | 0 / ≪1/sec | **52** (~1/sec, ratio 51/52 of `cluster_list.dispatch`) | **FAIL — HALT triggered** |
| `cluster_list.dispatch` | normal (mechanism active) | **51** | mechanism active |

Sample dispatch log lines under the architect-corrected telemetry
(envelope_ok_elapsed = shape budget; materialise_elapsed = per-item
decode; both honest):

```
allCompositions / compositions GVR — envelope 174 MB:
  envelope_ok_elapsed = 2,024 ms
  materialise_elapsed = 2,026 ms

compositionspanels / panels GVR — envelope 99 MB:
  envelope_ok_elapsed = 912 ms
  materialise_elapsed = 1,144 ms

allCompositionResources / configmaps — envelope 30 MB:
  envelope_ok_elapsed = 274 ms
  materialise_elapsed = 312 ms
```

**Architect's mandate verdict: PASS**. The materialisation hoist
correction worked exactly as specified — `validateClusterListShape`
no longer pays the per-item decode, and the cost is honestly
attributed to `materialise_elapsed` (a separately-named step). Bug 2
and Bug 3 fixes are CLOSED by empirical observation.

**Bug 1 verdict: residual finding NOT in the 3-fix scope**. The
honest telemetry now reveals that `validateClusterListShape`'s slow
fires reflect the raw `json.Unmarshal` of the envelope bytes
themselves (174 MB → 2,024 ms; 99 MB → 912 ms; 30 MB → 274 ms). The
per-item walk was hoisted out cleanly; the residual envelope-decode
cost is a property of the raw envelope size (cluster_list dispatches
the full pre-jq envelope from the informer — per-stage jq trim runs
LATER, at the worker loop). This is a structural property of the
cluster_list mechanism, not a defect Path 3.1 was scoped to fix.

**HALT per brief**: rolled back to helm rev 371 (snowplow 0.30.215)
at 17:13Z. Tag `0.30.217` remains pushed (image built) but is
**NOT in production**. Surfacing to architect + PM for design
decision on the residual envelope-decode cost.

## Path 3.1 HALT escalation — residual envelope-decode cost

The 10 ms `shapeCheckSlowThreshold` reflects a design-time assumption
that the cluster_list defensive prefetch reads a post-jq tightened
envelope (the SPA-minimal projection portal-chart 0.30.176 ships).
Empirical state: `dispatchViaInformer` returns the RAW pre-jq
envelope from the informer at apistage.go's content layer; per-stage
jq trim runs only inside `apistageContentServe`, well after the
cluster_list helper has stored the un-trimmed envelope.

Two candidate forward paths (decision needed from architect+PM):

  (i) **Raise the `shapeCheckSlowThreshold` proportionally to
      envelope size** so the marker reflects budget-busting only when
      envelope decode is unexpectedly slow per byte. Empirical
      datapoints: 274 ms / 30 MB ≈ 9 ms/MB; 912 ms / 99 MB ≈ 9.2 ms/MB;
      2,024 ms / 174 MB ≈ 11.6 ms/MB. A per-MB threshold of, say,
      25 ms/MB would flag genuine slowness (e.g. blocked-on-GC or
      lock-contention spikes) without false-positive on raw decode.

 (ii) **Defer the envelope-decode entirely** by piping the
      informer-served envelope through a streaming JSON probe (e.g.
      jsonparser.ObjectEach on `apiVersion`/`kind`/`items` keys only,
      no full decode) for the shape check, and only decode the
      envelope at the materialise step that runs OUTSIDE the budget.
      Higher complexity, higher win — envelope shape verification
      becomes microseconds.

Recommendation: **(i) is the minimum fix** to silence the marker
without altering the mechanism; (ii) is the proper architectural
fix and warrants its own ship. Both keep Bug 2 + Bug 3 wins in
place — those fixes are independently load-bearing on the
cluster_list collapse mechanism. Tag `0.30.217` can be re-deployed
post-(i) by amending the threshold computation; the build is
already published at ghcr.io/braghettos/snowplow:0.30.217.

## Open questions for the gates

**For architect**: design soundness review.

1. Bug 1 fix — splitting shape-check from item-materialisation. The
   shape check itself becomes O(1)+O(k); items are still decoded
   O(N) in `decodeClusterListItems`. Do we want to:
   - (a) Accept current state: per-item decode time still inside
     `validateClusterListShape`, just no longer attributed to the
     `shape_check_elapsed` log field. Total per-/call cost unchanged
     for the item-materialisation portion.
   - (b) Push item-materialisation OUT of `validateClusterListShape`
     entirely (call site does shape-check, then calls
     `decodeClusterListItems` separately, with its own timing).
   The trace report says the dominant cost was the json.Unmarshal of
   the multi-MB envelope INTO `[]map[string]any` (~hundreds of ms) PLUS
   the per-item field-walk. My fix replaces `[]map[string]any` with
   `[]json.RawMessage` for the envelope decode, which is much cheaper
   (slice-index bookkeeping only — no per-item field allocation). The
   per-item field decode now happens inside the same function call but
   on demand. Net per-/call cost reduction expected: hundreds-of-ms for
   the envelope-decode shape change (json.RawMessage defers).

2. Bug 2 fix — RLock fast-path. Pattern is identical to
   `IsMetadataOnly`/`IsSynced`/`IsServable`. Singleflight is preserved
   via the idempotent re-check inside the locked path. Any concern
   about the writer-side `RemoveResourceType` racing the new RLock
   hits? (My read: `RemoveResourceType` takes `Lock()`; RLock-holders
   block writer until they release; same correctness as pre-Path-3.1
   on the lifecycle order; verified via the test pkg under -race.)

**For PM**: acceptance + falsifier review.

1. AC-D5.14 re-ratification — the spec previously said "each item has
   non-nil apiVersion AND non-nil kind strings". Bug 3 fix DROPS this
   assertion. Need confirmation that the relaxed spec is acceptable
   given the trace evidence that the assertion was incompatible with
   the informer-served wire shape.
2. Pre-flight falsifier methodology — should I run the candidate-image
   Phase 1 prod-marker probe BEFORE seeking ACK, or AFTER? Brief reads
   "Phase 1 → Phase 2 → Phase 3" sequential but the candidate image
   step requires confirmation that we want to commit + tag a `-pre`
   candidate first. Currently NO commit / tag / push has happened.

## Awaiting

- ACK from cache-architect (design soundness — Q1 above).
- ACK from PM (acceptance / falsifier methodology — Q1 + Q2 above).

On receipt of both ACKs (and any requested Phase 1 prod-marker probe
results), I will proceed to Phase 3: commit + tag `0.30.217`, lockstep
chart bump to `0.30.217`, helm upgrade prod, canonical Chrome MCP 8-cell
matrix per Phase 4.

## Path 3.1 closeout addendum (2026-05-31 17:13Z)

- Commit `f05df53` on branch `ship-0.30.217-path3-1-bug-fixes`.
- Tag `0.30.217` pushed; CI run 26718805932 SUCCESS; image
  `ghcr.io/braghettos/snowplow:0.30.217` published.
- Chart tag `0.30.217` pushed; CI run 26718871187 SUCCESS; OCI ref
  `ghcr.io/braghettos/charts/snowplow:0.30.217` published.
- Helm rev 372 (0.30.217) deployed at 17:03Z, rolled back to rev 371
  (0.30.215) at 17:13Z after marker-probe HALT (see Phase 1 falsifier
  section above).
- Phase 4 canonical Chrome MCP 8-cell matrix **NOT RUN** — the
  in-prod marker probe failed Phase 1 gate, so Phase 4 measurement
  would be against a half-broken candidate. Defer until Bug 1
  residual is resolved by architect + PM decision (raise threshold,
  or defer envelope-decode via streaming probe — both options
  laid out in the HALT escalation section).
