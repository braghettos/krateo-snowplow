# ADR 0006 — Snowplow owns the external api-step HTTP fetch (accept YAML as well as JSON)

- **Status:** Accepted (snowplow 1.2.0)
- **Lead (verify against code):** `internal/resolvers/restactions/api/external_fetch.go`
  (`httpFetchAllowingNonJSON`), wired at the external branch of
  `internal/resolvers/restactions/api/resolve.go`.
- **Deep dive:** [`request-lifecycle.md`](../architecture/request-lifecycle.md) §2.9.

## Context

A `RESTAction` `spec.api[]` stage that targets an external `endpointRef` is dispatched
through `github.com/krateoplatformops/plumbing` `http/request.Do`. That function **rejects any
non-JSON `Content-Type` with HTTP 406 *before* the `ResponseHandler` is invoked**
(`plumbing@v0.9.x http/request/request.go:118-119`, identical through the latest `v1.9.0`), and
its handler type is `func(io.ReadCloser) error` (request.go:25) — the `*http.Response`/headers
are **not** passed to snowplow. snowplow cannot patch plumbing (never-upstream, ADR convention).

The portal Marketplace blueprint-discovery RA must GET a Helm repo `index.yaml` — which repos
commonly serve as `text/plain` / `text/yaml` / `application/x-yaml`. Under the plumbing path that
body is 406'd and never reaches jq, so the RA cannot consume it. More generally, no RESTAction api
step could consume *any* YAML endpoint.

## Decision

**For the external api-step branch, snowplow owns the HTTP round-trip** rather than calling
plumbing's gating `Do`. `httpFetchAllowingNonJSON` (`external_fetch.go`) is a faithful
transcription of `plumbing` `request.Do` (`request.go:38-116`) **minus only the 406 JSON gate**,
and it **reuses every security-critical path verbatim** via plumbing's *exported* helpers:

- `HTTPClientForEndpoint` — TLS, custom CA, client certs, proxy, timeouts, **and** the
  bearer / basic / AWS-SigV4 auth roundtrippers.
- `util.NewRetryClient` / `RetryClient.Do` — QPS limiter + 429/5xx retry.
- `ComputeAwsHeaders` — SigV4 header pre-compute.

Only ~40–50 LOC of pure *request assembly* + the non-2xx → `response.Status` error-envelope
shaping (transcribed byte-identical to `request.go:102-116`) is snowplow-local. With the response
in hand, snowplow now sees the `Content-Type` and **accepts JSON or YAML transparently**: a
JSON-shaped body passes through unchanged; otherwise (YAML content-type, or a body that fails
`json.Unmarshal`) it is converted with `sigs.k8s.io/yaml` `YAMLToJSON` before the existing decode.
`YAMLToJSON` round-trips valid JSON losslessly, so the JSON fast-path is byte-identical.

## Consequences

- **A RESTAction api step can consume any YAML *or* JSON external endpoint** (Helm `index.yaml`,
  `Chart.yaml`, any `*.yaml`). Purely **additive**: non-JSON was a hard 406 before, so nothing that
  worked previously changes (the application/json control is byte-identical).
- **The security surface stays minimal.** TLS / CA / client-certs / bearer-basic-AWS creds / proxy
  / retry all remain delegated to plumbing's exported helpers; only stable request *assembly* is
  transcribed. A future plumbing change to assembly must be mirrored; the volatile parts evolve
  under us for free.
- **One new, bounded error edge.** A 2xx body that is neither JSON nor YAML now reaches the decode
  and errors there (instead of an upstream 406) — caught by the per-item error path
  (`recordItemError`), no panic, no credential leak (the request still carried creds via the reused
  roundtrippers).
- **Cache posture unchanged.** External-endpoint results were never L1-pivot-cacheable and still
  aren't — they ride the live fetch each request. This change touches only the transport, downstream
  of every cache branch.
- **Toggle-able / removable** per ADR 0004: the conversion is on the live external path; under
  `CACHE_ENABLED=false` the same fetch runs (the cache toggle is orthogonal).

## Related

- ADR 0004 — caching is provisional and removable.
- The companion correctness fix shipped alongside (snowplow 1.2.0): a per-item **feed/decode error
  on any served dispatch branch records into `errorKey` and does *not* truncate the resolve** (the
  #313 Option C-A contract) — see [`request-lifecycle.md`](../architecture/request-lifecycle.md) §3.
