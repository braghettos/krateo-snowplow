# F6 (#130) — portal handoff: land the 104 `spec.keyExtras` declarations

Date: 2026-07-13 · snowplow side @ `828ac8e` (PR #109, merged to main @ `fcd45f9`, **not cut**) · audit source: `docs/f6-keyextras-audit-2026-07-12.md` + `/tmp/f6-audit/definitive-table.txt`

## Purpose

Snowplow F6 (`spec.keyExtras`) is merged but **cut-blocked on this portal-side landing** (A6-class cross-repo sequencing, PR #109 cut-conditions). F6 flips the widget cache key to a fold-nothing default: only the request-extras keys a widget DECLARES in `spec.keyExtras` partition its cache cell. Without a declaration a route/query-consuming widget shares one cell across routes/search/filter/time-range and serves stale/wrong content (the snowplow self-quarantine guard makes it leak-safe cross-user, but it does NOT fix stale-cross-route serving — only the declaration does).

This doc is the zero-re-derivation pack: the CRD schema field to add, ready-to-paste `keyExtras` blocks for all 104 widgets that need one (grouped by declaration set), the post-landing verification recipe, and the sequencing.

The widget→keyExtras mapping is **authoritative** (read from the live CRs on GKE `krateo-installer-test`). The one thing this pack cannot pin is the exact per-file location of each declaration inside the Portal blueprint chart (`oci://ghcr.io/braghettos/krateo/portal` 1.5.11) — the widget-template YAML naming convention observed in the older generation is `<kind>.<widget-name>.yaml` under `blueprint/templates/`; confirm against the 1.5.11 chart source (see audit §Caveats).

## 1. CRD schema field (add to every widget-kind CRD)

`spec.keyExtras` is a byte-for-byte mirror of the existing A2 `spec.identityContext` field — same array-of-string OpenAPI shape, same optionality — so it lands through the identical mechanism `identityContext` already went through in A6. Verified live shape of `spec.identityContext` on `buttons.widgets.templates.krateo.io` (v1beta1):

```json
"identityContext": {"items": {"enum": ["username", "groups"], "type": "string"}, "type": "array"}
```

Add `keyExtras` alongside it in each widget-kind CRD's `spec.properties`, in whatever form the portal chart's CRD schema source uses (mirror however `identityContext` is authored there). The only difference from `identityContext`: **no `enum`** — request-extras key names are author-open (the live corpus reads 8: `namespace`, `name`, `q`, `range`, `project`, `status`, `category`, `source`, but a `Select` widget's `queryParam` is author-configurable, so constraining the enum would be over-restrictive and could prune a valid future declaration).

```yaml
# in each widget-kind CRD, spec.properties (sibling of identityContext):
keyExtras:
  type: array
  items:
    type: string
  description: >-
    F6 (#130): the request-extras keys (route params / URL query folded into
    ?extras=) whose values this widget's resolution depends on. Only declared
    keys partition the widget cache cell. Absent/empty = the widget does not
    vary by any request extra (chrome/layout default). Author-open key names
    (namespace, name, q, range, project, status, category, source in the current
    corpus).
```

The widget kinds that need the field are exactly the kinds appearing in §3 below: **Button, Card, Descriptions, Form, LineChart, Listy, Paragraph, Select, Statistic, Table, Tag, YamlViewer** (12 kinds). Adding the optional field to every widget CRD is harmless (widgets that never declare it are unaffected) and is the simplest correct move.

## 2. Sequencing (one paragraph)

Land the CRD `keyExtras` field **and** all 104 declarations below in the Portal blueprint chart, roll it so the live widget CRs carry the values (verify with §4 — the CRD schema must stop pruning the field), then **confirm to the snowplow session**. Only then does snowplow cut its release off main @ `fcd45f9` and deploy. Deploying snowplow F6 BEFORE the declarations are live is the exact hazard this sequencing prevents: undeclared route/query widgets would collapse to one cross-route cell and serve wrong content. F5 (#131, PR #110) has no such dependency and can cut anytime.

## 3. Per-widget declarations (N=104, grouped by declaration set)

Each block is copy-pasteable; the comment on each line is `# <Kind>/<widget-name>` so you can locate the widget-template YAML by its `(kind, name)`. The `keyExtras` value is identical within a group. Distribution matches audit §6.

### `keyExtras: [project, q, status]` — 30 widgets
apiRef consumers: compositions-list.

```yaml
# Button/comp-status-btn-all  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Button/comp-status-btn-failed  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Button/comp-status-btn-healthy  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Button/comp-status-btn-pending  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Card/obs-by-kind-card  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Card/obs-by-namespace-card  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Card/obs-conditions-card  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Card/status-card  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Listy/dashboard-conditions  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Listy/obs-by-kind-list  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Listy/obs-by-namespace-list  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Listy/obs-conditions-list  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Listy/obs-rail-list  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Listy/obs-reconcile-breakdown  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Listy/rail-list  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/obs-reconcile-stat  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/obs-stat-failed  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/obs-stat-healthy  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/obs-stat-kinds  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/obs-stat-pending  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/obs-stat-projects  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/obs-stat-total  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/stat-compositions  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/stat-failed  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/stat-healthy  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Statistic/stat-reconciles  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Table/compositions-table  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Tag/delta-tag-failed  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Tag/delta-tag-healthy  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
# Tag/delta-tag-reconciles  (apiRef: compositions-list)
spec:
  keyExtras: [project, q, status]
```

### `keyExtras: [range]` — 28 widgets
apiRef consumers: dashboard-data, obs-error-log-digest, obs-log-stream, obs-reconcile-by-composition, obs-reconcile-metrics, obs-reconcile-perf. No-apiRef UI-filter widgets: comp-range-btn-24h, comp-range-btn-30d, comp-range-btn-7d, comp-range-btn-all, obs-range-btn-1h, obs-range-btn-24h, obs-range-btn-6h, obs-range-btn-7d, range-btn-24h, range-btn-30d, range-btn-7d, range-btn-all.

```yaml
# Button/comp-range-btn-24h  (no apiRef)
spec:
  keyExtras: [range]
# Button/comp-range-btn-30d  (no apiRef)
spec:
  keyExtras: [range]
# Button/comp-range-btn-7d  (no apiRef)
spec:
  keyExtras: [range]
# Button/comp-range-btn-all  (no apiRef)
spec:
  keyExtras: [range]
# Button/obs-range-btn-1h  (no apiRef)
spec:
  keyExtras: [range]
# Button/obs-range-btn-24h  (no apiRef)
spec:
  keyExtras: [range]
# Button/obs-range-btn-6h  (no apiRef)
spec:
  keyExtras: [range]
# Button/obs-range-btn-7d  (no apiRef)
spec:
  keyExtras: [range]
# Button/range-btn-24h  (no apiRef)
spec:
  keyExtras: [range]
# Button/range-btn-30d  (no apiRef)
spec:
  keyExtras: [range]
# Button/range-btn-7d  (no apiRef)
spec:
  keyExtras: [range]
# Button/range-btn-all  (no apiRef)
spec:
  keyExtras: [range]
# Card/obs-errors-card  (apiRef: obs-error-log-digest)
spec:
  keyExtras: [range]
# Card/obs-phases-card  (apiRef: obs-reconcile-perf)
spec:
  keyExtras: [range]
# Card/obs-throughput-card  (apiRef: obs-reconcile-metrics)
spec:
  keyExtras: [range]
# Card/throughput-card  (apiRef: dashboard-data)
spec:
  keyExtras: [range]
# LineChart/obs-reconcile-throughput  (apiRef: obs-reconcile-metrics)
spec:
  keyExtras: [range]
# LineChart/throughput-chart  (apiRef: dashboard-data)
spec:
  keyExtras: [range]
# Listy/obs-error-log-list  (apiRef: obs-error-log-digest)
spec:
  keyExtras: [range]
# Listy/obs-reconcile-phases  (apiRef: obs-reconcile-perf)
spec:
  keyExtras: [range]
# Paragraph/delta-cap-compositions  (apiRef: dashboard-data)
spec:
  keyExtras: [range]
# Paragraph/greeting-subtitle  (apiRef: dashboard-data)
spec:
  keyExtras: [range]
# Statistic/obs-perf-p50  (apiRef: obs-reconcile-perf)
spec:
  keyExtras: [range]
# Statistic/obs-perf-p95  (apiRef: obs-reconcile-perf)
spec:
  keyExtras: [range]
# Statistic/obs-perf-p99  (apiRef: obs-reconcile-perf)
spec:
  keyExtras: [range]
# Table/obs-log-stream  (apiRef: obs-log-stream)
spec:
  keyExtras: [range]
# Table/obs-reconcile-by-composition  (apiRef: obs-reconcile-by-composition)
spec:
  keyExtras: [range]
# Tag/delta-tag-compositions  (apiRef: dashboard-data)
spec:
  keyExtras: [range]
```

### `keyExtras: [name, namespace]` — 23 widgets
apiRef consumers: blueprint-detail, blueprint-formdef, composition-detail, composition-editdef, composition-events, composition-resources.

```yaml
# Button/blueprint-detail-create  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace]
# Button/blueprint-detail-delete  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace]
# Button/composition-detail-delete  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# Button/composition-detail-pause  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# Button/composition-detail-sync  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# Card/card-blueprint-detail-info  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace]
# Card/card-composition-detail-rail  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# Descriptions/descriptions-blueprint-detail-info  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace]
# Descriptions/descriptions-blueprint-detail-metadata  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace]
# Descriptions/descriptions-composition-detail-metadata  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# Descriptions/descriptions-composition-detail-spec  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# Form/blueprint-create  (apiRef: blueprint-formdef)
spec:
  keyExtras: [name, namespace]
# Form/blueprint-update  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace]
# Form/composition-edit  (apiRef: composition-editdef)
spec:
  keyExtras: [name, namespace]
# Listy/list-composition-detail-events  (apiRef: composition-events)
spec:
  keyExtras: [name, namespace]
# Listy/list-composition-detail-rail  (apiRef: composition-resources)
spec:
  keyExtras: [name, namespace]
# Listy/list-composition-detail-relations  (apiRef: composition-resources)
spec:
  keyExtras: [name, namespace]
# Paragraph/blueprint-detail-subtitle  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace]
# Paragraph/blueprint-detail-title  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace]
# Paragraph/composition-detail-header  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# Paragraph/composition-detail-subtitle  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# Tag/composition-detail-status  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
# YamlViewer/yamlviewer-composition-detail-manifest  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace]
```

### `keyExtras: [name]` — 7 widgets
apiRef consumers: marketplace-detail. No-apiRef UI-filter widgets: create-subtitle.

```yaml
# Button/marketplace-detail-action  (apiRef: marketplace-detail)
spec:
  keyExtras: [name]
# Descriptions/descriptions-marketplace-detail-source  (apiRef: marketplace-detail)
spec:
  keyExtras: [name]
# Paragraph/create-subtitle  (no apiRef)
spec:
  keyExtras: [name]
# Paragraph/marketplace-detail-description  (apiRef: marketplace-detail)
spec:
  keyExtras: [name]
# Paragraph/marketplace-detail-links  (apiRef: marketplace-detail)
spec:
  keyExtras: [name]
# Paragraph/marketplace-detail-title  (apiRef: marketplace-detail)
spec:
  keyExtras: [name]
# Tag/marketplace-detail-status  (apiRef: marketplace-detail)
spec:
  keyExtras: [name]
```

### `keyExtras: [q]` — 7 widgets
apiRef consumers: blueprints-cards, global-search.

```yaml
# Button/bp-cat-btn-all  (apiRef: blueprints-cards)
spec:
  keyExtras: [q]
# Button/bp-cat-btn-blueprint  (apiRef: blueprints-cards)
spec:
  keyExtras: [q]
# Button/bp-cat-btn-observability  (apiRef: blueprints-cards)
spec:
  keyExtras: [q]
# Button/bp-cat-btn-platform  (apiRef: blueprints-cards)
spec:
  keyExtras: [q]
# Listy/blueprints-list  (apiRef: blueprints-cards)
spec:
  keyExtras: [q]
# Listy/search-results  (apiRef: global-search)
spec:
  keyExtras: [q]
# Statistic/obs-stat-blueprints  (apiRef: blueprints-cards)
spec:
  keyExtras: [q]
```

### `keyExtras: [category, q, source]` — 4 widgets
apiRef consumers: blueprints-catalog.

```yaml
# Listy/marketplace-category-chips  (apiRef: blueprints-catalog)
spec:
  keyExtras: [category, q, source]
# Listy/marketplace-grid  (apiRef: blueprints-catalog)
spec:
  keyExtras: [category, q, source]
# Listy/marketplace-source-toggle  (apiRef: blueprints-catalog)
spec:
  keyExtras: [category, q, source]
# Select/marketplace-category-filter  (apiRef: blueprints-catalog)
spec:
  keyExtras: [category, q, source]
```

### `keyExtras: [category, name]` — 2 widgets
apiRef consumers: marketplace-detail.

```yaml
# Descriptions/descriptions-marketplace-detail-about  (apiRef: marketplace-detail)
spec:
  keyExtras: [category, name]
# Paragraph/marketplace-detail-subtitle  (apiRef: marketplace-detail)
spec:
  keyExtras: [category, name]
```

### `keyExtras: [name, namespace, status]` — 2 widgets
apiRef consumers: blueprint-detail, composition-detail.

```yaml
# Listy/list-composition-detail-conditions  (apiRef: composition-detail)
spec:
  keyExtras: [name, namespace, status]
# Tag/blueprint-detail-status  (apiRef: blueprint-detail)
spec:
  keyExtras: [name, namespace, status]
```

### `keyExtras: [category, q]` — 1 widgets
apiRef consumers: blueprints-cards.

```yaml
# Listy/blueprints-grid  (apiRef: blueprints-cards)
spec:
  keyExtras: [category, q]
```


## 4. Verification recipe (portal session, after landing)

After the CRD field + declarations roll, prove the CRD schema stopped pruning `spec.keyExtras` (a missing schema field is silently dropped by the apiserver — the #1 landing failure mode). For one widget per group + one chrome widget:

```bash
# must PRINT the declared list (proves the schema accepts + persists the field):
kubectl get <kind> <name> -n <widgets-namespace> -o jsonpath='{.spec.keyExtras}'
```

Spot-check set (one representative per group, plus a chrome control):

```bash
# [project, q, status]
kubectl get button comp-status-btn-all       -o jsonpath='{.spec.keyExtras}'   # → ["project","q","status"]
# [range]
kubectl get button range-btn-24h             -o jsonpath='{.spec.keyExtras}'   # → ["range"]
# [name, namespace]
kubectl get paragraph composition-detail-header -o jsonpath='{.spec.keyExtras}' # → ["name","namespace"]
# [name]
kubectl get tag marketplace-detail-status    -o jsonpath='{.spec.keyExtras}'   # → ["name"]
# [q]
kubectl get listy blueprints-list            -o jsonpath='{.spec.keyExtras}'   # → ["q"]
# [category, q, source]
kubectl get listy marketplace-grid           -o jsonpath='{.spec.keyExtras}'   # → ["category","q","source"]
# [category, name]
kubectl get paragraph marketplace-detail-subtitle -o jsonpath='{.spec.keyExtras}' # → ["category","name"]
# [name, namespace, status]
kubectl get tag blueprint-detail-status      -o jsonpath='{.spec.keyExtras}'   # → ["name","namespace","status"]
# [category, q]
kubectl get listy blueprints-grid            -o jsonpath='{.spec.keyExtras}'   # → ["category","q"]
# CHROME CONTROL — MUST stay empty (no declaration; proves the default is fold-nothing):
kubectl get layout app-shell                 -o jsonpath='{.spec.keyExtras}'   # → (empty)
kubectl get menu sidebar-nav                 -o jsonpath='{.spec.keyExtras}'   # → (empty)
```

An empty result on a declared widget = the CRD schema is still pruning the field (schema not landed on that kind) — fix before confirming. A non-empty result on a chrome widget = an accidental declaration (should never happen; chrome must fold nothing).

## 5. Notes / evidence pointers

- The 172 widgets NOT in §3 (chrome/layout + extras-independent apiRefs) need **no** declaration — that is the correct fold-nothing default (audit §4). All 8 known chrome widgets confirmed among them.
- `project-select` (Select) is a PRODUCER of `?project=` but reads nothing → no declaration; its consumer is `compositions-list`-backed widgets (the `[project, q, status]` group).
- `compositions-table` reads `.name` only as a row-column field, not as a route extra → declares `[project, q, status]` (not `name`); see audit §5.
- Full evidence per widget (RA filter reads, false-positive exclusions) is in `docs/f6-keyextras-audit-2026-07-12.md` §3/§5.
