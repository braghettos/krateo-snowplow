# `extras` — per-request context

`extras` is a per-request JSON object you pass on the query string of a `/call`:

```
GET /call?resource=widgets&apiVersion=…&name=…&extras=%7B%22env%22%3A%22prod%22%7D
                                                        └── url-encoded {"env":"prod"}
```

It is parsed once per request by `util.ParseExtras` into a `map[string]any`
(`internal/handlers/util/extras.go:9-24`); a malformed value is a 400, an absent
value is the empty map. The same `extras` mechanism works for **both**
[`RESTAction`](restactions.md) and [`Widget`](widgets.md) dispatches.

Use it to parametrise a resolve at request time without minting a new CR — e.g.
pass a selected namespace, an environment, or a row id that the RESTAction's jq
(its `path`/`payload`) or a widget template reads.

---

## What it does, in three rules

### 1. It is the *base dict*; API results overwrite it

On a RESTAction resolve the run dict **starts as a deep copy of `extras`**, and
the API stage outputs are then written on top
(`internal/resolvers/restactions/api/resolve.go:228-230`):

```go
dict := map[string]any{}
if opts.Extras != nil {
    dict = maps.DeepCopyJSON(opts.Extras)
}
```

So precedence is **API/apiRef result > extras**: an extras key collides with a
stage output → the stage output wins. The widget side reaches the same ordering
from the opposite direction — `ds` already holds the apiRef result, and
`mergeExtras` only fills keys that are *absent*
(`internal/resolvers/widgets/resolve.go:348-358`):

```go
for k, v := range extrasCopy {
    if _, present := ds[k]; !present { ds[k] = v }
}
```

The pagination `slice` triple is treated like an API result — it also wins over
`extras` (`resolve.go:88`, `:96`).

### 2. It is input-only

`extras` seeds the resolve; it is **never written back to `status`**. A widget's
`status.widgetData` / `status.resourcesRefs` carry only resolved data, never the
raw `extras` object. Nothing in the resolvers copies `extras` into the emitted
status.

### 3. It is folded into the cache key

`extras` is part of every L1 cache key, so two requests that differ only in
`extras` land on **distinct cache entries** — no cross-request collision, and a
warm entry for `extras=A` is never served to a request carrying `extras=B`.

`ComputeKey` folds `extras` last, via `canonicaliseExtras`
(`internal/cache/resolved.go:679-688`, `:697`): a **recursively sorted-key JSON**
surrogate. Sorting means two requests with the same content but different map
iteration order hash to the *same* key (a cache hit), while different content
hashes to a different key (`resolved.go:694-729`). On a marshal failure (cyclic /
non-JSON value) it falls back to a deterministic `fmt.Sprintf("%v", …)` so the key
still varies with content (`resolved.go:683-687`). This applies to the widget L1
key, the RESTAction per-binding L1 key, and the identity-free `widgetContent` cell
alike (`extras` is one of `widgetContent`'s key components,
`resolved.go:652-660`).

---

## The full path

### RESTAction (direct `/call`)

```
/call?resource=restactions&extras={…}
   → restactions dispatcher: util.ParseExtras (restactions.go:79)
   → L1 key includes extras (restactions.go:126/134, ComputeKey resolved.go:679)
   → restactions.Resolve → api.Resolve: dict := DeepCopyJSON(extras) (api/resolve.go:228)
   → API stages overwrite dict; spec.Filter projects; status.Raw emitted
```

### Widget (`/call`)

```
/call?resource=widgets&extras={…}
   → widgets dispatcher: util.ParseExtras (widgets.go:64)
   → L1 keys include extras (widgets.go:138/173/182)
   → widgets.Resolve:
       apiRef → apiref.Resolve → restactions.Resolve (extras seeds the RA dict)
       mergeExtras(ds, extras)  ← non-overwriting; covers apiRef-less widgets too
       widgetDataTemplate jq + resourcesRefsTemplate jq evaluate against ds
```

The prewarm / seed / refresher callers never set `extras`, so a nil/empty
`extras` is a no-op everywhere (the `if opts.Extras != nil` gate and the
`len(extras) == 0` guard skip the copy), and a resolve without `extras` is
byte-identical to the pre-extras behaviour
(`api/resolve.go:228`, `resolve.go:349`).

---

## See also

- [`widgets.md`](widgets.md) — how `extras` reaches each widget template path.
- [`restactions.md`](restactions.md) — the RESTAction `spec.api` resolve `extras`
  seeds.
- [caching deep-dive](../docs/architecture/caching.md) §3.1 — `ComputeKey` /
  `canonicaliseExtras` in the full key structure.
