"""Cluster I/O — kubectl wrapper + kubernetes-client helpers.

Pure cluster mutators/readers; no harness state. GKE-context guard lives
here as the module-level `gke_context_guard()`; the full preflight gate
will live in `cli.py:cmd_check` (Block 2).

Why this exists
---------------
At SCALE=50K cleanup work touches 30K+ finalizer-patches, 124K+ RBAC
objects, 29K+ ogen repoes, 17K+ Argo apps. Every kubectl(...) call forks
a subprocess (~30-100 ms wall-clock for binary load + kubeconfig parse +
TLS handshake), so even with 64 workers the bench spends hours on
cleanup. The kubernetes Python client uses ONE TLS connection and emits
direct REST calls (~5-10 ms each), giving ~10-20x speedup on the
bulk-delete hot paths.

Design
------
- Lazy init: kube-config is loaded the first time a k8s_* helper runs.
- Optional dependency: if `kubernetes` is not installed OR the cluster
  is unreachable, K8S_CLIENT_AVAILABLE stays False and callers fall
  back to kubectl(). The bench still works on a stock laptop.
- Helpers swallow 404 (NotFound) as success — bulk delete on a list
  that races with controllers regularly sees "already gone" and the
  contract is "ensure absent", not "you saw it first".
- The original kubectl() wrapper is RETAINED: rollout restart, exec,
  helm, top, etc. stay on subprocess. Only the high-fan-out cleanup
  loops migrate.
"""

from __future__ import annotations

import concurrent.futures
import os
import subprocess
import sys
import threading
import time

# ─── GKE context guard ───────────────────────────────────────────────────────

CANONICAL_GKE_CONTEXT = "gke_neon-481711_us-central1-a_cluster-1"


def gke_context_guard(allow_non_gke: bool | None = None) -> None:
    """Exit non-zero if kubectl current-context is not the canonical GKE.

    Enforces `feedback_kubectl_verify_gke_context` from the cluster module
    boundary so every bench import passes through the gate. The full
    7-item preflight gate lives in `cli.py:cmd_check` (Block 2); this is
    the cluster.py shard.

    Args:
        allow_non_gke: if True, bypass the gate. If None, read
            $BENCH_ALLOW_NON_GKE (truthy values "1", "true", "yes" bypass).

    Exits:
        3 on context mismatch (consistent with the planned CLI exit code).
        Does not exit on kubectl failure (lets callers see the subprocess
        error path); a missing kubectl returns to the caller silently so
        unit tests do not need a real binary.
    """
    if allow_non_gke is None:
        env_flag = os.environ.get("BENCH_ALLOW_NON_GKE", "0").strip().lower()
        allow_non_gke = env_flag in ("1", "true", "yes")
    if allow_non_gke:
        return
    try:
        proc = subprocess.run(
            ["kubectl", "config", "current-context"],
            capture_output=True,
            timeout=10,
        )
    except FileNotFoundError:
        # kubectl not installed → cannot enforce; let caller see this on
        # the next real kubectl call.
        return
    except subprocess.TimeoutExpired:
        return
    if proc.returncode != 0:
        # kubectl reachable but errored (no kubeconfig, etc.) — defer to
        # the caller; we do not want module import to die on a transient.
        return
    ctx = (proc.stdout.decode() or "").strip()
    if ctx != CANONICAL_GKE_CONTEXT:
        sys.stderr.write(
            f"bench: GKE context guard FAIL — current-context={ctx!r} "
            f"(want {CANONICAL_GKE_CONTEXT!r}). Set BENCH_ALLOW_NON_GKE=1 "
            f"to bypass (kind/minikube only).\n"
        )
        sys.exit(3)


# ─── kubectl wrapper ─────────────────────────────────────────────────────────

def kubectl(*args, input_data=None, timeout_secs=120):
    """Run a kubectl invocation. Returns (returncode, stdout, stderr).

    stdout/stderr are str (decoded). Timeout returns rc=1 and an error
    message in stderr to keep callers' error-handling uniform.
    """
    try:
        proc = subprocess.run(
            ["kubectl"] + list(args),
            input=input_data.encode() if input_data else None,
            capture_output=True,
            timeout=timeout_secs,
        )
        return proc.returncode, proc.stdout.decode().strip(), proc.stderr.decode().strip()
    except subprocess.TimeoutExpired:
        return 1, "", f"kubectl timed out after {timeout_secs}s"


# ─── Kubernetes-client helper layer (k8s_* prefix) ───────────────────────────

try:
    from kubernetes import client as _k8s_client_mod
    from kubernetes import config as _k8s_config_mod
    _K8S_LIB_AVAILABLE = True
except ImportError:
    _k8s_client_mod = None
    _k8s_config_mod = None
    _K8S_LIB_AVAILABLE = False

K8S_CLIENT_AVAILABLE = False  # flips True after first successful init
_k8s_core = None
_k8s_rbac = None
_k8s_custom = None
_k8s_apiext = None
_k8s_init_lock = threading.Lock()
_k8s_init_attempted = False


def _k8s_init():
    """Lazily load kubeconfig and instantiate API clients.

    Returns True on success, False if the kubernetes lib is missing or
    the cluster is unreachable. Callers can fall back to kubectl() on
    False without surprising the user.
    """
    global K8S_CLIENT_AVAILABLE, _k8s_core, _k8s_rbac, _k8s_custom
    global _k8s_apiext, _k8s_init_attempted
    if K8S_CLIENT_AVAILABLE:
        return True
    if not _K8S_LIB_AVAILABLE:
        return False
    with _k8s_init_lock:
        if K8S_CLIENT_AVAILABLE:
            return True
        if _k8s_init_attempted:
            # one-shot retry budget — don't keep re-trying load_kube_config
            # on every helper call when there is no cluster
            return False
        _k8s_init_attempted = True
        try:
            try:
                _k8s_config_mod.load_kube_config()
            except Exception:
                # Fall back to in-cluster config (when running in a pod)
                _k8s_config_mod.load_incluster_config()
            _k8s_core = _k8s_client_mod.CoreV1Api()
            _k8s_rbac = _k8s_client_mod.RbacAuthorizationV1Api()
            _k8s_custom = _k8s_client_mod.CustomObjectsApi()
            _k8s_apiext = _k8s_client_mod.ApiextensionsV1Api()
            K8S_CLIENT_AVAILABLE = True
            return True
        except Exception:
            return False


def _k8s_is_404(exc):
    """True if a kubernetes ApiException represents NotFound."""
    if not _K8S_LIB_AVAILABLE:
        return False
    return isinstance(exc, _k8s_client_mod.exceptions.ApiException) \
        and getattr(exc, "status", None) == 404


# ── Cluster-scoped RBAC ───────────────────────────────────────────────────

def k8s_list_clusterroles_by_prefix(prefix):
    """Return ClusterRole names whose metadata.name starts with `prefix`.

    Single REST list (no subprocess). Returns None if the kubernetes
    client is unavailable so callers can fall back to kubectl().
    """
    if not _k8s_init():
        return None
    items = _k8s_rbac.list_cluster_role(_request_timeout=300).items
    return [i.metadata.name for i in items
            if i.metadata.name and i.metadata.name.startswith(prefix)]


def k8s_list_clusterrolebindings_by_prefix(prefix):
    """Return ClusterRoleBinding names starting with `prefix`."""
    if not _k8s_init():
        return None
    items = _k8s_rbac.list_cluster_role_binding(_request_timeout=300).items
    return [i.metadata.name for i in items
            if i.metadata.name and i.metadata.name.startswith(prefix)]


def k8s_delete_clusterrole(name):
    if not _k8s_init():
        return False
    try:
        _k8s_rbac.delete_cluster_role(name=name, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


def k8s_delete_clusterrolebinding(name):
    if not _k8s_init():
        return False
    try:
        _k8s_rbac.delete_cluster_role_binding(name=name, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


# ── Namespace-scoped RBAC ─────────────────────────────────────────────────

def k8s_list_roles_all_ns_by_prefix(prefix):
    """Return [(ns, name), ...] for Roles cluster-wide whose name starts
    with `prefix`. Single REST list across all namespaces.
    """
    if not _k8s_init():
        return None
    items = _k8s_rbac.list_role_for_all_namespaces(
        _request_timeout=300).items
    return [(i.metadata.namespace, i.metadata.name) for i in items
            if i.metadata.name and i.metadata.name.startswith(prefix)]


def k8s_list_rolebindings_all_ns_by_prefix(prefix):
    if not _k8s_init():
        return None
    items = _k8s_rbac.list_role_binding_for_all_namespaces(
        _request_timeout=300).items
    return [(i.metadata.namespace, i.metadata.name) for i in items
            if i.metadata.name and i.metadata.name.startswith(prefix)]


def k8s_delete_role(ns, name):
    if not _k8s_init():
        return False
    try:
        _k8s_rbac.delete_namespaced_role(
            name=name, namespace=ns, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


def k8s_delete_rolebinding(ns, name):
    if not _k8s_init():
        return False
    try:
        _k8s_rbac.delete_namespaced_role_binding(
            name=name, namespace=ns, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


# ── Namespaces ────────────────────────────────────────────────────────────

def k8s_list_namespaces_by_prefix(prefix):
    """Return list of dicts {name, phase} for namespaces starting with
    `prefix`. Phase distinguishes Active from Terminating.
    """
    if not _k8s_init():
        return None
    items = _k8s_core.list_namespace(_request_timeout=300).items
    out = []
    for i in items:
        name = i.metadata.name
        if not name or not name.startswith(prefix):
            continue
        phase = (i.status.phase if i.status else None) or ""
        out.append({"name": name, "phase": phase})
    return out


def k8s_delete_namespace(name):
    if not _k8s_init():
        return False
    try:
        _k8s_core.delete_namespace(name=name, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


def k8s_create_namespace(name):
    """Idempotent: returns True if namespace exists or was created."""
    if not _k8s_init():
        return False
    try:
        body = _k8s_client_mod.V1Namespace(
            metadata=_k8s_client_mod.V1ObjectMeta(name=name))
        _k8s_core.create_namespace(body=body, _request_timeout=30)
        return True
    except Exception as e:
        if _K8S_LIB_AVAILABLE and isinstance(
                e, _k8s_client_mod.exceptions.ApiException):
            # 409 = AlreadyExists is success
            return getattr(e, "status", None) in (200, 201, 409)
        return False


# ── Custom resources (CRDs) ───────────────────────────────────────────────

def k8s_split_gvr(gvr_string):
    """Parse 'plural.group' into (group, plural). Version unknown — caller
    must supply (commonly 'v1' / 'v1beta1' / 'v1-2-2').
    """
    parts = gvr_string.split(".", 1)
    if len(parts) != 2:
        return (None, gvr_string)
    return (parts[1], parts[0])


def k8s_list_cluster_custom(group, version, plural):
    """Return [{"namespace": ..., "name": ...}, ...] for all instances
    of the given CRD across the cluster. Single REST call.
    """
    if not _k8s_init():
        return None
    try:
        resp = _k8s_custom.list_cluster_custom_object(
            group=group, version=version, plural=plural,
            _request_timeout=300)
    except Exception as e:
        if _k8s_is_404(e):
            return []
        raise
    items = resp.get("items", []) if isinstance(resp, dict) else []
    out = []
    for it in items:
        md = it.get("metadata", {}) if isinstance(it, dict) else {}
        out.append({
            "namespace": md.get("namespace", ""),
            "name": md.get("name", ""),
        })
    return out


def k8s_patch_custom_finalizers_null(group, version, plural, ns, name):
    """JSON Merge Patch metadata.finalizers=null on a custom resource."""
    if not _k8s_init():
        return False
    body = {"metadata": {"finalizers": None}}
    try:
        _k8s_custom.patch_namespaced_custom_object(
            group=group, version=version, plural=plural,
            namespace=ns, name=name, body=body,
            _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


def k8s_delete_custom(group, version, plural, ns, name):
    if not _k8s_init():
        return False
    try:
        _k8s_custom.delete_namespaced_custom_object(
            group=group, version=version, plural=plural,
            namespace=ns, name=name, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


# ── Bulk parallel helpers ─────────────────────────────────────────────────

def k8s_bulk_delete_clusterscope(kind, names, workers=64):
    """Delete N cluster-scoped objects of `kind` in parallel.

    `kind` is one of "clusterrole" | "clusterrolebinding".
    Returns count successfully deleted. Each delete is ONE REST call;
    no subprocess overhead.
    """
    if not names:
        return 0
    fn = {
        "clusterrole": k8s_delete_clusterrole,
        "clusterrolebinding": k8s_delete_clusterrolebinding,
    }.get(kind)
    if fn is None:
        return 0
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=workers) as ex:
        results = list(ex.map(fn, names))
    return sum(1 for r in results if r)


def k8s_bulk_delete_namespaced(kind, items, workers=64):
    """Delete N namespaced objects in parallel.

    `kind` is one of "role" | "rolebinding".
    `items` is a list of (ns, name).
    """
    if not items:
        return 0
    fn = {
        "role": k8s_delete_role,
        "rolebinding": k8s_delete_rolebinding,
    }.get(kind)
    if fn is None:
        return 0
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=workers) as ex:
        results = list(ex.map(lambda t: fn(t[0], t[1]), items))
    return sum(1 for r in results if r)


def k8s_bulk_patch_finalizers_null_custom(group, version, plural,
                                          items, workers=32):
    """Parallel JSON Merge Patch finalizers=null across a CRD batch.

    `items` is a list of (ns, name).
    """
    if not items:
        return 0
    def _go(t):
        return k8s_patch_custom_finalizers_null(
            group, version, plural, t[0], t[1])
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=workers) as ex:
        results = list(ex.map(_go, items))
    return sum(1 for r in results if r)


def k8s_bulk_delete_custom(group, version, plural, items, workers=32):
    """Parallel delete across a CRD batch. `items` is a list of (ns, name)."""
    if not items:
        return 0
    def _go(t):
        return k8s_delete_custom(group, version, plural, t[0], t[1])
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=workers) as ex:
        results = list(ex.map(_go, items))
    return sum(1 for r in results if r)


# ── GVR table for the CRDs the bench touches ──────────────────────────────

K8S_CRD_GVR = {
    "applications.argoproj.io":
        ("argoproj.io", "v1alpha1", "applications"),
    "repoes.git.krateo.io":
        ("git.krateo.io", "v1alpha1", "repoes"),
    "repoes.github.ogen.krateo.io":
        ("github.ogen.krateo.io", "v1alpha1", "repoes"),
    "compositiondefinitions.core.krateo.io":
        ("core.krateo.io", "v1alpha1", "compositiondefinitions"),
}


def _k8s_gvr_for(resource):
    """Resolve kubectl-style 'plural.group' to (group, version, plural).

    Returns None if unknown — callers fall back to kubectl() rather than
    guessing the version (which can vary per CRD revision).
    """
    return K8S_CRD_GVR.get(resource)


# ─── Composition + bench constants (shared across modules) ───────────────────

NS = "krateo-system"
COMPDEF_NAME = "github-scaffolding-with-composition-page"
COMP_GVR = "composition.krateo.io"
COMP_RES = "githubscaffoldingwithcompositionpages"
COMP_CONTROLLER_DEPLOY = f"{COMP_RES}-v1-2-2-controller"

FINALIZER_PATCH = '{"metadata":{"finalizers":null}}'


# ─── Cluster-state introspection helpers ─────────────────────────────────────

def _parse_ns_name(out):
    """Parse `kubectl ... -o custom-columns=NS:...,NAME:...` output into
    list of (ns, name) tuples.
    """
    return [(p[0], p[1]) for line in (out or "").splitlines()
            if (p := line.split(None, 1)) and len(p) >= 2]


def _count(resource):
    """Count all instances of a resource cluster-wide (or in cluster scope)."""
    rc, out, _ = kubectl("get", resource, "-A", "--no-headers")
    if rc != 0 or not out.strip():
        return 0
    return len([l for l in out.splitlines() if l.strip()])


def _count_match(resource, name_prefix="", ns_prefix=""):
    """Count instances of `resource` whose name and/or namespace match prefixes."""
    rc, out, _ = kubectl("get", resource, "-A", "--no-headers",
                         "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    if rc != 0 or not out.strip():
        return 0
    n = 0
    for ns, name in _parse_ns_name(out):
        if name_prefix and not name.startswith(name_prefix):
            continue
        if ns_prefix and not ns.startswith(ns_prefix):
            continue
        n += 1
    return n


def _crd_exists(crd):
    rc, _, _ = kubectl("get", "crd", crd, "--no-headers")
    return rc == 0


def _count_bench_argo():
    """Argo apps in bench-* namespaces OR with bench-app- name prefix."""
    rc, out, _ = kubectl("get", "applications.argoproj.io", "-A", "--no-headers",
                         "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    if rc != 0 or not out.strip():
        return 0
    return sum(1 for ns, name in _parse_ns_name(out)
               if ns.startswith("bench-") or name.startswith("bench-app-"))


def count_compositions():
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces", "--no-headers")
    if rc != 0 or not out.strip():
        return 0
    return len(out.strip().split("\n"))


def count_compositions_in_ns(ns_name):
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "-n", ns_name, "--no-headers")
    if rc != 0 or not out.strip():
        return 0
    return len(out.strip().split("\n"))


def count_bench_ns():
    rc, out, _ = kubectl("get", "ns", "--no-headers")
    return len([line for line in out.strip().split("\n")
                if "bench-ns-" in line and "Terminating" not in line])


def list_composition_names():
    """Return a set of "ns/name" strings for all compositions in the
    cluster via kubectl. Returns None on failure (so callers can
    distinguish from 'legitimately empty').
    """
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                         "-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}")
    if rc != 0:
        return None
    names = set()
    for line in out.strip().split("\n"):
        line = line.strip()
        if "/" in line:
            names.add(line)
    return names


# ─── Per-user visibility (SSAR-driven, RBAC-aware) ───────────────────────────


def _decode_jwt_groups(token):
    """Extract `groups` claim from a JWT (no signature verification — we
    only need the subject + groups to construct an SSAR with the SAME
    identity snowplow sees from its own context extractor).

    Returns a list of group names, or [] when the token is missing /
    malformed. Mechanism: standard JWT base64url-decode of payload.
    """
    if not token or "." not in token:
        return []
    parts = token.split(".")
    if len(parts) < 2:
        return []
    import base64 as _b64
    import json as _json
    payload_raw = parts[1]
    pad = payload_raw + "=" * (-len(payload_raw) % 4)
    try:
        payload = _json.loads(_b64.urlsafe_b64decode(pad))
    except Exception:
        return []
    g = payload.get("groups") or []
    if isinstance(g, list):
        return [str(x) for x in g]
    return []


def kubectl_auth_can_i(
    user, token, *,
    verb, resource, group=COMP_GVR,
    namespace=None, all_namespaces=False,
):
    """Probe RBAC reach for `user` via `kubectl auth can-i --as=<user>`.

    Returns True/False/None (None on probe-itself failure — caller should
    treat as conservative deny). Mechanism-uniform: works for any user/
    resource pair; never hardcodes user names or resource identifiers.

    `--as=<user>` impersonates the user via the kubeconfig admin
    credentials. When `token` is supplied, the JWT's `groups` claim is
    decoded and forwarded as `--as-group=<g>` flags so the SSAR sees the
    SAME (user, groups) identity snowplow's UserConfig context carries.
    Without group impersonation the bench would diverge from snowplow
    semantics on group-bound ClusterRoleBindings (cluster-admin group
    `admins`, narrow-RBAC group `devs`).

    The bench's local kubeconfig has impersonation rights (cluster-
    admin); this is the same mechanism the architect used in the
    2026-06-02 P2 cluster-state probes that confirmed H1.

    Returns:
        True  — verb allowed for user on (group, resource[, namespace])
        False — verb denied (no matching RoleBinding/ClusterRoleBinding)
        None  — kubectl probe itself failed (network, no impersonation
                rights, missing kubectl); callers SHOULD treat as deny.
    """
    args = ["auth", "can-i", verb, f"{resource}.{group}", f"--as={user}"]
    for grp in _decode_jwt_groups(token):
        args.append(f"--as-group={grp}")
    if all_namespaces:
        args.append("--all-namespaces")
    elif namespace:
        args.extend(["-n", namespace])
    rc, out, _ = kubectl(*args, timeout_secs=30)
    out_low = (out or "").strip().lower()
    # kubectl auth can-i exits 0 for "yes", 1 for "no", other non-zero
    # for probe errors. Out is "yes"/"no" on success cases.
    if out_low == "yes":
        return True
    if out_low == "no":
        return False
    return None


def user_visible_composition_count(
    user, token, *,
    gvr=COMP_GVR, resource=COMP_RES,
):
    """Return how many compositions `user` can list cluster-wide.

    Mechanism (matches snowplow's cache=on userAccessFilter):
      1. If `user` has cluster-wide list RBAC (SSAR --all-namespaces=yes),
         short-circuit and return the cluster total (admin path —
         bounds cost to 1 SSAR).
      2. Otherwise enumerate every namespace containing a composition
         (ground truth via kubectl), SSAR `list` per-namespace with
         `user`'s token, and count compositions in permitted namespaces.

    Pure read-only. No special cases — works uniformly for admin,
    narrow-RBAC, zero-RBAC users. Returns 0 (not None) when the user
    has no RBAC reach so caller comparisons stay numeric.

    Returns -1 on probe failure (kubectl missing, network blip) so
    the caller can distinguish "legitimately zero" from "couldn't
    measure" — the VERIFY loop treats -1 as a transient retry
    signal, same as it does for ui_count == -1.
    """
    # Step 0: cluster ground truth (admin path; doesn't need user token).
    # Uses the same jsonpath escape pattern as list_composition_names
    # (separator '/' literal; kubectl's "\\n" emits a newline).
    rc, out, _ = kubectl(
        "get", f"{resource}.{gvr}", "--all-namespaces",
        "-o", "jsonpath={range .items[*]}{.metadata.namespace}/"
              "{.metadata.name}{\"\\n\"}{end}",
    )
    if rc != 0:
        return -1
    by_ns = {}
    for line in (out or "").strip().split("\n"):
        line = line.strip()
        if "/" not in line:
            continue
        ns_name, _name = line.split("/", 1)
        by_ns.setdefault(ns_name, 0)
        by_ns[ns_name] += 1
    cluster_total = sum(by_ns.values())

    # Step 1: admin short-circuit — saves N×SSAR calls when the user has
    # cluster-wide list rights.
    can_all = kubectl_auth_can_i(
        user, token, verb="list", resource=resource, group=gvr,
        all_namespaces=True,
    )
    if can_all is True:
        return cluster_total

    # Step 2: per-namespace SSAR for narrow-RBAC users.
    permitted_total = 0
    for ns_name, count in by_ns.items():
        can_ns = kubectl_auth_can_i(
            user, token, verb="list", resource=resource, group=gvr,
            namespace=ns_name,
        )
        if can_ns is True:
            permitted_total += count
    return permitted_total


# ─── Narrow-RBAC Role provisioning (parameterized; per Diego #146 scope) ────


def _narrow_rbac_default_role_name(user, resource):
    """Default Role/RoleBinding name for a user×resource pair.

    Parameterized so the harness can provision RBAC for any user, not
    just cyberjoker. Pattern: `<user>-<resource>-reader` (lowercased).
    Wildcard `"*"` resource normalizes to "all" for shell-friendly
    kubectl naming (resource names with `*` need quoting).
    """
    resource_token = "all" if resource == "*" else resource.lower()
    return f"{user.lower()}-{resource_token}-reader"


def provision_narrow_rbac_role(
    user, ns, *,
    group=COMP_GVR, resources=("*",), verbs=("get", "list", "watch"),
    role_name=None,
):
    """Provision a namespace-scoped Role+RoleBinding granting `user`
    visibility into `(group, resources, verbs)` within `ns`.

    Mechanism-uniform: the (user, ns, group, resources, verbs) tuple is
    config-driven. The function never hardcodes user names or resource
    identifiers. Idempotent (uses `kubectl apply`).

    Used by the bench harness to model the customer-shape narrow-RBAC
    pattern (single Role in one namespace) when the test cluster's
    default state is too restrictive (zero bindings).

    Returns True on apply success, False otherwise.
    """
    name = role_name or _narrow_rbac_default_role_name(user, resources[0])
    api_groups_yaml = "[\"" + group + "\"]"
    resources_yaml = "[" + ", ".join(f'"{r}"' for r in resources) + "]"
    verbs_yaml = "[" + ", ".join(f'"{v}"' for v in verbs) + "]"
    body = f"""\
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {name}
  namespace: {ns}
rules:
- apiGroups: {api_groups_yaml}
  resources: {resources_yaml}
  verbs: {verbs_yaml}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {name}
  namespace: {ns}
subjects:
- kind: User
  name: {user}
  apiGroup: rbac.authorization.k8s.io
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {name}
"""
    rc, _, _ = kubectl("apply", "-f", "-", input_data=body)
    return rc == 0


def cleanup_narrow_rbac_role(user, ns, *, resources=("*",), role_name=None):
    """Tear down a Role+RoleBinding previously created by
    provision_narrow_rbac_role. Idempotent (--ignore-not-found).
    """
    name = role_name or _narrow_rbac_default_role_name(user, resources[0])
    kubectl("delete", "rolebinding", name, "-n", ns,
            "--ignore-not-found", "--wait=false")
    kubectl("delete", "role", name, "-n", ns,
            "--ignore-not-found", "--wait=false")


# ─── Composition deploy + YAML helpers ───────────────────────────────────────

def composition_yaml(ns, name):
    """Return the YAML body for a single bench composition."""
    return f"""\
apiVersion: composition.krateo.io/v1-2-2
kind: GithubScaffoldingWithCompositionPage
metadata:
  name: {name}
  namespace: {ns}
spec:
  argocd:
    namespace: krateo-system
    application:
      project: default
      source:
        path: chart/
      destination:
        server: https://kubernetes.default.svc
        namespace: {name}
      syncEnabled: false
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
  app:
    service:
      type: NodePort
      port: 31180
  git:
    unsupportedCapabilities: true
    insecure: true
    fromRepo:
      scmUrl: https://github.com
      org: krateoplatformops-blueprints
      name: github-scaffolding-with-composition-page
      branch: main
      path: skeleton/
      credentials:
        authMethod: generic
        secretRef:
          namespace: krateo-system
          name: github-repo-creds
          key: token
    toRepo:
      scmUrl: https://github.com
      org: krateoplatformops-test
      name: {name}
      branch: main
      path: /
      credentials:
        authMethod: generic
        secretRef:
          namespace: krateo-system
          name: github-repo-creds
          key: token
      private: false
      initialize: true
      deletionPolicy: Delete
      verbose: false
      configurationRef:
        name: repo-config
        namespace: demo-system"""


def deploy_compositiondefinition(ns="bench-ns-01"):
    """Apply the bench CompositionDefinition into `ns` and wait for Ready.

    Returns True if the CD becomes Ready within 300s, False otherwise.
    """
    yaml_str = f"""\
apiVersion: core.krateo.io/v1alpha1
kind: CompositionDefinition
metadata:
  name: {COMPDEF_NAME}
  namespace: {ns}
spec:
  chart:
    repo: {COMPDEF_NAME}
    url: https://marketplace.krateo.io
    version: 1.2.2
"""
    kubectl("apply", "--server-side", "-f", "-", input_data=yaml_str)
    deadline = time.time() + 300
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "compositiondefinitions.core.krateo.io",
                             COMPDEF_NAME, "-n", ns,
                             "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
        if rc == 0 and out.strip() == "True":
            return True
        time.sleep(5)
    return False


def ensure_composition_controller(ns):
    """Ensure the composition-dynamic-controller deployment is running in ns.

    If the deployment exists but has 0 ready replicas or is in CrashLoopBackOff,
    restart it. If it does not exist at all, log and continue (the composition
    was likely created before the controller was deployed into this namespace).
    """
    deploy = COMP_CONTROLLER_DEPLOY
    rc, out, _ = kubectl("get", f"deployment/{deploy}", "-n", ns,
                         "-o", "jsonpath={.status.readyReplicas}", "--ignore-not-found")
    if rc != 0 or not out.strip():
        rc2, _, _ = kubectl("get", f"deployment/{deploy}", "-n", ns, "--no-headers",
                            "--ignore-not-found")
        if rc2 != 0:
            return
        kubectl("rollout", "restart", f"deployment/{deploy}", "-n", ns)
        kubectl("rollout", "status", f"deployment/{deploy}", "-n", ns,
                "--timeout=60s")
        return

    ready = int(out.strip()) if out.strip().isdigit() else 0
    if ready <= 0:
        kubectl("rollout", "restart", f"deployment/{deploy}", "-n", ns)
        kubectl("rollout", "status", f"deployment/{deploy}", "-n", ns,
                "--timeout=60s")


def delete_argo_apps_in_ns(ns):
    """Delete all Argo applications in a namespace using batch operations."""
    rc, out, _ = kubectl("get", "applications.argoproj.io", "-n", ns,
                         "--no-headers", "-o", "custom-columns=NAME:.metadata.name",
                         "--ignore-not-found")
    if rc != 0 or not out.strip():
        return
    kubectl("patch", "applications.argoproj.io", "--all", "-n", ns,
            "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", "applications.argoproj.io", "--all", "-n", ns,
            "--ignore-not-found", "--wait=false")


def force_finalize_namespace(ns_name):
    """Force-finalize a stuck Terminating namespace via /finalize subresource."""
    import json as _json
    body = _json.dumps({
        'apiVersion': 'v1', 'kind': 'Namespace',
        'metadata': {'name': ns_name}, 'spec': {'finalizers': []}
    })
    kubectl("replace", "--raw", f"/api/v1/namespaces/{ns_name}/finalize",
            "-f", "-", input_data=body)


# ─── Statistics helper ───────────────────────────────────────────────────────

def pct(data, p):
    """Return the p-th percentile of `data` (a sorted-or-not list).

    Uses the nearest-rank method. Returns None for empty input.
    """
    if not data:
        return None
    s = sorted(data)
    return s[max(0, int(round(p / 100.0 * len(s))) - 1)]


def call_url(ns, name=""):
    """Build a /call URL for the bench composition GVR.

    Used by browser flows + cache invalidation probes. Keeping the URL
    template in cluster.py keeps "what /call URL do I hit" answerable
    without crossing the browser.py boundary.
    """
    url = f"/call?apiVersion={COMP_GVR}%2Fv1-2-2&resource={COMP_RES}&namespace={ns}"
    if name:
        url += f"&name={name}"
    return url


# ─── Module-import guard (idempotent) ────────────────────────────────────────

# Run the GKE guard ONCE at import time so all callers downstream are
# safe. Tests + CLI bypass via BENCH_ALLOW_NON_GKE=1.
gke_context_guard()


__all__ = [
    # Constants
    "CANONICAL_GKE_CONTEXT",
    "NS",
    "COMPDEF_NAME",
    "COMP_GVR",
    "COMP_RES",
    "COMP_CONTROLLER_DEPLOY",
    "FINALIZER_PATCH",
    "K8S_CRD_GVR",
    # Guards + wrappers
    "gke_context_guard",
    "kubectl",
    # k8s_client helpers
    "k8s_list_clusterroles_by_prefix",
    "k8s_list_clusterrolebindings_by_prefix",
    "k8s_delete_clusterrole",
    "k8s_delete_clusterrolebinding",
    "k8s_list_roles_all_ns_by_prefix",
    "k8s_list_rolebindings_all_ns_by_prefix",
    "k8s_delete_role",
    "k8s_delete_rolebinding",
    "k8s_list_namespaces_by_prefix",
    "k8s_delete_namespace",
    "k8s_create_namespace",
    "k8s_split_gvr",
    "k8s_list_cluster_custom",
    "k8s_patch_custom_finalizers_null",
    "k8s_delete_custom",
    "k8s_bulk_delete_clusterscope",
    "k8s_bulk_delete_namespaced",
    "k8s_bulk_patch_finalizers_null_custom",
    "k8s_bulk_delete_custom",
    # Counts + introspection
    "count_compositions",
    "count_compositions_in_ns",
    "count_bench_ns",
    "list_composition_names",
    # Per-user visibility (SSAR-driven, RBAC-aware)
    "kubectl_auth_can_i",
    "user_visible_composition_count",
    # Narrow-RBAC Role provisioning (parameterized; #146 scope)
    "provision_narrow_rbac_role",
    "cleanup_narrow_rbac_role",
    # Composition + namespace ops
    "deploy_compositiondefinition",
    "composition_yaml",
    "ensure_composition_controller",
    "delete_argo_apps_in_ns",
    "force_finalize_namespace",
    # Statistics
    "pct",
    "call_url",
    # Module-level state accessor (read-only flag)
    "K8S_CLIENT_AVAILABLE",
]
