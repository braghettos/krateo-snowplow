# Design: snowplow `/rbac` — resolve a RESTAction to the RBAC it would need

> Status: **IMPLEMENTED (PR #44)** — `internal/handlers/rbac.go`,
> `internal/resolvers/restactions/api/inspect.go`. Audience: snowplow maintainers · Scope:
> **snowplow only**. This doc now describes the **as-built** endpoint; the consumer (core-provider)
> side lives in `core-provider/docs/design/apiref-rbac-generation.md`.
>
> ⚠️ **One known fix outstanding before merge** — see §4: the in-cluster verb is hardcoded `get`;
> collection paths (LIST) must emit `list`.
>
> The build **deviated from the original draft** (which proposed a verb-less, chart-inspector-identical
> `[]Resource`): it carries a **per-row `verb`** and a `{restaction, readSet}` wrapper, and fails loud
> with `422`. That deviation is *kept* — per-row verbs give precise least-privilege. Sections below
> reflect what shipped.

---

## 1. What to build, in one paragraph

A new **inspect-only** endpoint, `GET /rbac`, that takes a RESTAction reference plus a composition
context, **resolves the RESTAction's `api[]` stages WITHOUT dispatching them**, and returns the set
of Kubernetes resources its in-cluster calls would touch — in **exactly the same wire shape
chart-inspector's `/resources` returns**. The caller (core-provider) turns that into RBAC. snowplow
never performs the reads and never needs the caller's permissions to compute the answer.

## 2. Why snowplow

snowplow is the only component that can map a RESTAction `api[]` call to a concrete GVR, because the
call's `Path` is an arbitrary `${ jq }` template that only snowplow can evaluate (it owns the
resolver and the jq context), and only snowplow has the discovery client to turn a URI into a GVR.
core-provider has none of these. So the *enumeration* lives here; the *RBAC writing* lives in
core-provider.

## 3. The contract

### 3.1 Request (as built)

```
GET /rbac?apiRefName=<name>&apiRefNamespace=<ns>&extras=<url-encoded JSON>
```

Only `apiRefName` + `apiRefNamespace` are required (the RESTAction GVR is fixed:
`templates.krateo.io/v1`, `restactions`); the handler loads the RA fresh from the informer cache
each call (`objects.Get`, same loader `/call` uses). `extras` is the inline `apiRef.extras` merged
with the per-instance context (`compositionId`, `namespace`, `name`, `spec`) — the **same root**
`/call` evaluates `api[].path` against, so paths resolve identically to a live reconcile.

### 3.2 Response (as built) — `{restaction, readSet}`, verb per row

```go
type rbacResponse struct {
    RESTAction restActionRef `json:"restaction"`   // { name, namespace }
    ReadSet    []Resource    `json:"readSet"`       // canonical: deduped + sorted
}
type Resource struct {
    Group     string `json:"group"`
    Version   string `json:"version"`
    Resource  string `json:"resource"`              // plural, post-discovery
    Namespace string `json:"namespace,omitempty"`   // "" ⇒ cluster-scoped
    Name      string `json:"name,omitempty"`        // currently always "" (resource-granularity grant)
    Verb      string `json:"verb"`                   // per row — §4
}
```

**The verb is part of the contract** (unlike chart-inspector). snowplow knows whether each stage is
a `get` (single object), `list` (collection), or `uaf.Verb` (userAccessFilter), so it emits the
exact verb and the consumer grants precisely that. An empty `readSet` (every stage external) is a
valid `200` with `[]`.

### 3.3 Fail-loud = `422` (refuse partial)

- **`200`** + `{restaction, readSet}` → the RESTAction was **fully** enumerated.
- **`422`** + an error string **naming every unresolvable stage** → at least one stage could not be
  enumerated (see §4). **No `readSet` is returned** — a partial read-set would let the consumer grant
  incomplete RBAC and the projection would silently under-read (the exact bug this feature kills).
- `400` for missing `apiRefName`/`apiRefNamespace`; `500` for structural failure (cyclic `api[]`,
  SA `rest.Config` unavailable).
- `endpointRef` stages are simply **absent** from `readSet` (external — not an error). UAF stages are
  **present** (§4).

## 4. The per-stage algorithm

For each `api[]` stage of the referenced RESTAction, classify exactly as the existing dispatcher
does (`internal/resolvers/restactions/api/resolve.go:resolveStageEndpoint`, ~line 360), but **emit a
`Resource` instead of dispatching**:

Classified (and ordered) as in `inspect.go`:

| Stage | Action (as built) |
|---|---|
| `userAccessFilter` (UAF) — classified **first** | **emit** — one row per resolved plural, `verb = uaf.Verb` verbatim. The in-process re-filter consults the caller group's access, so the grant is required (this resolved the original open question). |
| `EndpointRef != nil` (non-UAF) | **omit** — external; the Endpoint Secret authenticates, not k8s RBAC |
| in-cluster (no `EndpointRef`, non-UAF) | **resolve → emit** (below) |

For the in-cluster case (`inspectInClusterStage`):

1. **Evaluate** `api[].Path` against the `extras`-only dict — reuse the resolver's path evaluation;
   never string-parse the template. Classify **every** request-option (an extras-driven per-namespace
   iterator produces several).
2. **Parse** the concrete URI with `cache.ParseAPIServerPathToDep(path) → (gvr, namespace, name, ok)`:
   - resource path → `{gvr, namespace}` + the object **`name`** (`""` for a collection);
   - bare group-discovery path (`/apis/<g>/<v>`, `/api/<v>`) → **no row** (anonymous-readable catalogue);
   - anything else (residual `${…}`, upstream-dependent, non-kube) → **unresolvable** (§3.3).
3. **Validate** the GVR via discovery (`validateGVR`).
4. **Emit** `Resource{gvr, namespace, verb}` where:
   - **`verb = "get"` if `name != ""`** (single object),
   - **`verb = "list"` if `name == ""`** (collection — the dominant label-selector case).

> ⚠️ **Fix outstanding (PR #44).** `inspectInClusterStage` currently destructures
> `gvr, ns, _, ok := cache.ParseAPIServerPathToDep(path)` — **discarding `name`** — and hardcodes
> `Verb:"get"` for every path. A collection GET is a Kubernetes **LIST** needing RBAC `list`, so
> label-selector reads (e.g. `?labelSelector=krateo.io/composition-id=…`) would be granted `get` and
> **403 at the first `/call`**. The `name` is already returned — the fix is:
> `verb := "list"; if name != "" { verb = "get" }`.

**Unresolvable cases → `422` (§3.3), naming the stage:**
- `dependsOn` path interpolating a *prior call's result* (not available without dispatching);
- a GVR segment templated from data not in `extras` (can't evaluate);
- a path that doesn't parse to a kube GVR (likely an external call missing its `endpointRef`);
- a discovery miss (GVR not served) — record; never invent a grant.

**Canonical output:** dedupe identical `(group, version, resource, namespace)` rows and sort, so an
unchanged RESTAction yields a byte-identical response (the caller hashes it into a digest; churn
there causes needless re-reconciles).

## 5. Identity & dispatch-free guarantee

- The endpoint runs under **snowplow's own ServiceAccount** for discovery
  (`internal/dynamic/sa_client.go:ServiceAccountEndpoint`, in-cluster apiserver). It is the same
  central-oracle posture chart-inspector has (its own in-cluster SA).
- It is strictly **dispatch-free**: it evaluates paths + does discovery, but **never performs the
  `api[]` reads**. Therefore it needs **none** of the caller group's permissions — which is what lets
  RBAC be generated *before* the first successful resolution.

## 6. Security

- `/rbac` is a privileged oracle: it will resolve *any* RESTAction it is asked about. The endpoint
  **must be access-controlled** so only core-provider can call it. NB: chart-inspector's `/resources`
  is today called with no token and its handler reads none; whether its HTTP server enforces auth
  (NetworkPolicy / mTLS / middleware) is **unverified** — do **not** copy an unauthenticated posture
  by default for `/rbac`.
- The response is read-only metadata (GVRs), but it *informs* a privilege grant downstream, so
  treat tampering on this path as a privilege-escalation vector.

## 7. Open questions (snowplow-side)

1. **UAF stages. ✅ RESOLVED — emitted.** The build emits one row per plural with `uaf.Verb`
   verbatim; the in-process re-filter does consult the caller group's access, so the grant is
   required. No longer open.
2. **`dependsOn` resolvability boundary.** How much of a chained path can be evaluated at inspect
   time (e.g. earlier stages with static/own-extras paths) before a stage becomes unresolvable?
   Define the cutoff that flips a 200 into a non-200.
3. **Namespace scope.** When a path's namespace is templated to the composition's own namespace, a
   namespaced row suffices; when it's a cluster-wide LIST (or cross-namespace), emit `namespace=""`.
   Confirm the parser distinguishes these.
4. **Request shape — GET vs POST.** chart-inspector uses `GET` + query params; `extras` can be large.
   Either keep `GET ?extras=<json>` (parity, snowplow's existing `/call` convention) or accept a
   `POST` body. Decide for parity vs ergonomics.
5. **Reuse vs fork of the resolver.** Phase 0 should reuse the existing path-evaluation +
   `Discover`; decide whether `/rbac` is a thin mode flag on the resolver run (no dispatch) or a
   separate lightweight pass.

## 8. Integration points (verified, this tree)

- `apis/templates/v1/core.go:33` — `type API` (`Path`, `Verb`, `EndpointRef`, `UserAccessFilter`, `DependsOn`).
- `internal/resolvers/restactions/api/resolve.go:360` — `resolveStageEndpoint` (the UAF / endpointRef /
  per-user classification to mirror, minus dispatch).
- `internal/dynamic/client.go:54,110` — `Discover(ctx, category) ([]GroupVersionResource, error)`.
- `internal/dynamic/sa_client.go:105` — `ServiceAccountEndpoint()` (snowplow's own in-cluster SA).

## 9. Out of scope (the core-provider half — for reference only)

These are **not** snowplow's job; they live in `core-provider/docs/design/apiref-rbac-generation.md`:
the verb policy (read set vs `*`), reusing `rbacgen` to build a Role/ClusterRole **bound to the group
`krateo:cdc:<resource>-<apiVersion>`**, the digest/undeploy lifecycle, watching the RESTAction, and
the per-instance-GVR union. snowplow's only contract is **request → `[]Resource` (or non-200)**.
