# Installing `snowplow` on [Kind][kind]

> **Production deploys** use the Helm chart `braghettos/krateo-snowplow-chart`
> (chart name `krateo-snowplow`, image `ghcr.io/braghettos/krateo-snowplow`),
> with `CACHE_ENABLED=true` and the runtime tuning described in
> [operating.md](operating.md). This page is a single-node **quickstart** for
> trying snowplow on a disposable [Kind][kind] cluster.

If you have any Docker-compatible container runtime installed (including native Docker, Docker Desktop, or OrbStack), you can easily launch a disposable cluster just for this quickstart using [Kind][kind].

```sh {name=kind-up}
kind get kubeconfig >/dev/null 2>&1 || \
cat <<EOF | kind create cluster --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 30081
    hostPort: 30081
    listenAddress: "127.0.0.1"
    protocol: TCP
EOF
```

## 2. Create a namespace

Create a dedicated namespace where Snowplow and its related resources will live.

```sh {name=create-namespace depends=kind-up}
export NAMESPACE="demo-system"
kubectl create namespace ${NAMESPACE}
```

## 3. Create the JWT Secret

```sh {name=create-jwt-secret depends=create-namespace}
export JWT_SECRET=AbbraCadabbra
kubectl create secret generic jwt-sign-key \
  --from-literal=JWT_SIGN_KEY=${JWT_SECRET} -n ${NAMESPACE}
```

## 4. Create a Krateo PlatformOps User

To quickly create a Krateo PlatformOps user, install [`krateoctl`][krateoctl] and run the following command:

```sh {name=create-krateo-user depends=create-jwt-secret}
export KRATEO_USER=cyberjoker
export KRATEO_ACCESS_TOKEN=$(krateoctl add-user -k "${JWT_SECRET}" -n "${NAMESPACE}" "${KRATEO_USER}")

echo "KRATEO_USER=${KRATEO_USER}" > .env
echo "KRATEO_ACCESS_TOKEN=${KRATEO_ACCESS_TOKEN}" >> .env
```

## 5. RBACs for the Krateo PlatformOps User

After creating a new user, you must assign them a minimal set of RBAC permissions.
In this case, since we are testing [RESTActions][restactions], the user needs at least read access to this resource.
> Write, create, or delete permissions can be granted at the discretion of the cluster administrator.

Moreover, if the [RESTAction][restactions] invokes any internal cluster APIs (for example, to list other resources), the user must also have the necessary permissions to access those resources.

For now, we will grant read-only permissions on [RESTActions][restactions].
Since the user created earlier belongs to the _"devs"_ group, we will, for simplicity, assign these permissions to the entire _"devs"_ group:

```sh
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: restactions-viewer
rules:
- apiGroups:
  - templates.krateo.io
  resources:
  - restactions
  verbs:
  - get
  - list
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: restactions-viewer
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name:  restactions-viewer
subjects:
- kind: Group
  name: devs
  apiGroup: rbac.authorization.k8s.io
EOF
```


## 6. Deploy snowplow

Finally, install `snowplow` using the Helm chart. The chart serves on the single
`http` port `8081` (there is no separate probe port — see
[operating.md](operating.md)), so the NodePort maps to it:

```sh {name=install depends=create-jwt-secret}
helm install snowplow oci://ghcr.io/braghettos/krateo-snowplow-chart \
  --namespace ${NAMESPACE} \
  --set service.type=NodePort --set service.port=8081 --set service.nodePort=30081 \
  --set 'env.DEBUG=true' --set 'env.CACHE_ENABLED=true'
```

> `env.*` values are rendered into the chart's `snowplow` ConfigMap and consumed
> via `envFrom`; the container has no direct `env:` array. `CACHE_ENABLED=true`
> turns on the in-process cache path; omit it (or set `false`) for the
> transparent direct-apiserver fallback.


## 7. Wait until the `snowplow` deployment is ready

Finally, wait for the `snowplow` deployment to become **available**.
This ensures all pods are up and running before proceeding.

```sh {name=wait-for-snowplow depends=install}
kubectl wait deployment/snowplow \
  --namespace ${NAMESPACE} \
  --for=condition=available \
  --timeout=90s
```


You are now ready to move on to the next steps. From here, you can start testing the [RESTActions][restactions] to see how the different use cases work in practice. 

Experiment with creating, updating, and querying resources to get a hands-on understanding of the platform's capabilities.

## Related ADRs

- [Decoupling `authn` from `snowplow` for Testing and Operations](./decoupling-authn-from-snowplow-for-testing.md)




[kind]: https://kind.sigs.k8s.io/
[krateoctl]: https://github.com/krateoplatformops/krateoctl/releases
[restactions]: restactions.md
