# Proactive CR warm — design (plan only, READ-ONLY trace) — 2026-06-20

**Author**: cache architect
**Status**: 🅿️ **PARKED 2026-06-20 (Diego), RE-PARKED 2026-06-22 after source-verification. DO NOT BUILD.** — design halted; not a backlog priority. See the 2026-06-22 verification note below: path 2 (key-narrowing) is now eliminated, path 1 (CDC change) is off the table, only path 3 (L3 fast-miss) survives — and L1-HIT is confirmed not merely infeasible but *incorrect*.

> ## 🅿️ PARKED 2026-06-20 — controller use case is un-cacheable as specified
>
> Diego confirmed the composition-dynamic-controller passes **per-call-variable `extras`** on its `/call`. `extras` folds into the L1 key by design (it can change the resolved body — the `cache_must_not_constrain_jq` invariant), so per-call-variable extras means **every call is a unique cell by construction → no prewarm can produce an L1 HIT**. The L1-resolved-hit goal is therefore unachievable for this caller as it behaves today. Design stopped; not built, not gated.
>
> **Identity gates that DID clear (so a revisit starts from here):** the controller presents a snowplow-issued JWT carrying a Username (OQ-IDENTITY-1 ✅), and its `/call` already succeeds today as a cold MISS — so it has a `<username>-clientconfig` Secret and a `get`-grant on the RA (OQ-IDENTITY-2/3 ✅). Chosen scope was "all RBAC-reachable RAs" (no-special-cases-clean). The seed mechanism (§11) is sound; only the extras keying blocks the payoff.
>
> **Unblock conditions (any one revives this):**
> 1. The controller stops passing per-call-variable extras (passes none, or a small predictable set the seed can reproduce); OR
> 2. We confirm the varying extras keys are **passthrough the target RA's jq never reads** — then the key over-folds, and a (risky) key-narrowing fix could apply; OR
> 3. The goal relaxes from "L1 HIT" to "fast call": warm the **L3 substrate** so the unavoidable L1 miss resolves from in-memory data, not the apiserver (achievable regardless of extras — separate, smaller scope).
>
> The §11 design below + the §0–§10 L3 analysis are retained intact for whoever picks this up.

> ## 🅿️ RE-PARKED 2026-06-22 — source-verified: the extras are output-affecting; L1-HIT is incorrect, not just infeasible
>
> Diego asked to verify the extras-keying blocker by sourcing the code (rather than assume), and ruled that **changing the CDC is off the table** (snowplow must accommodate it as-is). Both verifications are now done:
>
> - **What the CDC sends (TRACED `composition-dynamic-controller/internal/composition/apiresolver.go:34-43`):** `mergedExtras` injects per-instance `compositionName` = `mg.GetName()`, `compositionNamespace` = `mg.GetNamespace()`, `compositionId` = `mg.GetUID()` over the static author extras. The UID makes every composition instance a unique extras value.
> - **Whether the target RA reads them (DECISIVE — TRACED `composition-dynamic-controller/hack/apiref-e2e/manifests/fixtures.yaml:52`):** the CDC's own canonical apiRef RA builds its request `path` from those runtime extras as jq dict keys:
>   `path: ${ "/get?cn=" + (.compositionName) + "&cns=" + (.compositionNamespace) + "&cid=" + (.compositionId) + "&region=" + (.region) }`
>   These are resolve-dict reads (the extras seed the dict), NOT Helm `.Values` render-time substitutions. So the extras **change the resolved output by design** — each composition resolves a different upstream path → a legitimately different body.
>
> **Consequence for the unblock paths:**
> - **Path 1 (CDC stops sending varying extras): OFF** — Diego ruled the CDC is not changeable here (2026-06-22).
> - **Path 2 (key-narrowing — extras are jq-passthrough): ELIMINATED** — the extras ARE read by the RA jq, so narrowing the key to drop them would serve one composition's body for another's request: a direct `cache_must_not_constrain_jq` / per-binding-correctness violation. Not viable.
> - **Path 3 (relax to "fast call" — warm the L3 substrate so the unavoidable per-composition miss resolves in-memory, not via apiserver): the ONLY survivor.** This is the §0–§10 L3 warm (Option A), and it is gated on the F-D0 first-nav-cold-miss-rate measurement (needs a re-stood-up production cluster; none exists — CLUSTER-1 deleted 2026-06-14). Until that data exists, even path 3 is not build-justified.
>
> **Net:** an L1-resolved HIT for the CDC is not merely un-keyable — it would be *wrong* (each composition's `.api` projection is genuinely distinct). The only correct optimisation is making the necessary miss fast (L3), which is itself blocked on the no-cluster measurement gate. **RE-PARKED.** Revive only if (a) a production cluster is re-stood-up to run F-D0 and the miss rate proves material → build §0–§10 Option A; or (b) the CDC's contract changes (new Diego ruling).

**Original status (pre-park)**: DESIGN for PM gate → Diego ratification → dev. NOT a build.

> ## ⚠️ RE-SCOPED 2026-06-20 — the concrete use case is L1, not L3. READ §11 FIRST.
>
> Diego's actual need is **NOT** the L3-metadata-un-navigated-CRD path designed in §0–§10 below. It is the deferred "L1 proactive resolve (Option C / second ship)" — now the PRIMARY target:
>
> **A composition-dynamic-controller calls `/call` to retrieve a RESTAction, and wants that `/call` to be an L1-RESOLVED cache HIT.**
>
> **§11 is the authoritative re-scoped design.** §0–§10 are retained as the L3 background analysis they were written for — still valid for the original "un-navigated CRD first-nav" framing, and §3.4 / §4 / §5 are reused by §11 (L1 needs L3 warm as a substrate; L3 RESTActions are already a boot seed so that prerequisite is free — §11.4). The §9 verdict line below is SUPERSEDED by §11.7 for the controller-RESTAction use case.

**Goal (original §0–§10 framing)**: proactively cache custom resources that have never been navigated, so a FIRST navigation is a HIT instead of a MISS — at strictly LOWER priority than navigated CRs.
**Repo**: `~/krateo/snowplow-cache/snowplow` (this tree, branch `main`, the canonical aligned ship line).
**Scope note**: every code claim below is labelled TRACED (verified file:line in this tree) or INFERRED. The `docs/walker-driven-informer-design-2026-06-01.md` (v6) is STALE on the CRD-informer question — Ships 0.30.233 / L(0.30.246) / 2-Stage-2(0.30.247) evolved past it; this doc traces the ACTUAL current code, not v6.

**Original L3 verdict (§0–§10, SUPERSEDED by §11.7 for the controller use case):** FEASIBLE-ONLY-BOUNDED at L3, metadata-only, RBAC-grant-filtered, capacity-capped. Full-scope L3 = OOM = NOT-ADVISABLE at 50K.

---

## 0. The crux in one paragraph (so the rest is readable)

"Caching a never-navigated CR" decomposes into two distinct caches with different costs and different enumeration problems:

- **L3 (informer substrate)** — register an informer for a discovered-but-un-navigated GVR so the first nav's resolve finds the CR objects already in the lister, identity-free. Enumeration is *easy* (discovery lists every GVR). The cost is *memory + apiserver watch* and it is the make-or-break constraint.
- **L1 (resolved per-user widget output)** — pre-resolve the widget/RESTAction output for cells the user has never opened. Enumeration is the *hard* problem (snowplow has no widget→CR map for cells outside the nav tree) but — critically — the existing `EnumeratePrewarmTargetsForGVR(gvr, verb)` + `AddNavigatedGVR(gvr)` machinery already enumerates per-binding targets for ANY GVR the RBAC index knows, NOT just navigated ones (TRACED §3.4). So L1 enumeration is *solvable*, but it is downstream of, and strictly more expensive than, L3.

The recommendation is **L3-first, bounded** (§5), because L3 is where "first-nav-hit" is actually delivered cheapest, and because L1-without-L3 cannot be done (you can't resolve a widget over a GVR whose informer isn't registered — the resolve would fall through to apiserver, defeating the purpose).

---

## 1. Prior-art check (`feedback_check_k8s_clientgo_prior_art`)

Does client-go already solve "pre-warm an informer for a GVR cheaply, at low priority, bounded"? Examined four primitives:

1. **`metadatainformer.NewFilteredMetadataInformer` (PartialObjectMetadata watch).** TRACED already wired in this repo: `internal/cache/watcher.go:792` (`addResourceTypeMetadataOnlyLocked`), routed by `shouldUseMetadataOnly` (`cache_mode.go:191`). This is the canonical "metadata-only, ~10× smaller" watch — `watcher.go:567` documents ~2.5 KiB/object metadata vs ~20 KiB/object full. **This IS the prior art for the bounded L3 subset** (§5). It is NOT new — the design reuses an existing, tested path.
   - **CAVEAT (TRACED, load-bearing):** `shouldUseMetadataOnly` is PERMANENTLY constant-false in production today (`cache_mode.go:127-167`, #197 resolution). Rule 2 (`!isStreamingException`) returns false for every non-RBAC GVR before the annotation/seed rules are reached. So today every CR informer is a FULL bytes-streaming informer (`watcher.go:1066-1108`, Ship H5 routing inversion). The metadata-only path is *inert-by-construction*, not dormant. Re-activating it for the proactive set is a deliberate routing change the design must make explicitly (a new `EnsureResourceTypeMetadataOnly` call site, which already exists at `watcher.go:931` and bypasses the predicate) — see §5.3.

2. **client-go `cache.NewExpirationStore` / TTL store.** Solves "on-demand Get with TTL eviction" — but it is a polling Get store, not a watch. Wrong shape: a first-nav-hit needs the *object set* present, which means a LIST (and to stay fresh, a WATCH). A TTL Get-store would still cold-miss the first nav (it has nothing until something asks). Rejected.

3. **client-go shared informer factory lazy start / `WaitForCacheSync`.** TRACED reused: the dynamic factory is at `watcher.go:317-322`-equivalent; `WaitAllInformersSynced` (`phase1.go:337`) is the sync barrier. There is NO client-go notion of "low-priority informer" or "preemptible informer" — informers are equal citizens of the factory and a watch's apiserver cost is not throttleable from the client side. **This is the gap that forces a custom lower-priority mechanism (§4): client-go gives us the informer, not the priority.**

4. **`PriorityLevelConfiguration` / APF (API Priority and Fairness).** This is the *apiserver-side* fairness primitive. It is the correct place to express "snowplow's proactive watches must yield apiserver capacity to snowplow's navigated watches" — but it is configured cluster-side (a `flowcontrol.apiserver.k8s.io` object), not from snowplow, and `project_no_upstream_authority` says we cannot rely on cluster-operator-side config cadence. **INFERRED**: APF *could* be a complementary defense (a low-priority FlowSchema matching snowplow's SA on the proactive watches) but cannot be the primary mechanism. Surfaced as an option in §4.4, not the recommendation.

**Prior-art conclusion:** client-go supplies the *metadata-only informer* (reuse `metadatainformer`, already wired) and the *sync barrier*. It does NOT supply (a) a low-priority/preemptible informer-warm scheduler, (b) a capacity-capped GVR-admission policy, or (c) RBAC-grant-filtering of the warm set. Those three are the genuinely-new surface the design must add, and they are small and bounded.

---

## 2. TRACED current state — what IS and ISN'T cached for an un-navigated CR

### 2.1 The demand/nav-driven population paths (the architecture we must not break)

- **Walker is the sole source of "which informers run."** TRACED `internal/handlers/dispatchers/phase1_walk.go:1116` (`walk()`), descends `status.resourcesRefs.items[]` children where `verb == "GET"` only (`walkShouldRecurse`, `phase1_walk.go:1480`). The boot seed is exactly 7 meta-query GVRs (`internal/cache/phase1.go:250` `MetaQuerySeeds` — routesloaders + navmenus + restactions + 4 RBAC); **every business GVR (widgets, panels, compositions) is ABSENT by construction** and only registered by resolution.
- **Informer registration entry point.** TRACED `internal/cache/watcher.go:612` `EnsureResourceType(gvr)` — idempotent, RLock fast-path, registers a full bytes-streaming informer (`addResourceTypeLocked`, `watcher.go:1030`; H5 streaming default `watcher.go:1066-1108`). Walker-driven call sites confirmed at `deps_extract.go`, `resolve.go`, `phase1_pip_seed.go`, `restactions.go` (per v6 doc §2; the entry function is unchanged).
- **CRD discovery side-effect (the v6 doc is stale here — TRACED current code).** A `customresourcedefinitions` informer DOES exist now. `internal/cache/crd_discovery_side_effect.go:1-26` documents it: Ship 0.30.233 restored "a CRD ADD drives discovery for the new CRD's group" via ONE side-effect hook on the EXISTING CRD-meta informer's AddFunc. The CRD GVR predicate is `IsCRDGVR` (`crd_gvr.go:44`). The chain is: CRD ADD/UPDATE → `submitCRDLifecycleEvent` (`crd_discovery_side_effect.go:246`) → bounded worker → `triggerCRDDiscovery` (`:323`) → `AddNavigationDiscoveredGroup` + `DiscoverGroupResources` (`:385`,`:394`). CRD DELETE → `triggerCRDDelete` (`:450`) → `RemoveResourceType` + `OnResourceTypeRemoved` (Ship L/0.30.246).
- **`DiscoverGroupResources`** (`internal/cache/discovery_lookup.go:217`) lists `ServerResourcesForGroupVersion` for every version of a group, skips built-ins (`isBuiltInKind`, `:420`), and calls `rw.EnsureResourceType(gvr)` for each CRD-backed kind. **CRITICAL — this is GROUP-scoped, not GVR-scoped, and it fires only on a CRD lifecycle event or a walker reach.** A group with 500 CRD-backed kinds, of which the user navigates 1, gets ALL 500 informers registered the moment any CRD in that group ADDs. (INFERRED implication for scale §6 — this is an *existing* over-registration the proactive design must not make worse.)
- **L1 resolved-output population.** TRACED two paths: (a) the cohort PIP seed `internal/handlers/dispatchers/phase1_pip_seed.go` (background, best-effort, per-(cohort, restaction/widget) reached by the Phase-1 walker — `:1-80`); (b) the dispatcher `/call` populate on cold-miss. L1 is per-user-keyed via `dispatchCacheLookupKey` under a ctx carrying the cohort's UserInfo (`phase1_pip_seed.go:75-80`) — `feedback_l1_per_user_keyed_never_cohort` is honored.

### 2.2 The precise gap — what an un-navigated CR has TODAY

For a CRD-backed kind **whose group has had a CRD lifecycle event OR a walker reach**: its informer IS registered (via `DiscoverGroupResources`), so its CRs ARE in the L3 lister — a first nav to it would be an L3 HIT. Its L1 is NOT seeded unless the walker reached the *widget* that renders it (the PIP seed harvests only walker-reached widgets/restactions).

For a CRD-backed kind **whose group has had NEITHER event** (e.g. a brand-new feature area no user has opened and whose CRDs predate this pod's boot): **neither L3 nor L1 is populated.** The first nav cold-misses at L3 (resolve falls through to apiserver) AND L1. **This is the gap the brief names, and it is real** (TRACED: nothing in the boot path registers an informer for a group the walker hasn't reached and that hasn't fired a CRD ADD since boot — `phase1.go:250` seeds only 7 GVRs; `DiscoverGroupResources` is only ever called from `triggerCRDDiscovery` or the walker per `crd_discovery_side_effect.go:394` + v6 doc §4.1).

So: **the un-navigated-CR miss exists specifically for GVRs whose group is neither walker-reached nor CRD-event-touched since boot.** The proactive design's job is to close that — cheaply and at lower priority.

### 2.3 The customer-priority machinery we subordinate to (TRACED)

- **Prewarm engine yield** — `internal/handlers/dispatchers/prewarm_engine.go:437` `yieldToCustomer(ctx)`: parks the worker while `customerInFlight()` (`:105`, an `atomic.Int64` incremented at restactions/widgets ServeHTTP entry, `:94-101`), re-checks every `defaultEngineYieldPoll = 25ms` (`:234`). The yield is BEFORE each scope (`runWorker`, `:403`) and per-cohort inside the seed (`engineYieldCheckpoint`, `:457`).
- **Refresher yield** — `internal/cache/refresher.go:580` `yieldToCustomer(ctx)`: same pattern, reads the dispatcher-injected `customerInflightHook` (`:140-185`, wired via `SetCustomerInflightHook` from `dispatchers.CustomerInFlight` — `prewarm_engine.go:118`), polls at `refresherYieldPoll = 25ms` (`:113`), caps a single park at `refresherYieldMaxParked = 5s` (`:124`) as defense-in-depth, counts `yieldedTotal`/`cappedTotal` (`:295-296`).
- The discipline (`feedback_customer_priority_over_refresher`): background work YIELDS its CPU budget; the customer path keeps absolute priority; the yield is cooperative (no hard mutex the customer path needs). **This is the exact pattern the proactive warm must reuse — §4.**

---

## 3. The layer decision + scope-enumeration mechanism

### 3.1 Decision: L3 (informer warm), metadata-only, RBAC-filtered, capped. L1 is a separate, later, dependent ship.

**Why L3 is the right layer for "first-nav-hit":**
- A first nav's resolve serves from the L3 lister identity-free (the informer store is shared across all users; RBAC is applied at serve time per the layering contract). One informer warms the CR set for *every* user at once — O(GVR) cost, not O(user × GVR).
- L1 cannot be warmed without L3: the resolve that produces L1 output reads the CR objects, and if their informer isn't registered the read falls through to apiserver (`cache_mode.go` modePassthrough / lister-miss path). So L1-first is structurally impossible; L3 is the prerequisite.
- L3 is where the existing demand-driven design already lives (`EnsureResourceType`), so the proactive path reuses the same registration plumbing — no parallel cache.

**Why NOT L1 as the primary layer:** L1 is per-user-keyed resolved output (`feedback_l1_per_user_keyed_never_cohort`). Proactively warming un-navigated cells means resolving widget output for cells outside the nav tree for *every cohort that can see them*. Cost is O(cohort × un-navigated-widget) of full resolves — at 1000 users / many cohorts this is the PIP-seed cost multiplied by the un-navigated surface. It also requires the widget→CR mapping for cells snowplow has never walked, which the walker is the only producer of. **L1 is feasible but strictly downstream of L3 and far more expensive — it is a SECOND ship, §3.4 + §7.**

### 3.2 Scope enumeration for L3 — how do we even know which un-navigated GVRs exist?

Snowplow only knows the nav tree today. But it ALSO has two enumeration sources that do NOT depend on navigation:

1. **Apiserver discovery** — `Discovery.ServerGroups()` + `ServerResourcesForGroupVersion` (already used by `DiscoverGroupResources`, `discovery_lookup.go:217`). This enumerates EVERY CRD-backed GVR in the cluster, navigated or not. This is the universe.
2. **The RBAC reverse index `BindingsByGVR`** — `internal/cache/bindings_by_gvr.go`. `EnumeratePrewarmTargetsForGVR(gvr, verb)` (`prewarm_enumeration.go:91`) returns the bindings that grant get/list on ANY gvr, against the published RBAC snapshot, INDEPENDENT of whether that gvr is navigated. **This is the load-bearing enumeration primitive (TRACED §3.4).**

**The enumeration mechanism the design uses (no nav-tree dependency, no static list, `feedback_no_special_cases`):**

```
proactive set = { gvr ∈ discovery(ServerGroups × ServerResourcesForGroupVersion)
                  : gvr is CRD-backed (¬isBuiltInKind)
                  ∧ ¬already-registered (¬rw.IsRegistered(gvr))
                  ∧ ∃ binding in BindingsByGVR granting get/list on gvr   ← RBAC GATE
                }
ranked by (number of granting bindings desc, then group/resource)   ← prioritization
capped at K (empirically-derived, §5.2)
```

The **RBAC gate** is the key scale-and-correctness move: we do NOT warm GVRs no binding grants — those CRs are unreachable by any user, so warming them is pure waste (and an information-exposure smell). This bounds the proactive set to "GVRs some user could navigate," which at production scale is a small fraction of all 30K CRDs (most CRDs back controller-internal types no portal user has a grant on). It also reuses `BindingsByGVR`, which `AddNavigatedGVR` already maintains (`bindings_by_gvr.go:455`).

This enumeration is itself a discovery LIST (bounded by `listPageLimit` paging, `cache_mode.go:332`) + a walk of the in-memory RBAC index (O(GVR × bindings), already paid by `BuildBindingsByGVRIndex`, `bindings_by_gvr.go:387`). It runs once at warm time and on a slow ticker (§4.3), NOT per request.

### 3.3 What gets registered — metadata-only, not full bytes

For each gvr in the capped proactive set, call **`EnsureResourceTypeMetadataOnly(gvr)`** (`watcher.go:931`, already exists, bypasses the inert `shouldUseMetadataOnly` predicate and forces the `metadatainformer` path). This stores `*metav1.PartialObjectMetadata` (~2.5 KiB/object, `watcher.go:567`) instead of the full bytes object (~20 KiB, `bytesobject.go` carries the COMPLETE JSON in `raw`).

**Startup precondition (TRACED, operational dependency the PM gate must verify):** `EnsureResourceTypeMetadataOnly` is a loud-logged no-op `(false, nil)` when `rw.metaClient == nil` (`watcher.go:942-949`). Production must have called `SetMetadataClient(metadata.NewForConfig(rc))` at startup, or the entire proactive warm silently does nothing. Today the metadata path is inert (#197), so `metaClient` MAY not be wired in the current boot sequence — the design must add/confirm that wiring (`watcher.go:690` already documents the `metaClient is nil` regression remediation). The toggle-off falsifier (F-T1) must run WITH `metaClient` wired so it tests the real path, not the silent no-op.

**Correctness consequence (TRACED, must be designed-around):** a metadata-only informer does NOT carry spec/status. A first nav that needs the full object (most widget resolves read spec/status) would get a metadata HIT for *existence/list* but still need a full fetch for content. So metadata-only delivers a **partial** first-nav-hit: the LIST envelope and existence are warm (the expensive cluster-LIST is avoided), but per-object content is fetched on first touch. This is the right tradeoff at scale (§6): warming 30K-CRD-worth of FULL objects is the OOM; warming metadata is ~10× cheaper and still kills the cluster-LIST cold cost, which `project_argocd_apps_scale` + the compositions-list trace show is the dominant first-paint cost.

**Upgrade-on-touch:** when the first real nav reaches a metadata-only proactive GVR, the existing walker path calls `EnsureResourceType(gvr)` — but that's idempotent and would see the GVR already registered (as metadata-only). The design adds a small **promote** step: on first navigated touch of a metadata-only proactive GVR, tear down the metadata informer (`RemoveResourceType`, `watcher.go:1292`, idempotent) and re-register full via `EnsureResourceType`. This is the "navigated CRs always win — they get the full treatment" rule made concrete. (INFERRED LOC ~40; the teardown+re-register primitives both exist.)

### 3.4 L1 enumeration IS solvable (for the later ship) — proof via existing code

The brief asks whether L1 enumeration for cells outside the nav tree is even possible. **It is, TRACED:** `EnumeratePrewarmTargetsForGVR(gvr, verb)` (`prewarm_enumeration.go:91`) returns `[]PrewarmTarget{BindingUID, Subject, GVR, Verb}` for any gvr — each target is a per-binding identity under which a prewarm `/call` can be dispatched, and the cell it populates is shared with every real-user request whose first-match for `(verb, gvr, ns)` is the same binding (the per-binding sharing invariant, `prewarm_enumeration.go:56-59`). And `AddNavigatedGVR(gvr)` (`bindings_by_gvr.go:455`) is exactly the call that widens this index to a newly-discovered GVR — it is ALREADY called from `DiscoverGroupResources` (`discovery_lookup.go:346`) on every discovery-spawned GVR, and ALREADY triggers `scopeKindGVRDiscovered` re-prewarm (`prewarm_engine.go:151`, `gvr_discovered_hook.go`).

So the L1-for-un-navigated-GVR machinery *substantially exists already* — the discovery path already widens the index and enqueues a re-prewarm scope. **The gap is only that this fires on CRD-ADD/walker-reach, not proactively for never-touched groups.** The L1 ship would therefore be: feed the §3.2 proactive set's GVRs through the SAME `AddNavigatedGVR` + `notifyGVRDiscoveredForReprewarm` path the discovery hook already uses (`discovery_lookup.go:345-348`), at low priority. This is why L1 is a *small* follow-on once L3 exists — but it is a separate ship because its cost (O(cohort × GVR) full resolves) needs the L3 ship's empirics to bound.

---

## 4. The lower-priority mechanism (Diego's explicit, load-bearing constraint)

The proactive warm must NEVER delay/degrade a navigated resolve or the customer `/call`. It reuses the EXISTING customer-priority-yield discipline verbatim — no new priority primitive.

### 4.1 Run the warm as a new prewarm-engine scope kind, behind the existing yield

Add `scopeKindProactiveWarm` to the prewarm engine (`prewarm_engine.go:131` enum). The engine worker ALREADY yields to customers before every scope (`runWorker` → `yieldToCustomer`, `:403`) and the seed loop yields per-unit (`engineYieldCheckpoint`, `:457`). By running the proactive warm AS an engine scope, it inherits:
- **CPU yield**: `customerInFlight()` park at 25ms cadence (`:437`) — the warm steps aside for the entire duration of any customer burst.
- **Bounded dedup queue**: one scope coalesces (`enqueueScope`, `:252`).
- **Process-lifetime ctx** (`:304` contract) — survives the boot-seed goroutine's death (the §1.5 trace lesson — don't bind to the boot ctx).

**Per-GVR yield checkpoint (the load-bearing addition):** the proactive scope handler must call `engineYieldCheckpoint(ctx)` BEFORE each `EnsureResourceTypeMetadataOnly(gvr)` call (not just before the scope) so a customer burst arriving mid-warm defers the NEXT GVR registration. This is the same per-cohort checkpoint the PIP seed uses; ~1 LOC per GVR-loop-iteration.

### 4.2 Navigated CRs always win — explicit ordering

- A navigated touch of a proactive metadata-only GVR PROMOTES it to full (§3.3) and removes it from the proactive backlog. Navigated work is never blocked waiting for the warm because the warm holds no lock the resolve path needs (the yield is cooperative; `EnsureResourceType`'s RLock fast-path, `watcher.go:625`, means a navigated registration of an already-registered GVR never even takes the writer lock).
- The proactive scope is LOWEST priority in the engine: boot re-walk and `scopeKindGVRDiscovered` scopes drain first. INFERRED implementation: dequeue ordering in `dequeueScope` (`:265`) is currently map-iteration-order (non-deterministic); the design adds a 2-class priority (proactive last) — ~15 LOC, mirrors the refresher's two-tier `processNext` (`refresher.go:651-704`).

### 4.3 Throttle + slow cadence (apiserver-quota yield)

The warm is not a one-shot — new un-navigated GVRs appear as CRDs install. But it must not hammer discovery. Design:
- **One discovery LIST per cadence tick** (default 30 min, env-tunable `PROACTIVE_WARM_INTERVAL` — a tuning knob under the master gate, not a feature flag), reusing `serverVersionsForGroup` + `ServerResourcesForGroupVersion` (`discovery_lookup.go:373`).
- **Rate-limit informer spawns**: at most M new metadata informers per tick (default M = small, e.g. 20), the rest deferred to the next tick. This bounds the apiserver WATCH-establishment burst. M is a tuning knob; the steady-state ceiling is K total (§5.2), so the warm reaches K over ⌈K/M⌉ ticks then idles.
- **APF complement (optional, §4.4).**

### 4.4 Option: apiserver-side APF FlowSchema (surface to Diego, NOT the default)

INFERRED: a `flowcontrol.apiserver.k8s.io/FlowSchema` matching snowplow's SA on the proactive watches, mapped to a low `PriorityLevelConfiguration`, would make the apiserver itself shed proactive watch-establishment under load before navigated watches. This is the cleanest apiserver-quota-yield — but it is cluster-operator-side config (`project_no_upstream_authority`: we cannot rely on its cadence) and it cannot distinguish snowplow's proactive vs navigated watches unless they use distinct SAs (they don't today). **Recommendation: ship the in-process yield (§4.1-4.3) as primary; file APF as a future hardening option for the chart/platform team, not a snowplow code dependency.**

---

## 5. Scale safety — the make-or-break analysis

### 5.1 The blast radius if we did the naive thing (why "cache everything" is rejected)

TRACED facts that bound the worst case:
- **30K CRDs, 50K compositions, 29,907 ArgoCD Applications** (`project_production_scale`, `project_argocd_apps_scale`).
- Full bytes informer cost: **~20 KiB/object** (`watcher.go:569`). The 0.30.92 OOM was 49K compositions × ~20 KiB ≈ ~1 GiB on ONE GVR's indexer (`cache_mode.go:6-9`).
- If we registered a FULL informer for every discovered GVR's CRs: the ArgoCD Applications alone (29,907 × ~20 KiB ≈ ~600 MiB) plus compositions (~1 GiB) plus the long tail of every other CRD-backed kind = **multi-GiB, blows the 2 GiB container limit** — exactly the OOM the metadata-only routing (`cache_mode.go`) and the bytesObject rebuild (`bytesobject.go`) were built to avoid. **Naive full-scope L3 warm = re-introduce the OOM. REJECTED.**

### 5.2 The bounded design's cost + the empirical-cap REQUIREMENT

The bounded design caps the proactive set at **K metadata-only informers**, RBAC-grant-filtered (§3.2). Cost model:

```
worst-case proactive RSS ≈ K × (avg objects per proactive GVR) × per-object-metadata-cost
worst-case proactive WATCH count ≈ K  (one watch per metadata informer)
```

**The cap K and the per-object cost MUST be empirically derived (`feedback_capacity_caps_empirical_per_entry_cost`), NOT guessed.** This is a HARD design requirement, flagged because the D.3/0.30.151 lesson is a 180× estimation error. The empirical inputs needed:
1. **Per-object metadata-informer cost** on the target cluster — controlled-injection: register N metadata informers over representative GVRs, measure RSS delta / total-objects. The `watcher.go:567` "~2.5 KiB/object" is a design-time figure and MUST be confirmed (Go runtime boxing of `PartialObjectMetadata` + ObjectMeta maps typically 2-5× the wire shape).
2. **Object-count distribution across grant-reachable un-navigated GVRs** — `kubectl get <gvr> --raw .../?limit=1` per candidate GVR reads `metadata.remainingItemCount` for an O(1) count estimate; sum over the RBAC-filtered candidate set.
3. **The cap**: `K_objects_budget = per_object_metadata_cost⁻¹ × (RSS_headroom / safety)`. Express K as an OBJECT budget, not a GVR count — a GVR with 29,907 objects (ArgoCD apps) costs 29,907× a GVR with 3 objects. The ranking (§3.2) admits GVRs until the object budget is hit.

**BLOCKER FOR THIS DESIGN'S CAP NUMBERS (TRACED, must be surfaced):** `project_current_state` records **CLUSTER-1 was DELETED 2026-06-14** — there is NO live cluster to probe today. Per `feedback_capacity_caps_empirical_per_entry_cost`'s own escape hatch, with no target cluster the cap AC **converts to a soft observation gate** ("log expected vs observed proactive RSS; investigate >2× discrepancy; HALT proactive admission if RSS_proactive > budget") rather than a hard pre-ship number. The empirical measurement is a **pre-ship falsifier to run on the re-stood-up cluster** (§7 sequencing). The design ships with the budget as a runtime-enforced, env-tunable ceiling that self-limits admission, so a wrong estimate degrades gracefully (warm stops admitting) instead of OOMing.

### 5.3 Self-limiting admission (the safety net that makes a wrong cap non-fatal)

The proactive scope checks live RSS (or the existing `RESOLVED_CACHE_MAX_RESIDENT_BYTES`-style accounting, `feedback_capacity_caps_empirical_per_entry_cost` cites `1.5 GiB` ratified at 0.30.245) before admitting each metadata informer:
- If projected `RSS + next_gvr_metadata_cost > proactive_budget` → STOP admitting, log `cache.proactive_warm.budget_reached`, idle until next tick.
- This makes a mis-estimated K non-catastrophic: the warm self-throttles at the RSS ceiling rather than OOM-killing the pod. Mirrors the refresher's `cappedTotal` defense-in-depth posture (`refresher.go:603`).

### 5.4 What's IN vs OUT (explicit)

| Item | In/Out | Why |
|---|---|---|
| Discovered CRD-backed GVRs with ≥1 RBAC grant | IN (capped, metadata-only) | Reachable by some user; first-nav-hit benefit real |
| Discovered GVRs with ZERO RBAC grant | OUT | Unreachable by any user — pure waste + exposure smell |
| Built-in kinds (`isBuiltInKind`) | OUT | Not CRD-backed; covered by demand path; many are control-plane noise |
| Already-registered GVRs (navigated / CRD-event-touched) | OUT | Already warm; would double-register |
| FULL bytes informers for the proactive set | OUT | The OOM (§5.1). Metadata-only only |
| Per-object content (spec/status) for proactive set | OUT (fetched on first touch) | Metadata-only is the cap; content warms on promote |
| L1 resolved output for un-navigated cells | OUT of THIS ship (separate ship §7) | Depends on L3; O(cohort×GVR) cost needs L3 empirics first |

---

## 6. Cache invariants (compliance check)

- **Per-user-keyed L1 / never cohort-leak** (`feedback_l1_per_user_keyed_never_cohort`): THIS ship touches L3 only (identity-free informer substrate). No L1 write. The later L1 ship reuses `EnumeratePrewarmTargetsForGVR` + the dispatcher's per-user `dispatchCacheLookupKey` (the exact path PIP uses, `phase1_pip_seed.go:75-80`) — no cohort-keyed cell. COMPLIANT.
- **Provisional + removable, default-OFF, single master gate** (`project_caching_is_provisional`, `feedback_single_cache_flag_direction`): new feature gated by a new tuning knob (e.g. `PROACTIVE_WARM_ENABLED`, default false initially) UNDER the `CACHE_ENABLED` master gate — when cache is off the prewarm engine is inert (`prewarm_engine.go` requires `PrewarmEnabled()` = `!Disabled()`, `phase1.go:73`) so the proactive scope never runs. Removing the feature = delete the scope kind + its enumerator; the L3 substrate is unchanged. COMPLIANT.
- **`CACHE_ENABLED=false` ⇒ inert** (`project_cache_off_is_transparent_fallback`): `DiscoverGroupResources` already soft-no-ops in `modePassthrough` (`discovery_lookup.go:222`); `EnsureResourceTypeMetadataOnly` is a no-op when the watcher is passthrough. The proactive scope is gated on `PrewarmEnabled()`. Cache-off ⇒ byte-identical to today. COMPLIANT.
- **No special-cases** (`feedback_no_special_cases`): the proactive set is derived purely from `Discovery` + `BindingsByGVR` (cluster state), ranked by grant count — zero hardcoded GVR/resource/user literals. The only literal is `IsCRDGVR`/`isBuiltInKind` which already exist as the CRD/built-in discriminators. COMPLIANT.
- **`feedback_cache_must_not_constrain_jq` / `feedback_restaction_no_widget_logic`**: metadata-only changes the STORAGE shape for the proactive set pre-promotion; on promote (first real nav) the GVR becomes a full informer BEFORE any widget resolve serves it, so widget-prop output for valid expressions is unchanged. The promote-before-serve ordering (§3.3) is the load-bearing guarantee here. COMPLIANT (must be a falsifier — §7).

---

## 7. Falsifier set (kind/unit; NEVER remote `go test` per `feedback_no_go_test_against_remote_kubeconfig`)

**Benefit (first-nav-hit):**
- F-B1 (unit, fake discovery + fake informer): after a proactive warm tick, `rw.IsRegistered(proactiveGVR)` is true AND `rw.IsMetadataOnly(proactiveGVR)` is true for a GVR the walker never reached. (`watcher.go:978` `IsMetadataOnly`, `phase1.go:427` `IsRegistered`.)
- F-B2 (kind/envtest): create a CRD-backed GVR with N objects, no nav, no widget; run a warm tick; assert a subsequent LIST against that GVR's lister returns N items with ZERO apiserver fallthrough (the cluster-LIST cold cost is gone). Content-level: the LIST envelope is non-empty and item count = N (`feedback_validate_content_not_just_status`).
- F-B3 (kind): on first navigated touch of a proactive metadata GVR, assert it promotes to full (`IsMetadataOnly` flips false) BEFORE the widget resolve serves, and widget-prop output is byte-identical to a never-proactive control (`feedback_cache_must_not_constrain_jq`).

**Priority (THE load-bearing one):**
- F-P1 (kind, -race, concurrent): drive a sustained navigated `/call` load concurrently with the proactive scope; assert (a) navigated `/call` p50/throughput is within ±5% of warm-disabled control, and (b) the engine's `yieldTotal` for the proactive scope rises under the burst (proves it yielded). Mirrors the refresher's `yieldedTotal` gate (`refresher.go:295`).
- F-P2 (kind): assert the proactive scope NEVER holds a lock on the navigated resolve path — `EnsureResourceType` of an already-registered GVR returns via the RLock fast-path (`watcher.go:625`) with zero writer-lock contention while a proactive warm is mid-flight.
- F-P3 (kind): under sustained customer burst, the proactive scope makes ZERO forward progress (every `engineYieldCheckpoint` parks) — navigated work fully starves the warm, never the reverse.

**Scale (empirical-cap):**
- F-S1 (PRE-SHIP, on re-stood-up cluster — soft gate per §5.2): controlled-injection of N metadata informers; measure RSS delta/object; confirm vs the design figure; investigate >2× discrepancy. This is the empirical-cap derivation, NOT a hard revert gate while no cluster exists.
- F-S2 (runtime expvar): `cache.proactive_warm.objects_resident` ≤ object budget; `cache.proactive_warm.budget_reached` increments and admission STOPS when projected RSS exceeds budget (§5.3). The self-limit is the airtight scale falsifier — even with a wrong K, RSS never exceeds budget.
- F-S3 (runtime expvar): `cache.proactive_warm.watch_count` ≤ K; one WATCH per admitted GVR, no leak.

**Toggle-off:**
- F-T1 (kind): `PROACTIVE_WARM_ENABLED=false` ⇒ the proactive scope is never enqueued; `NavigationDiscoveredGroupsSnapshot()`, `RegisteredGVRs()`, and L1 contents are byte-identical to a control run (demand-driven behavior unchanged). `CACHE_ENABLED=false` ⇒ same, via `PrewarmEnabled()=false`.

---

## 8. Options with recommendation (strategic surface for PM gate → Diego)

**Option A (RECOMMENDED): L3 metadata-only, RBAC-filtered, object-budget-capped, run as a low-priority prewarm-engine scope.** Delivers partial first-nav-hit (kills cluster-LIST cold cost) at ~10× lower memory than full; self-limits at an RSS budget; reuses the existing yield + discovery + RBAC-index plumbing; ~250-350 LOC INFERRED (scope kind + enumerator + metadata-spawn loop + budget guard + promote-on-touch + expvars). Empirical cap = pre-ship falsifier on re-stood-up cluster (soft gate now). **Verdict: FEASIBLE-ONLY-BOUNDED. Recommend ship.**

**Option B: L3 full-bytes warm of the RBAC-filtered set.** Delivers COMPLETE first-nav-hit (content too). But full-bytes at 30K-CRD/29,907-app scale is the OOM (§5.1) unless the cap is so tight it warms almost nothing. **Verdict: NOT-ADVISABLE at 50K. Reject.**

**Option C: L1 proactive resolve of un-navigated cells (the brief's literal "cache the CR's resolved output").** Feasible (enumeration solvable, §3.4) but strictly downstream of L3 and O(cohort × un-navigated-GVR) full resolves — must be gated on Option A's empirics. **Verdict: FEASIBLE as a SECOND ship, after A. Recommend defer.**

**Option D: do nothing / rely on demand + CRD-ADD discovery.** The status quo. The gap (§2.2) is narrow (only groups neither walker-reached nor CRD-event-touched since boot). If the empirical first-nav-miss rate on the re-stood-up cluster is low, Option D may be the right call. **Verdict: viable null hypothesis — the pre-ship measurement (how often does a real first-nav actually cold-miss?) should gate whether A is worth building at all.**

**My recommendation:** before building anything, run **F-D0 (a new pre-ship measurement)**: instrument the deployed pod for "first-nav cold-miss against an un-navigated, grant-reachable GVR" rate over a representative session. If that rate is material → ship **Option A** (bounded L3, empirical cap as soft gate). If negligible → **Option D** and close. This is the `feedback_data_driven_workflow` / `feedback_baseline_before_speculation` discipline: prove the gap hurts before spending the LOC.

---

## 9. Citations (file:line, this tree)

- `internal/cache/phase1.go:250` (`MetaQuerySeeds`, 7 GVRs), `:73` (`PrewarmEnabled`=`!Disabled()`), `:337` (`WaitAllInformersSynced`), `:427` (`IsRegistered`).
- `internal/cache/watcher.go:612` (`EnsureResourceType`), `:625` (RLock fast-path), `:931` (`EnsureResourceTypeMetadataOnly`), `:978` (`IsMetadataOnly`), `:1030` (`addResourceTypeLocked`), `:1066-1108` (H5 streaming default), `:1292` (`RemoveResourceType`), `:567-569` (~2.5 KiB metadata / ~20 KiB full).
- `internal/cache/cache_mode.go:191` (`shouldUseMetadataOnly`), `:127-167` (#197 inert-by-construction), `:6-9` (0.30.92 OOM), `:332` (`listPageLimit` paging).
- `internal/cache/discovery_lookup.go:217` (`DiscoverGroupResources`), `:345-348` (`AddNavigatedGVR` + `notifyGVRDiscoveredForReprewarm`), `:373` (`serverVersionsForGroup`), `:420` (`isBuiltInKind`).
- `internal/cache/crd_discovery_side_effect.go:1-26` (CRD informer restored — v6 doc stale), `:246` (`submitCRDLifecycleEvent`), `:323` (`triggerCRDDiscovery`), `:450` (`triggerCRDDelete`).
- `internal/cache/crd_gvr.go:44` (`IsCRDGVR`).
- `internal/cache/bindings_by_gvr.go:387` (`BuildBindingsByGVRIndex`), `:455` (`AddNavigatedGVR`).
- `internal/cache/prewarm_enumeration.go:91` (`EnumeratePrewarmTargetsForGVR`), `:56-59` (per-binding sharing invariant).
- `internal/cache/gvr_discovered_hook.go:63` (`RegisterGVRDiscoveredHook`), `:91` (`notifyGVRDiscoveredForReprewarm`).
- `internal/cache/refresher.go:580` (`yieldToCustomer`), `:113`/`:124` (yield poll/cap), `:295-296` (`yieldedTotal`/`cappedTotal`), `:651-704` (two-tier `processNext`), `:140-185` (customer-inflight hook).
- `internal/cache/bytesobject.go:1-22` (full-object ~20 KiB storage shape, GC rationale).
- `internal/handlers/dispatchers/prewarm_engine.go:105` (`customerInFlight`), `:131-158` (scope-kind enum), `:151` (`scopeKindGVRDiscovered`), `:403`/`:437`/`:457` (yield sites), `:304` (process-lifetime ctx contract).
- `internal/handlers/dispatchers/phase1_pip_seed.go:1-80` (PIP cohort seed, per-user-keyed), `:75-80` (`dispatchCacheLookupKey` per-user under cohort ctx).
- `internal/handlers/dispatchers/phase1_walk.go:1116` (`walk`), `:1480` (`walkShouldRecurse`, verb==GET only).
- Scale anchors: `project_production_scale` (1000 users / 50K comps / 30K CRDs), `project_argocd_apps_scale` (29,907 apps), `feedback_capacity_caps_empirical_per_entry_cost` (D.3 180× lesson; 0.30.245 1.5 GiB ratified), `project_current_state` (CLUSTER-1 DELETED 2026-06-14 — no live cluster to probe).

**Stale-source note:** `docs/walker-driven-informer-design-2026-06-01.md` (v6) claims "no CRD informer, ever." That is FALSE in the current tree — Ship 0.30.233 restored a CRD-meta informer with an AddFunc side-effect (`crd_discovery_side_effect.go`). This design traces the current code, not v6.

---

# §11. RE-SCOPED DESIGN — L1 prewarm so a composition-dynamic-controller's `/call` for a RESTAction is a HIT

**Use case (concrete, Diego 2026-06-20):** the composition-dynamic-controller (CDC) issues a `/call?resource=restactions&...&name=<RA>` to snowplow and wants that `/call` to short-circuit to an L1-RESOLVED hit (return cached `RawJSON`, skip `restactions.Resolve`), the same way a warmed human `/call` does. This is the deferred Option C, now primary.

## 11.1 The L1-keying crux — TRACED. What the prewarm must reproduce BYTE-FOR-BYTE.

A controller `/call` for a RESTAction is an L1 hit **iff** a prior prewarm `Put` an entry under the EXACT key the controller's `/call` computes. The key is `ComputeKey(ResolvedKeyInputs)` (`internal/cache/resolved.go:608-692`). For the `"restactions"` class it folds, in order:

`resolvedKeyVersion · CacheEntryClass("restactions") · Group · Version · Resource · Namespace · Name · BindingUID · PerPage · Page · [Stage if non-empty] · canonicaliseExtras(Extras)`
(`resolved.go:612-689`)

The RESTAction `/call` populates those inputs at `restactions.go:123-126` → `dispatchCacheLookupKey(ctx, "restactions", GVR.Group, GVR.Version, GVR.Resource, ns, name, perPage, page, extras)` (`helpers.go:200-243`). So what the prewarm MUST match:

| Key field | Source on the controller `/call` (TRACED) | What the prewarm must reproduce |
|---|---|---|
| `CacheEntryClass` | literal `"restactions"` (`restactions.go:123`) | use the same dispatcher seed path (PIP already passes `"restactions"`, `phase1_pip_seed.go`) — automatic |
| `Group/Version/Resource` | `got.GVR` from `util.ParseGVR(req)` — the `restactions.templates.krateo.io` GVR of the RA CR (`helpers.go:30`, `fetchObject`) | the RESTAction's own GVR — known from the RA CR object |
| `Namespace/Name` | `got.Unstructured.GetNamespace()/GetName()` — the RA CR's ns+name (`restactions.go:125`) | the specific RA CR's ns+name — known |
| **`BindingUID`** | `rbac.EvaluateRBAC(Verb:"get", Group, Resource, Namespace, Name)` first-match binding UID for the request identity (`helpers.go:212-220`) | **THE CRUX** — see §11.2. Must resolve under an identity whose first-match `get` binding on this RA CR is the SAME binding the controller's identity matches |
| `PerPage/Page` | `paginationInfo(req)` → `-1,-1` when absent; `page=1` only if `perPage>0` (`helpers.go:53-78`) | match the controller's pagination. If the controller calls un-paginated → seed with `PerPage=-1, Page=-1`. **Keying-risk if the controller paginates** (§11.6) |
| `Stage` | empty for the top-level `"restactions"` class (only the `apistage` sub-class sets it) | empty — automatic |
| **`Extras`** | `util.ParseExtras(req)` = JSON from `?extras=<json>`, empty map when absent (`extras.go`), canonicalised sorted-key (`resolved.go:697`) | **must byte-match the controller's extras.** If the controller passes `?extras=...` the prewarm doesn't know → key diverges → MISS. **Keying-risk** (§11.6) |

**The load-bearing finding (TRACED, decisive for feasibility):** **the endpoint / `<username>-clientconfig` is NOT in the key.** Identity enters the key ONLY as `BindingUID` (`resolved.go:652-655`). The endpoint determines which apiserver the RESOLVE dispatches against (`restactions.go:185-188` attaches the SA transport; per-user identity flows separately), but it does NOT shift the key. **Consequence:** the prewarm does NOT need the controller's token or clientconfig to produce a matching KEY — it needs only to resolve under SOME identity whose first-match `get`-binding on the RA CR equals the controller's. This is the per-binding sharing invariant (`prewarm_enumeration.go:56-59`, `resolved.go:631-637`): two identities granted by the same binding produce byte-identical keys AND byte-identical output. **This is exactly the cell-sharing the existing PIP/engine prewarm already exploits — the controller is just another member of a binding's equivalence class.**

## 11.2 The BindingUID match — the single hard correctness condition

The prewarm cell the controller hits must be keyed under the BindingUID that `rbac.EvaluateRBAC(get, restactions-GVR, ns, name)` returns **for the controller's identity**. `EvaluateRBAC` first-match semantics (per `l1-key-design-evaluation-2026-06-03.md` §1.2 trace of the upstream authorizer): CRBs walked first, then ns-scoped RBs; the FIRST binding whose subjects match AND whose role grants `get` on the RA wins, with a stable sort for determinism (`SkipBindingUID:false` path, `helpers.go:212`).

So the prewarm must enumerate, for the RESTAction GVR, the bindings that grant `get` on it, and pre-resolve ONE cell per binding under a representative subject of that binding. **`EnumeratePrewarmTargetsForGVR(restactionsGVR, "get")` does exactly this** (`prewarm_enumeration.go:91`): returns `[]PrewarmTarget{BindingUID, Subject(representative), GVR, Verb}` for every binding granting get/list on the GVR, INDEPENDENT of nav. The engine then resolves under each target's representative `Subject` and Puts under that `BindingUID`. When the controller calls, its `EvaluateRBAC` first-match returns one of those same BindingUIDs → HIT.

**The one subtlety the design must honor:** the controller's FIRST-MATCH binding must be among the enumerated set. It is, BY CONSTRUCTION, iff the controller's identity is a subject of a binding that grants `get` on the RA (§11.3 precondition). If the controller matches a binding the index doesn't know (e.g. the index wasn't widened to the RESTActions GVR), it misses. RESTActions IS a boot seed GVR (`phase1.go:250` `MetaQuerySeeds` includes `restActionGVR`), so `BuildBindingsByGVRIndex` already includes it at boot — no `AddNavigatedGVR` widening needed for RESTActions. **TRACED: this prerequisite is already satisfied.**

## 11.3 Identity model — OPEN QUESTIONS for Diego (the hard preconditions)

`/call`'s identity is non-negotiable and fully traced (`middleware/userconfig.go:130-237`):
1. `Authorization: Bearer <jwt>` required (`:134-145`) — no header ⇒ 401.
2. `jwtutil.Validate(signingKey, token)` ⇒ `userInfo.Username` + `userInfo.Groups` (`:151`).
3. Secret name = `<MakeDNS1123Compatible(Username)>-clientconfig` (`:166-167`); looked up in `AUTHN_NAMESPACE` via the informer-cached `api.FromInformerSecret` (`:169`), apiserver fallback on miss (`:211`). **No clientconfig Secret ⇒ 401** (`:217-219`).
4. `WithUserInfo(userInfo)` + `WithUserConfig(ep)` on ctx (`:229-233`); downstream `EvaluateRBAC` reads `Username`+`Groups` (`helpers.go:96`, `:212`).

**Therefore snowplow REQUIRES of the CDC, for ANY of this to work (hard preconditions — surface to Diego):**

- **OQ-IDENTITY-1 (the gating question):** What JWT does the CDC present to `/call`? It must be a `snowplow`-signing-key-validating JWT carrying a `Username` (+`Groups`). If the CDC uses a raw k8s SA token, that is NOT a snowplow-issued JWT and `jwtutil.Validate` rejects it ⇒ 401 ⇒ the call never even reaches L1. **If the CDC cannot present a snowplow JWT, the entire feature is moot — this must be answered first.**
- **OQ-IDENTITY-2:** Does the CDC's identity have a `<username>-clientconfig` Secret in `AUTHN_NAMESPACE`? Without it, `/call` is 401 (`userconfig.go:217`). The prewarm doesn't need the Secret (§11.1 — endpoint not in key), but the CDC's LIVE `/call` does. So the Secret must exist for the controller's real call to authenticate, even though the cell was seeded identity-light.
- **OQ-IDENTITY-3:** Is the CDC's identity (its `Username`/`Groups`) a subject of an RBAC binding that grants `get` on the target RESTAction CR? If not, `checkDispatchRBAC` denies the `/call` with 403 (`restactions.go:96-107`) BEFORE the L1 lookup — so there is nothing to warm and no hit is possible. The CDC must be a known, RBAC-granted principal. **This is the per-binding-class precondition: no grant ⇒ no cohort ⇒ no keyable cell.**

If OQ-IDENTITY-1/2/3 are all "yes" → the CDC is a normal cohort and the L1 prewarm keys to it cleanly. If any is "no" → flag as a HARD precondition the platform/chart must satisfy (issue the CDC a snowplow JWT + clientconfig Secret + an RBAC binding granting `get` on the RAs it calls). **None of these can be worked around in snowplow code** (`feedback_no_special_cases` forbids a hardcoded CDC bypass; and bypassing auth/RBAC for a controller would be a security defect).

## 11.4 Enumeration — does extending the existing cohort prewarm solve it? YES, with one widening.

**Assessment: extending the existing cohort prewarm to RBAC-reachable-RESTActions-beyond-nav solves it. No new cache mechanism is needed** — only a new enumeration SOURCE feeding the existing engine.

Today the PIP seed harvests RESTActions REACHED BY THE WALKER (`phase1_pip_seed.go:11-13` "once per (cohort, restaction) reached by the Phase-1 walker"). A CDC's RESTAction is not in the human nav tree, so the walker never harvests it ⇒ never seeded. The fix:

- **Enumerate the seed set from RBAC, not from the walk.** For the RESTActions GVR (already in `BindingsByGVR` via the boot seed, §11.2), call `EnumeratePrewarmTargetsForGVR(restactionsGVR, "get")` to get every (binding, representative-subject) that can `get` a RESTAction. This is the universe of cohorts that could ever hit an RA `/call` — INCLUDING the CDC's cohort, with zero nav dependency and zero hardcoded RA name (`feedback_no_special_cases` clean — purely RBAC-derived).
- **For each target, enumerate the RESTAction CR OBJECTS the binding grants and pre-resolve each.** A binding grants `get` on a (group,resource) possibly cluster-wide or ns-scoped; the concrete RA CRs are LISTable from the `restactions` informer (boot seed ⇒ already warm at L3 — the §11 prerequisite is FREE, unlike the §0–§10 L3 path). For each RA CR `(ns,name)` the target can get, dispatch a prewarm resolve under the target's representative `Subject` and `Put` under `ComputeKey`. This reuses the EXISTING `runPIPSeed` per-(cohort,restaction) loop shape (`phase1_pip_seed.go`) verbatim — only the restaction SET changes from "walker-harvested" to "RBAC-reachable."

**This is the cohort-prewarm engine doing what it already does, fed a wider RA set.** `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill` is honored: ONE dynamic engine, no static list, re-runs on RA/binding CRUD via the existing `scopeKindGVRDiscovered` + RBAC-shift hooks (`prewarm_engine.go:151`).

## 11.5 Lower priority — reuse the existing customer-yield verbatim (unchanged from §4)

The CDC prewarm is BACKGROUND and yields to human customer `/call`:
- Run as the existing engine BOOT scope's RA-seed (or a new `scopeKindControllerRASeed`) — inherits `yieldToCustomer` before every scope (`prewarm_engine.go:403/437`) and the per-cohort `engineYieldCheckpoint` (`:457`), and the refresher's customer-inflight hook (`refresher.go:580`). A human `/call` brackets `markCustomerInFlight` (`restactions.go:77`), so the CDC seed parks for the burst's duration. **The CDC is itself a `/call` client, but its prewarm is background; its own live `/call` is a normal customer dispatch and is NOT yielded** — only the proactive SEED yields. Navigated/human work always wins (§4.2 ordering applies).

## 11.6 Keying-risks (params the prewarm can't know ahead of time)

- **Extras (HIGH risk).** If the CDC passes `?extras=<json>`, the prewarm must reproduce the EXACT extras (canonicalised, `resolved.go:679-688`) or the key diverges → permanent MISS. The prewarm cannot know arbitrary controller-supplied extras a priori. **OQ-SECONDARY-A: does the CDC pass extras on its RA `/call`?** If yes and they vary per call, L1 cannot pre-key them — the design degrades to "seed the no-extras cell" and only no-extras calls hit. If the CDC's extras are FIXED/known, seed with those.
- **Pagination (MEDIUM risk).** `perPage/page` fold into the key (`resolved.go:657-660`). If the CDC paginates with values the seed didn't use → MISS. Most controller RA calls are un-paginated (`-1/-1`); seed that. **OQ-SECONDARY-B: does the CDC paginate?**
- **BindingUID drift (LOW, self-healing).** If RBAC changes between seed and call, the controller's first-match binding may differ from the seeded one → MISS → falls through to a correct live resolve + repopulates. The RBAC-shift hook re-seeds. Not a correctness issue, just a transient miss.
- **resolvedKeyVersion / pod restart.** Keys rotate on `resolvedKeyVersion` bump and the cache is per-pod in-memory — a fresh pod is cold until the seed runs. The CDC's first call after a pod restart may miss until the background seed completes (bounded by `pipGlobalTimeout`). Acceptable; same posture as human cohorts.

## 11.7 Feasibility verdict (RE-SCOPED)

**FEASIBLE — the cleanest of the three options, and materially simpler than the §0–§10 L3 path**, BECAUSE:
1. The L3 prerequisite is FREE: RESTActions is a boot seed GVR (`phase1.go:250`), so its informer is already warm and its bindings already indexed (`BuildBindingsByGVRIndex` at boot). No §5 OOM analysis applies — this is L1-resolve cost only.
2. The key does NOT fold the endpoint/clientconfig (§11.1) — the prewarm needs no controller credentials to produce a matching key; per-binding cell-sharing (`resolved.go:631-637`) makes the controller a normal equivalence-class member.
3. The enumeration mechanism EXISTS: `EnumeratePrewarmTargetsForGVR` (`prewarm_enumeration.go:91`) already returns per-binding RA targets RBAC-independently of nav.
4. The priority + per-user-keying + toggle machinery is the existing engine (§11.5), unchanged.

**Scale:** bounded by `#RESTActions × #cohorts-granting-get-on-them`. RESTActions are FEW (tens, not 50K) and the granting-cohort count is bounded by the RBAC binding count. This is the SAME order as the existing PIP seed (`phase1_pip_seed.go` already seeds per-(cohort,restaction)); the increment is only the RAs outside the nav tree. Empirical cap = `per-RA-resolved-cell-bytes × #RAs × #cohorts × 2-safety`, derived on the re-stood-up cluster (CLUSTER-1 DELETED 2026-06-14 — soft observation gate now, `feedback_capacity_caps_empirical_per_entry_cost`). The existing `RESOLVED_CACHE_MAX_RESIDENT_BYTES` (1.5 GiB ratified, 0.30.245) is the live ceiling; the seed self-limits under it (§5.3 reused).

**LOC estimate (INFERRED):** ~120–200 LOC — a new RBAC-reachable-RA enumerator (wrap `EnumeratePrewarmTargetsForGVR(restactionsGVR,"get")` + LIST the granted RA CR objects from the boot-seed informer) + feed the existing `runPIPSeed`/engine loop + a toggle (`PROACTIVE_RA_SEED_ENABLED`, default-OFF under `CACHE_ENABLED`) + expvars. No new cache, no new key shape, no new priority primitive.

**Verdict: SHIP-CANDIDATE, pending the OQ-IDENTITY answers (§11.3) which are HARD GO/NO-GO preconditions.**

## 11.8 Invariants (compliance)

- **Per-user-keyed L1 / no cohort leak** (`feedback_l1_per_user_keyed_never_cohort`): the seed Puts under `ComputeKey` with `BindingUID` identity (`resolved.go:652`) via the existing per-user `dispatchCacheLookupKey` path — the cell is keyed by the matched binding, and per-binding sharing IS the equivalence-class invariant (every member produces byte-identical output, `resolved.go:631-637`). No cohort-only key. COMPLIANT.
- **Toggle/removable, default-OFF, under `CACHE_ENABLED`** (`project_caching_is_provisional`, `feedback_single_cache_flag_direction`): new `PROACTIVE_RA_SEED_ENABLED` tuning knob, default false, gated under `PrewarmEnabled()` (`phase1.go:73`) which is implicit-on-`CACHE_ENABLED`. Cache-off ⇒ inert ⇒ byte-identical demand path. COMPLIANT.
- **No special-cases** (`feedback_no_special_cases`): the RA set is `EnumeratePrewarmTargetsForGVR(restactionsGVR,"get")` + the LISTed RA CR objects — zero hardcoded RA names, zero hardcoded CDC identity. The CDC is not named anywhere; it is just whatever cohort the RBAC index produces. COMPLIANT.
- **RESTAction unordered / widget canonicalizes** (`feedback_restaction_no_widget_logic`, `feedback_cache_must_not_constrain_jq`): the seed resolves the RA through the SAME `restactions.Resolve` the live `/call` uses (`restactions.go:212`), producing byte-identical `RawJSON` — the cell IS what the live call would compute. COMPLIANT.

## 11.9 Falsifier set (kind/unit; NEVER remote `go test`)

- **F-RA1 (benefit, kind):** seed under cohort C's representative subject for RA `(ns,name)`; then issue a `/call` for that RA as a DIFFERENT identity in the SAME binding class; assert `dispatcher.call.complete l1_hit:"hit"` and `RawJSON` byte-identical to a live resolve. Proves per-binding cell-sharing delivers the controller hit. (`restactions.go:138` emits `l1Hit`.)
- **F-RA2 (key parity, unit):** assert `ComputeKey` over the seed inputs == `ComputeKey` over the controller-`/call` inputs for matching (GVR, ns, name, BindingUID, perPage=-1, page=-1, extras={}). Diff via the existing `dispatch.cache_key.computed` diag (`helpers.go:349`). This is the airtight keying falsifier — if the hashes match, the hit is guaranteed.
- **F-RA3 (extras keying-risk, unit):** assert that a controller `/call` WITH `?extras=X` MISSES a cell seeded WITHOUT extras (proves §11.6 risk is real) and HITS a cell seeded WITH the same extras X. Documents the precondition.
- **F-RA4 (priority, kind -race):** sustained human `/call` load concurrent with the RA seed; assert human `/call` p50 within ±5% of seed-disabled control AND the engine `yieldTotal` rises (seed yielded). (`prewarm_engine.go:444`.)
- **F-RA5 (RBAC precondition, kind):** a CDC identity with NO `get` binding on the RA gets 403 at `checkDispatchRBAC` (`restactions.go:97`) — assert no L1 lookup occurs and no cell is keyable. Proves OQ-IDENTITY-3 is a hard gate.
- **F-RA6 (toggle-off, kind):** `PROACTIVE_RA_SEED_ENABLED=false` ⇒ the RA-beyond-nav set is never seeded; L1 contents byte-identical to control. `CACHE_ENABLED=false` ⇒ same via `PrewarmEnabled()=false`.
- **F-RA7 (scale, expvar, soft):** `cache.proactive_ra_seed.cells_resident` × avg-cell-bytes ≤ budget; self-limit halts seeding at `RESOLVED_CACHE_MAX_RESIDENT_BYTES`. Empirical per-cell cost derived on re-stood-up cluster.

## 11.10 OPEN QUESTIONS — what I need Diego to answer (GO/NO-GO ordered)

1. **[HARD GO/NO-GO] OQ-IDENTITY-1:** Does the composition-dynamic-controller present a **snowplow-issued JWT** (validating against snowplow's signing key, carrying a Username) to `/call`? Or does it use a raw SA token? If raw SA token → `jwtutil.Validate` rejects → 401 → feature is moot until the CDC is issued a snowplow JWT. **This gates everything.**
2. **[HARD] OQ-IDENTITY-2:** Does the CDC's identity have a `<username>-clientconfig` Secret in `AUTHN_NAMESPACE`? (Required for its live `/call` to authenticate — not for the seed key, but for the real call.)
3. **[HARD] OQ-IDENTITY-3:** Is the CDC's identity a subject of an RBAC binding granting `get` on the RESTAction CRs it calls? (No grant ⇒ 403 before L1 ⇒ no keyable cell. The CDC must be a known RBAC-granted cohort.)
4. **[SCOPING] OQ-SECONDARY-A (extras):** Does the CDC pass `?extras=<json>` on its RA `/call`? If yes, are they FIXED/known (seedable) or per-call-variable (un-seedable → only no-extras calls hit)?
5. **[SCOPING] OQ-SECONDARY-B (pagination):** Does the CDC paginate (`?perPage/?page`)? If un-paginated (expected), seed `-1/-1`.
6. **[SCOPING] OQ-SCOPE-C:** ONE known RESTAction, or ALL the CDC can reach? "All RBAC-reachable" is the `feedback_no_special_cases`-clean default (§11.4) and costs little more than the existing PIP seed; "one known RA" would need a name/identifier from outside cluster state, which is a special-case smell — prefer the RBAC-reachable-set design unless Diego wants to scope tighter.
7. **[SCOPING] OQ-FRESHNESS-D:** Is the freshness need first-call-only, or repeated? The existing L1 refresher already keeps RA cells fresh on CR UPDATE (dirty-mark → re-resolve, `refresher.go`) and TTL is the outer net — so repeated freshness is FREE if the cell stays resident. Confirm whether the CDC calls once (seed-once suffices) or repeatedly (existing refresh + TTL covers it).
