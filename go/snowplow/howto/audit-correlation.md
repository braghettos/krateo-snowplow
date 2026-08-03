# Audit correlation (`X-Krateo-Correlation-Id` + `AuditEvent`)

Snowplow implements a generic end-to-end audit correlation mechanism
(`internal/support/audit`):

## Correlation id

- Every request may carry an **`X-Krateo-Correlation-Id`** header. The
  portal (or any API client) injects it at the edge to tag one *logical
  business action*; unlike the per-request `X-Krateo-TraceId` shortid
  and the OTel `traceparent`, the same correlation id is reused across
  all the requests that make up that action.
- When the header is absent, snowplow falls back to the request trace
  id (or mints a random id), so the id is always non-empty.
- The id is **echoed on the response** (and CORS-exposed) so the caller
  can persist and link it.
- It is **propagated into every downstream external call** an api-step
  performs (`internal/resolvers/restactions/api/external_fetch.go`), so
  a downstream service/adapter can log its own records under the same
  id and an auditor can link portal action → resolved calls →
  downstream effect.
- Inbound ids are sanitized (max 128 chars, `[A-Za-z0-9._-]`); anything
  else is replaced, never logged raw.

## `AuditEvent` records

Snowplow emits a normalized **`AuditEvent`** as a structured JSON log
line on stdout for:

- every **write** through `/call` (POST/PUT/PATCH/DELETE), and
- every **`/export`** download.

Record shape (under the `audit` group):

```json
{
  "msg": "audit event",
  "audit": {
    "kind": "AuditEvent",
    "correlationId": "…",
    "action": "call | export",
    "verb": "POST",
    "group": "…", "version": "…", "resource": "…",
    "name": "…", "namespace": "…",
    "user": "…", "userGroups": ["…"],
    "outcome": "success | failure",
    "code": 200,
    "timestamp": "RFC3339Nano"
  }
}
```

## Shipping / immutability

Deliberately, snowplow takes **no sink dependency**: the records ride
the existing stdout → log-collector (filelog / Vector / Fluent Bit) →
ClickHouse pipeline (see `internal/tracing` coexistence contract).
Routing `kind=AuditEvent` records to a dedicated audit view **and to an
immutable/WORM sink** is log-pipeline configuration in the
observability stack, not application code — any deployment can add it
without touching snowplow.
