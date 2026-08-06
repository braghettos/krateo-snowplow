---
type: Decision
title: "ADR 0002 — L1 resolved-cache cells are keyed per-binding, never cohort-only"
description: >-
  Identity-bound L1 cache cells fold the authorising RBAC binding's UID (plus,
  since #118, the requesting subject's RBAC sub-generation) into the cache key,
  so a narrowly-authorised user can never read a broadly-authorised user's
  cached bytes. Empty-identity cells are neither populated nor served.
resource: snowplow
tags:
  - adr
  - cache
  - rbac
  - security
status: diverged
timestamp: 2026-08-06T00:00:00Z
---

# ADR 0002 — L1 resolved-cache cells are keyed per-binding, never cohort-only

- **Status:** Accepted; two material amendments since (the `RBACSubGen` key fold and the
  empty-identity fail-closed hardening), plus one walk-back (apistage — see ADR 0003).
- **Related:** ADR 0003 (the identity-free exceptions, re-narrowed at serve time),
  ADR 0004 (the cache is provisional). Deep dives:
  [`docs/architecture/caching.md`](../architecture/caching.md),
  [`docs/architecture/rbac-uaf.md`](../architecture/rbac-uaf.md).

## Context

Snowplow serves Krateo frontend content out of an in-process L1 cache of pre-encoded `/call`
JSON (`internal/cache/resolved.go`). A cache hit skips the whole resolver and writes the stored
bytes back to the client. The L1 cell key (`cache.ComputeKey`) therefore decides **which request
reads which stored body** — and every byte that leaves the pod must be exactly what the
*requesting user* is permitted to see.

The tempting design is to key an identity-bound cell by content alone:
`(group, version, resource, namespace, name, page, extras)`. That is wrong, and the failure is a
direct cross-user RBAC leak:

- User A is broadly authorised (an admin, or the wildcard-CRB snowplow service account). A's
  `/call` resolves a list and writes a cell holding every row A may see.
- User B is narrowly authorised. B's `/call` computes the *same* content key, hits A's cell, and
  receives rows B has no grant for.

A pre-v4 implementation keyed cells by a **cohort hash** (`BindingSetHash`, the v3 `BindingsByGVR`
equivalence-class digest). Attempts to collapse identity-bound cells down to a cohort/content-only
key were reverted six times (the "6-revert" L1/RBAC retrospective): every collapse reopened the
leak, because two users in the *same nominal cohort* can still differ in the binding that actually
authorised the specific layer's GET.

## Decision

For the identity-bound entry classes — `restactions`, `widgets`, `raFullList` — the dispatch key
folds **two** identity terms (`ComputeKey`, `internal/cache/resolved.go`; the current key-schema
salt is `resolvedKeyVersion = "v6"`):

1. **`BindingUID`** — the UID of the first-match RBAC binding that authorised *this layer's GET*
   for *this request's identity*.
   - Derived by `dispatchCacheLookupKey` (`internal/handlers/dispatchers/helpers.go`) via a
     direct `rbac.EvaluateRBAC(verb=get, …)` call with `SkipBindingUID` left at its safe zero
     value, so the returned UID is the deterministic, stable-sorted first match.
   - Produced by `BindingUIDFromCRB` / `BindingUIDFromRB` (`internal/cache/match_subject.go`),
     prefixed `"C:<uid>"` for ClusterRoleBindings and `"R:<ns>/<uid>"` for RoleBindings so the
     two namespaces of UID never alias and RB scope is carried in the identifier. (Bindings with
     an empty apiserver UID — synthetic/test fixtures — fall back to a content-tuple hash under
     the same prefixes.)
   - A deny or evaluation error **fails closed** to `bindingUID == ""`. The biconditional
     `BindingUID == "" ⟺ (cache off) ∨ (deny / no snapshot)` is asserted both directions by the
     F7 invariant tests (`internal/rbac/evaltest/empty_binding_uid_invariant_test.go`).
2. **`RBACSubGen`** (#118 (c)/(c)-v2) — the requesting subject's *effective per-subject RBAC
   sub-generation* (`cache.RBACSubGenForSubject` over the user + every presented group + SA
   counters, bumped at snapshot-publish). A grant/revoke that touches this user's *own* bindings
   rotates the key → cold miss → fresh resolve → fresh userAccessFilter refilter. This closes the
   staleness window the single dispatch-GET `BindingUID` is blind to (a per-namespace grant the
   UAF refilter depends on), with blast radius proportional to the affected subjects — not the
   whole cache. The seed writes `RBACSubGen==0` (a warm-miss perf gap for moved-sub-gen
   subjects, ticketed; never an authz-staleness bug — the dispatch key stays correct).

This is **finer-grained** than the v3 cohort hash it replaced: two users granted by the *same*
binding share one cell; the *same* user authorised by *different* bindings on different layers
gets different cells.

**Value-dedup is a free consequence**, not a separate mechanism: because every member of a
`BindingUID` equivalence class resolves to byte-identical output, they all land on one cell. The
first writer's identity is carried on the entry as `RepresentativeUsername`/`Groups` — refresher
bookkeeping only, explicitly *excluded* from `ComputeKey`. Page-dedup layers on top via the
`raFullList` class, which caches the full unpaginated result once and slices per page at serve
time (`internal/cache/ra_full_list_slice.go`).

### The empty-identity cell is dead, not shared

Originally the `BindingUID == ""` row was treated as a shared "safe floor". The #95 security fix
hardened this in both directions: the seed and the customer/refresher Put paths **skip populating**
any cell whose re-derived `BindingUID` is `""` (the Put-gates in
`internal/handlers/dispatchers/restactions.go` / `widgets.go` / the seed's FIX-C skip), and the
serve path treats a `""`-derivation as a **cache miss** (`serveFromCacheEligible`,
`internal/handlers/dispatchers/helpers.go`) — falling through to a direct resolve under the
request's own identity. A `""`-deriving request therefore never reads *or* writes a shared cell;
deny/error collapses to the same behaviour as `CACHE_ENABLED=false` (transparent direct resolve,
ADR 0004).

### The apistage walk-back

The 0.30.242 design declared `apistage` identity-bound ("RBAC-narrowed at populate time").
That claim was **wrong as shipped** and was corrected by #58: the apistage *content* cell's key
inputs never populate the identity fields (`contentKeyInputs`,
`internal/resolvers/restactions/api/apistage.go`), so the cell is identity-invariant, populated
SA-maximal, and **must** be RBAC-narrowed at serve time. Apistage is therefore governed by
ADR 0003's identity-free-with-serve-time-gate rule, not by this ADR's per-binding rule. The
identity-bound classes today are exactly `restactions`, `widgets`, and `raFullList`.

## Consequences

- **No cross-user leak through the cache key.** A narrowly-authorised user computes a different
  key from a broadly-authorised one and can never land on the other's cell.
- **The cache is per-binding, not per-user.** Memory scales with the number of distinct
  authorising bindings, not with the user count — which is what makes the cache viable at
  50K compositions × 1000 users while staying leak-free.
- **Out-of-band RBAC changes rotate keys.** The `RBACSubGen` fold means a live grant/revoke is
  reflected on the affected subjects' next `/call` without a global flush. As an interim bound
  for the residual refilter dependency, cells whose RESTAction declares a `userAccessFilter`
  stage also carry a short TTL override (`UAF_RESOLVED_TTL_SECONDS`, #118 (d); `HasUAF` is
  entry bookkeeping, not key material).
- **The deliberate exceptions are serve-time-gated.** `widgetContent` and `apistage` are
  identity-free in the key and re-narrowed per request at serve time — see ADR 0003. The general
  rule stands: *an entry may be identity-free in the key only if it is re-narrowed per-user at
  serve time.*
- **A regression here is high-severity.** Symptom: a user sees rows they lack a grant for. First
  check: `ComputeKey` still folds `BindingUID` + `RBACSubGen` for the affected class; the F7
  tests; `compute_key_identity_invariants_test.go`; the `feedback_l1_per_user_keyed_never_cohort`
  history. Re-introducing a cohort/content-only key for an identity-bound class is the exact
  mistake this ADR exists to prevent.
- **Fail-closed floor.** Deny/error and cache-off all collapse to a direct, un-cached resolve
  under the request's own identity — never a shared cell.
