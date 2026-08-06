---
type: Usage
title: Generic export (GET /export)
description: Export any /call-resolvable list (RESTAction or Widget) as a CSV or JSON attachment — parameters, row auto-detection, full-list pagination and the truncation cap, CSV shaping, and the audit trail.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [portal, export, csv, audit]
timestamp: 2026-08-06T00:00:00Z
---

# Generic export (`GET /export`)

`/export` turns **any `/call`-resolvable list** — a `RESTAction` or a
list/table `Widget` — into a downloadable **CSV** or **JSON** attachment.
It is a pure serializer layered on the `/call` resolve lane
(`internal/handlers/export.go`): the request is re-dispatched **in-process**
through the same dispatcher chain the `GET /call` route uses, so
authentication, the RBAC gate and the serve-time user-aware filtering are
identical. An export can never contain more than the caller's own `/call`
would return.

Every export also emits an [`AuditEvent`](./audit-correlation.md)
(a trace-correlated OTLP LogRecord, `krateo.action=export`) carrying the
request session id (the W3C **`baggage`** member `session.id`), so data egress
is auditable end-to-end. (There is no bespoke `X-Krateo-Correlation-Id`
header — correlation rides W3C baggage.)

## Request

```
GET /export?apiVersion=<gv>&resource=<plural>&name=<name>&namespace=<ns>[&format=...][&path=...][&fields=...][&filename=...][&page=...&perPage=...]
```

The `apiVersion` / `resource` / `name` / `namespace` parameters are the
standard `/call` ones. Authentication is the same JWT used for `/call`.

| Parameter  | Default | Description |
|------------|---------|-------------|
| `format`   | `csv`   | `csv` or `json`. |
| `path`     | auto    | Optional **jq expression** selecting the row array inside the resolved envelope, e.g. `.status.services`. |
| `fields`   | all     | Comma-separated list of column **dot-paths** selecting and ordering the CSV columns, e.g. `name,health,usage.cpu`. |
| `filename` | derived | Attachment file name (sanitized; extension appended). |
| `page` / `perPage` | full list | Export exactly that `/call` window. When **both** are omitted, the handler exports the **full list** (below). |

### Full-list pagination and the truncation cap

An `/export` that carries no `page`/`perPage` of its own (the scheduled-export
shape) does **not** inherit the target's default page size. The handler
paginates the `/call` lane internally (`perPage=500`) and concatenates the
extracted rows page by page until a short page signals exhaustion, bounded at
100 pages — a documented cap of **50 000 rows** (`collectAllPages`,
`export.go`). An export that hits the bound is truncated and the response
carries **`X-Export-Truncated: true`**, so a consumer can tell a complete
export from a capped one. A target that ignores the injected pagination is
detected by page fingerprinting and exported once, never duplicated.

### Row auto-detection

When `path` is omitted the rows are located by convention, in order
(`autoDetectRows`):

1. the resolved envelope itself, when it is an array;
2. `.items`;
3. `.status` when it is an array, then `.status.items`;
4. the first (sorted-key order) non-empty array under
   `.status.widgetData`, then under `.status`;
5. otherwise the whole envelope is exported as a single row.

### CSV shaping

Nested objects are flattened to dot-path columns (`usage.cpu`); arrays
and empty objects are JSON-encoded in place; the header row is the
sorted union of all flattened keys (or the explicit `fields` order).
String cells beginning with a spreadsheet formula trigger (`=`, `+`, `-`,
`@`, tab, CR) are prefixed with a single quote so a spreadsheet renders
them as literal text, not an executable formula (`neutralizeCSVCell` —
OWASP CSV-injection guard; the JSON format is unaffected).

## Examples

```sh
# CSV of a RESTAction list, columns picked and ordered explicitly
curl -H "Authorization: Bearer $JWT" \
  "$SNOWPLOW/export?apiVersion=templates.krateo.io/v1&resource=restactions&name=service-list&namespace=demo&fields=name,health,usage.pct"

# JSON of a table widget's rows
curl -H "Authorization: Bearer $JWT" \
  "$SNOWPLOW/export?apiVersion=widgets.templates.krateo.io/v1beta1&resource=tables&name=my-table&namespace=demo&format=json"

# jq-selected rows out of a custom envelope
curl -H "Authorization: Bearer $JWT" \
  "$SNOWPLOW/export?apiVersion=templates.krateo.io/v1&resource=restactions&name=usage&namespace=demo&path=.status.records&format=json"
```

## Scheduled / recurring exports

A scheduled export is deliberately **just a CronJob calling `/export`**
— no extra controller, no export-specific state. The CronJob runs with
its own (least-privilege) credentials, so the recurring export is
RBAC-scoped exactly like an interactive one, and each run emits its own
`AuditEvent`. Where the file goes (S3-compatible object storage, a PVC,
an email gateway, ...) is deployment configuration, not snowplow's
concern.

See [`manifests/scheduled-export.cronjob.example.yaml`](../manifests/scheduled-export.cronjob.example.yaml)
for a complete, generic example that uploads the CSV to any
S3-compatible bucket.
