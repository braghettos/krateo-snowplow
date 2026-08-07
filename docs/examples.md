---
type: ExampleIndex
title: snowplow — examples
description: Runnable RESTAction examples under examples/, each paired with a README stating preconditions and the one apply command.
resource: oci://ghcr.io/krateo-platformops/charts/snowplow
tags: [examples, restaction]
timestamp: 2026-08-06T00:00:00Z
---

# Examples

Each example is a runnable manifest + a README with preconditions and the one
`kubectl apply` command. Both work against a stock Krateo installer deploy (they use
the portal's `demo-system` namespace) and are executed through snowplow's `GET /call`
([api](./api.md)).

- [cluster-namespaces](../examples/cluster-namespaces/README.md) — the smallest useful
  `RESTAction`: list the cluster's namespaces via the Kubernetes API, JQ-filtered to
  names.
- [external-api](../examples/external-api/README.md) — call an external service
  (httpbin.org) through an `Endpoint` Secret, chaining a GET into a dependent POST
  whose payload is JQ-built from the first response.

The line-by-line walkthroughs these were distilled from live with the code:
[example-cluster-namespaces.md](../go/snowplow/howto/restactions/example-cluster-namespaces.md),
[example-external-api.md](../go/snowplow/howto/restactions/example-external-api.md).
