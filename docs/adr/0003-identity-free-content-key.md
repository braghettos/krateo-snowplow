# ADR 0003 — Identity-free widget content key + serve-time UAF

- **Status:** Accepted
- **Related:** ADR 0002 (the general per-binding keying rule this is the sanctioned exception to).
  Deep dives: [`docs/architecture/caching.md`](../architecture/caching.md),
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
recomputed for each request before the bytes are written.

## Decision

Snowplow uses two complementary mechanisms.

### 1. The identity-free content key (`widgetContent`)

`widgetContent` (`resolved.go:147`) is the **only** entry class for which `ComputeKey` skips the
identity fold (`resolved.go:652`). Its key is built by `dispatchWidgetContentKey`
(`helpers.go:147`) with Username/Groups left zero, so it is keyed only on
`(gvr, ns, name, perPage, page, extras)`. An admin and a narrow-RBAC user hit the **same cell**.

The stored body is a **shell**: `status.resourcesRefs.items[].allowed` flags are present as the SA
walker evaluated them, but the shell is **never served verbatim**.

### 2. Serve-time UAF (the cache is not trusted for RBAC)

Results are re-filtered against the requesting user *at serve time*, never trusted from the cache:

- On a `widgetContent` hit, `gateWidgetEnvelope` (`widgets.go:139-153`) **overwrites every
  `allowed` flag** under the request's own identity (via `rbac.UserCan`) before serialisation. The
  body is shared; the bytes that leave the pod are per-user.
- This path is **bypassed entirely** for RBAC-sensitive apiRef widgets — those whose
  `status.widgetData` is RBAC-narrowed and would leak the SA-maximal aggregate if shared
  (`isRBACSensitiveApiRefWidget`, `widgets.go:134`). Such widgets fall through to the per-binding
  key of ADR 0002.
- More broadly, serve-time UAF is a per-item gate applied across *all* served bodies, not just
  widgets. Every site calls `EvaluateRBAC(…, SkipBindingUID=true)` and keeps an item only if
  `allowed`: `refilter.go` (`applyUserAccessFilter`/`refilterSlice`/`evalSingle`),
  `informer_dispatch_rbac.go` (`filterListByRBAC`/`filterGetByRBAC`),
  `cluster_list.go`, and `objects/informer_serve.go`. Every one fails **closed** — a JQ error, an
  `EvaluateRBAC` error, or a deny **drops** the item.

The architectural statement: **the cache holds content; identity is applied at serve time.** A
hit can never short-circuit a permission check — the dispatcher's `EvaluateRBAC` gate also runs
*before* the L1 lookup (`restactions.go:96-116`), so the cell is consulted only after the request
itself is authorised.

## Consequences

- **One shared widget shell across all cohorts**, re-personalised per request — the expensive
  widget body is resolved once, not once-per-binding, without reopening the ADR 0002 leak.
- **The cache is RBAC-untrusted by design.** Even a correctly-keyed cell is re-checked per item
  at serve time, so a stale or over-broad cached body cannot leak: the gate drops anything the
  user lacks a grant for.
- **The exception is narrow and self-policing.** The rule is *identity-free in the key only if
  re-narrowed per-user at serve time*. The RBAC-sensitive-widget bypass (`widgets.go:134`) is the
  guard that keeps a widget whose *data* (not just action flags) is identity-specific off the
  shared path.
- **Performance lever.** Serve-time per-item gates pass `SkipBindingUID=true` to skip the
  CRB/RB stable-sort — a ~43% pod-CPU lever at 50K scale (`evaluate.go:107-111`, `:153-166`);
  correctness is unaffected because the sort only chooses *which* UID is returned, not the verdict.
- **Failure mode to watch.** If `gateWidgetEnvelope` stops re-stamping, or a non-`widgetContent`
  class is made identity-free, the shared shell leaks. The invariant is: the only identity-free
  key class is `widgetContent`, and it is always re-stamped (`widgets.go:139-153`,
  rationale at `resolved.go:127-147`).
