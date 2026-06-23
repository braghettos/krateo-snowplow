# Caching architecture — the three-tier cache

> **Audience:** snowplow maintainers.
> **Scope:** the read-path cache that lets `/call` serve resolved `RESTAction` / `Widget`
> JSON without re-hitting the apiserver on every request.
> **Status:** code-traced against commit `a42b072` (`main`). Every claim below cites a
> `file:line` in the current tree; where a working note disagreed with the code, the code won.

Snowplow stacks three caches in the request path. They are layered, not alternatives:

- **L3 — informer cache** (`internal/cache/watcher.go`): an in-process Kubernetes informer
  store. Resolvers read raw cluster objects from here instead of the apiserver.
- **L1 — resolved-entry cache** (`internal/cache/resolved.go`): a bounded LRU of the
  *already-resolved, pre-encoded JSON* a `/call` returns. A hit skips the whole resolver.
- **Dispatcher cache seam** (`internal/handlers/dispatchers/`): the request-handler logic that
  computes the L1 key, consults L1, and on a miss resolves + populates it. This is the "tier"
  where keying, the RBAC gate ordering, and the widget two-key fast-path live; the *store* it
  uses is L1.

The whole stack is gated behind one master toggle, `CACHE_ENABLED`
(`internal/cache/cache.go:37`). With it off, all three tiers vanish and every read goes straight
to the apiserver — same data, same UI, same RBAC, just slower (see *Invariants*).

---

## 1. Data flow

```
                         GET /call?resource=widgets&name=…&extras={…}
                                         │
                                         ▼
                    ┌──────────────────────────────────────────────┐
                    │  DISPATCHER SEAM  (handlers/dispatchers/)      │
                    │  widgets.go / restactions.go                   │
                    │                                                │
                    │  1. fetch dispatched CR (objects → L3)         │
                    │  2. EvaluateRBAC gate  (cache=on only)         │ restactions.go:96
                    │     ── deny → 403, never cached ───────────────│
                    │  3. compute L1 key + consult L1                │
                    └──────────────────────────────────────────────┘
                                         │
        ┌────────────────────────────────┼─────────────────────────────────┐
        │ WIDGET fast-path (widgets.go)   │                                  │
        │                                 │                                  │
   (a) widgetContent key  ──hit──▶ gateWidgetEnvelope (re-stamp `allowed`)   │
       identity-FREE               ──▶ write per-user body, return           │
       (gvr,ns,name,page,extras)        widgets.go:134-164                   │
            │ miss / RBAC-sensitive widget → fall through                    │
            ▼                                                                │
   (b) widgets key (per-binding) ──hit──▶ write RawJSON, return              │
       BindingUID folded in              widgets.go:183-196                  │
            │ miss                                                           │
        ────┴────────────────────────────────────────────────────────────┐ │
                                         │ (restactions: single per-binding key)
                                         ▼                                  │
                    ┌──────────────────────────────────────────────┐       │
                    │  L1  ResolvedCacheStore  (resolved.go)         │◀──────┘
                    │  Get(key) → (*ResolvedEntry, bool)             │  resolved.go:738
                    └──────────────────────────────────────────────┘
                                         │ miss
                                         ▼
                    ┌──────────────────────────────────────────────┐
                    │  RESOLVER  (resolvers/…)                       │
                    │  reads cluster objects from ──────────────────┼──▶ L3 informer
                    │                                                │   watcher.go:1609 GetObject
                    │                                                │   watcher.go:1669 ListObjects
                    │  records dep edges as it reads (WithL1Key)     │   deps.go:421 Record / :443 RecordList
                    └──────────────────────────────────────────────┘
                                         │
                                         ▼
                    encode once → write to response
                    → L1.Put(key, entry)  + Deps().Record(self-edge)
                       restactions.go:264-281

  ── invalidation (async, informer event → DepTracker) ────────────────────────
     L3 informer event ──▶ depEventHandlers ──▶ Deps().OnAdd / OnUpdate / OnDelete
       (watcher.go AddFunc/UpdateFunc/DeleteFunc → deps.go:504/516/590)
       ADD/UPDATE → dirty-mark (refresher re-resolve)   deps.go:524 onChange
       DELETE     → evict self-entry, dirty-mark deps    deps.go:590 OnDelete
```

---

## 2. L3 — the informer cache (`watcher.go`)

`ResourceWatcher` (`internal/cache/watcher.go:298` `NewResourceWatcher`) owns a
`dynamicinformer.DynamicSharedInformerFactory` (`watcher.go:344`) plus a per-GVR map of
`GenericInformer`s (`watcher.go:118`). Resolvers do not call the apiserver directly; they call:

- `GetObject(gvr, ns, name)` — `watcher.go:1609`. In `modeInformer` it reads
  `gi.Informer().GetIndexer().GetByKey(...)` (`watcher.go:1639`) — an in-memory map lookup, no
  network. In `modePassthrough` (cache off) it falls back to a live `rw.dyn.Resource(gvr).Get`
  apiserver call (`watcher.go:1616-1618`).
- `ListObjects(gvr, ns)` — `watcher.go:1669`. `modeInformer` materialises the namespace
  partition from the indexer (`listFromIndexer`, `watcher.go:1680/1683`); `modePassthrough`
  issues a fresh paged apiserver LIST (`listPassthrough`, `watcher.go:1670-1671`) with **no**
  in-process caching.

The mode split is the L3 half of the toggle invariant: same `GetObject`/`ListObjects` surface,
two backing implementations chosen once at construction. `NewResourceWatcher` logs
`"CACHE_ENABLED=false — typed-RBAC + informer cache + L1 ALL disabled"` and sets
`informer.get_list_path=apiserver` when the toggle is off (`watcher.go:316-323`).

Each informer also registers DepTracker event handlers (`watcher.go:845`
`gi.Informer().AddEventHandler(rw.depEventHandlers(gvr))`) so cluster mutations drive L1
invalidation (§5). The handler contract is `UpdateFunc → Deps().OnUpdate`,
`DeleteFunc → Deps().OnDelete` (`watcher.go:571-572`, `753-754`).

---

## 3. L1 — the resolved-entry cache (`resolved.go`)

`ResolvedCacheStore` (`resolved.go:393`) is a single-mutex bounded LRU: a
`container/list` for recency order + a `map[string]*list.Element` index (`resolved.go:397-399`),
with three caps — `maxEntries`, `maxBytes`, `ttl` (`resolved.go:401-403`) — plus a separate
*pinned* resident byte budget (`maxResidentBytes`, `resolved.go:415`) for expensive prewarmed
entries that LRU pressure must not evict.

The value is a `ResolvedEntry` (`resolved.go:185`): pre-encoded `RawJSON` bytes ready to write,
a `CreatedAt` for TTL, and the canonical `Inputs *ResolvedKeyInputs` the refresher re-resolves
from. Storing the *encoded* form (not the live object) keeps the hit path race-free — readers
get an immutable `[]byte` (`resolved.go:177-184`).

- `Get` (`resolved.go:738`): index lookup; a TTL-expired entry is dropped and counted as a miss
  in the same call (`resolved.go:751-756`); a hit moves the element to the LRU front
  (`resolved.go:758`).
- `Put` (`resolved.go:767`): stamps `CreatedAt`, computes `entryBytes` (`resolved.go:912`),
  resolves final pin status under `mu` (`resolved.go:783-799`), then inserts and evicts the LRU
  tail until under caps.

### 3.1 Key structure — `ComputeKey`

`ComputeKey(in ResolvedKeyInputs) string` (`resolved.go:608`) is a hex-encoded SHA-256 over a
versioned, NUL-delimited byte stream. The fields folded in, in order (`resolved.go:609-691`):

1. `resolvedKeyVersion` salt — currently `"v4"` (`resolved.go:384`). Bumping it rotates the
   entire key space on a rolling restart so no stale-shape entry ever serves as a hit.
2. `CacheEntryClass` — the entry-class discriminant (`resolved.go:277`), one of
   `"restactions"`, `"widgets"`, `"apistage"`, `"widgetContent"`, `"raFullList"`. The string
   *values* are load-bearing (hashed into the key + used as refresher registry keys).
3. The dispatched object's `Group / Version / Resource / Namespace / Name`
   (`resolved.go:278-282`).
4. **Identity** — `BindingUID` (`resolved.go:303`), folded in for *every class except*
   `widgetContent` (`resolved.go:652-655`). This is the load-bearing keying decision (§3.2).
5. `PerPage` / `Page` (`resolved.go:657-660`).
6. `Stage` — only for `apistage` entries; written with a `0x01` sentinel and skipped when empty
   so non-apistage keys hash byte-identically across the field's introduction
   (`resolved.go:669-673`).
7. `Extras` — canonicalised via `canonicaliseExtras` (`resolved.go:697`): a recursively
   sorted-key JSON surrogate, so two requests with the same extras content but different map
   iteration order produce the same key, and distinct extras produce distinct entries (no
   cross-request collision). On marshal failure it falls back to a deterministic
   `fmt.Sprintf("%v", …)` (`resolved.go:686`).

`RepresentativeUsername` / `RepresentativeGroups` are carried on `Inputs` but **excluded from
`ComputeKey`** (`resolved.go:322-327`) — they are bookkeeping for the refresher's re-resolve,
not key material; two members of the same equivalence class must not shift the cell's identity.

### 3.2 The two widget L1 keys

A widget `/call` can hit L1 by two different keys, tried in order in `widgets.go`:

1. **Identity-free content key** (`CacheEntryClassWidgetContent`, `resolved.go:147`). Built by
   `dispatchWidgetContentKey` (`helpers.go:147`) with `Username`/`Groups` left zero;
   `ComputeKey` skips the identity fold for this class (`resolved.go:652`). So admin and a
   narrow-RBAC user hit the **same cell**, keyed only on
   `(gvr, ns, name, perPage, page, extras)`. The stored body is a *shell* with SA-evaluated
   `status.resourcesRefs.items[].allowed` flags; it is **never served verbatim** — on hit,
   `gateWidgetEnvelope` re-stamps every `allowed` flag under the request's own identity before
   the body is written (`widgets.go:139-153`, rationale at `resolved.go:127-147`). This path is
   skipped for RBAC-sensitive apiRef widgets (those whose `status.widgetData` is RBAC-narrowed
   and would leak the SA-maximal aggregate) — `isRBACSensitiveApiRefWidget`, `widgets.go:134`.

2. **Per-binding key** (`CacheEntryClass=="widgets"`). Built by `dispatchCacheLookupKey`
   (`helpers.go:200`), which calls `rbac.EvaluateRBAC` to derive the **`BindingUID`** of the
   first-match binding that authorised this layer's GET (`helpers.go:212-220`) and folds it into
   the key (`helpers.go:228`). Two users granted by the *same* binding share one cell; a deny or
   error fails closed to `bindingUID=""` (`helpers.go:197-199`) — the same empty-identity row
   that cache-off produces. RESTActions use only this per-binding path (`restactions.go:123`).

The widget fast-path falls from (1) to (2) to a full resolve on successive misses
(`widgets.go:159-196`). The `BindingUID` derivation site of record is
`cache.BindingUIDFromCRB` / `BindingUIDFromRB` in `match_subject.go` (`resolved.go:300-302`);
prefixes `"C:<uid>"` / `"R:<ns>/<uid>"` keep ClusterRoleBinding and RoleBinding UIDs from
aliasing and carry namespace scope into the identifier (`resolved.go:289-298`).

### 3.3 Value-dedup

Dedup here is **cell-sharing by equivalence class**, not byte-interning:

- *Across users*: per-binding keying means every user authorised by the same binding resolves to
  byte-identical output and lands on one cell (`resolved.go:312-320`) — the
  per-user-keyed-never-cohort invariant satisfied at binding granularity.
- *Across pages*: the `raFullList` class (`resolved.go:149-175`) caches the RA's full
  unpaginated result with `PerPage/Page` forced to 0; every paginated `/call` differing only in
  slice shares that one cell and the page is applied as a cheap Go-slice at serve time
  (`ra_full_list_slice.go`). Widgets feeding the same RA under the same binding share the same
  cell — "the chokepoint dedupe across widgets" (`resolved.go:160-163`).
- *Across cohorts (widgets)*: the identity-free content cell collapses all cohorts onto one
  stored shell, re-personalised at serve time (§3.2).

---

## 4. The dispatcher seam (`handlers/dispatchers/`)

`/call` routes to a per-kind handler from the `dispatchers.All()` registry
(`dispatchers.go:14`). The handler (`restactions.go`, `widgets.go`) is where the cache is
*consulted* — the ordering here is load-bearing:

1. Fetch + convert the dispatched CR (reads L3).
2. **EvaluateRBAC gate runs BEFORE the L1 lookup** (`restactions.go:96-108`,
   `widgets.go` equivalent) so a cache hit can never short-circuit the permission check
   (`restactions.go:112-116`). Cache-off skips this in-process gate because the per-user
   apiserver fetch enforces RBAC inline (`restactions.go:92-95`).
3. Compute key + `Get`; on hit, `writeResolvedJSON(entry.RawJSON)` and return
   (`restactions.go:135-147`).
4. On miss: attach `WithL1KeyContext(ctx, cacheKey)` (`restactions.go:194-195`) so the resolver
   records dep edges against this key as it reads L3; resolve; encode once; serve; then `Put`
   **gated on zero per-item stage errors** (`restactions.go:249-267`) — a partial-with-errors
   body is served (200) but never persisted, so transient item failures self-heal on the next
   resolve. Finally `Deps().Record(cacheKey, gvr, ns, name)` records the self-edge
   (`restactions.go:281`) after `ensureWatcherInformerForGVR` guarantees the GVR's informer is
   wired (`restactions.go:280`).

The refresher hooks are registered once at startup (`dispatchers.go:73-112`
`RegisterRefreshFunc` for each class), all pointing at the shared `resolveAndPopulateL1`, which
re-resolves an entry from its own `Inputs` and re-`Put`s — it only ever `Put`s, never `Get`s.

---

## 5. Invalidation (`deps.go`)

L1 entries are invalidated by L3 informer events flowing through `DepTracker` (`deps.go:369`
`Deps()`). The tracker holds a forward index (DepKey → set of L1 keys) and a reverse index
(L1 key → set of DepKeys) (`deps.go:458-491`). Dependencies are recorded at resolve time:

- `Record(l1Key, gvr, ns, name)` — exact-object edge (`deps.go:421`).
- `RecordList(l1Key, gvr, ns)` — list-scope edge encoded as the `(gvr, ns, "*")` wildcard
  bucket (`deps.go:443-453`).

The three event handlers enforce the **invalidation rules** (header `deps.go:14-18`):

- **ADD / UPDATE → dirty-mark only, never evict.** `OnAdd` and `OnUpdate` both call
  `onChange` (`deps.go:504-518`), which enqueues every dependent L1 key into the refresher for
  stale-while-revalidate (`deps.go:524-559`). ADD is treated identically to UPDATE because a
  freshly-created object can satisfy a LIST-dep that previously resolved empty
  (`deps.go:497-502`).
- **DELETE → three-way classification** (`OnDelete`, `deps.go:590`):
  1. *self-representation* — the entry's own dispatched object is the deleted object →
     **EVICT** (the only authorised eviction trigger). Classified by `isSelfRepresentation`,
     which reads the entry's `Inputs` and compares GVR/ns/name (`deps.go:658-669`).
  2. *LIST-dep* — matched via the `(gvr, ns, "*")` wildcard; the entry's own object still
     exists but a list member went away → **DIRTY-MARK**.
  3. *dependent-GET-dep* — matched via an exact bucket but the entry's own object is a
     *different* object (e.g. a widget GET-depending on a deleted RESTAction) → **DIRTY-MARK**.
  Buckets 2 and 3 take the identical action, so `OnDelete` only needs the self-vs-non-self split
  (`deps.go:607-624`); it returns the evicted count and dirty-marks the rest (`deps.go:625-645`).
  `isSelfRepresentation` fails conservatively to `false` when the store/entry/Inputs is missing
  (`deps.go:653-665`) — missing an eviction merely leaves a stale entry until TTL, whereas
  over-eviction is the regression the falsifier catches.

This is the precise statement of the plan's rule: **DELETE evicts only the deleted object's own
entry; LIST-deps and dependent GET-deps are dirty-marked; ADD/UPDATE dirty-mark.** TTL
(`resolved.go:751`) is the outer safety net for any change the dep tracker cannot see.

The tracker and the store are kept in lock-step: `Deps().SetStore(...)` wires the L1 store
(`resolved.go:572`), and every L1 eviction path (LRU/TTL/DELETE) calls `Deps().RemoveL1Key` so
dep records never outlive their entry. The dep-record forward map is bounded by `maxRecords`;
on cap it drops new records silently and relies on TTL for correctness (`deps.go:466-480`).

---

## 6. Invariants

1. **Provisionality / toggle (transparent fallback).** `CACHE_ENABLED=false`
   (`cache.go:37` `Disabled()`) must be a transparent fallback to the direct apiserver path —
   **same data, same UI, same RBAC, only slower** — not a degraded mode. It is enforced at every
   tier: L3 switches to `modePassthrough` live apiserver reads (`watcher.go:1610`, `1670`); L1's
   `ResolvedCache()` returns `nil` and every consumer nil-checks and resolves directly
   (`resolved.go:547-550`, `484-494`); the dispatcher's in-process EvaluateRBAC gate is skipped
   because per-user apiserver fetches enforce RBAC inline (`restactions.go:92-108`). The whole
   subsystem stays cleanly removable per `cache.go:10-13`. `CACHE_ENABLED` is the single master
   gate — prewarm, the informer-serve pivot, and the api-stage L1 (Ship E) are implicit under it
   (the latter folded per #57; `RESOLVED_CACHE_APISTAGE_ENABLED` retired); only fine-grained
   back-out knobs (`RESOLVED_CACHE_ENABLED`, `WIDGET_CONTENT_L1_ENABLED`)
   remain (`cache.go:15-24`).
2. **RBAC is never short-circuited by a hit.** The EvaluateRBAC gate runs *before* the L1 lookup
   (`restactions.go:96-116`); the identity-free widget cell is re-personalised per request by
   `gateWidgetEnvelope` (`widgets.go:139-153`) and is bypassed entirely for RBAC-sensitive
   `widgetData` widgets (`widgets.go:134`). The cached body is the shell; the body that leaves
   the pod is per-user.
3. **Per-user (per-binding) keying, never cohort-only.** Identity-bound classes fold `BindingUID`
   (`resolved.go:652-655`); only `widgetContent` is identity-free, and only because its served
   body is re-stamped per request. Every member of a `BindingUID` equivalence class resolves to
   byte-identical output (`resolved.go:312-320`).
4. **DELETE-only eviction.** UPDATE/ADD use stale-while-revalidate dirty-marking; eviction is
   reserved for a DELETE of an entry's own object (`deps.go:14-18`, `590`).
5. **Never persist an under-served result.** `Put` is gated on zero per-item stage errors
   (`restactions.go:249-267`), so a partial body is served but never pins itself for the TTL.
6. **No per-resource special cases.** Key shape is per *class*, uniform across every GVR
   (`resolved.go:646-651`); behaviour is expressed via the entry-class discriminant, not
   hardcoded resource names.

---

## 7. Known failure modes

- **Seed→serve key divergence.** If the prewarm seed `Put`s under a different `BindingUID`
  (or extras / page) than the dispatcher `Get` computes, the warm cell is missed and the
  request falls through to a cold resolve. The `emitDispatchCacheKeyDiag` lines
  (`helpers.go:281-339`, sites `dispatcher_get` / `per_user_fallback_put` and the PIP-seed Put)
  exist specifically to diff the folded components for one object. Symptom: `l1=miss` on a
  request you expected to be warm; check the `binding_uid` field across the three sites.
- **Stale content past dirty-mark.** Dirty-marking only *enqueues* a refresher re-resolve; until
  the refresher runs, a hit serves the prior bytes (stale-while-revalidate by design). A wedged
  or back-pressured refresher leaves stale content until TTL.
- **Dep-record cap drop.** Past `maxRecords`, new dep edges are dropped silently
  (`deps.go:466-480`) and a one-shot WARN (`deps.cache.cap_reached`) fires; affected entries then
  rely on TTL rather than event-driven invalidation.
- **Conservative under-eviction on DELETE.** When `isSelfRepresentation` cannot read the entry's
  `Inputs` (e.g. legacy entry, store not wired) it returns `false` and the entry is dirty-marked
  instead of evicted (`deps.go:653-665`) — correct-but-slower; the entry clears at TTL.
- **Pin demotion under resident-budget pressure.** A `Put` that requests `Pinned` but does not
  fit the resident byte budget (or `maxResidentBytes==0`) is demoted to transient and counted in
  `residentDemoteTotal` (`resolved.go:783-799`, `460`) — an expensive prewarmed cell can then be
  LRU-evicted, reintroducing a cold navigation.

---

## 8. File map

| Concern | File | Key anchors |
|---|---|---|
| Master toggle | `internal/cache/cache.go` | `Disabled()` :37 |
| L1 store, keys, dedup | `internal/cache/resolved.go` | `ComputeKey` :608, `canonicaliseExtras` :697, `ResolvedKeyInputs` :271, classes :125/:147/:175, `Get` :738, `Put` :767 |
| Invalidation | `internal/cache/deps.go` | `Record` :421, `RecordList` :443, `OnAdd/OnUpdate` :504/:516, `onChange` :524, `OnDelete` :590, `isSelfRepresentation` :658 |
| L3 informer | `internal/cache/watcher.go` | `NewResourceWatcher` :298, `GetObject` :1609, `ListObjects` :1669, dep handlers :845 |
| Dispatcher seam — keys | `internal/handlers/dispatchers/helpers.go` | `dispatchCacheLookupKey` :200, `dispatchWidgetContentKey` :147, diag :315 |
| Dispatcher seam — RESTAction | `internal/handlers/dispatchers/restactions.go` | RBAC gate :96, L1 lookup :135, Put :264 |
| Dispatcher seam — Widget | `internal/handlers/dispatchers/widgets.go` | content fast-path :134, per-binding lookup :170 |
| Refresher wiring | `internal/handlers/dispatchers/dispatchers.go` | `RegisterRefreshFunc` :73-112 |
