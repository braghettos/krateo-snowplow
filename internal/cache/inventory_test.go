// inventory_test.go — Tag 0.30.92: unit coverage for the exported
// ParseAPIServerPathToGVR. The function is the load-bearing input to
// the resolver-side lazy-register hook (restactions/api/resolve.go);
// any parser regression silently disables informer registration for
// the affected GVR, which re-introduces the 0.30.91 evict_delete=0
// failure mode.

package cache

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseAPIServerPathToGVR(t *testing.T) {
	cases := []struct {
		name string
		path string
		want schema.GroupVersionResource
		ok   bool
	}{
		// /api/v1 shapes.
		{
			name: "core_namespaced_pods",
			path: "/api/v1/namespaces/demo/pods",
			want: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			ok:   true,
		},
		{
			name: "core_cluster_namespaces",
			path: "/api/v1/namespaces",
			want: schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
			ok:   true,
		},
		{
			name: "core_namespaced_single",
			path: "/api/v1/namespaces/demo/pods/foo",
			want: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			ok:   true,
		},
		// /apis/<group>/<version> shapes.
		{
			name: "apps_namespaced_deployments",
			path: "/apis/apps/v1/namespaces/demo/deployments",
			want: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			ok:   true,
		},
		{
			name: "rbac_cluster_clusterroles",
			path: "/apis/rbac.authorization.k8s.io/v1/clusterroles",
			want: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
			ok:   true,
		},
		{
			name: "composition_namespaced",
			path: "/apis/composition.krateo.io/v1/namespaces/bench/githubscaffoldingwithcompositionpages",
			want: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: "githubscaffoldingwithcompositionpages"},
			ok:   true,
		},
		{
			name: "trailing_slash_stripped",
			path: "/api/v1/namespaces/demo/pods/",
			want: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			ok:   true,
		},
		{
			name: "query_string_stripped",
			path: "/apis/apps/v1/deployments?labelSelector=foo%3Dbar",
			want: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			ok:   true,
		},
		// Non-apiserver / malformed paths.
		{
			name: "external_https",
			path: "https://api.github.com/repos/foo/bar",
			ok:   false,
		},
		{
			name: "jq_template_leak",
			path: `${ "/api/v1/namespaces/" + (.) + "/pods" }`,
			ok:   false,
		},
		{
			name: "empty",
			path: "",
			ok:   false,
		},
		{
			name: "root_slash",
			path: "/",
			ok:   false,
		},
		{
			name: "apis_no_resource",
			path: "/apis/apps/v1",
			ok:   false,
		},
		{
			name: "api_no_resource",
			path: "/api/v1",
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseAPIServerPathToGVR(c.path)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (path=%q got=%v)", ok, c.ok, c.path, got)
			}
			if !ok {
				return
			}
			if got != c.want {
				t.Fatalf("gvr mismatch: got %v want %v (path=%q)", got, c.want, c.path)
			}
		})
	}
}
