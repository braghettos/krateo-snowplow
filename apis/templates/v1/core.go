package v1

// Reference to a named object.
type Reference struct {
	// Name of the referenced object.
	Name string `json:"name"`
	// Namespace of the referenced object.
	Namespace string `json:"namespace"`
}

// Dependency reference to the identifier of another API on which this depends
type Dependency struct {
	// Name of another API on which this depends
	Name string `json:"name"`
	// Iterator defines a field on which iterate.
	Iterator *string `json:"iterator,omitempty"`
}

// API represents a request to an HTTP service
type API struct {
	// Name is a (unique) identifier
	Name string `json:"name"`
	// Path is the request URI path
	Path string `json:"path,omitempty"`
	// Verb is the request method (GET if omitempty)
	Verb *string `json:"verb,omitempty"`
	//+listType=atomic
	// Headers is an array of custom request headers
	Headers []string `json:"headers,omitempty"`
	// Payload is the request body
	Payload *string `json:"payload,omitempty"`
	// EndpointRef a reference to an Endpoint
	EndpointRef *Reference `json:"endpointRef,omitempty"`
	// DependsOn reference to another API on which this depends
	DependsOn *Dependency `json:"dependsOn,omitempty"`

	Filter *string `json:"filter,omitempty"`

	ContinueOnError *bool `json:"continueOnError,omitempty"`

	ErrorKey *string `json:"errorKey,omitempty"`

	ExportJWT *bool `json:"exportJwt,omitempty"`

	// UserAccessFilter declares that this API call dispatches via
	// the snowplow ServiceAccount (cluster-wide read) and that the
	// returned result set MUST be in-process-refiltered through
	// EvaluateRBAC before being returned to the caller. Added at
	// Tag 0.30.9 Sub-scope A — atomic ship: when present, both
	// ServiceAccount-dispatch AND refilter take effect; there is
	// no per-mechanism toggle. Optional — RestActions without this
	// field unchanged from 0.30.8 (per-user-token dispatch).
	//
	// Per Revision 2 (binding): even with UserAccessFilter set,
	// EvaluateRBAC continues to fire on the dispatch CR itself —
	// UserAccessFilter changes WHO dispatches the inner call, NOT
	// whether the outer dispatch is RBAC-gated. The refilter step
	// also calls EvaluateRBAC per object returned by the SA call.
	UserAccessFilter *UserAccessFilterSpec `json:"userAccessFilter,omitempty"`

	// ClusterListWhenAllowed declares that this API call is eligible
	// to dispatch as a SINGLE cluster-scoped LIST against
	// /apis/<g>/<v>/<resource> (instead of a per-namespace iterator
	// fan-out) when the requesting identity holds cluster-scope
	// `list` permission on the target GVR. Added at Tag 0.30.152
	// Ship D.5.
	//
	// Permission is checked against the Ship B typed RBAC snapshot
	// (cache.RBACSnapshot) via rbac.EvaluateRBAC(ctx, opts) with
	// opts.Namespace=="" — the existing cluster-list semantics at
	// internal/rbac/evaluate.go:198-211. On a deny verdict the call
	// falls through to the existing iterator path verbatim (no
	// behavioral change for non-cluster-list users e.g. cyberjoker).
	//
	// Additionally gated on !cache.Disabled() at the resolver entry
	// (AC-D5.13) and on Ship B snapshot readiness — the
	// `useClusterList` decision must NOT execute against a nil
	// snapshot. When the cache is "removed" (CACHE_ENABLED=false),
	// the cluster-list collapse is disabled entirely; dispatch falls
	// through to the existing per-NS iterator UNCHANGED. This
	// preserves the removable-cache invariant
	// (project_caching_is_provisional).
	//
	// Default false (nil): existing RestActions are byte-identical
	// to pre-D.5. Setting true is an OPT-IN by the RA author who
	// has verified that:
	//
	//   1. The target GVR is namespace-scoped (cluster-scoped GVRs
	//      have no iterator pattern to collapse).
	//   2. A cluster-list dispatch returns the SAME object set the
	//      iterator fan-out would have aggregated. For
	//      namespace-scoped resources, the apiserver cluster-scoped
	//      LIST endpoint /apis/<g>/<v>/<resource> returns objects
	//      across all namespaces; the iterator's per-NS LIST returns
	//      the same objects partitioned by namespace.
	//   3. The widget consuming the RA's output applies any
	//      per-object narrowing through the existing serve-time
	//      RBAC gate (gateContentEnvelope at
	//      internal/resolvers/restactions/api/apistage.go:94-145),
	//      NOT through the RA's iterator shape.
	//
	// When this field is true but DependsOn.Iterator is empty, the
	// field is a no-op (there is nothing to collapse). When both
	// are set, the resolver runs §2.3's permission check and
	// selects the dispatch path.
	ClusterListWhenAllowed *bool `json:"clusterListWhenAllowed,omitempty"`
}

// UserAccessFilterSpec declares the per-object refilter contract.
// Added at Tag 0.30.9 Sub-scope A.
//
// All fields except NamespaceFrom mirror the SubjectAccessReview
// ResourceAttributes inputs (verb/group/resource); NamespaceFrom is
// a JQ expression evaluated against each returned object to derive
// the per-object namespace the refilter calls EvaluateRBAC with.
//
// Example RestAction stanza:
//
//	api:
//	- name: namespaces
//	  path: /apis/v1/namespaces
//	  endpointRef: { name: krateo-kube, namespace: krateo-system }
//	  userAccessFilter:
//	    verb: get
//	    group: ""
//	    resource: namespaces
//	    namespaceFrom: .metadata.name
//
// At dispatch time:
//   1. snowplow-SA reads the cluster-wide namespace list.
//   2. For each returned namespace, EvaluateRBAC(user, "get", "",
//      "namespaces", .metadata.name) gates whether to keep the entry.
//   3. The filtered result set is returned + cached under a key that
//      includes user_identity (so admin and cyberjoker get distinct
//      L1 entries).
type UserAccessFilterSpec struct {
	// Verb is the Kubernetes RBAC verb checked per object.
	// Required. Lower-case ("get", "list", "watch", etc.).
	Verb string `json:"verb"`
	// Group is the API group of the checked resource. Empty string
	// = core group. Required (use "" explicitly for core).
	Group string `json:"group"`
	// Resource is the plural resource name (e.g. "namespaces").
	// The STATIC resource. Required UNLESS ResourcesFrom is set — when
	// ResourcesFrom is set the resource plural set is derived at
	// dispatch time and Resource may be left empty.
	Resource string `json:"resource,omitempty"`
	// ResourcesFrom is a JQ expression evaluated ONCE against the full
	// resolve dict, yielding a []string of resource plurals — Ship
	// 0.30.129. Symmetric with NamespaceFrom (which is jq-evaluated
	// per object): ResourcesFrom lets the checked resource set itself
	// be RUNTIME-DISCOVERED rather than a static literal.
	//
	// When set, the refilter keeps a namespace iff the user can perform
	// Verb on ANY plural in the set (OR semantics) in that namespace.
	// Group stays static (a single API group). When unset, behaviour is
	// byte-identical to pre-0.30.129 — the static Resource is checked.
	//
	// Use case: compositions-get-ns-and-crd discovers the composition
	// CRD plurals at runtime in dict["crds"]; resourcesFrom evaluates
	// "[ (.crds // [])[] | .plural ]" so the per-namespace RBAC prune
	// covers exactly the discovered composition CRDs — no hardcoded
	// plural literal.
	ResourcesFrom string `json:"resourcesFrom,omitempty"`
	// NamespaceFrom is a JQ path expression evaluated against each
	// returned object to derive the per-object namespace for the
	// EvaluateRBAC call. Typical values:
	//   - ".metadata.name" when the returned objects ARE namespaces
	//     (cluster-scoped check by name, returns namespace itself).
	//   - ".metadata.namespace" when the returned objects live IN
	//     namespaces (e.g. CustomResourceDefinitions don't, but
	//     compositions do).
	//   - "" / unset when the resource is cluster-scoped and the
	//     RBAC check is also cluster-scoped (namespace="").
	NamespaceFrom string `json:"namespaceFrom,omitempty"`
}

// ObjectReference is a reference to a named object in a specified namespace.
type ObjectReference struct {
	Reference  `json:",inline"`
	Resource   string `json:"resource,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
}

// Data is a key value pair.
type Data struct {
	// Name of the data
	Name string `json:"name"`
	// Value of the data. Can be also a JQ expression.
	Value string `json:"value,omitempty"`
	// AsString if true the value will be considered verbatim as string.
	AsString *bool `json:"asString,omitempty"`
}
