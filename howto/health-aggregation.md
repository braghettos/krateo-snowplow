# Health & usage aggregation (built-in `health` jq module)

Snowplow ships a **built-in jq module** (`internal/support/jq/modules/health.jq`)
available to every `RESTAction` filter and widget expression via
`include "health";` — no `JQ_MODULES_PATH` deployment configuration
needed (filesystem modules with the same name still win, so operators
can override it).

It defines the **normalized health vocabulary** used to aggregate
heterogeneous per-service health signals into one uniform scale:

> `OK` / `Warning` / `Critical` / `Unknown`
> (aggregation severity: `Critical` > `Warning` > `Unknown` > `OK`)

This is the generic mechanism behind consolidated health dashboards:
each service emits whatever raw status its backend produces
(`Healthy`, `running`, `GREEN`, `CrashLoopBackOff`, booleans, ...), and
a single RESTAction normalizes and rolls it up — no bespoke widget per
service.

## Functions

| Function | Input | Output |
|----------|-------|--------|
| `normalize_health` | any raw health value | `"OK"` / `"Warning"` / `"Critical"` / `"Unknown"` |
| `health_severity` | any raw health value | numeric severity (0–3, for sorting) |
| `worst_health` | array of raw values | worst normalized status (`"Unknown"` for `[]`) |
| `health_summary(f)` | array of objects, `f` extracts the raw value | `{total, ok, warning, critical, unknown, overall}` |
| `health_summary` | array of objects with a `.health` field | shorthand for `health_summary(.health)` |
| `health_rollup(g; f)` | array of objects, grouped by `g` | one `{key} + summary` per group |
| `usage_pct(used; capacity)` | numbers | percentage (1 decimal) or `null` when capacity is unknown/0 |
| `usage_health(pct; warnAt; critAt)` | percentage + thresholds | normalized status |
| `usage_summary(fu; fc; warnAt; critAt)` | array of objects | `{used, capacity, pct, status}` |

## Example — consolidated health RESTAction

One RESTAction fans out to N service health endpoints (one api-step per
service, or one step iterating a discovered list) and aggregates:

```yaml
apiVersion: templates.krateo.io/v1
kind: RESTAction
metadata:
  name: service-health-rollup
  namespace: demo
spec:
  api:
    - name: services
      # any endpoint returning [{org, tenant, service, health, used, capacity}, ...]
      path: /api/services/health
      endpointRef:
        name: my-aggregator
        namespace: demo
  filter: |
    include "health";
    (.services // []) as $rows
    | {
        overall:  ($rows | health_summary),
        byOrg:    ($rows | health_rollup(.org; .health)),
        byTenant: ($rows | health_rollup(.tenant; .health)),
        services: ($rows | map(. + {
            health: (.health | normalize_health),
            usagePct: usage_pct(.used; .capacity),
            usageStatus: usage_health(usage_pct(.used; .capacity); 80; 90)
          }))
      }
```

The resolved `status` then feeds any widget uniformly:

- `overall` → a single traffic-light / counters widget
  (`{total, ok, warning, critical, unknown, overall}`);
- `byOrg` / `byTenant` → a filterable rollup table (the `key` of each
  group is the filter dimension — group by any field you need);
- `services` → a per-row table with the normalized status and the
  derived usage percentage/status per configurable thresholds.

Time-interval aggregation composes the same way: point the api-step at a
range-query endpoint (interval as a parameter, e.g. via
[`extras`](./extras.md)) and apply the same functions to the returned
series.

Combined with [`/export`](./export.md), any of these aggregated views is
downloadable as CSV/JSON as-is.
