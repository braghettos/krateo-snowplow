// callpath.go — the shared `/call` query-param → ObjectReference decoder.
//
// #278-B (audit-clean-code-2026-06-09, Finding 3): MOVED VERBATIM out of
// internal/handlers/util into this leaf package to kill the logical
// layering back-edge `resolvers/restactions/api → handlers/util`. The
// resolver (internal/resolvers/restactions/api) needs this decoder for
// its in-process nested-/call loopback, and importing handlers/util for
// it inverted the stated handlers→resolvers direction (a future-cycle
// trap; util is a leaf so there was no Go cycle, only a logical one).
// internal/objects is the natural home: the function produces exactly the
// templatesv1.ObjectReference that objects.Get consumes, and objects is a
// clean leaf already imported by both callers (the resolver and the
// dispatchers package).
//
// Ship history before this move — Ship 0.30.123 (#155): the function was
// itself LIFTED VERBATIM out of internal/handlers/dispatchers/
// phase1_walk.go into handlers/util so the resolver and dispatchers could
// share one decoder. #278-B is the same kind of pure move, one layer
// down, with zero behaviour change.
//
// A `/call?resource=...&apiVersion=...&name=...&namespace=...` URL is
// snowplow's own loopback endpoint shape — the frontend dispatches every
// navigation child on exactly this shape, and a RESTAction stage whose
// `path` is such a URL is a nested /call into snowplow itself. This is
// the generic /call query-param decoder — the SAME params the dispatchers'
// ParseGVR / ParseNamespacedName read off a real HTTP request — NOT a
// hardcoded resource/path special-case (feedback_no_special_cases.md).

package objects

import (
	"net/url"
	"strings"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// ParseCallPathToObjectRef parses a `/call?resource=...&apiVersion=...&
// name=...&namespace=...` endpoint into the ObjectReference an
// objects.Get fetch needs. Returns ok=false for any path that is not a
// /call endpoint (external link, missing resource/apiVersion).
//
// Detection is keyed on the path SHAPE only — the trimmed path ends in
// "/call" AND both `resource` and `apiVersion` query params are present.
// No resource/name/host literal is consulted.
func ParseCallPathToObjectRef(path string) (templatesv1.ObjectReference, bool) {
	u, err := url.Parse(path)
	if err != nil {
		return templatesv1.ObjectReference{}, false
	}
	// Only a /call endpoint carries a CR. The trimmed path must end in
	// "/call" (it may be host-qualified or root-relative).
	trimmed := strings.TrimRight(u.Path, "/")
	if trimmed != "" && !strings.HasSuffix(trimmed, "/call") {
		return templatesv1.ObjectReference{}, false
	}
	q := u.Query()
	resource := q.Get("resource")
	apiVersion := q.Get("apiVersion")
	if resource == "" || apiVersion == "" {
		return templatesv1.ObjectReference{}, false
	}
	return templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      q.Get("name"),
			Namespace: q.Get("namespace"),
		},
		Resource:   resource,
		APIVersion: apiVersion,
	}, true
}
