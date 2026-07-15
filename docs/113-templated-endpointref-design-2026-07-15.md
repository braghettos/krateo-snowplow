# #113 — template a RESTAction api-step `endpointRef` from request extras

Date: 2026-07-15
Author: cache-architect
Repo: main @ 330a649 (1.7.12); 1.7.13 candidate
Issue: braghettos/krateo-snowplow #113 (hub-spoke: one RA serves N spokes)

## Headline

Template `endpointRef.name` through the **same** jq/extras evaluator that already renders `path`/`filter`/`payload`/`headers`, evaluated **before** the Secret lookup. One-site change (~15 LOC) at `resolveStageEndpoint`. Cache safety is **structural and already in place** (extras fold into the RA key; a spoke read-back hits the external-touch bump → Put declined → never cached). The issue's "no new trust surface" claim is **correct for the cache but WRONG for credential selection unmitigated** — extras are user query params, so a bare `${.name}` lets a caller select another user's `-clientconfig` Secret. The design's load-bearing content is therefore the **two security guardrails** (name-only V1 + reserved-suffix refusal, both enforced at two layers) and a **seed-skip** that prevents the boot seed caching a truncated body under the no-extras key. Small ship.

## 1. Mechanism (TRACED, file:line)

### 1.1 The evaluator to reuse

`path`/`payload`/`headers` are already extras/jq-templated in `createRequestOption` via `evalJQ(in.Path, ds)` (`internal/resolvers/restactions/api/setup.go:61`, and `:65`/`:72`). `evalJQ` (`setup.go:79`) runs `jqutil.MaybeQuery`-gated `jqutil.Eval` over `ds` — the per-stage data dict — with the snowplow module loader. The **same `ds`** carries the request extras as the reserved sibling key `pig["extras"]` (`internal/resolvers/restactions/api/handler.go:128`, task #10). So `.name` / `.extras.name` are already in scope for every step field **except** `endpointRef`.

`endpointRef` is the lone exception: `resolveStageEndpoint` (`resolve.go:371`) passes `apiCall.EndpointRef` **verbatim** to `r.mapper.resolveOne` (`resolve.go:387`; `resolveOne` at `internal/resolvers/restactions/api/endpoints.go:23`), which consumes `ref.Name`/`ref.Namespace` with no evaluation.

### 1.2 The change

In `resolveStageEndpoint`, for the **non-UAF, non-nil-ref** path only, evaluate `ref.Name` through `evalJQ` against the stage dict before calling `resolveOne`:

```go
// in resolveStageEndpoint, non-UAF branch, before mapper.resolveOne:
ref := apiCall.EndpointRef
if ref != nil && jqutil.MaybeQuery-shaped(ref.Name) {   // only when it's a template
    name := evalJQ(ref.Name, r.stageDict(id))            // SAME evaluator + SAME ds as path/filter
    name = kubeutil.MakeDNS1123Compatible(name)          // RFC1123-sanitize (already imported endpoints.go:54)
    // §2 guardrails applied HERE (fail-fast) — see below
    ref = &templates.Reference{Name: name, Namespace: ref.Namespace}  // namespace stays LITERAL (V1)
}
resolved, err := r.mapper.resolveOne(r.ctx, ref)
```

- The `ds` handed to `evalJQ` MUST be the SAME per-stage dict the step's `path`/`filter` see (the one carrying `pig["extras"]`) so `.name` resolves identically to how `path` already resolves it. The exact accessor is whatever `createRequestOption` is handed as `ds` for this stage; thread it into `resolveStageEndpoint` (it is a `resolveRun` method, so the dict is reachable — `r.dict`, augmented with the extras sibling key exactly as `handler.go` builds `pig`).
- `namespace` is **NOT** templated in V1 (§2 guardrail (a)) — `ref.Namespace` passes through literally.
- **Miss = honest error, unchanged posture**: a resolved name that matches no Secret → `resolveOne`'s `FromInformerSecret` soft-miss → `endpoints.FromSecret` apiserver GET → not-found error → `resolveStageEndpoint` returns `stageReturn` (`resolve.go:391`), which **truncates the resolve** exactly as a static bad `endpointRef` does today. No new error path.

### 1.3 Untouched paths (confirmed)

- **nil-ref / internal-dispatch / `<user>-clientconfig`** path (`endpoints.go:24-57`): only the **named**-ref path gains templating. A nil `EndpointRef` still takes the internal-endpoint-then-clientconfig branch verbatim. The `isInternal` SA override (`endpoints.go:83-85`, `:108-110`) is unchanged.
- **UAF** stages: `resolveStageEndpoint` short-circuits to `dynamic.ServiceAccountEndpoint()` (`resolve.go:372-385`) before any ref handling — a UAF stage never carries a per-user `endpointRef`, so templating does not touch the UAF SA-endpoint path.

## 2. Security (the two guardrails + threat walk)

**The issue's safety note is wrong unmitigated.** It claims "adds no new trust surface beyond what cluster registration already provisions." But `endpointRef` names an **endpoint Secret**, and the reserved `<user>-clientconfig` Secrets — which carry **per-user apiserver credentials** — live in the **same** `authnNS` (`endpoints.go:52-54`: `Namespace: m.authnNS, Name: <user>-clientconfig`). `extras` are **user-controlled query params** (they flow from the request URL into `pig["extras"]`). So a bare `endpointRef.name: ${ .want }` lets a caller pass `?extras.want=admin-clientconfig` and dial the spoke step **with the admin's credentials** — a credential-selection escalation. This is a genuine new trust surface and the design MUST close it.

### Guardrail (a) — V1 templates NAME only; namespace stays literal

The namespace is the trust boundary of the Secret store. Keeping `ref.Namespace` a chart-authored literal means a templated name can only ever select **within** the namespace the RA author already chose. A caller cannot redirect the lookup into `kube-system`, another tenant's namespace, or any namespace the author didn't bless. Templated namespace is a **rejected alternative** (§5) precisely because it widens this boundary. `.namespace` templating can be a V2 behind an explicit allowlist if a real need appears.

### Guardrail (b) — a templated ref may never resolve to a reserved `-clientconfig` name

This is the direct block on the escalation. The `-clientconfig` suffix is **snowplow's own internal-identity class** (the per-user credential Secrets `resolveOne` synthesizes at `endpoints.go:54`). A **templated** `endpointRef` resolving to any name ending in `-clientconfig` is refused with an honest error. This is a **general boundary, not a per-resource special-case** (`feedback_no_special_cases`): it keys on snowplow's own reserved internal-identity suffix, the same string literal the nil-ref path constructs — not on any spoke/tenant/customer resource name. A statically-authored `endpointRef` is unaffected (only the templated path is gated), because a chart author writing a literal `-clientconfig` ref is the internal path's own business; the danger is exclusively **user-extras-driven** selection.

### Placement — BOTH layers (recommended)

1. **At template-eval** (`resolveStageEndpoint`, the change site): refuse fast with a clear, greppable error the moment a templated name resolves to a `-clientconfig` suffix — so the operator sees exactly why their template was rejected, and the Secret lookup never fires.
2. **Defense-in-depth in `resolveOne`** (`endpoints.go`, the single choke point EVERY ref lookup passes): a belt-and-suspenders refusal when a **caller-templated** ref (marked as such — see below) carries the reserved suffix. This guarantees a **future** templating call site (a second caller added later) cannot bypass the boundary by forgetting the eval-site check. `resolveOne` is where the internal `-clientconfig` name is itself *constructed*, so it is the natural home for "a request-driven ref may not impersonate the internal identity class."

The two layers need a way for `resolveOne` to know the ref was **request-templated** (so a legitimate internal nil-ref→clientconfig synthesis is not refused — that path MUST still produce a `-clientconfig` name). Cleanest: the eval site passes a small flag/typed marker alongside the ref (e.g. a `templated bool` param or a `ref` origin field), so `resolveOne` refuses `-clientconfig` **only** for templated refs, never for its own internal synthesis. This keeps the internal path byte-identical.

### Threat walk (each an intended falsifier arm — §4)

- **T1 clientconfig forgery**: `?extras.name=admin` with `endpointRef.name: ${ .name + "-clientconfig" }` → resolved name `admin-clientconfig` → **REFUSED** at eval-site AND at `resolveOne` → honest error, no admin-credential dial. (Named falsifier arm — the load-bearing security proof.)
- **T2 namespace escape**: no vector in V1 — `.namespace` is literal, cannot be templated. (Guardrail (a).)
- **T3 arbitrary-secret read**: a templated name resolves to some non-`-clientconfig` Secret in the author's namespace that is NOT an endpoint Secret → `endpoints.FromSecret` returns "server-url missing" hard error (`endpoints.go:70-76`) → `stageReturn` truncate. No data leak (the Secret is never dialed; a non-endpoint Secret has no `server-url`). Bounded to the author's chosen namespace by (a).
- **T4 cross-spoke cache poisoning**: impossible — §3.

## 3. Cache safety (both hold, TRACED)

Two independent mechanisms, either sufficient; together decisive.

### 3.1 Extras fold into the RA key (partition)

The RA dispatch key is built by `dispatchCacheLookupKey` with `Extras: extras` folded in (`internal/handlers/dispatchers/helpers.go:268`). The **raw request extras** are part of the key. So `/clusters/spoke-a` (`extras.name=spoke-a`) and `/clusters/spoke-b` derive **different keys** → different cells → no cross-spoke collision, **even if** external-decline somehow didn't fire. This is the spoke-A/spoke-B **key-partition** falsifier arm.

Subscription/seed key parity note (the TL's "name any consumer where extras do NOT fold" ask): the RA request key folds request extras (`helpers.go:268`), and `DeriveSubscriptionKey` mirrors it (task #67 invariant). The seed key folds the **union** of author-declared inline maps + request extras (`helpers.go:293-319` / `effectiveKeyExtras`). For a templated-endpointRef RA the **selection** variable lives in **request** extras (`.name`), which the seed has none of — so the seed can't derive the same cell a request does. This is exactly why §3.2's external-decline makes it **moot** (the cell is never cached at all), AND why §4's seed-skip is needed (so the seed doesn't cache a *wrong* cell). There is no consumer where the omission is unsafe: the external-decline is the backstop for all of them.

### 3.2 Spoke read-back is external → Put declined (the killer)

A spoke apiserver read-back reaches the **external-touch bump** at `resolve.go:1048` (`cache.ExternalTouchedSinkFromContext(gctx).Bump()`). The comment at `resolve.go:1038-1047` is explicit and load-bearing: **the trigger is the DISPATCH SITE, not the Endpoint shape** — "mis-classifying an internal branch as external is structurally impossible because the signal is the dispatch site." A templated `endpointRef` dialing a spoke's `server-url` is a genuine external HTTP dispatch → it falls through every internal branch (`return nil` before `:1048`) → reaches the bump. Then `extTouchedSink.Count()>0` → the L1 Put surfaces **decline** the Put → spoke data is **served but never cached**, re-fetched live every `/call`.

**C-113-4 correction — the real Put-decline surface is 7 `BumpExternalSkippedPut()` sites, not "5".** As-built inventory (grep `BumpExternalSkippedPut()`): `restactions.go:361`, `widgets.go:384`, `widget_content.go:289`, `apiref/ra_full_list.go:335` + `:367`, `resolve_populate.go:275` (refresher), `phase1_pip_seed.go:1254` (seed). Of these, the ones a bare-RA `/call` (the hub-spoke shape) actually reaches are the **request-path** surfaces — `restactions.go:361` (the top-level RA cell) plus the apiref RAFullList surfaces (`ra_full_list.go`) when the RA is consumed as a widget apiRef. The `resolve_populate.go`/`phase1_pip_seed.go` sites are the refresher/seed paths (not the direct spoke `/call`), and `widgets.go`/`widget_content.go` are widget-class surfaces (reached only if a widget wraps the RA). The external-decline arm (§7.3) covers the reachable request-path set; the decline is uniform across all 7 because they all gate on the SAME `extTouchedSink.Count()>0` signal.

**Templating the name changes WHICH spoke is dialed, not WHETHER the touch is external.** So the external-decline holds identically on the templated path — the external-decline-still-fires falsifier arm (§4).

## 4. Seed-skip (with F4b nuance)

**Why skip.** The boot seed (`seedOneRestaction`, `phase1_pip_seed.go:737` → `restactions.Resolve`) runs with **no route extras**. So `evalJQ(ref.Name, ds)` yields either a jq error-string (`evalJQ` returns `err.Error()` on failure, `setup.go:92-94`) or an empty/partial name → `resolveOne` miss → `stageReturn` **truncate** (`resolve.go:391`).

**The F4b-class nuance (TRACED — this is the real reason to skip, not noise reduction).** An endpoint-resolve miss does **NOT** bump `extTouchedSink` (the external bump at `resolve.go:1048` is downstream of a successful endpoint resolve — a `stageReturn` at `:391` never reaches it) **AND** does **NOT** bump `stageErrSink` (`stageErrSink.Bump` fires only on **item-level httpcall** errors via `recordItemError` at `resolve.go:338`, not on the endpoint-resolve failure at `:391`). So `declineSeedPutOnError` (`phase1_pip_seed.go`, gates on `stageErrSink.Count()>0 || extTouchedSink.Count()>0`) would **NOT decline** — the seed could `Put` a **truncated/degraded body** under the no-extras key. That poisoned cell then serves cold on the first real `/call` until TTL. **That** is the load-bearing reason to seed-skip a templated-endpointRef RA.

**The skip predicate.** Extend the Class-5 precedent. `refHasUnresolvedTemplateToken` (`phase1_walk.go:1801`: `strings.Contains(ref.Name,"{") || strings.Contains(ref.Namespace,"{")`) already skips widget refs carrying an unsubstituted template token at the seed (`phase1_walk.go:1596`). But that check is on the **widget** `ObjectReference` — the `endpointRef` template lives on the **RA api-step** (`API.EndpointRef`, `apis/templates/v1/core.go:46`), a different surface. The new predicate: **skip seeding a RESTAction whose any api-step `EndpointRef.Name` is a jq template (`jqutil.MaybeQuery`-shaped) when the seed context carries no request extras to resolve it.** Provably **non-lossy**: these RAs are external-class (they dial a spoke apiserver → §3.2 external-decline → never cached anyway), so the seed was never going to warm a servable cell for them — skipping it removes only the poisoned-Put risk and per-pass error noise, warms nothing it could have warmed. Hook it in `seedOneRestaction` before the `restactions.Resolve` (`phase1_pip_seed.go:737`): read the fetched RA CR's `spec.api[].endpointRef.name`, and if any is template-shaped, skip with a greppable `phase1.seed.skip.templated_endpointref` line (mirrors the Class-5 skip idiom).

**F4b Lever A interplay (confirmed clean).** Because an endpoint-miss does NOT bump `extTouchedSink`, a templated-endpointRef RA would NOT be marked in the F4b engine-lived declined-external set (#132) — so absent this skip it would re-resolve every resume pass (like the pre-F4b whales, but via a *different* miss class). The seed-skip removes it from the seed set entirely, which is strictly better than relying on F4b's external-mark (which wouldn't fire for this miss class). No conflict with #132; this skip is the correct owner of the templated-endpointRef seed cost.

## 5. Rejected alternatives

- **Per-spoke RESTAction generation** (today's only option). A controller/core-provider generates one RA per registered spoke, each with a hardcoded `endpointRef`. REJECTED as the fix: it's exactly what the issue is trying to eliminate (N near-identical CRs, controller work, registration-time coupling). It also multiplies the RBAC/CRD surface. Templating collapses N RAs to 1.
- **Templated `endpointRef.namespace` (V1)**. REJECTED for V1 — it widens the credential-selection trust boundary (guardrail (a)): a templated namespace lets user extras redirect the Secret lookup into ANY namespace, re-opening the clientconfig-forgery class across namespaces and defeating guardrail (b)'s single-namespace assumption. Name-only keeps the blast radius inside the author's chosen namespace. A future V2 could allow it behind an explicit namespace-allowlist field on the RA, but that is out of scope and a bigger trust decision.
- **A new dedicated evaluator / DSL for endpointRef**. REJECTED — `feedback_no_special_cases` + reuse-first: `evalJQ`+`pig["extras"]` already renders every other step field; a second evaluator would drift from the one `path`/`filter` use. Reuse the existing path.
- **Cache the spoke read-back with a TTL** (to speed repeat spoke reads). REJECTED / out of scope — spoke data is external and un-invalidatable (no dep edge); the external-decline (§3.2) is the correct posture. If spoke-read latency becomes a concern, that's the #129 bounded-TTL-external-cache lever, not this issue.

## 6. Accepted trade-off (documented)

**One RA = one RBAC decision for all spokes.** With a single templated RA, the RBAC on the RESTAction (who may `/call` it) is evaluated once and applies to **every** spoke it can reach. Per-spoke RAs, by contrast, allow per-spoke RBAC granularity (bind user X to spoke-a's RA only). The issue accepts this implicitly ("RBAC on the RESTAction is unchanged"); making it explicit: **if a deployment needs per-spoke authorization granularity, it must keep per-spoke RAs — templating is for the common case where all reachable spokes share one authz decision.** This is a deliberate scope choice, not a defect.

## 7. Falsifiers (PM-gateable)

All hermetic (kind/unit) except where noted; the security arm is gate-blocking.

1. **T1 clientconfig-forgery REFUSED (gate-blocking, security):** a request with `extras` driving `endpointRef.name` to resolve to a `*-clientconfig` name is REFUSED at BOTH the eval-site and `resolveOne`; the spoke step never dials with the reserved-identity Secret. RED arm: remove the reserved-suffix guard → the forged name resolves → the admin-clientconfig Secret is selected → test FAILS. This is the load-bearing security proof.
2. **Spoke-A/spoke-B key-partition:** two `/call`s with `extras.name=spoke-a` vs `spoke-b` derive DIFFERENT RA cache keys (`dispatchCacheLookupKey` folds request extras). RED arm: drop extras from the key → both collapse to one cell → cross-spoke serve → FAIL.
3. **External-decline-still-fires:** a templated-endpointRef spoke read-back bumps `extTouchedSink` and the reachable L1 Put surfaces (the 7 `BumpExternalSkippedPut()` sites, §3.2 C-113-4) decline (spoke data never cached, re-fetched live). RED arm: neuter the external bump → the templated resolve caches → FAIL. Proves templating didn't dodge `resolve.go:1048`. (This arm exercises the external-touch→decline mechanism, which is dispatch-site-driven and identical for a templated vs literal external endpointRef — templating changes WHICH spoke is dialed, not WHETHER the touch is external.)
4. **Seed-skip:** the boot seed (no request extras) SKIPS a RESTAction whose api-step `endpointRef.name` is template-shaped — no `restactions.Resolve`, no Put of a truncated body, greppable skip line. RED arm: remove the skip → the seed resolves → a truncated/degraded body is Put under the no-extras key (neither sink bumped, so `declineSeedPutOnError` does NOT catch it) → FAIL. Proves the §4 nuance.
5. **Miss = honest error:** a templated name resolving to a non-existent Secret produces the SAME truncated-resolve posture as a static bad `endpointRef` (stageReturn), not a panic/500-new-class. RED arm: n/a (posture-preservation) — assert byte-equal error shape to the static-miss golden.
6. **Static ref unchanged (regression guard):** a literal (non-template) `endpointRef.name` is byte-identical to pre-change (the `MaybeQuery` gate skips evaluation) — including a literal `-clientconfig` internal ref, which is NOT refused (only templated refs are gated). Proves the internal nil-ref→clientconfig path is untouched.

**On-cluster proof (functional, when a hub-spoke deployment exists):** one `cluster-compositions` RA with `endpointRef.name: ${.name+"-endpoint"}`; `/clusters/spoke-a` dials `spoke-a-endpoint`, `/clusters/spoke-b` dials `spoke-b-endpoint`, each returns that spoke's live data; `/debug/apistage` shows both as external/uncached.

## 8. Effort

Small. ~15 LOC at `resolveStageEndpoint` (eval + sanitize + guardrails) + ~10 LOC defense-in-depth in `resolveOne` (templated-flag reserved-suffix refusal) + ~15 LOC seed-skip predicate in `seedOneRestaction`. No CRD change (`endpointRef.name` is already a free-form string; a jq template is a valid string value). No frontend change. Additive + backward-compatible: a literal `endpointRef.name` is byte-identical (the `MaybeQuery` gate). 1.7.13 candidate.
