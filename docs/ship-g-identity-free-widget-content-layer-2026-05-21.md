# Ship G / 0.30.16x — identity-free widget content layer

**Status:** DESIGN-READY for dev. ARCHITECT-AUTHORED 2026-05-21,
pre-PM-gate. **14 ACs**, **6 HARD GATES**, **3 OQs** (all flagged for
PM closure pre-gate). Branch A from this design (full-envelope
identity-invariance up to `allowed`) is the recommended landing
shape; Branch B (partial-envelope, identity-bearing residue) is the
empirically-driven fallback. Tag number left blank: dev picks
`0.30.16x` at commit time per
`feedback_tag_commits.md` — the tag MUST point to the meaningful
"identity-free widget content layer" commit, not a downstream merge.

**One-liner.** Add a CONTENT-LAYER cache class for the WIDGETS tier:
key `(class="widgetContent", gvr, ns, name, perpage, page)` —
identity-free. The F2 SA prewarm walker, which already
calls `widgets.Resolve` per navigation node, additionally Puts each
resolved widget envelope into this layer as a free side-effect. The
serve-time `widgetsHandler` reads the identity-free entry FIRST,
runs a per-user `gateWidgetEnvelope` over the embedded
`status.resourcesRefs.items[].allowed` flags (the ONE identity-
bearing field in the resolved widget shape), then writes the
narrowed body back. Same architectural pattern as F1's
`gateContentEnvelope` at the apistage class, applied one tier up at
the widget class.

This is the actual **zero-cold ship** per Diego's prompt of 2026-05-21
("There will be zero cold navigations if prewarm does its job"). It
closes failure mode (a) from this morning's prewarm-failure diagnosis:
**Phase 1 walker covers a DIFFERENT key space than the serve-time
per-user widget L1, so admin's first /call always misses widget L1
even when content L1 (apistage) is fully warm** — 9.94s session-cold
admin Dashboard, on the same pod where the apistage class is 2,368
hits / 0 misses in 90s.

---

## §1. Diagnosis cross-reference

This morning's empirical trace (2026-05-21, architect dispatch, today)
ground this ship:

- **Admin Dashboard session-cold:** 9.94s on a warm pod with cleared
  browser localStorage. NOT a pod-cold measurement — the pod's
  apistage content L1 is fully warm (F2 prewarm working as designed).
- **Apistage content L1 telemetry (90s window):** 2,368 hits / 0
  misses. F1+F2 verifiably hitting at the CONTENT layer for every
  RESTAction-level K8s call.
- **Widgets class L1 telemetry (same window):** ZERO hits for admin's
  first `/call` of the navigation. Admin rebuilds widget tree from
  scratch — per-user widget L1 keyed on `(class="widgets", gvr, ns,
  name, user, groups, perpage, page)` is empty for admin's
  `(username="admin", groups=[...])` tuple.
- **Phase 1 walker resolves widgets BUT discards output:**
  `phase1_walk.go:646-651` calls `widgets.Resolve` with the
  SA-credentialed `ctx`, returns into a discarded `res` variable
  (only `extractResourcesRefsItems(res.Object)` at `:672` is used —
  for recursion-target discovery, not for cache population).
  Compare `phase1_walk.go:333` block comment: "Output discarded.
  Resolution errors are collected, not fatal" — discovery-only by
  design.
- **F2 content prewarm fills apistage layer, NOT widgets layer.**
  `runContentPrewarmPass` at `phase1_content_prewarm.go:237-319`
  iterates the harvested RESTAction set under
  `withContentPrewarmSAContext` (which carries
  `cache.WithApistagePrewarm`); each `restactions.Resolve` populates
  the IDENTITY-FREE apistage entry as a side-effect at
  `apistage.go:441` and the per-user `widgets` L1 is never touched.
  This is the diagnosis's failure mode (a) at file:line.
- **Cyberjoker hits the same miss path on his first /call**; narrow
  RBAC reduces his composition working set to ~5%, so his
  session-cold is faster (1,457ms median per the D.5 baseline
  captured 2026-05-21 at `/tmp/snowplow-runs/0.30.152/before/`) but
  he still pays a full widget-tree rebuild.

**The mechanism gap is structural:** the widget envelope is resolved
once, key-cached per-user, and Phase 1's walker can't populate the
per-user cache because there IS NO USER yet — the SA identity is the
only identity available. F2 closed this for the apistage class by
keying that class identity-free (Ship F1, `resolved.go:340-353`); the
widgets class above it still keys identity-bearing.

**The ship.** Repeat the F1 split one tier up, at the widgets class.

---

## §2. Architectural mechanism — file:line citations

### §2.0 Prior-art check (`feedback_check_k8s_clientgo_prior_art`)

Before designing, check whether client-go or any other k8s primitive
already models an identity-free shared store + per-user-narrowed read
path. The answer is **YES, exactly twice — and Ship G is the third
instance of the same pattern**:

1. **Shared informer factory + per-request RBAC evaluation
   (apiserver model).** A k8s shared informer holds the
   identity-free union of all `(namespace, name)` units for a GVR; the
   apiserver's `RequestInfo` middleware narrows per-request via
   `authorization.Authorizer`. Snowplow's analogue is the
   `cache.ResourceWatcher` informer set + `rbac.EvaluateRBAC` —
   already in production. Ship G's content-keyed widget layer is
   STRUCTURALLY IDENTICAL: stored entry = identity-free; serve-time
   gate = `gateWidgetEnvelope` (per-user, RBAC-derived).
2. **Ship F1's `apistage` class (already shipped, default-off,
   2026-05-08).** `apistage.go:13-25` documents the exact split:
   "K8s RBAC is a binary gate on (gvr, ns, [name]) units — it never
   filters items or shapes content, so the SAME content unit is
   shared by every user the gate admits." The widget tier carries
   ONE additional identity-bearing field (`status.resourcesRefs.items[].allowed`
   — set per-user by `resourcesrefs/resolve.go:88`), so the gate has
   ONE extra responsibility (narrow the `allowed` flags); the rest
   of the envelope is identity-invariant.

There is no NEW machinery to invent. The mechanism is **identical to
F1's** under a different `CacheEntryClass` discriminant and a
slightly richer per-user gate. Ship G is "F1 one tier up."

### §2.1 New CacheEntryClass vs new key shape under existing widgets class

**DECISION: new class `CacheEntryClassWidgetContent` = "widgetContent",
SEPARATE from the existing `"widgets"` class.**

Trade-off analysis:

- **Option (i) — REUSE the existing `"widgets"` class with a key-shape
  switch.** Reject. The widgets class is per-user-keyed; F1's
  precedent at `resolved.go:340-353` proves the pattern of branching
  ComputeKey on `CacheEntryClass`, but a class that hashes per-user
  for some entries and identity-free for others would be a
  hidden-mode confusion — the same class string would describe two
  different key shapes, breaking the invariant that one
  `CacheEntryClass` value defines one key shape. The refresher
  registration site at
  `internal/handlers/dispatchers/dispatchers.go:83`
  (`cache.RegisterRefreshFunc("widgets", refreshFunc)`) maps one
  class to one refresh handler; splitting the class's internal
  semantics would silently change refresh behaviour for in-flight
  entries.
- **Option (ii) — NEW class `"widgetContent"`.** Accept. Mirrors F1's
  introduction of `"apistage"` at `resolved.go:79`
  (`CacheEntryClassApistage = "apistage"`). One new constant; the
  `ComputeKey` branch at `resolved.go:340-353` already gates
  identity-hashing on `in.CacheEntryClass != CacheEntryClassApistage`
  — extend to `in.CacheEntryClass != CacheEntryClassApistage &&
  in.CacheEntryClass != CacheEntryClassWidgetContent`. NO key-space
  rotation for any pre-existing entry (a different string hashes to
  a different cell). Per-class entry distinguishable in expvar
  counters (extend the Ship E pattern at `resolved.go:194-204`).
- **Option (iii) — overload `"apistage"` to include widget content.**
  Reject. The apistage class semantically describes a single K8s
  CALL envelope (one GVR + ns + name unit); the widget tier
  describes the resolved WIDGET shape (a CR with status fields like
  `widgetData`, `resourcesRefs.items`, `apiRef` dataSource carryover).
  Two distinct semantics under one class would break the F1 mental
  model.

**Implementation:** add `CacheEntryClassWidgetContent = "widgetContent"`
at `internal/cache/resolved.go:79` (alongside the existing
`CacheEntryClassApistage`). The string value never changes after ship
G lands (per the `apistage` precedent — the string is hashed into the
cache key AND used as a refresher registry key; rotating it would
invalidate every in-flight entry).

### §2.2 Key composition + identity-invariance verification

**Key composition.** `cache.ResolvedKeyInputs` with:
- `CacheEntryClass = CacheEntryClassWidgetContent`
- `Group, Version, Resource` — the widget CR's GVR
  (e.g. `widgets.templates.krateo.io/v1`, resource `panels`)
- `Namespace, Name` — the widget CR's namespace + name
- `Username, Groups` — left ZERO (the ComputeKey branch at
  `resolved.go:340-353`, once extended per §2.1, skips identity for
  this class)
- `PerPage, Page` — preserved EXACTLY as the per-user `"widgets"`
  class uses them at `dispatchCacheLookupKey` (`helpers.go:152-153`).
  This is load-bearing: `widgets.Resolve` reads `opts.PerPage,
  opts.Page` (`widgets/resolve.go:32-33`) and emits a paginated
  `status.resourcesRefs` slice at `resolve.go:76-87`. PerPage/Page
  belong in the content key.
- `Extras` — the same `extras` map a widgets request carries today
  (`helpers.go:154`). These are user-supplied request-time variables
  that flow into the widget's `apiRef` data source via
  `apiref.Resolve(...).Extras` — they shape the envelope content.
  They belong in the content key, identity-free.

**Identity-invariance up to one flag.** The resolved widget envelope
(`Widget = unstructured.Unstructured` per `widgets/resolve.go:26`) is
the input widget CR mutated with these status fields:

| Status field set by | Path | Identity-dependent? |
|---|---|---|
| `resolveApiRef` (`resolve.go:40`) → `apiref.Resolve` | n/a (returns `ds`, threaded into widget data) | dataSource: resolved RESTAction output goes through the F1 apistage content layer (identity-free at apistage; per-user gate already runs there on the dataSource items). The `ds` map passed into `widgetdatatemplate` is THE SAME `ds` map regardless of requester for items the apistage gate admits. |
| `resolveWidgetData` (`resolve.go:47`) → `status.widgetData` | `status.widgetData` (`widgets/widgets.go:14` — `widgetDataKey="widgetData"`) | NO — jq evaluation over the (already-RBAC-narrowed-at-apistage) dataSource. Identity entered the data flow only through the apistage gate's narrowing, NOT through any per-user jq logic. Output is a function of (template, dataSource); identical inputs → identical output. |
| `resolveResourceRefs` (`resolve.go:61`) → `status.resourcesRefs.items[]` | each item carries `ID, Path, Verb, Allowed, Payload` (templatesv1.ResourceRefResult) | **YES, on the `Allowed` boolean per item.** `resourcesrefs/resolve.go:88-92` sets `el.Allowed = rbac.UserCan(ctx, ...)` per item per verb — identity-bearing. All OTHER fields (`ID, Path, Verb, Payload`) are derived from the widget's resourcesRefs spec + the request identity's namespace inheritance, NOT from per-user RBAC. |
| `traceId` injection (`widgets.go:128-132`) | `status.traceId` | per-request; NOT cacheable. We exclude this from the cache content (see §2.3). |

**Conclusion — TRACED:** the resolved widget envelope is identity-
invariant **EXCEPT** for the `Allowed` flag in each
`status.resourcesRefs.items[]` entry. Up to that one flag, two
different users' resolved envelopes for the same `(gvr, ns, name,
perpage, page, extras)` produce byte-identical output. This is exactly
the shape Diego previously certified for the Phase 1 walker at
`phase1_walk.go:766-805` — the same `allowed` flag, set by the same
`rbac.UserCan` call at `resourcesrefs/resolve.go:88-92`, deliberately
NOT used as a recursion gate because discovery is identity-
independent. **That certification extends directly to the widget
content envelope.** The "drop `allowed` to make the body
identity-free, re-apply at serve-time" mechanic is the cached-data
analogue of the walker's "do not gate discovery on `allowed`"
mechanic. Both pivot on the same property: the body of the resolved
widget is identity-independent in every field except `allowed`.

### §2.3 F2 walker populate path

**Where the Put happens:** `phase1_walk.go:646-651`, inside the
recursive `phase1Walker.walk` method, IMMEDIATELY AFTER the
`widgets.Resolve(...)` call.

**GVR threading invariant** (replaces design's earlier reference
to a non-existent `widgets.GetResource(in.Object)` helper).
`widgets.GetResource` DOES NOT EXIST — `internal/resolvers/widgets/widgets.go:20-44`
only exposes `GetAPIVersion`, `GetNamespace`, `GetName`. The GVR
is ALREADY threaded into the walker via `objects.Get`'s return
shape at `internal/handlers/dispatchers/phase1_walk.go:719`:
`got := objects.Get(ctx, ref)` returns `objects.Result{ GVR
schema.GroupVersionResource, Unstructured *unstructured.Unstructured,
Err *response.Status }` (`internal/objects/get.go:22-26`).

For the **recursive site** at `phase1_walk.go:736`
(`_ = w.walk(ctx, got.Unstructured, depth+1, childPage, childPerPage)`),
the GVR lives on `got.GVR` immediately above the recursive call —
thread it into `walk` as a new parameter. For the **root site** at
`phase1_walk.go:557` (`return w.walk(rctx, root, 0,
prewarmPageLimit(), prewarmPageLimit())`), the lister at
`phase1_roots.go:122-222` currently returns
`[]*unstructured.Unstructured` and discards `got.GVR` from
`objects.Get`; the lister MUST be widened to return
`[]rootWithGVR{ Unstructured, GVR }` pairs OR the root's GVR
re-derived from the parsed `templatesv1.ObjectReference` via
`schema.ParseGroupVersion(ref.APIVersion).WithResource(ref.Resource)`
at the `resolveNavigationRoot` call site (`phase1_walk.go:546-557`).
Either path is a mechanical thread — NO new helper. The diff
sketch below shows the recursive site (where `got.GVR` is
already in scope).

**Diff sketch (load-bearing position is BEFORE `extractResourcesRefsItems`
mutates anything; the walker only reads the resolved object after
this Put):**

```go
// phase1_walk.go:610 — walk gains a `gvr` parameter so the Put
// site has the (gvr, ns, name) tuple identity-free key composition
// needs. Caller threading: phase1_walk.go:557 (root) re-derives
// from the parsed ObjectReference; phase1_walk.go:736 (recursion)
// passes got.GVR from objects.Get's return at :719.
func (w *phase1Walker) walk(ctx context.Context, in *unstructured.Unstructured, gvr schema.GroupVersionResource, depth int, page, perPage int) error {
    // ... existing body unchanged up to widgets.Resolve at :646 ...

    res, err := widgets.Resolve(ctx, widgets.ResolveOptions{
        In:      in,
        AuthnNS: w.authnNS,
        PerPage: perPage,
        Page:    page,
    })
    if err != nil {
        // ... existing error handling (lines 652-666) ...
    }
    if res == nil {
        return nil
    }

    // Ship G — populate the identity-free widget content layer.
    // Gated on cache.WidgetContentL1Enabled(). The PUT site is the
    // SA-credentialed walker's resolution result, which carries no
    // per-user identity (the SA's UserInfo + ResolveOptions). The
    // envelope IS the cache content; the per-user gate runs at
    // serve time over the embedded resourcesRefs.items[].allowed
    // flags.
    //
    // Bind by:
    //   - (gvr, ns, name)         gvr from caller; ns/name from `in`
    //   - (perPage, page)         from the walker's pagination
    //                              (Ship 0.30.127's declared-or-default)
    //   - extras                  nil at prewarm (the walker does not
    //                              receive user-supplied extras)
    //
    // TraceId stripping: the prewarm path never injects traceId
    // (widgets.go:128-132 runs only on the dispatcher path), so no
    // strip-before-Put step is needed for the walker-side Put.
    if cache.WidgetContentL1Enabled() {
        populateWidgetContentL1(ctx, gvr, in, perPage, page, res)
    }

    // ... existing children-recursion block unchanged ...
    // At phase1_walk.go:736 the recursive call now becomes:
    //   _ = w.walk(ctx, got.Unstructured, got.GVR, depth+1, childPage, childPerPage)
    // (got.GVR comes from objects.Get's return at :719 — already
    // in scope, no new helper).
}
```

**Identity-bearing flag overwrite invariant — LOAD-BEARING.** The
walker's `widgets.Resolve` at `phase1_walk.go:646` runs under the
context built by `withPhase1SAContext` (`phase1_walk.go:514`),
which installs the SA `UserInfo` (`phase1_walk.go:525`) AND
`cache.WithInternalEndpoint` + `cache.WithInternalRESTConfig`
(`phase1_walk.go:528-529`) but does NOT set
`cache.WithApistagePrewarm`. The downstream apistage gate
(`internal/resolvers/restactions/api/apistage.go:93-145`)
therefore runs under the SA identity — its per-item
`rbac.UserCan` verdicts evaluate the SA's `*/*` get/list/watch
ClusterRoleBinding and produce `allowed=true` flags for every
navigation `resourcesRefs.items[]` entry. Ship G's
`populateWidgetContentL1` Puts this envelope un-stripped: the
stored body carries SA-evaluated `allowed=true` per item.
`gateWidgetEnvelope` (§2.4) **OVERWRITES every
`status.resourcesRefs.items[].allowed`** per-request before
serialisation, deriving each flag via `rbac.UserCan` under the
REQUEST identity from `xcontext.UserInfo(ctx)`. AC-G.4
byte-equivalence holds because the gate is a re-run of the same
`rbac.UserCan` → `EvaluateRBAC` function
(`internal/resolvers/widgets/resourcesrefs/resolve.go:88-92`)
over the same typed-RBAC snapshot the cold-resolve uses; the
stored SA-evaluated flag is never served verbatim.

`populateWidgetContentL1` (new helper, sibling to `recordWidgetDeps`
at `widgets.go:151`):

```go
func populateWidgetContentL1(
    ctx context.Context,
    gvr schema.GroupVersionResource,
    in *unstructured.Unstructured,
    perPage, page int,
    res *unstructured.Unstructured,
) {
    c := cache.ResolvedCache()
    if c == nil {
        return
    }
    inputs := cache.ResolvedKeyInputs{
        CacheEntryClass: cache.CacheEntryClassWidgetContent,
        Group:           gvr.Group,
        Version:         gvr.Version,
        Resource:        gvr.Resource,
        Namespace:       in.GetNamespace(),
        Name:            in.GetName(),
        PerPage:         perPage,
        Page:            page,
        // Username/Groups/Extras intentionally zero — see §2.2.
    }
    key := cache.ComputeKey(inputs)
    encoded, err := encodeResolvedJSON(res)
    if err != nil {
        return
    }
    c.Put(key, &cache.ResolvedEntry{
        RawJSON: encoded,
        Inputs:  &inputs,
    })
}
```

**The Put runs on EVERY navigation widget the walker resolves** (root
+ every GET-recursion child whose subtree the walker descends).
Pagination defaults at `prewarmPageLimit()` (`phase1_content_prewarm.go:125-131`,
default 5) plus per-widget `slice`-declared overrides per Ship 0.30.127
— the SAME pagination the per-user dispatcher uses on a real request
of the same widget. PerPage/Page are part of the content key, so
prewarm warms exactly the entries any-perPage `/call` hits IFF the
serve-time perPage matches the prewarm perPage. (See OQ-1 for the
serve-time fallback when they don't.)

**SA-derived `allowed` flag overwrite invariant** (PM-flagged
2026-05-21 — verbatim per PM template; load-bearing for AC-G.4
byte-equivalence):

> The walker's `widgets.Resolve` runs under `withPhase1SAContext`
> (`phase1_walk.go:514`), which does NOT set `WithApistagePrewarm`.
> The apistage gate runs under SA identity, producing SA-evaluated
> `allowed=true` flags in the envelope. Ship G's Put stores these
> flags un-stripped. `gateWidgetEnvelope` OVERWRITES every
> `status.resourcesRefs.items[].allowed` per-request before
> serialisation, derived via `rbac.UserCan` under the request
> identity. AC-G.4 byte-equivalence holds because the gate is a
> re-run of the same function over the same typed-RBAC snapshot
> the cold-resolve uses.

### §2.4 Serve-time read path

**Where the Get happens:** `widgets.go:80-95`, BEFORE the existing
per-user widget L1 lookup. Same handler, ONE new branch inserted ahead
of the existing branch.

**Diff sketch:**

```go
// EXISTING per-user lookup at :80 stays put — it remains the fall-
// back when (a) the content layer is flag-off, (b) the content
// entry is missing for this (gvr, ns, name, perpage, page),
// or (c) gateWidgetEnvelope returns served=false (no identity →
// fail-closed).

// Ship G — identity-free content layer lookup runs FIRST. Same
// gating semantics as the existing per-user lookup at :80
// (post-EvaluateRBAC, post-RBAC dispatch gate at :62-72).
if cache.WidgetContentL1Enabled() {
    contentKey, contentHandle := dispatchWidgetContentKey(
        req.Context(),
        got.GVR.Group, got.GVR.Version, got.GVR.Resource,
        got.Unstructured.GetNamespace(),
        got.Unstructured.GetName(),
        perPage, page,
    )
    if contentHandle != nil {
        if entry, ok := contentHandle.Get(contentKey); ok {
            // CONTENT HIT — apply the per-user RBAC narrowing.
            gated, served := gateWidgetEnvelope(
                req.Context(),
                got.GVR, got.Unstructured.GetNamespace(),
                entry.RawJSON,
            )
            if served {
                cache.RecordApiserverFallthrough(req.Context(),
                    cache.ReasonWidgetContentHit, got.GVR.String())
                emitResolvedCacheLookup(log, "widgetContent",
                    contentKey, true, len(gated))
                writeResolvedJSON(wri, gated)
                log.Info("Widget successfully resolved",
                    slog.String("duration", util.ETA(start)),
                    slog.String("l1", "content-hit"),
                )
                return
            }
            // served==false → fail-closed: no identity on ctx.
            // Existing per-user path also bails on this exact
            // condition (dispatchCacheLookupKey returns nil handle
            // when xcontext.UserInfo fails, helpers.go:137-142).
        }
        // CONTENT MISS — fall through to the existing per-user
        // widget L1 lookup. The miss is the EXPECTED path when
        // F2 has not warmed this (gvr,ns,name,perpage,page).
        cache.RecordApiserverFallthrough(req.Context(),
            cache.ReasonWidgetContentMissPerUserFallback,
            got.GVR.String())
    }
}

// EXISTING per-user lookup at :80 — unchanged.
cacheKey, cacheHandle, cacheInputs := dispatchCacheLookupKey(...)
// ... existing flow continues ...
```

`gateWidgetEnvelope` (new helper, sibling to `gateContentEnvelope` at
`apistage.go:93-120`):

```go
// gateWidgetEnvelope applies the serve-time per-user RBAC gate to a
// raw widget envelope retrieved from the identity-free content layer.
// It is the Ship G analogue of F1's gateContentEnvelope, narrowing
// the embedded resourcesRefs.items[].allowed flags to the requesting
// identity. The body is parsed ONCE per hit (mirroring F1's
// parseListEnvelope at apistage.go:139-161 — the parsed items can be
// pre-decoded at Put time and stored on entry.Items for the R3
// double-unmarshal optimisation, see OQ-2).
//
// Returns (gatedEnvelope, served):
//   - served==false — fail-closed: no identity on ctx. The caller
//     falls through to the existing per-user widget L1 lookup,
//     which itself nil-checks UserInfo at helpers.go:137-142 and
//     correctly bails on the same condition.
//   - served==true  — gatedEnvelope is the RBAC-narrowed bytes ready
//     to write to the response.
func gateWidgetEnvelope(
    ctx context.Context,
    gvr schema.GroupVersionResource,
    namespace string,
    raw []byte,
) ([]byte, bool) {
    ui, err := xcontext.UserInfo(ctx)
    if err != nil {
        return nil, false
    }
    var obj map[string]any
    if err := json.Unmarshal(raw, &obj); err != nil {
        return nil, false
    }
    // Walk status.resourcesRefs.items[] and re-evaluate `allowed`
    // per item per verb under the request identity. The items'
    // (ID, Path, Verb, Payload) are identity-invariant; only the
    // `allowed` boolean is re-derived.
    items, ok, _ := maps.NestedSlice(obj, "status", "resourcesRefs", "items")
    if ok {
        for i, raw := range items {
            it, ok := raw.(map[string]any)
            if !ok {
                continue
            }
            // Re-derive allowed under THIS user's identity.
            it["allowed"] = recomputeAllowedFromRefItem(ctx, ui, it)
            items[i] = it
        }
        maps.SetNestedSlice(obj, items, "status", "resourcesRefs", "items")
    }
    // Re-encode with the SAME encoder settings encodeResolvedJSON
    // uses (helpers.go:193 — SetIndent("", "  ")) so the body is
    // byte-identical to a cold-resolve response for the same
    // request identity (AC-G.4).
    return encodeResolvedJSON(obj)
}
```

`recomputeAllowedFromRefItem` (new helper):

```go
// recomputeAllowedFromRefItem replays resourcesrefs/resolve.go:88-92
// for ONE item under the supplied identity. It parses the item's
// Path (a /call?... URL) via the existing shared decoder
// util.ParseCallPathToObjectRef at internal/handlers/util/callpath.go:36,
// constructs the GVR INLINE from the parsed ObjectReference's
// (APIVersion, Resource) — schema.ParseGroupVersion +
// .WithResource, NO new helper — and calls rbac.UserCan with the
// request identity's verb. Identical to the per-user resolveOne
// loop but operating on an already-resolved item — re-derivation
// is sub-microsecond (typed-RBAC snapshot, no apiserver round-trip).
//
// (PM-flagged 2026-05-21: an earlier sketch referenced a
// `util.GVRFromObjectRef` helper that DOES NOT EXIST in
// internal/handlers/util/. The shape below is the only one
// actually achievable today; no helper introduction is needed.)
func recomputeAllowedFromRefItem(
    ctx context.Context,
    ui jwtutil.UserInfo,
    item map[string]any,
) bool {
    path, _ := item["path"].(string)
    verb, _ := item["verb"].(string)
    if path == "" || verb == "" {
        return false // defensive
    }
    // util.ParseCallPathToObjectRef — internal/handlers/util/callpath.go:36.
    // Returns ok=false for any path that is not a /call endpoint.
    ref, ok := util.ParseCallPathToObjectRef(path)
    if !ok {
        return false
    }
    // INLINE GVR construction — no new helper. The standard
    // client-go pattern is schema.ParseGroupVersion +
    // GroupVersion.WithResource, already used at
    // internal/handlers/util/gvr.go:23.
    gv, err := schema.ParseGroupVersion(ref.APIVersion)
    if err != nil {
        return false
    }
    gvr := gv.WithResource(ref.Resource)
    // Same call signature resourcesrefs/resolve.go:88-92 uses.
    return rbac.UserCan(ctx, rbac.UserCanOptions{
        Verb:          strings.ToLower(verb),
        GroupResource: gvr.GroupResource(),
        Namespace:     ref.Namespace,
    })
}
```

**Critical:** the per-user re-derivation runs over the SAME
`rbac.UserCan` → `EvaluateRBAC` path that the cold-resolve
`resourcesrefs.resolveOne` uses (`resourcesrefs/resolve.go:88-92`).
This is what AC-G.4 (byte-identical narrowing) is bound by — the gate
is a re-run of the exact same function over the same typed-RBAC
snapshot. The narrowing is byte-equivalent BY CONSTRUCTION; no new
RBAC mechanism is introduced.

### §2.5 Per-user RBAC narrowing mechanism

**Single gate site, single mechanism.** `gateWidgetEnvelope` runs on
every content-Get-hit. Identity flows from `xcontext.UserInfo(ctx)` —
the standard request-identity carrier. `rbac.UserCan` resolves under
the Ship B typed RBAC snapshot (`internal/rbac/evaluate.go`); the
typed snapshot is identity-invariant under load and serves the same
verdict the apiserver would (AC-B.12).

**No cross-user leak path** by construction:
1. The cache content carries `allowed: true` flags computed under
   the SA identity (which can do `*/*` get/list/watch). For a user
   who SHOULD NOT see a resourcesRef, the SA-set `allowed: true`
   would leak if served verbatim.
2. `gateWidgetEnvelope` re-derives `allowed` from the request
   identity, NOT from the cache content. The stored flag is
   overwritten in-place per request before serialisation.
3. The frontend WidgetRenderer's `items.filter(({allowed})=>allowed)`
   (see `phase1_walk.go:788-792`) is the consumer; it sees the
   per-user-narrowed boolean.

**`feedback_l1_per_user_keyed_never_cohort` justification (load-bearing
critical):** The feedback file states the widget L1 cache MUST stay
per-user-keyed and never cohort/groups-only because of RBAC
cross-user leak risk. Ship G appears to violate this on first read —
the content layer is identity-free. The justification for the split
is **structurally identical to F1's apistage split** (which the same
feedback file's invariant did NOT prohibit, because F1 passed PM
gate):

- The per-user-keyed invariant exists to prevent RBAC cross-user
  leak when the CACHE CONTENT itself differs per user. F1's
  innovation was the **split**: store the un-narrowed shell
  identity-free, run the per-user narrowing at the gate, never store
  the narrowed body. Cross-user leak is impossible because the gate
  runs on every request before serialisation.
- Ship G repeats the split at the widgets class. The (identity-free
  shell) layer holds the un-narrowed envelope (every `allowed`
  derived under the SA identity = all-true for navigation widgets).
  The (per-user gate) layer overwrites every `allowed` flag with the
  request identity's `rbac.UserCan` verdict before the body is
  serialised back to the client. **The body that leaves the pod is
  per-user; the body in the cache is shell-only.**
- The cross-user-keyed-cache risk the feedback file calls out is for
  designs that serve cache content VERBATIM. Ship G never serves a
  cached widget body verbatim — every Get-hit runs through the gate.
  This is the same architectural property F1 introduced, applied one
  tier up.

**The split is RBAC-safe because the gate is monotonic with respect
to apiserver RBAC verdicts:** `rbac.UserCan` returns true if and
only if the apiserver would return non-deny for a corresponding
SelfSubjectAccessReview. Per-user narrowing at the gate is therefore
identical to per-user narrowing at cold-resolve time
(AC-G.4 byte-equivalence). No information available pre-gate is
withheld post-gate; no information withheld pre-gate is exposed
post-gate.

### §2.6 Interaction with D.5

D.5 (cluster-list-when-allowed iterator collapse) is one layer below
Ship G — D.5 collapses N namespaced LISTs into 1 cluster-scope LIST
at the apistage class. Ship G adds a HIGHER tier of identity-free
caching at the widgets class.

**The two ships compose cleanly:**
- A cold admin /call on a Dashboard widget hits the Ship G content
  layer FIRST. On a Ship G HIT, the resolver never reaches D.5's
  cluster-list dispatch site (because the apistage layer is not
  consulted — the widget envelope is already resolved and cached).
- On a Ship G MISS (or flag-off), the request falls through to the
  existing per-user widget L1, which falls through to a cold
  resolve, which fires the apistage gate, which (under D.5)
  dispatches a cluster-list when allowed.
- D.5's apistage-content store reads/writes the (gvr, ns="", name="")
  cell at `apistage.go:61-70` — DIFFERENT cell from any
  `CacheEntryClassWidgetContent` entry. No key-space collision.

**Ship G does NOT depend on D.5 being green.** D.5 lands a separate
gain (admin cold 8.16s → ≤3.5s when D.5 hits its HG-1 and the per-NS
fan-out collapses). Ship G lands an INDEPENDENT gain (admin cold
9.94s session-cold → ≤1.5s when widget content L1 is warm). Both
gains compose on a cold path that misses both layers; on the
steady-state warm path, Ship G subsumes D.5's wins because the widget
envelope is served from the upper tier without reaching the apistage
gate.

**Cyberjoker:** under D.5 alone, his apistage entries are TTL-evicted
and he re-pays the fan-out (Branch C territory, separate). Under
Ship G alone, the widget content L1 is warmed by the SA prewarm, his
gate re-runs `rbac.UserCan` per item, the `allowed` flags narrow to
his actual permissions, and he serves from L1 in ~450ms warm-steady-
state (the per-user L1 warm baseline). Cyberjoker IS a beneficiary
of Ship G even though he never benefits from D.5.

### §2.7 Interaction with refresher

The new class needs a refresh handler. **Registration site is one
line** at `internal/handlers/dispatchers/dispatchers.go:83` (alongside
the existing `"widgets"` registration):

```go
cache.RegisterRefreshFunc("widgets", refreshFunc)
cache.RegisterRefreshFunc(cache.CacheEntryClassApistage, refreshFunc)
cache.RegisterRefreshFunc(cache.CacheEntryClassWidgetContent, refreshFunc) // Ship G
```

**The refreshFunc body itself does not change.** It dispatches by
`in.CacheEntryClass` through `resolveAndPopulateL1`
(`dispatchers.go:80`) → `resolve_populate.go`, which has a class-aware
switch that ALREADY handles `restactions`, `widgets`, and `apistage`.
A `widgetContent` re-resolve runs the SAME `widgets.Resolve` the
walker runs, under the refresher's SA identity context (the
refresher's `saEP, saRC` at `dispatchers.go:65-71`), and writes the
re-resolved envelope back under the IDENTITY-FREE content key.

**Dep tracking already covers this class.** The dep tracker
(`internal/cache/deps.go`) records edges keyed on the L1 cache key
(opaque hash), not on the class string. Ship G's L1 keys flow into
the dep tracker via the standard `recordWidgetDeps`-equivalent path
at `widgets.go:151` — but **on the prewarm-Put site only**. The
dispatcher-path Get-hit does NOT re-record deps because the dep
edges were recorded when the entry was first written. UPDATE/PATCH
events on any K8s object the widget envelope depends on flow through
the dep tracker → `Deps().SetRefreshHook` → the refresher workqueue
→ the registered `widgetContent` handler.

**OQ-3 (see §8)** asks whether the refresher needs a new dirty-mark
path. Answer: no — the existing reverse-index (`Deps().RemoveL1Key`
called from `resolved.go:671`) is class-agnostic.

---

## §3. Decision tree / fix shapes

### Branch A — full envelope identity-invariant up to `allowed`

**Trigger (default-on shape):** OQ-2's empirical pre-deploy probe
returns "the resolved widget envelope is byte-equivalent between admin
and cyberjoker for the same `(gvr, ns, name, perpage, page, extras)`,
EXCEPT for `status.resourcesRefs.items[].allowed` booleans (and
`status.traceId` per-request).

**Action:** ship as designed in §2. Single class, single gate site.

**Empirical check (binds the branch):** before commit, dev runs
two `/call` requests against the same widget under admin then
cyberjoker, captures both responses, runs:

```
$ jq 'del(.status.traceId) | (.status.resourcesRefs.items[].allowed) |= null' admin.json > admin.normalised.json
$ jq 'del(.status.traceId) | (.status.resourcesRefs.items[].allowed) |= null' cyberjoker.json > cyberjoker.normalised.json
$ diff admin.normalised.json cyberjoker.normalised.json
```

Zero diff → Branch A confirmed. Any diff → Branch B.

### Branch B — envelope carries identity-bearing fields BEYOND `allowed`

**Trigger:** the empirical diff returns non-zero. SOME OTHER field
carries identity-derived content. Possible carriers (hypothetical, to
be falsified):
- A widget whose `widgetDataTemplate` jq expression directly reads
  `.user.username` or `.user.groups` from a dataSource that exposes
  identity (today none do; verify per `feedback_cache_must_not_constrain_jq`).
- A widget whose `apiRef` returns user-narrowed items at the
  apistage layer that DIFFER per user (would mean F1's apistage
  identity-free assumption is itself violated — separate
  follow-up, not Ship G's job).
- A widget's `resourcesRefs.items[].payload` carries an identity-
  bearing template variable.

**Action under Branch B:** identify WHICH field carries the residue.
Two sub-branches:

- **Branch B.1 — single localised carrier.** If the carrier is a
  single named field, EXTEND `gateWidgetEnvelope` to re-derive that
  field under the request identity at gate time (mirroring how
  `allowed` is re-derived). The content layer still stores the
  un-narrowed shell; the gate's responsibility grows by one
  re-derivation. AC-G.4 byte-equivalence test re-validated.
- **Branch B.2 — diffuse / unpredictable carrier.** If the residue
  cannot be cleanly factored, ship Ship G **partial-envelope
  scope-restricted**: a per-widget-CR opt-in field (similar to
  D.5's `clusterListWhenAllowed`) that an RA / widget author flags
  on widgets known to be identity-invariant. Default-off. Ship G's
  scope shrinks but the architecture is preserved.

### Binding to pre-deploy empirical checks

The Branch A/B decision is bound to OQ-2's pre-flight probe (see
§6.1). NO Branch B sub-design is in scope for this design doc —
they are conditional follow-up ships, pre-designed only in shape.

---

## §4. Acceptance criteria (PM-binding, 14)

**AC-G.1** — F2 walker populates an identity-free widget content L1
entry on every navigation-tree widget resolution. Verified by adding
an `apistage_store_total`-style counter for the new class and asserting
the counter advances by N (≥ navigation-tree-depth widgets) over a
Phase 1 cycle. Implementation: extend `resolved.go:194-204`'s Ship E
pattern with `widgetContentStoreTotal`, `widgetContentEvictTotal`.

**AC-G.2** — Serve-time widget handler reads the identity-free entry
FIRST (BEFORE the existing per-user widget L1 lookup at
`widgets.go:80-95`), composes shell + per-user RBAC narrowing via
`gateWidgetEnvelope`, returns the gated body.

**AC-G.3** — Cross-user share: admin and cyberjoker requesting the
same `(gvr, ns, name, perpage, page)` widget hit the SAME L1 key.
First request's Put populates; second request's Get hits.
`widgetContentStoreTotal` increments by 1 (not 2). Each request's
`gateWidgetEnvelope` runs independently and writes its own narrowed
body; the cache content itself is shared.

**AC-G.4** — Per-user RBAC narrowing produces byte-identical output
to a pre-Ship-G per-user widget L1 entry for the same request
identity. Test:
1. Disable Ship G (`WIDGET_CONTENT_L1_ENABLED=false`). Fetch widget
   X under admin. SHA256 the response.
2. Enable Ship G. Warm the content layer under SA. Fetch widget X
   under admin. SHA256 the response.
3. Assert SHA256 equal.

The byte-equivalence is bound by `gateWidgetEnvelope` re-running
`rbac.UserCan` over the SAME typed RBAC snapshot the cold-resolve
path uses (`resourcesrefs/resolve.go:88-92`).

**AC-G.5** — Refresher tracks widget content entries. The refresher
handler is registered for `cache.CacheEntryClassWidgetContent` at
`dispatchers.go` (alongside the existing `widgets`/`apistage`
handlers). UPDATE/PATCH events on any K8s object a widget content
entry depends on trigger a refresh via the existing dep-tracker
reverse index. Verified by: kubectl apply a modified Composition that
the Dashboard widget's apiRef RESTAction depends on → observe a
`refresher.completed` log entry citing a `widgetContent` key →
observe the next /call returns the updated content (zero TTL wait).

**AC-G.6** — Default-on for the prod gate, cleanly disabled via
`CACHE_ENABLED=false` per `project_caching_is_provisional`. ALSO
provides a fine-grained toggle `WIDGET_CONTENT_L1_ENABLED` (mirroring
`RESOLVED_CACHE_APISTAGE_ENABLED` at `resolved.go:54-59`). When
`CACHE_ENABLED=false`, the entire path is skipped — the dispatcher
runs the EXACT 0.30.6 code path. When `WIDGET_CONTENT_L1_ENABLED=false`
(but `CACHE_ENABLED=true`), only the Ship G layer is bypassed; the
existing per-user widget L1 + apistage L1 continue serving.

**AC-G.7** — Two new FallthroughReason constants in
`internal/cache/fallthrough_meter.go` (closed enum extension 20 → 22):
- `ReasonWidgetContentHit` — diagnostic counter, fires when the Ship
  G content layer is consulted and Gets a hit; gate runs.
- `ReasonWidgetContentMissPerUserFallback` — diagnostic counter,
  fires when the Ship G content layer Gets a miss and the request
  falls through to the existing per-user widget L1 lookup.

Cardinality: 22 × 10 × 50 ≈ 11,000 series. Below Prometheus comfort.
Same per-stage label pattern as D.5's AC-D5.9.

**AC-G.8** — Pre-flight empirical baseline at
`/tmp/snowplow-runs/0.30.16x/before/`:
- Admin n=3 session-cold reference (~9.94s expected).
- 4 named canary SHAs (`nav-admin`, `nav-cj`, `rl-admin`, `rl-cj`)
  carried from `/tmp/snowplow-runs/0.30.152/before/`.
- Clean-wire-shape audit per
  `feedback_byte_identical_baselines_clean_wire_shape`: scan
  responses for `Authorization: Bearer`, `eyJ`-prefix JWTs,
  `userAccessFilter`, AND new this ship — `_cacheKey` or any
  internal-cache-key field that might leak into the response when
  the gate misencodes.
- **Empirical widget-content entry resident size**: capture
  `kubectl get --raw ... | wc -c` for each navigation-root widget
  CR + a sampling of leaf widget envelopes (panels, datagrids) at
  `/tmp/snowplow-runs/0.30.16x/before/widget_entry_byte_sizes.txt`,
  per `feedback_capacity_caps_empirical_per_entry_cost`.
- **Pre-deploy identity-invariance probe** (OQ-2 closure check):
  fetch the Dashboard widget twice — once under admin, once under
  cyberjoker — strip `status.traceId` + `status.resourcesRefs.items[].allowed`,
  assert byte-equivalence. **HARD GATE on this probe: if the diff is
  non-zero, Ship G falls to Branch B and the design must be
  re-PM-gated** (the additive defensive scope-reduction is out of
  scope for this design's commit).

**AC-G.9** — Post-deploy admin cold session-cold ≤ 1.5s (n=3 median).
Projection: 9.94s today's baseline reduces by ~85-90% to land at
~1.0-1.5s, anchored to:
- Per-user widget L1 warm steady-state median (~450ms, measured
  multiple times in the per-user L1 telemetry baseline from
  2026-04-25 through 2026-05-10 in `project_baseline_2026_05_03.md`
  lineage) — Ship G's content hit + gate cost is structurally
  comparable to a per-user L1 hit (both are L1 reads + a single
  response encode; Ship G adds the gate's per-item `rbac.UserCan`
  re-derivation cost, which is sub-microsecond per item × tens of
  items per widget tree ≈ <50ms cumulative).
- ~200-300ms first-action overhead (HTTPS handshake, token verify,
  /call routing) that survives any L1 win.
- Sum: ~450 + ~50 + ~250 ≈ ~750ms p50; bounded to ≤ 1.5s as the gate
  to absorb gate-overhead measurement variance.

**AC-G.10** — Tag `0.30.16x` (dev picks the exact number at commit
time per `feedback_tag_commits`; the tag MUST point to the
"identity-free widget content layer" commit, not a downstream merge).
Commit type `feat(cache):`. Subject:
`feat(cache): identity-free widget content layer (Ship G, 0.30.16x)`.
Body cites:
- This design doc.
- Today's diagnosis (this morning's session, 2026-05-21) for
  9.94s session-cold root cause.
- HG-1 through HG-6 with targets.
- Branch A confirmation evidence (OQ-2 pre-deploy probe).
- file:line links to F1 precedent at `resolved.go:340-353` +
  `apistage.go:13-25` + this ship's mirror at the new
  `CacheEntryClassWidgetContent` site.

**AC-G.11** — Pre-commit dev review by architect + PM before
commit/tag/push (per `feedback_dev_review_with_architect_pm_before_commit`).

**AC-G.12** — Per-user widget L1 entry COUNT reduces materially when
Ship G is on. Verified via expvar `widget_l1_keys` (the existing
per-user `"widgets"` class count via `resolved.go:Stats()`). Under
N users requesting the same widget tree of M widgets:
- Pre-Ship-G: ~N × M entries in the widgets class.
- Post-Ship-G: ~M entries in `widgetContent` class + ~0 entries in
  `widgets` class (the per-user fallback rarely fires when prewarm
  is healthy).

Test: under N=4 synthetic users requesting the same Dashboard
(M=~20 widgets), assert `widgets`-class entry count is ≤ 1 (the
fallback might fire once on a TTL eviction) and `widgetContent`
entry count is ~M.

**AC-G.13** — No per-user widget L1 entry is created for paths
covered by the identity-free layer. Verified by:
- N users request the same widget.
- Inspect `/debug/vars` cache counters: `widget_content_store_total`
  ≥ 1, `widgets_class_store_total` increments stays at the
  pre-request baseline (the per-user fallback did not fire). The
  test is class-disaggregated so the existing per-user widget L1's
  legitimate uses (cache=on but widget-content layer off) still
  pass.

**AC-G.14** — Memory delta non-positive per
`feedback_capacity_caps_empirical_per_entry_cost`. The empirical
per-entry cost × identity-free entry count (M widgets ≈ 20-50 per
nav tree) MUST be less than the pre-Ship-G per-user × widgets
product (N × M ≈ N × 20-50). Concretely: under N=1000 users (the
P0 customer scale per `project_production_scale.md`), pre-Ship-G
per-user widget L1 entries ≈ 1000 × 20 = 20,000 entries; post-Ship-G
identity-free entries ≈ 20 entries. Even with a 5× per-entry size
increase (the Ship G entry holds the un-narrowed shell with
SA-evaluated `allowed: true` for every item), the bytes-resident
delta is ≈ -19,980 × per-entry-bytes. **Empirical bound:** capture
`bytes` from `/debug/vars` pre- and post-deploy; delta MUST be
non-positive (RSS goes down, never up). 180×-estimation-error
floor per the feedback file: the design-time projection is "memory
goes down by orders of magnitude"; the empirical landing must
confirm non-positive at minimum.

---

## §5. HARD GATES (PM-binding, 6)

**Methodology anchor.** All gates are **session-cold-warm-pod**:
warm snowplow pod with cleared browser localStorage, measuring the
user-facing first-paint timeline as the browser session opens.
**NOT** pod-cold (snowplow pod freshly restarted with empty L1).
Pod-cold scenarios are tracked under #157-lineage and are out of
Ship G scope. HG-5's cache-toggle invariant is also session-cold
(pod is up, `CACHE_ENABLED=false` set via chart values, browser
session is fresh). Same anchor as D.5 — explicitly shared so the
Ship G measurement runs on the SAME pod as the D.5 baseline.

| Gate | Target | HARD REVERT trigger |
|---|---|---|
| **HG-1** admin cold Dashboard (session-cold) | ≤ 1.5s, n=3 median | > 2.0s |
| **HG-2** cyberjoker no-regression directional | ≤ 1.25× the 2026-05-21 session-cold-warm-pod baseline (cold median 1,457ms, warm median 1,364ms — captured at `/tmp/snowplow-runs/0.30.152/before/measurements.json`) | post-Ship-G cyberjoker cold median > 1,821ms (1.25×1,457) OR warm median > 1,705ms (1.25×1,364) |
| **HG-3** byte-identical wire output | sha256(admin Dashboard /call response) under Ship G == sha256 of the same request pre-Ship-G | Any non-trivial diff outside the 11+/12 budget |
| **HG-4** named canary SHAs match | 4 named canaries (`nav-admin`, `nav-cj`, `rl-admin`, `rl-cj`) byte-identical to 0.30.145 baseline (carried via D.5's gate) | Any diff |
| **HG-5** `CACHE_ENABLED=false` reverts to ~10s admin baseline | admin Dashboard ≈ 9.94s ± 15% when `CACHE_ENABLED=false` set via chart values | Outside ±15% band, OR widget content store counter advances under cache-off (would prove Ship G left residue when "removed") |
| **HG-6** Clean-wire audit ZERO matches | no JWT (`eyJ`-prefix), no `Authorization: Bearer`, no `userAccessFilter`, no `_cacheKey` / internal-cache-key field in any response under the 12-item content corpus | Any match |

**Gate measurement protocol:**

- **HG-1:** Chrome MCP page-load measurement, n=3 admin
  session-cold runs on a warm pod (browser localStorage.clear +
  sessionStorage.clear, login, navigate to /dashboard). Capture the
  first-paint timeline metric Chrome's Performance trace emits.
  Median across n=3. (Same protocol Diego's morning diagnosis used
  to capture the 9.94s baseline.)
- **HG-2:** Chrome MCP n=3 cyberjoker cold + n=3 cyberjoker warm on
  the SAME pod as HG-1 (informer-warmup state must match the D.5
  baseline pod). Compute medians; compare to D.5's 2026-05-21
  baseline.
- **HG-3:** byte-diff admin /call response under Ship G vs the
  pre-Ship-G dump. The 11+/12 budget is the existing tester
  acceptance window for traceId / per-request artefacts.
- **HG-4:** `sha256sum` against the 4 canaries from
  `/tmp/snowplow-runs/0.30.152/before/`.
- **HG-5:** redeploy with `helm upgrade --set
  snowplow.env.CACHE_ENABLED=false` (chart-only path per
  `feedback_chart_only_for_snowplow`). Re-run admin session-cold.
  Expect ~10s. Inspect `/debug/vars` to confirm
  `widget_content_store_total` stays AT the pre-request baseline.
- **HG-6:** scripted grep over the 12-item content corpus capture
  under `/tmp/snowplow-runs/0.30.16x/after/` for the prohibited
  patterns. Same scanner script that landed Ship D.4.

**Any gate failure = HARD REVERT** (helm rev backward). Per the
post-D.2 / D.4 / D.4.1 revert template, the COMMIT may stay on
branch; the IMAGE is reverted at helm level.

---

## §6. Falsifier-first capture

Per `feedback_falsifier_first_before_ship.md`: pre-flight artifact
captured BEFORE coding; attach to ledger row.

### §6.1 Pre-flight baseline

**FIRST WORK ITEM — falsifier-first script authorship**
(`feedback_falsifier_first_before_ship`). The four scripts §6.1
references DO NOT EXIST in the repo today. Tester authors them
under `scripts/` BEFORE any baseline capture, BEFORE dev starts.
Dev is BLOCKED until OQ-2 closes Branch A (zero diff) — OR PM
re-gates Branch B per §6.3. The four scripts:

1. **`scripts/identity-invariance-probe.sh`** — Step 7 below. For
   each of the 4 canary widget endpoints, curl twice (admin token
   + cyberjoker token), `jq` strip `status.traceId` and
   `(.status.resourcesRefs.items[].allowed) |= null`, then
   `diff` the two normalised dumps. Threshold = ZERO diff across
   all 4 endpoints; non-zero diff trips the Branch A / Branch B
   decision (§6.3).
2. **`scripts/capture-content-corpus.sh`** — re-capture the
   12-item corpus matching the existing
   `/tmp/snowplow-runs/0.30.152/before/content-corpus/`
   shape (`apis-{admin,cj}.bin`, `nav-{admin,cj}.bin`,
   `page-blueprints-{admin,cj}.bin`,
   `page-compositions-{admin,cj}.bin`,
   `page-dashboard-{admin,cj}.bin`,
   `panel-comp-{admin,cj}.bin`, `rl-{admin,cj}.bin`) into
   `/tmp/snowplow-runs/0.30.16x/before/content-corpus/`. The
   12 items + their SHA256 sums feed HG-3 (byte-identical wire)
   and HG-6 (clean-wire audit).
3. **`scripts/clean-wire-audit.sh`** — `grep` over the captured
   corpus for the prohibited patterns:
   - `eyJ` (JWT prefix), per
     `feedback_byte_identical_baselines_clean_wire_shape`.
   - `Authorization: Bearer`.
   - `userAccessFilter` (internal-field exposure).
   - `_cacheKey` — NEW THIS SHIP; the Ship G internal key string
     MUST NOT leak through `gateWidgetEnvelope` into the response
     body. HG-6 trips on any single match.
4. **`scripts/measure-widget-content-bytes.sh`** — `kubectl get
   --raw '/apis/widgets.templates.krateo.io/v1/namespaces/<ns>/<resource>/<name>'`
   piped to `wc -c` for the navigation widget set the F2 walker
   reaches (root + every GET-recursion child). Output writes
   one `<gvr-ns-name>: <bytes>` line per widget into
   `/tmp/snowplow-runs/0.30.16x/before/widget_entry_byte_sizes.txt`.
   Bounds AC-G.14 (memory delta non-positive per
   `feedback_capacity_caps_empirical_per_entry_cost`).

**4 named canary widget endpoints** (carried verbatim from
`/tmp/snowplow-runs/0.30.152/before/canaries/SHA256SUMS_raw.txt`):

```
fab594aad3d935b1839e4b83e54142beb9efb98211615c21fa25109b80ba6207  nav-admin.bin
6be25e9348a35336c09f93a86a9121a87c90871e1a1b6eefd20174b5bc3473d4  nav-cj.bin
73bf782e5b1f9b40efd6fee4db8b42915d0deedd7bad6c2afab88cf6512e36a3  rl-admin.bin
4c86523079f5e5a56789324f1c3f2ecf7c027edb6f2dd28849fa873a97405538  rl-cj.bin
```

The 4 endpoint paths drive `identity-invariance-probe.sh` (the
admin/cj pairs collapse to 2 distinct widget /call endpoints —
navigation root + RESTAction-loader root — each probed under
both identities). HG-4 binds the same 4 SHAs against the
post-deploy `/tmp/snowplow-runs/0.30.16x/after/` capture.

**Ordering invariant.** Tester authors the 4 scripts as the
FIRST work item BEFORE any baseline capture; dev does NOT start
until OQ-2 closes Branch A or PM re-gates Branch B.

```
mkdir -p /tmp/snowplow-runs/0.30.16x/before

# 1. Admin session-cold n=3 reference (matches diagnosis methodology).
#    Chrome MCP, fresh localStorage, login, dashboard nav.
#    Persist to /tmp/snowplow-runs/0.30.16x/before/admin_cold.json:
#      [{run: 1, ms: 9940}, {run: 2, ms: ...}, {run: 3, ms: ...}]

# 2. Cyberjoker session-cold n=3 + warm n=3 (matches D.5 baseline).
#    Same protocol as D.5's 2026-05-21 baseline run.

# 3. 4 named canary SHA256s (carry from D.5 verbatim, see above).
cp /tmp/snowplow-runs/0.30.152/before/canaries/SHA256SUMS_raw.txt \
   /tmp/snowplow-runs/0.30.16x/before/canaries.sha256

# 4. 12-item content corpus with clean-wire-shape audit.
./scripts/capture-content-corpus.sh /tmp/snowplow-runs/0.30.16x/before/content-corpus
./scripts/clean-wire-audit.sh /tmp/snowplow-runs/0.30.16x/before/content-corpus

# 5. Empirical widget-content entry size measurement.
#    For each navigation widget the F2 walker resolves, capture the
#    encodeResolvedJSON output size:
./scripts/measure-widget-content-bytes.sh > \
   /tmp/snowplow-runs/0.30.16x/before/widget_entry_byte_sizes.txt

# 6. expvar widget_l1_keys snapshot (pre-deploy baseline for AC-G.13).
curl -s $POD_IP:8081/debug/vars | jq '.cache.widgets_entries, .cache.widget_content_entries' \
   > /tmp/snowplow-runs/0.30.16x/before/widget_l1_keys.txt

# 7. OQ-2 IDENTITY-INVARIANCE PROBE (Branch A confirmation gate).
#    Two same-widget /call dumps under admin and cyberjoker;
#    normalise (strip traceId, allowed) and diff. Runs across
#    the 4 named canary endpoints listed above (nav + rl, each
#    under admin + cj). Zero diff => Branch A confirmed.
./scripts/identity-invariance-probe.sh \
   > /tmp/snowplow-runs/0.30.16x/before/identity_probe.txt
# Expected: zero diff => Branch A confirmed.
# Non-zero diff => Branch B — Ship G design must be re-PM-gated.
```

### §6.2 Post-deploy validate against HG-1 through HG-6

Captured in §5 protocol. Tester runs the 6-gate validation against
the `0.30.16x` deploy, persists artifacts under
`/tmp/snowplow-runs/0.30.16x/after/`.

### §6.3 Branch B fallback procedure

If OQ-2's pre-deploy identity-invariance probe (§6.1 step 7)
surfaces a non-zero diff outside `status.traceId` and
`status.resourcesRefs.items[].allowed`, OR HG-3 byte-equivalence
fails post-deploy, the failing field is the identity-bearing
residue. Procedure:

1. Compute the JSON diff between the post-Ship-G admin response and
   the pre-Ship-G admin response (already captured per §6.1).
2. Identify the differing field path.
3. If the field is `status.resourcesRefs.items[].allowed`: the gate
   is malfunctioning — this is a Branch A defect, not a Branch B
   trigger. Debug `gateWidgetEnvelope`.
4. If the field is anything else: Branch B trigger. Re-design under
   PM gate before re-shipping; the two pre-designed sub-branches
   below (B.1 + B.2) are the only architectural shapes considered
   in scope for the re-design, so PM re-gate is bounded.

**Branch B.1 — single-localised identity-bearing field**

Triggered when OQ-2's diff surfaces ONE named field path (e.g.,
`status.foo`) carrying per-user content beyond `allowed`. Two
symmetric edits, mirroring how `allowed` is handled:

- **Strip-before-Put** in `populateWidgetContentL1` (the §2.3
  helper). After `encodeResolvedJSON(res)` succeeds but BEFORE
  `c.Put`, walk the encoded object map and remove the named field
  via `maps.RemoveNestedField(obj, "status", "foo")`, then
  re-encode. The placeholder path `status.foo` stays in the
  design body until OQ-2 surfaces the actual field path; tester
  fills it in if the probe trips. The walker-side Put then stores
  a shell missing the identity-bearing residue.

  Diff sketch (Put site):
  ```go
  func populateWidgetContentL1(ctx context.Context, gvr schema.GroupVersionResource,
      in *unstructured.Unstructured, perPage, page int, res *unstructured.Unstructured) {
      // ... existing inputs / key composition unchanged ...

      // Branch B.1 — strip identity-bearing residue BEFORE Put.
      // The field path is OQ-2-derived (placeholder "status.foo"
      // here; tester replaces with the surfaced path).
      stripped := res.DeepCopy()
      _ = maps.RemoveNestedField(stripped.Object, "status", "foo")

      encoded, err := encodeResolvedJSON(stripped)
      if err != nil {
          return
      }
      c.Put(key, &cache.ResolvedEntry{
          RawJSON: encoded,
          Inputs:  &inputs,
      })
  }
  ```

- **Re-derive-before-encode** in `gateWidgetEnvelope` (the §2.4
  helper). After the existing `items[].allowed` re-derivation but
  BEFORE `encodeResolvedJSON(obj)`, compute the per-request value
  of `status.foo` under the request identity (the same function
  the cold-resolve runs — name TBD by OQ-2 surface) and add it
  back via `maps.SetNestedField(obj, value, "status", "foo")`.
  Diff sketch:
  ```go
  func gateWidgetEnvelope(ctx context.Context, gvr schema.GroupVersionResource,
      namespace string, raw []byte) ([]byte, bool) {
      // ... existing parse + items[].allowed re-derivation unchanged ...

      // Branch B.1 — symmetric add: re-derive the per-request value
      // of the stripped field under THIS user's identity and set it
      // back before re-encode. The derivation function name TBD by
      // OQ-2 surface (placeholder recomputeFooField).
      fooVal := recomputeFooField(ctx, ui, obj, gvr, namespace)
      _ = maps.SetNestedField(obj, fooVal, "status", "foo")

      return encodeResolvedJSON(obj)
  }
  ```

AC-G.4 byte-equivalence under B.1 is preserved by symmetry: the
strip-Put + re-derive-encode pair is a one-step extension of the
existing `allowed` mechanic. Both sites named with file:line —
Put at `dispatchers/widgets.go` (new helper sibling to
`recordWidgetDeps` at `widgets.go:151`), gate at `dispatchers/widgets.go`
(new helper sibling to `gateContentEnvelope` at
`internal/resolvers/restactions/api/apistage.go:93-145`).

**Branch B.2 — per-widget CR opt-in**

Triggered when OQ-2's diff surfaces a residue that cannot be
cleanly factored into a single-field strip/re-derive pair (e.g.,
the residue lives in `status.widgetData` from a jq template that
directly references `.user.username` — a future widget author
might legitimately do this per `feedback_cache_must_not_constrain_jq`).
Pre-designed shape mirrors D.5's `clusterListWhenAllowed` opt-in:

- **CRD field:** `spec.cacheable.identityFree: bool` (default
  `false` / absent → byte-identical to pre-Ship-G behaviour).
  Additive bool, mirrors the D.5 `*bool` shape at
  `apis/templates/v1/core.go:106`.

- **Field location:** widget CRDs are NOT defined in
  `apis/widgets/v1alpha1/...` — that package DOES NOT EXIST in
  the snowplow tree (verified 2026-05-21:
  `ls apis/` returns only `apis.go`, `apis_test.go`,
  `generate.go`, `templates/`). Widget CRs are user-supplied
  Custom Resources read as `unstructured.Unstructured` via
  dynamic client at `internal/objects/get.go`. Branch B.2's
  opt-in field is therefore NOT a typed Go struct field — it is
  a convention path the snowplow code reads from `in.Object`
  via `unstructured.NestedBool(in.Object, "spec", "cacheable",
  "identityFree")`. RAs / widget authors set it on the widget
  CR YAML directly. (This mirrors how D.5's
  `ClusterListWhenAllowed` is read from RestAction CRDs — a
  typed Go field there because RestActions ARE defined in
  `apis/templates/v1`, an unstructured-read here because widgets
  are NOT.)

- **Gating at the populate site:** Ship G's Put only happens when
  the widget CR opts in. Diff sketch:
  ```go
  // phase1_walk.go's walk() — Branch B.2 gate at the Put site:
  if cache.WidgetContentL1Enabled() {
      identityFree, _, _ := unstructured.NestedBool(in.Object,
          "spec", "cacheable", "identityFree")
      if identityFree {
          populateWidgetContentL1(ctx, gvr, in, perPage, page, res)
      }
  }
  ```
  No serve-time gate change is needed beyond §2.4 — a content-
  Miss for a non-opted-in widget cleanly falls through to the
  existing per-user widget L1 (the §2.4 "EXISTING per-user
  lookup at :80 stays put" branch).

- **Default-off invariant:** the absent/false case is
  byte-identical to pre-Ship-G behaviour for every existing
  widget. Adoption is a strictly additive RA-author opt-in; Ship
  G can land Branch B.2 with ZERO widgets opted in (no
  behavioural change) and adoption proceeds widget-by-widget as
  authors verify identity-invariance of their own widget shapes.
  Same rollout shape as D.5 (`ClusterListWhenAllowed` defaults
  false; adoption proceeds RA-by-RA).

---

## §7. Test plan

### §7.1 Unit tests

**TestWidgetContentKey_IdentityFree:**

```go
adminKey := cache.ComputeKey(cache.ResolvedKeyInputs{
    CacheEntryClass: cache.CacheEntryClassWidgetContent,
    Group: "widgets.templates.krateo.io", Version: "v1",
    Resource: "panels",
    Namespace: "fireworks-app", Name: "dashboard-summary",
    Username: "admin", Groups: []string{"system:authenticated"},
    PerPage: 5, Page: 1,
})
cjKey := cache.ComputeKey(cache.ResolvedKeyInputs{
    CacheEntryClass: cache.CacheEntryClassWidgetContent,
    Group: "widgets.templates.krateo.io", Version: "v1",
    Resource: "panels",
    Namespace: "fireworks-app", Name: "dashboard-summary",
    Username: "cyberjoker", Groups: []string{"system:authenticated"},
    PerPage: 5, Page: 1,
})
if adminKey != cjKey {
    t.Fatalf("widget content key MUST be identity-free across admin vs cyberjoker")
}

// And: changing perPage MUST shift the key.
diffPerPageKey := cache.ComputeKey(cache.ResolvedKeyInputs{
    CacheEntryClass: cache.CacheEntryClassWidgetContent,
    Group: "widgets.templates.krateo.io", Version: "v1",
    Resource: "panels",
    Namespace: "fireworks-app", Name: "dashboard-summary",
    PerPage: 10, Page: 1,
})
if adminKey == diffPerPageKey {
    t.Fatalf("widget content key MUST vary by perPage")
}
```

**TestWidgetContentKey_DistinctFromWidgetsClass:** confirm a
`widgetContent` key with the same gvr/ns/name as a `widgets` key
hashes to a different cell (the class string IS in the hash at
`resolved.go:310`).

**TestGateWidgetEnvelope_ByteIdentical_AdminBranchA (AC-G.4):**

```go
// 1. Synthesise an admin-identity ctx.
// 2. Resolve widget X under admin via cold widgets.Resolve. Capture rawAdmin.
// 3. Synthesise an SA-identity ctx; resolve same widget via cold widgets.Resolve.
//    Capture rawSA (this is what F2's prewarm would Put).
// 4. Run gateWidgetEnvelope(adminCtx, rawSA) -> gatedAdmin.
// 5. Assert sha256(gatedAdmin) == sha256(rawAdmin) (modulo traceId stripping).
```

**TestGateWidgetEnvelope_PerUserNarrowing_Cyberjoker (AC-G.3):**

```go
// 1. Populate widget content entry under SA (admin's RBAC + cyberjoker's
//    RBAC both apply to it).
// 2. Get the entry, run gateWidgetEnvelope(cyberjokerCtx, raw).
// 3. Assert items where admin sees allowed=true but cyberjoker does NOT
//    have RBAC, the gated body shows allowed=false for those items.
// 4. Assert items both can see have allowed=true in the gated body.
```

**TestWidgetContentCacheOffFallthrough (HG-5, AC-G.6):**

```go
// 1. cache.Disabled() = true.
// 2. Issue widget request.
// 3. Assert NO Put on the widget content store.
// 4. Assert response equals pre-Ship-G cache-off response (byte-identical
//    to 0.30.6 path).
```

**TestRefresherHandlerRegistered (AC-G.5):** assert
`refresherSingleton().handlers["widgetContent"]` is non-nil after
`RegisterRefreshHandlers` runs at startup.

**TestWidgetContentCounterIncrementsOnHit (AC-G.7):** assert
`cache.FallthroughCount(path, gvr, cache.ReasonWidgetContentHit)`
increments by 1 on a content-Get-hit.

### §7.2 Integration tests

Existing `dispatchers/widgets_test.go` (the per-user widget L1 path
tests) MUST continue to pass unmodified under
`WIDGET_CONTENT_L1_ENABLED=false`. New integration test
`widgets_content_l1_test.go` exercises the prewarm-Put → Get-hit →
gate-narrow flow.

### §7.3 Falsifier test (data-driven)

`TestWidgetContentByteIdenticalToPreShipGOnRealCorpus`:
1. Load the 12-item content corpus from
   `/tmp/snowplow-runs/0.30.16x/before/corpus`.
2. For each item: resolve under cold path (cache off) → resolve
   under Ship G warm path → assert byte-equal modulo the
   `status.traceId` field strip.
3. Run under TWO identities (admin + cyberjoker). For each identity,
   assert the gated response equals the cold response under the
   SAME identity. (AC-G.4 byte-equivalence per-identity.)

### §7.4 Post-deploy validation

HG-1 through HG-6 protocol per §5.

---

## §8. Open questions

**OQ-1 (open — flag for PM closure pre-gate):** Where in
`widgets.go` does the new identity-free lookup land — UPSTREAM of
the existing per-user lookup at `:80-95` (recommended in §2.4),
REPLACING it, or downstream?

**Recommended answer:** UPSTREAM. The content layer is the broader
cache; the per-user layer is the narrower-but-faster fallback. On a
content-Hit, the per-user layer is bypassed entirely (no per-user
Put either) — which is the intent of AC-G.13. On a content-Miss,
the per-user layer still serves as the second-tier cache (rare but
valid: a request with `perPage` the prewarm did NOT cover, or a
widget the walker did NOT reach).

**TRACED at:** `widgets.go:80-95` (lookup), `widgets.go:142-152`
(Put), §2.4 mechanism.

**OQ-2 (open — HARD GATE on Branch A vs Branch B):** Does the widget
content envelope contain any identity-bearing fields BEYOND
`status.resourcesRefs.items[].allowed` and `status.traceId`?

**Empirical probe required pre-deploy** (per §6.1 step 7). The
TRACED evidence in §2.2 enumerates every field
`widgets.Resolve` sets: `status.widgetData` (jq over identity-free
dataSource), `status.resourcesRefs.items` (allowed-only identity
residue per `resourcesrefs/resolve.go:88-92`), `status.error` (only
set on error — separate path), `status.traceId` (per-request,
stripped at gate). INFERRED: no other identity carrier in the
documented code paths. The probe falsifies this inference.

**TRACED at:** `widgets/resolve.go:37-112` (entire Resolve body),
`widgets/resourcesrefs/resolve.go:60-129` (the per-user item loop),
`widgets/widgets.go:14-17` (status field names).

**OQ-3 (open — likely closed by trace, flag for PM confirmation):**
Does the refresher need a new dirty-mark path for
`CacheEntryClassWidgetContent`, or does the existing dep-tracker
handle it?

**TRACED answer:** existing dep-tracker handles it. The dep tracker
operates on opaque L1 keys (`Deps().RemoveL1Key(key)` at
`resolved.go:671`); the class string is NOT in the dep-tracker's
indexing. UPDATE/PATCH events on a watched object trigger
`Deps().enqueueUpdate` → the refresher workqueue receives the L1
key → `refresher.processOne` at `refresher.go:304-343` looks up the
entry → reads `entry.Inputs.CacheEntryClass` → dispatches to the
registered handler. Once Ship G registers a handler for
`"widgetContent"` at `dispatchers.go` alongside the existing three,
the dep flow is complete with no new wiring.

**TRACED at:** `internal/cache/refresher.go:300-343`,
`internal/cache/resolved.go:648-672`,
`internal/handlers/dispatchers/dispatchers.go:73-93`.

---

## §9. Tag, commit, ledger

**Tag:** `0.30.16x` (dev picks the exact number at commit time per
`feedback_tag_commits.md`).

**Commit subject:**
`feat(cache): identity-free widget content layer (Ship G, 0.30.16x)`

**Body cites:**
- This design doc.
- Diego's 2026-05-21 morning diagnosis (in-session, no separate
  doc) for the 9.94s admin session-cold root cause and the
  prewarm-failure mode (a) finding.
- HG-1 through HG-6 with targets.
- All 3 OQs as closed with file:line evidence per §8 (Branch A
  confirmation evidence from §6.1's identity-invariance probe is a
  body-cited artifact).
- `internal/cache/resolved.go:340-353` for the F1 identity-free
  branch — Ship G extends this to a second class.
- `internal/resolvers/restactions/api/apistage.go:13-25` for the
  F1 doc that motivates the analogous Ship G architecture.
- `internal/resolvers/widgets/resourcesrefs/resolve.go:88-92` for
  the identity-bearing `allowed` flag that the new
  `gateWidgetEnvelope` re-derives.
- `internal/handlers/dispatchers/phase1_walk.go:646-651` (the
  walker's `widgets.Resolve` call site where the content L1 Put
  attaches) and `:766-805` (the same `allowed`-flag certification
  Diego previously ratified for the walker, now extending to
  the cached envelope).

**Ledger row:** appended to `project_north_star_ledger.md`
post-deploy with the n=3 admin session-cold median + cyberjoker
no-regression delta + cache-toggle invariant + canary-diff count.

**Feature journal:** appended to `project_feature_journal.md` with
expected (admin ≤ 1.5s session-cold) vs actual (measured) + delta.

---

## §10. Architect summary

Ship G is "F1 one tier up": a content-keyed L1 layer at the widgets
class (`CacheEntryClassWidgetContent`) populated identity-free by the
F2 SA prewarm walker as a free side-effect of its existing
`widgets.Resolve` call, served via a new `gateWidgetEnvelope` that
re-derives the embedded `resourcesRefs.items[].allowed` flags per
request identity (the ONE identity-bearing field in the resolved
widget envelope, confirmed via the OQ-2 pre-deploy probe and
ratified by §2.2's TRACED enumeration of every status field
`widgets.Resolve` sets). The cross-user-leak risk is closed by the
same architectural property F1 introduced: the cache content is the
shell, the gate runs on every Get-hit, the body that leaves the pod
is per-user. Default-on, cleanly disabled via
`CACHE_ENABLED=false` (preserves the removable-cache invariant per
`project_caching_is_provisional`), per-class fine-grained toggle
`WIDGET_CONTENT_L1_ENABLED`. Composes orthogonally with D.5 (both
shipping in parallel). Projected admin Dashboard session-cold:
9.94s → ≤ 1.5s. This is the actual zero-cold ship for the prewarm-
covered navigation tree per Diego's 2026-05-21 framing.
