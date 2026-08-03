# snowplow — architecture

The single canonical map of snowplow. Everything else hangs off this file; the deep dives under
[`docs/architecture/`](docs/architecture/) trace each subsystem through the code at `file:line`.

> **Read this as a map, not the territory.** Every claim here is traced to the deep dives, which are
> traced to the current tree. If a deep dive and this page disagree, the deep dive (and the code it
> cites) wins — fix this page.

## What snowplow is

A **content bridge**: it resolves `RESTAction` and frontend `Widget` custom resources into the JSON
the Krateo frontend renders, served over **`/call`**. It is not a BFF and holds no product state —
it composes content on demand from Kubernetes CRs and serves it to whatever client (the SPA, or
`curl`) reads it. Server wiring and routes live in `main.go` (`/call`, `/health`, `/readyz`,
`/debug/vars`, `/debug/pprof`).

## The request path (→ [request-lifecycle.md](docs/architecture/request-lifecycle.md))

```
GET /call ─▶ Dispatcher middleware ─▶ dispatcher (restactions | widgets) ─▶ resolve
            (internal/handlers/proxy.go)   (internal/handlers/dispatchers/)   (internal/resolvers/)
                                                    │
                                       RBAC gate + serve-time filter ─▶ serialize ─▶ write
```

- **GET** verbs flow through the `Dispatcher` middleware (`internal/handlers/proxy.go`) into a
  resolver. **Write** verbs bypass dispatch entirely and are a raw apiserver passthrough
  (`internal/handlers/call.go`) — never resolved, never cached.
- **Resolver layering contract (invariant):** a `RESTAction` emits *unordered* data; the *widget*
  canonicalizes and shapes it. RESTAction carries **no widget-shaping logic**. Equivalence is
  asserted at widget props.

## The three-tier cache (→ [caching.md](docs/architecture/caching.md))

```
L3 informer cache  ─▶  L1 resolved-entry cache  ─▶  dispatcher cache
(internal/cache/watcher.go)   (internal/cache/resolved.go)   (internal/handlers/dispatchers/)
```

- A widget has **two** L1 keys: an **identity-free content key** (`dispatchWidgetContentKey`,
  username/groups zeroed) and an **identity-bound key** that folds the **first-match RBAC
  `BindingUID`** (`dispatchCacheLookupKey` → `rbac.EvaluateRBAC`). Keys are a versioned SHA-256
  (`resolved.go ComputeKey`, `resolvedKeyVersion="v4"`).
  > **Correction over older docs/notes:** the identity-bound key is **per-binding-UID**, *not*
  > "per-cohort". The pre-`v4` per-cohort `BindingSetHash` was replaced by per-binding `BindingUID`.
  > Per-binding sharing is the equivalence class that still satisfies "never cohort-only" (below).
- **Invalidation:** `DELETE` evicts only the deleted object's own self-representation; LIST-deps and
  dependent GET-deps are dirty-marked; `ADD`/`UPDATE` dirty-mark (never evict). Values are
  deduplicated by equivalence class.

## Prewarm (→ [prewarm.md](docs/architecture/prewarm.md))

- Boot seed (`RegisterMetaQuerySeeds`) + a **phase-1 walker that replays frontend navigation**:
  roots are read from the frontend ConfigMap (`navmenus` INIT + `routesloaders` ROUTES_LOADER, not
  hardcoded), recursing `status.resourcesRefs` **only where `verb==GET`**.
- Cohort model is **dynamic** — no static cohort list, no lazy cold-fill.
- **The full prewarm *engine* is opt-in** (`PREWARM_ENGINE_ENABLED=false` by default); with it off,
  the legacy global-cohort seed path runs. CRUD re-prewarm is two mechanisms (object CRUD →
  refresher re-resolve; new-GVR → engine re-walk); widget-CR-triggered subtree re-walk is currently
  a **placeholder**.

## RBAC & user-aware filtering (→ [rbac-uaf.md](docs/architecture/rbac-uaf.md))

- L1 is **keyed per binding-UID, never cohort-only** — this is the load-bearing invariant (a
  6-revert retrospective); cohort-only keying leaks one user's resources to another.
- An **RBAC subject index** (`internal/cache/rbac_snapshot.go`) selects candidate bindings;
  `EvaluateRBAC` walks ClusterRoleBindings then RoleBindings with **predicate symmetry** across
  User / Group / ServiceAccount subjects.
- **Serve-time UAF:** results are re-filtered against the requesting user at serve time (multiple
  sites in resolvers/objects/dispatchers), not trusted from the cache alone.

## North-star (→ [north-star.md](docs/architecture/north-star.md))

- **Aspirational contract:** 1s cold / 500ms warm / 1s fresh at 50K compositions × 1000 users.
- **Measured as real Chrome page-load** (`e2e/bench/.../browser.py` `waterfallMs` over `/call`
  resource entries), **mix-weighted 0.95 narrow-RBAC + 0.05 admin** (`ledger.py`) — NOT `curl` p50.
- **Enforced gate today is 2200ms cold / 1000ms warm / 30000ms fresh** (`ledger.py` tier constants),
  relaxed-on-proof: server compute is sub-millisecond; the residual is SPA fan-out (frontend-scoped).
  The 1s/500ms figure is the goal, not the current gate.

## The provisionality invariant (load-bearing)

All caching is **provisional and cleanly removable**. `CACHE_ENABLED=false`
(`internal/cache/cache.go` `Disabled()`) is a **transparent fallback** to the direct apiserver —
same data, same UI, same RBAC, just slower — not a degraded mode. Caching must never constrain the
RESTAction/widget contract, and must be removable wholesale when Kubernetes ships better caching.

## Package map

| Package | Role | Deep dive |
|---|---|---|
| `main.go` | server wiring, routes, pprof/expvar | [request-lifecycle](docs/architecture/request-lifecycle.md) |
| `internal/handlers/`, `internal/handlers/dispatchers/` | dispatch, L1 keys/lookup/put, prewarm, refresher | [request-lifecycle](docs/architecture/request-lifecycle.md), [caching](docs/architecture/caching.md), [prewarm](docs/architecture/prewarm.md) |
| `internal/resolvers/` (`restactions/`, `widgets/`) | resolve CRs → content; the layering contract | [request-lifecycle](docs/architecture/request-lifecycle.md) |
| `internal/cache/` | L3 informer + L1 resolved cache, keying, invalidation, dedup, prewarm engine | [caching](docs/architecture/caching.md), [prewarm](docs/architecture/prewarm.md) |
| `internal/rbac/` | RBAC evaluation, subject index, authz memo | [rbac-uaf](docs/architecture/rbac-uaf.md) |
| `internal/objects/`, `internal/dynamic/` | object fetch, cached dynamic client | [request-lifecycle](docs/architecture/request-lifecycle.md) |
| `apis/templates/` | the `RESTAction` / `Widget` CRD types | [restactions.md](howto/restactions.md), [widgets.md](howto/widgets.md) |

## For agents

The Krateo `krateo-snowplow-agent` grounds in these docs at the **deployed version's tag** — see
[`docs/llms.txt`](docs/llms.txt) for the index and the version-pinned retrieval procedure. The
deployment/CRD/wiring view lives in the chart repo (`braghettos/krateo-snowplow-chart` `docs/`);
this repo's docs are the internals/runtime view.
