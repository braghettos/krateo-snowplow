//go:build integration
// +build integration

package restactions

// httpfixture_test.go — B (hermetify) test-harness: a suite-local httptest.Server
// that serves RECORDED fixture JSON (testdata/restactions/httpresponses/*.json)
// for the three formerly-live RESTAction fixtures (github / httpbin / typicode).
//
// WHY (docs/flaky-integration-gate-fix-design-2026-07-02.md): the PR merge gate
// (release-pullrequest.yaml:47 `go test -tags=unit,integration`) used to dispatch
// these three RAs' api-steps to LIVE third-party hosts (api.github.com,
// httpbin.org, jsonplaceholder.typicode.com) → flake on external drift +
// unauthenticated github rate-limit → gated EVERY PR on third-party uptime. This
// server makes them DETERMINISTIC while KEEPING all six Assess steps in the gate
// (no coverage loss): TestMain starts it, Setup rewrites the three endpointRef
// Secrets' `server-url` to srv.URL, and the resolve pipeline is exercised
// end-to-end (dispatch + jq-filter) against the local server. Zero live calls.
//
// ROUTING: all three Secrets point at the SAME srv.URL; the mux disambiguates by
// PATH (the paths across the three RAs are disjoint). A per-path hit counter
// gives the falsifier a decisive "zero live egress" signal: the expected number
// of api-step dispatches == the server's total hit count, so if the test passes
// the requests went HERE, not to the internet.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const httpResponsesDir = "../../../testdata/restactions/httpresponses"

// fixtureServer is the suite-local recorded-response server. hits counts total
// requests served (the falsifier reads it to prove dispatches landed locally).
type fixtureServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits int
	// perPath records the count per request path for diagnostics / assertions.
	perPath map[string]int
}

// mustReadFixture reads a recorded JSON body, failing the process if absent —
// a missing fixture is a harness bug, not a flake.
func mustReadFixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join(httpResponsesDir, name))
	if err != nil {
		panic("httpfixture: unable to read recorded fixture " + name + ": " + err.Error())
	}
	return b
}

// newFixtureServer builds the mux + starts the server. Each handler serves a
// recorded body with application/json. The mux keys by path; query strings
// (typicode ?userId=N, github ?per_page=2) are ignored by ServeMux path matching,
// and one recorded body covers each path's shape (the fixtures carry rows for
// the userIds/ids the RAs iterate).
func newFixtureServer() *fixtureServer {
	fs := &fixtureServer{perPath: map[string]int{}}

	serve := func(body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			fs.mu.Lock()
			fs.hits++
			fs.perPath[r.URL.Path]++
			fs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	}

	mux := http.NewServeMux()

	// --- typicode (jsonplaceholder) ---
	// GET /users (array) → filter map(select(.email|endswith(".biz")))
	mux.HandleFunc("/users", serve(mustReadFixture("typicode_users.json")))
	// GET /todos?userId=N (array) for the first 3 users
	mux.HandleFunc("/todos", serve(mustReadFixture("typicode_todos.json")))

	// --- github ---
	// The timing path is MORE SPECIFIC than the runs path (which is its prefix);
	// Go 1.22+ ServeMux precedence prefers the more specific pattern, but to be
	// version-robust we register the runs collection at an EXACT path and let the
	// timing subtree match the remaining /.../{id}/timing requests via a subtree
	// pattern. Both are under the same repo prefix.
	//   runs collection:  /repos/krateoplatformops/snowplow/actions/runs (exact, ?per_page=2)
	//   run timing:       /repos/krateoplatformops/snowplow/actions/runs/{id}/timing
	mux.HandleFunc("/repos/krateoplatformops/snowplow/actions/runs",
		serve(mustReadFixture("github_runs.json")))
	// Subtree: any /repos/.../actions/runs/<id>/timing. A subtree pattern (trailing
	// slash) matches /runs/ and everything under it; the exact "/runs" registration
	// above wins for the bare collection, so this only catches the /runs/<id>/timing
	// children. Serve the timing body for all of them (deterministic per id).
	mux.HandleFunc("/repos/krateoplatformops/snowplow/actions/runs/",
		serve(mustReadFixture("github_timing.json")))

	// --- httpbin ---
	// GET /get?...  → echo {args:{uid:...}}
	mux.HandleFunc("/get", serve(mustReadFixture("httpbin_get.json")))
	// POST /post    → echo {json:{compositionID:...}}
	mux.HandleFunc("/post", serve(mustReadFixture("httpbin_post.json")))

	fs.srv = httptest.NewServer(mux)
	return fs
}

// URL is the base URL the endpointRef Secrets' server-url are rewritten to.
func (fs *fixtureServer) URL() string { return fs.srv.URL }

// Hits returns the total number of requests served (falsifier: > 0 and == the
// expected dispatch count proves the resolve went local, not to the internet).
func (fs *fixtureServer) Hits() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hits
}

// Close shuts the server down (deferred from TestMain).
func (fs *fixtureServer) Close() {
	if fs.srv != nil {
		fs.srv.Close()
	}
}

// assertServedAtLeast is a small helper for the falsifier arm.
func (fs *fixtureServer) assertServedAtLeast(t *testing.T, n int) {
	t.Helper()
	if got := fs.Hits(); got < n {
		t.Fatalf("fixture server served %d requests, want >= %d — some dispatch may have gone LIVE (external) instead of local", got, n)
	}
}
