// phase1_walk_template_token_test.go — #74 Class 5: the prewarm/resourcesRefs
// walk must SKIP a child ref whose name/namespace still carries an
// unsubstituted widget-template token ('{'), so the literal "{name}-…" is never
// GET'd (a guaranteed 404 + ERROR log-noise). Provably non-lossy: a real K8s
// object name/namespace cannot contain '{' (RFC1123), so a '{'-bearing ref can
// never name a resource.
package dispatchers

import (
	"testing"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

func refOf(name, namespace string) templatesv1.ObjectReference {
	return templatesv1.ObjectReference{
		Reference:  templatesv1.Reference{Name: name, Namespace: namespace},
		Resource:   "tablists",
		APIVersion: "widgets.templates.krateo.io/v1beta1",
	}
}

// TestClass5_RefHasUnresolvedTemplateToken — the discriminating skip predicate.
// A ref with a '{' token (in name OR namespace) is skipped; a fully-substituted
// RFC1123 ref is walked. RED arm: if the walk did NOT test for the token, the
// literal "{name}-…" ref would be GET'd → the 404 + ERROR this fix removes.
func TestClass5_RefHasUnresolvedTemplateToken(t *testing.T) {
	cases := []struct {
		name     string
		refName  string
		refNS    string
		wantSkip bool
	}{
		// --- unsubstituted template tokens → SKIP (the Class 5 noise) ---
		{"{name} in name", "{name}-composition-tablist", "demo-system", true},
		{"{kind}-{name} in name", "{kind}-{name}-composition-tablist", "demo-system", true},
		{"{namespace} in namespace", "real-tablist", "{namespace}", true},
		{"token in both", "{name}-x", "{namespace}", true},
		{"bare brace", "a{b", "demo-system", true},

		// --- fully-substituted real refs → WALK (must NOT skip) ---
		{"real RFC1123 name", "demo-vpc-composition-tablist", "demo-system", false},
		{"real dotted name", "my.app.tablist", "krateo-system", false},
		{"empty (no token)", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := refHasUnresolvedTemplateToken(refOf(c.refName, c.refNS))
			if got != c.wantSkip {
				t.Fatalf("refHasUnresolvedTemplateToken(name=%q ns=%q) = %v, want %v "+
					"(a '{'-bearing ref must be skipped — it can never name a real K8s object; "+
					"a substituted ref must be walked)", c.refName, c.refNS, got, c.wantSkip)
			}
		})
	}
}
