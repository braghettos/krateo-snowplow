# How to migrate a RESTAction `/call` api-step to a direct apiserver path

**Audience:** whoever is editing RESTAction templates (portal-chart / composition-portal blueprints — braghettos fork only, NEVER upstream krateoplatformops; portal is helm-only). **Apply in a separate session.**

## Why (what changed in snowplow 1.4.x)
snowplow 1.4.x **retired the in-process `/call` loopback for RESTAction *api steps*.** Before, a stage could set `path: /call?resource=…` and snowplow would resolve the referenced `RESTAction`/`Widget` in-process. That dispatch branch is **gone**. A stage whose `path` is still a `/call?…` URL now falls through to the **external HTTP fetch** → it needs an `endpointRef` or it errors/404s (and it is **not cached**).

The supported replacement is a **direct apiserver path + `spec.api[].resolve`** (default `true`): point the stage `path` straight at the referenced CR's Kubernetes apiserver path; snowplow fetches it from the informer (cacheable, dependency-tracked) and **resolves it in-process — byte-identical to what the old `/call` returned, with no outbound round-trip.**

> ⚠️ Only the **RESTAction-stage** `/call` loopback is retired. The `/call?resource=…` URL shape is **still used by the frontend SPA and the prewarm walker** for navigation (it appears in widget `status.resourcesRefs[].path`). **Do NOT rewrite those** — only rewrite `/call` paths that appear as a `spec.api[].path` inside a RESTAction.

## Step 1 — find the stages to migrate
In the RA templates (e.g. `chart/templates/restaction.*.yaml`):
```
grep -rnE 'path:.*\/call' chart/templates/
```
Only hits where `/call` is a `spec.api[].path` value need migrating. Ignore `/call` in comments and in widget `resourcesRefs`/SPA-nav contexts.

## Step 2 — rewrite the path
`/call` query params map 1:1 to the apiserver path:

| `/call?` param | apiserver path piece |
|---|---|
| `apiVersion=<group>/<version>` | `/apis/<group>/<version>` (core group `v1` → `/api/v1`) |
| `namespace=<ns>` | `/namespaces/<ns>` (omit for cluster-scoped) |
| `resource=<plural>` | `/<plural>` |
| `name=<name>` | `/<name>` (omit for a LIST) |

**Examples**
```yaml
# BEFORE
- name: inner
  path: /call?resource=restactions&apiVersion=templates.krateo.io/v1&namespace=krateo-system&name=my-ra
# AFTER (resolve defaults to true — omit it)
- name: inner
  path: /apis/templates.krateo.io/v1/namespaces/krateo-system/restactions/my-ra
```
```yaml
# BEFORE (widget)
- name: card
  path: /call?resource=cards&apiVersion=widgets.templates.krateo.io/v1beta1&namespace=krateo-system&name=my-card
# AFTER
- name: card
  path: /apis/widgets.templates.krateo.io/v1beta1/namespaces/krateo-system/cards/my-card
```
Templated paths work the same — build the same apiserver URL with `${ … }` instead of a `/call` URL.

## Step 3 — set `resolve` correctly
- **`resolve: true` (the DEFAULT — you can omit it):** snowplow runs the fetched `RESTAction`/`Widget` through the resolver in-process and substitutes the **resolved envelope** — this is what the old `/call` returned. Use this for the normal case (you referenced another RA/widget to consume its resolved output).
- **`resolve: false`:** returns the **raw** stored CR (unresolved spec). Set this **only** if the stage was consuming the raw CR spec, not the resolved output.
- Non-`RESTAction`/`Widget` path (e.g. a ConfigMap, a composition CR): `resolve` is a no-op — the raw object is returned. No change needed.

## Step 4 — gotchas (read these)
1. **Use a SERVED apiVersion** (the RC-2 lesson, 2026-06-22). If you template the version from CRD data, select a **served** version — never `.status.storedVersions[0]` (the storage version may be `served:false`, e.g. a `vacuum` migration version → guaranteed 404 → uncacheable). Use `([.spec.versions[] | select(.served)][0]).name` (or storage-preferred-if-served). Hard-coded versions must be the served one.
2. **`&extras={…}` does NOT migrate.** If a `/call` stage passed author-templated `?extras=`, the direct-path form has no equivalent (extras are a per-request concept of the *outer* `/call`, not of a CR fetch). Such stages need individual review — if the inner RA's jq reads those extras, a direct-path fetch won't supply them. Flag these rather than blind-rewrite.
3. **RBAC is unchanged** — the in-process resolve is gated on the requesting identity being allowed to `get` the referenced CR (same as the old `/call`), and is depth-capped (cyclic refs terminate with a bounded error). Cache-off (`CACHE_ENABLED=false`) resolves the same data via the user's own token — transparent.
4. **Pagination:** a `/call?…&page=N&perPage=M` LIST becomes a direct LIST path with `?limit=&continue=` (apiserver paging). Single-CR GET-by-name has no pagination.

## Step 5 — verify (per stage)
After the change, in the snowplow pod logs for a request that hits the stage:
- **No** `/call` outbound HTTP for the inner ref; the dispatch is in-process.
- **No** `the server could not find the requested resource` and **no** `declining to cache the partial result` for that traceId (if you see these, the apiVersion is wrong — gotcha #1).
- The stage output is byte-identical to the old `/call` resolution; the result is cacheable → 2nd navigation logs `l1_hit:"hit"`.

## Safety
- Requires snowplow **≥ 1.4.1** on the cluster (the loopback is retired there; an un-migrated `/call` stage will break on 1.4.x — so migrate in lockstep with the 1.4.x rollout).
- braghettos fork only; portal changes are **helm-only** (chart template → helm upgrade), never `kubectl apply`.
- See `howto/restactions.md` (the `resolve` field) and `docs/corpus-audit-call-loopback-2026-06-22.md` (the retirement scope).
