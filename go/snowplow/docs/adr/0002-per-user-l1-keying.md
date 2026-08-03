# ADR 0002 — L1 resolved-cache cells are keyed per-binding-UID, never cohort-only

- **Status:** Accepted
- **Related:** ADR 0003 (the one identity-free exception, re-narrowed at serve time),
  ADR 0004 (the cache is provisional). Deep dives:
  [`docs/architecture/caching.md`](../architecture/caching.md),
  [`docs/architecture/rbac-uaf.md`](../architecture/rbac-uaf.md).

## Context

Snowplow serves Krateo frontend content out of an in-process L1 cache of pre-encoded `/call`
JSON (`internal/cache/resolved.go`). A cache hit skips the whole resolver and writes the stored
bytes back to the client. The L1 cell key (`ComputeKey`, `resolved.go:608`) therefore decides
**which request reads which stored body** — and every byte that leaves the pod must be exactly
what the *requesting user* is permitted to see.

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

For every identity-bound entry class — `restactions`, `widgets`, `apistage`, `raFullList` —
`ComputeKey` folds the **`BindingUID`** into the key (`resolved.go:652-655`): the UID of the
first-match RBAC binding that authorised *this layer's GET* for *this request's identity*.

- The UID is derived by `dispatchCacheLookupKey` (`helpers.go:200`) via a direct
  `EvaluateRBAC(verb=get, …)` call with `SkipBindingUID` left at its safe zero value, so the
  returned UID is the deterministic, stable-sorted first match (`helpers.go:211-228`).
- The UID itself is produced by `BindingUIDFromCRB` / `BindingUIDFromRB`
  (`match_subject.go:83` / `:107`), prefixed `"C:<uid>"` for ClusterRoleBindings and
  `"R:<ns>/<uid>"` for RoleBindings so the two namespaces of UID never alias and RB scope is
  carried in the identifier.
- A deny or evaluation error **fails closed** to `bindingUID == ""` (`helpers.go:197-199`) — the
  same empty-identity row that `CACHE_ENABLED=false` produces. The biconditional
  `BindingUID == "" ⟺ (cache off) ∨ (deny / no snapshot)` is asserted both directions by the F7
  invariant tests (`evaltest/empty_binding_uid_invariant_test.go`).

This is **finer-grained** than the v3 cohort hash it replaced: two users granted by the *same*
binding share one cell; the *same* user authorised by *different* bindings on different layers
gets different cells (`resolved.go:256-267`, `:284-303`).

**Value-dedup is a free consequence**, not a separate mechanism: because every member of a
`BindingUID` equivalence class resolves to byte-identical output, they all land on one cell
(`resolved.go:312-320`). Page-dedup layers on top via the `raFullList` class, which caches the
full unpaginated result once and slices per page at serve time (`ra_full_list_slice.go`).

## Consequences

- **No cross-user leak through the cache key.** A narrowly-authorised user computes a different
  key from a broadly-authorised one and can never land on the other's cell.
- **The cache is per-binding, not per-user.** Memory scales with the number of distinct
  authorising bindings, not with the user count — which is what makes the cache viable at
  50K compositions × 1000 users while staying leak-free.
- **One deliberate exception.** `widgetContent` is the only identity-free class, and only because
  its served body is re-narrowed per request at serve time — see ADR 0003. The general rule:
  *an entry may be identity-free in the key only if it is re-narrowed per-user at serve time.*
- **A regression here is high-severity.** Symptom: a user sees rows they lack a grant for. First
  check: `ComputeKey` still folds `BindingUID` for the affected class (`resolved.go:652-655`); the
  F7 tests; the `feedback_l1_per_user_keyed_never_cohort` history. Re-introducing a cohort/content-
  only key for an identity-bound class is the exact mistake this ADR exists to prevent.
- **The empty-UID row is the safe floor.** Deny/error and cache-off all collapse to the same
  empty-identity cell shape, so the fail-closed path is identical to the transparent fallback
  (ADR 0004).
