# Generic export (`GET /export`)

`/export` turns **any `/call`-resolvable list** — a `RESTAction` or a
list/table `Widget` — into a downloadable **CSV** or **JSON** attachment.
It is a pure serializer layered on the `/call` resolve lane: the request
is re-dispatched **in-process** through the same dispatcher chain the
`GET /call` route uses, so authentication, the RBAC gate and the
serve-time user-aware filtering are identical. An export can never
contain more than the caller's own `/call` would return.

Every export also emits an [`AuditEvent`](./audit-correlation.md)
(structured log record, `action=export`) carrying the request session id
(the W3C **`baggage`** member `session.id`), so data egress is auditable
end-to-end. (The bespoke `X-Krateo-Correlation-Id` header is retired — the
server ignores it; correlation now rides W3C baggage.)

## Request

```
GET /export?apiVersion=<gv>&resource=<plural>&name=<name>&namespace=<ns>[&format=...][&path=...][&fields=...][&filename=...]
```

The `apiVersion` / `resource` / `name` / `namespace` parameters are the
standard `/call` ones. Authentication is the same JWT used for `/call`.

| Parameter  | Default | Description |
|------------|---------|-------------|
| `format`   | `csv`   | `csv` or `json`. |
| `path`     | auto    | Optional **jq expression** selecting the row array inside the resolved envelope, e.g. `.status.services`. |
| `fields`   | all     | Comma-separated list of column **dot-paths** selecting and ordering the CSV columns, e.g. `name,health,usage.cpu`. |
| `filename` | derived | Attachment file name (sanitized; extension appended). |

### Row auto-detection

When `path` is omitted the rows are located by convention, in order:

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
