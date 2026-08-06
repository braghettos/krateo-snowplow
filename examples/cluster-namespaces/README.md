---
type: Example
title: RESTAction — list cluster namespaces
description: A RESTAction that calls the Kubernetes API server and JQ-filters the namespace list down to names, resolved via GET /call.
resource: restactions.templates.krateo.io
tags: [restaction, kubernetes-api, jq]
timestamp: 2026-08-06T00:00:00Z
---

# RESTAction: list cluster namespaces

The smallest useful `RESTAction`: one API stage against the cluster's own
`/api/v1/namespaces`, filtered to just the names. Full walkthrough (every field
explained): [example-cluster-namespaces.md](../../go/snowplow/howto/restactions/example-cluster-namespaces.md).

## Preconditions

- snowplow deployed with the `RESTAction` CRD present — a stock Krateo installer
  deploy, or the [Kind quickstart](../../go/snowplow/howto/install.md).
- A `demo-system` namespace (the installer's portal creates it; else
  `kubectl create namespace demo-system`).
- To *execute* it: a Krateo JWT for a user whose RBAC allows `get` on this
  `restactions.templates.krateo.io` resource **and** `list` on `namespaces` (snowplow
  enforces the caller's own RBAC on every stage; grants shown in the walkthrough).

## Apply

```sh
kubectl apply -f ./manifest.yaml
```

## Execute

Applying stores inert spec — the calls run when the CR is read through snowplow:

```sh
curl -s -G \
  -H "Authorization: Bearer ${KRATEO_ACCESS_TOKEN}" \
  -d 'apiVersion=templates.krateo.io/v1' \
  -d 'resource=restactions' \
  -d 'name=cluster-namespaces' \
  -d 'namespace=demo-system' \
  "http://${SNOWPLOW_HOST}:8081/call"
```

The response carries the filtered result under `status.namespaces`, e.g.
`["default", "demo-system", "kube-system", …]`.
