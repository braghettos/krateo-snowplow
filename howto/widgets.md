# Understanding the `Widget` Custom Resource

**API Group:** `*.widgets.templates.krateo.io`
**Resource:** `widgets`
**Resolver:** `internal/resolvers/widgets/resolve.go`

A `Widget` is the bridge between a frontend-defined layout and live cluster
data. snowplow resolves a widget on demand over `/call` and writes a render-ready
`status` the Krateo frontend reads directly. The widget *canonicalizes* data into
the shape the frontend renders; the data itself comes from a [`RESTAction`](restactions.md)
(via `apiRef`) and/or from static `spec` fields.

> See also: [`extras.md`](extras.md) (per-request context), the
> [request lifecycle](../docs/architecture/request-lifecycle.md) deep-dive, and
> the [caching](../docs/architecture/caching.md) deep-dive (the two widget L1
> keys + per-binding-UID keying).

---

## The `spec` contract

A widget's `spec` carries four resolver-facing fields plus the
frontend-authored `widgetData`. The resolver runs them in a fixed order
(`resolve.go:38-173`):

| `spec` field | Type | What the resolver does |
|---|---|---|
| `apiRef` | object ref | Fetches a `RESTAction` and resolves it into the widget's **data source** (`ds`). `GetApiRef` defaults `resource: restactions` + `apiVersion: templates.krateo.io/v1` (`widgets.go:82-100`). |
| `widgetData` | object | The frontend-authored static base of `status.widgetData`. Read by `GetWidgetData` (`widgets.go:60-66`). |
| `widgetDataTemplate` | `[]{forPath, expression}` | jq expressions evaluated against `ds`; each result is written into `status.widgetData` at `forPath`. Type `WidgetDataTemplate` (`apis/templates/v1/widgetdatatemplate.go`). |
| `resourcesRefs.items` | `[]ResourceRef` | Static action references; each is RBAC-checked and emitted with an `allowed` flag. Type `ResourceRef` (`apis/templates/v1/resourcesrefs.go:11`). |
| `resourcesRefsTemplate` | `[]{iterator, template}` | jq-templated action references expanded against `ds`, then merged with the static `resourcesRefs`. Type `ResourceRefTemplate` (`apis/templates/v1/resourcesrefstemplate.go`). |

### `apiRef`

Points at a `RESTAction`; an empty `name`/`namespace` is a no-op that yields an
empty data source (`apiref/resolve.go:26-28`). The referenced RESTAction is
resolved under snowplow's ServiceAccount, and its output becomes the top-level
`ds` the templates below evaluate against. Pagination (`?page` / `?perPage`) and
`?extras` flow `widget → apiRef → RESTAction` (`resolve.go:181-201`).

A widget needs no `apiRef`: a static widget (only `widgetData` + `resourcesRefs`)
resolves against an empty `ds` (still seeded with `extras` — see below).

### `widgetDataTemplate`

```yaml
spec:
  widgetData:
    title: ""
  widgetDataTemplate:
    - forPath: title
      expression: .getDeployment.metadata.name   # jq over ds
```

Each entry's `expression` is evaluated against `ds`; the result is set into the
static `widgetData` at `forPath` (`resolve.go:226-258`). A *read* error on
`widgetDataTemplate` **fails soft** to the static-only `widgetData`
(`resolve.go:220-224`) — a load-bearing invariant kept symmetric with the cache
routing predicate so a read error can never land a ServiceAccount-maximal
aggregate in the shared identity-free cell (`resolve.go:209-219`).

### `resourcesRefs` and `resourcesRefsTemplate`

Both produce `ResourceRef`s that become `status.resourcesRefs.items`. Static
`resourcesRefs.items` are read verbatim (`widgets.go:102-114`);
`resourcesRefsTemplate` entries are jq-expanded against `ds` (optionally over an
`iterator`) and appended (`resolve.go:275-286`). Every resolved ref is then
turned into a result with:

- a `path` — a `/call` URL for the action (`resourcesrefs/resolve.go:151-172`),
- a `verb`, and
- an **`allowed`** flag from `rbac.UserCan` under the requesting identity
  (`resourcesrefs/resolve.go:108-112`).

The frontend renders only `allowed == true` actions.

---

## The `status` the frontend consumes

`Resolve` writes these `status` keys (`resolve.go:114`, `:151`, `:159`):

| `status` key | Shape | Source |
|---|---|---|
| `status.widgetData` | object | static `widgetData` + `widgetDataTemplate` results |
| `status.resourcesRefs.items[]` | `[]{id, path, verb, payload?, allowed}` | `resourcesRefs` + `resourcesRefsTemplate`; type `ResourceRefResult` (`resourcesrefs.go:29`) |
| `status.resourcesRefs.slice` | `{perPage, page, continue}` | added only when the request is paginated (`resolve.go:131-142`) |
| `status.error` | string | set on any resolve failure (`resolve.go:74`, `:103`, `:118`, `:162`) |

The final `status` is validated against the widget's own CRD schema before it is
returned; a schema failure is a 400 `StatusError` (`resolve.go:159-170`).

---

## `extras` — per-request context

`?extras={…}` (URL-encoded JSON) is per-request context usable by **every** path
in a widget resolve (full story in [`extras.md`](extras.md)):

- the `apiRef` RESTAction's own jq (its `path` / `payload`) can reference
  `extras` keys — extras seed the RESTAction resolve dict
  (`restactions/api/resolve.go:228-230`);
- `widgetDataTemplate` **and** `resourcesRefsTemplate` jq can reference `extras`
  keys — `mergeExtras` folds them into `ds` (`resolve.go:96`, `:348-358`);
- **apiRef-less** widgets get `extras` too — `mergeExtras` is the only thing that
  puts them into `ds` when there is no apiRef result (`resolve.go:331-337`).

`extras` is **input-only** (never echoed to `status`) and **non-overwriting**:
any apiRef-result key, or the pagination `slice` triple, wins on a key collision
(`resolve.go:348-358`). `extras` is also folded into the widget's L1 cache key, so
distinct extras resolve to distinct cache entries (`cache/resolved.go:679-688`).

---

## Resolve order (summary)

`apiRef` → `injectSlice` → `mergeExtras` → `widgetDataTemplate` →
`resourcesRefs(+Template)` → CRD-schema validate (`resolve.go:69-170`). Each phase
is wall-clocked for the seed-path timing log.

Refer to the [Krateo Widgets documentation](https://github.com/krateoplatformops/frontend/blob/main/docs/docs.md)
for the frontend-side widget catalogue.
