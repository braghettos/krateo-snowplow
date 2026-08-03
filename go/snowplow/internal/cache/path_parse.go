// path_parse.go — Ship 0.5 / 0.30.223 — pure-string parser for apiserver
// paths. Relocated from the pre-v6 CRD-watch file (deleted under v6).
//
// The functions here have NO informer side-effects and NO apiserver
// hops; they live in cache because that is the only package where they
// are consumed (phase1_walk.go's lazy-register hook + the new
// discovery_lookup.go).

package cache

import "strings"

// ExtractAPIServerGroupFromTemplatedPath extracts the static apiserver
// GROUP from a (possibly JQ-templated) apiserver path. Unlike
// ParseAPIServerPathToGVR — which rejects any path containing `${` —
// this tolerates templated version/namespace/resource segments because
// the GROUP segment is always static:
//
//	/apis/<group>/<version>/...        -> <group>   (named group)
//	/apis/<group>/${.v}/namespaces/... -> <group>   (templated version OK)
//	/api/v1/...                        -> ""        (core group, ignored)
//
// Returns ("", false) for core-group paths, external endpoints, paths
// whose group segment itself is templated (`/apis/${...}/...`), or any
// non-apiserver shape. A templated GROUP segment is deliberately
// rejected — we cannot know the group statically, and admitting a
// `${...}` literal as a group would corrupt the navigation-discovered
// group set.
func ExtractAPIServerGroupFromTemplatedPath(path string) (string, bool) {
	// Strip a leading `${ "..." }` wrapper if the whole path is one JQ
	// string expression — take the first quoted apiserver-looking
	// fragment. We only need the /apis/<group> prefix to survive.
	if i := strings.Index(path, "/apis/"); i >= 0 {
		rest := path[i+len("/apis/"):]
		// The group is everything up to the next '/'.
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			return "", false
		}
		group := rest[:slash]
		// Reject a templated group segment — cannot key the set on it.
		if group == "" || strings.Contains(group, "${") || strings.Contains(group, "\"") {
			return "", false
		}
		return group, true
	}
	return "", false
}

// IsTemplatedAPIServerPath reports whether path is a templated
// apiserver path (contains a `${...}` segment). Convenience predicate
// for callers that want to distinguish "static-resolvable GVR" from
// "templated, need group-only extraction".
func IsTemplatedAPIServerPath(path string) bool {
	return strings.Contains(path, "${")
}
