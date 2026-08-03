# Path 3.2.2 — Phase 0 Baselines (0.30.219 helm rev 378)

Captured 2026-05-31 ~23:50 UTC via port-forward → /debug/vars.

## widget-content L1 dispatch lookups (`snowplow_dispatch_l1_lookups`)

| GVR | hit | miss | hit_ratio |
|---|---:|---:|---:|
| widgetContent buttons | 46 | 0 | 1.000 |
| widgetContent markdowns | 15 | 0 | 1.000 |
| widgetContent navmenuitems | 309 | 0 | 1.000 |
| widgetContent pages | 126 | 0 | 1.000 |
| widgetContent panels | 114 | 0 | 1.000 |
| widgetContent routes | 206 | 0 | 1.000 |
| widgetContent rows | 114 | 0 | 1.000 |
| TOTAL widgetContent | 930 | 0 | 1.000 |

**Important**: L1 hit ratio is 1.000 because L1 only sees *populated* keys. The
real customer-facing problem is captured by `snowplow_apiserver_fallthrough_cells`:

## widget-content fallthrough (`snowplow_apiserver_fallthrough_cells`)

| GVR | widget-content-hit | widget-content-miss-per-user-fallback |
|---|---:|---:|
| buttons | 46 | **3,437** |
| markdowns | 15 | **3,401** |
| navmenuitems | 309 | 135 |
| pages | 126 | 45 |
| panels | 114 | 46 |
| routes | 206 | 90 |
| rows | 114 | 46 |

The `widget-content-miss-per-user-fallback` reason is the path the gap
manifests on: customer hits `/call?resource=buttons&namespace=bench-ns-XX&name=...`,
identity-free `widgetContent` cell is empty → falls through to the per-user
widget L1 which has nothing for that specific composition's button CR.

## Coverage at boot (`snowplow_phase1_walk_children`)

Walker visited 114 widget observations across 16 widget GVRs. Per-GVR counts:

| GVR | walked | zero-children |
|---|---:|---:|
| buttons | **2** | 0 |
| datagrids | 2 | 1 |
| eventlists | 10 | 10 |
| filters | 2 | 2 |
| markdowns | **10** | 10 |
| navmenuitems | 3 | 0 |
| navmenus | 1 | 0 |
| pages | 3 | 0 |
| panels | **42** | 0 |
| piecharts | 2 | 2 |
| routes | 2 | 0 |
| routesloaders | 1 | 0 |
| rows | 2 | 0 |
| tables | 12 | 12 |
| tablists | 10 | 0 |
| yamlviewers | 10 | 10 |

## Diagnostic

The 10 per-composition widgets walked correspond to the 10 displayed rows of
`compositions-page-datagrid` (2 datagrids × `perPage=5` default). Cluster
has ~50K compositions, so 99.98% of per-composition widget CRs are never
walked at boot. Customer clicks a composition → backend serves cold.

## Other relevant counters

- `snowplow_phase1_seed_widgets_total` = **0** (cohort widget seed disabled / no-op for our roster)
- `snowplow_widget_content_skipped_rbac_sensitive_total` = **56** (the predicate fires; these widgets correctly route to per-cohort L1)
- `snowplow_phase1_walk_observations_total` = 138
- `snowplow_apiserver_fallthrough_total` = 29,374

## Path 3.2.2 falsifier targets (post-deploy 30-min window)

- `buttons widget-content-hit / (hit + miss-per-user-fallback)` >= 0.95
- `markdowns widget-content-hit / (hit + miss-per-user-fallback)` >= 0.95
- `buttons widget-content-miss-per-user-fallback` count delta < 50 over 30 min
- snowplow pod restarts = 0 (no OOM / panic from amplified populate)
