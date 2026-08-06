---
type: Log
title: snowplow — log
description: Curated chronological history — notable changes, decisions and incidents; release notes stay in GitHub Releases.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [history]
timestamp: 2026-08-06T00:00:00Z
---

# Log

Curated history, newest first. Durable decisions live in
[`go/snowplow/docs/adr/`](../go/snowplow/docs/adr/); the append-only engineering
ship-log is [`go/snowplow/docs/notes/`](../go/snowplow/docs/notes/) (point-in-time,
not authoritative).

## 2026-08-06 — adopted the Krateo Documentation Standard (pilot repo)

This bundle: root `docs/` + `examples/` + thin README. The README's eight doc links had
been broken since the monorepo fold (targets moved under `go/snowplow/howto/`); the
install/operating docs still prescribed pre-migration chart and image locations — all
re-grounded in the current release reality. The deep corpus under `go/snowplow/`
(architecture, ADRs, how-tos) deliberately stays code-adjacent; this bundle maps into it.

## 2026-08-03 — 1.9.0: the monorepo fold

The separate chart repo was collapsed into `helm/` (one version line: image + both
charts ship from one tag) and the app moved into `go/snowplow/`; Go module identity
migrated to `github.com/krateo-platformops/snowplow`; CI moved to the org's shared
reusable workflows (multi-arch image build, Go checks + CRD-drift guard, security).
Lesson paid for en route: the first fold release was tagged `v1.8.0` — the `v` prefix
matches no release workflow trigger, so it shipped nothing; `1.9.0` (plain semver) is
the release that followed. Tag without `v`, always ([release](./release.md)).

## 2026-07-22/30 — 1.7.14 → 1.7.20: RBAC-correctness and boot-budget hardening

The #118 arc folded a per-user RBAC sub-generation into the resolved cache key (stale
RBAC reads could no longer serve a pre-rebinding entry), with role-rule coverage and
key-version bumps; the prewarm seed's boot budget was re-traced and raised
(`PHASE1_TIMEOUT_SECONDS=900`) so the cohort seed stops starving on large boots; boot
resume learned to reuse the discovery snapshot; an extensive feature-coverage test
suite (Waves 1+2) landed alongside.

## 2026-07-02 — 1.5.29: prewarm-gated readiness (zero-gap deploys)

`/readyz` now flips on **prewarm-complete**, not mere informer sync, and the chart
pairs it with `maxUnavailable: 0` / `maxSurge: 1` — on a rolling deploy the old warm
pod serves all traffic until the new pod is warm. Companion: the prewarm seed's
loopback auth (#57) — a projected `audience=authn` ServiceAccount token exchanged at
authn — which is why the chart hard-requires the authn CRD at install time
([usage](./usage.md)).

## 2026-06 — the cache invariants settle

The per-binding-UID L1 keying invariant ("never cohort-only" — a six-revert
retrospective), the identity-free content key with serve-time user filtering, and the
provisionality contract (`CACHE_ENABLED=false` = transparent fallback) were fixed as
ADRs 0002–0004 after the May/June cache campaign. They are the load-bearing rules every
change is reviewed against ([overview](./overview.md)).
