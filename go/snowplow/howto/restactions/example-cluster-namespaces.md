# Example [`RESTAction`][restactions]: list _cluster-namespaces_

## Prerequisites

Before you begin with this example, make sure you have **Snowplow** installed on a Kind cluster.
Follow the guide here to set it up: [Installing `snowplow` on Kind](howto/install.md).

## Overview

This `RESTAction` retrieves a list of Kubernetes namespaces by calling the Kubernetes API server at the `/api/v1/namespaces` endpoint.

It then applies a [JSONPath/JQ-like][jq] filter to extract only the namespace names (`.metadata.name`) from the API response.

The result can be consumed by other Krateo resources or displayed in the Karateo PlatformOps UI, depending on configuration.

## Example

Let's apply the RESTAction:

```sh {name=restaction-cluster-namespaces}
export NAMESPACE=demo-system

cat <<EOF | kubectl apply -f -
apiVersion: templates.krateo.io/v1
kind: RESTAction
metadata:
  annotations:
    "krateo.io/verbose": "true"
  name: cluster-namespaces
  namespace: ${NAMESPACE}
spec:
  api:
  - name: namespaces
    path: "/api/v1/namespaces?limit=15"
    filter: "[.namespaces.items[] | .metadata.name]"
    continueOnError: true
    errorKey: namespacesError
EOF
```

To check if the RESTAction is installed:

```sh
kubectl get restactions -n ${NAMESPACE}
```

## `spec` Explanation


The `spec` section defines **what API calls** this action will execute.

### **spec.api**

A list of API requests to be executed.

Each entry in the list describes one HTTP request with optional chaining, filtering, and error handling.

#### **api[].name**

* **Value:** `namespaces`
  The unique name of this API request within the RESTAction.
  In addition, this value also represents the key under which the results of the API call will be stored after applying any defined filters or JQ expressions. These results are placed by Snowplow into the resource’s status field, under that same key.

#### **api[].path**

* **Value:** `/api/v1/namespaces?limit=15`
  The HTTP path of the API endpoint.
  Here it targets the Kubernetes API for listing namespaces, limited to 15 results.

#### **api[].filter**

* **Value:** `[.namespaces.items[] | .metadata.name]`
  A filter [jq][jq] expression used to extract only the namespace names from the API response.
  For example, if the raw response is:

  ```json
  {
    "items": [
      {"metadata": {"name": "default"}},
      {"metadata": {"name": "kube-system"}}
    ]
  }
  ```

  The filter would produce:

  ```json
  ["default", "kube-system"]
  ```

#### **api[].continueOnError**

* **Value:** `true`
  Indicates that execution should continue even if this API call fails.
  This allows workflows to proceed without halting on non-critical errors.

#### **api[].errorKey**

* **Value:** `namespacesError`
  A key under which any error message or exception will be stored in the result, useful for debugging or referencing errors in dependent steps.
  In addition, this value also represents the key under which Snowplow will place the error details in the resource’s status field.


## Execution Flow

When this RESTAction is executed:

1. It sends an HTTP `GET` request to the cluster’s `/api/v1/namespaces?limit=15` endpoint.
2. It retrieves the list of namespaces (up to 15).
3. The response is processed using the `filter` to return only the namespace names.
4. If the request fails, the error is stored under the `namespacesError` key, but the workflow continues due to `continueOnError: true`.

## Executing the RESTAction

Since this RESTAction calls an internal Kubernetes API — specifically, the one that lists all namespaces — the Krateo user (created during the Snowplow installation process) also needs the appropriate RBAC permissions for that resource.

```sh 
cat <<EOF | kubectl apply -f -
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: namespaces-viewer
rules:
- apiGroups:
  - ''
  resources:
  - namespaces
  verbs:
  - get
  - list
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: namespaces-viewer
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name:  namespaces-viewer
subjects:
- kind: Group
  name: devs
  apiGroup: rbac.authorization.k8s.io
EOF
```

Because this user belongs to the devs group, we will conveniently assign these permissions to the entire devs group.

To resolve the RESTAction along with all its [JQ][jq] expressions, filters, iterators, and other transformations, you need to invoke the Snowplow `/call` endpoint with the following parameters:

```sh {name=execute-restaction-cluster-namespaces depends=restaction-cluster-namespaces}
# Load environment variables from the .env file
# This file contain KRATEO_ACCESS_TOKEN
source .env
export NAMESPACE=demo-system

# Send a GET request to the Snowplow /call endpoint.
curl -sv -G \
  -H "Authorization: Bearer ${KRATEO_ACCESS_TOKEN}" \
  -d 'apiVersion=templates.krateo.io/v1' \
  -d 'resource=restactions' \
  -d 'name=cluster-namespaces' \
  -d "namespace=${NAMESPACE}" \
  "http://127.0.0.1:30081/call"
```

> The _.env_ file stores the environment variables required to authenticate with Snowplow.
> In particular, it contains the _KRATEO_ACCESS_TOKEN_, which you obtained during the Snowplow [installation][install.md] process when > you created a user using the `krateoctl add-user` command, as [explained in the previous guide][install.md].

Snowplow will fetch the corresponding CR, execute all the API calls, apply filters, iterators, and JQ expressions, and store the results in the resource's `status` field.

```json
{
  "status": {
    "namespaces": [
      "default",
      "demo-system",
      "kube-node-lease",
      "kube-public",
      "kube-system",
      "local-path-storage"
    ]
  }
}
```

[restactions]: restactions.md
[jq]: https://jqlang.org/tutorial/
