// discovery_path_test.go — Fix A1: table coverage for the
// ParseAPIServerDiscoveryPath shape predicate, the load-bearing RBAC
// boundary for the discovery dispatch branch
// (resolvers/restactions/api/discovery_dispatch.go).
//
// The predicate MUST return true ONLY for a bare single-GroupVersion
// discovery path (/apis/<g>/<v> or /api/<v>) and FALSE for every resource
// path — that false is what structurally prevents the SA-serving discovery
// branch from leaking a resource path cross-user (PM gate AC2 / F4).

package cache

import "testing"

func TestParseAPIServerDiscoveryPath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		wantGV string
		wantOK bool
	}{
		// --- discovery shapes (ok=true) ---
		{"named group discovery", "/apis/templates.krateo.io/v1", "templates.krateo.io/v1", true},
		{"apps group discovery", "/apis/apps/v1", "apps/v1", true},
		{"core group discovery", "/api/v1", "v1", true},
		{"named group trailing slash", "/apis/templates.krateo.io/v1/", "templates.krateo.io/v1", true},
		{"core group trailing slash", "/api/v1/", "v1", true},
		{"named group with query", "/apis/apps/v1?timeout=30s", "apps/v1", true},

		// --- resource paths (ok=false) — the RBAC boundary (F4) ---
		{"group LIST", "/apis/templates.krateo.io/v1/compositiondefinitions", "", false},
		{"group GET-by-name", "/apis/templates.krateo.io/v1/compositiondefinitions/foo", "", false},
		{"group namespaced LIST", "/apis/apps/v1/namespaces/krateo-system/deployments", "", false},
		{"group namespaced GET", "/apis/apps/v1/namespaces/krateo-system/deployments/x", "", false},
		{"core LIST", "/api/v1/namespaces", "", false},
		{"core GET-by-name", "/api/v1/namespaces/krateo", "", false},
		{"core cluster LIST", "/api/v1/pods", "", false},
		{"core namespaced LIST", "/api/v1/namespaces/krateo/pods", "", false},

		// --- non-single-GroupVersion roots (ok=false) ---
		{"multi-group discovery index", "/apis", "", false},
		{"multi-group discovery index slash", "/apis/", "", false},
		{"core root", "/api", "", false},
		{"core root slash", "/api/", "", false},
		{"group version-list", "/apis/templates.krateo.io", "", false},

		// --- non-apiserver / malformed (ok=false) ---
		{"external URL", "https://example.com/apis/foo/v1", "", false},
		{"unresolved JQ", "/apis/${.group}/v1", "", false},
		{"empty", "", "", false},
		{"root slash", "/", "", false},
		{"random", "/healthz", "", false},
		{"apis empty version", "/apis/apps/", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gv, ok := ParseAPIServerDiscoveryPath(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ParseAPIServerDiscoveryPath(%q) ok = %v, want %v (gv=%q)", tc.path, ok, tc.wantOK, gv)
			}
			if gv != tc.wantGV {
				t.Fatalf("ParseAPIServerDiscoveryPath(%q) gv = %q, want %q", tc.path, gv, tc.wantGV)
			}
		})
	}
}
