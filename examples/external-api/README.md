---
type: Example
title: RESTAction — invoke an external API
description: Two chained HTTP calls against httpbin.org through an Endpoint Secret — a GET whose response feeds a dependent POST's JQ-built payload.
resource: restactions.templates.krateo.io
tags: [restaction, endpoint, external-api, jq]
timestamp: 2026-08-06T00:00:00Z
---

# RESTAction: invoke an external API

Calls a service **other than** the Kubernetes API server, so each stage carries an
`endpointRef` to an [`Endpoint`](../../go/snowplow/howto/endpoints.md) Secret
(`server-url: https://httpbin.org`). Stage `two` depends on stage `one` and builds its
POST payload from `one`'s response with a JQ expression — the chained-call pattern.
Full walkthrough:
[example-external-api.md](../../go/snowplow/howto/restactions/example-external-api.md).

## Preconditions

- snowplow deployed with the `RESTAction` CRD present — a stock Krateo installer
  deploy, or the [Kind quickstart](../../go/snowplow/howto/install.md).
- A `demo-system` namespace (the installer's portal creates it; else
  `kubectl create namespace demo-system`).
- Outbound internet access from the snowplow pod (it is snowplow, not your shell, that
  calls `https://httpbin.org`).
- To *execute* it: a Krateo JWT for a user whose RBAC allows `get` on this
  `restactions.templates.krateo.io` resource.

## Apply

```sh
kubectl apply -f ./manifest.yaml
```

## Execute

```sh
curl -s -G \
  -H "Authorization: Bearer ${KRATEO_ACCESS_TOKEN}" \
  -d 'apiVersion=templates.krateo.io/v1' \
  -d 'resource=restactions' \
  -d 'name=httpbin' \
  -d 'namespace=demo-system' \
  "http://${SNOWPLOW_HOST}:8081/call"
```

The response's `status.one` holds the GET echo; `status.two.json.compositionID` is
`"AA-BB-CC"` — the value JQ lifted out of stage `one`'s response.
