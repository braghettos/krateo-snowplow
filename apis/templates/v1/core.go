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
	// Required.
	Resource string `json:"resource"`
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
