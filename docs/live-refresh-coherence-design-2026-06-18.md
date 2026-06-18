# Snowplow per-key live-refresh-coherence — file:line implementation design

**Date:** 2026-06-18
**Author:** cache architect (snowplow)
**Status:** design / pre-PM-gate. NO code changes; design only.
**Repo:** `~/krateo/snowplow-cache/snowplow` (branch `main`, tip `9bd24c7`; read-only checkout).
**Embraces:** `/Users/diegobraga/.claude/plans/snowplow-live-refresh-coherence-plan.md` + `…-proposal.md`.
**Source-map note:** This `main` checkout is the re-aligned canonical line (`project_release_branch_topology`); the plan's anchors were written against it and are accurate — verified below. kubectl current-context at trace time was `gke_integration-test-431120_europe-west1-b_krateo-bell` (NOT canonical) — irrelevant to this design (source-only), but any falsifier run MUST pass `--context gke_neon-481711_us-central1-a_cluster-1`.

The MECHANISM/root-cause (independent eventrouter-SSE vs snowplow-informer pipelines + stale-while-revalidate) is taken as TRACED per the brief and not re-derived. This document is the buildable fix.

---

## 0. Prior-art check (`feedback_check_k8s_clientgo_prior_art`)

| Need | client-go / stdlib prior art | Verdict |
|---|---|---|
| In-process fan-out hub | `k8s.io/apimachinery/pkg/watch.Broadcaster` (`mux.go:43`, `NewBroadcaster(queueLength, fullChannelBehavior)` :68; `FullChannelBehavior=DropIfChannelFull`) — TRACED at `…/apimachinery@v0.31.0/pkg/watch/mux.go`. | **Borrow the pattern, not the type.** `Broadcaster` fans EVERY event to EVERY watcher with NO per-key/per-subject routing (`mux.go` `loop()`/`distribute`). Using it as-is would push every refreshing l1Key to every connection → leaks the cluster-wide churn set to all 1000 users and wastes fan-out. We need **per-key-set subscription routing + per-subject validation**, which `Broadcaster` does not provide. We copy its proven primitives: per-watcher buffered channel + drop-on-full (so one slow SSE consumer never blocks the refresher). |
| SSE over `net/http` | `http.NewResponseController(w).SetWriteDeadline(time.Time{})` (Go 1.20+; we are on go 1.25.3 per `go.mod`) defeats the server's global `WriteTimeout` per-connection. `http.Flusher`/`ResponseController.Flush()` for frame flushing. Standard stdlib pattern. | **Adopt.** No third-party SSE lib. (Confirmed: golang/go #54136, MDN ResponseController.) |
| `EventSource` auth without headers | `EventSource` cannot set `Authorization`; the ONLY native credential channel is cookies via `withCredentials:true` (MDN `EventSource.withCredentials`). Token-in-URL is rejected by the brief (leaks in logs/referrer). | **Cookie-borne JWT.** See §4. |

**No existing `/refreshes`, broadcaster, or `Subscribe`/`PublishRefresh` symbol in the tree** (grepped `internal/**`, excluding worktrees/_test) — this is net-new.

---

## 1. Anchor verification (TRACED vs INFERRED, your numbers vs source)

### 1.1 Emit point — **your anchor is DIRECTIONALLY right but the precise seam is one layer deeper. CORRECTION below.** (TRACED)

The refresher call chain, end to end:

- `processNext` (`internal/cache/refresher.go:651`) dequeues a key, applies the rate-floor, then calls `processOne` at **`refresher.go:755`**.
- `processOne` (`refresher.go:804`) looks up the registered `RefreshFunc` by class (`refresher.go:820-826`) and invokes it at **`refresher.go:827`**: `if err := fn(ctx, key, *entry.Inputs); err != nil { … }`. Success falls through to `return nil` at **`refresher.go:837`**.
- The registered `fn` is the closure in `RegisterRefreshHandlers` (`internal/handlers/dispatchers/dispatchers.go:79-87`), which delegates to `resolveAndPopulateL1` (`dispatchers.go:86`).
- `resolveAndPopulateL1` (`internal/handlers/dispatchers/resolve_populate.go:92`) recomputes `key := cache.ComputeKey(inputs)` (`resolve_populate.go:101`) and **commits the L1 write at `c.Put(key, entry)` — `resolve_populate.go:291`.**

**Your claim that `SetRefreshHook` is the WRONG seam — CONFIRMED (TRACED).** `DepTracker.SetRefreshHook` (`internal/cache/deps.go:407`, wired from `refresher.go:400`) installs `enqueueFn`, which `onChange` (`deps.go:524`) calls at **`deps.go:539`** — at **dirty-mark time, pre-resolve**. It only enqueues into the workqueue; nothing is in L1 yet. Emitting there would announce a refresh that has not happened. Correct to reject it.

**CORRECTION to "emit in `processOne` after `fn` returns".** Emitting at `refresher.go:836-837` (after `fn` returns nil) is *almost* right but **over-fires**, because `resolveAndPopulateL1` has FOUR success-returns (`return nil`, which `processOne` reads as success) that DO NOT Put:
- `resolve_populate.go:98` — cache nil (cache-off).
- `resolve_populate.go:217` — `encoded == nil`, handler declined.
- `resolve_populate.go:228` — entry evicted during refresh; **deliberately not resurrected**.
- `resolve_populate.go:259` — stage-error decline; **prior good entry intentionally kept** (`feedback_validate_content_not_just_status` lineage).

Emitting in `processOne` would fire a "refreshed" signal on all four — triggering a frontend refetch when L1 did NOT change (and, in the evicted/declined cases, the refetch would read a stale or absent entry). That violates "coherent **by construction** — the signal exists only after the fresh entry is in L1" (proposal §4) and adds needless fan-out.

> **EXACT EMIT LINE (the deliverable):** immediately **after** `c.Put(key, entry)` at `internal/handlers/dispatchers/resolve_populate.go:291`, on the refresher path only, insert one call:
> ```go
> c.Put(key, entry)
> cache.PublishRefresh(key)   // ← new line, post-commit, only on actual-Put paths
> ```
> This is strictly post-commit (the Put returned), and it is reached ONLY when L1 actually changed. It is in `internal/handlers/dispatchers`, which already imports `internal/cache` (no cycle — `internal/cache` does NOT import `internal/handlers`; verified by grep). `PublishRefresh` lives in the broadcaster in `internal/cache` (§2).

**One subtlety to honour, NOT a blocker:** `resolveAndPopulateL1` is ALSO the shared target of the **prewarm/F2 walker** and the **cold dispatch** paths? — **No.** Verified: `resolveAndPopulateL1` is invoked ONLY from the refresher closure (`dispatchers.go:86`); grep for `resolveAndPopulateL1(` shows the definition (`resolve_populate.go:92`) + the single call site in the closure. Cold dispatch Puts via `dispatchCacheLookupKey`→handler→`writeResolvedJSON` and a separate Put; prewarm Puts via the walker. So emitting at `resolve_populate.go:291` fires on **refresher commits only** — exactly the dep-change-driven re-resolve we want to announce, NOT cold first-resolves or boot prewarm. This is the cleanest possible seam: surgically the refresher's post-commit, reached only on a genuine refresh that changed L1. (If a future change makes `resolveAndPopulateL1` shared with prewarm, the emit would need a `from-refresher` guard; today it is not shared.)

### 1.1a — WHEN does the post-commit fire? Two on-by-default gates sit IN FRONT of the re-resolve (TRACED; Phase-0 tester corroborated)

The proposal/plan assumed the refresher commit is "sub-ms–ms" so the signal closes the gap to ~milliseconds. **The raw commit IS sub-ms** (the Phase-0 tester measured 56–86µs for the Put itself) — **but two gates between dequeue and the re-resolve delay the commit deterministically on EXACTLY our scenario** (a live-refetch hits a *young* dirty entry that was just dirty-marked by the same reconcile):

1. **Per-key re-resolve RATE-FLOOR, default 2s** — `defaultRefresherRateFloorSeconds int64 = 2` (`refresher.go:105`; env `RESOLVED_CACHE_REFRESHER_RATE_FLOOR_SECONDS`, **unset in-tree → 2s in prod**). In `processNext` (`refresher.go:729-752`): if the dequeued entry's `time.Since(entry.CreatedAt) < floor`, the re-resolve is DEFERRED via `q.AddAfter(key, floor-elapsed)` (`refresher.go:749`) and `processOne` does NOT run this cycle. A composition reconcile dirty-marks the dependent cell; a live-refetch arriving < 2s later finds a young entry → the floor fires → the commit (and thus our emit) is deferred to ~floor-expiry. This is Task #321 / #318-R1a (within-wave CRUD-burst collapse). **FAIL-OPEN nuance (TRACED, `refresher.go:743-747`):** `CreatedAt` is stamped only by a successful `Put` (`resolved.go:771-772`); a cell that declines to cache re-resolves every dequeue. So the floor only delays cells that recently committed — i.e. exactly the live-refresh-after-reconcile case.
2. **Customer-priority cooperative YIELD, cap 5s** — `refresherYieldMaxParked = 5 * time.Second` (`refresher.go:124`, Ship #98), applied by `yieldToCustomer` (`refresher.go:580`, called at `refresher.go:694` BEFORE the handler). Under sustained customer `/call` the refresher worker parks (25ms poll, `refresher.go:113`) up to 5s before proceeding.

The docstring at **`refresher.go:97-99`** states the composed worst case verbatim: **"yield 5s + resolve ≤3s + floor 2s = 10s"** convergence SLA.

**Corrected freshness claim (do NOT overstate — this is the honest framing):** the emit at `resolve_populate.go:291` fires at the **post-floor (and post-yield) commit**, so under default config the signal arrives **~2s after the change at rest, ~5s under sustained `/call`, ~10s pathological worst case** — NOT "milliseconds." **What the design buys is not a smaller number but COHERENCE:** today the refetch is *silent-stale until the frontend's blind 5s throttle re-fires* (proposal §2, probabilistic); after this change the client is *signalled at exactly the moment L1 is fresh* — it learns precisely when to refetch and the refetch is a guaranteed HIT. We close a **deterministic ~2s/5s SILENT-stale window** to a **coherent, signalled ~2s** (the floor latency is shared with today; we just make the freshness observable instead of guessed). The Phase-0 tester confirmed the emit POINT (refresher post-commit, never the pre-resolve `SetRefreshHook`) is correct; the floor/yield gates change only the latency narrative, not the seam.

**This surfaces a strategic scope trade (options A/B below, §10) — do NOT pick here.** Either accept the ~2s post-floor latency (option A, respects #318-R1a burst-collapse) or lower/bypass the floor for signal-subscribed keys (option B, sub-second, but trades against the floor's reason-for-existing — CRUD-burst collapse / refresher amplification, a regression class that has bitten before: `feedback_refresher_populate_amplification`, the 0.30.185 5.9× wall-clock revert).

### 1.2 Subscription key = the L1 key. (TRACED)
`ComputeKey(ResolvedKeyInputs)` (`internal/cache/resolved.go:608`) is a SHA-256 over `version ∥ class ∥ GVR ∥ ns ∥ name ∥ BindingUID ∥ perPage ∥ page ∥ stage ∥ extras`. The identity fold is **`in.BindingUID`** at `resolved.go:653`, written for every class **except `widgetContent`** (the `if in.CacheEntryClass != CacheEntryClassWidgetContent` guard, `resolved.go:652`). So:
- For `restactions`/`widgets`/`apistage`/`raFullList`, the key is **identity-bearing** (folds the binding that authorised the requester). TRACED.
- For `widgetContent` the key is **identity-FREE** — shared across all users (`resolved.go:644,652`; the per-user `allowed` flag is re-derived at serve time per `widgets.go:144`). **This is the one class that is NOT per-subject at the key level** — see §5 for how isolation still holds.

### 1.3 Subject = `xcontext.UserInfo`. (TRACED)
`internal/handlers/dispatchers/helpers.go:96/205/327` all read `xcontext.UserInfo(ctx)` → `jwtutil.UserInfo{Username, Groups}`. `xcontext` = `github.com/krateoplatformops/plumbing/context` (`context.go:59`), value placed by `WithUserInfo` (`context.go:101`). TRACED.

### 1.4 Mux. (TRACED)
`main.go:777-808` registers `/swagger /health /readyz /api-info/names /list /call` (+ write-verb `/call`, `/jq`, `/debug/*`, expvar). No stream endpoint. Base chain = `use.NewChain(use.TraceId(), use.Logger(log))` (`main.go:157-160`); auth (`middleware.UserConfig(*signKey, *authnNS)`) is **appended per-route** (`main.go:793,804,…`). `/refreshes` is net-new.

---

## 2. Broadcaster — `internal/cache/refresh_broadcaster.go` (new file, ~120 LOC)

A purpose-built per-key fan-out hub. Borrows `watch.Broadcaster`'s drop-on-slow-consumer discipline; adds per-key subscription routing.

### 2.1 Types & API (the `internal/cache` surface)
```go
// refresh_broadcaster.go (new)
package cache

// subscriber is one SSE connection's sink. chans are buffered; a full
// channel DROPS (coalesce-by-design — the frontend refetches on the next
// signal, and a refetch is idempotent, so a dropped duplicate is harmless).
type refreshSub struct {
    id    uint64
    keys  map[string]struct{} // l1Keys this connection is armed for
    ch    chan string         // buffered (cap ~64); carries l1Key
}

type refreshBroadcaster struct {
    mu   sync.RWMutex
    subs map[uint64]*refreshSub
    next uint64
    // coalesce: per-key last-emit timestamp (monotonic), for the
    // churn-collapse window (§2.3).
    coalesceWindow time.Duration
    lastEmit       map[string]time.Time
    cmu            sync.Mutex
}

// package-singleton, lazily built; nil-safe so cache-off is a no-op.
func refreshHub() *refreshBroadcaster { … }   // returns nil when Disabled()

// PublishRefresh announces that l1Key was just committed to L1. Non-
// blocking: fans out to every subscriber armed for l1Key; a full sink is
// dropped (counter bump). No-op when cache is disabled or no hub exists.
func PublishRefresh(l1Key string) { … }       // called from resolve_populate.go:291

// SubscribeRefresh registers a connection. Returns the sink channel + an
// unsubscribe func. armedKeys is the validated key-set (§5).
func SubscribeRefresh(armedKeys map[string]struct{}) (<-chan string, func()) { … }

// ArmKey / DisarmKey let a live connection add/remove a key without
// reconnecting (the frontend mounts/unmounts widgets over one stream).
func (s *refreshSub) ArmKey(l1Key string)  { … }
func (s *refreshSub) DisarmKey(l1Key string){ … }
```

### 2.2 `PublishRefresh` fan-out (non-blocking, drop-on-full)
```go
func PublishRefresh(l1Key string) {
    if Disabled() { return }          // transparent-fallback no-op
    h := refreshHub()
    if h == nil { return }
    if h.coalesced(l1Key) { return }  // §2.3 churn-collapse
    h.mu.RLock()
    for _, s := range h.subs {
        if _, armed := s.keys[l1Key]; !armed { continue }
        select {
        case s.ch <- l1Key:
        default:
            refreshDroppedTotal.Add(1)  // expvar; slow consumer, coalesced away
        }
    }
    h.mu.RUnlock()
}
```
Mirrors `watch.Broadcaster`'s per-watcher `DropIfChannelFull` (`mux.go:56-62`): a slow SSE consumer never stalls the refresher goroutine (the `default:` arm). Drop is safe — the next committed refresh for the same key re-signals, and the frontend's refetch is idempotent.

**Concurrency:** `PublishRefresh` is called from the refresher worker goroutine(s) (post-Put, off the customer request path). The `sync.RWMutex` read-lock is held only for the fan-out loop (no I/O inside). Subscribe/unsubscribe take the write-lock. `-race` test required (`feedback_shared_vs_copy_is_a_concurrency_change`).

### 2.3 Coalesce per key (proposal §5.5)
`coalesced(l1Key)` consults `lastEmit[l1Key]` under `cmu`; if the last emit for this key was within `coalesceWindow` (default ~250ms, env-tunable, see §7), return `true` (skip — collapse the burst to one signal). Under heavy churn (a composition reconcile that re-resolves a LIST cell repeatedly) this caps fan-out to ≤1 signal / window / key. The frontend ALSO throttles per widget (proposal §6, `liveRefresh.ts` leading+trailing 5s) — server coalescing is the cheaper first line.

### 2.4 Cache-off (`project_cache_off_is_transparent_fallback`)
`refreshHub()` returns nil under `Disabled()`; `PublishRefresh` no-ops; `SubscribeRefresh` returns a closed/empty channel. The refresher does not run when cache is off (no `ResolvedCache()`), so `resolve_populate.go:96-99` returns before the Put, so the emit line is never reached either. `/refreshes` (§3) detects cache-off and serves a clean idle stream that emits only heartbeats. **Verified-removable.**

---

## 3. `/refreshes` SSE endpoint — `internal/handlers/refreshes.go` (new, ~140 LOC) + `main.go` wiring

### 3.1 Mux wiring (`main.go`, alongside `:803-808`)
```go
mux.Handle("GET /refreshes", chain.Append(
    middleware.RefreshAuth(*signKey),                 // §4 — cookie-or-header → UserInfo
    cache.FallthroughScopeMiddleware(cache.ScopeCallGeneric), // optional; benign
).Then(handlers.Refreshes(*signKey)))
// NOTE: NOT cache.RegisterScopedRoute — /refreshes issues zero apiserver
// reads, so it is out of the read-path-scoped invariant (AssertReadPathsScoped,
// main.go:914). Confirm with PM whether to register a no-op scope for symmetry.
```
`chain` is the base `use.Chain` (TraceId+Logger). `Append` returns a new chain (`use/chain.go:84`); `Then` terminates (`use/chain.go:45`).

### 3.2 Handler shape (`handlers.Refreshes`)
```go
func Refreshes(signingKey string) http.HandlerFunc {
  return func(w http.ResponseWriter, r *http.Request) {
    // 1. cache-off → clean idle stream (transparent fallback).
    if cache.Disabled() { serveIdleSSE(w, r); return }

    // 2. identity already on ctx (RefreshAuth middleware put UserInfo).
    ui, err := xcontext.UserInfo(r.Context())
    if err != nil { response.Unauthorized(w, err); return }   // defence in depth

    // 3. validate the requested key-set against THIS subject (§5).
    armed := validateSubscription(r, ui)   // map[string]struct{}; empty ⇒ 400
    if len(armed) == 0 { http.Error(w, "no valid subscription keys", 400); return }

    // 4. SSE headers + defeat the 300s WriteTimeout for THIS connection.
    rc := http.NewResponseController(w)
    _ = rc.SetWriteDeadline(time.Time{})         // ← the WriteTimeout fix (§6)
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    w.Header().Set("X-Accel-Buffering", "no")    // disable proxy buffering
    w.WriteHeader(http.StatusOK)
    rc.Flush()

    // 5. subscribe + stream until client disconnect.
    ch, unsub := cache.SubscribeRefresh(armed)
    defer unsub()
    heartbeat := time.NewTicker(20 * time.Second)
    defer heartbeat.Stop()
    for {
      select {
      case <-r.Context().Done(): return          // client gone
      case k := <-ch:
        fmt.Fprintf(w, "event: refresh\ndata: %s\n\n", k)
        rc.Flush()
      case <-heartbeat.C:
        fmt.Fprint(w, ": keepalive\n\n")          // SSE comment frame
        rc.Flush()
      }
    }
  }
}
```
**Signal-only, no payload** (proposal §5.3): `data:` carries just the l1Key. The frontend refetches `/call` → reads the freshly-committed L1 as a HIT. Heartbeat (SSE comment `:`) keeps intermediaries from idling the connection and lets the client detect a dead link.

### 3.3 Request chain (one round)
```
browser EventSource(/refreshes?keys=…, {withCredentials:true})
  → gateway → snowplow mux GET /refreshes
  → use.CORS (main.go:889; see §4 origin caveat)
  → base chain (TraceId, Logger)
  → middleware.RefreshAuth (cookie|header JWT → WithUserInfo)   ← NEW
  → handlers.Refreshes: validate key-set vs UserInfo → SubscribeRefresh → stream
```

---

## 4. AUTH — the make-or-break. **VERDICT: viable, NOT a hard blocker, but requires a new cookie-aware auth shim + a CORS-origin decision.** (TRACED)

### 4.1 How `UserInfo` is populated TODAY (TRACED, the make-or-break trace)
The `/call` (and `/list`) auth middleware is `middleware.UserConfig(signKey, authnNS)` in **`internal/handlers/middleware/userconfig.go:130`**. The flow:
1. `req.Header.Get("Authorization")` — **header-only**; missing ⇒ 401 (`userconfig.go:134-138`).
2. `strings.SplitN(authHeader, " ", 2)`, require `bearer` prefix (`userconfig.go:141-145`).
3. **`userInfo, err := jwtutil.Validate(signingKey, parts[1])`** (`userconfig.go:151`).
4. `xcontext.WithUserInfo(userInfo)` onto ctx (`userconfig.go:231`).

So today `UserInfo` comes **strictly from an `Authorization: Bearer <jwt>` header.** `EventSource` cannot set that header → mounting `/refreshes` behind `middleware.UserConfig` as-is would 401 every connection at `userconfig.go:135`. **That is the blocker the brief warned about — and it is real for the unmodified middleware.**

### 4.2 Why it is NOT a HARD blocker (the decisive finding)
`jwtutil.Validate` (`…/plumbing@v0.9.3/jwtutil/validate.go:16`) is a **pure, stateless HS256 verification**: `jwt.ParseWithClaims(bearer, &KrateoClaims{}, …signingKey…)` → returns `claims.UserInfo`. The `UserInfo{Username, Groups}` is **embedded in the token** (`jwtutil/create.go:16-24` — `KrateoClaims` embeds `UserInfo`). **There is NO session store, NO server-side lookup.** The token is self-contained and signature-verified against `signKey` (the same `*signKey` flag, `main.go:82`, `JWT_SIGN_KEY` / `AUTHN_JWT_SECRET`).

**Consequence:** the identical `UserInfo` is recoverable from the SAME JWT regardless of transport. If the SPA delivers that JWT in a **cookie** (which `EventSource` sends automatically with `withCredentials:true`), a `/refreshes`-specific middleware can extract it from the cookie and call the byte-identical `jwtutil.Validate(signingKey, cookieToken)` → byte-identical `UserInfo`. No new token type, no new validation path, no session infrastructure.

### 4.3 Design — `middleware.RefreshAuth(signingKey)` (new, in `internal/handlers/middleware/refreshauth.go`, ~40 LOC)
A SIBLING of `UserConfig`, NOT a replacement (do not perturb the audited `UserConfig` mirror — `feedback_claim_vs_code_identity_at_diff_review`; it is a verbatim upstream transcription with a drift-pin test). `RefreshAuth`:
1. Token source order: **(a) `Authorization: Bearer` header** (so non-browser/polyfill clients and curl falsifiers work) → **(b) a cookie** (the `EventSource` path). Cookie name = a config constant (e.g. `krateo-session` — confirm the portal's actual cookie name with the frontend owner; that is a cross-team fact, NOT source-recoverable here → OPEN).
2. `jwtutil.Validate(signingKey, token)` — **the identical call** as `userconfig.go:151`.
3. `xcontext.WithUserInfo(userInfo)` (`context.go:101`) — identical to `userconfig.go:231`.
4. **Deliberately SKIP the `<user>-clientconfig` Secret/`WithUserConfig` lookup** that `UserConfig` does (`userconfig.go:169-232`). `/refreshes` issues **zero apiserver reads** — it never resolves, so it needs no `UserConfig` endpoint. This keeps `/refreshes` off the F-3 Secret-GET path entirely (no fall-through counter, no apiserver hit). **`UserInfo` alone is sufficient for §5 validation.** TRACED: §5's `validateSubscription` needs only `ui.Username`+`ui.Groups` (it calls `rbac.EvaluateRBAC` with those).

**No token-in-URL.** The query string carries only resource-coordinates + the claimed key (§5/§6), never the JWT.

### 4.4 CORS — a deployment-topology decision you MUST make (TRACED finding)
Current CORS (`main.go:889-902`, via `plumbing/server/use/cors`): `AllowedOrigins:["*"]` + `AllowCredentials:true`. Traced in `cors.go:256-271`: under the `*` config (`allowedOriginsAll`), the handler emits **literal `Access-Control-Allow-Origin: *` together with `Access-Control-Allow-Credentials: true`**. The web platform **forbids** this pair for credentialed requests — a browser will reject a `withCredentials:true` EventSource against `*` (MDN; `Access-Control-Allow-Credentials`). **So cross-origin cookie-SSE does NOT work under today's CORS.**

Two resolutions (strategic — surface to Diego/team lead):
- **(I) Same-origin deployment (preferred if true).** If the portal SPA and snowplow `/refreshes` are served under the SAME origin (same gateway host), the CORS-credential restriction does not apply (no CORS preflight for same-origin), and the cookie flows. **Verify the deployed topology** — this is a `reference_portal_chart` / gateway fact, NOT snowplow-source-recoverable → OPEN. Likely the case (portal is behind the same gateway), which would make this a non-issue.
- **(II) Cross-origin.** Change `AllowedOrigins` from `["*"]` to the explicit portal origin(s) so `use.CORS` echoes the specific origin (`cors.go:259`) alongside credentials — the legal combination. This edits `main.go:890` (snowplow code) but the origin list is config-shaped; it should flow via a flag/env, NOT a hardcoded literal (`feedback_no_special_cases`). This is a small additive change, but it is a **behaviour change to the shared CORS for ALL routes** → needs PM sign-off and a check that the `*`-using clients today (if any non-browser) are unaffected.

**Auth verdict, one line:** *Cookie-borne JWT is the clean path; `jwtutil.Validate` is stateless so `UserInfo` is transport-independent; the only real work is a ~40-LOC `RefreshAuth` sibling middleware + resolving the CORS `*`-vs-credentials topology question (I) or (II). NOT a blocker.*

---

## 5. Per-subject isolation — re-derivation, NOT a provenance map (the load-bearing design; `feedback_l1_per_user_keyed_never_cohort`)

### 5.1 The constraint, traced
The l1Key folds `BindingUID` (`resolved.go:653`), and `BindingUID` is derived per-request from `rbac.EvaluateRBAC(Username, Groups, verb, GVR, ns, name)` at **`internal/handlers/dispatchers/helpers.go:212`** (the `dispatchCacheLookupKey` path). The key is a one-way SHA — **the server cannot recover the subject from the key alone**, and the client never computes the key (snowplow returns it, §6). Two users sharing a binding **share one key** (per-binding sharing — `resolved.go:305-330`, the equivalence-class invariant); `RepresentativeUsername` records only the FIRST writer (`helpers.go:236`, `resolved.go:319`), so **representative-equals-you is the WRONG isolation test** (it would deny a legitimate co-binding user). And `widgetContent` keys are identity-FREE (`resolved.go:652`) — shared by everyone.

### 5.2 Mechanism — server re-derives the expected key under the CONNECTION's identity (RECOMMENDED)
The client, on `/refreshes`, sends for each widget it wants to arm: the **resource coordinates it used for `/call`** (`group, version, resource, namespace, name, page, perPage, extras, class`) — NOT just the opaque key. `validateSubscription(r, ui)` then, for each requested tuple, **recomputes the expected key under the connection's authenticated `UserInfo`** using the IDENTICAL derivation the dispatcher uses:

- identity-bound classes → reuse `dispatchCacheLookupKey`'s logic: `rbac.EvaluateRBAC(ui.Username, ui.Groups, "get", group, resource, ns, name)` → `BindingUID` → `cache.ComputeKey(inputs)` (`helpers.go:212-242`).
- `widgetContent` → reuse `dispatchWidgetContentKey` (`helpers.go:147`, identity-free `ComputeKey`).

If the recomputed key **equals** the key the client claims (and/or the client just sends the tuple and the server arms the recomputed key directly — see §6), arm it; else reject that key.

**Why this is forgery-proof:** the server computes `BindingUID` from the **connection's** JWT-derived `UserInfo` (`ui`, from §4), never from anything the client supplies. A malicious client that sends user-B's coordinates + user-B's key gets the server to compute the key under **attacker-A's** identity → A's `BindingUID` → a DIFFERENT key → mismatch → rejected. A client can only ever arm keys that A's own identity legitimately produces. This is exactly the `dispatchCacheLookupKey` FAIL-CLOSED posture (`helpers.go:197-210`: missing/foreign identity ⇒ empty-identity key ⇒ no match). **This reuses the existing key-derivation seam verbatim — no new identity logic, no `feedback_no_special_cases` violation.**

**`widgetContent` caveat (the one identity-free class):** its key is shared, so re-derivation yields the same key for every subject — a client CAN legitimately arm a widgetContent key it doesn't "own" because nobody owns it (the envelope is shared; the per-user `allowed` flag is applied at serve time, `widgets.go:144`, NOT in the cached body). **This is NOT a leak:** the signal is content-free (just "this shared envelope refreshed"), and the subsequent `/call` refetch re-applies per-user gating at serve. So arming a widgetContent key reveals only "the shared widget envelope changed," which is not subject-specific information. Document this explicitly; it is consistent with the existing identity-free-shell design.

### 5.3 Alternative — provenance map (NOT recommended)
Record `key → {authorized subjects}` at `/call` serve time; validate/filter against it at subscribe (a) or emit (b). **Rejected:** (i) unbounded growth (every served key × every subject); (ii) races a freshly-granted user (not yet in the map → false deny) or a freshly-revoked user (stale allow → leak window); (iii) duplicates state the RBAC snapshot already has. Re-derivation (§5.2) reads the live RBAC snapshot each time — correct under binding churn by construction.

---

## 6. Subscription-key exposure in `/call` (additive; `feedback_cache_must_not_constrain_jq`, body contract preserved)

The `/call` body is written by `writeResolvedJSON` (`internal/handlers/dispatchers/helpers.go:413`) — `Content-Type: application/json` + raw resolved bytes. Hit/miss sites: `restactions.go:139,291`, `widgets.go:147,187,299`. **Do NOT mutate the JSON body** (it is the widget-prop contract; any envelope change risks the layering contract and the byte-identical baselines, `feedback_byte_identical_baselines_clean_wire_shape`).

**RECOMMENDED — response HEADER, not body.** Emit the L1 key as a response header at every `/call` serve site:
```go
wri.Header().Set("X-Snowplow-Refresh-Key", cacheKey)   // additive; before WriteHeader
```
`cacheKey` is already in scope at all serve sites (it is the `dispatchCacheLookupKey`/`dispatchWidgetContentKey` return). A header is invisible to the JSON body contract and to non-subscribing clients. It MUST be added to CORS `ExposedHeaders` (`main.go:899`, currently only `["Link"]`) so the browser `fetch`/react-query layer can read it cross-origin.

**The frontend then has two equivalent options (frontend decides):**
- **(a) subscribe by the key** (`?keys=<k1>,<k2>`) AND send the coordinates for §5 validation; OR
- **(b) subscribe by the coordinates only** (`?sub=<base64 json of {group,version,resource,ns,name,page,perPage,extras,class}>`), and let the server re-derive the key under the connection identity (§5.2) and arm THAT. **(b) is cleaner** — the client never needs to round-trip the opaque key, and there's nothing to validate-by-equality (the server simply arms what it derives). The `X-Snowplow-Refresh-Key` header is then optional/diagnostic. **Recommend (b)**; expose the header anyway for debuggability + a future "supersede the hand-declared `watch`" path (plan Phase 5).

**Tradeoff to flag:** option (b) means the `?sub=` payload includes `extras`, which can be large for some widgets; cap/validate its size (reject > a few KB) to avoid a memory-amplification subscribe vector.

---

## 7. Flags / gating (`project_single_cache_flag_direction`, `project_caching_is_provisional`)

- **No new master flag.** `/refreshes` and the broadcaster are **implicit under `CACHE_ENABLED`** (lean implicit, per `project_single_cache_flag_direction`). Cache-off ⇒ no refresher ⇒ no emit ⇒ idle stream. The endpoint is registered unconditionally on the mux (a route that no-ops under cache-off is harmless and matches `/list`/`/call` which also serve transparently cache-off).
- **One sub-layer back-out knob** (consistent with `RESOLVED_CACHE_APISTAGE_ENABLED` et al.): `REFRESH_SSE_ENABLED` (default true-when-cache-on) so the SSE layer is independently disable-able without losing L1 — provisional/removable. The broadcaster `Publish`/`Subscribe` become no-ops when off.
- **Coalesce window:** `REFRESH_COALESCE_WINDOW_MS` (default 250). **Connection budget / heartbeat interval:** must be sized **empirically** at 1000 users × N widgets (`feedback_capacity_caps_empirical_per_entry_cost`) — do NOT ship a guessed FD/connection cap. The Phase-0 tester baseline + a connection-scale measurement feed this.

---

## 8. LOC estimate & file:line targets

| Component | File | LOC | Kind |
|---|---|---|---|
| Broadcaster (hub, Publish, Subscribe, Arm/Disarm, coalesce, expvar counters) | `internal/cache/refresh_broadcaster.go` | ~120 | NEW |
| Emit call (1 line) | `internal/handlers/dispatchers/resolve_populate.go` @ `:291` | 1 | EDIT |
| SSE handler | `internal/handlers/refreshes.go` | ~140 | NEW |
| `validateSubscription` (re-derive key under conn identity; reuse `dispatchCacheLookupKey`/`dispatchWidgetContentKey` logic — may need a small exported helper from `dispatchers`) | `internal/handlers/refreshes.go` (+ ~30 in `dispatchers` to export the derivation) | ~70 | NEW |
| `RefreshAuth` cookie-or-header middleware | `internal/handlers/middleware/refreshauth.go` | ~45 | NEW |
| Mux wiring + `ExposedHeaders` += refresh-key | `main.go` @ `:803` region, `:899` | ~6 | EDIT |
| `X-Snowplow-Refresh-Key` header at 5 serve sites | `restactions.go:139,291`; `widgets.go:147,187,299` | ~5 | EDIT |
| CORS origin change (ONLY if cross-origin, §4.4-II) | `main.go:890` (config-shaped) | ~3 | EDIT (conditional) |
| Flags (`REFRESH_SSE_ENABLED`, `REFRESH_COALESCE_WINDOW_MS`) | `internal/cache/*.go` + `main.go` | ~15 | NEW |
| **B-delta — `keyRefs` reverse index + `HasRefreshSubscriber`** | `internal/cache/refresh_broadcaster.go` (A's file) | ~15 | NEW (B) |
| **B-delta R-B1' — `ResolvedEntry.LastResolveMS` field + stamp (`time.Since` around `resolveOnceFn`)** | `internal/cache/resolved.go` (struct) + `internal/handlers/dispatchers/resolve_populate.go:209` | ~4 | EDIT+NEW (B) |
| **B-delta — `effectiveFloor` + `isCheapShortFloorEligible` (positive cost gate: PRIMARY `LastResolveMS` + belt-and-braces `entryBytes`,resolved.go:902 + optional `Pinned`) + per-cheap-key bypass-rate cap + `flooredSubscribedTotal` counter** | `internal/cache/refresher.go` @ `:729` (replace `rateFloor()` read) + helpers | ~45 | EDIT+NEW (B) |
| **B-delta — flags `REFRESH_SUBSCRIBED_FLOOR_MS` (default=coalesce window), `REFRESH_SHORT_FLOOR_MAX_MS` (R-B2.c, PRIMARY), `REFRESH_SHORT_FLOOR_MAX_BYTES` (R-B2.a), `REFRESH_MAX_SHORT_FLOOR_PER_MIN` (R-B2.b)** | `internal/cache/*.go` | ~12 | NEW (B) |
| Unit + `-race` tests (broadcaster ordering/drop/coalesce; isolation forge-reject incl. 9.4 per-row; cache-off unreachability 9.5; 9.7 amplification harness; 9.8 dropped-terminal) | `*_test.go` | ~310 | NEW (test) |

**Total production code: A ≈ 380 LOC; B-delta ≈ +63 LOC** (one EDIT at refresher.go:729 + the broadcaster reverse-index + 2 flags) across the same 3 new files + ~5 edited lines; **+~310 LOC tests.** Net-new behaviour is additive and toggle-gated; no existing path's bytes change (the `/call` body is untouched; the header is additive; the emit fires only on refresher commits; B alters ONLY the floor *value* for subscribed keys, leaving the defer/dedup/dispatch identical).

---

## 9. Falsifiers (the gates; `feedback_falsifier_first_before_ship`, `feedback_validate_content_not_just_status`)

All runtime falsifiers MUST run against `--context gke_neon-481711_us-central1-a_cluster-1` (diagnostic-only; `kubectl` not in any latency score).

1. **No apiserver LIST issued by the refetch (THE invariant).** Drive a composition reconcile; observe one SSE `event: refresh` for the dependent widget's key; the frontend (or a curl emulating it) refetches `/call`. **Assert:** `snowplow_apiserver_fallthrough_cells` (the F-1/F-3 scope counters, `/debug/vars`) for the `call-*` scope do **not** increment across the refetch, AND `resolved_cache.lookup hit=true` fires for that key (`helpers.go:269`). Artifact: expvar delta + the `resolved_cache.lookup` log line. (If a refetch ever shows a miss→passthrough, the design failed — proposal §4 risk.)
2. **Coherence, not zero-latency (the corrected falsifier — see §1.1a).** Capture `body_sha` of the widget `/call` at: (t0) pre-reconcile, (t1) the instant the SSE signal arrives, (t2) the refetch response. **Assert:** (i) t2 `body_sha` == the post-reconcile expected; (ii) the signal at t1 arrives ONLY AFTER `resolveAndPopulateL1` logged `re-resolved + stored` (`resolve_populate.go:292`) for that key (ordering: emit is strictly post-Put); (iii) a `/call` issued at any t∈(t0, t1) still returns the OLD `body_sha` — i.e. the signal does NOT arrive before L1 is actually fresh (coherent-by-construction). **Latency expectation, NOT a fail condition:** t1−t0 ≈ the rate-floor (~2s at-rest default; ~5s under sustained `/call`) — this MATCHES the Phase-0 tester's measured ~2.0s/~5s/~10s window and is the floor's cost, shared with today. The PASS criterion is **coherence (the signal is never premature, and the post-signal refetch is always fresh)**, NOT a sub-second t1. Compare t1−t0 against the tester's baseline; if option B (§10-E) is chosen, re-assert t1−t0 drops sub-second AND re-run falsifier-for-amplification (§9.7).
3. **Emit fires once per coalesced commit, never pre-commit.** Unit: a refresher cycle that Puts → exactly one `PublishRefresh`; a cycle that hits a no-Put return (`resolve_populate.go:217/228/259`) → **zero** `PublishRefresh` (this is the §1.1 correction's encoded test); N Puts within the coalesce window → 1 signal.
4. **Cross-user isolation (RBAC-leak falsifier) — STRENGTHENED per PM (9.4).** Two parts:
   - **(4a) identity-bound classes:** User A's connection subscribes with user B's resource coordinates + B's key. **Assert** the server re-derives under A's identity → different key → arm rejected (0 signals to A for B's key) even when B's key actively refreshes.
   - **(4b) identity-FREE `widgetContent` — per-ROW RBAC CONTENT assertion (NOT status-only; `feedback_validate_content_not_just_status`).** A arms a shared `widgetContent` key (legitimately — nobody owns it, §5.2). On signal, A refetches `/call`. **Assert the refetched BODY is per-A-gated at the row level:** for a widget whose `status.resourcesRefs.items[]` A and B can see DIFFERENT subsets of, assert A's response contains A's `allowed:true` rows and `allowed:false` (or omitted) for rows only B may act on — i.e. diff A's served rows against B's served rows for the SAME key and assert they differ exactly per each subject's RBAC. The signal being shared must NOT leak B's row-level visibility into A's body. Artifact: A's vs B's `/call` body row-set + `allowed` flags for the shared key, post-signal, asserted against each subject's `rbac.EvaluateRBAC` grant. This is the gate that proves the identity-free-shell + serve-time-gate design (widgets.go:144) holds end-to-end through the live-refresh path.
5. **`CACHE_ENABLED=false` ⇒ clean no-op — STRENGTHENED per PM (9.5): prove UNREACHABILITY + correct CONTENT.** Two parts:
   - **(5a) `PublishRefresh` provably unreachable:** with cache off, the refresher singleton does not exist and `resolveAndPopulateL1` returns at `resolve_populate.go:96-99` BEFORE the Put → the emit at `:291` is never reached. **Assert** via a code-path falsifier: a unit/integration test with `CACHE_ENABLED=false` drives an informer-equivalent change and asserts the broadcaster's emit counter stays 0 AND `refreshHub()` returns nil (the §2.4 nil-path). Belt-and-braces: `PublishRefresh` itself early-returns on `Disabled()` (§2.2) — assert that guard with a direct call under cache-off (counter unchanged).
   - **(5b) `/call` correct CONTENT under cache-off (transparent fallback, not just 200):** `GET /refreshes` returns 200 + idle stream (only `: keepalive`, zero `refresh` events); AND a `/call` for the same widget returns the **byte-correct resolved body** served direct from apiserver (assert non-empty, correct rows — `feedback_validate_content_not_just_status` — NOT merely HTTP 200). Artifact: cache-off pod (`--context gke_neon-481711_…`), curl `/refreshes` frame dump + `/call` body content-diff vs cache-on body.
6. **Slow-consumer never stalls the refresher.** `-race` test + a deliberately-blocked subscriber sink: assert `PublishRefresh` returns promptly (drop counter bumps), other subscribers still receive, refresher `completedTotal` keeps climbing.
7. **(MANDATORY pre-B-ship, B is RATIFIED §12) — no refresher-amplification regression from the short floor (9.7). THE B-delta gate.**
   - **Setup / denominator:** the concurrent tester is capturing the **floor-ON (A) baseline** — refresher CPU%, alloc rate (B/s), workqueue cumulative-delay, refresher `completedTotal`/`flooredTotal`, and warm `/call` p50 — under a **composition-install CRUD churn wave at SCALE=50000** (`feedback_test_scale_50k`), with a representative *subscribed working set* (e.g. 1000 connections × K mounted widgets armed via `/refreshes`). This baseline IS the denominator.
   - **B arm:** identical wave + identical subscribed working set (which MUST include BOTH aggregate widgets — piechart/compositions-list — AND detail widgets, all armed via `/refreshes`), with the short floor active (`REFRESH_SUBSCRIBED_FLOOR_MS=250`, `maxShortFloorPerMin` at its empirically-chosen default).
   - **Assertions (ALL must hold, content-and-mechanism, not status):**
     - **(a) ALL THREE expensive-cell sources stay long-floored even when subscribed (the dangerous case excluded by MEASURED COST — §13.2 axis 0; the PRIMARY gate). Three sub-cells, all asserted (so the artifact provably exercises the bytes-vs-cost divergence, not just large-bytes cells):**
       - (a-i) a subscribed **registered cluster-list** cell (compositions-list / piechart count) — assert it took the **2s floor**, re-resolve count == floor-ON baseline (~100:1 collapse preserved), `flooredSubscribedTotal` does NOT tick; `isCheapShortFloorEligible` returns false (large `LastResolveMS` + large bytes + Pinned).
       - (a-ii) a subscribed **RA-FullList** cell — assert 2s floor, baseline re-resolve count, eligibility false (large `LastResolveMS` + Pinned).
       - (a-iii) **THE R-B1' HOLE — the SCAN-1000-NS-RETURN-3-ROWS cell, named explicitly as the falsifier scenario.** Construct a narrow-RBAC identity whose cluster-list collapse fails gate 5 RBAC (`cluster_list.go:248`) so the request falls to the per-NS iterator path and scans ~1000 namespaces returning a handful of rows — Put via `apistage.go:513` WITHOUT `RegisterClusterListKey`, yielding **SMALL result bytes BUT large measured resolve wall-ms** (the `cluster_list.go:74-79` 11-22s class). **Assert it took the 2s floor on the `LastResolveMS >= shortFloorMaxMS` term — NOT on bytes (its `entryBytes` is small) and NOT on class (`IsClusterListKey(key)==false`, `isRAFullListEntry==false`).** This is the decisive assertion that the prior bytes-only/class predicate would have FAILED (the cell would have short-floored → 0.30.205 leak) and the wall-ms predicate PASSES. Assert its expensive per-NS-scan re-resolve count == floor-ON baseline and `flooredSubscribedTotal` does NOT tick for it. **The artifact MUST record this cell's small `entryBytes` alongside its large `LastResolveMS` side-by-side, to prove the divergence is what the gate keyed on.**
       - All three: the expensive re-resolve path is byte-identical to floor-ON.
     - (b) **DETAIL cells get the short floor + the ~250ms benefit.** For a subscribed detail widget: assert it took the short floor (`flooredSubscribedTotal` ticks) and its signalled-fresh latency t1−t0 (§9.2) ≈ 250ms. (If detail latency did NOT drop, B bought risk with no benefit → reject.)
     - (c) **Under the 50K-install churn wave, AGGREGATE re-resolve counts == floor-ON baseline (no amplification on the expensive path).** The aggregate cells' re-resolve count, refresher CPU attributable to cluster-LIST re-resolves, and alloc from aggregate parsing are within baseline variance — the expensive path shows ZERO incremental work from B. This is the direct 0.30.205 / 0.30.185 guard.
     - (d) **No overall refresher-amplification regression:** total refresher CPU%, alloc B/s, and workqueue cum-delay within an agreed band of the floor-ON baseline (band from baseline variance — NOT a guessed %; `feedback_capacity_caps_empirical_per_entry_cost`). The 0.30.185 revert was 5.9× — the gate must catch anything approaching that. With axis 0 excluding the expensive path, the only permitted increment is bounded CHEAP-detail re-resolves.
     - (e) **Subscriber-gate did not leak:** UNsubscribed dirtied detail keys show the SAME re-resolve count as floor-ON (`flooredSubscribedTotal` vs `flooredTotal` consistent with only the subscribed-detail set taking the short floor).
     - (f) **Warm `/call` p50 non-regression:** mix-weighted warm p50 (0.95 narrow + 0.05 admin, `project_north_star_ledger`) within the agreed band of the floor-ON baseline (customer-facing guard, `feedback_customer_priority_over_refresher`).
   - **Artifact (attaches to the B-delta ledger row):** floor-ON vs B-arm side-by-side: pprof CPU+alloc profiles **split by re-resolve class** (aggregate vs detail), `/debug/vars` refresher counters (`completedTotal`, `flooredTotal`, `flooredSubscribedTotal`, `clusterListCompleted`, `droppedTotal`), per-key re-resolve histogram tagged aggregate/detail, workqueue depth/delay, warm p50 histogram, and the t1−t0 distribution split aggregate/detail. **This is the gate that authorises B to ship.**
8. **Dropped-terminal-signal degrades to the frontend throttle, NEVER indefinite stale (9.8, per PM).** A dropped signal (slow-consumer full-channel drop, §2.2 `default:` arm) for the FINAL emit in a churn burst must not strand the widget stale forever. **Assert:** with a subscriber whose sink is saturated so the *last* `refresh` for a key is dropped (and no further commit re-signals), the frontend's blind 5s throttle re-fire (`liveRefresh.ts` leading+trailing, proposal §6) still issues a refetch within ≤5s → reads the fresh L1 → converges. I.e. the SSE signal is an *optimisation* over the existing 5s safety-net, never a replacement that can deadlock on a drop. Artifact: a saturated-sink integration test — drop the terminal signal, assert a refetch fires by the 5s throttle and the body converges to fresh; assert the widget never stays stale past 5s. (This is why A's design keeps coexistence with the frontend throttle during AND after cutover; the throttle is the floor under the signal.)

---

## 10. Risks (ranked) & options

**Freshness expectation-setting (the Phase-0 tester's finding, §1.1a; now resolved by the B ratification).** Under A's floor (every UNsubscribed key, and the fallback if 9.7 fails) the signalled-fresh latency is the rate-floor (~2s at-rest, ~5s under load — `refresher.go:97-99,105,124`); the win there is COHERENCE, not a smaller number. **Under B (RATIFIED, §12) the SUBSCRIBED-key latency drops to the short floor (~250ms)** — sub-second signalled-fresh — at the cost of the amplification surface bounded in §13 and gated by 9.7. The honest ledger framing: "silent-stale ~2s → signalled-fresh ~250ms for visible widgets (B), ~2s coherent for the rest (A)." The biggest residual risk shifts from "is ~2s acceptable" to "does the short floor amplify the refresher under a 50k install wave" — which §13's bound answers and §9.7 proves.

**Biggest risk — connection scale at 1000 users × N widgets.** Long-lived SSE connections × users, each holding a goroutine + buffered channel, plus gateway/LB/FD ceilings. `watch.Broadcaster`'s drop-on-full protects the refresher, but the raw connection count is the production unknown. **Must be sized empirically** (`feedback_capacity_caps_empirical_per_entry_cost`) — one multiplexed connection per browser tab (the frontend's ref-counted `sseClient.ts` already does this, proposal §6) keeps it to ~1 connection/user, not 1/widget. **Action:** a connection-scale measurement BEFORE shipping a cap; idle-eviction of armed-but-silent connections via the heartbeat.

**Second — the 300s `WriteTimeout` (`main.go:904`).** Confirmed (TRACED): an SSE connection held open > 300s is killed by the server unless `http.ResponseController.SetWriteDeadline(time.Time{})` is set per-connection (§3.2 step 4; Go 1.25.3 supports it). If the gateway in front ALSO imposes a shorter idle/read timeout, the 20s heartbeat must beat it — verify the gateway timeout (deployment fact → OPEN). **Option:** a dedicated `http.Server` on a second listener with `WriteTimeout:0` for `/refreshes`, if the per-connection deadline-reset proves fragile under the gateway — surface as a fallback.

**Third — CORS `*`-vs-credentials** (§4.4). Same-origin (likely) → non-issue; cross-origin → a shared-CORS change needing PM sign-off.

**Fourth — `?sub=` `extras` size** (§6): cap to avoid a subscribe-time memory vector.

### Strategic options for Diego / team lead
- **A. Subscription transport:** (a) subscribe-by-key (client round-trips the opaque key) vs **(b) subscribe-by-coordinates** (server re-derives). **Recommend (b)** — simpler client, forgery-proof by construction, nothing to validate-by-equality.
- **B. CORS:** **(I) confirm same-origin** (recommend — verify topology, likely zero change) vs (II) explicit-origin list (a shared-CORS behaviour change).
- **C. SSE server:** **per-connection `SetWriteDeadline(0)` on the existing server** (recommend) vs a dedicated `WriteTimeout:0` listener (fallback if gateway-fragile).
- **D. Emit seam:** **`resolve_populate.go:291` post-Put** (recommend — surgically correct, fires only on real commits) vs `processOne` post-`fn` (simpler but over-fires on the 4 no-Put returns — rejected, §1.1).
- **E. Freshness/floor trade — RESOLVED: Diego RATIFIED option B (2026-06-18), eyes-open against the A-recommendation. See §12–§13 for the full B mechanism.**
  - **(A) post-floor commit, no floor change** — ~2s/~5s signalled-coherent, zero amplification risk. Remains the behaviour for every UNsubscribed key under B.
  - **(B) subscriber-gated SHORT floor, CHEAP cells ONLY (RATIFIED + scoped by Diego 2026-06-18; closure refined by PM re-gate R-B1 → R-B1').** Sub-second (~250ms) signalled-fresh for *subscribed CHEAP* cells only. NOT no-floor — a much shorter floor (= coalesce window) so within-wave bursts still collapse (§12.1). **Eligibility is a POSITIVE cost gate keyed on MEASURED resolve wall-ms (`LastResolveMS < MAX_MS`, PRIMARY) + result bytes (belt-and-braces) + optional `Pinned` (§12.2a) — NOT class-exclusion and NOT bytes-alone.** This closes the R-B1' hole: the scan-1000-NS-return-3-rows apistage cell (small result bytes BUT 11-22s resolve, `cluster_list.go:74-79`) is caught by the wall-ms term that bytes-only missed. `entry.Pinned` is RAFullList-only (TRACED, `resolve_populate.go:108-113`) so it is explicitly NOT the cost signal. EVERY expensive cell (registered cluster-list, raFullList, AND the scan-many-return-few per-NS hole) keeps the 2s floor by measured cost. Closure (i) "short-floor widgetContent by class" was REJECTED on coherence (a refreshed envelope reads its apistage L1 intermediate via content-Get-HIT, `apistage.go:479`, so a floored intermediate → stale rows). Safety = measured-cost-exclusion (primary, §13.2 axis 0) + subscriber-gated + still-coalesced + per-cheap-key cap; 9.7 (§9.7) is the MANDATORY pre-ship gate. Emit seam (§1.1), broadcaster, SSE, auth, isolation ALL unchanged — B is a one-gate floor-timing delta (§12.2) + a 2-line cost stamp (§12.2a), touching ONLY the cheap path.
  - **Decision logged:** B ships behind the 9.7 amplification falsifier passing against the tester's floor-ON baseline. If 9.7 shows a regression approaching the 0.30.185 class, B is held and A is the fallback (the broadcaster/SSE/auth/isolation work is identical and ships regardless).

---

## 11. Open items needing facts I cannot recover from snowplow source (cross-team / deployment)
- **Portal session cookie name** (for `RefreshAuth` cookie extraction, §4.3) — frontend/portal-chart fact.
- **Deployed origin topology** (portal SPA vs `/refreshes` same-origin? §4.4) — gateway/chart fact.
- **Gateway idle/read timeout** in front of snowplow (heartbeat must beat it, §10) — deployment fact.
- **Empirical connection ceiling** at 1000 users (cap sizing, §7/§10) — the concurrent tester's connection-scale measurement.
- **Phase-0 stale-window magnitude** (the concurrent tester) — if the window is already vanishingly small under the in-process refresher, re-confirm the build is worth the connection-scale cost at the PM gate (per the brief's revise-at-PM-gate note).

---

## 12. OPTION B — subscriber-gated short-floor (RATIFIED by Diego 2026-06-18, against the A-default recommendation, eyes-open)

> **Status:** B is now in scope, **additive to A**. Everything in §1–§11 (emit seam `resolve_populate.go:291`, broadcaster, SSE, `RefreshAuth`, isolation-by-re-derivation) is **UNCHANGED**. B touches **one gate only**: the per-key re-resolve rate-floor in `processNext`, and **only for keys that have an active `/refreshes` subscriber**. A's ~2s coherence holds for every unsubscribed key; B lowers the floor to the coalesce window (~250ms) for subscribed keys so the signalled-fresh latency drops sub-second.

### 12.1 The mechanism is a SHORTER floor, NOT no-floor (the safety-critical distinction)
A floor of **zero** would let a CRUD storm become a re-resolve storm (every dirty-mark re-resolves immediately). B uses a **second, much shorter floor for subscribed keys** = the coalesce window (default 250ms, `REFRESH_COALESCE_WINDOW_MS`, §2.3). Within-wave bursts still collapse — the delaying-queue's earliest-wins dedup (§12.4) plus a 250ms floor means a key dirty-marked 50× in a 250ms wave still re-resolves **once**, not 50×. We trade 2s→250ms (an 8× shorter floor), NOT 2s→0.

### 12.2 Where the floor check lives, and the exact alteration (TRACED, file:line)
The floor gate is `processNext` at **`internal/cache/refresher.go:729-753`**:
```go
c := ResolvedCache()
entry, ok := c.Get(key)                   // :727 — the SINGLE Get (serves the floor's CreatedAt read)
if floor := r.rateFloor(); floor > 0 && ok && entry != nil {
    if elapsed := time.Since(entry.CreatedAt); elapsed < floor {
        q.Forget(key)
        q.AddAfter(key, floor-elapsed)   // :749 — defer to floor expiry
        r.flooredTotal.Add(1)
        return true
    }
}
```
`r.rateFloor()` (`refresher.go:358-359`) reads `RESOLVED_CACHE_REFRESHER_RATE_FLOOR_SECONDS` (default 2s) fresh per dequeue. **B changes the effective floor to be per-key, consulting subscription state AND cell-class:**
```go
// B-delta — replace the single rateFloor() read with a per-key effective floor.
floor := r.effectiveFloor(key, entry)     // ← NEW (refresher.go ~:729; entry already loaded at :727)
if floor > 0 && ok && entry != nil {
    if elapsed := time.Since(entry.CreatedAt); elapsed < floor {
        q.Forget(key)
        q.AddAfter(key, floor-elapsed)     // unchanged — same defer + same dedup
        r.flooredTotal.Add(1)
        r.flooredSubscribedTotal.Add(boolToU64(floor < r.rateFloor()))  // ← NEW counter (short-floor took effect)
        return true
    }
}
```
with the refined helper (in `refresher.go`, ~25 LOC) — **Diego 2026-06-18 + PM R-B1: the short floor is for CHEAP cells ONLY; every EXPENSIVE cell (registered cluster-list, RA-FullList, AND the per-NS-iterator-fallback apistage LIST) keeps the 2s floor even when subscribed, gated by a POSITIVE cost predicate (§12.2a), not by class**:
```go
// effectiveFloor returns the SHORT cheap-cell floor (the coalesce window)
// when key has ≥1 active /refreshes subscriber AND the cell is CHEAP to
// re-resolve (isCheapShortFloorEligible — PRIMARY: LastResolveMS < MAX_MS;
// belt-and-braces: entryBytes < MAX_BYTES; optional: !Pinned — §12.2a/R-B1');
// otherwise the standard #318-R1a 2s floor. Expensive cells keep
// the long floor unconditionally — they are the continuously-churning
// 100:1-collapse case the floor exists to protect (the 0.30.205 / full-
// cluster-LIST class, INCLUDING the per-NS-iterator apistage hole that a
// class label misses). Nil-safe: no hub / cache-off → standard floor
// (A-identical).
func (r *refresher) effectiveFloor(key string, entry *ResolvedEntry) time.Duration {
    base := r.rateFloor()                   // :358 — the 2s default, unchanged
    if base <= 0 { return base }            // floor disabled → leave disabled
    if !HasRefreshSubscriber(key) {         // ← broadcaster O(1) lookup, §12.3
        return base                         // unsubscribed → standard floor
    }
    if !r.isCheapShortFloorEligible(entry) { // ← §12.2a — POSITIVE cheap-cost gate (closure ii)
        return base                          // subscribed BUT expensive/aggregate → KEEP 2s floor
    }
    short := subscribedFloor()               // REFRESH_SUBSCRIBED_FLOOR_MS (default = coalesce window)
    if short < base { return short }         // NEVER raise a key's floor
    return base
}
```
**Nothing else in `processNext` changes** — same `q.Forget`/`q.AddAfter` defer, same delaying-queue, same `processOne` dispatch, same emit at `resolve_populate.go:291`. The ONLY behavioural delta is the floor *value*, and only for **subscribed CHEAP** cells (§12.2a defines "cheap" by a positive cost gate, NOT by class-exclusion — see the R-B1 closure verdict below).

### 12.2a — R-B1 CLOSURE: the hole, the data-path verdict, and the corrected predicate (closure ii — positive cheap-cost gate). (TRACED)

**The hole the PM found is REAL (TRACED).** The cluster-list collapse has deny-gates — gate 4 (GVR derivation, `cluster_list.go:215`), gate 5 (RBAC, `:223`/`:248`), gate 8 (cell-cold async, `:318`) — each `return nil, false, N`. On any deny, the caller falls back to the **per-NS iterator path** and the resulting apistage content cell is Put via `apistageContentServe` (`apistage.go:513-539`) WITHOUT `RegisterClusterListKey`. So a class-exclusion predicate (`IsClusterListKey || isRAFullListEntry`) classifies it as detail → short-floor-eligible — yet its per-refresh cost can be the 0.30.205 magnitude (code-pinned: `cluster_list.go:74-79` — "admin warm /call 11-22s, CPU 7.4/8"). Class-exclusion's "structurally impossible" claim held for only the 2 *registered* classes. **The PM is correct.**

**DATA-PATH VERDICT (the decisive trace that picks the closure):** when the refresher re-resolves a `widgetContent`/`widgets` key, does it re-run FRESH from L3, or read the floored apistage L1 intermediate?
- `resolveOnceProd` (`resolve_populate.go:369-383`) routes `widgetContent` → `resolveWidgetForRefresh` (`:517`), which calls `widgets.Resolve(...)` on the **freshly re-fetched CR** (`got.Unstructured`, re-`objects.Get` at `resolve_populate.go:344`).
- BUT inside that resolve, an apistage-backed stage goes through `apistageContentServe`, which does `store.Get(contentKey)` (`apistage.go:479`) and **on HIT uses `entry.RawJSON`/`entry.Items` from the apistage L1 cell and SKIPS the fresh dispatch** — the fresh `dispatchViaInformer` is ONLY in the MISS `else` branch (`apistage.go:513-516`).

**∴ a widgetContent refresh reads the apistage L1 intermediate via content-Get-HIT (`apistage.go:479-512`); it does NOT re-dispatch fresh from L3 when that cell is warm.** Therefore **closure (i) "short-floor widgetContent only" is NOT coherent** — it would re-resolve the envelope on the 250ms cadence but serve data only as fresh as the floored (≥2s, or per-NS-hole-2s) apistage intermediate. Closure (i) is REJECTED on coherence grounds.

**R-B1' CORRECTION (PM close-out, verified at source — the predicate above was DEFECTIVE).** Two source facts kill the bytes+Pinned predicate:
- **`entry.Pinned` is RAFullList-ONLY.** TRACED: the only `Pinned=true` sites are `resolve_populate.go:269` (`Pinned: prePinned`, and `prePinned` is non-false ONLY for `CacheEntryClassRAFullList`, `:108-113`) + `ra_full_list_store.go:66,87`; the `resolved.go:802,808` sites set it `false`. So **`!entry.Pinned` is always true for apistage cells** → it excludes nothing on the apistage path.
- **`ResolvedEntry` has NO wall-ms/duration field** (struct = RawJSON, CreatedAt, Inputs, Items, ItemsAPIVersion, ItemsKind, Pinned — TRACED), and `entryBytes` (`resolved.go:902-920`) is a **RESULT-SIZE** proxy (`len(RawJSON)` + Items-tree estimate), NOT a cost proxy.
- ∴ for apistage, the predicate collapses to **bytes-only** → the **scan-1000-NS-return-3-rows** cell (small result bytes, but the `cluster_list.go:74-79` 11-22s / CPU 7.4/8 *resolve*) PASSES → short-floored → the 0.30.205 leak. **The hole stayed open. PM is correct.**

The fix is in my own design lineage: `resolved.go:235` already specifies cost as "resolve wall-ms **OR** envelope bytes over a threshold" — but the *field* carrying wall-ms was never stamped. **R-B1' stamps it and makes wall-ms the primary cost signal.**

**STAMP SITE (file:line) — `internal/handlers/dispatchers/resolve_populate.go:209`.** Wrap the `resolveOnceFn` wall-clock boundary (the resolve runs on the refresher goroutine, which already pays this cost — zero added work):
```go
// resolve_populate.go ~:209 — B-delta R-B1' (≈2 lines + 1 struct field)
t0 := time.Now()
encoded, err := resolveOnceFn(rctx, inputs)
resolveMS := time.Since(t0).Milliseconds()
// … (existing err / encoded==nil / evicted / stage-error gates unchanged) …
entry := &cache.ResolvedEntry{
    RawJSON:       encoded,
    Inputs:        &inputs,
    Pinned:        prePinned,
    LastResolveMS: resolveMS,   // ← NEW field on ResolvedEntry; the measured cost signal
}
```
New field: `ResolvedEntry.LastResolveMS int64` (stamped here; on cold-dispatch Puts it can be stamped at the dispatcher resolve boundary too, or left 0 — a 0 value FAILS the `< MAX` cheap test only if we invert; see fail-safe below). The refresher re-stamps it every re-resolve, so it tracks the cell's current cost.

**ADOPT closure (ii), R-B1'-corrected — wall-ms primary, bytes belt-and-braces:**
```go
// isCheapShortFloorEligible — §12.2a (R-B1' closure ii). Short floor applies
// ONLY to entries CHEAP to RE-RESOLVE, by MEASURED resolve wall-ms (primary)
// AND result bytes (belt-and-braces), regardless of class. Catches cheap
// widgetContent + cheap apistage GET-by-name; EXCLUDES the expensive
// per-NS-iterator/LIST apistage envelope AND raFullList — INCLUDING the
// scan-many-return-few hole (small bytes, large wall-ms) the bytes-only
// predicate missed.
func (r *refresher) isCheapShortFloorEligible(entry *ResolvedEntry) bool {
    if entry == nil || entry.Inputs == nil { return false }       // fail-safe → 2s floor
    if entry.LastResolveMS <= 0 { return false }                  // un-stamped/cold → fail-safe 2s (until a refresher re-resolve stamps it)
    if entry.LastResolveMS >= shortFloorMaxMS() { return false }  // ← PRIMARY: measured resolve cost (resolved.go:235 lineage)
    if entryBytes(entry) >= shortFloorMaxBytes() { return false } // belt-and-braces: result size (resolved.go:902)
    // entry.Pinned MAY stay as a RAFullList belt-and-braces term, but it is
    // NOT the cost signal (it is RAFullList-only). Optional:
    if entry.Pinned { return false }                              // RAFullList belt-and-braces only
    return true                                                   // cheap → short-floor eligible
}
```
- **`entry.LastResolveMS` (PRIMARY, NEW)** carries the cost load. The scan-1000-NS-return-3-rows cell → small bytes BUT large `LastResolveMS` (2000-22000ms) → caught → 2s floor. Threshold `REFRESH_SHORT_FLOOR_MAX_MS` (R-B2.c). This is the discriminant the hole evaded.
- **`entryBytes` (belt-and-braces)** still excludes a large-result cheap-per-row cell. Threshold `REFRESH_SHORT_FLOOR_MAX_BYTES` (R-B2.a).
- **`entry.Pinned` (optional belt-and-braces)** — RAFullList-only; kept as a defensive third term, explicitly NOT load-bearing for cost.
- **Fail-safe direction:** an un-stamped entry (cold Put before any refresher re-resolve, or a legacy entry) has `LastResolveMS<=0` → NOT cheap → 2s floor. This is the SAFE direction (a cell is short-floored only AFTER a refresher re-resolve has measured it cheap), and it self-corrects: the first refresher re-resolve stamps the real cost, and from then the gate is authoritative. All reads are on the already-in-hand `entry` (loaded at `refresher.go:727`) — **zero extra Get.**

**Why R-B1'-corrected closure (ii) is BOTH amplification-safe AND coherent:**
- *Amplification-safe:* the expensive per-refresh cost is now measured directly (`LastResolveMS`), so EVERY expensive cell — registered cluster-list, raFullList, AND the scan-many-return-few per-NS-iterator apistage LIST (the hole) — stays 2s-floored by its measured resolve time, independent of result size or class. The 0.30.205/0.30.185 class is excluded by **measured cost**, the exact property that made it dangerous. **This is the honest, hole-closing claim.**
- *Coherent:* a cheap widgetContent's apistage intermediate, IF also cheap (small wall-ms), is ITSELF short-floor-eligible → both layers refresh on the 250ms cadence → genuinely-fresh data. IF the intermediate is expensive (large wall-ms → 2s-floored), the cheap envelope re-resolves every 250ms but reads the 2s intermediate via content-Get-HIT (`apistage.go:479`) — it converges at the source's 2s cadence and **never serves data older than its source** (coherent; not sub-second for widgets backed by expensive aggregates — exactly correct). No detail widget ever serves stale-beyond-its-source.

**Note vs the old class-exclusion:** `IsClusterListKey`/`isRAFullListEntry` are NO LONGER the predicate (they missed the per-NS hole). They remain useful only as a coarse cross-check in the 9.7 artifact's aggregate/detail bucketing (§9.7). The positive cost gate subsumes them: a registered cluster-list cell is also large-bytes/Pinned, so it is excluded by cost too — belt-and-braces.

### 12.3 "Is this key subscribed?" — the broadcaster's per-key index (reuses A's structure)
A's broadcaster (§2.1) already holds `subs map[uint64]*refreshSub`, each `refreshSub.keys map[string]struct{}`. For an O(1) subscriber-presence check (called on the hot dequeue path, must NOT scan all subs), B adds a **reverse index** to the broadcaster — a refcount per armed key:
```go
// refresh_broadcaster.go (A's file) — add to refreshBroadcaster:
keyRefs map[string]int   // l1Key → # of subscribers armed for it; guarded by mu
```
maintained in `ArmKey`/`DisarmKey`/`SubscribeRefresh`/`unsub` (increment on arm, decrement on disarm, delete at zero). Then:
```go
// HasRefreshSubscriber reports whether ≥1 connection is armed for l1Key.
// O(1) map read under RLock. Nil-safe (cache-off / no hub → false →
// effectiveFloor falls back to the standard floor → A-identical).
func HasRefreshSubscriber(l1Key string) bool {
    if Disabled() { return false }
    h := refreshHub(); if h == nil { return false }
    h.mu.RLock(); n := h.keyRefs[l1Key]; h.mu.RUnlock()
    return n > 0
}
```
This lives in `internal/cache` (no import cycle; the refresher is already in `internal/cache`). **A's broadcaster gains one map + refcount bookkeeping (~15 LOC); the refresher gains `effectiveFloor` + `isCheapShortFloorEligible` (~30 LOC, reusing existing cost signals `entry.Pinned` + `entryBytes`).** That is the entire B production surface beyond A.

### 12.4 The floor-bypass interacts CORRECTLY with the existing dedup + FAIL-OPEN (TRACED — load-bearing)
Two existing invariants make the shorter floor safe by construction:
- **Earliest-wins per-key dedup** (`client-go@v0.33.0/util/workqueue/delaying_queue.go:355-368`, `insert[T]`): a deferred `AddAfter` for a key already pending only updates `readyAt` to be sooner (`knownEntries[entry.data]` keyed by the l1Key) — **never a duplicate**. `waitingEntryByData` (:288) holds at most one entry per key. So N concurrent re-marks of ONE subscribed key in a wave collapse to ONE pending re-resolve regardless of floor length. The floor length controls only *how soon*, not *how many*.
- **FAIL-OPEN on decline** (`refresher.go:743-747`): `CreatedAt` is stamped only by a successful `Put` (`resolved.go:771-772`). A cell that declines to cache (stage-error / never-cache-partials) keeps an old `CreatedAt` → never floored → re-resolves each dequeue. The shorter floor does not change this — declining cells behave identically under A and B.

### 12.5 Cache-off / removability (unchanged from A)
`HasRefreshSubscriber` returns false under `Disabled()` → `effectiveFloor` falls back to the standard floor → **byte-identical to A, and to pre-B when cache is off.** `REFRESH_SSE_ENABLED=false` (§7) makes the broadcaster a no-op → no subscribers → `keyRefs` empty → standard floor everywhere. B is fully toggle-gated and removable (`project_caching_is_provisional`).

---

## 13. Amplification-safety argument (load-bearing — the #318-R1a / 0.30.185 5.9× / 0.30.205 dangerous case is EXCLUDED BY COST via the positive cheap-cost gate, then a residual bound)

### 13.1 The worst case, made explicit
**1000 users, each with a `/refreshes` connection armed for K widgets, during a composition-install CRUD wave** (the canonical scale, `project_composition_install_rbac_scale`): the composition-dynamic-controller installs N compositions, each ADD/UPDATE dirty-marking the shared aggregate cells (piechart count, compositions-list, the dashboard Statistics/LineChart) + per-binding dependent detail cells. **The dangerous case is the EXPENSIVE-per-refresh cell:** a single piechart/compositions-list cell is dirty-marked by EVERY one of 50K install ADDs, and each re-resolve is a **full cluster-LIST** (the 0.30.205 class; the 0.30.185 populate-amplification revert was exactly *expensive re-resolve work × churn-rate = wall-clock blow-up*; code-pinned at `cluster_list.go:74-79` — admin warm /call 11-22s, CPU 7.4/8). **Crucially (the R-B1 hole), an expensive cell is NOT always class-labelled "aggregate":** a cluster-list collapse that fails a deny-gate (4/5/8, `cluster_list.go:215/223/318`) falls back to the per-NS iterator path and Puts a plain `apistage` LIST cell — expensive, but NOT `RegisterClusterListKey`'d (`apistage.go:513`) and NOT raFullList. So the safety boundary MUST be *cost*, not *class*. The floor's tester-confirmed value is a **100:1 re-resolve collapse**, concentrated on these continuously-churning EXPENSIVE keys; detail-widget backing objects churn far slower than the 250ms window, so the floor buys them ~nothing.

### 13.2 Why a CRUD storm CANNOT become a re-resolve storm under B — the dangerous case is excluded BY COST, then a residual bound

**Axis 0 — POSITIVE CHEAP-COST GATE by MEASURED RESOLVE WALL-MS (R-B1' closure ii; the primary safety, excludes the dangerous case by the property that MAKES it dangerous — its measured per-refresh cost).** The short floor is gated on `isCheapShortFloorEligible(entry)` (§12.2a, R-B1'-corrected): an entry is short-floor-eligible ONLY if its **measured `entry.LastResolveMS < shortFloorMaxMS()` (PRIMARY)** AND `entryBytes(entry) < shortFloorMaxBytes()` (belt-and-braces) AND `!entry.Pinned` (optional RAFullList belt-and-braces). So EVERY expensive cell keeps the 2s floor unconditionally by its **measured resolve time** — a registered cluster-list cell (large wall-ms + bytes + Pinned), a raFullList cell (Pinned + large wall-ms), AND — critically — **the scan-many-return-few per-NS-iterator apistage LIST cell the deny-gates produce: small RESULT bytes but large measured wall-ms (the `cluster_list.go:74-79` 11-22s resolve), caught by the wall-ms term that the prior bytes-only predicate MISSED.** The 0.30.205/0.30.185 amplification class is **structurally excluded by measured cost**, not by a class label nor by a result-size proxy that the scan-many-return-few hole evades. This is the honest, hole-closing claim: the measured per-refresh COST that made the dangerous case dangerous is precisely the gate's primary discriminant.

Given axis 0, the residual short-floored population is **CHEAP-TO-RE-RESOLVE cells only** (small measured wall-ms AND small bytes, un-pinned: cheap widgetContent, cheap apistage GET-by-name, cheap detail widgets) — sub-`MAX_MS` re-resolves, backing objects that churn slower than the window. The residual is then bounded on three further axes:

1. **Subscriber-gated (bounded population).** The short floor applies ONLY to `HasRefreshSubscriber(key)==true` (§12.2) — the set of cheap cells *mounted in a live browser tab*, bounded by `users × mounted-cheap-widgets-per-tab`, NOT the 50K install fan-out. Every unsubscribed cell keeps the 2s floor.
2. **Still-coalesced per key.** The delaying-queue earliest-wins dedup (§12.4, delaying_queue.go:355-368) collapses any within-window burst of re-marks for one key to ONE re-resolve. A cheap cell dirty-marked 50× in a 250ms window re-resolves once, not 50×. Per-key re-resolve rate ≤ 1 / coalesce-window by an existing, proven mechanism.
3. **Per-cheap-key bypass-rate cap (defense-in-depth, NEW; guards the cheap residual ONLY).** A subscribed cheap cell may take the SHORT floor at most `maxShortFloorPerMin` times/min (R-B2 derivation, §R-B2 — empirically derived, NOT guessed, `feedback_capacity_caps_empirical_per_entry_cost`); beyond that it reverts to the 2s floor for the rest of the window. Per-key token check inside `effectiveFloor` — no unbounded map (bounded by the subscribed-cheap-key set, itself bounded by axis 1). Same poison-pill philosophy as `maxRefreshRequeues` (refresher.go:137). With expensive cells excluded by axis 0, this cap now protects only against an unusually fast-churning *cheap* object — a far less likely and far cheaper pathology than the expensive case it previously had to backstop.

**Composed worst-case re-resolve rate under B** = `(subscribed CHEAP cells) × min(1/coalesce-window, maxShortFloorPerMin/60s) × (cheap re-resolve cost)` — every factor bounded AND the expensive-per-resolve term is absent by axis 0. Compare the floor-ON denominator: `(all dirtied keys incl. expensive) × 1/2s`. B's incremental refresher work over floor-ON is **(subscribed-cheap-set × extra re-resolves from the 8× shorter window, capped by axis 3, at cheap cost)** — a small, bounded increment on the CHEAP path only. The expensive path is byte-identical to floor-ON. **This is the argument the PM close-out must confirm; 9.7 (§9.7) proves it empirically — including the per-NS-iterator apistage assertion (9.7a) so the hole cannot re-open silently.**

### 13.3 What B does NOT change (so the amplification surface is precisely scoped)
- **Expensive cells (registered cluster-list, raFullList, AND the per-NS-iterator-fallback apistage LIST): byte-identical to A/floor-ON** (axis 0 cost gate) — the expensive re-resolve cadence is unchanged for ALL of them.
- No new populate/resolve work per cycle (0.30.185 class) — B only shifts *when* an already-scheduled re-resolve fires, for a bounded *cheap* subset.
- No change to the customer-priority yield (`refresher.go:124,580`) — a subscribed-cheap short floor still yields to in-flight `/call` first (the yield is upstream of the floor gate at `refresher.go:694`). Customer `/call` retains absolute priority (`feedback_customer_priority_over_refresher`).
- No change to the two-tier cluster_list priority queue (`refresher.go:207-219`) — cluster_list cells refresh on their own tier AND keep the 2s floor (axis 0 cost gate); B's short floor applies orthogonally and only to cheap cells.

---

## R-B2 — derivation of the three numbers (NO design-time guesses; `feedback_capacity_caps_empirical_per_entry_cost`)

All three are DERIVED from the C-0 floor-ON baseline (the tester is capturing it; all land on the B-delta ledger row). No value is chosen at design time.

### R-B2.c — `shortFloorMaxMS()` (`REFRESH_SHORT_FLOOR_MAX_MS`) — the PRIMARY cheap/expensive resolve-cost boundary (§12.2a, R-B1')
The load-bearing threshold (closes the R-B1' hole). Derivation: from the C-0 baseline, record measured `LastResolveMS` (stamped at `resolve_populate.go:209`) for (1) the expensive re-resolves — registered cluster-list, raFullList, AND the **scan-many-return-few per-NS-iterator apistage LIST** at 50K scale (2000-22000ms per the `cluster_list.go:74-79` code pin) — and (2) the cheap re-resolves — a typical detail widgetContent / apistage GET-by-name (single-object re-dispatch, ~50-100ms). These populations are **orders of magnitude apart in wall-ms** (tens-of-ms vs seconds), so the threshold sits in the wide gap: `shortFloorMaxMS = cheap-population p99 ms × safety-margin`, with the margin chosen so the threshold is still ≤ `expensive-population p1 ms / 10`. **Arithmetic on the ledger row:** `cheap p99 = A ms; expensive p1 = B ms; threshold M s.t. A·margin ≤ M ≤ B/10`. This is the discriminant the scan-1000-NS-return-3-rows cell evaded under bytes-only (small bytes, but `LastResolveMS` lands in the expensive population) — the ms-gap is what catches it.

### R-B2.a — `shortFloorMaxBytes()` (`REFRESH_SHORT_FLOOR_MAX_BYTES`) — the belt-and-braces result-size boundary (§12.2a)
Derivation: from the C-0 baseline, record `entryBytes` (resolved.go:912) for (1) the expensive cells — registered cluster-list, raFullList, and the per-NS-iterator-fallback apistage LIST envelope at 50K scale (the compositions LIST is ~12.9 MB / ~30-174 MiB cluster-list per the code pins, helpers.go:405 + cluster_list.go:209-211) — and (2) the cheap cells — a typical detail widgetContent / apistage GET-by-name envelope (single-object, low-KB). These populations are **orders of magnitude apart** (KB vs MB), so the threshold sits in the wide empty gap between them: set `shortFloorMaxBytes = max(cheap-population p99 bytes) × safety-margin`, with the margin chosen so the threshold is still ≤ `min(expensive-population p1 bytes) / 10`. **Arithmetic shown on the ledger row:** `cheap p99 = X KB; expensive p1 = Y MB; threshold T s.t. X·margin ≤ T ≤ Y/10`. The 180×-estimation-error lesson (0.30.151) is why this is measured per-entry, not estimated; the KB-vs-MB gap makes the threshold robust (no knife-edge). Cross-check: every `Pinned` cell must also exceed T (belt-and-braces — Pinned already excludes the prewarmed-expensive set).

### R-B2.b — `maxShortFloorPerMin` (`REFRESH_MAX_SHORT_FLOOR_PER_MIN`) — the per-cheap-key bypass-rate cap (§13.2 axis 3)
Derivation arithmetic, from the floor-ON 100:1-collapse ceiling:
- The 2s floor's ceiling is **1 re-resolve / 2s = 30 re-resolves/min/key** (the floor-ON per-key maximum; this is the "100:1 collapse" denominator the tester confirms).
- A 250ms short floor's raw ceiling is **1 / 0.25s = 240 re-resolves/min/key** — 8× the floor-ON ceiling. The cap exists to claw an *outlier* cheap key back toward the floor-ON ceiling.
- Set `maxShortFloorPerMin = ceil(30 × allowed-multiplier)`, where `allowed-multiplier` is the per-key amplification we will tolerate on the CHEAP path, chosen from the baseline so that the AGGREGATE permitted increment — `subscribed-cheap-set-size × maxShortFloorPerMin × measured-cheap-re-resolve-cost-ms` — stays within the 9.7(d) refresher-CPU band. **Arithmetic shown:** with measured cheap re-resolve cost `c` ms (C-0 baseline; the ~50-100ms detail figure is the placeholder until measured) and subscribed-cheap-set size `S` (from the connection-scale measurement, §10), pick the largest `maxShortFloorPerMin` s.t. `S × maxShortFloorPerMin × c / 60000 ≤ band-fraction × (refresher core budget)`. Default lands once `c` and `S` are measured; the PM close-out records the exact value + the inequality it satisfied.

Both numbers' final values + the arithmetic that produced them attach to the B-delta ledger row (the falsifier-first artifact, `feedback_falsifier_first_before_ship`). If the C-0 baseline shows the cheap/expensive byte-gap is NOT orders apart (it should be — KB vs MB), R-B2.a's robustness assumption is re-examined at the close-out before B builds.

---

### Anchor corrections summary (for the brief's "correct me with file:line")
1. **Emit point:** your `processOne` post-`fn` (refresher.go:836) is one layer too shallow — it over-fires on `resolveAndPopulateL1`'s 4 no-Put success-returns. **Correct seam: `internal/handlers/dispatchers/resolve_populate.go:291`, immediately after `c.Put(key, entry)`** (refresher-only path; post-commit; fires only when L1 actually changed). `SetRefreshHook` (deps.go:407, fired at deps.go:539) as the wrong/pre-resolve seam — CONFIRMED.
2. **Auth:** `xcontext.UserInfo` is populated **header-Bearer-only** today (`userconfig.go:134-151,231`) — so the default `/call` middleware WOULD 401 an `EventSource`. **NOT a hard blocker** because `jwtutil.Validate` is stateless (`validate.go:16`) — the same JWT validates from a cookie. Needs a ~45-LOC `RefreshAuth` sibling + a CORS-origin decision (current `*`+credentials, `cors.go:256-271`, is browser-rejected cross-origin).
3. **Isolation:** `RepresentativeUsername` is the WRONG basis (per-binding shared cell; `resolved.go:305-330`). **Correct: server re-derives the key under the CONNECTION's `UserInfo` via the existing `dispatchCacheLookupKey` path (helpers.go:200-242)** — forgery-proof, no provenance map. `widgetContent` (identity-free, `resolved.go:652`) is a documented non-leak.
