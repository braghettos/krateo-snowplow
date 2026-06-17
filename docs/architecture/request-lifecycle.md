# Request lifecycle: `/call` → dispatch → resolve → RBAC → serialize

How a single `/call` request becomes JSON the Krateo frontend renders. This
traces the *current* code at commit `a42b072`; every claim below cites a
`file:line` you can open. Where a verb-specific or cache-mode-specific branch
matters, it is called out — the path is not uniform.

snowplow resolves two CR kinds into JSON over `/call`: a `RESTAction`
(`templates.krateo.io/restactions`) emits raw, *unordered* data assembled from
one or more API stages; a `Widget` (`*.widgets.templates.krateo.io`)
canonicalizes data into the render-ready shape the frontend consumes. Every
other GVR falls through to a raw apiserver passthrough.

---

## 1. Data-flow diagram

```
                         HTTP GET/POST/PUT/PATCH/DELETE /call?apiVersion=&resource=&namespace=&name=&page=&perPage=&extras=
                                              │
            ┌─────────────────────────────────┴──────────────────────────────────┐
            │  main.go:797  mux.Handle("GET /call", chain.Append(                  │
            │     middleware.UserConfig(signKey, authnNS)   ← authn: JWT → UserInfo│
            │     cache.FallthroughScopeMiddleware(ScopeCallGeneric)               │
            │     handlers.Dispatcher(dispatchers.All()))   ← routes by GVR group  │
            │   .Then(handlers.Call()))                     ← fallthrough handler  │
            └─────────────────────────────────┬──────────────────────────────────┘
                                              │
                       proxy.go:11 Dispatcher: GET only; key by gv.Group
                       (restactions → "restactions."+group; widget → group)
                                              │
        ┌──────────────────────┬──────────────┴───────────────────┬──────────────────────────┐
        │ key in All() map?    │                                  │ NOT a GET, or no handler  │
        │ "restactions.        │ "widgets.templates.krateo.io"    │ for this group            │
        │  templates.krateo.io"│                                  │                           │
        ▼                      ▼                                  ▼                           ▼
  RESTAction()           Widgets()                          handlers.Call()             (write verbs:
  restactions.go:60      widgets.go:50                      call.go:63                    POST/PUT/PATCH/
        │                      │                            raw apiserver passthrough     DELETE skip the
        │                      │                            under the USER's own token    dispatcher entirely)
        │                      │                            RecordApiserverFallthrough
        ▼                      ▼                            (call.go:114)
  fetchObject (helpers.go:27) ─ objects.Get (objects/get.go:28)
   cache-on: informer-served + filterGetByRBAC (get.go:102)
   cache-off: getFromAPIServer under user token (get.go:159)
        │
        ▼
  checkDispatchRBAC  (helpers.go:93) — cache-on ONLY; gate the GET on the dispatch-target CR
   rbac.EvaluateRBAC (evaluate.go:167); deny → 403  (restactions.go:96 / widgets.go:90)
        │
        ▼
  L1 resolved-output cache lookup (cache-on)
   widgets: identity-FREE content cell FIRST (widgets.go:134, gated by isRBACSensitiveApiRefWidget)
            then per-binding-UID cell        (widgets.go:170, dispatchCacheLookupKey helpers.go:200)
   restactions: per-binding-UID cell         (restactions.go:123)
        │ HIT → write cached bytes (widgets content-hit re-runs serve-time UAF gate first)
        │ MISS ↓
        ▼
  RESOLVE
   RESTAction: restactions.Resolve (restactions.go:35) → api.Resolve (per-stage)
               per-stage userAccessFilter → refilter.go (per-item EvaluateRBAC, fail-closed)
               → spec.Filter jq projection → status.Raw
   Widget:    widgets.Resolve (widgets/resolve.go:38)
               apiRef → widgetDataTemplate → resourcesRefs (per-item rbac.UserCan → allowed flag)
               → resourcesRefsTemplate → CRD-schema validate status
        │
        ▼
  SERIALIZE  encodeResolvedJSON (helpers.go:390, json.Encoder, no indent)
   stageErrSink.Count()==0 ? Put into L1 + record dep edges : decline to cache (partial body still served)
        │
        ▼
  writeResolvedJSON (helpers.go:413) — Content-Type: application/json, 200, single buffered Write
```

For a widget served from the identity-free content cell the cached bytes are a
*shell*: `gateWidgetEnvelope` re-derives every
`status.resourcesRefs.items[].allowed` flag under the requester before the
bytes leave the pod (widget_content.go:469). The shell is never served verbatim.

---

## 2. Trace through the packages

### 2.1 Server wiring and routes — `main.go`

`main()` builds one `http.ServeMux` (main.go:769) and registers the full route
table:

- `GET /health` → `handlers.HealthCheck(...)` (main.go:774)
- `GET /readyz` → `handlers.ReadyCheck()` (main.go:775) — gated on Phase-1
  prewarm completion; returns 503 `{"status":"warming"}` until
  `cache.MarkPhase1Done()` fires (main.go:761-767 startup safety-net, and the
  `Phase1Warmup` goroutine at main.go:664-675).
- `GET /call` (main.go:797) — the canonical read path. Its middleware chain is
  `middleware.UserConfig(signKey, authnNS)` → `FallthroughScopeMiddleware(ScopeCallGeneric)`
  → `handlers.Dispatcher(dispatchers.All())`, finally `.Then(handlers.Call())`.
- `POST/PUT/PATCH/DELETE /call` (main.go:809-831) — same `UserConfig` middleware
  and a write-scope `FallthroughScopeMiddleware`, but **no** `Dispatcher` in the
  chain; they go straight to `handlers.Call()`.
- `GET /debug/vars` → `expvar.Handler()` (main.go:869), with snapshot/authz-memo
  expvars registered just before the mount (main.go:862, :868).
- `GET /debug/pprof/*` (main.go:837-841) registered on this mux directly (the
  server does not use `http.DefaultServeMux`).

The `http.Server` uses `WriteTimeout = 300s` (main.go:58, :898). That deadline is
anchored to request-read time, sized to clear the cache-OFF heavy compute path
(measured ~159s at 50K) before the handler's single buffered `Write`; cache-ON is
sub-4s and never approaches it (main.go:47-58).

### 2.2 Authentication — `middleware.UserConfig` (`internal/handlers/middleware/userconfig.go`)

`UserConfig` is snowplow's cache-aware sibling of plumbing's `use.UserConfig`,
pinned to a transcribed copy of plumbing's control flow (userconfig.go:13, :57).
It validates the JWT with `jwtutil.Validate(signingKey, token)` and installs
`WithAccessToken` / `WithUserInfo` / the per-user endpoint onto the request
context (userconfig.go:71, :104). Downstream the identity is read with
`xcontext.UserInfo(ctx)` (e.g. helpers.go:96, refilter.go:80) and the per-user
apiserver endpoint with `xcontext.UserConfig(ctx)` (call.go:80, get.go:170).

### 2.3 Dispatch — `handlers.Dispatcher` (`internal/handlers/proxy.go`)

`Dispatcher` (proxy.go:11) is a middleware, not a handler. It:

1. Passes any **non-GET** request straight to `next` (= `handlers.Call()`)
   without routing (proxy.go:14-17). Write verbs are therefore never resolved —
   they are raw apiserver passthroughs.
2. Parses `apiVersion` + `resource` query params into a GVR (proxy.go:21-36).
3. Computes the lookup key: `key = gv.Group`, except for `resource == "restactions"`
   where `key = "restactions." + gv.Group` (proxy.go:38-42). This is the "Hack
   caused by new Widgets handlers" — a widget CR's group *is*
   `widgets.templates.krateo.io`, so it keys on the group; a RESTAction's group is
   `templates.krateo.io`, so the `restactions.`-prefix disambiguates.
4. Looks the key up in the `dispatchers.All()` map (dispatchers.go:14-19):
   `"restactions.templates.krateo.io"` → `RESTAction()`,
   `"widgets.templates.krateo.io"` → `Widgets()`. On a hit it forwards to that
   handler; on a miss it falls through to `next` = `handlers.Call()` (proxy.go:44-51).

### 2.4 Fallthrough — `handlers.Call` (`internal/handlers/call.go`)

`handlers.Call()` (call.go:27) is the raw passthrough for every GVR not in the
dispatch map and for all write verbs. It validates the request (call.go:141),
builds an apiserver URI path (`/apis/<g>/<v>/namespaces/<ns>/<res>[/<name>]`,
call.go:194), then issues the call under the **user's own** endpoint via
`request.Do` (call.go:115) — RBAC is enforced inline by the apiserver. It records
`cache.RecordApiserverFallthrough(..., ReasonClientBuild, "")` *before* the call
so a panicking plumbing call still counts (call.go:114). The response is decoded
into a dict (list responses land under `items`, call.go:240-261) and JSON-encoded
to the wire (call.go:134).

### 2.5 The two dispatchers — `internal/handlers/dispatchers/{restactions,widgets}.go`

Both handlers are constructed once (`RESTAction()` restactions.go:22 /
`Widgets()` widgets.go:22) and capture the snowplow ServiceAccount transport pair
(`saEP`, `saRC`) at construction time, not per request — the per-request
`snowplowSACtx()` was found to serialize dispatches on the SA-singleton mutexes
(restactions.go:23-43). Out-of-cluster runs get `(nil, nil)` and skip the attach.

Both `ServeHTTP` methods follow the same skeleton:

1. `beginPerCall` / deferred `pcEmit` — structured `dispatcher.call.complete`
   timing log (restactions.go:70, per_call_log.go:39-53).
2. `defer markCustomerInFlight()()` — signals the prewarm engine to yield
   background work for the dispatch's duration (restactions.go:77, widgets.go:62).
3. `util.ParseExtras(req)` — parses the `?extras=<json>` query param into a
   `map[string]any` (restactions.go:79; util/extras.go:9).
4. `fetchObject(req)` — fetch the dispatch-target CR (helpers.go:27 →
   `objects.Get`, §2.6).
5. **RBAC dispatch gate** — cache-on only (§2.7).
6. **L1 cache lookup** — cache-on only (§2.8).
7. On miss, build the resolve context (attach SA transport, the L1 key, a
   stage-error sink), call the resolver, encode, conditionally cache, write.

### 2.6 Object fetch — `objects.Get` (`internal/objects/get.go`)

`fetchObject` (helpers.go:27) delegates to `objects.Get` (get.go:28). The fetch
is cache-mode-routed:

- **cache-off** (`cache.Disabled()`): `getFromAPIServer` under the user's own
  endpoint/token (get.go:51-52, :159). RBAC is enforced inline by the apiserver.
- **cache-on**: served from the in-process informer cache *iff* the GVR is
  servable (registered + `HasSynced`, get.go:90) **and** the requester passes
  `filterGetByRBAC` (get.go:102). A miss, a not-yet-synced informer, or an
  RBAC-denied GET all fall through to `getFromAPIServer` under the user's token,
  which returns the authoritative 403/404 (get.go:137-151).

When `ctx` carries an L1 key (`cache.WithL1KeyContext`), a successful Get records
a dependency edge `(GVR, ns, name)` so a later DELETE/ADD/UPDATE of that object
invalidates the L1 entry (get.go:39-41, :249-258).

### 2.7 The RBAC dispatch gate — `checkDispatchRBAC` (`helpers.go:93`)

In cache-on mode `objects.Get`'s informer branch bypasses the per-user token, so
the GET on the dispatch-target CR is re-checked explicitly. Both dispatchers call
`checkDispatchRBAC` and return **403** on deny (restactions.go:96-108,
widgets.go:90-100). It extracts `UserInfo`, calls `rbac.EvaluateRBAC` with verb
`get` and `SkipBindingUID: true` (it discards the binding UID), and returns the
allow bit; any error fails closed to deny (helpers.go:96-128). In cache-off mode
the gate is skipped — the per-user apiserver fetch in `objects.Get` already
enforced it.

### 2.8 L1 lookup keys

The dispatch-target lookup runs **strictly after** the RBAC gate so a cache hit
can never short-circuit the permission check (restactions.go:112-116).

- `dispatchCacheLookupKey` (helpers.go:200) builds the per-request key. It reads
  `UserInfo`; **a missing/unparseable identity makes the request uncacheable**
  (nil handle, helpers.go:205-209) — the resolve still runs, but nothing is read
  from or written to L1. Identity is folded as a **per-binding UID**: a direct
  `rbac.EvaluateRBAC(verb=get, …)` returns the first-match binding UID that
  authorized the GET, and that UID — *not* the literal username — is the
  `BindingUID` field in the key (helpers.go:211-228). Two users authorized by the
  same binding share the cell. `Username`/`Groups` are carried only as the
  refresher's `Representative*` re-resolve identity, **not** folded into
  `ComputeKey` (helpers.go:229-237, resolved.go:305-327).
- Widgets additionally try an **identity-free** content cell first
  (`dispatchWidgetContentKey`, helpers.go:147; widgets.go:134-164). Its key is
  `(gvr, ns, name, perPage, page, extras)` with username/groups omitted entirely
  (`CacheEntryClassWidgetContent`; resolved.go:131, :652). This shared cell is
  **skipped** for an apiRef-driven render widget classified RBAC-sensitive by
  `isRBACSensitiveApiRefWidget` (widgets.go:134, widget_content.go:348), because
  its `status.widgetData` aggregate is not narrowed at serve time and would leak
  cross-user. On a content-cell hit, `gateWidgetEnvelope` re-derives every
  `items[].allowed` flag under the requester (widgets.go:141; widget_content.go:469)
  before writing.

`extras` is folded into every L1 key via `canonicaliseExtras` (a sorted-key JSON
encoding) inside `ComputeKey` (resolved.go:680-718) — so distinct `extras` values
never collide on one cell.

### 2.9 Resolve — RESTAction (`internal/resolvers/restactions/`)

`restactions.Resolve` (restactions.go:35) runs the typed CR's `spec.api[]` stages
through `api.Resolve`, producing a `dict` of per-stage outputs. Each stage that
declares `userAccessFilter` is RBAC-filtered **per item** by the api package's
refilter (refilter.go). Then `spec.Filter` (if present) is a single jq projection
over the dict (restactions.go:58-68); otherwise the dict is marshalled as-is
(restactions.go:69-74). The result is written to `status.Raw` as a
`runtime.RawExtension` (restactions.go:77-79), with `last-applied-configuration`
and `managedFields` stripped (restactions.go:81-86).

The RESTAction emits *unordered* data. It contains **no widget-shaping logic** —
the layering boundary is asserted in the refilter package doc (refilter.go:16-18):
RBAC narrowing lives in the resolver/per-API-stage layer, never in widget
canonicalization.

### 2.10 Resolve — RBAC refilter (`internal/resolvers/restactions/api/refilter.go`)

`userAccessFilter` is the per-item RBAC filter for a SA-dispatched LIST. The list
is fetched under the snowplow SA, then narrowed to what the *requesting user* may
see (refilter.go:1-30). For each item:

- `NamespaceFrom` jq resolves the namespace (default `.metadata.namespace`,
  refilter.go:237-240).
- The resource-plural set is resolved once per dispatch — static `uaf.Resource`
  or jq-derived `ResourcesFrom` (refilter.go:303). An item is kept iff
  `rbac.EvaluateRBAC` permits the user for **any** resource in the set
  (OR-semantics, refilter.go:254-284).

Every failure mode fails **closed**: missing `UserInfo` drops all items
(refilter.go:80-90); an unresolvable resource set drops all items
(refilter.go:101-106); a jq error or an unrecognized item shape denies that item
(refilter.go:182-207, :241-248). The production callsite is
`applyUserAccessFilterOnPig` (refilter.go:428), which resolves `ResourcesFrom`
against the full resolver `dict` (so it can reference upstream stage outputs like
`.crds`) while refiltering the per-stage items.

### 2.11 Resolve — Widget (`internal/resolvers/widgets/resolve.go`)

`widgets.Resolve` (resolve.go:38) canonicalizes into the render-ready envelope,
in fixed phases (each wall-clocked, resolve.go:69-160):

1. `resolveApiRef` (resolve.go:175) — fetch the widget's apiRef RESTAction (under
   SA transport), yielding the data source `ds`. `extras` flows
   widget → apiref → restactions → api here (resolve.go:181-201).
2. `injectSlice(ds, perPage, page)` (resolve.go:88, :304) — re-inject the
   pagination triple stripped by the RA's `spec.Filter` projection.
3. `mergeExtras(ds, extras)` (resolve.go:96, :348) — fold extras into `ds`,
   **non-overwriting** (apiRef results and the slice triple win on collision);
   this also makes extras available to apiRef-less widgets.
4. `resolveWidgetData` → `status.widgetData` (resolve.go:99-112). A
   `widgetDataTemplate` read error **fails soft** to static-only data
   (resolve.go:220-224) — kept symmetric with `isRBACSensitiveApiRefWidget`'s
   de-classification so a read error never lands a SA-maximal aggregate in the
   shared identity-free cell (resolve.go:209-219).
5. `resolveResourceRefs` → `status.resourcesRefs.items` (resolve.go:115-148).
   Each ref gets an `allowed` flag from `rbac.UserCan` under the *resolving*
   identity (resourcesrefs/resolve.go:108).
6. `crdschema.ValidateObjectStatus` validates the status against the widget's CRD
   schema; a failure returns a 400 `StatusError` (resolve.go:159-170).

### 2.12 RBAC evaluator — `internal/rbac/`

`rbac.UserCan` (rbac.go:32) routes through `rbac.EvaluateRBAC` in cache-on mode
and through a SubjectAccessReview only in cache-off mode (rbac.go:25-44).

`EvaluateRBAC` (evaluate.go:167) is the in-process evaluator:

- **cache-off**: falls through to `UserCan` → SelfSubjectAccessReview, returns no
  binding UID (evaluate.go:171-183). This is the correctness baseline that keeps
  `CACHE_ENABLED=false` a transparent fallback.
- **cache-on**: reads a single coherent `*cache.RBACSnapshot`
  (evaluate.go:206); a nil watcher or nil snapshot **degrades to deny**, never to
  apiserver (evaluate.go:185-217) — the "zero SubjectAccessReview in cache-on"
  rule. A per-generation authz memo short-circuits the candidate walk on a hit;
  only **permits** are memoized — a deny can be transiently wrong under snapshot
  churn and must re-walk every call (evaluate.go:231-289).
- The walk (`evaluateAgainstInformerFirstMatch`, evaluate.go:334) checks
  ClusterRoleBindings first, then namespaced RoleBindings; first permitting rule
  wins (RBAC v1 has no deny rules). Candidate sets come from subject indexes
  (`selectCRBCandidates` / `selectRBCandidates`, evaluate.go:453, :507) and are
  post-filtered by `anySubjectMatches` (evaluate.go:694) for **User**, **Group**
  (including the implicit `system:authenticated` and SA synthetic groups), and
  **ServiceAccount** subjects symmetrically. `rulesPermit` honors verb/apiGroup/
  resource wildcards and `resourceNames` scoping (evaluate.go:597, :651).

### 2.13 Serialize and conditional cache write

Both dispatchers encode with `encodeResolvedJSON` (helpers.go:390) — a
`json.Encoder` with **no** indentation, deliberately matched between the cache-Put
bytes and the wire bytes so "cache-on warm == cache-off" holds (helpers.go:386-396;
note `handlers.Call` at call.go:135 does indent — that path is the raw passthrough,
not a resolver result). The write is a single buffered
`writeResolvedJSON` (helpers.go:413) — no streaming (helpers.go:402-412).

The L1 **Put** is gated on `stageErrSink.Count() == 0`: if any per-item stage
error fired during resolve, the partial body is still **served** (200) but **not
persisted**, so transient item failures self-heal on the next resolve
(restactions.go:211, :249-256; widgets.go:224, :266-271). On a clean resolve the
bytes are Put under the computed key and dependency edges are recorded — the
self-dep on the dispatch-target CR (restactions.go:280-281) and, for widgets, the
apiRef→RESTAction and render-eligible resourcesRefs deps
(`recordWidgetDeps`, widgets.go:291).

---

## 3. Invariants

1. **RBAC gate precedes the cache.** The L1 lookup is always after
   `checkDispatchRBAC`, so a cache hit can never bypass a permission check
   (restactions.go:112-116, widgets.go:166-167).
2. **Layering: RESTAction emits unordered data; the widget canonicalizes.** RBAC
   narrowing lives in the resolver / per-API-stage layer (refilter.go:16-18); the
   RESTAction has no widget-shaping logic.
3. **Per-binding-UID L1 keying — never identity-free for sensitive content.** The
   per-cohort cell keys on the authorizing binding UID, not username
   (helpers.go:211-228). The identity-free widget-content cell is used only for
   non-RBAC-sensitive widgets and is always passed through serve-time
   `gateWidgetEnvelope`, which re-derives `allowed` per requester
   (widgets.go:134, widget_content.go:469). A request with no identity is
   uncacheable (helpers.go:205-209).
4. **Cache-off is a transparent fallback.** `CACHE_ENABLED=false` makes
   `objects.Get`, `EvaluateRBAC`, and `UserCan` route to the apiserver /
   SubjectAccessReview under the user's own token (get.go:51, evaluate.go:171,
   rbac.go:35) — same data and RBAC, just slower. No dispatch RBAC gate runs
   because the per-user fetch already enforced it (restactions.go:96).
5. **Fail-closed everywhere RBAC is evaluated.** Missing identity, evaluator
   error, nil snapshot, unparseable item shape → deny / drop, never allow-all
   (refilter.go:80, :182-207; evaluate.go:185-217; widget_content.go:526-559).
6. **Serve bytes == cache bytes.** Resolver responses are encoded identically on
   the Put path and the write path (helpers.go:386-396).
7. **Only clean resolves are cached.** A per-item stage error serves the partial
   body but declines the Put (restactions.go:249, widgets.go:266).

---

## 4. Known failure modes

| Symptom | Likely cause | Where it surfaces in code |
|---|---|---|
| `/call` returns 403 for a widget/RESTAction the user expects | `checkDispatchRBAC` denied the GET on the dispatch-target CR (cache-on); or in-process snapshot not yet built (degrade-to-deny) | restactions.go:96-108, widgets.go:90-100; evaluate.go:207-217 |
| All list items vanish for a narrow-RBAC user | `userAccessFilter` fail-closed: missing `UserInfo`, unresolvable `ResourcesFrom`, or a `NamespaceFrom` jq error dropped every item | refilter.go:80-90, :101-106, :241-248 |
| Widget panel renders 0 rows for one user, full for admin | working as designed: `gateWidgetEnvelope` set `allowed=false` per requester; the frontend renders only `allowed==true` items | widget_content.go:449-507 |
| `/call` hangs then client gets HTTP 0 under cache-OFF at scale | heavy compute exceeded `WriteTimeout`; the t0-anchored 300s deadline is the bound | main.go:47-58, :898 |
| Cross-user data leak from a shared widget cell | an RBAC-sensitive apiRef widget incorrectly routed to the identity-free content cell — the `isRBACSensitiveApiRefWidget` / `resolveWidgetData` error-direction symmetry was broken | widgets.go:134; resolve.go:209-224; widget_content.go:348 |
| Stale content after an object change | dependency edges not recorded (Get ran without an L1 key in ctx, or cache off); TTL is the outer safety net | get.go:39-41, :249-258; restactions.go:268-281 |
| Cache never serves; everything hits apiserver | cache disabled, informer not servable (`!HasSynced`), or a nil/passthrough/metadata-only watcher | get.go:51, :80-90; evaluate.go:185-198 |
| `/readyz` stuck `{"status":"warming"}` | Phase-1 prewarm never flipped `Phase1Done`; the startup safety-net's 3-disjunct guard should cover cache-off | main.go:761-767, :664-675 |

---

## 5. Notes where code diverged from the documentation plan

These are flagged because the plan's prose did not match the current tree:

- **"per-user L1 keying."** The plan (§3 ADR-0002, §4) describes the L1 cell as
  *per-user*-keyed. The current code keys the per-cohort cell on the **first-match
  binding UID** returned by `EvaluateRBAC` (`BindingUID`, helpers.go:211-228),
  with username/groups deliberately removed from `ComputeKey`
  (resolved.go:256-269, :305-327). Username/groups survive only as the refresher's
  `Representative*` re-resolve identity. "Per-binding-UID" is the accurate
  description; "per-user" is correct only in the sense that the key is still
  identity-derived and never shared across permission boundaries.

- **`handlers.Call` is the fallthrough, not the resolver entry.** The plan frames
  `/call` as "dispatcher → resolver." In code the `handlers.Call()` handler
  (call.go:27) is the **raw apiserver passthrough** for unmatched GVRs and for all
  write verbs; the resolvers are reached only through the `Dispatcher` middleware's
  `All()` map (proxy.go:44, dispatchers.go:14). Write verbs bypass the dispatcher
  entirely (proxy.go:14-17) and are never resolved.

- **`internal/handlers/dispatchers/` is not a thin layer.** The plan's anchor list
  treats it as the dispatch site; in practice this package also owns the entire L1
  key/lookup/Put logic, the prewarm engine, and the background refresher. The pure
  routing decision is the small `proxy.go` middleware in `internal/handlers/`, not
  in the `dispatchers/` subpackage.

- **Widget serialization indents differently from the passthrough.**
  `encodeResolvedJSON` (resolver path) emits compact JSON deliberately for
  cache-byte parity (helpers.go:390); `handlers.Call` (passthrough) emits
  two-space-indented JSON (call.go:135). The two `/call` response shapes are not
  byte-identical across the resolver vs. passthrough lanes.
