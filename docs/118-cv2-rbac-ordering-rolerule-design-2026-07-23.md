# #118 (c)-v2 — RBAC ordering + role-rule bump: durable UAF authz-staleness fix

Date: 2026-07-23
Author: cache-architect
Status: DESIGN (design-first; dual-gate then route to dev). No prod code in this doc.
Base: main @ `9e38dcd` (carries (c) v1 @ `a087702` + (d) @ `423d23b`).
Cluster context verified: `gke_operations-dev-krateo-io_europe-west4_krateo-installer-release` (west4 release, the canonical live target). No mutation performed — this is a code trace + design.

---

## 0. Headline (read this first)

(c) v1 folds a per-subject `RBACSubGen` counter into the resolved key so an RBAC change rotates the key → cold miss → fresh refilter. It is **soundness-incomplete** for two reasons TRACED to source:

- **GAP 1 — role/ClusterRole RULE edits do not rotate the key.** `onRoleRulesChanged` re-routes the seed index but never calls `BumpSubjectSubGens`. TRACED `internal/cache/bindings_by_gvr_delta.go:242-299` (no bump anywhere in the function) vs the binding hooks which DO bump (`:139,145,162,165,172,176,196,201`). A verb grant/revoke via a Role-rule edit leaves the subject's sub-gen unchanged → identical key → stale serve. This is Diego's live repro.

- **GAP 2 — bump-vs-snapshot-publish ordering race (the headline).** The bump is **synchronous** on the binding delta event (`bindings_by_gvr_delta.go:131→139`). The snapshot the refilter reads is rebuilt **async + debounced** (`rbac_snapshot.go:306 scheduleRBACRebuild` → detached goroutine → `rebuildRBACSnapshot:371` → `rbacSnap.Store:489`). In the window (bump landed, rebuild pending) a request derives the NEW key (bump visible via `RBACSubGenForSubject`, `rbac_subgen.go:96`) but the refilter runs `EvaluateRBAC` against the OLD snapshot (`evaluate.go:206 snap := rw.Snapshot()`). The pre-change verdict is cached **under the new key** and, because nothing re-bumps at publish, it **sticks until TTL** (served on hit at `evaluate.go:242` memo + the resolved-cell hit). This defeats the login hot path exactly: Kyverno generates the user's RoleBindings → the browser's first resolve races the rebuild → wrong result pinned under the right key.

**Chosen GAP-2 approach: (a) defer the per-subject bump to snapshot-publish**, accumulating the changed-subject set across the debounce and bumping ALL of them inside `rebuildRBACSnapshot` **after** `rbacSnap.Store`. This makes "a new key ⇒ a snapshot ≥ the change that rotated it" true by construction: the key does not rotate until the fresh snapshot the refilter will read is already live. It is per-subject (no global churn) and adds zero blast radius beyond (c) v1. Option (b) `rbacSnapshotPublishSeq`-in-key is **REJECTED** (churns every identity-bound key on every cluster-wide rebuild → 50K install-storm cache-cold, the exact failure that killed global `RBACGen`). Option (c) gate-the-Put is **REJECTED as primary** (adds a stale-seq field to the L1 Put contract at 7 sites and still needs the deferred set to know "latest bump seq"; strictly more surface than (a) for the same guarantee).

**Key-version: v5 → v6 REQUIRED.** Not because the fold shape changes (it doesn't — `RBACSubGen uint64` stays), but because deferring the bump changes the counter's *timeline*: a v5 cell was written under a value that (c) v1 bumped synchronously; v6 cells are written under the publish-deferred value. Across a rolling restart a v5 cell for a subject mid-transition could carry a sub-gen the v6 reader would compute differently for the same access → serve a v5 cell as a v6 hit and re-pin the very staleness we're fixing. A salt bump forces the clean break (same rationale as every prior v-bump at `resolved.go:420-466`). See §5.

**Un-briefed GAP 3 surfaced (see §6):** the seed does **not** stamp `RBACSubGen` at all — the ONLY stamp site in the tree is the dispatch path `helpers.go:265`. The `resolved.go:415` comment ("Stamped wherever a ResolvedKeyInputs is BUILT ... seedOneRestaction for the seed") is **stale/aspirational** — grep proves no seed site sets it. This is a seed↔dispatch key-divergence risk independent of GAP 1/2 and must be adjudicated in this cycle.

---

## 1. Prior-art check (opens every design)

- **client-go / k8s already solve "key rotates on RBAC change"?** No. client-go has no resolved-authz cache; `SubjectAccessReview` is the apiserver's per-call authorizer with no client-side generation to fold. The informer's `ResourceVersion` is per-object, not per-subject-effective-RBAC — folding it would be option (b)'s global-churn failure at object granularity (worse). There is no upstream "happens-after publish" barrier we can borrow; our `rbacSnap` atomic Store IS the publish primitive, and (a) hangs the bump off it. **No reinvention — (a) reuses the existing publish site.**
- **We already have the deferred-set pattern.** `scheduleRBACRebuild`'s dirty-flag debounce (`rbac_snapshot.go:307-345`) already coalesces bursts into one publish; (a) rides the SAME coalescing by accumulating the subject set in the same window. No new goroutine, no new lock discipline beyond one `sync.Mutex`-guarded set (or a `sync.Map` batch).

---

## 2. Root cause, TRACED

### GAP 1 (role-rule blind)
- `onRoleRulesChanged(roleKind, ns, name, rules)` — `bindings_by_gvr_delta.go:242-299`. It looks up `idx.byRole[rk]` (`:259`), snapshots the referencing binding ids (`:264-267`), and for each id **re-routes its GVR/wildcard bucket membership** (`:279-296`). It reads `idx.entries[id]` (`:269`) — which **carries `subjects []subjectKey`** (confirmed `bindings_by_gvr.go:123-126`, `entries[id].subjects`) — but **never calls `BumpSubjectSubGens`**. So the mechanism to move the key exists and the subjects are in hand, but the bump call is simply absent.
- Contrast the binding hooks: every one of `onBindingAdd/Update/Delete` bumps (`:139,145,162,165,172,176,196,201`). GAP 1 is a missing call, not a missing capability.
- Why it produces the symptom: `EvaluateRBAC`'s refilter verdict depends on the role's RULES (`evaluate.go` → `roleRefPermits` resolves `snap.ClusterRolesByName`/`RolesByNSName`). A rule edit changes what a bound subject can access, but `RBACSubGenForSubject` (`rbac_subgen.go:96`) sums only the subject counters, which didn't move → same key → the pre-edit resolved cell is served.

### GAP 2 (ordering race) — the mechanism, line by line
1. Binding event fires → `rbacSnapshotEventHandlers.AddFunc` (`rbac_snapshot.go:862`): calls `scheduleRBACRebuild(rw)` (async) THEN `onBindingAdd(obj)` (`:865`) which **synchronously** `BumpSubjectSubGens(subj)` (`bindings_by_gvr_delta.go:139`).
2. `scheduleRBACRebuild` (`rbac_snapshot.go:306`) flips dirty, tryLocks, spawns a detached goroutine that will eventually `rebuildRBACSnapshot` → `rbacSnap.Store(snap)` (`:489`). The debounce (`:339-345`) can delay this by one or more rebuild cycles under a burst.
3. **Window:** bump done, Store not yet. A request arrives:
   - key-build reads `RBACSubGenForSubject` (`helpers.go:265`) → sees the NEW (bumped) sub-gen → NEW resolved key → L1 cold miss → fresh resolve.
   - the fresh resolve's UAF stage calls `EvaluateRBAC` → `snap := rw.Snapshot()` (`evaluate.go:206`) → **OLD snapshot** (Store hasn't landed). Old snapshot ⇒ pre-change RBAC ⇒ pre-change filtered view.
   - the L2 authz memo stores that verdict keyed on `snap.PublishSeq` (old) at `evaluate.go:283`, AND the resolved cell is Put under the NEW resolved key.
4. When the snapshot finally publishes, `PublishSeq` bumps and the L2 memo shard swaps (self-heals the memo). **But nothing re-bumps `RBACSubGen`** → the resolved key does not rotate again → the wrong resolved cell written in step 3 keeps its key and is served on every subsequent hit until TTL. The L2 memo self-heals; the L1 resolved cell does not. That is the durable defect.
- Confirms the brief: the (c) v1 arch review's "self-heals on next resolve" was wrong. A cached wrong resolved cell is served on hit (`evaluate.go` is not even re-entered on an L1 resolved hit — the whole resolve is skipped).

---

## 3. Fix design

### 3.1 GAP 1 fix — bump subjects on role-rule change
**Target:** `internal/cache/bindings_by_gvr_delta.go`, inside `onRoleRulesChanged` (currently `:242-299`).

**What:** collect the union of `subjectKey`s across every binding entry in `idx.byRole[rk]` and hand them to the deferred-bump accumulator (per §3.2 — NOT a synchronous `BumpSubjectSubGens`, so GAP 1's fix is ordering-correct too). The subjects are already on `idx.entries[id].subjects`; the loop at `:268-297` already visits each `entry` (it currently discards it via `_ = entry` at `:297`). Build the union there.

**Which events route here (confirmed):** `onRoleObjectChanged` (`:215-232`) is called from ALL THREE of `rbacSnapshotEventHandlers`' Add/Update/Delete for the non-binding (ClusterRole/Role) GVRs (`rbac_snapshot.go:867,875,883`), and it dispatches to `onRoleRulesChanged` for both `ClusterRole` (`:224`) and `Role` (`:228`). So ADD/UPDATE/DELETE of a Role or ClusterRole all reach the fix. DELETE routes with the last-known rules (`:213-214` comment) — the union-of-affected-subjects is still correct (those subjects lost the grant; they must rotate).

**Per-subject scoping preserved:** the union is exactly the subjects of the bindings that reference the changed role — never global. Topology is ~1:1 role:binding from per-composition RBAC (Gate-2 worst case 4 referencing bindings, `:239-241`), so the union is tiny.

**LOC bound:** ~12 LOC (a `[]subjectKey` accumulator in the existing loop + one call to the batch-record helper). No new function beyond §3.2's shared accumulator.

**Edge — empty-rules / role-deleted:** the binding stays in `byRole`+`entries` (`:276-278`) so its subjects are still enumerable; we bump them (their effective access dropped to nothing → key MUST rotate). Correct.

### 3.2 GAP 2 fix — defer the bump to snapshot-publish (option a)

**New state (rbac_subgen.go or a small sibling):**
```
var pendingSubGenBumps struct { mu sync.Mutex; set map[subjectKey]struct{} }
```
- `recordPendingSubGenBumps(subjects []subjectKey)` — under `mu`, add each to `set`. Called from the binding hooks AND `onRoleRulesChanged` **instead of** the current synchronous `BumpSubjectSubGens`.
- `flushPendingSubGenBumps()` — under `mu`, drain `set` to a slice, clear the map, then (outside the lock) `BumpSubjectSubGens(drained)`.

**Wiring:** call `flushPendingSubGenBumps()` inside `rebuildRBACSnapshot` **immediately after** `rbacSnap.Store(snap)` (`rbac_snapshot.go:489`) — the publish barrier. Any request that observes the bumped sub-gen (new key) is guaranteed to observe a snapshot `Store`d at or after the change, because the bump becomes visible only after the Store.

**Debounce accumulation (the brief's watch-item):** the accumulator is a set keyed by `subjectKey`, populated on EVERY event (synchronous, cheap — a map insert under a short lock) and drained ONLY at publish. The `scheduleRBACRebuild` dirty-loop (`:339-345`) can coalesce N events into one `rebuildRBACSnapshot`; because every event recorded into the set BEFORE the loop's `rebuildRBACSnapshot` ran, the single flush after that Store bumps ALL accumulated subjects. An event that lands MID-rebuild re-flips dirty (`:340` clears dirty before rebuild, `:342` re-checks after) → another loop iteration → another Store → another flush that catches the mid-rebuild event's subject. No subject is lost, none is bumped before its snapshot is live. **Ordering invariant holds across the debounce.**

**Correctness of the happens-after:** `atomic.Pointer.Store` (`:489`) is a release; the reader's `rbacSnap.Load` (`evaluate.go:206`) is an acquire; the `atomic.Uint64.Add` bump (`rbac_subgen.go:77`) sequenced AFTER the Store on the same goroutine is visible to a reader only via a subsequent Load that also sees the new snapshot. A reader that sees the new sub-gen (new key) therefore cannot see a pre-Store snapshot. No window.

**Why not a stale-under-new-key residual:** in the deferred model the key does NOT rotate during the window at all — a request in the window derives the OLD key (sub-gen not yet bumped), hits the OLD resolved cell (correct pre-change view, which is what the current snapshot still authorizes), and the moment the new snapshot publishes + the bump flushes, the NEXT request derives the NEW key → cold miss → fresh resolve against the NEW snapshot. The transient (one snapshot-lag window, ≤ the AC-B.12 <1s propagation bound) serves the last-correct view, never a wrong-under-new-key view. This is strictly better than (c) v1.

**LOC bound:** ~35 LOC total (accumulator struct + 2 helpers ~25, swap 8 call-sites from `BumpSubjectSubGens`→`recordPendingSubGenBumps` ~8, one `flushPendingSubGenBumps()` line after the Store). Net: `BumpSubjectSubGens` stays exported (tests + the flush call it) but is no longer called directly by the hooks.

**Per-subject + no-churn proof:** the flushed set is exactly {subjects whose bindings/roles changed in this rebuild batch}. A 50K install storm creating tenant-X bindings flushes only tenant-X subjects per rebuild — identical blast radius to (c) v1, which the 50K analysis already blessed (`rbac_subgen.go:12-21`). Option (b)'s global `PublishSeq` fold would rotate EVERY identity-bound key on that same rebuild; (a) does not. The only added per-rebuild cost is one map drain of the changed set (O(changed subjects), already bounded by the events that fed it).

### 3.3 Interaction with (c) v1 (no regression)
- The `RBACSubGen` field, `ComputeKey` fold (`resolved.go:405-417,738-747`), `RBACSubGenForSubject` reader (`rbac_subgen.go:96`), and the group-grant sum (C-118-2) are **unchanged**. (a) only moves WHEN `BumpSubjectSubGens` fires (publish-time vs event-time). The grant-via-group / herd / parity falsifiers still pass — they assert key-rotates-on-effective-change, which still holds (now at publish rather than at event; the tests should assert AFTER driving a real publish — see §4).
- `#95` (re-derived `""`-BindingUID treated as MISS) untouched — orthogonal to the sub-gen term.
- Per-user keying (`feedback_l1_per_user_keyed_never_cohort`) untouched — RBACSubGen is per-subject-summed, still folded per identity.

---

## 4. THE MISSING FALSIFIER — real end-to-end behavioral arm (hard requirement)

Location: `internal/cache/` + an envtest/kind harness (NOT the hermetic unit that masked both gaps). Two arms, each must show RED on current code and GREEN on the fix. Both drive the REAL RBAC informers + REAL `EvaluateRBAC` refilter + REAL async `scheduleRBACRebuild` timing — no hand-installed snapshot, no hand-fed key (per `feedback_falsifier_must_drive_real_boundary_not_install_crossed_state` + `feedback_consultation_mutation_is_not_key_correctness`).

**Arm (i) — Role-rule edit rotates the key (GAP 1).**
- Setup: envtest apiserver; create Role R granting get/list on GVR G in ns N; RoleBinding binds user U (or group U-is-in) to R; build the bindings index + publish initial snapshot; warm an L1 resolved cell for U over G/N (UAF stage present) — capture key K1.
- Act: `kubectl`/client edit R's rules to REVOKE get on G (or grant a new verb). Wait for the informer UPDATE → `onRoleObjectChanged` → publish.
- Assert: the key U now derives for G/N ≠ K1 (sub-gen rotated), the next resolve is a cold miss, and the refilter reflects the new rules (revoked → empty/denied view).
- **RED on current code:** `onRoleRulesChanged` never bumps → derived key == K1 → warm hit → stale (pre-revoke) view served. GREEN after §3.1.

**Arm (ii) — bump→publish race, no stale-under-new-key (GAP 2).**
- Setup: as (i) but the target is an ADD (grant): user U starts with NO binding for G/N; warm/observe the pre-grant state.
- Act: create the RoleBinding granting U access to G/N. This fires the synchronous bump path (current) vs the deferred path (fix). **Interleave a request between the binding event and the snapshot publish** — the harness must inject a resolve in that window. Realize the window deterministically by pausing the rebuild goroutine (test seam: a `rebuildBarrier chan struct{}` gated in `rebuildRBACSnapshot` under a test-only flag, released by the test AFTER it fires the in-window request) so the interleave is not timing-flaky.
- Assert: the value SERVED to U after the dust settles is the POST-change (granted) view — i.e. no wrong verdict is pinned under any key.
- **RED on current code:** in-window request derives NEW key (synchronous bump visible), refilters against OLD snapshot (pre-grant), caches the pre-grant (empty) view under the NEW key; post-publish nothing re-rotates → U keeps seeing the empty view on hit → stale. GREEN after §3.2: in-window request derives OLD key (bump deferred), serves last-correct view; post-publish+flush the next request derives NEW key → fresh granted view. Assert the final served view is granted AND (stronger) that no cell exists carrying the pre-grant view under the post-grant key.

**Falsifier shape guards (per `feedback_falsifier_shape_must_discriminate` + `feedback_falsifier_must_actually_run_under_gate_tag_env`):**
- Both arms MUST show the `=== RUN` line and the RED must be observed by neutering the fix (revert the bump-call / revert the flush) — not merely asserted.
- Arm (ii) MUST use the real `scheduleRBACRebuild` path (barrier-gated), not a hand-published snapshot, or it is blind to the exact ordering it tests.
- Group arm: run arm (i) once with U bound via a GROUP (grant-via-group), to keep the C-118-2 crux exercised through the new publish-time path.

**Falsifier for GAP 3 (§6), if adjudicated in-scope:** a seed-vs-dispatch key-parity arm — seed a UAF cell for U, then dispatch the same coords as U, assert identical resolved key. RED today iff the seed omits `RBACSubGen` while dispatch stamps it AND U's sub-gen is non-zero.

---

## 5. Key-version decision: v5 → v6 (REQUIRED)

`resolvedKeyVersion` at `resolved.go:466`. Bump to `"v6"` with a comment block mirroring the existing v-history (`:430-465`).

**Why a bump is needed even though the field shape is unchanged:** the SEMANTICS of the folded `RBACSubGen` value change across the deploy. Pre-fix (v5) a cell was written with the synchronously-bumped counter; post-fix (v6) with the publish-deferred counter. For a subject mid-transition at the rolling-restart boundary, the same logical access can map to a different sub-gen under the two regimes → a v5 cell could be served as a v6 hit and re-pin the pre-change view (the exact staleness we fix). A salt rotation forces every pod to treat pre-v6 cells as non-hits → clean break, no cross-regime contamination. This is the identical rationale as v3→v4 and v4→v5 (`:444-465`). ~2 LOC (the const + comment).

Recommendation: **bump to v6.** Cost is one rolling-restart cold window (already paid on any deploy); the correctness upside is eliminating the cross-regime re-pin.

---

## 6. GAP 3 (un-briefed, surfaced) — seed does NOT stamp RBACSubGen

**TRACED:** the ONLY `RBACSubGen:` stamp in the entire tree is `internal/handlers/dispatchers/helpers.go:265` (dispatch Path-B). `grep -rn "RBACSubGen:" internal --include=*.go | grep -v _test.go` returns exactly that one line. Every other `ResolvedKeyInputs{}` builder either is an **identity-free class** (correctly exempt — `apistage.go:66`, `widget_content.go:88` "Username/Groups intentionally zero", `ra_full_list_slice.go:89` RAFullList) or is the **content-prewarm dep-edge context** (`phase1_content_prewarm.go:378`, class "restactions" but keyed on the RA's own coords, not an identity-bound resolved-view cell — no BindingUID either, so it is not a user-facing serve cell).

**Consequence:** the `resolved.go:415` comment claiming the seed stamps `RBACSubGen` via "seedOneRestaction" is **stale** — grep finds no `seedOneRestaction`/seed site stamping it. IF there exists an identity-bound seed Put that pre-warms a user-facing UAF cell (the #42 dedup-by-representative seed path), it would write that cell with `RBACSubGen == 0`, while the same user's dispatch derives a non-zero sub-gen after any grant/revoke → **seed cell is unreachable by the live browser** (the extras-xlen class from `project_seed_key_divergence_extras_buid`, but for the sub-gen term). This is a warm-miss (perf), not a leak (the dispatch key is still correct), but it silently defeats seeding for any subject whose sub-gen ever moved.

**Recommendation (strategic — surface to TL/Diego):** two options, I recommend **B**:
- **A — fix in this cycle:** locate the identity-bound seed Put and stamp `RBACSubGen: cache.RBACSubGenForSubject(repUser, repGroups)` from the representative identity, add the §4 GAP-3 parity arm. Keeps seed reachable. +~4 LOC + 1 arm. Risk: expands scope.
- **B — de-scope + correct the comment now, ticket the seed stamp separately:** (c)-v2's GAP 1/2 are the reopened-#118 blockers; GAP 3 is a pre-existing seed-reachability perf gap, not a correctness/authz-staleness bug (dispatch key stays correct). Fix the stale `resolved.go:415` comment in this PR (1 LOC, truth-in-source) and file the seed stamp as a #42-class follow-up. **Recommended** — keeps the security fix tight; the seed gap is orthogonal and lower-severity.

This is a strategic call (scope of the security cut) → for the TL / Diego, not decided here.

---

## 7. Options-with-recommendation summary

| Decision | Options | Recommendation |
|---|---|---|
| GAP-2 ordering | (a) defer bump to publish / (b) fold PublishSeq in key / (c) gate the Put | **(a)** — per-subject, no-churn, closes the window by construction; (b) 50K-fatal, (c) more surface for same guarantee |
| Key version | keep v5 / bump v6 | **v6** — cross-regime re-pin prevention, ~2 LOC |
| GAP 3 (seed stamp) | A fix now / B de-scope + fix comment + ticket | **B** — keep the security cut tight; seed gap is orthogonal perf |

---

## 8. Falsifier (the specific proof this fix works, or didn't)

- **GAP 1:** envtest arm (i) — edit a Role's rules; assert the subject's derived key changes AND the refilter reflects the new rules. RED = key unchanged / stale view (current). GREEN = key rotates / fresh view.
- **GAP 2:** envtest arm (ii) barrier-gated — inject a resolve in the bump→publish window; assert the FINAL served view is post-change AND no cell carries a pre-change view under the post-change key. RED = pre-change view pinned under new key (current). GREEN = last-correct view served in-window, fresh view after publish.
- **No-churn / 50K:** run the (c) v1 herd arm through the new publish-time path; assert only the changed cohort's sub-gens moved after a scoped RBAC event (unchanged users' keys stable). Optional on-cluster: at 50K, a single-tenant binding create bumps only that tenant's subjects (expvar/log of flushed-set size ≈ tenant subjects, not cluster-wide).
- **Runtime artifact to watch on deploy:** on a live grant to user U, `rbac.evaluate` DEBUG for U's next resolve shows `path=in-process` (cold, not memo-hit) with the post-grant verdict, and `cache.rbac.snapshot.published` precedes the first post-grant resolve. The stale-serve disappears iff U's first post-change page-load reflects the change without a pod restart.

---

## 9. Files touched (design targets, all TRACED)

- `internal/cache/bindings_by_gvr_delta.go` — GAP 1: union-of-subjects in `onRoleRulesChanged` (~12 LOC), swap `BumpSubjectSubGens`→`recordPendingSubGenBumps` at the 8 binding-hook sites (~8 LOC).
- `internal/cache/rbac_subgen.go` (or sibling `rbac_subgen_pending.go`) — GAP 2: pending-bump accumulator + record/flush helpers (~25 LOC).
- `internal/cache/rbac_snapshot.go` — GAP 2 wiring: `flushPendingSubGenBumps()` after `rbacSnap.Store(snap)` at `:489` (~1 LOC), + optional test-only `rebuildBarrier` seam for arm (ii).
- `internal/cache/resolved.go` — v5→v6 at `:466` (~2 LOC); GAP-3 comment correction at `:415` if option B (~1 LOC).
- `internal/cache/rbac_subgen_race_test.go` (new, envtest/kind-tagged) — arms (i), (ii), group arm, no-churn arm.

Total prod LOC: ~50 (well within a tight security cut). Test LOC: the envtest harness dominates.
