# F2 — Content-prewarm informer-serve wiring (Fix #130 F2)

Date: 2026-07-11
Author: cache architect
Repo state: main @ 8b5163a (1.7.5, has F1b)
Evidence: /tmp/f1b-deploy/boot-1.7.5-t0.log, /tmp/f1b-deploy/acceptance-summary-1.7.5.txt

> PATH NOTE (C-F2-5 correction): the bare filenames `internal_dispatch.go`,
> `apistage.go`, and `resolve.go` referenced throughout this doc live under
> **`internal/resolvers/restactions/api/`**, NOT `internal/handlers/dispatchers/`.
> Only the `phase1_*` files (`phase1_content_prewarm.go`, `phase1_walk.go`) and
> `deps.go` are under `internal/handlers/dispatchers/` / `internal/cache/`. The
> `phase1_content_prewarm.go` and `phase1_walk.go` line references are correct.

## TL;DR

The 3 residual benchapps live paged LISTs (~23s each) during the content-prewarm
window are caused by ONE missing line: `withContentPrewarmSAContext`
(internal/handlers/dispatchers/phase1_content_prewarm.go:210-224) does NOT attach
`cache.WithServeWatcher`, whereas the discovery-walk's `withPhase1SAContext`
(phase1_walk.go:1029) does. Without the watcher on ctx, the internal-dispatch
informer-serve branch (internal_dispatch.go:380) is never entered, so the
content pass's benchapps LIST always takes the live paged apiserver LIST — even
though the benchapps informer was synced+servable ~2.5 minutes earlier.
Fix = add the identical one line. LOC bound: 1 (+ comment).

## 1. TRACED root cause + dispatch chain

### The residuals (TRACED, boot-1.7.5-t0.log)
- content.prewarm.started 12:35:57.298 (data_sources:16), completed 12:37:30.192
  (warmed:16, elapsed_ms:92894). [lines 1129, 1299]
- 3 benchapps `internal_dispatch.paged_list.completed` at 12:36:23.792 (22456ms),
  12:36:53.666 (23735ms), 12:37:23.584 (23747ms), each pages:120 items:60000,
  namespace:"". [lines 1167, 1219, 1266] — all INSIDE the window.
- The discovery-walk had already served benchapps FROM the informer:
  `internal_dispatch.list.informer_served` 12:33:42.843, 28039908 bytes,
  "#121 1a — served the prewarm-walk LIST from the synced informer". [line 358]
- benchapps informer registered 12:33:37.810 (discovery.gvr_registered) and
  never broke afterward (no watch.broken / relist for benchapps in the window).
- NO `informer_dispatch.list_served` for benchapps in the content window — the
  content pass's benchapps LISTs go straight to the LIVE `internal_dispatch`
  path. serve_miss=0 for benchapps in the window (the F1b diagnostic branch is
  never entered because the watcher is absent — see §1.3).

### The call chain (TRACED, file:line)
1. `runContentPrewarmPass` (phase1_content_prewarm.go:237) builds the resolve
   ctx via `withContentPrewarmSAContext(ctx, saEP, saRC)` at :252.
2. `withContentPrewarmSAContext` (:210-224) sets: WithUserConfig, WithLogger,
   WithUserInfo(SA username), WithInternalEndpoint, WithInternalRESTConfig,
   `WithApistagePrewarm` (:221), `WithPrewarmIterSerial` (:222).
   It does **NOT** call `cache.WithServeWatcher`. This is the divergence.
3. `prewarmOneRESTAction` (:340) resolves each harvested RESTAction via
   `restactions.Resolve` under that ctx; the inner benchapps LIST call fans out
   to the dispatch chain in resolve.go.
4. Because `WithApistagePrewarm` is set, resolve.go takes the apistage branch
   (resolve.go:717-796): `apistageContentServe` (apistage.go:448).
5. The content cell for benchapps is COLD at boot (that is the whole point of the
   prewarm), so apistage takes the MISS branch (apistage.go:598-601) →
   `dispatchViaInformer(cache.WithApistageContentResolve(ctx), call)`.
6. `dispatchViaInformer` returns `dispatchedOK=false` for the content pass's
   benchapps LIST (empirically: no `informer_dispatch.list_served` appears; the
   live path is taken instead). apistage.go:602-604 returns `ok=false`.
7. resolve.go falls through past the apistage block (resolve.go:794-796 "ok=false")
   to `dispatchViaInternalRESTConfig` at resolve.go:849.
8. `dispatchViaInternalRESTConfig` (internal_dispatch.go:245) reaches the
   informer-serve branch at :380 — `if rw, haveRW := cache.ServeWatcherFromContext(ctx); haveRW`.
   Because the watcher was never attached (step 2), `haveRW==false` → the whole
   serve branch is skipped → the live paged LIST at :414-onwards runs (the
   `internal_dispatch.paged_list.completed` WARN we observe).

The `:210-221` attribution in the brief is CONFIRMED, with the precise correction
that the seam is the ABSENCE of `WithServeWatcher` (not `WithApistagePrewarm`,
which is correctly present). Note the brief's line number referenced the older
`internal/cache/phase1_content_prewarm.go` path; the real file is
`internal/handlers/dispatchers/phase1_content_prewarm.go`.

### 1.3 Why F1b can't help here (branch-not-entered)
F1b primes conjunct-4 typeConfirmed at the lazy-register seam so the serve branch,
WHEN ENTERED, does not miss on typeConfirmed=false. But on the content-prewarm
path the serve branch is never entered at all (ServeWatcherFromContext=false),
so there is nothing for F1b to fix. serve_miss=0 during the residual LISTs is the
signature of "branch not entered", NOT "branch entered and missed".

## 2. Why the same sweep runs 3× in one 95s window

NOT a 30s timer cadence and NOT one RA retried. It is THREE DISTINCT data-source
RESTActions each independently doing a full benchapps cluster LIST, run
back-to-back because the content pass is SERIAL (OOM mitigation 1,
phase1_content_prewarm.go:31-33 / :265 `for _, ref := range refs`).

Timing arithmetic (TRACED): each benchapps LIST takes ~23s; the deltas between
LIST *completions* are 12:36:23.79 → 12:36:53.67 (29.9s) → 12:37:23.58 (29.9s).
23s LIST + ~7s of interleaved small per-composition getComposition LISTs
(items:1, ~200ms each — the `allCompositionResources` .status.managed iteration,
lines 1167-1266 context) = ~30s spacing. The 30s is emergent from serial work,
not a schedule.

**Dedup ruling.** These are 3 different composition-widget RESTActions whose
resolve each contains a benchapps LIST step (the compositions-list / dashboard
composition-count / composition-resources family). The content harvester ALREADY
dedups by (ns,name) RESTAction identity (harvestApiRef, :154-173), so the 3 are
genuinely distinct RAs, not a harvester dedup miss. A deeper dedup — coalescing
identical inner (gvr,ns,name) LIST *calls* across different RAs within one pass —
is a SEPARATE optimization, NOT part of F2, and is MOOT once F2 lands: with the
serve-watcher attached, all 3 benchapps LISTs serve from the same synced informer
indexer in-memory (sub-ms each, ~28MB read), so the 3× cost collapses to
negligible regardless of dedup. Recommendation: do NOT add call-level dedup;
F2 removes the cost that would motivate it.

## 3. The fix

Add the serve-watcher to the content-prewarm SA context, identically to the
discovery walk. In `withContentPrewarmSAContext`
(internal/handlers/dispatchers/phase1_content_prewarm.go), after the existing
`cache.WithInternalRESTConfig(rctx, saRC)` at :220, add:

```go
    rctx = cache.WithServeWatcher(rctx, cache.Global()) // F2 (#130): serve inner LISTs from the synced informer, mirroring withPhase1SAContext:1029
```

LOC bound: 1 line + 1 comment. No new flag, no env, no special-case. It is the
BYTE-IDENTICAL mechanism the discovery walk already uses (phase1_walk.go:1029).
nil-safe: `WithServeWatcher` returns ctx unchanged when `cache.Global()` is nil
(CACHE_ENABLED=false), so flag-off behavior is preserved (phase1.go:668-672).

### Why this makes the symptom disappear
With the watcher attached, when the content pass's benchapps LIST falls through
to `dispatchViaInternalRESTConfig` (resolve.go:849), the serve branch at
internal_dispatch.go:380 now has `haveRW==true`, runs the bounded
`WaitForGVRSync` (returns immediately — benchapps is already synced) + the
IsServable conjunct gate, and serves via `ListServableEnvelopeJSON` (:386-394) —
exactly as it did for the discovery walk at 12:33:42. The live paged LIST
(:414+) is never reached. paged_list benchapps → 0.

### Prior-art check
client-go/informers already IS the substrate; the `ResourceWatcher` +
`ListServableEnvelopeJSON` serve path is snowplow's existing #121-1a mechanism.
This fix does not reinvent anything — it reuses the exact seam already proven on
the walk path. No new abstraction.

## 4. Ordering / reachability

- Step 7 `WaitAllInformersSynced` (phase1_walk.go:752) runs BEFORE the content
  pass `contentWarm(ctx)` (phase1_walk.go:762). So every registered informer,
  benchapps included, is synced when the content pass runs — the `WaitForGVRSync`
  in the serve branch returns immediately (common boot case, internal_dispatch.go:384).
  The boot log corroborates: benchapps served from informer at 12:33:42, ~2.5 min
  before the first residual at 12:36:23.
- The serve branch is a SUPERSET-safe change: on any miss (unsynced past the 5s
  bound, unregistered, watch-broken, not-servable) it falls through to the live
  LIST unchanged (internal_dispatch.go:411 "never worse"). So an
  informer-not-yet-synced GVR at content-pass time degrades to today's behavior,
  not worse.
- Note: `withContentPrewarmSAContext` rebuilds ctx from scratch via
  `xcontext.BuildContext` (:218), so the watcher cannot be inherited from the
  caller's cctx — it MUST be attached inside this function. This is why the fix
  belongs here and not at the call site.

## 5. Safety — identity semantics + content parity

The content pass runs under SA identity and writes IDENTITY-FREE content cells
(apistage BindingUID is "" — contentKeyInputs never sets it; apistage.go:680-681).
Serving the LIST from the informer instead of the apiserver must not change what
gets written, nor leak scope.

1. **Same read-set.** `ListServableEnvelopeJSON(gvr, namespace="")` returns the
   FULL informer set (no per-user narrowing) — internal_dispatch.go:374-378
   documents this is byte-parity with the SA cluster/namespace-wide LIST the live
   path issues. Both the live paged LIST and the informer serve return the same
   SA-maximal set for a cluster-wide LIST. The discovery walk already relies on
   this parity (it served benchapps from the informer at 12:33:42 and that fed the
   same downstream).
2. **No per-user gate on this path.** The content pass sets `WithApistagePrewarm`,
   so apistage.go:662-666 returns `served=false` after populating the cell and
   NEVER runs the per-user RBAC gate (there is no requester). The stored cell is
   SA-maximal un-gated content by design — F1's whole contract. The per-user
   narrowing happens later, at SERVE time, on the real user's /call
   (gateListItems / UAF refilter, apistage.go:674-697). Attaching the serve-watcher
   changes only the SOURCE of the un-gated bytes (informer vs apiserver), not the
   gating. No cross-user leak is introduced.
3. **Content byte-parity.** The informer-served envelope and the live paged-LIST
   envelope are the same {apiVersion, kind:<Resource>List, items[]} shape
   (internal_dispatch.go:340-345 documents the paged path rebuilds the identical
   marshal shape; ListServableEnvelopeJSON produces the canonical list envelope).
   The content cell stores whichever bytes the dispatch returns; both sources
   yield the same items set for a synced informer. This is the tester's CONTENT
   discipline gate (§6 falsifier d).
4. `WithServeWatcher` and `WithApistageContentResolve` are independent context
   keys (phase1.go:642 vs deps.go:198); attaching one does not perturb the other.

## 6. Falsifiers (PM gate)

- **(a) Hermetic — serve-watcher wiring.** A content-prewarm dispatch over a
  servable informer serves from the informer, not live. Construct: register a GVR
  in a fake ResourceWatcher, mark it synced+servable, run a LIST through the
  content-prewarm ctx (withContentPrewarmSAContext) → assert
  `internal_dispatch.list.informer_served` fires (served=true from
  ListServableEnvelopeJSON), NOT the live nri.List. RED arm = remove the
  `WithServeWatcher` line → the dispatch takes the live LIST (haveRW=false).
  This is the load-bearing mechanism proof.
- **(b) On-deploy HARD ZERO.** Across a full 50K boot: benchapps
  `internal_dispatch.paged_list.completed` count during the content.prewarm window
  (and anywhere post-sync) == 0. Pre-fix baseline = 3 (1.7.5). Grep the boot log
  for `paged_list.completed` with `Resource=benchapps` → must be empty.
- **(c) F1-row inherited condition — POST-prewarm user-path LIST served.** A
  benchapps LIST on the real per-user /call path AFTER prewarm completes is
  proven informer-served (`internal_dispatch.list.informer_served` or
  `informer_dispatch.list_served`), closing the F1-close evidence gap. (Mandatory
  per the F1 close.)
- **(d) Content parity.** The benchapps content cell written by the prewarm pass
  is byte-identical whether populated from the informer serve or the live LIST.
  Capture the cell (via /debug/apistage or the resolved bytes) under both a
  watcher-attached and watcher-absent build → assert equal items set + envelope
  shape. Tester CONTENT discipline.

## 7. Interaction with the BOOT-WINDOW follow-up row

The follow-up row ("3×23s live LISTs degrade boot-window UX") is made MOOT by
this fix: F2 eliminates all 3 live benchapps LISTs entirely (they become
sub-ms informer reads), so the 69s of live-LIST wall-clock inside the 92.9s
content window collapses. Expect content.prewarm elapsed_ms to drop
substantially (from ~92.9s toward the residual small-LIST + resolve cost). Close
that row on F2 landing, contingent on falsifier (b) HARD ZERO.

## 8. Reachability (the F1 lesson — no flag gate)

- The content pass is IMPLICIT-ON under CACHE_ENABLED (PrewarmContentEnabled →
  cache.PrewarmEnabled(), phase1_content_prewarm.go:111-113). It ran in the 1.7.5
  boot log (content.prewarm.completed, line 1299), so the deployed binary DOES
  reach `withContentPrewarmSAContext` → `runContentPrewarmPass` on every boot.
- The new line sits on that exact always-run path (withContentPrewarmSAContext
  :210, called unconditionally at runContentPrewarmPass:252). No flag gates it.
- Production call chain proving reach: main boot → phase1WarmupWith (Step 7.5,
  phase1_walk.go:761-762) → contentWarm(ctx) → runContentPrewarmPass:252 →
  withContentPrewarmSAContext:210 (new line) → prewarmOneRESTAction:340 →
  restactions.Resolve → resolve.go:849 dispatchViaInternalRESTConfig →
  internal_dispatch.go:380 serve branch (now haveRW=true).
- Falsifier (b)'s on-deploy HARD ZERO is the deployed-binary reach proof: if the
  benchapps paged_list count drops 3→0 on a real 50K boot, the deployed binary
  reached the new code.
