# Ship D.5 / 0.30.152 — cluster-list-when-allowed iterator collapse

**Status:** DESIGN-READY for dev. PM-gated 2026-05-21 with **14 ACs**
(12 original + AC-D5.13 cache-off gate + AC-D5.14 defensive shape
check), **6 HARD GATES**, all 5 OQs closed (OQ-2 and OQ-5
re-ratified 2026-05-21 with structural binding). Branch A from
`docs/issue-213-admin-dashboard-9s-rediagnosis-0.30.151.md` ratified.

**One-liner.** Add an additive RA opt-in field
`spec.api[].clusterListWhenAllowed: true`. When the requester (or the
Phase 2 SA prewarm) holds cluster-scope `list` on the target GVR,
dispatch ONE cluster-scoped LIST against
`/apis/<g>/<v>/<resource>` instead of fanning out an
iterator over N namespaces. Cache under apistage key `(gvr, ns="",
name="")`. Per-user RBAC narrowing remains at the existing
`gateContentEnvelope` site (no cross-user leak). Fallback (default-off
or cluster-list-denied) is byte-identical to pre-D.5.

---

## §1. Diagnosis cross-reference

See `docs/issue-213-admin-dashboard-9s-rediagnosis-0.30.151.md` for the
full TRACED runtime-artifact evidence. Headline numbers grounding this
ship:

- Admin cold Dashboard 8.16s median; ~7.2s server-side in 80-NS
  iterator fan-out with apistage L1 HITTING for all per-NS slots.
- 71 timed `allCompositions` calls: median 56.6ms, p90 115ms — the
  arithmetic cost of N-call orchestration even with zero apiserver
  round-trips.
- Per-call cost decomposes to ~15-20ms filterListByRBAC over 1000-item
  slices (`internal/resolvers/restactions/api/informer_dispatch_rbac.go:93-180`)
  + ~5ms gojq per-stage filter + ~5-10ms merge + ~5-15ms
  scheduling/context-switch.
- Cyberjoker first-cold 26.72s = ~7s in-process + ~17-19s of apiserver
  round-trips on TTL-evicted slots (separate root cause; addressed by
  the queued Branch C, not D.5).

The diagnosis empirically refutes Candidates (1) widget-stage-key
share defect and (2) jq sort dominant (jq accounts for only ~10% of
admin cold). The true cost is the iterator-orchestration wall; D.5
collapses that to a single call.

---

## §2. Architectural mechanism — file:line citations

### §2.1 CRD schema addition

**File:** `apis/templates/v1/core.go:20-60` — the `API` struct that
holds existing fields `Name`, `Path`, `Verb`, `Headers`, `Payload`,
`EndpointRef`, `DependsOn`, `Filter`, `ContinueOnError`, `ErrorKey`,
`ExportJWT`, `UserAccessFilter`.

**Diff (additive only):**

```go
// API represents a request to an HTTP service
type API struct {
    // ... existing fields verbatim ...

    UserAccessFilter *UserAccessFilterSpec `json:"userAccessFilter,omitempty"`

    // ClusterListWhenAllowed declares that this API call is eligible
    // to dispatch as a SINGLE cluster-scoped LIST against
    // /apis/<g>/<v>/<resource> (instead of a per-namespace iterator
    // fan-out) when the requesting identity holds cluster-scope
    // `list` permission on the target GVR.
    //
    // Permission is checked against the Ship B typed RBAC snapshot
    // (cache.RBACSnapshot) via rbac.EvaluateRBAC(ctx, opts) with
    // opts.Namespace=="" — the existing cluster-list semantics at
    // internal/rbac/evaluate.go:198-211. On a deny verdict the call
    // falls through to the existing iterator path verbatim (no
    // behavioral change for non-cluster-list users e.g. cyberjoker).
    //
    // Default false: existing RestActions are byte-identical to
    // pre-D.5. Setting true is an OPT-IN by the RA author who has
    // verified that:
    //   1. The target GVR is namespace-scoped (cluster-scoped GVRs
    //      have no iterator pattern to collapse).
    //   2. A cluster-list dispatch returns the SAME object set the
    //      iterator fan-out would have aggregated. (For
    //      namespace-scoped resources, the apiserver
    //      cluster-scoped LIST endpoint
    //      `/apis/<g>/<v>/<resource>` returns objects across all
    //      namespaces; the iterator's per-NS LIST returns the
    //      same objects partitioned by namespace.)
    //   3. The widget consuming the RA's output applies any
    //      per-object narrowing through the existing serve-time
    //      RBAC gate (`gateContentEnvelope` at
    //      internal/resolvers/restactions/api/apistage.go:94-145),
    //      NOT through the RA's iterator shape.
    //
    // When this field is true but DependsOn.Iterator is empty, the
    // field is a no-op (there is nothing to collapse). When both
    // are set, the resolver runs §2.3's permission check and
    // selects the dispatch path.
    ClusterListWhenAllowed *bool `json:"clusterListWhenAllowed,omitempty"`
}
```

Pointer-bool with `omitempty` so the JSON shape on existing CRs is
unchanged (the field is absent in JSON when unset). The
`zz_generated.deepcopy.go` regenerates trivially via
`controller-gen`.

**AC-D5.1 discharge:** CRD-schema diff pre/post-D.5 shows ONE added
field, no removals, no required-flag changes — `clusterListWhenAllowed`
is optional (`*bool` + omitempty). YAML round-trip via existing
`controller-gen` is exercised by the existing CRD generator (no new
machinery).

### §2.2 RBAC check via Ship B typed snapshot — NO SubjectAccessReview

**File:** `internal/rbac/evaluate.go:110` —
`func EvaluateRBAC(ctx context.Context, opts EvaluateOptions) (bool, error)`.

EvaluateRBAC reads `cache.Global().Snapshot()` (the
`atomic.Pointer[RBACSnapshot]`) ONCE at the top of evaluation
(`evaluate.go:146-166`, AC-B.3 invariant), then runs
`evaluateAgainstInformer` which walks
`snap.ClusterRoleBindings` for cluster-wide permits
(`evaluate.go:198-211`). For an `opts.Namespace == ""` query, the
RoleBindings loop at `evaluate.go:213-235` is skipped (only
ClusterRoleBindings count for cluster-scope).

**This is exactly what D.5 needs.** No SubjectAccessReview, no
apiserver round-trip — the typed snapshot answer is sub-microsecond
under `Ship B` semantic-rules contract (AC-B.12 — semantics match
apiserver RBAC v1).

**The call site D.5 adds (sketch):**

```go
// In internal/resolvers/restactions/api/resolve.go, immediately AFTER
// the `ep` resolution at :330 and BEFORE the
// `tmp := createRequestOptions(ctx, log, apiCall, dict)` at :324.
// (Numbering will shift slightly post-edit; the load-bearing position
// is BEFORE createRequestOptions runs the iterator expansion.)

useClusterList := false
if ptr.Deref(apiCall.ClusterListWhenAllowed, false) && apiCall.DependsOn != nil && apiCall.DependsOn.Iterator != nil && *apiCall.DependsOn.Iterator != "" {
    // The RA opted in AND there is an iterator to collapse.
    // Derive the target GVR from the iterator's resulting Path
    // template — but the path template is per-iteration, so we
    // probe with a synthetic single-iteration evaluation OR we
    // require the RA to declare the target GVR.
    //
    // (See §2.3 for the resolved GVR-derivation approach.)
    gvr, derivedOK := deriveTargetGVRForClusterList(ctx, log, apiCall, dict)
    if derivedOK {
        userInfo, _ := xcontext.UserInfo(ctx)
        permitOpts := rbac.EvaluateOptions{
            Username:  userInfo.Username,
            Groups:    userInfo.Groups,
            Verb:      "list",
            Group:     gvr.Group,
            Resource:  gvr.Resource,
            Namespace: "",   // cluster-scope check (evaluate.go:198-211)
        }
        permit, err := rbac.EvaluateRBAC(ctx, permitOpts)
        if err == nil && permit {
            useClusterList = true
        }
    }
}
```

**AC-D5.2 discharge:** the permission check is sourced from
`cache.RBACSnapshot` (`internal/cache/rbac_snapshot.go`) through the
existing `rbac.EvaluateRBAC` entry point. **No
SubjectAccessReview path is touched.** Correctness is bound by
Ship B's semantic-rules contract (AC-B.12); ratified by the typed-RBAC
parity tests already in `internal/rbac/evaluate_test.go`.

### §2.3 Dispatch decision — target-GVR derivation and path collapse

The iterator's `Path` template (e.g.
`${ "/apis/composition.krateo.io/" + .version + "/namespaces/" + .namespace + "/" + .plural }`)
references both `.version` and `.plural` from the iterator dataSource.
Cluster-list collapse requires:

1. Deriving the target `schema.GroupVersionResource` from the per-call
   path template — which is per-iteration-evaluated, so we can't read
   it from the RA spec.
2. Constructing the cluster-scoped LIST path
   `/apis/<group>/<version>/<resource>`.

**Two approaches:**

**Approach (i) — synthetic single-iteration probe.** Run
`createRequestOptions` once over the iterator (`createRequestOption` at
`internal/resolvers/restactions/api/setup.go:55-79` is the per-element
builder), take the FIRST resolved `Path`, run
`cache.ParseAPIServerPathToDep(path)` at
`internal/cache/inventory.go:251-330` — get
`(gvr, ns, name)`. Strip the namespace segment to construct
`/apis/<gvr.Group>/<gvr.Version>/<gvr.Resource>`. **Cost:** one jq
evaluation of one iteration element. **Correctness:** the GVR is
identical across all iteration elements by construction (the iterator
fans out over `(crd × namespace)` pairs; same CRD across all of them).
**Recommended.**

**Approach (ii) — additive CRD field declaring the GVR.** Require the RA
author to spell out `clusterListGVR: { group, version, resource }` next
to `clusterListWhenAllowed`. **Rejected:** introduces a per-resource
literal in the RA spec, increases blueprint footprint, easy to drift
from the iterator's `Path` template.

**Implementation site:** new helper
`deriveTargetGVRForClusterList(ctx, log, apiCall, dict)` in
`internal/resolvers/restactions/api/setup.go` (sibling to
`createRequestOptions`), runs ONE jq evaluation of the iterator-stage
path template against the first iterator element, then
`cache.ParseAPIServerPathToDep`. Returns `(gvr, true)` on a
namespaced-form path; `(gvr, false)` for cluster-scope or malformed
paths (a cluster-scope path means the RA already operates cluster-wide
— no collapse needed).

**The dispatch site itself** is at
`internal/resolvers/restactions/api/resolve.go:355` — the existing
bounded-parallel iterator entry. D.5 inserts a branch BEFORE the
errgroup loop:

```go
if useClusterList {
    // Single cluster-scoped LIST.
    clusterCall := buildClusterListCall(apiCall, ep, gvr)
    tmp = []httpcall.RequestOptions{clusterCall}
}
```

`buildClusterListCall` constructs ONE `httpcall.RequestOptions` with
`Path = "/apis/<g>/<v>/<resource>"`, headers + JWT + endpoint inherited
from the resolved `ep`. Sets `Verb = http.MethodGet`. Then the
EXISTING bounded-parallel loop at :355 runs over a single-element
`tmp` — the apistage path at :589-610 (`apistageContentServe`)
handles the cache get/store; `gateContentEnvelope` runs the per-user
RBAC narrowing at `apistage.go:94-145`.

**Critical:** the rest of the loop is UNCHANGED. The collapse swaps
`tmp[N]` for `tmp[1]`; all downstream code (jsonHandler, dictMu,
errgroup, lazyRegisterInnerCallPaths) behaves byte-identically for the
single-call case (already covered by existing non-iterator stages).

### §2.4 Cache-key derivation — confirmed identity-free per Ship F1

**File:** `internal/resolvers/restactions/api/apistage.go:61-70` —
`contentKeyInputs(gvr, namespace, name)`. Returns a
`cache.ResolvedKeyInputs` with `CacheEntryClass:
cache.CacheEntryClassApistage`, `Group/Version/Resource` from GVR,
`Namespace` and `Name` as supplied.

**For D.5's cluster-list dispatch:**
- Path: `/apis/<g>/<v>/<resource>` (no `/namespaces/<x>/` segment).
- `cache.ParseAPIServerPathToDep(path)` at `inventory.go:251` returns
  `(gvr, ns="", name="")` — confirmed at `inventory.go:184-193` (the
  `/apis/<g>/<v>/<resource>` form returns
  `{Group, Version, Resource}` with empty namespace).
- `contentKeyInputs(gvr, "", "")` → `ResolvedKeyInputs{Namespace:"",
  Name:""}`.
- `cache.ComputeKey(in)` at `internal/cache/resolved.go:304-395`
  hashes `(version, class="apistage", g, v, r, ns="", name="",
  perPage, page, stage, extras)`. The apistage class triggers the
  `if in.CacheEntryClass != CacheEntryClassApistage { ... }` branch
  at `resolved.go:341-353` — Username/Groups are NOT hashed for
  apistage class. **Identity-free key**, shared across cluster-list-
  permitted users.

**Empirical verification at `resolved.go:126`** (PM-cited): the
`ResolvedKeyInputs.Namespace` field is `string` — `ns=""` is a
distinct, valid value that hashes to a distinct cache cell from any
non-empty namespace string. Unit test `TestApistageContentKey_ClusterScopeDistinctFromNamespaced`
discharges AC-D5.3.

### §2.5 UAF + RBAC narrowing at the serve-time gate

**File:** `internal/resolvers/restactions/api/apistage.go:94-145` —
`gateContentEnvelope` is the single F1 RBAC gate. It runs on BOTH the
cache-Get hit path AND the un-gated-dispatch-miss path
(`apistage.go:386-510`).

For a cluster-list cache entry:
- The entry is the raw apiserver envelope of ONE cluster-scoped LIST
  — items across ALL namespaces, identity-free.
- `gateContentEnvelope` calls `gateListEnvelope` → `gateListItems` →
  `filterListByRBAC` over the FULL 49000-item slice.
- `filterListByRBAC` (`informer_dispatch_rbac.go:93-180`) memoizes per
  namespace: it walks items, calls `it.GetNamespace()`, and runs ONE
  `EvaluateRBAC` per distinct namespace via `rbacMemo`.

**Key empirical fact:** for ~80 distinct namespaces in the item set,
filterListByRBAC runs 80 EvaluateRBAC calls — IDENTICAL to the 80
RBAC evaluations the iterator path runs (one per per-NS call). The
RBAC-eval cost is unchanged; what collapses is the
80× orchestration overhead (filterListByRBAC outer loop runs ONCE over
all items instead of 80 times over 1000-item slices).

**Pre-D.5 cost:** 80 × (15-20ms RBAC filter + 5ms gojq filter + 5-10ms
merge + 5-15ms scheduling) = ~3.5-4.5s in-process.

**Post-D.5 cost (admin, cluster-list path):** 1 × (~50ms RBAC filter
over 49000 items via per-NS memo + ~50ms gojq filter + ~10ms merge) =
~120ms. **~30-40× collapse in orchestration cost.**

**UAF interaction** — the layering contract per
`feedback_restaction_no_widget_logic`:
1. apistage cache stores the un-gated cluster-list envelope (49000
   items).
2. gateContentEnvelope runs filterListByRBAC under the REQUEST
   identity at every Get-hit. RBAC narrowing happens here.
3. The narrowed envelope feeds the stage's `Filter` (e.g.
   `compositions-list`'s
   `[.allCompositions.items[]? | {uid, name, ns, ts, kind, av, conditions}]`)
   → dict[id].
4. UAF (if present on the stage) runs as before — UAF's per-object
   `EvaluateRBAC` at the refilter step is downstream of step 2.

**AC-D5.4 discharge:** the cluster-list path receives the SAME serve-
time gate as the iterator path. UAF byte-equivalence test in §7
verifies admin + UAF identity produces byte-identical widget props on
cluster-list vs iterator dispatch for the same input fixture.

### §2.6 Default-off fallback semantics

**File:** the new `useClusterList` branch in §2.3 wraps the entire
collapse decision. When `apiCall.ClusterListWhenAllowed == nil` or
`false`, the branch is skipped, `tmp` is built by the existing
`createRequestOptions` at `setup.go:28-58`, and the rest of the loop
runs verbatim. **Default-false = pre-D.5 behaviour, byte-identical.**

When `ClusterListWhenAllowed == true` BUT the RBAC check denies
(cyberjoker), `useClusterList = false` and the same fallback applies.

**AC-D5.6 discharge:** existing tests on the iterator path
(`internal/resolvers/restactions/api/phase1_itercap_falsifier_test.go`,
`resolve_test.go`, etc.) MUST continue to pass — the default-off
branch is unchanged code. The cyberjoker-with-clusterListWhenAllowed
fallback test (described in §7) gates the deny-and-fallback path.

### §2.7 Shared apistage entry across cluster-list-capable users

**Two distinct cluster-list-permitted identities** (e.g. admin and a
hypothetical second cluster-admin user) requesting the same RA both
arrive at `apistageContentServe` with identical
`contentKeyInputs(gvr, "", "")`. The first call's Put populates the
cache; the second call's Get hits.

`gateContentEnvelope` then runs filterListByRBAC under each requester's
identity in turn — both see the full 49000 items (same RBAC verdict),
but the cache content itself is shared.

**AC-D5.5 discharge:** test
`TestClusterListCacheSharedAcrossClusterListUsers` populates the cache
under admin's identity, then issues a second request under a second
cluster-list-permitted identity, asserts apistage store counter does
NOT advance and that the response is byte-equivalent.

---

## §3. Decision tree for post-D.5 branches

Per the D.4.2 §3 precedent — pre-design fix shapes for the conditional
follow-up ships, dispatched only if D.5's empirical landing leaves
specific gaps.

### Branch B — jq pre-sort (dispatch ONLY if admin cold post-D.5 > 1s)

**Trigger:** D.5 lands HG-1 (admin cold ≤ 3.5s) but HG-1 reading is
> 1s. The diagnosis bench measured ~830ms total jq cost across
piechart (5 expressions, ~600ms) + table (1 expression, ~230ms). If
D.5 collapses everything else and jq dominates, Branch B targets the
jq cost directly.

**Design shape:** move jq filter COMPILATION from per-request to
per-RA-doc parse-time. The compiled query object (`*gojq.Query` from
`jqutil.MaybeQuery`) is hoist-cached on the RestAction's resolved
struct (or on the widget's WidgetDataTemplate resolved form). At
request time, only `gojq.Run` over the compiled query + dataSource
runs — the parse + AST + type-check phase is amortised across all
calls for the lifetime of the resolved RA / widget.

**Additional sub-option (B-sort):** for any stage whose Filter is a
pure sort+slice (recognised pattern: `sort_by(.X) | reverse |
[0:slice]`), pre-sort the apistage content entry at Put-time so the
runtime expression is a no-op slice. New `ResolvedEntry.SortedItems`
or extend `ResolvedEntry.Items` ordering invariant.

**Per-RA opt-in field:** `spec.api[].sortItemsBy: ".metadata.creationTimestamp"`
(additive, default empty → no pre-sort).

**Estimated impact:** ~830ms → ~50ms = ~780ms saved if Branch A's
collapse leaves jq as the dominant residual cost.

### Branch C — refresher TTL pre-expiry (dispatch ONLY if cyberjoker first-cold post-D.5 > 5s)

**Trigger:** D.5 lands but HG-3 reading (cyberjoker first-cold) is
> 5s. D.5 does NOT help cyberjoker (he doesn't get cluster-list);
his first-cold pays apiserver round-trips for TTL-evicted apistage
entries. Branch C closes that gap by refreshing entries before they
expire.

**Design shape:** the L1 refresher
(`internal/cache/refresher.go`) subscribes to each apistage entry's
TTL deadline (or a per-RA `Spec.RefreshInterval`, default
`RESOLVED_CACHE_TTL_SECONDS - 2s`). Before expiry, the refresher
re-runs the SA prewarm for that entry under
`withContentPrewarmSAContext` (`internal/handlers/dispatchers/phase1_content_prewarm.go`
infrastructure). The Put replaces the entry atomically; user requests
during the rebuild see the old entry.

**Estimated impact:** cyberjoker first-cold 26.7s → ~7s (matches
admin's residual fan-out cost; the apistage HITs become reliable). If
D.5 also extends to populating per-NS entries at prewarm under the SA
(via per-NS RA's iterator), cyberjoker drops to ~2.5s.

**Branch C is INDEPENDENT of D.5** (it works whether or not D.5
ships), but PM-gated to dispatch only when D.5's reading shows
cyberjoker still over 5s.

### Branch B and Branch C are NOT in D.5 scope.

D.5 ships standalone. PM amends the design with Branch B or C scope
expansion only after the D.5 ledger row lands and the empirical
reading triggers the respective gate.

---

## §4. Acceptance criteria (PM-binding, 14)

Transcribed from PM gate dispatch 2026-05-21 (AC-D5.1 through
AC-D5.12 verbatim; AC-D5.13 cache-off gate + AC-D5.14 defensive shape
check ADDED post-gate 2026-05-21 per OQ-2 + OQ-5 re-ratification).

**AC-D5.1** — Additive CRD field `spec.api[].clusterListWhenAllowed:
bool` (default `false`, backward-compatible). YAML round-trips
cleanly through existing CRD generator. No removal of existing fields.
Verified by CRD-schema diff pre/post-D.5.

**AC-D5.2** — Cluster-list permission detection **via Ship B typed
RBAC snapshot** (`internal/cache/rbac_snapshot.go`). **NO per-/call
SubjectAccessReview** (would re-introduce apiserver round-trip). Use
`RBACSnapshot.Snapshot().Load()` + existing `evaluateAgainstInformer`-
equivalent to check cluster-scope `list` permission on target GVR.
Correctness bound by Ship B's existing semantic-rules contract
(AC-B.12).

**AC-D5.3** — Cluster-scope cache key disambiguation via existing
`contentKeyInputs` semantics at `apistage.go:61-70`. PM empirically
verified at file:line: `ns=""` distinct from per-namespace strings in
`ResolvedKeyInputs.Namespace` (`resolved.go:126`); `ComputeKey` tuple
hash. Unit test
`TestApistageContentKey_ClusterScopeDistinctFromNamespaced` asserts
hash difference.

**AC-D5.4** — UAF applies POST-cluster-list at existing serve-time
gate `gateContentEnvelope` (`apistage.go:72-74`). Cluster-list cache
content is the full RBAC-allowed cluster view; UAF narrows per-request
at gate. Layering contract per `feedback_restaction_no_widget_logic`
preserved — widget receives same `{list: [...]}` shape. Test:
cluster-list+UAF byte-identical widget props to namespaced-iterator+UAF
on same identity.

**AC-D5.5** — Cluster-list entry **SHARED across cluster-list-capable
users** (apistage class `CacheEntryClassApistage` at `resolved.go:79`
is identity-free). Two cluster-list-permitted identities request same
RA → second request hits L1 (NOT a second apiserver dispatch). Test
verifies.

**AC-D5.6** — Fallback semantic correctness: cyberjoker (no
cluster-list grant) takes existing namespaced iterator path unchanged.
Default-false field = existing path verbatim. Every existing
`restactions/api` test on the iterator path MUST still pass. Test:
cyberjoker-identity with `clusterListWhenAllowed: true` RA + no
cluster-list grant → falls through to namespaced iterator →
byte-identical output to pre-D.5.

**AC-D5.7** — VERBATIM `kubectl get --raw` fixtures for BOTH paths per
`feedback_empirical_apiserver_probe_for_predicate_design`:
- `testdata/cluster_scope_compositions_list.json` (cluster-scope GET)
- `testdata/namespaced_compositions_list.json` (one namespace's worth)

**AC-D5.8** — Byte-equivalence test: cluster-list aggregated result
identical to per-namespace aggregated result post-jq. Partition the
cluster-scope fixture by namespace into 80 mock per-NS responses,
simulate iterator path, assert both produce identical `{list: [...]}`
shape.

**AC-D5.9** — Two new `FallthroughReason` constants in
`fallthrough_meter.go`. Closed-enum 18 → 20:
- `ReasonClusterListDispatch` — counter fires when cluster-list path
  is selected (NOT a fallthrough; diagnostic for "how often is
  cluster-list activating").
- `ReasonClusterListShapeFallback` — counter fires when the
  defensive multi-element GVR / shape check (AC-D5.14) rejects the
  cluster-scope response and the dispatcher falls back to the per-NS
  iterator.

Per-stage label via `opts.key` per AC-D4.1.11 precedent. Cardinality
20 × 10 × 50 ≈ 10,000 series, within Prometheus comfort.

**AC-D5.10** — Tag `0.30.152` + commit type `feat(cache):`. Subject:
`feat(cache): cluster-list-when-allowed iterator collapse (Ship D.5,
0.30.152)`. Body cites diagnosis doc + HG targets + 5 closed OQs +
file:line cache-key disambiguation evidence (runtime artifact per
`feedback_empirical_root_cause_trace_before_fix` tightening).

**AC-D5.11** — Pre-flight baseline at `/tmp/snowplow-runs/0.30.152/before/`
+ **empirical per-entry capacity measurement** per
`feedback_capacity_caps_empirical_per_entry_cost`:
- Admin cold Dashboard reference (~8.16s)
- Cyberjoker first-cold reference (~26.72s)
- 4 named canary SHA256s (carry from `/tmp/snowplow-runs/0.30.145/before/`)
- 12-item content corpus with **clean-wire-shape audit** per
  `feedback_byte_identical_baselines_clean_wire_shape` (scan for
  `Authorization: Bearer`, `eyJ`-prefix JWTs, `userAccessFilter` BEFORE
  SHA256-locking)
- **Empirical cluster-scope entry resident size**:
  `kubectl get --raw .../compositions | wc -c` byte-shape + sum of 49
  per-NS `wc -c` byte-shapes — both captured under
  `/tmp/snowplow-runs/0.30.152/before/`.

**Directional capacity check (PM-ratified 2026-05-21).** The prior
±50 MiB band is structurally wrong (the measured -60 to -300 MiB
resident-delta range's lower bound sits BELOW the band's lower bound,
so a ±50 MiB band would HARD-REVERT a correct ship). Converted to a
directional invariant per
`feedback_capacity_caps_empirical_per_entry_cost`:

> **memory delta is non-positive AND cluster-list entry RSS
> contribution ≤ sum of per-NS entry RSS contributions it replaces.**

Memory delta direction is **informational, NOT a HARD REVERT
trigger.** HG-5 (CACHE_ENABLED=false → admin returns to ~8s baseline)
remains the load-bearing toggle invariant and the only
memory-related HARD REVERT signal in the gate set.

**AC-D5.12** — Pre-commit dev review by architect + PM before commit /
tag / push.

**AC-D5.13** — **Cluster-list dispatch gated by `!cache.Disabled()`**
at the resolver entry. With `CACHE_ENABLED=false`, the code path falls
through to the existing per-NS iterator UNCHANGED — no cluster-scope
GET is dispatched in cache-off mode.

This gate is **structurally required**, not an optimization, per
`project_caching_is_provisional` ("cache is removable"): HG-5 requires
CACHE_ENABLED=false to fall back to the existing 80-NS iterator (the
pre-D.5 cache-off behavior). Without the gate, cache-off would still
dispatch the cluster-scope GET as a one-shot, creating a behavior
that exists ONLY in cache-off-with-D.5 mode — which violates the
removable-cache invariant (cache layers cannot leave residual
behavioral changes when disabled).

The gating site (resolver entry, immediately before `useClusterList`
is computed) is also the natural place to additionally guard against
pre-readiness window misbehaviour by checking the Ship B
`atomic.Pointer[RBACSnapshot]` is published — see
`internal/cache/rbac_snapshot.go:687-700`
(`waitAndPublishInitialRBACSnapshot` / "Servable" signal). Before
that goroutine completes, `rbacSnap.Load()` returns nil and
`EvaluateRBAC` degrades to deny (AC-B.8); the cluster-list collapse
must NOT execute against a nil snapshot. Implementation: bail to the
iterator fallback when `cache.Global().Snapshot().Load() == nil`.

**AC-D5.14** — **Defensive multi-element shape check on cluster-scope
LIST response.** PM ratified CONDITIONALLY on ≤10ms overhead at dev
measurement. Structural definition:

> Verify response is a **list envelope** (kind ends in `"List"`),
> `.items` is a **non-empty array of objects**, and each item has
> non-nil `apiVersion` and `kind` strings.

**On any failure**, fall back to the existing per-NS iterator —
NOT a 500; NOT a cached malformed entry. Surface the fallback via:

```go
cache.RecordApiserverFallthrough(ctx, cache.ReasonClusterListShapeFallback, gvr.String())
```

Dev measures the shape-check overhead during implementation. **If
overhead >10ms, surface at dev-review for PM re-ratification** (the
defensive check budget is conditional, not unconditional). PM noted.

This is the second new `FallthroughReason` constant in AC-D5.9's
closed-enum expansion (18 → 20).

---

## §5. Hard gates (PM-binding, 6)

Transcribed verbatim from PM gate dispatch 2026-05-21.

**Methodology anchor (ratified 2026-05-21).** HG-1 (admin cold ≤ 3.5s)
and HG-3 (cyberjoker first-cold ±10% of 26.72s) are anchored to
**SESSION-COLD methodology**: a warm snowplow pod with cleared browser
localStorage, measuring the user-facing first-paint timeline as the
browser session opens. They are **NOT** pod-cold measurements
(snowplow pod freshly restarted with empty L1). Pod-cold is a separate
operational concern tracked under #157-lineage and is out of D.5
scope. HG-5's cache-toggle invariant is also session-cold (pod is up,
`CACHE_ENABLED=false` is set via chart values, browser session is
fresh).

| Gate | Target | HARD REVERT trigger |
|---|---|---|
| HG-1 admin cold Dashboard | ≤ 3.5s | > 4.0s |
| HG-2 ONE cluster-scope GET per admin cold | Exactly 1, verified via apistage-content-hit log with `ns=""` | > 1 (gate didn't trigger cleanly) |
| HG-3 cyberjoker no-regression directional | post-D.5 cyberjoker cold median ≤ 1.25× TODAY's pre-D.5 baseline AND post-D.5 cyberjoker warm median ≤ 1.25× TODAY's pre-D.5 warm baseline (pre-D.5 baseline: 1,457ms cold median, 1,364ms warm median, captured 2026-05-21 n=3 on pod z6dx9 under session-cold-warm-pod methodology, persisted at /tmp/snowplow-runs/0.30.152/before/measurements.json) | post-D.5 cold median > 1,821ms (1.25 × 1,457) OR post-D.5 warm median > 1,705ms (1.25 × 1,364) |
| HG-4 named canaries (`nav-admin`, `nav-cj`, `rl-admin`, `rl-cj`) | byte-identical OUTSIDE 11+/12 budget | Any diff |
| HG-5 cache-toggle invariant (load-bearing memory-related HARD REVERT signal per `feedback_capacity_caps_empirical_per_entry_cost`) | `CACHE_ENABLED=false` → admin Dashboard ~8s, ±15% of pre-D.5; cluster-list dispatch path NOT taken (AC-D5.13 gate) | Outside band, OR cluster-scope GET observed under cache-off |
| HG-6 RBAC narrowing intact | cyberjoker requests show NO `ns=""` apistage entries (cluster-list path NOT triggered for him) | Any `ns=""` apistage entry created on cyberjoker's behalf |

**Gate measurement protocol:**

- **HG-1:** Chrome MCP page-load measurement, n=6 admin cold runs
  (session-cold-warm-pod: warm pod, fresh Chrome session with
  localStorage.clear + sessionStorage.clear, login, navigate to
  /dashboard). Median admin cold across n=6.
- **HG-3:** Chrome MCP page-load measurement, n=3 cyberjoker cold runs
  AND n=3 cyberjoker warm runs on the SAME pod as HG-1's measurement
  (pod must NOT be restarted between HG-1 and HG-3 captures —
  informer-warmup state must match the pre-D.5 baseline at
  /tmp/snowplow-runs/0.30.152/before/). Compute medians; compare to
  pre-D.5 1.25× bands above.
- **HG-2:** `kubectl logs $POD --tail=99999 | grep 'apistage.content_hit.*"ns":""'` count = exactly 1 per admin cold Dashboard request.
- **HG-4:** `sha256sum` against the 4 named canaries from
  `/tmp/snowplow-runs/0.30.152/before/` corpus. The 11+/12 budget is
  the existing tester acceptance window.
- **HG-5:** redeploy with `helm upgrade --set
  snowplow.env.CACHE_ENABLED=false` (chart-only path per
  `feedback_chart_only_for_snowplow`), re-run admin cold. Expect
  ~8s ± 15%.
- **HG-6:** grep apistage cache state via `/debug/vars` on the live
  pod after cyberjoker cold runs — must see ZERO `ns:""` entries
  attributed to cyberjoker dispatch.

**Any gate failure = HARD REVERT** (helm rev backward). Per the
post-D.2 / D.4 / D.4.1 revert template, the COMMIT may stay on branch;
the IMAGE is reverted at helm level. No partial roll-forward.

---

## §6. Cross-ship invariants preserved

(a) **Per-user L1 invariant** (`feedback_l1_per_user_keyed_never_cohort`):
the cluster-list apistage entry is identity-free by Ship F1's
construction (`resolved.go:341-353` skips Username/Groups hashing for
apistage class). UAF + filterListByRBAC at the serve-time gate
(`apistage.go:94-145`) deliver the per-user narrowing. **No
cross-user leak** — the gate runs on every Get-hit; an unauthorized
user falls through to apiserver (whose per-user token narrows
correctly).

HG-3's no-regression band (PM-ratified 2026-05-21) replaces the
prior match-the-anchor framing because D.5's mechanism is
structurally inaccessible to cyberjoker: cluster-list dispatch
requires cluster-scope `list` permission (evaluate.go:198-211);
cyberjoker is narrow-RBAC by definition; useClusterList is always
false on his path; the existing 80-NS iterator runs verbatim. HG-3
becomes a sanity check that the default-off fallback path is
unchanged, not an improvement target.

(b) **Layering contract** (`feedback_restaction_no_widget_logic`):
the RA's `Filter` stage receives the SAME `{list: [items]}` shape
whether items came from cluster-list dispatch or iterator
aggregation. The widget's `widgetDataTemplate` jq expressions are
unchanged. Output canonicalisation at widget props is preserved.

(c) **No special cases** (`feedback_no_special_cases`):
`clusterListWhenAllowed` is an opt-in additive field on the RA spec;
snowplow Go contains NO per-resource literal (no
`if gvr.Resource == "githubscaffoldingwithcompositionpages" {...}`).
The dispatch decision is purely derived from `(RA.spec opt-in) AND
(RBAC permits cluster-list)`.

(d) **Cache removable** (`project_caching_is_provisional`,
`project_redis_removal`) — PM-ratified 2026-05-21, see OQ-2 +
AC-D5.13: the `useClusterList` decision is **STRUCTURALLY GATED** on
`!cache.Disabled()` at the resolver entry. With
`CACHE_ENABLED=false`, the cluster-list collapse is disabled
entirely; dispatch falls through to the existing per-NS iterator
**UNCHANGED**. This preserves the removable-cache invariant: when the
cache is "removed" (CACHE_ENABLED=false), no D.5-specific behavioral
residue remains in the dispatch path — the pre-D.5 iterator path is
restored verbatim. HG-5 verifies: cache-off admin Dashboard returns
to the ~8s pre-D.5 baseline (±15%). The gate is additionally
combined with a Ship B snapshot-readiness check
(`rbacSnap.Load() != nil`) at the resolver entry — see AC-D5.13.

(e) **Falsifier-first** (`feedback_falsifier_first_before_ship`):
§7's test plan captures the pre-D.5 baseline runtime artifact at
`/tmp/snowplow-runs/0.30.152/before/` BEFORE coding. The ledger row
binds gate measurements to those artifacts.

(f) **Architect-design rigor** (`feedback_architect_design_rigor`):
every code claim in §2 cites a file:line. INFERRED vs TRACED labels
applied where appropriate (§2.3 has TWO TRACED file:line refs for the
existing iterator/dispatch site + ONE INFERRED for the not-yet-
implemented `useClusterList` branch — that branch is the design's new
code).

---

## §7. Test plan

### §7.1 Pre-flight baseline (AC-D5.11)

```
mkdir -p /tmp/snowplow-runs/0.30.152/before
# Admin cold Dashboard reference
... (n=6 runs, median, captured via Chrome MCP)
# Cyberjoker first-cold reference (pod-kill between runs)
... (n=3 runs since cyberjoker first-cold is expensive)
# 4 named canary SHA256s — re-fetch from 0.30.145 corpus and re-verify
sha256sum /tmp/snowplow-runs/0.30.145/before/canaries/{nav-admin,nav-cj,rl-admin,rl-cj}.json > /tmp/snowplow-runs/0.30.152/before/canaries.sha256
# 12-item content corpus
... clean-wire-shape audit script + capture
# Cluster-scope entry size
kubectl get --raw "/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages" | wc -c > /tmp/snowplow-runs/0.30.152/before/cluster_scope_byte_size.txt
# Per-NS entry sample size (one namespace)
kubectl get --raw "/apis/composition.krateo.io/v1-2-2/namespaces/bench-ns-01/githubscaffoldingwithcompositionpages" | wc -c > /tmp/snowplow-runs/0.30.152/before/per_ns_byte_size.txt
```

### §7.2 Unit tests

**TestApistageContentKey_ClusterScopeDistinctFromNamespaced (AC-D5.3):**

```go
nsKey := cache.ComputeKey(cache.ResolvedKeyInputs{
    CacheEntryClass: cache.CacheEntryClassApistage,
    Group: "composition.krateo.io", Version: "v1-2-2",
    Resource: "githubscaffoldingwithcompositionpages",
    Namespace: "bench-ns-01", Name: "",
})
clusterKey := cache.ComputeKey(cache.ResolvedKeyInputs{
    CacheEntryClass: cache.CacheEntryClassApistage,
    Group: "composition.krateo.io", Version: "v1-2-2",
    Resource: "githubscaffoldingwithcompositionpages",
    Namespace: "", Name: "",
})
if nsKey == clusterKey {
    t.Fatalf("cluster-scope key MUST differ from namespaced key: ns=%q cluster=%q", nsKey, clusterKey)
}
```

**TestClusterListPathCollapse_AdminPermitted (AC-D5.2 + AC-D5.8):**

Synthetic admin-identity context with a cluster-list-permitting RBAC
snapshot, RA with `ClusterListWhenAllowed: true` + iterator,
80-namespace fixture pre-loaded into apistage cache (per-NS + a
cluster-scope envelope from the partition). Dispatch:
- Assert `tmp` has length 1 (single call) — not 80.
- Assert the call's path is `/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages` (no `/namespaces/...`).
- Assert apistage Get on `(gvr, "", "")` is called exactly once.
- Assert filterListByRBAC runs ONCE over 49000 items.
- Assert `ReasonClusterListDispatch` counter advances by 1.

**TestClusterListPathCollapse_CyberjokerDenied (AC-D5.6):**

Synthetic cyberjoker-identity context with no cluster-list grant in
the snapshot. Same RA. Dispatch:
- Assert `useClusterList == false` at the decision point.
- Assert `tmp` has length 80 (iterator expansion intact).
- Assert NO apistage entry with `ns=""` is created on cyberjoker's
  behalf.
- Assert output is byte-identical to the same dispatch with
  `ClusterListWhenAllowed: false` (pre-D.5 path).

**TestClusterListSharedAcrossClusterListUsers (AC-D5.5):**

Two admin-equivalent identities (admin1, admin2) both holding
cluster-list. First request populates the cache; second request:
- Assert apistage `Get(clusterKey)` returns hit.
- Assert `apistageStoreTotal` counter does NOT advance.
- Assert byte-equivalent response.

**TestClusterListByteEquivalenceVsIteratorAggregation (AC-D5.8):**

Load the cluster-scope fixture `testdata/cluster_scope_compositions_list.json`
(verbatim apiserver output). Partition into 80 mock per-NS responses
by `metadata.namespace`. Run TWO synthetic dispatches:
- (a) cluster-list path with full envelope cached.
- (b) iterator path with 80 per-NS envelopes cached.

Assert post-RA-filter `{list: [items]}` shape is byte-identical
(items may differ in order; canonicalise by `metadata.uid` sort
before compare).

**TestClusterListUAFNarrowingPreserved (AC-D5.4):**

Admin identity + RA with `ClusterListWhenAllowed: true` + a stage
having UAF. Dispatch and assert:
- Cluster-list cache populated (un-narrowed full envelope).
- gateContentEnvelope runs filterListByRBAC.
- UAF refilter runs downstream.
- Final widget props byte-identical to namespaced-iterator+UAF with
  same identity + RBAC.

**TestClusterListCacheOffFallthrough (HG-5, cross-ship invariant d):**

`cache.Disabled() == true`. RA with `ClusterListWhenAllowed: true`.
PM decision pending on whether collapse runs under cache=off; the
recommended default is: collapse disabled under cache=off (gated on
`!cache.Disabled()`). Test asserts the recommended branch: 80 calls,
no cluster-list dispatch.

### §7.3 Integration tests

Existing `restactions/api` integration tests on the iterator path MUST
continue to pass without modification (AC-D5.6 byte-identical fallback).

### §7.4 Post-deploy validation (HG-1 through HG-6)

Captured in §5 protocol. Tester runs the 6-gate validation against
`0.30.152` deploy, persists artifacts under
`/tmp/snowplow-runs/0.30.152/after/`.

---

## §8. Risks and open questions flagged post-PM gate

### Risk-1: cluster-scope LIST envelope size on apistage L1

**Empirical capacity projection (PM-ratified 2026-05-21).** Per
`feedback_capacity_caps_empirical_per_entry_cost`, the design's
prior -20 MB design-time estimate is replaced by the tester's
empirical wire-byte measurement.

**Tester measurement source:**
`/tmp/snowplow-runs/0.30.152/before/` — `cluster_scope_byte_size.txt`
+ `per_ns_byte_size.txt` (1 cluster-scope LIST byte size vs sum of 49
per-NS LIST byte sizes for the same RA).

- **Wire delta (measured):** **-119 MiB** — the cluster-scope LIST
  response is 119 MiB smaller on the wire than the arithmetic sum of
  49 per-NS responses combined (the per-NS envelopes carry duplicated
  `apiVersion`, `kind`, list `metadata`, and resource-version headers
  49 times; the cluster-scope LIST emits these once).
- **Resident delta (estimated):** **-60 to -300 MiB** — Go runtime
  typically holds 2-5× the wire-byte cost in resident memory (parsed
  unstructured trees + Items slice + map literals + header overhead).
  Applying the 2-5× multiplier to the -119 MiB wire delta yields a
  -60 MiB (low end) to -300 MiB (high end) RSS reduction band.
- **Direction:** unambiguously **non-positive** (memory pressure goes
  DOWN, never UP, regardless of which point in the 2-5× band lands).

The cluster-list entry replaces 49 per-NS entries; the per-RA
per-entry envelope-overhead duplication is the dominant savings
component (Items slice content is conserved across the partition).

The prior design-time -20 MB estimate is **withdrawn** as
estimation-only and superseded by the tester's measurement. AC-D5.11
+ AC-D5.14 (revised below) now bind the empirical numbers.

### Risk-2: ParseAPIServerPathToDep returns ns="" for BOTH the cluster-scope path AND a malformed path

`internal/cache/inventory.go:251-330` returns `(gvr, "", "", true)`
for `/apis/<g>/<v>/<resource>` (cluster-scope) and would ALSO return
the same shape for a malformed path that happens to match. **Mitigation:**
the path is constructed by D.5's own code from a validated GVR (from
`deriveTargetGVRForClusterList`); it is never user-supplied. The
existing `inventory.go` validation (`${...}` rejection at :255,
prefix check at :260) covers external-path malformedness.

### Risk-3: filterListByRBAC memo cost under cluster-scope item set

filterListByRBAC at `informer_dispatch_rbac.go:93-180` memoizes per
namespace. A cluster-scope envelope spans ALL namespaces, so the memo
size = N_distinct_namespaces (~80 in the bench). Each memo entry is
~50 bytes (map[string]bool). Memo footprint: ~4 KB. **Negligible.**

The 80 EvaluateRBAC calls under the memo are identical to the 80
EvaluateRBAC calls the iterator path makes (one per per-NS LIST). NO
RBAC-eval cost increase. The collapse is purely in the
ORCHESTRATION cost (Go scheduler, errgroup, dispatch dictMu).

### OQ-1 (closed): Should ClusterListWhenAllowed default to true for backward compat?

**Answer: NO.** Default false. Pre-D.5 RAs are byte-identical to
their existing behaviour. Setting it true requires the RA author to
verify the 3 contract conditions in §2.1's docblock (namespace-scoped
GVR, equivalent object set, widget tolerates per-object narrowing).
**TRACED at:** §2.1 docblock, AC-D5.1 default-false requirement.

### OQ-2 (closed, PM-ratified 2026-05-21): When cache=off, does cluster-list still collapse?

**Answer: NO — STRUCTURALLY REQUIRED gate, not a default.** The
`useClusterList` decision **MUST** be gated on `!cache.Disabled()` at
the resolver entry. Reasoning (per
`project_caching_is_provisional`'s "cache is removable" invariant):
HG-5 requires CACHE_ENABLED=false to fall back to the existing 80-NS
iterator (the pre-D.5 cache-off behavior). Without the gate, cache-off
would still dispatch the cluster-scope GET as a one-shot, creating a
behavior that exists ONLY in cache-off-with-D.5 mode — a residual
behavioral footprint when the cache is "removed," which violates the
removability invariant.

The gate site is also the natural place to additionally check Ship B
`RBACSnapshot` readiness (`rbacSnap.Load() != nil`) at
`internal/cache/rbac_snapshot.go:687-700` — see AC-D5.13 for the
combined gate.

**Discharged by:** AC-D5.13 (new), §6 invariant (d), §7.2
TestClusterListCacheOffFallthrough.
**TRACED at:** `internal/cache/rbac_snapshot.go:687-700` ("Servable"
signal).

### OQ-3 (closed): What is the cardinality of the new ReasonClusterListDispatch cell?

**Answer:** AC-D5.9 documents 19 × 10 × 50 ≈ 9,500 series. The
per-stage label is bounded by the set of RAs with
`ClusterListWhenAllowed: true` — currently 1 (`compositions-list`).
Cardinality is well below Prometheus cardinality-explosion concern.
**TRACED at:** AC-D5.9, the Ship D.4.1 precedent for closed-enum
cardinality counting at `fallthrough_meter.go`.

### OQ-6 (closed, PM-ratified 2026-05-21): Why is HG-3 a no-regression band, not a match-the-anchor band?

**Answer:** D.5's `useClusterList` decision is structurally gated on
cluster-scope `list` permission (§2.2, evaluate.go:198-211).
Cyberjoker is narrow-RBAC; the cluster-list path never fires for him;
his dispatch runs through the unchanged §2.6 fallback. Yesterday's
26,722ms cyberjoker anchor was captured against a likely
cold-typed-RBAC-informer pod and is not methodologically comparable
to a post-D.5 warm-informer measurement (tester n=3 2026-05-21:
[6197, 1301, 1457] cold runs show the canonical warmup-then-steady
signature; runs 2-3 are 18× faster than the anchor). Match-the-anchor
gating would HARD-REVERT a correct ship on informer-warmup variance
alone. HG-3 reframed as +25% no-regression directional check against
TODAY's session-cold-warm-pod baseline (1,457ms cold / 1,364ms warm
medians) tied to the same pod (z6dx9) the HG-1 measurement runs on.
The improvement signal vs yesterday's anchor is acknowledged as a
side-finding consistent with project_l1_stale_gap_finding_2026_05_14's
typed-RBAC informer-sync-lag observation; pursued separately under
Branch C if HG-3 still surfaces a real cyberjoker-side concern post-D.5.

**TRACED at:** internal/rbac/evaluate.go:213-217 (the executable
`for _, crb := range snap.ClusterRoleBindings` loop that gates
cluster-scope permits; documented at evaluate.go:188-210 function
header block); §2.6 default-off fallback semantics; measurements.json
at /tmp/snowplow-runs/0.30.152/before/.

### OQ-4 (closed): Does cluster-scope LIST hit a different apiserver code path with different latency characteristics?

**Answer:** YES — apiserver's cluster-scope namespace-scoped-resource
LIST is a well-trodden code path (every `kubectl get pods -A` hits
it). It has identical RBAC-evaluation cost as the namespaced LIST,
and the response shape is the union of all namespaces. **Empirical
verification at AC-D5.7's fixture.**
**TRACED at:** `inventory.go:184-193` (path parsing supports it),
K8s apiserver REST routing.

### OQ-5 (closed, PM-ratified CONDITIONALLY 2026-05-21): What if the iterator's Path template references additional dataSource fields beyond `.version`/`.namespace`/`.plural`?

**Answer:** `deriveTargetGVRForClusterList` runs ONE jq evaluation
against the FIRST iterator element — extracting the GVR. If the path
references additional iteration-specific fields, the GVR is still
derivable (the GVR shape is fixed across iterations; only `namespace`
varies, which we strip). **TRACED at:** §2.3 Approach (i) derivation
recipe.

If the path is dynamically constructed in a way that produces
DIFFERENT GVRs per iteration (e.g.
`${ "/apis/" + .group + "/" + .version + "/" + .resource }`), then
`deriveTargetGVRForClusterList` would observe varying GVRs across the
first few iterations and return `(gvr, false)` → falls through to the
iterator path.

**Defensive multi-element GVR check — IN-SCOPE for D.5 per PM
ratification.** Structurally specified at AC-D5.14:

> Verify response is a list envelope (`kind` ends in `"List"`),
> `.items` is a non-empty array of objects, each item has non-nil
> `apiVersion` and `kind` strings.

On any failure → fall back to the existing per-NS iterator (NOT a
500; NOT a cached malformed entry), surfaced via the new
`cache.ReasonClusterListShapeFallback` fallthrough counter
(AC-D5.9 closed-enum 18 → 20).

**Conditional ratification:** PM ratified the defensive check
CONDITIONAL on ≤10ms overhead. Dev measures during implementation;
if overhead >10ms, surface at dev-review for PM re-ratification.

**Discharged by:** AC-D5.14 (new), AC-D5.9 (revised cardinality),
§2.3 derivation recipe.

---

## §9. Tag, commit, ledger

**Tag:** `0.30.152`.
**Commit subject:** `feat(cache): cluster-list-when-allowed iterator collapse (Ship D.5, 0.30.152)`.
**Body should cite:**
- `docs/issue-213-admin-dashboard-9s-rediagnosis-0.30.151.md` for the runtime-artifact root cause
- HG-1 through HG-6 with their gate targets
- All 5 OQs as closed with file:line evidence per §8
- `internal/cache/resolved.go:341-353` for the apistage identity-free key (AC-D5.3 evidence)
- `internal/rbac/evaluate.go:198-211` for the cluster-scope ClusterRoleBindings check (AC-D5.2 evidence)
- `internal/resolvers/restactions/api/apistage.go:94-145` for the serve-time gate preservation (AC-D5.4 evidence)

**Ledger row:** appended to `project_north_star_ledger.md` post-deploy
with the n=6 admin cold median + cyberjoker first-cold reading +
cache-toggle invariant + canary-diff count.

**Feature journal:** appended to `project_feature_journal.md` with
expected (admin ≤ 3.5s) vs actual (measured) + delta.

---

## §10. Architect summary one-liner

D.5 collapses the 80-namespace iterator's ~7s in-process
orchestration cost to a single cluster-scoped LIST (1 call, 1
filterListByRBAC pass), backed by the existing apistage L1 +
RBAC-snapshot path with ZERO new RBAC mechanisms. Default-off,
permission-gated, byte-identical fallback. Projected admin cold
8.16s → ≤ 3.5s.
