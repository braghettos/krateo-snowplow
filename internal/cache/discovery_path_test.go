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

// TestParseAPIServerDiscoveryRoot — #74 Class 1: the bare-root sibling. ok=true
// for EXACTLY /api and /apis (the multi-group discovery indexes the
// group-version predicate rejects), false for every GroupVersion path, every
// resource path (the RBAC boundary — a root predicate that matched a resource
// path would reopen the leak), and external/${}/malformed.
func TestParseAPIServerDiscoveryRoot(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantRoot string
		wantOK   bool
	}{
		// --- the two bare roots (ok=true) ---
		{"core root", "/api", "/api", true},
		{"multi-group root", "/apis", "/apis", true},
		{"core root trailing slash", "/api/", "/api", true},
		{"multi-group root slash", "/apis/", "/apis", true},
		{"core root with query", "/api?timeout=30s", "/api", true},
		{"multi-group root with query", "/apis?x=1", "/apis", true},

		// --- GroupVersion paths (ok=false — these go to the GV predicate) ---
		{"core GroupVersion", "/api/v1", "", false},
		{"named GroupVersion", "/apis/apps/v1", "", false},
		{"group version-list", "/apis/templates.krateo.io", "", false},

		// --- resource paths (ok=false — the RBAC boundary, RED arm) ---
		{"core namespaces LIST", "/api/v1/namespaces", "", false},
		{"core GET-by-name", "/api/v1/namespaces/krateo", "", false},
		{"group namespaced GET", "/apis/apps/v1/namespaces/krateo/deployments/x", "", false},

		// --- non-apiserver / malformed (ok=false) ---
		{"external URL", "https://example.com/api", "", false},
		{"unresolved JQ", "/api${x}", "", false},
		{"empty", "", "", false},
		{"root slash", "/", "", false},
		{"random", "/healthz", "", false},
		{"apiserver-ish prefix not root", "/apiserver", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, ok := ParseAPIServerDiscoveryRoot(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ParseAPIServerDiscoveryRoot(%q) ok = %v, want %v (root=%q)", tc.path, ok, tc.wantOK, root)
			}
			if root != tc.wantRoot {
				t.Fatalf("ParseAPIServerDiscoveryRoot(%q) root = %q, want %q", tc.path, root, tc.wantRoot)
			}
		})
	}
}
