# Contributing to snowplow

Thanks for working on snowplow. This is the fork's contributor contract: how to build and run it
locally, the one test you must never run carelessly, the review gate, how releases ship, and the
invariants a change must keep. Start from [`ARCHITECTURE.md`](ARCHITECTURE.md) — it is the canonical
map and every rule below points back into it.

House rule for docs and claims: snowplow is **data-driven and code-traced**. Verify every claim
against the current tree at `file:line` before you write it down; if a note disagrees with the code,
the code wins.

## Build & run locally

snowplow runs in a local [kind](https://kind.sigs.k8s.io/) cluster, built with
[`ko`](https://ko.build/) straight into the cluster's image store. The scripts in `scripts/` are the
canonical workflow — prefer them over the longhand in [`howto/developer-guide.md`](howto/developer-guide.md)
(see [Known drift](#known-drift)).

```sh
scripts/kind-up.sh      # create the kind cluster (maps host ports 30081 + 30082)
scripts/reboot.sh       # full cycle: kind-down → kind-up → build → jq ConfigMap → kubectl apply -f manifests/
scripts/reload.sh       # fast inner loop: rebuild image + re-apply manifests/deploy.snowplow.yaml
scripts/build.sh        # ko build into kind.local only, then list loaded node images
scripts/kind-down.sh    # delete the cluster
```

What each does, grounded in the tree:

- `scripts/kind-up.sh` is idempotent (`kind get kubeconfig … || kind create cluster`) and exposes
  **both** `30081` and `30082` on `127.0.0.1`.
- `scripts/build.sh` runs `KO_DOCKER_REPO=kind.local ko build --base-import-paths .` (build config in
  [`.ko.yaml`](.ko.yaml)) — no external registry, no push.
- `scripts/reboot.sh` is the clean-slate path; `scripts/reload.sh` is the iterate path. Both deploy
  from [`manifests/`](manifests/) (`manifests/deploy.snowplow.yaml`), not from inline heredocs.
- `scripts/jqmodule-to-configmap.sh` loads the custom `jq` modules snowplow expects at runtime.

### Codegen (CRDs)

CRD manifests under [`crds/`](crds/) are generated, never hand-edited. The single uniform entry point
is the [`Makefile`](Makefile):

```sh
make generate           # go mod tidy + go generate ./...  → regenerates crds/
```

`go generate` drives the build-tagged `controller-gen` directive in
[`apis/generate.go:21`](apis/generate.go), pinned to the `sigs.k8s.io/controller-tools` version in
[`go.mod`](go.mod). For CI you can use the drift gate directly:

```sh
scripts/gen.sh --check  # regenerate into a scratch tree; fail if committed crds/ differs
```

Stage 1 of the CRD pipeline (Go type → `crds/`) is a hard CI gate: a stale or hand-edited `crds/` is
a build failure. After changing `apis/templates/v1/*` types, run `make generate` and commit the
regenerated `crds/`.

## Testing trap (hard rule)

**Never run `go test ./internal/rbac/...` against a remote kubeconfig.**

The `TestMain` in [`internal/rbac/rbac_test.go:106`](internal/rbac/rbac_test.go) **creates and tears
down CRDs on whatever cluster the current kubeconfig points at**. If it picks up your default
kubeconfig (e.g. a production GKE), it garbage-collects every RESTAction CR cluster-wide. The file
ships two safety guards (an `RBAC_TEST_ALLOW_DESTRUCTIVE=1` opt-in and a production-kubeconfig
deny-list), but do not rely on them — treat the rule as the contract.

For normal work:

- **Keep `KUBECONFIG` unset for unit runs.** The non-destructive RBAC tests live in the
  [`internal/rbac/evaltest`](internal/rbac/evaltest/) sub-package (it has **no** `TestMain` and needs
  no cluster). Run those, or `go test ./...` with `KUBECONFIG` unset — the destructive `TestMain`
  exits 0 and skips itself without the opt-in flag, so the rest of the suite still runs.
- If you genuinely need the destructive cluster-backed RBAC tests, run them **only against a
  throwaway kind cluster**, exactly as the guard message instructs:

  ```sh
  kind create cluster --name rbac-test
  KUBECONFIG=$(kind get kubeconfig --name rbac-test) \
    RBAC_TEST_ALLOW_DESTRUCTIVE=1 \
    go test ./internal/rbac/...
  ```

## Review gate

Before any change is committed or merged it passes a two-role review:

- **Architect** — design soundness: does the change respect the layering and cache invariants below,
  and the architecture in [`ARCHITECTURE.md`](ARCHITECTURE.md)?
- **PM** — acceptance + falsifier: is there a concrete acceptance criterion, and a test or check that
  would *fail* if the change were wrong?

A change without an architect sign-off on design and a PM falsifier does not merge.

## Release process

snowplow and its Helm chart are **separate repos that ship in lockstep**.

1. **Tag the code repo** — `braghettos/krateo-snowplow` (this repo; confirm with `git remote -v` →
   `origin https://github.com/braghettos/krateo-snowplow.git`). The image is built and published from
   the tag.
2. **Lockstep-tag the chart repo** — `braghettos/krateo-snowplow-chart`, matching the code tag.
3. **Reconcile via `helm upgrade`** against the new chart version. **Never** `kubectl set image` or
   otherwise mutate the running Deployment out of band — the chart is the source of truth and an
   in-place image bump drifts the cluster from the chart.

Repo-name accuracy is critical:

- The chart repo's `origin` remote points at **upstream** (`krateoplatformops/*`). Push the
  braghettos fork remote **explicitly** — do not push tags to `origin` blindly.
- **Never push to `krateoplatformops/*`.** This is the braghettos fork only, for both repos.

## Invariants a change must not break

These are load-bearing. Read the linked deep dives before touching the subsystem.

- **Resolver layering contract** — a `RESTAction` emits *unordered* data; the **widget**
  canonicalizes and shapes it. RESTAction carries **no widget-shaping logic**; equivalence is
  asserted at widget props. See [`ARCHITECTURE.md`](ARCHITECTURE.md) →
  [`docs/architecture/request-lifecycle.md`](docs/architecture/request-lifecycle.md).

- **Per-binding-UID L1 keying, never cohort-only** — the identity-bound L1 key folds the first-match
  RBAC `BindingUID`, never a cohort/group set alone. Cohort-only keying leaks one user's resources to
  another (a 6-revert retrospective). See
  [`docs/architecture/rbac-uaf.md`](docs/architecture/rbac-uaf.md) and
  [`docs/architecture/caching.md`](docs/architecture/caching.md).

- **Cache toggle-ability** — all caching is **provisional and cleanly removable**.
  `CACHE_ENABLED=false` ([`internal/cache/cache.go:37`](internal/cache/cache.go) `Disabled()`) is a
  **transparent fallback** to the direct apiserver: same data, same UI, same RBAC, just slower — not
  a degraded mode. Caching must never constrain the RESTAction/widget contract. See
  [`docs/architecture/caching.md`](docs/architecture/caching.md).

- **No hardcoded special-cases** — never special-case a specific path, resource kind, or user in the
  resolver/cache/RBAC code. Express new behavior as **additive CRD fields** on the
  `RESTAction`/`Widget` types (`apis/templates/v1/`) so it stays data-driven.

When in doubt, trace it: confirm the behavior at `file:line` in the current tree, and cross-check it
against [`ARCHITECTURE.md`](ARCHITECTURE.md) and the relevant doc under
[`docs/architecture/`](docs/architecture/).

## Known drift

[`howto/developer-guide.md`](howto/developer-guide.md) predates the current scripts/manifests
workflow and is kept as a reference walkthrough, not the canonical path. Where it disagrees with the
tree, the tree wins:

- It documents a manual inline-heredoc deploy; the canonical deploy is
  `kubectl apply -f manifests/` via `scripts/reboot.sh` / `scripts/reload.sh`.
- Its kind config maps only `30081`; `scripts/kind-up.sh` maps **both** `30081` and `30082`.
- It predates `make generate` / `scripts/gen.sh`; use those for codegen.
