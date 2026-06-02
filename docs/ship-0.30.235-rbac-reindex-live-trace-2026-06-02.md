# Ship 0.30.235 — UAF refilter sees post-filter-projected items (LIVE TRACE)

**Author:** cache-architect
**Date:** 2026-06-02
**Live reproduction:** GKE cluster `gke_neon-481711_us-central1-a_cluster-1`, snowplow pod `snowplow-6c6c9f99fb-p5lbc` (image 0.30.233, helm rev 406)
**Brief:** team-lead dispatch 2026-06-02, post phase6 PID 18860 ConvergenceTimeout

## TL;DR

The defect is **NOT** in the RBAC indexer or BindingsByGVR. It is in the **UAF refilter ↔ stage filter layering** in the resolver. EvaluateRBAC returns the WRONG answer for cj because the refilter feeds it a NamespaceFrom-expression that evaluates to `""` (empty namespace == cluster-scope), and cj has no cluster-scope grant. The apiserver SSAR returns YES because it sees the namespace; snowplow's evaluator never receives the namespace.

H1, H2, H3, H4, H5, H6 are all FALSIFIED. The actual root cause is hypothesis H_new below, traced empirically with file:line.

## Empirical traces

### Live cluster probes (READ-ONLY)

```
$ kubectl auth can-i list githubscaffoldingwithcompositionpages.composition.krateo.io --as=cyberjoker --as-group=devs -n bench-ns-01
yes

$ kubectl get rb cyberjoker-all-reader -n bench-ns-01 -o yaml
... subjects: [{kind: User, name: cyberjoker, apiGroup: rbac.authorization.k8s.io}]
... roleRef: {kind: Role, name: cyberjoker-all-reader}

$ kubectl get role cyberjoker-all-reader -n bench-ns-01 -o yaml
... rules: [{apiGroups: [composition.krateo.io], resources: ["*"], verbs: [get, list, watch]}]
```

### Snowplow /debug/vars

```
snowplow_plurals_registered_gvrs: { "rbac.authorization.k8s.io/v1/rolebindings", ... 38 GVRs }
→ H6 FALSIFIED. RoleBindings ARE registered.

snowplow_cohort_memo_entries_total: 54-58 (growing)
snowplow_bindings_by_gvr_delta_skipped_non_typed: 0
→ H1 FALSIFIED. The BindingsByGVR delta hook IS firing.
```

### Snowplow log evidence (per-/call UAF emit)

For cj's piechart serve:
```
"msg":"userAccessFilter","user":"cyberjoker","api":"allCompositions",
"verb":"list","group":"composition.krateo.io","resource":"",
"refilter_kept":0,"refilter_dropped":1,"evaluate_rbac_calls":1
```

For admin's piechart serve:
```
"msg":"userAccessFilter","user":"admin","api":"allCompositions",
"verb":"list","group":"composition.krateo.io","resource":"",
"refilter_kept":20,"refilter_dropped":0,"evaluate_rbac_calls":20
```

Cj's path: 1 item enters refilter, 1 EvaluateRBAC call, 1 dropped → 0 kept.
Admin's path: 20 items enter refilter, 20 EvaluateRBAC calls, 0 dropped → 20 kept.

### Apiserver SSAR (oracle)

```
$ cat | kubectl create -f - --dry-run=server (cyberjoker, list, composition.krateo.io, ns=bench-ns-01, resource="")
allowed: true
reason: 'RBAC: allowed by RoleBinding "cyberjoker-all-reader/bench-ns-01" of Role "cyberjoker-all-reader" to User "cyberjoker"'

$ cat | kubectl create -f - --dry-run=server (cyberjoker, list, composition.krateo.io, ns="", resource="")
allowed: false
```

The apiserver permits cj IFF namespace=bench-ns-01. Cluster-scope (namespace="") denies cj. **So if snowplow's evaluator is called with `namespace=""`, it correctly denies. The defect is that snowplow IS calling with `namespace=""` when it should be calling with `namespace="bench-ns-01"`.**

## Hypothesis H_new — UAF refilter sees post-filter-projected items

### Reproduction sequence (RESTAction `compositions-list`)

Stage spec (verified on live cluster, `/tmp/cj-compositions-list.json`):

```json
{
  "name": "allCompositions",
  "path": "${ \"/apis/composition.krateo.io/\" + (.version) + \"/\" + (.plural) }",
  "filter": "[.allCompositions.items[]? | {uid: .metadata.uid, name: .metadata.name, ns: .metadata.namespace, ts: ..., kind: ..., av: ..., conditions: (.status.conditions // [])}]",
  "userAccessFilter": {
    "verb": "list",
    "group": "composition.krateo.io",
    "namespaceFrom": ".metadata.namespace"
  }
}
```

### Resolver path (file:line trace)

1. Worker dispatches SA-cluster-scope LIST: `/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages` returns ALL 20 compositions (envelope shape: `{items: [{kind, apiVersion, metadata: {namespace, name, uid, ...}, status, ...}, ...]}`).

2. `apistageContentServe` (resolve.go:688) hits the apistage L1 cell. `gateContentEnvelope` → `gateListItemsWithMemo` (apistage_cohort_memo.go:141) → `populateCohortGateMemo` → `CohortNSACL(snap, "cyberjoker", ["devs"], gvr)` (cohort_ns_acl.go:106) correctly finds the new RB in `snap.RBsByUserByNS["bench-ns-01"]["cyberjoker"]` and returns `permittedNS = {bench-ns-01, demo-system, krateo-system}` (the devs-group landings in demo-system + krateo-system don't intersect bench namespaces; the cj-User-kind landing in bench-ns-01 does). keptNames = `{"bench-ns-01/bench-app-01-01"}` (1 item). **CORRECT.**

3. `jsonHandlerCore` (handler.go:83) decodes the narrowed envelope (1 item with FULL `.metadata`), then applies `opts.filter = apiCall.Filter` at handler.go:100:
   ```go
   v, ok, err := EvalValue(context.TODO(), q, pig, jqsupport.ModuleLoader())
   ```
   The filter `[.allCompositions.items[]? | {uid, name, ns: .metadata.namespace, ...}]` projects each item to a flat map **without** `.metadata`. Result: `dict["allCompositions"] = [{uid, name, ns: "bench-ns-01", ts, kind, av, conditions}]` (1 item, NO `.metadata`).

4. `applyUserAccessFilter` (refilter.go:71) runs over `dict["allCompositions"]` (resolve.go:886). At refilter.go:192 `refilterSlice` iterates the 1 item. `evalSingle` (refilter.go:226) computes namespace via the UAF expression:
   ```go
   nsExpr := uaf.NamespaceFrom // ".metadata.namespace"
   ns, err := evalJQString(ctx, nsExpr, item)
   ```
   On the projected item (no `.metadata` field), `jq` evaluates `.metadata.namespace` → `null`. `evalJQString` (refilter.go:365) returns `("", nil)` for the nil branch (refilter.go:383). **namespace = "".**

5. EvaluateRBAC is called with `(verb=list, group=composition.krateo.io, resource="", namespace="")` at refilter.go:255. This is a **cluster-scope** check. For cj, no CRB matches (cj has no User-kind CRB; cj's "devs" group's CRBs are for CRDs/namespaces resources, not compositions). `selectRBCandidates(snap, "", opts)` returns nil at evaluate.go:357 (`ns == ""` guard); `selectCRBCandidates` runs but no CRB grants `list composition.krateo.io`. Result: **deny**.

6. Refilter drops the 1 item → `refilter_kept=0, refilter_dropped=1`. dict["allCompositions"] is emptied. The outer RESTAction filter `{list: (.allCompositions // [])}` yields `status.list = []`. Piechart's widgetDataTemplate computes `length=0` → title="0".

### Why admin sees 20 despite the SAME projection bug

Admin's path: step 2 returns `permitAll=true` (admin has `cluster-admin` ClusterRoleBinding). `cohortGateMemoServe` returns ALL 20 items. Step 3 projects to 20 items (no `.metadata`). Step 4: per-item `evalSingle` with `namespace=""` (same projection bug) → cluster-scope EvaluateRBAC. Admin's `cluster-admin-binding-krateo-system` CRB grants `*/*` cluster-wide → **permit**. 20 items kept.

So the projection bug is **always present**, but its symptom is masked for cluster-wide-admin identities and exposed only for namespace-scoped identities (cj after the bench's per-namespace RB add).

### Why this is hidden until the bench's S2-S4 flow

- Pre-bench-RB-add: cj has no User-kind binding; CohortNSACL's permittedNS = {demo-system, krateo-system} (devs-group landings). 0 compositions live in those namespaces. keptNames empty. cohortGateMemoServe returns 0 items. Refilter receives 0 items, makes 0 EvaluateRBAC calls, drops 0. api=0 — but indistinguishable from "RBAC working correctly with no access".
- Post-bench-RB-add: cj gets the User-kind RB in bench-ns-01. CohortNSACL adds bench-ns-01 to permittedNS. keptNames = `{bench-ns-01/bench-app-01-01}` (1 item). cohortGateMemoServe returns 1 item. Refilter receives 1 item. THIS is where the projection bug manifests — UAF's namespaceFrom can't read it, cluster-scope deny, drops to 0.

The bench's S2-S4 flow is the FIRST scenario in the whole codebase where a namespace-scoped user has access to a LIST that previously was empty for that user. Until this commit, the projection bug had zero observable consequence because narrow-RBAC users never had any items to narrow.

## Why earlier hypotheses are falsified

| Hyp | Claim | Falsifier |
|---|---|---|
| H1 | BindingsByGVR doesn't watch RBs | RBs in `snowplow_plurals_registered_gvrs`; `snowplow_bindings_by_gvr_delta_skipped_non_typed=0` (delta hook fires per event). The BindingsByGVR is the SEED-TARGETING index per bindings_by_gvr.go:21-25; "AUTHZ BOUNDARY UNTOUCHED. This index is SEED-TARGETING only." |
| H2 | Cohort memo stale | If memo were stale-empty, refilter would see 0 items (`evaluate_rbac_calls=0`). Log shows 1 item. Memo IS fresh post-RB-add. |
| H3 | Stage-2 iterator cache | Iterator collapses to SA-cluster-scope LIST (cluster_list.go: useClusterList denied at Gate 5 for cj → falls back to per-NS iter path; for THIS RA there's no per-NS iter because the `.crds` iterator yields only the ONE CRD plural and the iterator path is cluster-scope regardless). The iterator dimension is correct. |
| H4 | Snowplow uses its own evaluator (not apiserver SSAR) | Confirmed — `EvaluateRBAC` (rbac/evaluate.go:110) is in-process. But the evaluator implementation is correct; the BUG is upstream of the evaluator (the wrong namespace is fed in). |
| H5 | JWT subject identity mismatch | JWT carries `username=cyberjoker, groups=[devs]`. Matches snowplow's expectation. CohortNSACL correctly identifies cj's bindings (memo populates with 1 item). |
| H6 | Walker missed RB watch | `rbac.authorization.k8s.io/v1/rolebindings` IS in `snowplow_plurals_registered_gvrs.gvrs`. |

## Bonus latent finding (NOT load-bearing for this defect)

The cohort-memo invalidation has a **latent gen-collision bug**: `CohortRBACGen(username, groups)` normalizes username to `groupOnlyCohortSentinel` for users with no User-kind bindings (binding_set_enumeration.go:148-180). The cohort-memo storage key is computed by `CohortKeyHash(username, groups)` WITHOUT normalization (apistage_cohort_memo.go:125-127). When a user transitions across the normalization boundary (gains/loses User-kind bindings), the gen-state key inside CohortRBACGen switches buckets — the new bucket starts at gen=0 and can bump to a value that COINCIDENTALLY equals the previously-stamped memo.rbacGen for the same memo storage key. A coincidence yields a stale-memo serve.

**This is NOT the defect we are seeing today** (the memo IS fresh per the 1-item evidence above). But it is a separate correctness hazard worth a follow-up ship. Documented for the regression journal.

## Design — Ship 0.30.235

### Architectural choice

The clean fix is to **run the UAF refilter on the PRE-filter raw items**, so `namespaceFrom: ".metadata.namespace"` evaluates against the K8s shape (with `.metadata`) instead of the projection shape.

Two implementation options:

#### Option A — Pre-filter UAF in jsonHandlerCore

Move the UAF call into `jsonHandlerCore` (handler.go:83), BEFORE the stage filter is applied. The call sequence becomes:
1. Decode → `tmp` (raw envelope value, e.g. `{kind, apiVersion, items: [...]}`)
2. **NEW**: if `apiCall.UserAccessFilter != nil`, run UAF refilter on `tmp.items` (or `tmp` if a non-list shape) → narrowed `tmp`
3. Apply stage filter → projected `tmp`
4. Merge into dict

Pros: clean layering, UAF sees the K8s shape it documents (`.metadata.namespace`).
Cons: requires threading `apiCall` into `jsonHandlerCore`'s opts; current handler is filter-only.

#### Option B — UAF on the iterator-worker's raw envelope (pre-jsonHandler)

In the worker (resolve.go ~688), after a successful gated/dispatched envelope is in hand and BEFORE feeding into jsonHandler, run UAF on the envelope's items. Then feed the narrowed envelope into jsonHandler. The stage filter runs on the already-narrowed envelope (projection still drops `.metadata`, but the UAF check already passed against the metadata-bearing shape).

Pros: structural — same shape boundaries.
Cons: UAF currently runs ONCE post-g.Wait() (one merged dict[id]); moving it into the worker means it runs per-call (N times for N iterator elements). For the compositions-list case N=1 (single CRD plural), no cost change. For other RAs with per-namespace iterators, UAF would run per-iter call instead of once on the merged dict — same total work, different shape.

### Recommendation: Option A (jsonHandlerCore-internal)

Adopt Option A. The change is contained to handler.go + a small plumb-through. The UAF refilter operates on the same shape (raw envelope) regardless of whether the call came from cache or fresh dispatch — uniform behaviour.

### File:line changes

1. `internal/resolvers/restactions/api/handler.go:16-20` — extend `jsonHandlerOptions`:
   ```go
   type jsonHandlerOptions struct {
       key    string
       out    map[string]any
       filter *string
       uaf    *templates.UserAccessFilterSpec // NEW
       apiCallName string                      // NEW — for log emission
   }
   ```

2. `internal/resolvers/restactions/api/handler.go:83-120` (`jsonHandlerCore`) — insert UAF call BEFORE the filter:
   ```go
   func jsonHandlerCore(ctx context.Context, opts jsonHandlerOptions, tmp any) error {
       log := xcontext.Logger(ctx)

       pig := map[string]any{
           opts.key: tmp,
       }
       if si, ok := opts.out["slice"]; ok {
           pig["slice"] = si
       }

       // NEW (Ship 0.30.235): UAF runs on the RAW envelope (with .metadata
       // intact) BEFORE the stage filter projects items to a stage-specific
       // shape. The projection can omit .metadata and break NamespaceFrom JQ.
       // Running UAF first uses the K8s-canonical shape the UAF spec documents.
       if opts.uaf != nil {
           rf := applyUserAccessFilterOnPig(ctx, pig, opts.key, opts.uaf)
           emitRefilterFalsifierFromHandler(log, opts.apiCallName, rf)
       }

       if opts.filter != nil {
           // ...existing filter logic, now operating on already-narrowed pig...
       }
       // ...rest of merge logic unchanged...
   }
   ```

3. `internal/resolvers/restactions/api/refilter.go` — add a sibling `applyUserAccessFilterOnPig` that operates on the `pig` map keyed by stage name. Signature aligns with existing `applyUserAccessFilter` (refilter.go:71); the inner `refilterSlice` / `evalSingle` are unchanged.

4. `internal/resolvers/restactions/api/resolve.go:512` — pass `uaf` and `apiCallName` into `hOpts`:
   ```go
   hOpts := jsonHandlerOptions{
       key: id, out: dict, filter: apiCall.Filter,
       uaf: apiCall.UserAccessFilter, apiCallName: apiCall.Name,
   }
   ```

5. `internal/resolvers/restactions/api/resolve.go:879-888` — REMOVE the post-g.Wait() `applyUserAccessFilter` call (its work is now done per-worker inside jsonHandlerCore). The `uafActive` flag and `emitRefilterFalsifier` remain on the dispatch / unservable paths.

### LOC estimate

- handler.go: +25 LOC (struct fields + UAF call block)
- refilter.go: +40 LOC (new `applyUserAccessFilterOnPig` shim; reuses existing helpers)
- resolve.go: +5 LOC (hOpts wiring), -15 LOC (delete post-g.Wait() block)

Net: ~+55 LOC.

### Pre-commit falsifier (HARD GATE)

A unit test at `internal/resolvers/restactions/api/refilter_layering_test.go` that:

1. Constructs a synthetic stage spec mirroring `compositions-list`:
   - filter: `[.testStage.items[]? | {uid, name, ns: .metadata.namespace}]`
   - UAF: `{verb: list, group: composition.krateo.io, namespaceFrom: .metadata.namespace}`
2. Seeds an RBACSnapshot via `PublishRBACSnapshotForTest` with:
   - Role in ns=A granting `composition.krateo.io/* list`
   - RoleBinding in ns=A binding User=user1 to the Role
3. Constructs a fake list envelope with 2 items: one in ns=A, one in ns=B (no grant).
4. Runs the resolver path on user1's identity.
5. Asserts: `dict["testStage"]` has 1 item with `ns: "A"`.

**Pre-commit gate**: this test FAILS on 0.30.233 (binary at HEAD) because UAF currently runs post-filter on the projected `{ns}`-only shape and cluster-scope-denies user1. It PASSES on the Ship 0.30.235 binary.

### Post-deploy gate

Re-run phase6 from S4 against the live cluster:
```
$ python -m bench --from-stage S4
```

Expectation: cj's VERIFY converges within the 300s budget:
- `api=1, ui=1, expected=1, cluster=20` (cj sees exactly the 1 composition in bench-ns-01)

Admin's piechart MUST still serve 20 (no regression).

### Risk register

| Risk | Mitigation |
|---|---|
| Order change exposes a callsite that relied on filter-before-UAF | The current code only has ONE UAF call site (refilter.go:886). The test corpus (refilter_test.go, refilter_uaf_resourcesFrom_test.go, etc.) exercises UAF in isolation — they pass the K8s-shape items directly, so the order swap is transparent. |
| Per-worker UAF call adds RBAC eval cost on iterator stages | For compositions-list the iterator is N=1; no cost change. For per-namespace iterator RAs, UAF currently runs ONCE on the merged dict[id]; post-fix it runs per-worker call but on the same total item set. The per-call dispatch already pays per-item EvaluateRBAC in the L1 gate; running UAF in the same goroutine is locality-neutral. |
| UAF on pre-filter shape might over-keep items that the filter would have dropped | The filter is a stage-spec projection — it does NOT make RBAC decisions, it only reshapes already-permitted items. UAF on pre-filter ALWAYS sees ≥ the post-filter item set. There is no security loss; the per-item count fed to UAF is identical to today's post-filter set when the filter is a 1:1 projection. |
| feedback_no_special_cases — fix must be uniform | Affirmed. The change is GVR-uniform, identity-uniform, resource-uniform. No carve-out for compositions / cj / bench-ns-*. |
| feedback_l1_invalidation_delete_only — fix must not touch eviction | Affirmed. UAF order change is a request-path-only narrowing; no L1 invalidation hooks touched. |
| feedback_check_k8s_clientgo_prior_art — prior art? | k8s.io/apiserver runs authorization BEFORE the storage layer's shape projections. Our fix aligns the snowplow resolver with that ordering. |

### Why not "fix the portal RESTAction" (i.e. change `namespaceFrom: ".ns"`)

`project_no_upstream_authority.md`: snowplow cannot wait for portal-chart fixes. AND `feedback_no_special_cases.md`: the snowplow resolver must not hardcode per-RA assumptions. The fix must make snowplow robust to any RA spec that conforms to the layering contract (UAF expressions reference K8s shape; filter expressions project to stage shape). Today's layering DOES NOT enforce this — UAF sees post-filter, which breaks every RA whose filter projection drops `.metadata`.

### Customer-priority + refresher decoupling

The Ship 0.30.235 fix does not touch the refresher / seed paths. The change is per-/call request path only. Per `feedback_customer_priority_over_refresher`, customer /call latency is unaffected; the refresher continues to populate apistage cells; the cohort memo continues to dedupe per-cohort.

## ACK + handoff to team-lead for PM gate

- **TRACED** verdict: ✅ file:line empirical evidence, apiserver SSAR oracle, /debug/vars probe, refilter log line, RA spec on disk.
- **Falsifier-first**: ✅ unit test at refilter_layering_test.go is the pre-commit hard gate.
- **No special-cases**: ✅ change is GVR/identity/resource uniform.
- **k8s prior art**: ✅ aligns with apiserver authz-before-projection ordering.
- **PM-gate question (per feedback_empirical_root_cause_trace_before_fix)**: "Would the fix make the symptom disappear?"
  - YES. With UAF running on pre-filter shape, namespaceFrom extracts `bench-ns-01`, EvaluateRBAC is called with the right namespace, cj's RoleBinding permits the item, refilter keeps it, status.list=[1 element], piechart title="1".
- **Fresh-eyes PM gate**: cache-architect has visited the RBAC indexer surface 4×. Per `feedback_recurring_regression_pattern`, this design is FIRST architect deliverable for the LAYERING surface (refilter/jsonHandler ordering), not the indexer. Different surface entirely; fresh-eyes lens applied at the design phase.

Sign cache-architect.
