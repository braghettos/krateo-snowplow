# RBAC & User-Aware Filtering (UAF)

**Audience:** maintainer · **Status:** code-traced against commit `a42b072` (snowplow 1.0.1 line)
· **Subsystem:** `internal/rbac/` (evaluator) + `internal/cache/` (snapshot, subject index, L1
keying) + the serve-time filter sites in `internal/resolvers/` and `internal/objects/`.

Snowplow serves Krateo frontend content out of an in-process cache. Every byte that leaves the pod
must be exactly what the *requesting user* is permitted to see — no more. This document traces how
that guarantee is enforced without a per-request `SubjectAccessReview` to the apiserver, and
captures the one invariant whose violation caused a six-revert retrospective: **the L1 resolved
cache must be keyed by the binding that authorised the request, never by a cohort/content key
alone.**

---

## 1. Flow diagram

```
                                  /call  (request carries UserInfo: Username + Groups)
                                    │
        ┌───────────────────────────┴───────────────────────────────┐
        │                                                             │
   (A) CACHE-KEY DERIVATION                                  (B) SERVE-TIME UAF
   dispatchCacheLookupKey                                    per-item gate on the served body
   helpers.go:200                                            (list/get drop, widget allowed-flag)
        │                                                             │
        │ EvaluateRBAC(verb=get, …, SkipBindingUID=false)             │ EvaluateRBAC(verb=list/get,
        │   → (allowed, matchedBindingUID)                            │   SkipBindingUID=true)
        ▼                                                             ▼   → allowed only
   BindingUID folded into ComputeKey  ───────────► L1 cell        keep / drop each item
   (resolved.go:608 ComputeKey)                    keyed by       (refilter.go, informer_serve.go,
        │                                          BindingUID     informer_dispatch_rbac.go,
        │                                          NOT cohort     cluster_list.go)
        ▼                                                             │
   two users on the SAME binding share a cell                        │
   two users on DIFFERENT bindings get DIFFERENT cells               │
                                                                      ▼
                                                              body that leaves the pod
                                                              is per-user-correct

        ┌──────────────────────── EvaluateRBAC (evaluate.go:167) ─────────────────────────┐
        │  cache.Disabled()?  ──yes──►  UserCan → SelfSubjectAccessReview (SAR baseline)   │
        │       │ no                                  (rbac.go:65 userCanViaSAR)           │
        │       ▼                                                                          │
        │  rw.Snapshot()  (cache.RBACSnapshot, single Load, threaded through the walk)     │
        │       │ nil → degrade-to-DENY (evaluate.go:198 / :216)                           │
        │       ▼                                                                          │
        │  L2 authz memo lookup  (snapshot_authz_memo.go, keyed by snap.PublishSeq)        │
        │       │ hit → return cached (allowed, uid)                                       │
        │       ▼ miss                                                                     │
        │  evaluateAgainstInformerFirstMatch (evaluate.go:334)                             │
        │       1. selectCRBCandidates (subject index union)  ──► anySubjectMatches        │
        │       2. selectRBCandidates (ns-scoped)             ──► roleRefPermits           │
        │                                                          → rulesPermit           │
        │       first permit wins (RBAC v1 has no deny rules)                              │
        │  store PERMITS only (deny is never cached) → return                              │
        └──────────────────────────────────────────────────────────────────────────────┘
```

The two halves are deliberate. **(A)** decides *which cache cell* a request reads/writes. **(B)**
decides *which items inside a served body* the user keeps. Both call the same evaluator; they differ
only in whether they consume `matchedBindingUID` (A) or just `allowed` (B).

---

## 2. The evaluator — trace through `internal/rbac/`

### 2.1 Entry points: `UserCan` and `EvaluateRBAC`

`UserCan` (`rbac.go:32`) is the per-item yes/no API. With cache on it delegates to `EvaluateRBAC`
and discards the binding UID (`rbac.go:44`); with cache off it falls to `userCanViaSAR`
(`rbac.go:59`), which issues a `SelfSubjectAccessReview` against the user's own kubeconfig
(`rbac.go:86-103`). That SAR path is the correctness baseline that keeps `CACHE_ENABLED=false` a
transparent fallback, and `userCanViaSAR` "MUST be reachable only when `cache.Disabled() == true`"
(`rbac.go:62-64`).

`EvaluateRBAC` (`evaluate.go:167`) is the core. Signature: `(bool allowed, string
matchedBindingUID, error)` (`evaluate.go:120-166`). Decision order:

1. **`cache.Disabled()`** → route to `UserCan`/SAR, return `("", )` empty UID (`evaluate.go:171-183`).
   No snapshot exists, so there is no binding to identify.
2. **`rw := cache.Global()` nil** → cache flagged on but watcher not wired: **degrade to deny**, do
   *not* silently fall back to the apiserver (`evaluate.go:185-199`). Falling back here would violate
   the "zero `SubjectAccessReview` in cache=on" rule.
3. **`snap := rw.Snapshot()` nil** → typed-RBAC snapshot not yet published: **degrade to deny**
   (`evaluate.go:206-217`). A single `Snapshot()` load is threaded through the entire walk so one
   evaluation observes one coherent snapshot generation (`evaluate.go:201-206`).
4. **L2 authz memo lookup** keyed on `snap.PublishSeq` (`evaluate.go:231-250`) — see §2.5.
5. **Cold walk** via `evaluateAgainstInformerFirstMatch` (`evaluate.go:252`).
6. **Store PERMITS only** (`evaluate.go:282-289`) — see §2.5; denies are never cached.

### 2.2 The candidate walk — `evaluateAgainstInformerFirstMatch`

`evaluate.go:334`. Two phases, mirroring the upstream Kubernetes RBAC authorizer:

- **Phase 1 — ClusterRoleBindings.** `selectCRBCandidates(snap, opts)` returns a superset of the
  matching CRBs from the subject index (`evaluate.go:340`, §2.3). When the caller consumes the UID
  (`SkipBindingUID == false`) the candidates are stable-sorted by `(Name, UID)` via `sortCRBsStable`
  (`evaluate.go:344-346`, `:397`) so the first match is deterministic. Each candidate is gated
  through `anySubjectMatches` (the post-index correctness barrier, `evaluate.go:352`) and then
  `roleRefPermits` (`evaluate.go:355`). **First permit wins**, returning `cache.BindingUIDFromCRB(crb)`
  (`evaluate.go:359-361`).
- **Phase 2 — RoleBindings in `opts.Namespace`,** only when `Namespace != ""` (`evaluate.go:368`).
  Same structure via `selectRBCandidates` / `sortRBsStable` / `BindingUIDFromRB`
  (`evaluate.go:369-384`).

No match in either phase → `(false, "", nil)` (`evaluate.go:387`). There are **no deny rules in RBAC
v1**, so any single matching permit short-circuits the whole walk — which is why the candidate sort
is purely for UID determinism and never changes the yes/no verdict (`evaluate.go:341-343`).

### 2.3 The subject index — `selectCRBCandidates` / `selectRBCandidates`

The index lives on the snapshot (`cache/rbac_snapshot.go:157-169`) and is built once per rebuild by
`rebuildSubjectIndexes` (`cache/rbac_snapshot.go:536`). It maps a subject to the bindings that name
it:

| Subject kind | CRB index field | RB index field (per-ns) |
|---|---|---|
| `User` | `CRBsByUser[name]` | `RBsByUserByNS[ns][name]` |
| `Group` | `CRBsByGroup[name]` | `RBsByGroupByNS[ns][name]` |
| `ServiceAccount` | `CRBsByServiceAccount["<ns>/<name>"]` | `RBsByServiceAccountByNS[ns]["<ns>/<name>"]` |
| unrecognised kind | `CRBsCatchAll` | `RBsCatchAllByNS[ns]` |

`selectCRBCandidates` (`evaluate.go:453`) unions the routes that apply to the request identity,
deduplicating by pointer (a multi-subject binding appears once, `evaluate.go:466-476`):

1. `CRBsByUser[Username]` when `Username != ""` (`evaluate.go:478-480`)
2. `CRBsByGroup[g]` for every `g` in `effectiveGroups(opts)` (`evaluate.go:481-483`)
3. `CRBsByGroup["system:authenticated"]`, gated on `Username != ""` so an unauthenticated identity
   never lands the implicit group (`evaluate.go:488-490`)
4. `CRBsByServiceAccount["<ns>/<name>"]` when the username is a canonical SA (`evaluate.go:491-493`)
5. `CRBsCatchAll` — always (`evaluate.go:497`)

The index is a **pre-filter only**: it is allowed to over-include, and `anySubjectMatches`
(`evaluate.go:694`) is the authoritative equality barrier after lookup
(`cache/rbac_snapshot.go:149-152`). The hard invariant is the *other* direction: the index must
never **under**-include — a binding the linear scan would match must appear in the candidate union,
or a permit is silently lost. The `CRBsCatchAll` arm exists exactly so an unrecognised future
`Subject.Kind` cannot be dropped (`evaluate.go:494-497`). `selectRBCandidates`
(`evaluate.go:507`) is the namespace-scoped analogue; a missing namespace returns nil and yields an
empty candidate set with no allocations (`evaluate.go:502-506`).

### 2.4 Predicate symmetry: User AND Group (and ServiceAccount)

The load-bearing symmetry rule is that **every place the verdict depends on a subject kind, both the
index route and the matcher route handle User, Group, and ServiceAccount the same way.** If the
index routes a User but the matcher only checks Groups (or vice-versa), the answer is wrong. The
matcher `anySubjectMatches` (`evaluate.go:694`) is the canonical statement of the predicate:

- `UserKind` → exact `s.Name == opts.Username` (`evaluate.go:700-703`)
- `GroupKind` → membership test against `effectiveGroups`, **plus** the implicit
  `system:authenticated` for any authenticated identity (`evaluate.go:704-715`)
- `ServiceAccountKind` → `(Namespace, Name)` match on a parsed canonical SA username
  (`evaluate.go:716-719`)
- any other kind → no case → no match (`evaluate.go:698-721`)

`effectiveGroups` (`evaluate.go:743`) is the single source of group expansion: for a ServiceAccount
identity it appends the two Kubernetes synthetic groups `system:serviceaccounts` and
`system:serviceaccounts:<ns>` (`evaluate.go:728-730`, `:743-754`); for a non-SA identity it returns
`opts.Groups` unchanged with no allocation. Crucially **`selectCRBCandidates` reuses the same
`effectiveGroups`** (`evaluate.go:462`, `:481`) so the index and the matcher agree on group
expansion — the symmetry is enforced by shared code, not by two parallel implementations.

`rulesPermit` (`evaluate.go:597`) walks the resolved role's `PolicyRule`s with Kubernetes wildcard
semantics (`stringSliceMatches`, `evaluate.go:670`: `"*"` matches everything) over Verbs, APIGroups,
Resources, and honours `resourceNames`. `resourceNameMatches` (`evaluate.go:651`) implements the
upstream `ResourceNameMatches` rule: a non-empty `rule.ResourceNames` can only ever grant a
**name-specific verb** (`get`/`update`/`patch`/`delete`, `nameSpecificVerbs` at `evaluate.go:624`)
and only when `opts.Name` is in the list — a `resourceNames`-scoped rule **never** grants `list`.
This was added in 0.30.109; its absence had over-exposed every object on a `resourceNames`-scoped
rule — a cross-user leak in `filterListByRBAC` (`evaluate.go:591-596`).

### 2.5 The L2 authz memo — `snapshot_authz_memo.go`

The candidate walk at 50K scale re-walks the same ~18K same-subject CRBs on every call for a verdict
that repeats thousands of times within a generation. The L2 memo collapses that
(`snapshot_authz_memo.go:1-12`). Properties that keep it correct:

- **Generation-scoped.** A single `atomic.Pointer[snapshotAuthzShard]`; each shard carries the
  `snap.PublishSeq` it is valid for (`snapshot_authz_memo.go:108-116`). Lookup compares the shard
  gen to the `PublishSeq` of the snapshot the caller *already holds* — no second snapshot load, no
  TTL (`evaluate.go:231-242`, `snapshot_authz_memo.go:22-34`). A `PublishSeq` change CAS-swaps a
  fresh empty shard (`currentAuthzShard`, `snapshot_authz_memo.go:141-157`), so no entry outlives its
  snapshot. `Gen` is also in the key (`snapshot_authz_memo.go:84-94`) to close the store-race window.
- **PERMITS only — never cache a deny** (`evaluate.go:259-289`). A deny can be transiently wrong
  under snapshot churn (a momentarily-incoherent rebuild yields a fail-closed `false`); caching it
  would pin the wrong deny for the whole generation on a hot key (the snowplow SA's wildcard CRB can
  never be correctly denied), starving the refresher — the #301 incident. Permits are
  monotone-correct within a generation (a binding removal bumps `PublishSeq`), so they are safe to
  cache. Denies fall back to the walk every call and self-heal; `authzMemoDenyUncached` counts them
  (`snapshot_authz_memo.go:120-131`, `evaluate.go:288`).
- **`SkipBindingUID` is part of the key** (`snapshot_authz_memo.go:93`) so a UID-consumer and a
  verdict-only consumer never share an entry (`evaluate.go:219-230`).
- **Capacity-capped** at `16384` with insert-refusal on breach — never evict-to-OOM
  (`snapshot_authz_memo.go:62-67`, `:182-192`).
- **Below the cache=off short-circuit** so `CACHE_ENABLED=false` never reaches it
  (`snapshot_authz_memo.go:49-52`).

Groups fold into the key via `canonicalGroupsHash` (`groups_hash.go:68`): order-independent
(sort-first) FNV-1a with a per-element **length prefix** so distinct set partitions like
`["a","bc"]` vs `["ab","c"]` cannot alias (`groups_hash.go:14-23`, `:79-86`). Hashing the *raw*
`opts.Groups` is sufficient because `effectiveGroups` is a pure function of `(Username, Groups)`
(`groups_hash.go:25-39`). Counters are exposed read-only over `/debug/vars` via
`RegisterAuthzMemoExpvar` (`snapshot_authz_memo_expvar.go:35`).

### 2.6 BindingUID derivation — `cache/match_subject.go`

`BindingUIDFromCRB` (`match_subject.go:83`) returns `"C:<uid>"`; `BindingUIDFromRB`
(`match_subject.go:107`) returns `"R:<ns>/<uid>"`. The `C:`/`R:` prefixes keep CRB and RB UIDs from
aliasing and the `R:<ns>/` prefix carries namespace scope into the identifier. Empty UID (synthetic
fixtures / pre-stamp gap) falls back to a content tuple framed with a `\x1f` separator
(`match_subject.go:90-93`, `:114-115`). Both return `""` iff the pointer is nil.

---

## 3. Per-user-keyed L1 — the load-bearing invariant (ADR 0002/0003)

### 3.1 What gets keyed

`ComputeKey` (`cache/resolved.go:608`) builds the L1 cell key. For every identity-bound entry class
(`restactions`, `widgets`, `apistage`, `raFullList`) it folds the **`BindingUID`** — the first-match
binding that authorised *this layer's* GET for *this request's* identity — into the key
(`resolved.go:652-655`). `dispatchCacheLookupKey` (`helpers.go:200`) derives that UID with a direct
`EvaluateRBAC(verb=get, …)` call leaving `SkipBindingUID` at its safe zero value so the returned UID
is the deterministic sorted first-match (`helpers.go:211-220`, `:228`).

The result (`resolved.go:256-267`, `:284-303`): **two users granted by the SAME binding share one
cell; the same user authorised by DIFFERENT bindings on different layers gets different cells.** This
is finer-grained than the v3 cohort hash (`BindingSetHash`) it replaced.

### 3.2 The cross-user leak it prevents

The invariant is: **an identity-bound L1 cell must never be keyed by a cohort/content key alone.**

If the cell were keyed only by content (gvr/ns/name/page/extras) and *not* by the authorising
binding, then user A — broadly authorised, e.g. an admin or the wildcard-CRB snowplow SA — would
write a cell holding rows A is permitted to see. User B, narrowly authorised, would then **read A's
cell** because the content key matches, and receive rows B has no grant for. That is a direct
cross-user RBAC leak. Folding `BindingUID` into the key means B's request (authorised by a different
binding, or denied → `BindingUID == ""`) computes a *different* key and never lands on A's cell. The
`feedback_l1_per_user_keyed_never_cohort` retrospective records that attempts to collapse this to a
cohort/content-only key were reverted six times; the binding-keyed cell is the durable fix and is the
substance of ADR 0002/0003.

### 3.3 The one exception that proves the rule: `widgetContent`

`widgetContent` is the **only** class that skips the identity fold (`resolved.go:652`,
`:127-147`). Its cached body is a *shell*: `status.resourcesRefs.items[].allowed` flags are stored as
populated by the SA walker, but the serve-time gate `gateWidgetEnvelope`
(`dispatchers/widgets.go:109-114`, `:141`) **overwrites every `allowed` flag per-request via
`rbac.UserCan` under the requesting identity before serialisation.** So the body is shared but the
bytes that leave the pod are per-user — the identity narrowing moves from the *key* to a *serve-time
rewrite*. This is the identity-free-content-key + serve-time-UAF pattern (ADR 0003). The general rule
holds: an entry is identity-free in the key **only if** it is re-narrowed per-user at serve time.

### 3.4 The empty-UID biconditional

`BindingUID == ""` iff (cache off) OR (deny / no snapshot) — verified both directions by the F7
invariant tests (`evaltest/empty_binding_uid_invariant_test.go:94`, `:122`, `:161`, `:194`, `:240`).
A non-empty UID for a deny would leak the matched-binding identity into the cache key and is treated
as a broken invariant. Fail-closed: an empty UID collapses the cell to the empty-identity row — the
same shape as cache=off's transparent fallback (`helpers.go:197-199`).

---

## 4. Serve-time UAF — the per-item gate

Serve-time UAF is the second guarantee: even on a correctly-keyed cell, every item handed out is
re-checked against the requesting identity. The sites, all calling `EvaluateRBAC(…,
SkipBindingUID=true)` and consuming only `allowed`:

- **`refilter.go`** — `applyUserAccessFilter` (`refilter.go:71`) / `refilterSlice`
  (`refilter.go:182`) / `evalSingle` (`refilter.go:226`, `EvaluateRBAC` at `:257`): the
  `userAccessFilter` dispatch path. SA-dispatched list results are filtered per-object; a JQ error,
  an `EvaluateRBAC` error, or a deny **drops** the item (fail-closed, `refilter.go:178-182`,
  `:269`). This is the authoritative gate — if it drops, the user does not see the item.
- **`informer_dispatch_rbac.go`** — `filterListByRBAC` (`:93`, eval at `:155`) and `filterGetByRBAC`
  (`:231`, eval at `:267`): post-LIST per-item drop and single-object GET gate for the informer-served
  path. Errors fail closed (item dropped).
- **`cluster_list.go`** — the cluster-scoped list gate (`EvaluateRBAC` at `:238`); a not-yet-published
  snapshot degrades to deny (`cluster_list.go:181-183`).
- **`objects/informer_serve.go`** — `filterGetByRBAC` (`:229`, eval at `:257`): no identity, an
  `EvaluateRBAC` error, or a deny all fail closed (`informer_serve.go:200-214`).
- **`dispatchers/helpers.go`** — `checkDispatchRBAC` (eval at `:108`): the per-request dispatch gate.

Because every per-item site discards `matchedBindingUID`, they pass `SkipBindingUID: true` to skip
the CRB/RB stable-sort — the ~43% pod-CPU lever at 50K scale (`evaluate.go:107-111`,
`:153-166`). Correctness is unaffected: the sort only affects which UID is returned, not the verdict.

The cache-key callers — `dispatchCacheLookupKey` (`helpers.go:212`), the `helpers.go` diagnostic
(`:339`), `ra_full_list` — are the ones that leave `SkipBindingUID` false and keep the deterministic
UID.

---

## 5. Invariants

1. **Identity-bound L1 cells are keyed by `BindingUID`, never cohort/content alone.** Violation =
   cross-user leak (§3.2). The sole identity-free class, `widgetContent`, is re-narrowed per-user at
   serve time (§3.3). (ADR 0002 / 0003.) `ComputeKey` `resolved.go:652-655`.
2. **Zero `SubjectAccessReview` to the apiserver in cache=on mode.** All checks resolve against the
   informer-cached typed RBAC snapshot. A nil watcher or nil snapshot **degrades to deny**, never to
   apiserver fallback (`evaluate.go:185-199`, `:206-217`). The hard rollback trigger for the tag
   (`evaluate.go:5-8`).
3. **cache=off is a transparent correctness baseline.** `CACHE_ENABLED=false` routes through
   `UserCan` → `SelfSubjectAccessReview` and returns an empty `BindingUID`; the memo and the snapshot
   are never reached (`rbac.go:62-64`, `evaluate.go:171-183`, `snapshot_authz_memo.go:49-52`).
4. **One snapshot generation per evaluation.** A single `Snapshot()` load is threaded through the
   whole walk and the memo key (`evaluate.go:201-206`, `:231-242`), so reads are coherent across a
   republish.
5. **Memo caches PERMITS only.** A deny can be transiently wrong under churn and must self-heal by
   re-walking every call (`evaluate.go:259-289`). Caching a deny is the #301 incident.
6. **Predicate symmetry across subject kinds.** The subject index and `anySubjectMatches` route
   User, Group, and ServiceAccount identically, sharing `effectiveGroups` for group expansion
   (`evaluate.go:462`/`:481` vs `:694`/`:743`).
7. **The subject index may over-include but never under-include.** `anySubjectMatches` is the
   post-lookup equality barrier; `CRBsCatchAll` guards unrecognised kinds
   (`cache/rbac_snapshot.go:149-152`, `evaluate.go:494-497`).
8. **`BindingUID == ""` ⟺ (cache off) ∨ (deny / no snapshot)** — both directions
   (`evaltest/empty_binding_uid_invariant_test.go`).
9. **`resourceNames`-scoped rules never grant collection verbs.** A non-empty `rule.ResourceNames`
   matches only name-specific verbs and only the named object (`evaluate.go:651-666`).

---

## 6. Known failure modes

| Symptom | Likely cause | Where to look |
|---|---|---|
| Cross-user content leak (user sees rows they lack a grant for) | An identity-bound class lost its `BindingUID` fold, or a cohort/content-only key was reintroduced. | `ComputeKey` `resolved.go:652-655`; the `feedback_l1_per_user_keyed_never_cohort` retrospective; F7 tests. |
| Admin list stuck empty under load; refresher starved | A transiently-wrong deny got cached (regression of the PERMITS-only rule). | `evaluate.go:259-289`; `snowplow_authz_memo_deny_uncached_total` expvar should be > 0 and rising. |
| Everything denied right after pod start | Snapshot not yet published — degrade-to-deny pre-readiness gate firing as designed; self-heals once the first rebuild publishes. | `evaluate.go:206-217`; `cluster_list.go:181-183`. |
| Permits silently lost for one subject kind | Subject index under-includes (index/matcher predicate drift); or a future `Subject.Kind` not routed to `CRBsCatchAll`. | `selectCRBCandidates` `evaluate.go:453`; `anySubjectMatches` `evaluate.go:694`; `rebuildSubjectIndexes` `cache/rbac_snapshot.go:536`. |
| Wrong verdict for a ServiceAccount caller | Synthetic SA groups not expanded, or index/matcher disagree on expansion. | `effectiveGroups` `evaluate.go:743`; `parseServiceAccountUsername` `evaluate.go:759`. |
| `resourceNames`-scoped rule over-exposes a list | `resourceNameMatches` regressed (pre-0.30.109 behaviour). | `evaluate.go:651-666`. |
| Stale verdict served across a binding change | Memo generation-binding broke (gen not in key, or shard not swapped on `PublishSeq` change). | `snapshot_authz_memo.go:84-94`, `:141-157`; `evaluate.go:231-242`. |
| Group-set hash collision (two different group sets share an entry) | A second, non-length-prefixed groups hasher was introduced (the 0.30.239 two-hasher drift). | `canonicalGroupsHash` `groups_hash.go:68`; do not inline a second hasher. |
| Loud `rbac.indexer.read fallback=true` WARN | Typed transform did not fire on an RBAC object; the defensive `as{Kind}` conversion path ran. | `asClusterRoleBinding` etc. `evaluate.go:788-859`. |

> **Testing caution.** Do **not** run `go test ./internal/rbac/...` against a remote kubeconfig — its
> `TestMain` destructively deletes the RESTAction CRD. Use the `evaltest/` sub-package or a kind
> cluster only, with `KUBECONFIG` unset for unit runs.
