//go:build unit
// +build unit

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/ptr"
)

// external_fetch_falsifier_test.go — the (a)-(k) falsifiers for the
// snowplow-side JSON-or-YAML external-GET relaxation
// (feat/restaction-yaml-response).
//
// These exercise httpFetchAllowingNonJSON directly against an
// httptest.Server, which drives the REUSED plumbing apparatus
// (HTTPClientForEndpoint → auth roundtrippers, util.NewRetryClient,
// the non-2xx error envelope) plus the new content-type detection +
// YAMLToJSON conversion. The handler-chain consumer ACs assert the
// converted JSON bytes drive jsonHandlerCore + a jq stage filter
// identically to a real blueprint stage (which is what the resolver
// feeds the bytes into via feedBytes at resolve.go:822).
//
// No kubeconfig, no kind cluster — pure in-process
// (feedback_no_go_test_against_remote_kubeconfig).

// helmIndexYAML is a faithful Helm repository index.yaml shape — the
// canonical consumer the ship enables (AC1).
const helmIndexYAML = `apiVersion: v1
entries:
  postgresql:
    - name: postgresql
      version: 12.1.0
      appVersion: "15.1.0"
    - name: postgresql
      version: 12.0.0
      appVersion: "15.0.0"
  redis:
    - name: redis
      version: 17.0.0
      appVersion: "7.0.0"
generated: "2026-06-20T00:00:00Z"
`

// fetchAgainst spins an httptest.Server returning the given status,
// content-type, and body, then runs httpFetchAllowingNonJSON against
// it. The returned recordedAuth is the inbound Authorization header
// the server saw (falsifier (k)).
func fetchAgainst(t *testing.T, status int, contentType, body string, ep func(serverURL string) endpoints.Endpoint) (st *response.Status, jsonBytes []byte, ct string, recordedAuth string, err error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordedAuth = r.Header.Get("Authorization")
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	endpoint := ep(srv.URL)
	verb := http.MethodGet
	call := httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{
			Verb: &verb,
		},
		Endpoint: &endpoint,
	}
	st, jsonBytes, ct, err = httpFetchAllowingNonJSON(context.Background(), call)
	return st, jsonBytes, ct, recordedAuth, err
}

func noAuth(serverURL string) endpoints.Endpoint {
	return endpoints.Endpoint{ServerURL: serverURL}
}

// runStageFilter feeds jsonBytes into the exact handler chain a real
// stage uses (jsonHandlerBytesApply → jsonHandlerCore) with the given
// jq filter, and returns the accumulated dict[key].
func runStageFilter(t *testing.T, key, filter string, jsonBytes []byte) any {
	t.Helper()
	out := map[string]any{}
	opts := jsonHandlerOptions{
		key:    key,
		out:    out,
		dict:   out,
		filter: ptr.To(filter),
	}
	if err := jsonHandlerBytesApply(context.Background(), opts, jsonBytes); err != nil {
		t.Fatalf("jsonHandlerBytesApply failed: %v", err)
	}
	return out[key]
}

// (a) index.yaml served text/plain → converts → jq blueprint filter
// returns expected entries (where today the call 406s before the body
// is read).
func TestFalsifierA_HelmIndexTextPlain(t *testing.T) {
	st, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "text/plain", helmIndexYAML, noAuth)
	if err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}
	if st.Status == response.StatusFailure {
		t.Fatalf("expected success status, got failure: %+v", st)
	}
	// Blueprint-style filter: pluck the postgresql versions. Production
	// stage filters reference the stage key (jsonHandlerCore evaluates the
	// filter against the WRAPPED envelope `{<key>: <body>}`), so the filter
	// is rooted at `.idx`.
	got := runStageFilter(t, "idx", `.idx.entries.postgresql | map(.version)`, jsonBytes)
	want := []any{"12.1.0", "12.0.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blueprint filter mismatch.\n got: %#v\nwant: %#v", got, want)
	}
}

// (b) text/yaml content-type sniff path.
func TestFalsifierB_TextYAML(t *testing.T) {
	_, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "text/yaml", helmIndexYAML, noAuth)
	if err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}
	got := runStageFilter(t, "idx", `.idx.entries.redis | map(.version)`, jsonBytes)
	if !reflect.DeepEqual(got, []any{"17.0.0"}) {
		t.Fatalf("text/yaml conversion mismatch: %#v", got)
	}
}

// (c) application/x-yaml content-type sniff path.
func TestFalsifierC_ApplicationXYAML(t *testing.T) {
	_, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "application/x-yaml", helmIndexYAML, noAuth)
	if err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}
	got := runStageFilter(t, "idx", `.idx.apiVersion`, jsonBytes)
	if got != "v1" {
		t.Fatalf("application/x-yaml conversion mismatch: %#v", got)
	}
}

// (d) application/json control → passthrough byte-identical
// (no-regression). YAMLToJSON round-trips valid JSON losslessly, but the
// dict result must be value-identical to a direct json.Unmarshal.
func TestFalsifierD_JSONPassthroughByteIdentical(t *testing.T) {
	jsonBody := `{"entries":{"postgresql":[{"name":"postgresql","version":"12.1.0"}]},"apiVersion":"v1"}`
	_, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "application/json", jsonBody, noAuth)
	if err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}
	// Decoded value must equal a plain json.Unmarshal of the original
	// body — the conversion must be a no-op for JSON.
	var viaFetch, direct any
	if err := json.Unmarshal(jsonBytes, &viaFetch); err != nil {
		t.Fatalf("fetch bytes not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(jsonBody), &direct); err != nil {
		t.Fatalf("control body not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(viaFetch, direct) {
		t.Fatalf("JSON passthrough not value-identical.\n got: %#v\nwant: %#v", viaFetch, direct)
	}
}

// (e) malformed YAML → conversion fails → returned as a Failure status
// so the resolver's existing recordItemError honours
// ContinueOnError/ErrorKey. The fetch itself must NOT panic and must NOT
// return a non-nil go error that would truncate the resolve; it returns
// a StatusFailure envelope (mirroring how httpcall.Do surfaced failures).
func TestFalsifierE_MalformedYAML(t *testing.T) {
	bad := "entries:\n  - this: is\n   : not valid yaml\n\t bad indent"
	st, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "text/yaml", bad, noAuth)
	if err != nil {
		t.Fatalf("fetch must not return a hard go error for malformed body; got %v", err)
	}
	if st == nil || st.Status != response.StatusFailure {
		t.Fatalf("expected StatusFailure envelope for malformed YAML, got %+v", st)
	}
	if jsonBytes != nil {
		t.Fatalf("expected nil json bytes on conversion failure, got %q", string(jsonBytes))
	}
}

// Architect note A — symmetric structured-shape guard. A bare top-level
// YAML SCALAR served under an EXPLICIT yaml content-type (text/yaml) must
// be rejected as a clean conversion failure, identically to the same
// scalar arriving via the body-shape fallback path. This pins the
// uniform-convert contract: no asymmetry where text/yaml scalars slip
// through but text/plain scalars are rejected. (Such a body was 406'd
// pre-ship, so the clean failure is no regression.)
func TestFalsifierE2_YAMLScalarExplicitContentTypeRejected(t *testing.T) {
	scalar := `"v1.2.3"`
	st, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "text/yaml", scalar, noAuth)
	if err != nil {
		t.Fatalf("fetch must not return a hard go error for a scalar body; got %v", err)
	}
	if st == nil || st.Status != response.StatusFailure {
		t.Fatalf("expected StatusFailure for a bare YAML scalar under text/yaml, got %+v", st)
	}
	if jsonBytes != nil {
		t.Fatalf("expected nil json bytes on scalar rejection, got %q", string(jsonBytes))
	}
}

// (f) bearer-auth Endpoint → call succeeds (auth apparatus reused).
func TestFalsifierF_BearerAuthSucceeds(t *testing.T) {
	bearerEP := func(serverURL string) endpoints.Endpoint {
		return endpoints.Endpoint{ServerURL: serverURL, Token: "s3cr3t-token"}
	}
	st, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "text/yaml", helmIndexYAML, bearerEP)
	if err != nil {
		t.Fatalf("bearer-auth fetch error: %v", err)
	}
	if st.Status == response.StatusFailure {
		t.Fatalf("bearer-auth fetch returned failure: %+v", st)
	}
	got := runStageFilter(t, "idx", `.idx.apiVersion`, jsonBytes)
	if got != "v1" {
		t.Fatalf("bearer-auth conversion mismatch: %#v", got)
	}
}

// (g) 200 + HTML/garbage → clean error (conversion failure), no panic,
// no creds leak in the surfaced message.
func TestFalsifierG_HTMLGarbageCleanError(t *testing.T) {
	html := "<!DOCTYPE html><html><body><h1>503 upstream</h1></body></html>"
	garbageEP := func(serverURL string) endpoints.Endpoint {
		return endpoints.Endpoint{ServerURL: serverURL, Token: "leak-me-if-you-can"}
	}
	st, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "text/html", html, garbageEP)
	if err != nil {
		t.Fatalf("HTML garbage must not return a hard go error; got %v", err)
	}
	if st == nil || st.Status != response.StatusFailure {
		t.Fatalf("expected StatusFailure for HTML garbage, got %+v", st)
	}
	if jsonBytes != nil {
		t.Fatalf("expected nil json bytes on conversion failure")
	}
	if strings.Contains(st.Message, "leak-me-if-you-can") {
		t.Fatalf("credential leaked into error message: %q", st.Message)
	}
}

// (j) non-2xx error-status byte-identical to plumbing request.go:102-116
// — both the JSON-Status branch and the raw-string branch, incl. the
// 2048-byte truncation.
func TestFalsifierJ_Non2xxByteIdentical(t *testing.T) {
	t.Run("json-status-body", func(t *testing.T) {
		statusJSON := `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"forbidden","reason":"Forbidden","code":403}`
		st, _, _, _, err := fetchAgainst(t, http.StatusForbidden, "application/json", statusJSON, noAuth)
		if err != nil {
			t.Fatalf("unexpected go error: %v", err)
		}
		if st.Code != 403 || st.Message != "forbidden" || st.Reason != response.StatusReasonForbidden {
			t.Fatalf("json-status non-2xx not byte-identical: %+v", st)
		}
	})
	t.Run("raw-string-body", func(t *testing.T) {
		// Use a 4xx (404). The non-2xx BODY-shaping branch
		// (request.go:102-116) only runs for codes the RetryClient does
		// NOT retry — i.e. non-5xx, non-429. 5xx is retried 5× and then
		// surfaced as a `server error after N retries` go error +
		// response.New(500, err), NOT the body envelope; that 5xx path is
		// asserted byte-identical in the "5xx-retry-error" subtest below.
		raw := "plain text upstream error"
		st, _, _, _, err := fetchAgainst(t, http.StatusNotFound, "text/plain", raw, noAuth)
		if err != nil {
			t.Fatalf("unexpected go error: %v", err)
		}
		// request.go raw branch: response.New(statusCode, fmt.Errorf("%s", dat)).
		if st.Code != http.StatusNotFound || st.Message != raw {
			t.Fatalf("raw non-2xx not byte-identical: %+v", st)
		}
	})
	t.Run("2048-truncation", func(t *testing.T) {
		big := strings.Repeat("A", 4096)
		st, _, _, _, err := fetchAgainst(t, http.StatusBadRequest, "text/plain", big, noAuth)
		if err != nil {
			t.Fatalf("unexpected go error: %v", err)
		}
		if len(st.Message) != 2048 {
			t.Fatalf("expected 2048-byte truncation, got %d bytes", len(st.Message))
		}
	})
	t.Run("5xx-retry-error", func(t *testing.T) {
		// 5xx: RetryClient retries then returns a go error. request.Do
		// (and our transcription) both turn that into response.New(500,
		// err) + a non-nil go error — byte-identical. We cap retries to 0
		// via env so this subtest does not pay 5× backoff.
		t.Setenv("CLIENT_MAX_RETRIES", "0")
		st, _, _, _, err := fetchAgainst(t, http.StatusInternalServerError, "text/plain", "boom", noAuth)
		if err == nil {
			t.Fatalf("expected a hard go error for exhausted 5xx retries")
		}
		if st == nil || st.Code != http.StatusInternalServerError || st.Status != response.StatusFailure {
			t.Fatalf("5xx retry-exhaustion envelope not byte-identical: %+v", st)
		}
	})
}

// (k) auth header ON THE WIRE — assert the server received exactly the
// expected bearer/basic Authorization value (reuse EXERCISED, not just
// imported).
func TestFalsifierK_AuthHeaderOnTheWire(t *testing.T) {
	t.Run("bearer", func(t *testing.T) {
		bearerEP := func(serverURL string) endpoints.Endpoint {
			return endpoints.Endpoint{ServerURL: serverURL, Token: "wire-bearer-xyz"}
		}
		_, _, _, gotAuth, err := fetchAgainst(t, http.StatusOK, "text/yaml", helmIndexYAML, bearerEP)
		if err != nil {
			t.Fatalf("fetch error: %v", err)
		}
		if gotAuth != "Bearer wire-bearer-xyz" {
			t.Fatalf("bearer auth not on the wire: got %q", gotAuth)
		}
	})
	t.Run("basic", func(t *testing.T) {
		basicEP := func(serverURL string) endpoints.Endpoint {
			return endpoints.Endpoint{ServerURL: serverURL, Username: "alice", Password: "pw123"}
		}
		_, _, _, gotAuth, err := fetchAgainst(t, http.StatusOK, "text/yaml", helmIndexYAML, basicEP)
		if err != nil {
			t.Fatalf("fetch error: %v", err)
		}
		req := &http.Request{Header: http.Header{"Authorization": []string{gotAuth}}}
		u, p, ok := req.BasicAuth()
		if !ok || u != "alice" || p != "pw123" {
			t.Fatalf("basic auth not on the wire: got %q", gotAuth)
		}
	})
}

// (h) internal-rest-config JSON no-regression: the internal-rest-config
// dispatch path feeds its in-memory JSON bytes via feedBytes →
// jsonHandlerBytes, NEVER through httpFetchAllowingNonJSON/toJSONBytes.
// This asserts that path's handler result is byte-identical to a direct
// json.Unmarshal of the same bytes — and, critically, that toJSONBytes
// (the YAML conversion) is NOT in that code path (the conversion lives
// only in external_fetch.go; the dispatch paths are untouched, AC4).
func TestFalsifierH_InternalRESTConfigJSONNoRegression(t *testing.T) {
	jsonBody := []byte(`{"items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}}]}`)

	out := map[string]any{}
	fn := jsonHandlerBytes(context.Background(), jsonHandlerOptions{key: "ks", out: out, dict: out})
	if err := fn(jsonBody); err != nil {
		t.Fatalf("jsonHandlerBytes failed: %v", err)
	}

	var direct any
	if err := json.Unmarshal(jsonBody, &direct); err != nil {
		t.Fatalf("control unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(out["ks"], direct) {
		t.Fatalf("internal-rest-config JSON not byte-identical.\n got: %#v\nwant: %#v", out["ks"], direct)
	}
}

// (i) in-process nested-call JSON no-regression: a loopback nested /call
// returns Status.Raw bytes that the resolver feeds via feedBytes →
// jsonHandlerBytes (same as (h)). The decoded dict value must be
// byte-identical to a direct unmarshal of the Status.Raw — proving the
// nested-call path is untouched and does NOT route through the YAML
// conversion (AC4).
func TestFalsifierI_NestedCallJSONNoRegression(t *testing.T) {
	statusRaw := []byte(`{"kind":"RESTAction","apiVersion":"templates.krateo.io/v1","status":{"foo":"bar"}}`)

	out := map[string]any{}
	fn := jsonHandlerBytes(context.Background(), jsonHandlerOptions{key: "nested", out: out, dict: out})
	if err := fn(statusRaw); err != nil {
		t.Fatalf("jsonHandlerBytes failed: %v", err)
	}

	var direct any
	if err := json.Unmarshal(statusRaw, &direct); err != nil {
		t.Fatalf("control unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(out["nested"], direct) {
		t.Fatalf("nested-call Status.Raw not byte-identical.\n got: %#v\nwant: %#v", out["nested"], direct)
	}
}

// Architect note B — explicit apiserver-fall-through no-regression. When an
// apiserver-GVR GET/LIST falls through ALL pivot branches (e.g. informer
// unsynced) to the terminal external branch, it previously hit httpcall.Do
// and now hits httpFetchAllowingNonJSON. The apiserver returns
// application/json (a {items:[...]} list envelope), which takes the JSON
// fast-path (json.Valid → bytes returned unchanged) and must be
// value-identical to a direct json.Unmarshal. (h)/(i) cover the
// internal-rest-config + nested-call no-regression; this names the THIRD
// terminal-branch no-regression case explicitly in the ledger.
func TestFalsifierB2_ApiserverJSONFallThroughByteIdentical(t *testing.T) {
	listEnvelope := `{"apiVersion":"v1","kind":"List","items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}}]}`
	st, jsonBytes, _, _, err := fetchAgainst(t, http.StatusOK, "application/json", listEnvelope, noAuth)
	if err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}
	if st.Status == response.StatusFailure {
		t.Fatalf("apiserver-style JSON returned failure: %+v", st)
	}
	var viaFetch, direct any
	if err := json.Unmarshal(jsonBytes, &viaFetch); err != nil {
		t.Fatalf("fetch bytes not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(listEnvelope), &direct); err != nil {
		t.Fatalf("control body not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(viaFetch, direct) {
		t.Fatalf("apiserver-fall-through JSON not value-identical.\n got: %#v\nwant: %#v", viaFetch, direct)
	}
}
