//go:build unit || integration

package restactions

// httpfixture_hygiene_test.go — B (hermetify) harness hygiene guard.
//
// The hermetic gate's "zero live egress" property depends on TWO things:
//   1. the per-test Setup rewriting each formerly-live endpointRef Secret's
//      server-url to the local fixtureSrv.URL() (restactions_test.go), and
//   2. the on-disk fixture Secrets NOT already pointing at a loopback address.
//
// (2) is the subtle one: if a fixture Secret were authored with a
// 127.0.0.1/localhost server-url, the rewrite would be a no-op-looking success
// AND the hit-counter falsifier could pass even if the Setup rewrite silently
// broke — masking a real live-egress regression. This guard asserts the authored
// server-urls are the human-readable EXTERNAL placeholders (api.github.com /
// httpbin.org / jsonplaceholder.typicode.com), i.e. exactly the hosts that MUST
// be redirected. It reads the YAML fixtures textually (no cluster, unit-tagged)
// so it runs in the PR gate cheaply and fails loudly if a fixture drifts toward
// a loopback URL that would defeat the falsifier.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationFixtureSecretsAreNonLoopback(t *testing.T) {
	// The three formerly-live RA fixtures whose endpoint Secrets the hermetic
	// Setup rewrites. Each maps to the external placeholder host its authored
	// server-url MUST contain (and, by construction, must NOT be a loopback).
	cases := map[string]string{
		"github.yaml":   "api.github.com",
		"httpbin.yaml":  "httpbin.org",
		"typicode.yaml": "jsonplaceholder.typicode.com",
	}

	dir := filepath.Join("..", "..", "..", "testdata", "restactions")
	loopbackMarkers := []string{"127.0.0.1", "localhost", "::1"}

	for file, wantHost := range cases {
		file, wantHost := file, wantHost
		t.Run(file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatalf("read fixture %s: %v", file, err)
			}
			body := string(b)

			// Locate the server-url line(s).
			var serverURLs []string
			for _, line := range strings.Split(body, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "server-url:") {
					serverURLs = append(serverURLs,
						strings.TrimSpace(strings.TrimPrefix(trimmed, "server-url:")))
				}
			}
			if len(serverURLs) == 0 {
				t.Fatalf("fixture %s has no server-url — the hermetic Setup rewrite has nothing to redirect", file)
			}

			for _, u := range serverURLs {
				for _, lb := range loopbackMarkers {
					if strings.Contains(u, lb) {
						t.Fatalf("fixture %s server-url %q contains loopback marker %q — a pre-baked loopback URL lets the hermetic hit-counter falsifier pass even if the Setup rewrite is broken; keep the authored URL as the external placeholder %q", file, u, lb, wantHost)
					}
				}
				if !strings.Contains(u, wantHost) {
					t.Fatalf("fixture %s server-url %q does not contain the expected external placeholder host %q — the hermetic Setup rewrite redirects these hosts to the local fixture server; an unexpected host may not be redirected and could leak live", file, u, wantHost)
				}
			}
		})
	}
}
