# Path 3.2.1 / 0.30.219 — Pre-commit diff (Phase 3 ACK)

Date: 2026-05-31
Branch: `ship-0.30.219-path3-2-1-enumeration-fix`
Cluster: GKE `gke_neon-481711_us-central1-a_cluster-1`
Baselines:
- snowplow 0.30.218 (helm rev 374) — Path 3.2 PIP pre-warm shipped but `EnumerateClusterListCells` returns cells=0
- cj /dashboard warm n=3 median **2,633ms** (Path 3.2 dev's report) vs 0.30.215 baseline **1,788ms** (+47%)

## Hard rules honored

- NO new snowplow endpoints — pre-existing `widget_tree.go` + `widget_tree_test.go` + `main.go` `/call-tree` route REVERTED before this branch.
- NO frontend modifications — `braghettos/frontend` unchanged at 1.0.9.
- Helm-only deployment surface (chart-side bump in Phase 4).
- Customer-priority invariant preserved (pre-warm runs under SA scope, before MarkPhase1Done).

## What this ship changes

ONE file modified, ONE test file added. No surface other than `EnumerateClusterListCells`.

### `internal/resolvers/restactions/api/cluster_list_prewarm.go`

**Diff shape**: replaces the iterator-jq-probe-only enumeration algorithm with static GVR extraction from every stage's literal `path` template via `cache.ParseAPIServerPathToDep`.

**Before** (Path 3.2 / 0.30.218):
- Required `DependsOn.Iterator` non-empty
- Required jq probe success on empty dict → FAILS for parent-derived iterators
- Required resolved path to be namespace-scoped (ns != "")
- Production observed: cells=0

**After** (Path 3.2.1 / 0.30.219):
- For each stage: parse `apiCall.Path` directly via `ParseAPIServerPathToDep`
- Accept GET / HEAD verbs only (no PUT/POST/DELETE)
- Skip inter-RA `/call?…` paths (not apiserver)
- Skip unresolved `${…}` templates (parent-derived — stays cold-fallback)
- Accept LIST shape only (name=="")
- Dedupe by GVR.String()

**Net code delta**:
- +1 import (`strings`)
- ~70 LOC: new `extractClusterListGVRFromStage` helper (one path per parse rule, documented in-line)
- `EnumerateClusterListCells` body shrinks from 35 → 25 LOC (delegates to helper)
- Adds one `log.Info("cluster_list.prewarm.enumerated", ...)` summary at end of enumeration so the production counter is observable in pod logs

### `internal/resolvers/restactions/api/cluster_list_prewarm_test.go` (NEW)

10 unit tests pinning the new contract:

| Test | Covers |
| ---- | ------ |
| `TestExtractClusterListGVRFromStage_ClusterScopeLiteralPath` | dominant production case (literal /apis/.../<plural>) |
| `TestExtractClusterListGVRFromStage_WidgetClusterScope` | widget GVRs the SPA fetches: routes, panels, navmenuitems, crds |
| `TestExtractClusterListGVRFromStage_NamespaceScopeLiteralPath` | ns-scope literal (hardcoded ns) registers cluster-list cell |
| `TestExtractClusterListGVRFromStage_ParentDerivedIterator` | `${ …(.version)… }` correctly SKIPPED (stays cold-fallback) |
| `TestExtractClusterListGVRFromStage_InterRACall` | `/call?...` SKIPPED |
| `TestExtractClusterListGVRFromStage_GETByName` | path with name segment SKIPPED |
| `TestExtractClusterListGVRFromStage_NonGETVerb` | PUT/POST/DELETE/PATCH SKIPPED |
| `TestExtractClusterListGVRFromStage_EmptyOrNil` | degenerate inputs |
| `TestEnumerateClusterListCells_FullProductionRoster` | pins the 0.30.218 RA inventory captured 2026-05-31; asserts exact 5-cell roster |
| `TestEnumerateClusterListCells_NilAndEmpty` + `_DedupAcrossRAs` | edge cases |

All tests PASS under `go test -race -count=1`.

## Target enumeration set (post-fix)

Captured from live 0.30.218 cluster via `kubectl get restactions.templates.krateo.io -n krateo-system -o jsonpath='{.spec.api}'` for each of the 7 deployed RAs:

| Cell # | GVR | Source RA(s) |
| ------ | --- | ------------ |
| 1 | `widgets.templates.krateo.io/v1beta1/routes` | all-routes |
| 2 | `core.krateo.io/v1alpha1/compositiondefinitions` | blueprints-list |
| 3 | `widgets.templates.krateo.io/v1beta1/panels` | blueprints-panels + compositions-panels (deduped) |
| 4 | `apiextensions.k8s.io/v1/customresourcedefinitions` | compositions-get-ns-and-crd |
| 5 | `widgets.templates.krateo.io/v1beta1/navmenuitems` | sidebar-nav-menu-items |

**Stays cold-fallback** (parent-derived `${…}` — cannot derive at boot):
- compositions-list/`allCompositions` — `${ "/apis/composition.krateo.io/" + (.version) + "/" + (.plural) }` reads `.allNamespacesAndCrds.status` from a prior stage. Refresher's lazy populate covers it at first customer touch (per Path 3.2 design §3.3).

**Inter-RA call** (skipped — not an apiserver path):
- compositions-list/`allNamespacesAndCrds` — `/call?apiVersion=…&resource=restactions&name=compositions-get-ns-and-crd&…`

**Not in current RA roster** but reachable via per-page resourcesRefs (piecharts, tables, buttons, datagrids, pages, rows, forms, markdowns, flowcharts, eventlists): NOT covered by this ship's first part. If Phase-2 measurement shows cj /dashboard warm hasn't crossed the 1,800ms threshold, a second part will harvest widget GVRs via the existing Phase-1 `navWidgetHarvester` — out of scope here, captured as Phase 5 stretch follow-up.

## Side-effects & invariants

- **No behavior change when cells=0** — empty roster still routes to `cluster_list.prewarm.empty_roster` log path (`PrewarmClusterListCells` early-return).
- **No change to populate body** — `populateClusterListCellSync` body unchanged; same shape check, same dispatch, same Put.
- **No change to refresher / dirty-mark / cold-fallback** — those are downstream of enumeration and untouched.
- **Dedupe semantics unchanged** — same `seenGVR[gvr.String()]` keying as before.
- **Customer-priority invariant unchanged** — pre-warm still under SA identity, still bounded by `ClusterListPrewarmTimeout = 60s`, still completes before `MarkPhase1Done`.
- **Per-feedback `feedback_no_special_cases`** — no hardcoded GVR / RA / cohort branching. Algorithm is purely a function of harvested RA path templates.

## Phase 2 (bench probe — REQUIRED before commit/tag)

1. `go build ./...` PASS (confirmed)
2. `go test -race -count=1 -timeout=240s ./internal/resolvers/restactions/api/... ./internal/handlers/dispatchers/...` PASS (confirmed)
3. Build candidate image `0.30.219-pre1`
4. helm upgrade snowplow to candidate (NOT a final 0.30.219 tag)
5. Inspect pod logs for `cluster_list.prewarm.enumerated cells=N` — expect N ≥ 5
6. Inspect pod logs for `cluster_list.prewarm.completed populated=X attempted=Y` — expect Y=N (from step 5) and X close to Y
7. Capture n=3 cj /dashboard warm via Chrome MCP — expect median ≤ 2,200ms (≥20% improvement vs 0.30.218 2,633ms)
8. If step 7 fails: HALT, helm rollback to 374 (0.30.218), document the floor, escalate to architect.

## Hard revert path

`helm rollback snowplow 374 -n krateo-system` — restores 0.30.218 in <30s. The new code is purely additive to the enumeration algorithm; rollback is byte-identical to the prior in-prod release.
