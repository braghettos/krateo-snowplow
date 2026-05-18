// callpath.go — the shared `/call` query-param decoder.
//
// Ship 0.30.123 (#155): ParseCallPathToObjectRef was LIFTED VERBATIM out
// of internal/handlers/dispatchers/phase1_walk.go's parseCallPathToObjectRef
// into this leaf package so BOTH the resolver (internal/resolvers/
// restactions/api — which cannot import dispatchers, import cycle) and
// the dispatchers package can share one decoder. Pure move, zero
// behaviour change: phase1_walk.go now calls this shared function.
//
// A `/call?resource=...&apiVersion=...&name=...&namespace=...` URL is
// snowplow's own loopback endpoint shape — the frontend dispatches every
// navigation child on exactly this shape, and a RESTAction stage whose
// `path` is such a URL is a nested /call into snowplow itself. This is
// the generic /call query-param decoder — the SAME params util.ParseGVR
// / util.ParseNamespacedName read off a real HTTP request — NOT a
// hardcoded resource/path special-case (feedback_no_special_cases.md).

package util

import (
	"net/url"
	"strconv"
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

// ParseCallPathPagination extracts the `page` and `perPage` query
// parameters a `/call?...` widget endpoint carries — Ship 0.30.127.
//
// A widget's resolved resourcesRefs child Path carries page/perPage when
// the parent widget declared a `slice` (resourcesrefs/resolve.go writes
// them from spec.slice). The Phase-1 discovery walk reads them so it can
// honour the declared per-widget pagination instead of resolving every
// child unbounded.
//
// Returns ok=false when either param is absent or non-positive — the
// caller then applies its own bounded default (never the unbounded -1).
func ParseCallPathPagination(path string) (page, perPage int, ok bool) {
	u, err := url.Parse(path)
	if err != nil {
		return 0, 0, false
	}
	q := u.Query()
	p, perr := strconv.Atoi(q.Get("page"))
	pp, pperr := strconv.Atoi(q.Get("perPage"))
	if perr != nil || pperr != nil || p <= 0 || pp <= 0 {
		return 0, 0, false
	}
	return p, pp, true
}
