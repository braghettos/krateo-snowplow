---
type: Decision
title: "ADR 0003 — Identity-free content keys + serve-time RBAC gating"
description: >-
  Two L1 entry classes — widgetContent and apistage — are identity-free in the
  cache key and re-narrowed per requesting user at serve time; the cache holds
  content, identity is applied when the bytes leave the pod. RBAC-sensitive
  widgets bypass the shared cell, and UAF stages are deliberately exempt from
  the raw-RBAC serve gate.
resource: snowplow
tags:
  - adr
  - cache
  - rbac
  - widgets
status: diverged
timestamp: 2026-08-06T00:00:00Z
---

# ADR 0003 — Identity-free content keys + serve-time RBAC gating

- **Status:** Accepted; amended — `widgetContent` is no longer the *only* identity-free class
  (`apistage` joined it when the 0.30.242 "narrowed-at-populate" design was corrected by #58),
  and the serve-time gate has a deliberate UAF exemption.
- **Related:** ADR 0002 (the general per-binding keying rule these are the sanctioned exceptions
  to). Deep dives: [`docs/architecture/caching.md`](../architecture/caching.md),
  [`docs/architecture/rbac-uaf.md`](../architecture/rbac-uaf.md).

## Context

ADR 0002 establishes that identity-bound L1 cells must fold `BindingUID` into the key, because a
content-only key leaks one user's rows to another. But that rule, applied naively to widgets,
forces a separate cached widget body per authorising binding — and a widget body is the most
expensive thing snowplow resolves (it fans out into `resourcesRefs` children, each with its own
`allowed` flag). At 50K scale, paying that cost once per binding for a widget whose *content* is
identical across users is wasteful.

The observation that unlocks the exception: a widget's body is largely **identity-independent**.
The list of `status.resourcesRefs.items[]` and their content is the same for everyone who can see
the widget; only the per-item `allowed` flag (does *this* user get the action button?) is
identity-specific. So the body can be shared if — and only if — the identity-specific part is
recomputed for each request before the bytes are written. The same logic applies one tier down to
the raw apiserver envelope a RESTAction api-stage caches: the envelope is what the SA saw; which
*items* a given user may see is per-user and must be decided at serve time.

## Decision

Snowplow uses two complementary mechanisms, applied to **two** identity-free entry classes.

### 1. The identity-free content keys (`widgetContent`, `apistage`)

- **`widgetContent`** — its key is built by `dispatchWidgetContentKey`
  (`internal/handlers/dispatchers/helpers.go`) with the identity fields left zero, so it is keyed
  only on `(gvr, ns, name, perPage, page, extras)`; `ComputeKey` skips the identity fold for this
  class entirely. An admin and a narrow-RBAC user hit the **same cell**. The stored body is a
  **shell**: `status.resourcesRefs.items[].allowed` flags are present as the SA walker evaluated
  them, but the shell is **never served verbatim**.
- **`apistage`** — its key is built by `contentKeyInputs`
  (`internal/resolvers/restactions/api/apistage.go`) with the identity fields never populated
  (`ComputeKey` would fold them for this class, but they are zero by construction), so the
  per-K8s-call content cell is keyed on `(gvr, ns, name-or-empty, stage)` and shared by every
  user. It is populated **SA-maximal and un-gated** (the miss dispatch skips the inline RBAC
  filter under `WithApistageContentResolve`). The 0.30.242 claim that this cell was
  "RBAC-narrowed at populate time" was wrong as shipped; #58 re-established the serve-time gate.

### 2. Serve-time gating (the cache is not trusted for RBAC)

Results are re-filtered against the requesting user *at serve time*, never trusted from the cache:

- On a `widgetContent` hit, `gateWidgetEnvelope`
  (`internal/handlers/dispatchers/widget_content.go`) **overwrites every `allowed` flag** under
  the request's own identity (via `rbac.UserCan`) before serialisation. The body is shared; the
  bytes that leave the pod are per-user.
- The shared-cell path is **bypassed entirely** for RBAC-sensitive widgets
  (`isRBACSensitiveApiRefWidget`, same file — applied symmetrically at the populate-write and
  serve-read sites): a widget whose `status.widgetData` is built from an apiRef through
  `widgetDataTemplate`/`resourcesRefsTemplate` (its *data*, not just its action flags, is
  RBAC-narrowed and would leak the SA-maximal aggregate if shared), and — the #72 extension —
  any widget carrying an **inline + GET `resourcesRefs` item** (its server-resolved child
  `rendered` body is narrowed per user). Such widgets fall through to the per-binding key of
  ADR 0002. A transient accessor error de-classifies safely only because the resolver fails soft
  to static-only widgetData on the same error — the two sites must stay symmetric.
  Additionally, `shouldSkipEmptyWidgetShell` keeps transient-empty apiRef+template shells out of
  the shared cell at boot (a poison-shell guard, not an RBAC guard).
- On an `apistage` hit, the gate depends on the stage (#58):
  - **Non-UAF LIST** → `gateListItems` → `filterListByRBAC`
    (`internal/resolvers/restactions/api/informer_dispatch_rbac.go`): the requester's raw RBAC,
    per item. **GET-by-name** → `gateContentEnvelope` / `filterGetByRBAC`.
  - **UAF LIST** → served **un-narrowed** (`serveParsedListEnvelope`). This is deliberate, not a
    gap: a `userAccessFilter` stage runs with elevated privilege precisely because the requester
    *lacks* the raw RBAC; the per-user narrowing is the UAF refilter downstream
    (`refilter.go` — `applyUserAccessFilter`/`refilterSlice`/`evalSingle`), whose verb/resource/
    namespace derivation diverges from raw list-RBAC. Applying the raw gate here would strip the
    UAF-intended scope to near-empty and break UAF.
- More broadly, serve-time per-item RBAC gates run across the served bodies:
  `filterListByRBAC`/`filterGetByRBAC` on the informer dispatch path
  (`informer_dispatch.go`), `filterGetByRBAC` in `internal/objects/informer_serve.go` (consumed
  by `objects/get.go`), and the cluster-list resolver's `EvaluateRBAC(verb=list)` check
  (`cluster_list.go`). Every gate fails **closed** — a JQ error, an `EvaluateRBAC` error, or a
  deny **drops** the item (the UAF exemption above being the one privilege-inverting exception,
  which fails closed inside the refilter instead).

The architectural statement: **the cache holds content; identity is applied at serve time.** A
hit can never short-circuit a permission check — the dispatcher's `EvaluateRBAC` gate also runs
*before* the L1 lookup (`internal/handlers/dispatchers/restactions.go`), so the cell is consulted
only after the request itself is authorised.

## Consequences

- **One shared shell across all cohorts**, re-personalised per request — the expensive widget
  body (and the raw apistage envelope) is resolved once, not once-per-binding, without reopening
  the ADR 0002 leak.
- **The cache is RBAC-untrusted by design.** Even a correctly-keyed cell is re-checked per item
  at serve time, so a stale or over-broad cached body cannot leak: the gate drops anything the
  user lacks a grant for.
- **The exception is narrow and self-policing.** The rule is *identity-free in the key only if
  re-narrowed per-user at serve time*. The RBAC-sensitive-widget bypass is the guard that keeps
  a widget whose *data* (not just action flags) is identity-specific off the shared path.
- **Performance lever.** Serve-time per-item gates pass `SkipBindingUID=true`
  (`internal/rbac/evaluate.go`) to skip the CRB/RB stable-sort — a ~43% pod-CPU lever at 50K
  scale; correctness is unaffected because the sort only chooses *which* UID is returned, not
  the verdict.
- **Failure modes to watch.** If `gateWidgetEnvelope` stops re-stamping, if an apistage non-UAF
  LIST is served without `gateListItems` (the #58 over-serve regression: a narrow tenant received
  the full SA list), or if a third class is made identity-free without a serve-time gate, the
  shared cell leaks. The invariant: the identity-free key classes are exactly `widgetContent` and
  `apistage`, and both are always re-narrowed at serve time.
