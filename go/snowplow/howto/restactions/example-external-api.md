---
type: Usage
title: "RESTAction walkthrough: invoke an external API"
description: Chain two dependent RESTAction stages against an external service (HTTPBin) through an Endpoint Secret, template the second payload with jq, and execute via the snowplow /call endpoint.
resource: snowplow
tags:
  - snowplow
  - restaction
  - walkthrough
  - endpoint
timestamp: 2026-08-06T00:00:00Z
---

# Example [`RESTAction`][restactions]: Invoke external API

## Prerequisites

Before you begin with this example, make sure you have **Snowplow** installed on a Kind cluster.
Follow the guide here to set it up: [Installing `snowplow` on Kind](../install.md).


## Overview

This [`RESTAction`][restactions] calls an external service. When you want to call a service other than the Kubernetes API server, you need to specify an [`Endpoint`][endpoints]. When `endpointRef` is omitted, snowplow resolves the calling user's own `<user>-clientconfig` Secret and targets the Kubernetes API server — which is why no `endpointRef` is needed when calling Kubernetes APIs.


```sh {name=create-endpoint}
export NAMESPACE=demo-system

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Secret
type: Opaque
metadata:
  name: httpbin-endpoint
  namespace: ${NAMESPACE}
stringData:
  server-url: https://httpbin.org
EOF
```

In this example, for simplicity, we will use the HTTPBin service and call two APIs: the first with a GET request and the second, which depends on the result of the first, with a POST request. This also demonstrates how to chain multiple dependent calls.


## Example

Let's apply the RESTAction:

```sh {name=restaction-httpbin}
cat <<'EOF' | kubectl apply -f -
apiVersion: templates.krateo.io/v1
kind: RESTAction
metadata:
  name: httpbin
  namespace: demo-system
spec:
  api:
  - 
    # Identifier for the first API call
    name: one
    # GET request path with query parameters
    path: "/get?name=Alice&email=alice@example.com&age=30&uid=AA-BB-CC"
    # Reference to the external service endpoint
    endpointRef:
      name: httpbin-endpoint
      namespace: demo-system
  - 
    # Identifier for the second API call
    name: two
    # This call depends on the result of the first call
    dependsOn: 
      name: one
    # HTTP method for this call (default GET)
    verb: POST
    path: "/post"
    headers:
      - "Content-Type: application/json"
    # JSON payload using a value from the first call
    payload: '${ {compositionID: .one.args.uid} }'
    # Reference to the same external service endpoint
    endpointRef:
      name: httpbin-endpoint
      namespace: demo-system
EOF
```

## Execution Flow

When this RESTAction is executed:

1. The first API call ("one") performs a GET request to HTTPBin with query parameters (name, email, age, uid).
2. The response from the first call is stored and can be referenced by subsequent calls.
3. The second API call ("two") waits for the completion of the first call.
4. The second call performs a POST request to HTTPBin, using the 'uid' from the first call as part of its JSON payload.
5. Both calls use the same endpoint reference defined in the Kubernetes namespace.
6. The `RESTAction` completes after the second call, with all dependent data available for further processing.


To resolve the RESTAction along with all its [JQ][jq] expressions, filters, iterators, and other transformations, you need to invoke the Snowplow `/call` endpoint with the following parameters:


```sh {name=execute-restaction depends=restaction-httpbin}
# Load environment variables from the .env file
# This file contain KRATEO_ACCESS_TOKEN
source .env
export NAMESPACE=demo-system

# Send a GET request to the Snowplow /call endpoint.
curl -sv -G \
  -H "Authorization: Bearer ${KRATEO_ACCESS_TOKEN}" \
  -d 'apiVersion=templates.krateo.io/v1' \
  -d 'resource=restactions' \
  -d 'name=httpbin' \
  -d "namespace=${NAMESPACE}" \
  "http://127.0.0.1:30081/call"
```


> The _.env_ file stores the environment variables required to authenticate with Snowplow.
> In particular, it contains the _KRATEO_ACCESS_TOKEN_ minted in step 4 of the
> [installation guide](../install.md). The user's `<user>-clientconfig` Secret
> (step 5 there) must also exist — without it snowplow answers `401`.


Snowplow fetches the corresponding CR, executes all the API calls, applies filters, iterators, and jq expressions, and returns the results under the `status` field **of the HTTP response** (nothing is written back to the cluster):

```json
"status": {
    "one": {
      "args": {
        "age": "30",
        "email": "alice@example.com",
        "name": "Alice",
        "uid": "AA-BB-CC"
      },
      "headers": {
        "Accept": "application/json",
        "Accept-Encoding": "gzip",
        "Host": "httpbin.org",
        "User-Agent": "Go-http-client/2.0",
        "X-Krateo-Traceid": "tbqnqyRvg"
      },
      "url": "https://httpbin.org/get?name=Alice\u0026email=alice%40example.com\u0026age=30\u0026uid=AA-BB-CC"
    },
    "two": {
      "args": {},
      "data": "{\"compositionID\":\"AA-BB-CC\"}",
      "files": {},
      "form": {},
      "headers": {
        "Accept-Encoding": "gzip",
        "Content-Length": "28",
        "Content-Type": "application/json",
        "Host": "httpbin.org",
        "User-Agent": "Go-http-client/2.0",
        "X-Krateo-Traceid": "tbqnqyRvg"
      },
      "json": {
        "compositionID": "AA-BB-CC"
      },
      "url": "https://httpbin.org/post"
    }
  }
```

[restactions]: restactions.md
[endpoints]: endpoints.md
[jq]: https://jqlang.org/tutorial/
