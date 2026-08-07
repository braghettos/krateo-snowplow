---
type: Runbook
title: RESTAction api-steps use direct apiserver paths (the /call loopback is retired)
description: Current-state rules for referencing another RESTAction/Widget from a spec.api[] step — direct apiserver path + resolve (default false), the /call-to-path mapping for any legacy template, and the verification steps.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [portal, restaction, migration, resolver]
timestamp: 2026-08-06T00:00:00Z
---

# RESTAction api-steps use direct apiserver paths (the `/call` loopback is retired)

**Audience:** whoever is editing RESTAction templates (portal-chart /
composition-portal blueprints under the `krateo-platformops` org; portal is
helm-only).

**Status: the migration is complete.** The in-process `/call` loopback for
RESTAction *api steps* was retired in the 2026-06-22 unified ship, after a
corpus audit (`docs/corpus-audit-call-loopback-2026-06-22.md`) confirmed zero
live RESTActions carried a `/call` api-step path. This document now describes
the current contract, and keeps the `/call`→path mapping as a reference for
anyone who finds a legacy `/call` stage in an out-of-tree template.

## The current contract

A `spec.api[]` step that references another `RESTAction` or `Widget` points its
`path` **straight at the referenced CR's Kubernetes apiserver path**. Snowplow
fetches it internally (informer-served where possible — cacheable and
dependency-tracked) and the step's `resolve` field decides what the stage
output is (`apis/templates/v1/core.go`, applied in
`internal/resolvers/restactions/api/resolve.go`):

- **`resolve: true`** — snowplow runs the fetched `RESTAction`/`Widget` through
  the resolver **in-process** and substitutes the **resolved envelope** —
  byte-identical to what an HTTP `/call` of that CR would return, with no
  outbound round-trip. Use this when the step consumes the referenced CR's
  *resolved* output (e.g. a resolved-only `.status.<field>`).
- **`resolve: false` — or OMITTED (the default)** — the stage output is the
  **raw** stored CR (unresolved spec). The default was flipped to omit→false
  on 2026-07-02 (see the `Resolve` field contract in
  `apis/templates/v1/core.go`), aligned with
  progressive rendering: the server returns raw refs/CRs unless a step
  explicitly opts in. **A step that needs the resolved envelope MUST set
  `resolve: true` explicitly.**
- Non-`RESTAction`/`Widget` path (e.g. a ConfigMap, a composition CR):
  `resolve` is a harmless no-op — the raw object is returned.

There is **no** `/call` dispatch branch for api-steps any more: a stage whose
`path` is still a `/call?…` URL falls through to the **external HTTP fetch**
lane — it then needs an `endpointRef` or it errors, and it is **not cached**.

> ⚠️ Only the **RESTAction-stage** `/call` loopback is retired. The
> `/call?resource=…` URL shape is **still the navigation contract** for the
> frontend SPA and the prewarm walker (it appears in widget
> `status.resourcesRefs[].path`, emitted by `buildPath`). Do NOT rewrite
> those — the rules here apply only to `/call` paths appearing as a
> `spec.api[].path` inside a RESTAction.

## Rewriting a legacy `/call` stage

Find candidates in RA templates (e.g. `chart/templates/restaction.*.yaml`):

```
grep -rnE 'path:.*\/call' chart/templates/
```

Only hits where `/call` is a `spec.api[].path` value need rewriting. Ignore
`/call` in comments and in widget `resourcesRefs`/SPA-nav contexts.

`/call` query params map 1:1 to the apiserver path:

| `/call?` param | apiserver path piece |
|---|---|
| `apiVersion=<group>/<version>` | `/apis/<group>/<version>` (core group `v1` → `/api/v1`) |
| `namespace=<ns>` | `/namespaces/<ns>` (omit for cluster-scoped) |
| `resource=<plural>` | `/<plural>` |
| `name=<name>` | `/<name>` (omit for a LIST) |

**Examples**
```yaml
# BEFORE (legacy loopback)
- name: inner
  path: /call?resource=restactions&apiVersion=templates.krateo.io/v1&namespace=krateo-system&name=my-ra
# AFTER — resolve:true is REQUIRED to keep the resolved output (omit→false)
- name: inner
  path: /apis/templates.krateo.io/v1/namespaces/krateo-system/restactions/my-ra
  resolve: true
```
```yaml
# BEFORE (widget)
- name: card
  path: /call?resource=cards&apiVersion=widgets.templates.krateo.io/v1beta1&namespace=krateo-system&name=my-card
# AFTER
- name: card
  path: /apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/cards/my-card
  resolve: true
```
Templated paths work the same — build the same apiserver URL with `${ … }`
instead of a `/call` URL.

## Gotchas

1. **Use a SERVED apiVersion.** If you template the version from CRD data,
   select a **served** version — never `.status.storedVersions[0]` (the storage
   version may be `served:false`, e.g. a `vacuum` migration version →
   guaranteed 404 → uncacheable). Use
   `([.spec.versions[] | select(.served)][0]).name` (or
   storage-preferred-if-served). Hard-coded versions must be the served one.
2. **`&extras={…}` does NOT map.** If a legacy `/call` stage passed
   author-templated `?extras=`, the direct-path form has no equivalent (extras
   are a per-request concept of the *outer* `/call`, not of a CR fetch). Such
   stages need individual review — if the inner RA's jq reads those extras, a
   direct-path fetch won't supply them. Flag these rather than blind-rewrite.
3. **RBAC is unchanged** — the in-process resolve is gated on the requesting
   identity being allowed to `get` the referenced CR (same as the old `/call`),
   and is depth-capped (cyclic refs terminate with a bounded error; a
   denied/depth-capped resolve surfaces a 403-class error, not empty content).
   Cache-off (`CACHE_ENABLED=false`) resolves the same data via the user's own
   token — transparent.
4. **Pagination:** a `/call?…&page=N&perPage=M` LIST becomes a direct LIST path
   with `?limit=&continue=` (apiserver paging). Single-CR GET-by-name has no
   pagination.

## Verify (per stage)

In the snowplow pod logs for a request that hits the stage:

- **No** `/call` outbound HTTP for the inner ref; the dispatch is in-process.
- **No** `the server could not find the requested resource` and **no**
  `declining to cache the partial result` for that traceId (if you see these,
  the apiVersion is wrong — gotcha #1).
- With `resolve: true`, the stage output is the resolved envelope (identical to
  an HTTP `/call` of the referenced CR); the result is cacheable → 2nd
  navigation logs `l1_hit:"hit"`.

See `howto/restactions.md` (the `resolve` field) and
`docs/corpus-audit-call-loopback-2026-06-22.md` (the retirement scope).
Portal-chart changes remain **helm-only** (chart template → helm upgrade),
never `kubectl apply`.
