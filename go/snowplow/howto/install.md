# Installing `snowplow` on [Kind][kind]

> **Production deploys** use the Helm chart from this repo (`helm/snowplow`,
> published as `oci://ghcr.io/krateo-platformops/charts/snowplow`, image
> `ghcr.io/krateo-platformops/snowplow`), with `CACHE_ENABLED=true` and the
> runtime tuning described in [operating.md](operating.md) — normally via the
> Krateo installer (see [docs/usage.md](../../../docs/usage.md)). This page is a
> single-node **quickstart** for trying snowplow on a disposable [Kind][kind]
> cluster.

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

A Krateo access token is a plain HS256 JWT signed with the `JWT_SIGN_KEY` above, carrying
`username` and `groups` claims (the shape is defined by the shared auth library,
`github.com/krateo-platformops/plumbing` `jwtutil` — `CreateToken` / `Validate`). You can mint
one with nothing but Python's standard library:

```sh {name=create-krateo-user depends=create-jwt-secret}
export KRATEO_USER=cyberjoker
export KRATEO_ACCESS_TOKEN=$(python3 - "${JWT_SECRET}" "${KRATEO_USER}" <<'PYEOF'
import base64, hashlib, hmac, json, sys, time
def b64(d): return base64.urlsafe_b64encode(d).rstrip(b"=")
key, user = sys.argv[1], sys.argv[2]
now = int(time.time())
header = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
payload = b64(json.dumps({
    "username": user, "groups": ["devs"],
    "iss": "krateo.io", "sub": user,
    "iat": now, "nbf": now, "exp": now + 8 * 3600,
}).encode())
sig = b64(hmac.new(key.encode(), header + b"." + payload, hashlib.sha256).digest())
print((header + b"." + payload + b"." + sig).decode())
PYEOF
)

echo "KRATEO_USER=${KRATEO_USER}" > .env
echo "KRATEO_ACCESS_TOKEN=${KRATEO_ACCESS_TOKEN}" >> .env
```

> The `krateoctl add-user` CLI (where you have it) mints the same token via the same shared
> library — see [ADR 0001](../docs/adr/0001-decouple-authn.md). The inline mint above keeps this
> quickstart free of extra tooling. The user belongs to the `devs` group, which the RBAC below
> targets.

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

First install the CRDs snowplow needs. The `snowplow-crds` chart ships the `RESTAction` CRD;
the `authn-crds` chart ships the `serviceaccount.authn.krateo.io` CRD that the snowplow chart's
prewarm-seed allowlist CR requires at install time (a hard dependency — without it the install
fails fast on the unknown kind; see the `seedAuthn` notes in `helm/snowplow/values.yaml`):

```sh {name=install-crds depends=kind-up}
helm install snowplow-crds oci://ghcr.io/krateo-platformops/charts/snowplow-crds
helm install authn-crds oci://ghcr.io/krateo-platformops/charts/authn-crds
```

> On a bare Kind cluster (no `authn` operator running) the prewarm seed's loopback token
> exchange fails at runtime — that is fine here: the seed is best-effort warmth, not a
> correctness gate, and `/call` serves normally.

Then install `snowplow` itself. The chart serves on the single
`http` port `8081` (there is no separate probe port — see
[operating.md](operating.md)), so the NodePort maps to it:

```sh {name=install depends=install-crds}
helm install snowplow oci://ghcr.io/krateo-platformops/charts/snowplow \
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
[restactions]: restactions.md
