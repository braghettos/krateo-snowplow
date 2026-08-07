---
type: Decision
title: "ADR 0001 — Decouple authn from snowplow for testing and operations"
description: >-
  Snowplow can be tested and operated without the authn service: its auth
  contract is a stateless HS256 JWT plus a per-user clientconfig Secret, both
  mintable/creatable by hand. The original krateoctl vehicle is gone, and one
  best-effort runtime coupling (the prewarm-seed loopback token exchange)
  has since been added.
resource: snowplow
tags:
  - adr
  - authn
  - jwt
  - testing
status: diverged
timestamp: 2026-08-06T00:00:00Z
---

# ADR 0001 — Decouple `authn` from snowplow for testing and operations

- **Status:** Accepted (2025-10-22); the tooling vehicle has diverged — see
  *What holds today* below.
- **Supersedes:** `howto/decoupling-authn-from-snowplow-for-testing.md` — that how-to is
  preserved here in ADR form; this record is now authoritative.

## Context

Snowplow depends on the [`authn`][authn] service for authentication and Bearer-token issuance.
Both run on Kubernetes with bespoke setup and CRDs, which makes isolated testing of snowplow
fragile: standing up `authn` just to exercise snowplow's `/call` path adds setup overhead,
recurring configuration breakage, and slow iteration. Authentication is a hard dependency for
every snowplow request (it carries the `UserInfo` — Username + Groups — that drives all RBAC and
per-user caching), but it should not be a hard dependency for *testing or local operation* of
snowplow.

## Original decision (2025-10-22)

Use the `krateoctl add-user` command to perform a one-time user registration and token
generation using the **shared authentication library** — the same library `authn` uses —
without requiring the `authn` service to be deployed. Because the token is minted from the
shared library, it is compatible with the real authentication flow.

## What holds today

The *architectural* decision — `authn`'s **deployment** is decoupled, its **contract** is not —
survives, but the vehicle and the exact boundary have both moved:

- **`krateoctl` is gone.** It is not published under the `krateo-platformops` org. The token it
  minted is a plain **HS256 JWT** from the shared auth library
  (`github.com/krateo-platformops/plumbing` `jwtutil` — `CreateToken` / `Validate`), so any
  tool can mint it against the `JWT_SIGN_KEY` secret; [`howto/install.md`](../../howto/install.md)
  shows a stdlib-Python inline mint that replaces the CLI.
- **Token validation is stateless.** Snowplow's auth middleware
  (`internal/handlers/middleware/userconfig.go`, a snowplow-local transcription of plumbing's
  `use.UserConfig`) calls `jwtutil.Validate(signingKey, token)` — a pure HS256 check against the
  `jwt-sign-key` secret. No call to the `authn` service is ever made to validate a request.
- **The full `/call` credential is JWT + clientconfig Secret.** After validating the JWT, the
  middleware fetches the per-user `<dns1123(username)>-clientconfig` Secret from the authn
  namespace (cache-first via the in-process Secrets snapshot, apiserver fallback; missing Secret
  → 401). In production `authn` creates that Secret at login; for authn-free operation it must
  be provided by hand alongside the minted JWT. Decoupling the *service* does not remove this
  *data* dependency.
- **One new, best-effort runtime coupling.** The prewarm seed authenticates its nested loopback
  `/call`s by exchanging snowplow's projected (audience-bound) ServiceAccount token at `authn`'s
  `/serviceaccount/login` endpoint (`internal/authn/authn.go`, ported verbatim from the cdc's
  authn client; wired in `main.go` via `URL_AUTHN`). Without a running `authn` this exchange
  fails at runtime and the seed is skipped — the seed is best-effort warmth, not a correctness
  gate, and `/call` serves normally. Additionally, the snowplow chart's prewarm-seed allowlist
  CR requires the `authn-crds` chart (the `serviceaccount.authn.krateo.io` CRD) at install time
  — a CRD dependency, not a service dependency.

## Consequences

- **Simplified testing.** Services that depend on authentication can be tested without the
  `authn` service: mint the HS256 JWT from the shared library and create the user's
  clientconfig Secret.
- **Reduced operational overhead.** No `authn` deployment needed in local or CI environments;
  the prewarm seed degrades gracefully in its absence.
- **Consistency.** Tokens are minted from the same shared library `authn` uses, so they behave
  identically to real `authn`-issued tokens (`jwtutil.Validate` is the single verifier).
- **Boundary preserved.** This decouples *deployment* of `authn`, not the *contract*: snowplow
  still consumes the same `UserInfo` shape, so the RBAC and per-user-cache invariants
  (ADR 0002 / ADR 0003) are exercised exactly as in production.

This follows microservice testing best practice — reduce inter-service coupling and lean on
the shared library boundary — while staying aligned with the production authentication
mechanism.

[authn]: https://github.com/krateo-platformops/authn
