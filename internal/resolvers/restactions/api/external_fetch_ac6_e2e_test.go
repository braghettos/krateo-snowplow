//go:build unit
// +build unit

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/client-go/rest"
)

// external_fetch_ac6_e2e_test.go — AC6 END-TO-END falsifier
// (feat/restaction-yaml-response).
//
// PM-requested gap closure: the (e)/(g) unit falsifiers in
// external_fetch_falsifier_test.go assert httpFetchAllowingNonJSON's
// RETURN TUPLE (StatusFailure envelope shape on malformed YAML / HTML
// garbage). AC6 is stated against the recordItemError CONTRACT
// (resolve.go:823-856) — ContinueOnError / ErrorKey honouring — which is
// only exercised through the full dispatchOneCall path. These tests
// drive the REAL api.Resolve over an httptest server (same harness model
// as resolve_iter_continue_test.go: WithInternalEndpoint → external
// branch → httpFetchAllowingNonJSON → recordItemError), so they prove:
//
//   - ContinueOnError=true  → the malformed-YAML stage does NOT abort the
//     resolve; the downstream JSON stage still populates; dict[errorKey]
//     carries the failure value (the contract C-A/W-A path).
//   - ContinueOnError=false → the failing stage truncates the resolve (the
//     downstream stage does NOT run), exactly as an httpcall.Do
//     StatusFailure did pre-ship.
//   - dict[errorKey] / the surfaced message NEVER contains the endpoint
//     bearer token (no-creds-leak, end-to-end — closes the PM's question
//     that the unit (g) only checked st.Message, not the errorKey path).
//
// No kubeconfig, no kind cluster — pure in-process
// (feedback_no_go_test_against_remote_kubeconfig). Reuses iterResolveCtx
// / keysOf / errEntryMentions from resolve_iter_continue_test.go.

// ac6BearerToken is the endpoint credential the no-leak assertions hunt
// for in the surfaced failure value / message.
const ac6BearerToken = "ac6-secret-bearer-DO-NOT-LEAK"

// ac6Fixture serves a fixed (status, content-type, body) at /bad and a
// good JSON object at /good, recording the inbound Authorization header.
type ac6Fixture struct {
	server   *httptest.Server
	gotAuth  string
	badCT    string
	badBody  string
	badCode  int
}

func newAC6Fixture(t *testing.T, badCode int, badCT, badBody string) *ac6Fixture {
	t.Helper()
	f := &ac6Fixture{badCT: badCT, badBody: badBody, badCode: badCode}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/good":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true,"name":"downstream-ran"}`)
		default: // "/bad"
			if f.badCT != "" {
				w.Header().Set("Content-Type", f.badCT)
			}
			w.WriteHeader(f.badCode)
			fmt.Fprint(w, f.badBody)
		}
	}))
	t.Cleanup(srv.Close)
	f.server = srv
	return f
}

// runAC6Resolve drives the REAL api.Resolve over the fixture with the
// endpoint carrying ac6BearerToken (so the no-leak check is meaningful:
// the token rides the same call.Endpoint into httpFetchAllowingNonJSON).
func runAC6Resolve(f *ac6Fixture, stages []*templates.API) map[string]any {
	ctx := cache.WithInternalEndpoint(iterResolveCtx(), &endpoints.Endpoint{
		ServerURL: f.server.URL,
		Token:     ac6BearerToken,
	})
	return Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               stages,
		RESTActionNamespace: "default",
		RESTActionName:      "ac6-yaml-falsifier",
	})
}

// badStage builds a single external GET stage hitting /bad with the given
// ContinueOnError + ErrorKey.
func badStage(continueOnError bool) *templates.API {
	return &templates.API{
		Name:            "bad",
		Path:            "/bad",
		Verb:            ptr.To(http.MethodGet),
		ContinueOnError: ptr.To(continueOnError),
		ErrorKey:        ptr.To("badErr"),
	}
}

// goodDownstream depends on "bad" so topologicalSort runs bad first; its
// presence in the dict proves the resolve did NOT truncate.
func goodDownstream() *templates.API {
	return &templates.API{
		Name:      "good",
		Path:      "/good",
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: "bad"},
		Filter:    ptr.To(".good"),
	}
}

// assertNoTokenLeak fails if the endpoint bearer token appears anywhere
// in the JSON-rendered dict (errorKey value, message, etc.).
func assertNoTokenLeak(t *testing.T, dict map[string]any) {
	t.Helper()
	rendered := fmt.Sprintf("%#v", dict)
	if strings.Contains(rendered, ac6BearerToken) {
		t.Fatalf("CREDS LEAK: endpoint bearer token surfaced in the resolved dict: %s", rendered)
	}
}

// failFastRetries pins the RetryClient knobs so a 5xx /bad fails on the
// first attempt (mirrors iterFailFastRetries).
func ac6FailFast(t *testing.T) {
	t.Helper()
	t.Setenv("CLIENT_MAX_RETRIES", "0")
	t.Setenv("CLIENT_BASE_BACKOFF", "1ms")
	t.Setenv("CLIENT_MAX_BACKOFF", "1ms")
}

// AC6 (i): malformed YAML 200 + ContinueOnError=true → resolve continues,
// downstream JSON stage populates, dict[errorKey] carries the failure, no
// token leak.
func TestAC6_MalformedYAML_ContinueOnError_True(t *testing.T) {
	ac6FailFast(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	malformed := "entries:\n  - a: 1\n   : broken indent\n\tbad"
	f := newAC6Fixture(t, http.StatusOK, "text/yaml", malformed)

	dict := runAC6Resolve(f, []*templates.API{badStage(true), goodDownstream()})

	// errorKey carries the conversion failure value (W-A accumulating slice
	// or scalar — both acceptable; the contract is "errorKey populated").
	if _, ok := dict["badErr"]; !ok {
		t.Fatalf("AC6: dict[\"badErr\"] absent on a continueOnError=true malformed-YAML stage — "+
			"recordItemError did not honour ErrorKey. dict keys=%v", keysOf(dict))
	}
	// Downstream stage MUST have run (no truncation).
	if _, ok := dict["good"]; !ok {
		t.Fatalf("AC6: downstream stage \"good\" absent — the malformed-YAML stage truncated the "+
			"resolve despite continueOnError=true. dict keys=%v", keysOf(dict))
	}
	assertNoTokenLeak(t, dict)
}

// AC6 (ii): HTML/garbage 200 + ContinueOnError=true → same contract
// (clean failure, continue, errorKey populated, no leak). Covers the (g)
// path end-to-end including the errorKey surface the unit (g) did not.
func TestAC6_HTMLGarbage_ContinueOnError_True(t *testing.T) {
	ac6FailFast(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	html := "<!DOCTYPE html><html><body>upstream 503</body></html>"
	f := newAC6Fixture(t, http.StatusOK, "text/html", html)

	dict := runAC6Resolve(f, []*templates.API{badStage(true), goodDownstream()})

	if _, ok := dict["badErr"]; !ok {
		t.Fatalf("AC6: dict[\"badErr\"] absent on a continueOnError=true HTML-garbage stage. dict keys=%v", keysOf(dict))
	}
	if _, ok := dict["good"]; !ok {
		t.Fatalf("AC6: downstream stage absent — HTML garbage truncated the resolve. dict keys=%v", keysOf(dict))
	}
	assertNoTokenLeak(t, dict)
}

// AC6 (iii): malformed YAML 200 + ContinueOnError=FALSE → the failing
// stage's error is recorded under errorKey BYTE-IDENTICALLY to the
// pre-ship httpcall.Do StatusFailure path (resolve.go:823-856 is
// UNCHANGED by this ship). No token leak.
//
// IMPORTANT behavioural note discovered by this falsifier (and verified
// against the unchanged StatusFailure code path): since Ship 0.30.257
// (#313, Option C-A) a per-item hard error — even with
// ContinueOnError=false — does NOT cancel the errgroup; the worker
// records itemErr into its disjoint itemErrs[i] slot and returns nil, so
// g.Wait() returns nil and the resolve does NOT truncate. The
// false-vs-true distinction at resolve.go:878-881 is ONLY whether the
// lazy fmt.Errorf itemErr is built (it lands in itemErrs[i] → a Debug
// join line, NOT the dict). dict[errorKey] is populated in BOTH cases.
// My ship feeds the SAME StatusFailure envelope into that SAME unchanged
// code, so the observable dict result is byte-identical to the pre-ship
// httpcall.Do path for both ContinueOnError values. (The earlier draft of
// this test wrongly expected false → truncate, a pre-#313 mental model;
// the falsifier caught the stale assumption.)
func TestAC6_MalformedYAML_ContinueOnError_False(t *testing.T) {
	ac6FailFast(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	malformed := "entries:\n  - a: 1\n   : broken indent\n\tbad"
	f := newAC6Fixture(t, http.StatusOK, "text/yaml", malformed)

	dict := runAC6Resolve(f, []*templates.API{badStage(false), goodDownstream()})

	// The failure is recorded under errorKey (the StatusFailure path runs
	// for both ContinueOnError values; only the Debug itemErr differs).
	if _, ok := dict["badErr"]; !ok {
		t.Fatalf("AC6: dict[\"badErr\"] absent on a continueOnError=false malformed-YAML stage — "+
			"the StatusFailure path did not record ErrorKey. dict keys=%v", keysOf(dict))
	}
	// Per #313 C-A the resolve still completes (does not panic / hard-abort);
	// the downstream stage runs exactly as for the pre-ship httpcall.Do
	// StatusFailure (this same unchanged code).
	if _, ok := dict["good"]; !ok {
		t.Fatalf("AC6: downstream stage \"good\" absent — #313 C-A semantics broken. dict keys=%v", keysOf(dict))
	}
	assertNoTokenLeak(t, dict)
}

// AC6 (iv): the no-leak guard is meaningful — assert the token actually
// reached the wire (so a passing no-leak check is not a false negative
// from the token never being sent). If this fails, the endpoint auth was
// not exercised and the no-leak assertions above are vacuous.
func TestAC6_TokenReachedWire(t *testing.T) {
	ac6FailFast(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	f := newAC6Fixture(t, http.StatusOK, "text/yaml", "entries:\n  bad\n\t: x")
	_ = runAC6Resolve(f, []*templates.API{badStage(true)})

	if f.gotAuth != "Bearer "+ac6BearerToken {
		t.Fatalf("AC6: endpoint token did not reach the wire (got %q) — the no-leak checks would be "+
			"vacuous. The owned fetch must send Authorization through the reused roundtripper.", f.gotAuth)
	}
	_ = context.Background()
}

// errKeyMessage extracts the surfaced failure message from dict[errorKey].
// The StatusFailure path records accumVal = response.AsMap(res) (a map
// carrying "message"), accumulated under errorKey as a W-A slice (first
// error) or a bare value. This mirrors the Status.Message the unit (g)
// checked — so asserting on it closes PM's "errorKey path AND st.Message"
// gap end-to-end.
func errKeyMessage(t *testing.T, dict map[string]any, errorKey string) string {
	t.Helper()
	v, ok := dict[errorKey]
	if !ok {
		t.Fatalf("(l): dict[%q] absent — recordItemError did not honour ErrorKey. keys=%v", errorKey, keysOf(dict))
	}
	// W-A accumulating slice: take the first entry; else the value itself.
	entry := v
	if s, isSlice := v.([]any); isSlice {
		if len(s) == 0 {
			t.Fatalf("(l): dict[%q] is an empty slice — no failure recorded", errorKey)
		}
		entry = s[0]
	}
	switch e := entry.(type) {
	case map[string]any:
		if msg, ok := e["message"].(string); ok {
			return msg
		}
		t.Fatalf("(l): dict[%q] entry has no string message field: %#v", errorKey, e)
	case string:
		return e
	}
	t.Fatalf("(l): dict[%q] entry is unexpected type %T: %#v", errorKey, entry, entry)
	return ""
}

// ---------------------------------------------------------------------------
// Falsifier (l) — team-lead spec: end-to-end via runStage/dispatchOneCall,
// exercising the CHANGED dispatch routing (the new `else { feedBytes }`
// success branch + the StatusFailure fall-through → recordItemError).
// ---------------------------------------------------------------------------

// (l): stage1 = external malformed-YAML 2xx, ContinueOnError=true + ErrorKey;
// stage2 = a normal external stage that MUST still populate. Asserts: no
// hard-abort; stage2 populates; dict[errorKey] carries the failure; the auth
// token is absent from BOTH dict[errorKey] (whole value) AND the surfaced
// failure MESSAGE (the asMap "message" the StatusFailure path records — the
// end-to-end equivalent of the unit (g)'s st.Message check).
func TestFalsifierL_MalformedYAML_ContinueTrue_E2E(t *testing.T) {
	ac6FailFast(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	malformed := "entries:\n  - a: 1\n   : broken indent\n\tbad"
	f := newAC6Fixture(t, http.StatusOK, "text/yaml", malformed)

	dict := runAC6Resolve(f, []*templates.API{badStage(true), goodDownstream()})

	// No hard-abort: the downstream normal stage populated.
	if _, ok := dict["good"]; !ok {
		t.Fatalf("(l): downstream stage \"good\" absent — malformed-YAML stage hard-aborted the resolve "+
			"despite continueOnError=true. keys=%v", keysOf(dict))
	}
	// errorKey carries the failure.
	if _, ok := dict["badErr"]; !ok {
		t.Fatalf("(l): dict[\"badErr\"] absent — recordItemError did not honour ErrorKey. keys=%v", keysOf(dict))
	}
	// No leak in the WHOLE errorKey value.
	if rendered := fmt.Sprintf("%#v", dict["badErr"]); strings.Contains(rendered, ac6BearerToken) {
		t.Fatalf("(l) CREDS LEAK in dict[errorKey]: %s", rendered)
	}
	// No leak in the surfaced failure MESSAGE specifically (end-to-end
	// equivalent of unit (g)'s st.Message check); and the message IS the
	// conversion failure, not the endpoint detail.
	msg := errKeyMessage(t, dict, "badErr")
	if strings.Contains(msg, ac6BearerToken) {
		t.Fatalf("(l) CREDS LEAK in surfaced failure message: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "yaml") && !strings.Contains(strings.ToLower(msg), "convert") &&
		!strings.Contains(strings.ToLower(msg), "json") {
		t.Errorf("(l): surfaced message does not look like a conversion failure: %q", msg)
	}
	// Belt-and-suspenders whole-dict leak scan.
	assertNoTokenLeak(t, dict)
}

// (l-variant): malformed-YAML 2xx + ContinueOnError=FALSE.
//
// SPEC vs CODE: the team-lead spec calls for "hard-abort" here. The CODE
// (resolve.go:854-886, UNCHANGED by this ship) does NOT hard-abort a
// StatusFailure for either ContinueOnError value — since Ship 0.30.257
// (#313 Option C-A) the StatusFailure branch records the item error via
// recordItemError and FALLS THROUGH (returns nil); g.Wait() only
// truncates on a genuine ctx-cancel (resolve.go:1189-1198). (NOTE: as of
// the B-regression fix in this ship, a success-branch feedBytes/decode
// error is ALSO shaped into a StatusFailure → recordItemError → no
// truncate, matching the pre-ship httpcall.Do ResponseHandler-error path —
// see TestFalsifierB_SuccessBranchDecodeFailure_NoTruncate. So the only
// remaining truncation source is ctx-cancel.) A malformed-YAML body is
// surfaced as a StatusFailure ENVELOPE (never reaching feedBytes), so it
// takes the no-truncate path regardless of ContinueOnError.
//
// This is NOT introduced by the ship — it is the unchanged #313 contract;
// the pre-ship httpcall.Do StatusFailure fed the same block. Writing a
// test that asserted a hard-abort here would FAIL (or could only pass by
// faking), so per feedback_no_fake_production_scenarios this test pins the
// ACTUAL behaviour: false → errorKey recorded, the lazily-built itemErr
// is present in the Debug join (not the dict), resolve completes, no leak.
// The spec/code divergence is raised to PM + team-lead.
func TestFalsifierL_Variant_MalformedYAML_ContinueFalse_E2E(t *testing.T) {
	ac6FailFast(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	malformed := "entries:\n  - a: 1\n   : broken indent\n\tbad"
	f := newAC6Fixture(t, http.StatusOK, "text/yaml", malformed)

	dict := runAC6Resolve(f, []*templates.API{badStage(false), goodDownstream()})

	// Actual #313 contract: errorKey recorded, resolve does NOT truncate.
	if _, ok := dict["badErr"]; !ok {
		t.Fatalf("(l-variant): dict[\"badErr\"] absent — StatusFailure path did not record ErrorKey. keys=%v", keysOf(dict))
	}
	if _, ok := dict["good"]; !ok {
		t.Fatalf("(l-variant): downstream stage absent — the #313 no-truncate contract (unchanged by this "+
			"ship) was broken. keys=%v", keysOf(dict))
	}
	// No leak (same surface as (l)).
	if msg := errKeyMessage(t, dict, "badErr"); strings.Contains(msg, ac6BearerToken) {
		t.Fatalf("(l-variant) CREDS LEAK in surfaced failure message: %q", msg)
	}
	assertNoTokenLeak(t, dict)
}

// Architect note B (non-blocking, folded in): explicit external-JSON
// fall-through. A normal external GET returning a 2xx JSON object takes
// the new `else { feedBytes }` SUCCESS branch (the changed routing) and
// must populate dict[id] byte-identically to the pre-ship httpcall.Do
// ResponseHandler path. This is the success-side companion to the
// (l)/(l-variant) failure-side e2e coverage — it exercises the SAME
// changed dispatch routing end-to-end, proving the no-regression for the
// common JSON external stage (AC3) through runStage, not just the unit
// (d).
func TestFalsifierL_ExternalJSONFallThrough_E2E(t *testing.T) {
	ac6FailFast(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	// /good returns {"ok":true,"name":"downstream-ran"} as application/json.
	f := newAC6Fixture(t, http.StatusOK, "application/json", "")

	jsonStage := &templates.API{
		Name:   "j",
		Path:   "/good",
		Verb:   ptr.To(http.MethodGet),
		Filter: ptr.To(".j"),
	}
	dict := runAC6Resolve(f, []*templates.API{jsonStage})

	got, ok := dict["j"].(map[string]any)
	if !ok {
		t.Fatalf("note-B: dict[\"j\"] is %T, want map (external JSON did not populate via feedBytes). keys=%v",
			dict["j"], keysOf(dict))
	}
	if got["ok"] != true || got["name"] != "downstream-ran" {
		t.Fatalf("note-B: external JSON 2xx not byte-identical through the success branch: %#v", got)
	}
}

// ---------------------------------------------------------------------------
// Falsifier B-regression (team-lead) — success-branch DECODE-FAILURE path,
// arbitrated against the pre-ship httpcall.Do oracle.
// ---------------------------------------------------------------------------
//
// SCENARIO: a 2xx body that CONVERTS cleanly to JSON (so it reaches the new
// `else { feedBytes }` SUCCESS branch — NOT the StatusFailure branch) but
// whose stage Filter then forces a jsonHandlerCore error. A jq filter of
// `empty` yields zero values → jsonHandlerCore returns
// `fmt.Errorf("jq filter %q yielded no value")` (handler.go) → feedBytes
// returns that error.
//
// PRE-SHIP ORACLE (httpcall.Do, request.go:121-126): the handler ran as
// call.ResponseHandler(body); a ResponseHandler error was wrapped
// `return response.New(http.StatusInternalServerError, err)` → a
// StatusFailure → at resolve.go the `res.Status == StatusFailure` branch ran
// recordItemError and FELL THROUGH returning nil → under #313 C-A NO
// truncation (downstream stages still run).
//
// NEW CODE (resolve.go:899-901): `if err := feedBytes(jsonBytes); err != nil
// { return err }` returns the error RAW to dispatchOneCall → the errgroup
// worker returns non-nil → g.Wait() returns non-nil → TRUNCATION.
//
// => If this falsifier is RED (downstream "good" absent), the new success
// branch truncates where pre-ship continued — a REAL REGRESSION. The fix is
// to route a feedBytes error through the SAME StatusFailure → recordItemError
// fall-through the old ResponseHandler-error path used (shape it as a
// per-item StatusFailure, NOT a raw return err).
//
// The oracle is captured empirically in the report by re-running this exact
// test against pre-ship resolve.go (git checkout of the transport path).
func TestFalsifierB_SuccessBranchDecodeFailure_NoTruncate(t *testing.T) {
	ac6FailFast(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	// /bad returns a VALID-converting YAML object (2xx, text/yaml) → reaches
	// the success branch. Its filter `empty` forces a jsonHandlerCore
	// zero-yield error inside feedBytes.
	f := newAC6Fixture(t, http.StatusOK, "text/yaml", "entries:\n  - name: a\n  - name: b\n")

	decodeFailStage := &templates.API{
		Name:            "bad",
		Path:            "/bad",
		Verb:            ptr.To(http.MethodGet),
		ContinueOnError: ptr.To(true),
		ErrorKey:        ptr.To("badErr"),
		Filter:          ptr.To("empty"), // zero-yield → jsonHandlerCore error
	}

	dict := runAC6Resolve(f, []*templates.API{decodeFailStage, goodDownstream()})

	// ORACLE assertion: pre-ship did NOT truncate on a ResponseHandler error
	// (it became a StatusFailure → recordItemError → nil). The downstream
	// stage MUST still populate. RED here = the new success branch truncates
	// → real regression.
	if _, ok := dict["good"]; !ok {
		t.Fatalf("B-REGRESSION: downstream stage \"good\" ABSENT — the new success-branch feedBytes "+
			"error TRUNCATED the resolve, where pre-ship httpcall.Do turned a ResponseHandler error into "+
			"a StatusFailure → recordItemError → no-truncate. keys=%v", keysOf(dict))
	}
}
