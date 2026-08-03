# ADR 0001 — Decouple `authn` from snowplow for testing and operations

- **Status:** Accepted (2025-10-22)
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

## Decision

Use the existing [`krateoctl`][krateoctl] tool (already distributed across environments) via its
`krateoctl add-user` command. The command performs a one-time user registration and token
generation using the **shared authentication library** — the same library `authn` uses — without
requiring the `authn` service to be deployed.

Developers and admins obtain a valid user and a Bearer token directly from the CLI, then call
snowplow with that token. Because the token is minted from the shared library, it is compatible
with the real authentication flow.

## Consequences

- **Simplified testing.** Services that depend on authentication can be tested without `authn`.
- **Reduced operational overhead.** No `authn` deployment needed in local or CI environments.
- **Consistency.** The CLI reuses the shared auth library, so tokens behave identically to those
  issued by the real `authn` service.
- **Admin utility.** The same command lets administrators bootstrap or manage users quickly.
- **Boundary preserved.** This decouples *deployment* of `authn`, not the *contract*: snowplow
  still consumes the same `UserInfo` shape, so the RBAC and per-user-cache invariants
  (ADR 0002 / ADR 0003) are exercised exactly as in production.

This follows microservice testing best practice — reduce inter-service coupling and lean on
existing operational tools — while staying aligned with the production authentication mechanism.

[authn]: https://github.com/krateoplatformops/authn
[krateoctl]: https://github.com/krateoplatformops/krateoctl/releases
