# Troubleshooting — x509 "unknown authority" on a bare group-discovery api-step — 2026-06-23

**Symptom:** `Get "https://kubernetes.default.svc/apis/templates.krateo.io/v1": tls: failed to verify certificate: x509: certificate signed by unknown authority` (500). Seen on snowplow **1.1.0**.

**Trigger RA:** `test-composition-resources-19` step 2 `discovery` — `dependsOn` iterator `[.getComposition.status.managed[] | .apiVersion] | unique`, `path: ${ if . == "v1" then "/api/v1" else "/apis/" + . end }`, verb GET, `continueOnError: false`. It GETs a **bare group-discovery URL** `/apis/<group>/<version>` (no resource/name, no endpointRef) per managed apiVersion; for `templates.krateo.io/v1` → the error. `continueOnError:false` → fails the whole RA.

## Root cause (TRACED on main @ 1.4.2, plumbing v0.9.3)
A bare `/apis/<g>/<v>` path has only 2 segments → `cache.ParseAPIServerPathToDep` declines (`inventory.go:302-304`, needs ≥3 / a resource segment). So it **parse-fails every CA-bearing dispatch branch** and falls through to the external fetch:
1. informer-pivot / apistage-content (`resolve.go:705-805`) — `ParseAPIServerPathToDep` ok=false → fall through.
2. internal-rest-config SA (`resolve.go:837`) — no `WithInternalRESTConfig` on a per-user `/call` (Phase-1-only) + would parse-decline anyway → fall through.
3. cache-off in-process resolve (`resolve.go:914`) — `maybeResolveInProcess` gate 4 `!parseOK` → fall through.
4. **EXTERNAL fetch (`resolve.go:966`, `httpFetchAllowingNonJSON` external_fetch.go:126) — THE EMIT SITE.**

The external client is built from the stage Endpoint (`resolve.go:535` ← per-user `<username>-clientconfig`, ServerURL forced to `https://kubernetes.default.svc` at `endpoints.go:84/109`). That endpoint is **token-auth** (bearer JWT + `caData`, no client cert/key). plumbing `HTTPClientForEndpoint → tlsConfigFor` (`http/request/transport.go:18`) installs the CA pool **only inside the `HasCertAuth()` branch** (`transport.go:37-39` returns early with no RootCAs when `!HasCertAuth()`; `HasCertAuth` requires BOTH cert+key, `endpoints/types.go:39-40`). So for a **token-auth** endpoint the cluster `caData` is **dropped** → apiserver cert verified against the system root store → `x509: unknown authority`. (Same plumbing TLS defect `internal_dispatch.go:48-69` documents for the Phase-1 SA path; here it bites a per-user request.)

## Verdict: upgrading does NOT fix it
Byte-identical 1.1.0 → 1.4.2. The 1.1.0 path used `httpcall.Do` (same `HTTPClientForEndpoint→tlsConfigFor`); #35/ADR-0006 (1.2.0) replaced it with `httpFetchAllowingNonJSON` which **reuses the same transport verbatim**. rc/SArc threading is per-user-request-inert here. **A1+A2 (#42) is orthogonal (perf, not transport).** No version in the range resolves this — upgrading buys nothing for this error.

## Fix recommendation
**(A) snowplow — a discovery-shaped dispatch branch (~40-60 LOC).** Add a branch ahead of the external fetch keyed on path SHAPE (`/apis/<g>/<v>` or `/api/<v>`, no resource segment, GET, apiserver host) that serves group-discovery via client-go `ServerResourcesForGroupVersion` on a **CA-bearing SA transport** (reuse `cache.SARESTConfigSingleton`/`inClusterConfigFn` / `discoveryClientBuilder` — the prior art `dispatchViaInternalRESTConfig`/`plurals_resolver` already use, which fixed the Phase-1 SA x509 in 0.30.104). Discovery is identity-free / anonymous-readable → SA-serve is sound (**PM to confirm** the RBAC posture). New `ParseAPIServerDiscoveryPath` shape predicate (`cache/inventory.go`) + `dispatchViaDiscovery` sibling + one branch at `resolve.go:~705`. No special-cases (shape-keyed, not literal-keyed).
- Strategic note: there is NO in-cluster CA-bearing `rest.Config` on the per-user request ctx today (`WithInternalRESTConfig` is Phase-1-only) → the fix needs the process-wide SA rest.Config (the SA singleton). **Recommend A1 (SA-serve)** over threading the user CA into a corrected transport.

**(B) RA-author note (portal-chart, out of snowplow repo).** Step 2 does a raw discovery-URL GET purely to enumerate `.status.managed[].apiVersion` served resources — a discovery probe, not shaped widget data. Better solved via snowplow's own resolution than raw `/apis/<g>/<v>` GETs. Surface to whoever owns the composition-detail/composition-resources RA.

## Falsifier (kind / in-process, NEVER remote kubeconfig)
1. **Negative (reproduces today):** dispatch an api-step `path:/apis/templates.krateo.io/v1` GET with a token-auth (no client-cert) Endpoint carrying non-system `caData`, ServerURL = an httptest TLS server (self-signed CA) → assert served Status 500 carrying `x509: certificate signed by unknown authority`.
2. **Positive (proves A):** same dispatch after the fix → discovery branch fires, served=true, body = the `APIResourceList`, over a CA-bearing transport (httptest CA trusted, no x509). Mirror `TestInternalRESTConfigDispatch_TrustsClusterCA` (internal_dispatch_tls_test.go); real on-cluster falsifier = a `/call` of the triggering RA returning 200 with no x509 in pod logs.
</content>
