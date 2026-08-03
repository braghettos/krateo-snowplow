// callpath.go — the shared `/call` query-param decoders.
//
// Ship 0.30.123 (#155): the `/call` decoders were LIFTED VERBATIM out of
// internal/handlers/dispatchers/phase1_walk.go into this leaf package so
// BOTH the resolver and the dispatchers package could share them.
//
// #278-B (audit-clean-code-2026-06-09, Finding 3): ParseCallPathToObjectRef
// has since been MOVED again, one layer down into internal/objects, to
// kill the logical layering back-edge resolvers/restactions/api →
// handlers/util (the resolver was importing this package solely for that
// decoder). See internal/objects/callpath.go. ParseCallPathPagination
// stays here — its only caller is the dispatchers package (a forward
// edge), so it carries no back-edge and there is no reason to move it.
//
// A `/call?resource=...&apiVersion=...&name=...&namespace=...` URL is
// snowplow's own loopback endpoint shape — the frontend dispatches every
// navigation child on exactly this shape. The page/perPage params are the
// SAME params the dispatchers read off a real HTTP request — NOT a
// hardcoded resource/path special-case (feedback_no_special_cases.md).

package util

import (
	"net/url"
	"strconv"
)

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
