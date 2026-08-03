# #118 — userAccessFilter RBAC stale-read: resolved-cache key blind to the refilter's RBAC dependency

Design doc. Author: cache-architect. Date: 2026-07-22.
Checkout: `feat/c6c8s5 @ bae1f6b` (= `main @ dd073f8` #116 + 3 export-security commits that do
not touch any file cited here; the UAF/cache/RBAC surface is byte-identical to `dd073f8`).
KUBECONFIG unset for the whole trace; no cluster touched; no `go test ./internal/rbac/...`.

Every code claim below is **TRACED** (file:line on this checkout) unless labelled **INFERRED**.

---

## Headline

- **Real bug — CONFIRMED.** All three legs of defect 1 verified in source. An RBAC grant/revoke
  bumps `RBACGen` + rebuilds the snapshot but evicts **zero** resolved cells; the resolved-cache
  key folds a single dispatch-authorizing `BindingUID` and **no RBAC generation**, while the UAF
  refilter re-evaluates RBAC **per object per per-object-namespace** — a dependency the key never
  sees. Defect 2 (dynamically-published composition types watched lazily-only) is also confirmed,
  with the exact registration hook identified.
- **Recommended fix: option (c) — per-user RBAC sub-generation folded into the key, with option
  (d) short-TTL as the same-cycle interim stopgap.** Rationale: (c) is the only option that is
  both correct AND survives the 50K composition-install RBAC storm (blast radius = only users
  whose *own* effective bindings changed). (a) global RBACGen is correct but collides head-on with
  the install storm; (b) per-user binding-UID set is correct but pays a full binding-set walk per
  request. See §fix-options for why (c) is feasible against the existing typed snapshot indexes.
- **Seeded/subscribed parity answer:** UAF **IS seeded** (proactive-RA seed enumerates all
  RBAC-reachable RESTActions incl. UAF ones, resolved under a **cohort representative identity** —
  NOT userless), and UAF **IS subscribable** on `/refreshes` (identity-bound restactions class,
  same `dispatchCacheLookupKey` derivation). Both consumers call the SAME key-derivation function,
  so a term folded into `dispatchCacheLookupKey` propagates to seed + subscription **for free** —
  the parity surface is one function, not eight. C7's "userless→empty→frozen" hazard does **not**
  apply to today's seed (it uses a real representative identity + fail-closed ""-skip).
- **Interim mitigation: YES, ship option (d) short-TTL first.** But with a corrected exposure model:
  the window is NOT "until restart." TTL is a hard absolute lifetime from write (`CreatedAt`), and
  a hot cell that keeps getting **data-plane refreshed** slides `CreatedAt` forward on every refresh
  → effectively unbounded for an actively-churning composition-list cell. A quiescent cell expires
  at `RESOLVED_CACHE_TTL_SECONDS` (default 3600s). So the stopgap is worth shipping.

---

## §root-cause

### Defect 1, leg 1 — the key folds the wrong RBAC identity

`dispatchCacheLookupKey` (helpers.go:228-271) derives one `BindingUID` via a single
`rbac.EvaluateRBAC` call for the **layer's own GET-permit** (helpers.go:240-248):

```
_, bindingUID, _ := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
    Username: ui.Username, Groups: ui.Groups, Verb: "get",
    Group: group, Resource: resource, Namespace: namespace, Name: name})
```

That `bindingUID` is the ONLY identity material in `ResolvedKeyInputs` (helpers.go:256).
`ComputeKey` (resolved.go:638-722) folds: version, class, GVR, ns, name, `BindingUID`
(all classes except widgetContent, resolved.go:682-685), perPage, page, Stage, Extras — and
**nothing else identity/RBAC-derived**.

But the UAF refilter (`evalSingle`, refilter.go:250-308) fires a SEPARATE `rbac.EvaluateRBAC`
**per object, per that object's own NamespaceFrom-derived namespace**, with `SkipBindingUID: true`
(refilter.go:279-289). The set of objects the refilter KEEPS is a function of the user's
per-namespace RoleBindings across every namespace in the result — a completely different RBAC
evaluation from the single dispatch-GET `BindingUID`. **The key is blind to it.** Two RBAC states
that produce the same dispatch `BindingUID` but different per-namespace refilter outcomes collide
on the same cell.

### Defect 1, leg 2 — `RBACGen` exists but is never consulted by the cache key

`RBACGen()` (rbac_snapshot.go:254-256) returns `rbacSnapshotPublishSeq` (rbac_snapshot.go:241),
bumped `.Add(1)` on every snapshot publish (rbac_snapshot.go:477). Its only non-test consumers:
- `internal/metrics/metrics.go:394` — expvar observation.
- `internal/cache/rbac_snapshot_expvar.go:55` — expvar `snowplow_rbac_publish_seq`.
- `internal/handlers/refreshes.go:211` — `refreshWarmupIncomplete` boot-gate (`RBACGen()==0`).

It is referenced **zero** times in helpers.go, resolved.go (`ComputeKey`), or any eviction path.
The generation counter that would let a cell notice "the RBAC store changed under me" is computed
and published but **never wired into the resolved cache**.

### Defect 1, leg 3 — RBAC-plane changes evict no resolved cell

Snapshot rebuilds are scheduled by `scheduleRBACRebuild` (rbac_snapshot.go:306), wired to the
4 typed-RBAC GVR informers' Add/Update/Delete handlers (`rbacSnapshotEventHandlers`,
rbac_snapshot.go:859-887). That path rebuilds the snapshot and bumps the gen — and stops there.

The resolved-cache invalidation surface is entirely **data-plane**: `Deps().OnAdd/OnUpdate/OnDelete`
(deps.go:635/647/721), driven by the *data* informers' events keyed by the watched object's
`(gvr, namespace, name)` (deps_watch.go:117-223). There is **no edge** from `scheduleRBACRebuild`
(or any RBAC-GVR event) into `OnAdd/OnUpdate/OnDelete` or any resolved-cell eviction/dirty-mark.

⇒ **An RBAC change bumps `RBACGen` + rebuilds the snapshot but dirty-marks/evicts NO resolved
cell.** The stale UAF result is served until the cell leaves cache by another mechanism (TTL /
LRU / a data-plane DELETE of the cell's own object) — see §interim-mitigation for the real window.

### Defect 2 — dynamically-published composition types are watched lazily-only

Data informers register lazily: `ensureWatcherInformerForGVR` (deps_extract.go:147) →
`rw.EnsureResourceType` (watcher.go:613), called only **after** a successful resolve Put
(e.g. restactions.go:399). A CompositionDefinition that publishes a new CRD/type post-boot
(e.g. `selfservicekrateoes`) is observed by the **CRD-discovery informer** (`triggerCRDDiscovery`,
crd_discovery_side_effect.go:344, wired to the CRD informer AddFunc, :214), which on ADD refreshes
the discovery cache + invalidates the schema memo + relists **already-registered** GVRs
(`triggerCRDSchemaRelist`, :559) — but it deliberately does **NOT** register a data informer for
the newly-served GVR: the relist is gated `if !rw.IsRegistered(gvr) { continue }`
(crd_discovery_side_effect.go:607) with the comment "lazy registration is the resolver's job."

So the new type stays unwatched until first resolver touch. Defect 1's permanent-hit
(the same user's frozen composition-list cell that never re-dispatches into the new type) starves
that touch → the type is never watched → no data-change invalidation for it → self-reinforcing
freeze. Confirmed compound, as the brief states. #119/#120 are split out (noted at end).

---

## §severity — within-user, invariants preserved

- **WITHIN-USER staleness, not cross-user leak.** The key folds `ui.Username`-derived `BindingUID`,
  and the #95 serve-side guard `serveFromCacheEligible` (helpers.go:288-290) treats a re-derived
  `""`-BindingUID as a MISS (falls through to a direct resolve under the request's own identity).
  A different user with a different effective `BindingUID` lands on a different cell. So the hazard
  is: **the same user sees their own now-stale UAF view** — e.g. access granted in namespace N is
  not yet visible, or access revoked in N is still visible — until the cell leaves cache.
- **Serious but bounded to the user's own identity.** It is an authz-**freshness** defect (revoked
  access served, or newly-granted access withheld) with a potentially long window (§interim), not a
  confidentiality breach across tenants.
- **Invariants the fix MUST NOT regress:**
  - L1 stays **per-user-keyed**, never cohort/groups-only (feedback_l1_per_user_keyed_never_cohort).
    Options (b)/(c) preserve this by folding a per-USER discriminant; (a) global RBACGen is also
    per-user-safe (it only adds a term, never widens sharing).
  - The #95 `""`-BindingUID serve+populate guard (helpers.go:288, restactions.go:368,
    phase1_pip_seed.go:668) stays intact — the new term is folded **alongside** BindingUID, the
    fail-closed empty-identity collapse is unchanged.

---

## §fix-options

All options add a term to `ResolvedKeyInputs` + `ComputeKey` (resolved.go:303/638) and bump
`resolvedKeyVersion` v4→v5 (resolved.go:416) so no pre-fix cell serves as a post-fix hit on the
rolling restart. They differ only in WHAT term and its blast radius under RBAC churn.

### (a) Global RBACGen in the key — correct, worst herd
Fold `RBACGen()` into `ComputeKey`. Any RBAC snapshot publish rotates the entire UAF key space at
once. **Correct** (any RBAC change → new key → cold miss → fresh resolve → fresh refilter).
**Fatal at 50K:** composition installs CREATE RBAC bindings continuously
(project_composition_install_rbac_scale), each bump invalidates ALL identity-bound cells (not just
UAF — every restactions/widgets/apistage/raFullList cell folds BindingUID and would fold RBACGen),
so a steady install stream keeps the whole L1 permanently cold. Collides head-on with the
install-storm snowplow must survive. **REJECT** as the standalone fix.

### (b) Per-user relevant-binding-UID SET folded in — correct, expensive per request
Fold a hash of the SET of binding UIDs that grant the requesting user anything relevant. Blast
radius bounded to users whose binding set changed. **Correct**, per-user-safe. **Cost:** requires
walking the user's full effective binding set on every /call (not just the single dispatch-GET
EvaluateRBAC), on the hot path. At 50K/1M-binding scale that walk is the concern. Feasible but
pays per-request CPU that (c) avoids.

### (c) Per-user RBAC sub-generation — RECOMMENDED
Maintain a per-subject counter that bumps ONLY when THAT subject's effective bindings change, and
fold the requesting user's current sub-gen into the key. Blast radius = exactly the users whose
own bindings changed; a composition-install binding for tenant-X bumps only tenant-X's subjects,
leaving every other user's cells hot. **Correct** (a relevant RBAC change → the user's sub-gen
bumps → new key → cold miss → fresh refilter), per-user-safe, and herd-proportional to the change,
not to total traffic.

**Feasibility against the existing snapshot (the crux of whether (c) is real, not hand-waved):**
The typed snapshot already maintains subject→bindings indexes — `CRBsByUser`, `CRBsByGroup`,
`CRBsByServiceAccount`, `CRBsCatchAll`, and `RoleBindingsByNS` (rebuilt in `rebuildSubjectIndexes`,
rbac_snapshot.go:462/504, logged at :497-500). The per-event binding delta hooks
(`onBindingAdd/Update/Delete`, wired at rbac_snapshot.go:864-885) already fire per binding change
and already know the binding's subjects (they maintain the BindingsByGVR index). So the sub-gen
map can be bumped from those SAME hooks: on a binding event, resolve its subjects (users + groups +
SAs) and bump each affected subject's counter. A request's effective sub-gen = a fold over the
user's own username-counter + each of its groups' counters (read lock-free like `RBACGen`). This
reuses existing subject-index machinery — no new informer, no new walk on the hot path beyond a
handful of map reads. **INFERRED** that the group-fold cost is bounded (a user has O(10) groups);
must be confirmed by the dev against the exact `onBinding*` subject-extraction shape, but the
indexes and hooks needed all exist.

**Group-vs-user granularity caveat (dev must resolve):** a binding can grant via a GROUP the user
is in. The sub-gen fold therefore must include every group the user presents, or a group-scoped
grant/revoke won't bump the user's effective sub-gen. Folding `max`/`sum` over
`{userCounter} ∪ {groupCounter[g] for g in ui.Groups}` handles this; the RED falsifier below
(`grant via group`) pins it.

### (d) Short-TTL / cache-bypass for UAF-bearing RESTActions only — interim stopgap
Stamp a short `TTLOverride` (reuse the R1 Layer 2 machinery, resolved.go:270-279/788-796) on any
resolved cell whose RESTAction declares `userAccessFilter`, OR bypass cache entirely for UAF cells.
Bounds the window to the short TTL regardless of the key defect. Cheap, uniform (per-class-ish
predicate: "entry's RA has a UAF stage"), reversible. Does NOT fix the key — a within-TTL RBAC
change is still stale — but caps exposure. **Ship this in the same cycle as the interim.**

### Recommendation
**Ship (d) as the interim in the same PR/cycle, then (c) as the durable fix.** (d) caps the
exposure window immediately at low risk; (c) is the correct key fix that also survives 50K. This
is the "interim (d) + eventual (c)" hybrid the brief flagged as acceptable. Avoid (a) entirely at
50K; keep (b) as the fallback if (c)'s per-subject bump proves infeasible against the snapshot
(it should not).

---

## §key-parity-surface

**Is UAF seeded? YES.** `proactive_ra_seed.go` enumerates ALL RBAC-reachable RESTActions
(`RBACReachableRestActionRefs`, :55) with **no GVR/resource filter** (Option B rejected, :35), so
UAF-bearing RESTActions are in the seed set. They resolve under a **cohort REPRESENTATIVE
identity**, not userless: `withCohortSeedContext` installs `xcontext.WithUserInfo{Username:
cohort.Username, Groups: cohort.Groups}` (phase1_pip_seed.go:438-441); `seedOneRestaction`
(:620) calls `dispatchCacheLookupKey` which reads that identity (:638) and fail-closed-skips a
`""`-BindingUID (:668). So the seed folds a real BindingUID under a real representative — C7's
"userless→empty→frozen" cell does NOT exist in today's seed.

**Is UAF subscribed? YES.** UAF RESTActions are `restactions`-class, an identity-bound subscribable
class (refresh_subscription.go:11-13, 51-53). `/refreshes` arms them by re-deriving the key via the
SAME `dispatchCacheLookupKey` under the connection's identity (refresh_subscription.go header,
:85-88 — "dispatchCacheLookupKey's only external touch is rbac.EvaluateRBAC").

**The parity surface is ONE function, not eight.** Because seed (`seedOneRestaction`
phase1_pip_seed.go:638), subscription (`deriveSubscription` → `dispatchCacheLookupKey`,
refresh_subscription.go), and dispatch (helpers.go:270) all route through
`dispatchCacheLookupKey` → `cache.ComputeKey`, **any term folded into `ResolvedKeyInputs` +
`ComputeKey` propagates to all three automatically.** The consumers the brief listed that must
stay byte-parity:

| Consumer | file:line | Folds via |
|---|---|---|
| dispatch (customer /call) | helpers.go:270 | `dispatchCacheLookupKey`→`ComputeKey` |
| seed / prewarm | phase1_pip_seed.go:638-654, :889 | `dispatchCacheLookupKey`→`ComputeKey` |
| subscription-refresh | refresh_subscription.go (deriveSubscription) | `dispatchCacheLookupKey`→`ComputeKey` |
| widget / widget content | widgets.go:433, widget_content.go:303 | `ComputeKey` (widgetContent identity-free — see below) |
| ra_full_list | ra_full_list.go (RAFullList class) | `ComputeKey` |
| apistage | apistage.go | `ComputeKey` (folds Stage + BindingUID) |
| cluster_list | cluster_list.go | `ComputeKey` |
| refresher re-Put | resolve_populate.go:316 | uses stored `Inputs` → `ComputeKey` recompute |

**Scoping consequence:** the new RBAC sub-gen term must be populated wherever a
`ResolvedKeyInputs` is BUILT (helpers.go for dispatch/seed/subscription; widgets.go / apistage.go /
etc. for the widget-side builders) — NOT just in `ComputeKey` (which only hashes what it's given).
The clean placement is **inside `dispatchCacheLookupKey`** (it already does the `EvaluateRBAC`
call and has `ui.Username`/`ui.Groups` in hand — helpers.go:240) plus the widget-side key builders,
so every builder stamps the term from the same helper. **widgetContent stays identity-free**
(resolved.go:682) — it is a shared envelope re-filtered per-user at serve time, so it does NOT fold
the RBAC sub-gen; the fix must NOT touch its key or it breaks the shared-content invariant. UAF
lives in restactions/apistage (identity-bound), so widgetContent exclusion is safe for #118.

**#64 / F-ARCH lesson honored:** the CI invariant test (#67) already asserts
`DeriveSubscriptionKey == ComputeKey` per class; the new term must be added to BOTH sides via the
shared helper so that test keeps passing — and a new key-parity arm (§falsifiers) pins it.

---

## §interim-mitigation ruling — and the corrected exposure model

**Does TTL bound the window at all? YES — but with a critical nuance the "until restart" framing
misses.**

- `CreatedAt` is stamped exactly once, at Put, only when zero (resolved.go:843-844). `Get` expires
  on `time.Since(entry.CreatedAt) > effectiveTTL` (resolved.go:823); the LRU touch on a hit does
  **not** reset `CreatedAt` (resolved.go:830). So for a cell that is only ever READ, the window is
  a hard **absolute** lifetime = `RESOLVED_CACHE_TTL_SECONDS` (default 3600s = 1h,
  resolved.go:103) from first write. **Not "until restart."**
- **BUT** the refresher re-Put builds a FRESH `ResolvedEntry` with zero `CreatedAt`
  (resolve_populate.go:287-295 — no CreatedAt field set), so the Put stamps a NEW `time.Now()`
  → **`CreatedAt` slides forward on every data-plane refresh.** The keepwarm sweep does the same by
  design (prewarm_keepwarm_sweep.go:18/118: "each sweep Put resets CreatedAt"). A UAF
  composition-list cell whose underlying watched objects keep churning (an active tenant) is
  data-plane-refreshed repeatedly → its TTL never elapses → the stale RBAC/refilter result is
  served **effectively indefinitely** (until the cell falls out of the refresh/keepwarm rotation
  for a full TTL, or a DELETE of its own object evicts it). This is why the "until restart"
  intuition is directionally right for a hot cell even though the mechanism is TTL-slide, not
  no-TTL.

**Ruling: SHIP option (d) short-TTL as the interim, same cycle.** A short `TTLOverride` on
UAF-bearing cells caps the window even for a hot, refreshed cell — as long as the override applies
on the refresh re-Put too (it will, via the same `IsServable`/predicate stamp path the R1 Layer 2
`TTLOverride` already uses; the dev must confirm the UAF predicate is applied on the refresher Put
in resolve_populate.go:287, not only the first customer Put). This is worth doing because the
un-mitigated window is unbounded-for-hot-cells, and (c) is a larger change requiring the per-subject
sub-gen plumbing.

---

## §defect-2 informer-registration design

**Prior art:** the hook already exists — the CRD-discovery informer AddFunc → `triggerCRDDiscovery`
(crd_discovery_side_effect.go:214/344). It currently refreshes discovery + schema memo + relists
**registered** GVRs (:607 gates on `IsRegistered`). It does NOT register a data informer for a
newly-served GVR.

**Design:** add an additive, gated branch in `triggerCRDDiscovery`'s ADD path that, for a CRD whose
served GVRs are **not yet registered** AND whose group is navigation-discovered
(`AddNavigationDiscoveredGroup(group)` already runs at :396), eagerly calls `rw.EnsureResourceType(gvr)`
for each served GVR (crdServedGVRs, :593) — i.e. register the data informer at CRD-publish time
rather than waiting for first resolver touch. Then `Deps().OnResourceTypeSchemaRelisted(gvr)` (or an
ADD-equivalent) to dirty-mark any dependent LIST cells so a change to the freshly-self-served type
invalidates them.

**Bounding (no unbounded informer growth):**
- Gate on **navigation-discovered group** only (reuse `AddNavigationDiscoveredGroup`/the
  removable-discriminator at watcher.go:749/1064) so snowplow does not spawn informers for every
  CRD in the cluster — only for groups the portal navigation actually reaches. This is the same
  discriminator the schema-relist already trusts.
- `EnsureResourceType` is registration-idempotent + singleflighted via `rw.mu` (watcher.go:613),
  so double-fire under CRD churn is safe.
- The informer count is bounded by the number of nav-reachable served GVRs — the same set the
  resolver would have lazily registered anyway; this only changes WHEN (publish-time vs
  first-touch), not the ceiling.

**Toggle-able (cache is provisional/removable — project_caching_is_provisional):** gate the eager
registration behind an env flag (e.g. `CRD_EAGER_INFORMER_REGISTER`, default off or on per Diego),
so it is cleanly removable and the lazy path remains the fallback. When off, behavior is exactly
today's.

**Interaction with defect 1's fix:** once (c) evicts the frozen UAF cell on the user's own RBAC
change, the user re-dispatches, which lazily registers the type anyway — so defect 2's fix is
belt-and-suspenders (covers the data-change-invalidation gap for a freshly-published type even
before any user touches it). Both are worth shipping; defect 2 can be a separate PR.

---

## §falsifiers

Each is the specific artifact that proves the fix works (or didn't). Empirical-first: the on-cluster
arms are the acceptance gate; the hermetic arms are the standing regression guard.

1. **RBAC-grant → immediate-visibility (on-cluster, kind or GKE).** User U with a UAF
   composition-list cell warm (l1:HIT). Grant U access in namespace N (create a RoleBinding).
   Within one refresh interval, U's next /call MUST include N's items. **RED before fix**
   (item stays absent until TTL/restart); GREEN after (c).
2. **RBAC-revoke → immediate-drop (on-cluster).** Symmetric: revoke U's access in N; U's next
   /call MUST drop N's items promptly. **RED before fix** (revoked item still served); GREEN after.
   This is the security-load-bearing arm.
3. **Per-user-keying NOT regressed (hermetic + on-cluster).** Two users U1, U2 with different
   effective bindings resolve the same UAF RA: distinct keys, distinct cells, no cross-serve.
   Assert `ComputeKey(U1 inputs) != ComputeKey(U2 inputs)` and that U1's grant change does NOT
   bump U2's sub-gen (per-subject isolation). Pins feedback_l1_per_user_keyed_never_cohort.
4. **#95 ""-BindingUID guard intact (hermetic).** A `""`-BindingUID request still MISSes and
   falls through (serveFromCacheEligible + the populate-side skip). The new sub-gen term must NOT
   make a `""`-BindingUID cell servable. RED arm = a fix that folds sub-gen but drops the ""-guard.
5. **Key-parity across consumers (hermetic, extends #67).** For a UAF coord: assert
   `DeriveSubscriptionKey(coords, identity) == ComputeKey(dispatch)` AND
   `seedKey == dispatchKey` after the sub-gen term is added — driving BOTH sides through the REAL
   `dispatchCacheLookupKey` (no shadow copy; feedback_key_parity_golden_real_inputs_prehash_diff:
   assert PRE-HASH field equality of `ResolvedKeyInputs.RBACSubGen`, not just the digest). RED arm
   = fold the term on the dispatch side only → keys diverge → the test catches it.
6. **Grant-via-GROUP bumps the user's sub-gen (hermetic).** A binding that grants via a group U is
   in must bump U's effective sub-gen. RED arm = a fold over only `{userCounter}` (blind to groups)
   → this test fails; GREEN = fold over `{user} ∪ {groups}`. This pins the granularity caveat in (c).
7. **50K herd bound (bench analysis + arm).** Under a simulated composition-install RBAC-binding
   stream for tenant-X only, assert cells for an UNRELATED tenant-Y stay HIT (their sub-gen does not
   bump). This is the arm that discriminates (c) from (a): under (a) all cells go cold; under (c)
   only tenant-X's do. Falsifier shape must have ≥2 tenants (feedback_falsifier_shape_must_discriminate
   — a single-tenant arm masks the global-vs-per-user distinction). Runs on the bench; the negative
   control is "swap (c)→(a) and watch tenant-Y go cold."
8. **Interim (d) window bound (on-cluster).** With (d) enabled, a UAF cell's staleness after an
   RBAC change is ≤ the short TTL even for a data-plane-refreshed hot cell — confirm the UAF
   `TTLOverride` predicate is applied on the refresher Put (resolve_populate.go:287), not only the
   first Put; otherwise the slide defeats it. Falsifier: churn the cell's data to force refreshes,
   verify `CreatedAt`-slide does not extend a UAF cell past its override.
9. **Defect-2: freshly-published type is watched at publish (on-cluster).** Publish a
   CompositionDefinition creating a new type post-boot; WITHOUT any user touching it, mutate an
   object of that type and assert a dependent LIST cell dirty-marks. RED before fix (unwatched
   until first touch); GREEN after the eager-register branch. Bound-check: assert no informer is
   spawned for a CRD in a NON-nav-discovered group (the growth bound).

---

## §rejected-alternatives

- **Global RBACGen in the key (option a) as the standalone fix** — REJECTED: correct but collides
  with the 50K install storm (every binding create invalidates all identity-bound cells).
- **Evict-on-RBAC-change by walking L1 for affected cells** — REJECTED: L1 is keyed by an opaque
  hash with no reverse index from `(subject → keys)`; a wholesale scan on every RBAC publish is the
  same herd as (a) plus a full-map walk. The key-fold approach (c) achieves the same effect lazily
  (next lookup misses) without a scan.
- **Fold the full per-request refilter RBAC evaluation into the key** — REJECTED: the refilter's
  RBAC eval is per-object over the (unknown-until-resolved) result set; it cannot be computed at
  key-derivation time without resolving first (chicken-and-egg). The sub-gen is the right proxy:
  it captures "did anything about this user's RBAC change" without enumerating the result.
- **widgetContent folding the RBAC term** — REJECTED: widgetContent is deliberately identity-free
  (shared envelope, per-user serve-time filter, resolved.go:682); folding identity/RBAC there
  breaks the shared-content invariant. UAF is not a widgetContent concern.
- **Userless-prewarm exclusion for UAF (C7)** — NOT NEEDED as a fix: today's seed uses a real
  cohort representative identity (phase1_pip_seed.go:438-441), not userless, and fail-closed-skips
  `""`-BindingUID (:668). There is no userless-empty UAF cell to freeze. (If a future change adds a
  userless prewarm arm, THEN exclude UAF — noted as a forward guard, not a #118 fix.)

---

## §effort

- **Interim (d):** ~30-60 LOC. UAF-predicate detection on the resolved entry (does the entry's RA
  declare a UAF stage) + `TTLOverride` stamp at both Put sites (customer + refresher). Reuses R1
  Layer 2 machinery. Same cycle.
- **Durable (c):** ~150-250 LOC. New per-subject sub-gen map + bump from `onBinding*` hooks
  (rbac_snapshot.go:864-885) + a `RBACSubGenForSubject(user, groups)` reader (lock-free, mirrors
  `RBACGen`) + `RBACSubGen` field on `ResolvedKeyInputs` + fold in `ComputeKey` + stamp in
  `dispatchCacheLookupKey` and the widget-side key builders + `resolvedKeyVersion` v4→v5 bump.
  Plus the 6-9 falsifier arms.
- **Defect 2:** ~40-80 LOC. Additive eager-register branch in `triggerCRDDiscovery`
  (crd_discovery_side_effect.go:344) gated on nav-discovered group + env flag + dirty-mark.
  Separate PR.

Sequence: interim (d) + defect-2 can ship first/cheap; (c) is the main effort and the security
close-out. All default-off/toggle-able per project_caching_is_provisional until validated.

---

## Compound / split-out notes
- **#119** (readyz-unserved-group) and **#120** (roots rate-limiter) are SPLIT OUT per the brief;
  not folded here. Both compound defect 2's freeze (a freshly-published group being unserved /
  rate-limited delays the first touch that would lazily register it), but their fixes are
  independent.
