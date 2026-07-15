# Feature journal — #113 templated api-step endpointRef (hub-spoke)

Date: 2026-07-15
Ship: snowplow 1.7.13 candidate (PARKED, Diego-reserved merge)
Frozen SHA: c26654a190c80a42f0e9058f18d767c0aa0ab0b9 (branch fix/113-templated-endpointref, base main @330a649/1.7.12)
Design: docs/113-templated-endpointref-design-2026-07-15.md
Gate: DUAL-GATE FINAL ACCEPT (arch soundness PASS + PM final-accept, both own-hands RED re-derived)

## What

A RESTAction api-step `endpointRef.name` can now be a jq template evaluated
against REQUEST extras through the SAME `evalJQ`/extras path that already renders
`path`/`payload`/`headers`, evaluated BEFORE the Secret lookup. This collapses N
near-identical per-spoke RESTActions to ONE (extras.name selects which spoke
endpoint is dialed) — the hub-spoke case where one RA serves N registered spokes.

Three sites (~40 LOC, all in-package, no CRD or frontend change):

1. **`resolve.go` `evalEndpointRef` (eval site).** For a `MaybeQuery`-shaped
   `endpointRef.name`, evaluate it via `evalJQ` against `r.dict` — the SAME
   per-stage dict `path`/`payload` see, so `.name` resolves identically (request
   extras sit at the TOP LEVEL of `r.dict`; `pig["extras"]` is a handler-side
   FILTER construct, not what these step fields evaluate against — so the correct
   template shape is `.name`, not `.extras.name`). RFC1123-sanitize; guardrail (a)
   keeps `namespace` the author-literal (never templated in V1); guardrail (b)
   refuses a resolved `-clientconfig` name fast (the Secret is never dialed);
   returns `templated=true`.
2. **`endpoints.go` `resolveOne` (defense-in-depth).** A new `templated bool`
   param DEFAULTS false — the internal nil-ref `<user>-clientconfig` synthesis and
   static author refs pass false and are NEVER refused. The `-clientconfig`
   reserved-suffix refusal is gated on `templated` at the single choke point every
   ref lookup passes, so a FUTURE second templating caller that forgets the
   eval-site check still cannot dial a per-user credential Secret. A single-source
   `clientConfigSuffix` const is shared by the synthesis site and both guardrails.
3. **`phase1_pip_seed.go` `seedOneRestaction` (seed-skip).** Skip seeding an RA
   whose any api-step `endpointRef.name` is templated (nil-guarded per step and per
   `*Reference`): the boot seed has no request extras, so the endpoint resolve
   misses and truncates — a miss that bumps NEITHER GTTL-1 sink, so
   `declineSeedPutOnError` would NOT catch it and the seed would Put a truncated
   body under the no-extras key (poisoning it until TTL). Non-lossy: spoke reads
   are external → external-touch-declined → never cached anyway.

## Security (the load-bearing content)

extras are user-controlled query params, so a bare `endpointRef.name: ${ .name }`
folding to `admin-clientconfig` would let a caller dial the spoke step with
ANOTHER user's apiserver credentials — a credential-selection escalation. Closed
by two guardrails at TWO layers: (a) V1 templates the NAME only (namespace stays
the author-literal, bounding the blast radius to the author's chosen namespace);
(b) a templated ref may never resolve to the reserved `-clientconfig` suffix,
refused at BOTH the eval site (fail-fast) and `resolveOne` (defense-in-depth).

## Test

All arms hermetic, `-race`, `=== RUN` + PASS on the committed tree (full api +
dispatchers + cache packages green 3×):

- **C-113-1 two-layer security (gate-blocking):** T1 clientconfig-forgery refused
  at eval-site AND resolveOne INDEPENDENTLY. The "never DIALED" proof seeds the
  secrets snapshot with a forge-target `admin-clientconfig` carrying a SENTINEL
  server-url and asserts the guard never lets that sentinel be RETURNED (the
  escalation is the dial, not merely an error). Both layer REDs proven — neuter
  either guard and the forged name selects/returns the admin credential.
- **C-113-2 marker default (two directions):** internal nil-ref synthesis resolves
  unmarked; a request-templated `-clientconfig` ref is refused. RED = drop the
  templated gate.
- **C-113-3 seed-skip real path:** `hasTemplatedEndpointRef` over real CR shapes
  (8 sub-cases incl nil-step / nil-EndpointRef / nil-RA), plus the §4 both-sinks-
  zero nuance via the REAL `declineSeedPutOnError`, plus a wiring guard (the skip
  precedes `restactions.Resolve`). REDs proven.
- **C-113-5 namespace never templated; C-113-6 static/literal byte-identical**
  (incl a literal `-clientconfig`, which is the author's own business). RED = drop
  the `MaybeQuery` gate.
- **C-113-4 doc:** design §3.2 "5 Put surfaces" corrected to the real 7
  `BumpExternalSkippedPut` sites.

## Expected / actual

Additive + backward-compatible — a literal `endpointRef.name` is byte-identical
(the `MaybeQuery` gate skips evaluation). Documented trade-off: one templated RA =
ONE RBAC decision for all spokes (per-spoke authz granularity needs per-spoke RAs).

**Actual: PENDING** — parked PR (Diego-reserved merge; not tagged/cut). C-113-7
on-cluster hub-spoke functional proof DEFERRED (no spoke cluster exists; security
proven hermetically) — owed-when-deployed. Row closes on the on-deploy hub-spoke
proof (spoke-a/spoke-b dial distinct endpoints, both external/uncached at
`/debug/apistage`) once a hub-spoke deployment exists.
