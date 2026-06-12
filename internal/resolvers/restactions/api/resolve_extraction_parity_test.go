// resolve_extraction_parity_test.go — Task #329 / #330 frozen equivalence
// oracle for the api.Resolve extraction series.
//
// WHY (design docs/task-329-api-resolve-extraction-design-2026-06-12.md §4.2.3):
// the extraction (commits 1-5) decomposes the 878-LOC Resolve body into a
// resolveRun struct + methods with ZERO behaviour change. Go cannot run
// "old code" after an edit, so the proof of byte-identical output across the
// staged series is a golden captured ONCE here, in this FIRST (test-only)
// commit, BEFORE any production line moves — then re-asserted byte-for-byte
// by every subsequent commit's gate run (feedback_falsifier_first_before_ship).
//
// This commit is test-only: it adds the golden harness + the four gap pins
// the parity corpus alone does not cover (PM CONDITION 4):
//   - R-2  : non-UAF mapper.resolveOne err → recordStageTiming(); return dict
//            (truncated — downstream stages absent).  resolve.go EARLY-RETURN #2.
//   - C-2  : UAF stage, SA endpoint unavailable → dict[id]=={items:[]} +
//            recordStageTiming(); continue (downstream stage STILL runs).
//            resolve.go EARLY-CONTINUE.
//   - UserInfo-err early-exit : xcontext.UserInfo(ctx) err → log.Error +
//            return map[string]any{} (R-pre3).
//   - slog-event-order : the method-call sequence the extraction introduces
//            must preserve emit order across a 2-stage resolve.
//
// THE GOLDEN (TestResolveExtractionParity): a corpus of named cases, each its
// OWN Resolve invocation with its OWN fixture + env (the cases need
// incompatible global env — cache-off httpcall cases vs the cache-on informer
// case — so they cannot share one Resolve call). For each case the resolved
// dict is marshaled with json.Marshal (which sorts map keys → deterministic
// top-level + nested key order) and compared against the frozen want map
// below. RESOLVER_ITER_PARALLELISM=1 pins iterator item-merge order so the
// accumulated arrays are byte-stable too.
//
// The cancel case is deliberately NOT in the byte-frozen golden (its dict
// depends on how many items completed before the cancel — nondeterministic);
// genuine-cancellation preservation is pinned by
// TestResolve_CallerCancelStillAbortsRemainingItems. This harness adds a
// non-byte cancel parity sub-assertion (returns a map, no panic) for
// completeness.
//
// SCAFFOLDING: per design §5 commit 5 the harness is removed in the final
// cleanup commit — it exists only to prove the staged series byte-identical.
//
// Run with -race. KUBECONFIG=/dev/null.

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/client-go/rest"
)

// ---------------------------------------------------------------------------
// Golden — frozen PRE-refactor dict output, keyed by corpus case name.
// Captured at commit 0 (this file) against HEAD f5f9bb0 and FROZEN. Every
// extraction commit re-marshals each case's dict and asserts byte-equality
// against the matching entry here. A diff ⇒ the extraction changed behaviour.
//
// The values are the exact json.Marshal(dict) bytes produced by the
// PRE-refactor Resolve. They are committed as literals (not a separate
// fixture file) so the golden travels with the harness and any change is a
// visible byte-diff in review.
// ---------------------------------------------------------------------------

// resolveParityGolden maps caseName → json.Marshal(dict) string. Populated by
// the first run via the RECAPTURE switch below (kept off in committed code);
// the committed literals are the frozen oracle.
var resolveParityGolden = map[string]string{
	"get_single_httpcall":   `{"single":{"name":"only"}}`,
	"iterator_multi_item":   `{"items":[{"name":"v0"},{"name":"v1"},{"name":"v2"}],"vals":[{"ns":"v0"},{"ns":"v1"},{"ns":"v2"}]}`,
	"nested_call_inprocess": `{"nested":{"nestedField":"nested-value"}}`,
	"error_item_403":        `{"error":[{"apiVersion":"v1","code":403,"kind":"Status","message":"item 1 (/items/v1) deliberately failed","reason":"Forbidden","status":"Failure"}],"items":[{"name":"v0"},{"name":"v2"}],"vals":[{"ns":"v0"},{"ns":"v1"},{"ns":"v2"}]}`,
	"uaf_sa_endpoint_unavailable": `{"downstream":{"name":"down"},"uafStage":{"items":[]}}`,
}

// recaptureParityGolden, when true, makes TestResolveExtractionParity PRINT
// the freshly-marshaled dict per case (so the implementer can copy the frozen
// literals on the very first capture) and SKIP the equality assertion. It is
// committed as false — the committed literals above ARE the frozen oracle.
const recaptureParityGolden = false

// ---------------------------------------------------------------------------
// Corpus drivers (each returns the resolved dict for one case).
// ---------------------------------------------------------------------------

// parityHTTPCtx builds a cache-OFF resolve ctx with UserInfo + the fixture
// endpoint attached, so resolveOne short-circuits to it and every dispatch
// falls to httpcall.Do (no informer pivot, no internal-rest-config).
func parityHTTPCtx(f *iterFixture) context.Context {
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "parity-user"}),
	)
	return cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: f.server.URL})
}

// parityResolveHTTP drives the real api.Resolve over the fixture server.
func parityResolveHTTP(ctx context.Context, f *iterFixture, stages []*templates.API, extras map[string]any) map[string]any {
	return Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               stages,
		Extras:              extras,
		RESTActionNamespace: "default",
		RESTActionName:      "resolve-extraction-parity",
	})
}

// caseGetSingleHTTPCall — a single non-iterator GET stage served by
// httpcall.Do. dict["single"] == {"name":"only"}.
func caseGetSingleHTTPCall(t *testing.T) map[string]any {
	t.Helper()
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	f := newIterFixture(t, 1, nil, false)
	stage := &templates.API{
		Name:   "single",
		Path:   iterItemPath("only"),
		Verb:   ptr.To(http.MethodGet),
		Filter: ptr.To(".single"),
	}
	return parityResolveHTTP(parityHTTPCtx(f), f, []*templates.API{stage}, nil)
}

// caseIteratorMultiItem — an iterator stage fanning 3 items, all 200.
// dict["items"] == [{"name":"v0"},{"name":"v1"},{"name":"v2"}].
func caseIteratorMultiItem(t *testing.T) map[string]any {
	t.Helper()
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	const n = 3
	f := newIterFixture(t, n, nil, false)
	stage := iterStage("items", "error", nil)
	stage.Filter = ptr.To(".items")
	extras := map[string]any{"vals": iterStageVals(n)}
	return parityResolveHTTP(parityHTTPCtx(f), f, []*templates.API{stage}, extras)
}

// caseNestedCallInprocess — a /call-loopback GET stage served by the
// in-process nested-call resolver (registered + torn down here). The stubbed
// resolver returns canned bytes; dict["nested"] == {"nestedField":"nested-value"}.
func caseNestedCallInprocess(t *testing.T) map[string]any {
	t.Helper()
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	// Register a deterministic nested-call resolver; restore on cleanup so
	// no other test sees it.
	prev := nestedCallResolver
	nestedCallResolver = func(_ context.Context, _ templates.ObjectReference, _, _ int, _ map[string]any) ([]byte, error) {
		return []byte(`{"nestedField":"nested-value"}`), nil
	}
	t.Cleanup(func() { nestedCallResolver = prev })

	// No fixture server needed — the in-process branch never dispatches HTTP.
	// resolveOne still needs an endpoint; attach a placeholder internal one.
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "parity-user"}),
	)
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})

	stage := &templates.API{
		Name:   "nested",
		Path:   "http://snowplow.invalid/call?resource=widgets&apiVersion=widgets.krateo.io/v1&name=w1&namespace=default",
		Verb:   ptr.To(http.MethodGet),
		Filter: ptr.To(".nested"),
	}
	return Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               []*templates.API{stage},
		RESTActionNamespace: "default",
		RESTActionName:      "resolve-extraction-parity",
	})
}

// caseErrorItem403 — an iterator stage where item 1 returns 403; the other
// items succeed (C-A: stage continues past the per-item error). dict carries
// the surviving items under "items" + the accumulated error under "error".
func caseErrorItem403(t *testing.T) map[string]any {
	t.Helper()
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	const n = 3
	failIdx := map[int]struct{}{1: {}}
	f := newIterFixture(t, n, failIdx, false)
	stage := iterStage("items", "error", ptr.To(true)) // continueOnError true → all items run
	stage.Filter = ptr.To(".items")
	extras := map[string]any{"vals": iterStageVals(n)}
	return parityResolveHTTP(parityHTTPCtx(f), f, []*templates.API{stage}, extras)
}

// caseUAFSAEndpointUnavailable — a UAF stage whose SA endpoint cannot be
// acquired (no /var/run/secrets in the test env → ServiceAccountEndpoint
// errors) → C-2: dict["uafStage"]=={items:[]}; a downstream non-UAF stage
// STILL runs and lands dict["downstream"]. Doubles as the UAF corpus case.
func caseUAFSAEndpointUnavailable(t *testing.T) map[string]any {
	t.Helper()
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	f := newIterFixture(t, 1, nil, false)
	uafStage := &templates.API{
		Name: "uafStage",
		Path: "/apis/example.test/v1/anyplural",
		Verb: ptr.To(http.MethodGet),
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:     "list",
			Group:    "example.test",
			Resource: "anyplural",
		},
	}
	downstream := &templates.API{
		Name:      "downstream",
		Path:      iterItemPath("down"),
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: "uafStage"},
		Filter:    ptr.To(".downstream"),
	}
	return parityResolveHTTP(parityHTTPCtx(f), f, []*templates.API{uafStage, downstream}, nil)
}

// ---------------------------------------------------------------------------
// TestResolveExtractionParity — the frozen golden assertion. The -run gate in
// the PM acceptance conditions matches this exact name.
// ---------------------------------------------------------------------------

func TestResolveExtractionParity(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T) map[string]any
	}{
		{"get_single_httpcall", caseGetSingleHTTPCall},
		{"iterator_multi_item", caseIteratorMultiItem},
		{"nested_call_inprocess", caseNestedCallInprocess},
		{"error_item_403", caseErrorItem403},
		{"uaf_sa_endpoint_unavailable", caseUAFSAEndpointUnavailable},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dict := tc.run(t)
			got, err := json.Marshal(dict)
			if err != nil {
				t.Fatalf("%s: json.Marshal(dict): %v", tc.name, err)
			}
			if recaptureParityGolden {
				t.Logf("PARITY-RECAPTURE %q => %s", tc.name, string(got))
				return
			}
			want, ok := resolveParityGolden[tc.name]
			if !ok {
				t.Fatalf("%s: no frozen golden entry — add one (recapture with recaptureParityGolden=true)", tc.name)
			}
			if string(got) != want {
				t.Errorf("PARITY DRIFT in case %q — the extraction changed dict output.\n got:  %s\n want: %s",
					tc.name, string(got), want)
			}
		})
	}
}

// TestResolveExtractionParity_CancelReturnsMap is the non-byte cancel parity
// sub-assertion: a caller-cancel mid-resolve still returns a map (never
// panics, never deadlocks). The byte content is nondeterministic (depends on
// item completion timing) so it is NOT in the frozen golden; the
// genuine-cancellation behaviour is pinned by
// TestResolve_CallerCancelStillAbortsRemainingItems.
func TestResolveExtractionParity_CancelReturnsMap(t *testing.T) {
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	const n = 6
	f := newIterFixture(t, n, nil, true) // block=true: items hang until cancel
	stage := iterStage("items", "error", ptr.To(true))
	stage.Filter = ptr.To(".items")
	extras := map[string]any{"vals": iterStageVals(n)}

	ctx, cancel := context.WithCancel(parityHTTPCtx(f))
	go func() {
		select {
		case <-f.started:
		case <-time.After(2 * time.Second):
		}
		cancel()
	}()

	done := make(chan map[string]any, 1)
	go func() { done <- parityResolveHTTP(ctx, f, []*templates.API{stage}, extras) }()

	select {
	case d := <-done:
		if d == nil {
			t.Fatal("cancel parity: Resolve returned a nil map")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel parity: Resolve did not return within 10s after caller cancel")
	}
}

// ---------------------------------------------------------------------------
// PM CONDITION 4 — dedicated R-2 / C-2 pins (the parity corpus alone does not
// cover the truncation/continue control-flow distinctions these encode).
// ---------------------------------------------------------------------------

// TestResolveExtractionParity_R2_TruncatedDict pins EARLY-RETURN #2
// (resolve.go non-UAF mapper.resolveOne err → recordStageTiming(); return
// dict). A non-UAF stage whose EndpointRef references an UNRESOLVABLE
// clientconfig forces resolveOne to error; the resolve truncates and the
// downstream stage is ABSENT from the returned dict.
//
// Mechanism: no internal endpoint is attached to ctx and the stage carries a
// named EndpointRef whose clientconfig Secret does not exist → mapper.resolveOne
// returns an error → resolve.go EARLY-RETURN #2 (return dict before the
// downstream stage). dict therefore carries NEITHER the first stage's output
// NOR the downstream's.
func TestResolveExtractionParity_R2_TruncatedDict(t *testing.T) {
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	// Stage 1: a named EndpointRef that cannot resolve (no clientconfig
	// Secret, no internal endpoint on ctx, empty rest.Config Host →
	// rest.RESTClientFor fails at client construction, network-free).
	// resolveOne errors → R-2.
	failingStage := &templates.API{
		Name: "failing",
		Path: iterItemPath("x"),
		Verb: ptr.To(http.MethodGet),
		EndpointRef: &templates.Reference{
			Name:      "nonexistent-endpoint",
			Namespace: "default",
		},
		Filter: ptr.To(".failing"),
	}
	// Stage 2: a downstream stage that WOULD succeed if reached — its absence
	// proves the truncation.
	downstream := &templates.API{
		Name:      "downstream",
		Path:      iterItemPath("down"),
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: "failing"},
		Filter:    ptr.To(".downstream"),
	}

	// IMPORTANT: do NOT attach an internal endpoint — so resolveOne must try
	// to resolve the named EndpointRef and fail.
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "parity-user"}),
	)
	dict := Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               []*templates.API{failingStage, downstream},
		RESTActionNamespace: "default",
		RESTActionName:      "resolve-extraction-parity-r2",
	})

	if _, ok := dict["downstream"]; ok {
		t.Errorf("R-2: downstream stage present — resolve did NOT truncate on the endpoint-resolve error. dict keys=%v", keysOf(dict))
	}
	if _, ok := dict["failing"]; ok {
		t.Errorf("R-2: failing stage produced output — expected truncation before any dispatch. dict keys=%v", keysOf(dict))
	}
}

// TestResolveExtractionParity_C2_UAFEmptyResultDownstreamRuns pins the
// EARLY-CONTINUE C-2 path: a UAF stage whose SA endpoint is unavailable lands
// dict[id]=={items:[]} (NOT a truncation) AND the downstream stage STILL runs.
// This is the distinguishing contrast to R-2 (truncate vs continue-with-empty).
func TestResolveExtractionParity_C2_UAFEmptyResultDownstreamRuns(t *testing.T) {
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")
	f := newIterFixture(t, 1, nil, false)

	uafStage := &templates.API{
		Name: "uafStage",
		Path: "/apis/example.test/v1/anyplural",
		Verb: ptr.To(http.MethodGet),
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:     "list",
			Group:    "example.test",
			Resource: "anyplural",
		},
	}
	downstream := &templates.API{
		Name:      "downstream",
		Path:      iterItemPath("down"),
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: "uafStage"},
		Filter:    ptr.To(".downstream"),
	}

	dict := parityResolveHTTP(parityHTTPCtx(f), f, []*templates.API{uafStage, downstream}, nil)

	// C-2: dict["uafStage"] == {"items": []}
	uafOut, ok := dict["uafStage"].(map[string]any)
	if !ok {
		t.Fatalf("C-2: dict[\"uafStage\"] is %T, want map[string]any{items:[]}. dict keys=%v", dict["uafStage"], keysOf(dict))
	}
	items, ok := uafOut["items"].([]any)
	if !ok {
		t.Fatalf("C-2: dict[\"uafStage\"][\"items\"] is %T, want []any{}", uafOut["items"])
	}
	if len(items) != 0 {
		t.Errorf("C-2: dict[\"uafStage\"][\"items\"] len=%d, want 0 (empty result on SA-endpoint err)", len(items))
	}
	// C-2: the downstream stage STILL ran (continue, not return).
	if _, ok := dict["downstream"]; !ok {
		t.Errorf("C-2: downstream stage ABSENT — the UAF SA-endpoint error wrongly truncated the resolve. dict keys=%v", keysOf(dict))
	}
}

// TestResolveExtractionParity_UserInfoErrEarlyExit pins R-pre3: a ctx with NO
// UserInfo makes xcontext.UserInfo(ctx) error → Resolve logs an Error event
// and returns an EMPTY map (no stage runs). Captures the slog Error event too.
func TestResolveExtractionParity_UserInfoErrEarlyExit(t *testing.T) {
	logEvents := captureSlogEvents(t, func(ctx context.Context) {
		// ctx carries a logger (injected by captureSlogEvents) but NO UserInfo.
		stage := &templates.API{
			Name:   "neverRuns",
			Path:   "/items/x",
			Verb:   ptr.To(http.MethodGet),
			Filter: ptr.To(".neverRuns"),
		}
		dict := Resolve(ctx, ResolveOptions{
			RC:                  &rest.Config{},
			Items:               []*templates.API{stage},
			RESTActionNamespace: "default",
			RESTActionName:      "resolve-extraction-parity-userinfo",
		})
		if len(dict) != 0 {
			t.Errorf("UserInfo-err: expected empty dict, got %d keys: %v", len(dict), keysOf(dict))
		}
	})

	// The R-pre3 Error event must be present.
	if !slogEventsContain(logEvents, "unable to fetch user info from context") {
		t.Errorf("UserInfo-err: expected the 'unable to fetch user info from context' Error event; got events=%v", logEvents)
	}
	// No stage-level event should have fired (early-exit before the loop).
	if slogEventsContain(logEvents, "api successfully resolved") {
		t.Errorf("UserInfo-err: a stage event fired despite the early exit; events=%v", logEvents)
	}
}

// ---------------------------------------------------------------------------
// slog-event-order pin — the method-call sequence the extraction introduces
// must preserve emit order across a 2-stage resolve.
// ---------------------------------------------------------------------------

// TestResolveExtractionParity_SlogEventOrder pins the ordered sequence of
// slog event MESSAGES emitted by a 2-stage httpcall resolve. The extraction
// reorders inline code into method calls; this asserts the orchestrator calls
// them in the same order the inline code ran, so emit order is preserved.
//
// The assertion is a SUBSEQUENCE match on the load-bearing ordered events
// (pagination → base dict → per-stage "api successfully resolved" ×2) rather
// than an exhaustive full-log equality, because Debug-level events
// (dep.recorded, "calling api", endpoint-resolved) are interleaved by the
// concurrent worker and are not order-stable at parallelism>1. We pin
// PARALLELISM=1 and assert the Info-level spine, which IS order-stable.
func TestResolveExtractionParity_SlogEventOrder(t *testing.T) {
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	// Two non-iterator GET stages, stage2 depends on stage1 → topologicalSort
	// runs stage1 then stage2 deterministically.
	f := newIterFixture(t, 1, nil, false)
	stage1 := &templates.API{
		Name:   "stage1",
		Path:   iterItemPath("a"),
		Verb:   ptr.To(http.MethodGet),
		Filter: ptr.To(".stage1"),
	}
	stage2 := &templates.API{
		Name:      "stage2",
		Path:      iterItemPath("b"),
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: "stage1"},
		Filter:    ptr.To(".stage2"),
	}

	events := captureSlogEventsAt(t, slog.LevelInfo, func(ctx context.Context) {
		// ctx carries the capture logger; add UserInfo (so Resolve does NOT
		// early-exit on the UserInfo guard) + the fixture endpoint.
		ctx = xcontext.BuildContext(ctx, xcontext.WithUserInfo(jwtutil.UserInfo{Username: "parity-user"}))
		ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: f.server.URL})
		_ = Resolve(ctx, ResolveOptions{
			RC:                  &rest.Config{},
			Items:               []*templates.API{stage1, stage2},
			RESTActionNamespace: "default",
			RESTActionName:      "resolve-extraction-parity-slog",
		})
	})

	// The Info-level spine, in order:
	//   "pagination options"           (P0)
	//   "base dict for api resolver"   (P3)
	//   "api successfully resolved"    (stage1)
	//   "api successfully resolved"    (stage2)
	wantSubsequence := []string{
		"pagination options",
		"base dict for api resolver",
		"api successfully resolved",
		"api successfully resolved",
	}
	if !isSubsequence(events, wantSubsequence) {
		t.Errorf("slog-order: emitted Info events do not contain the expected ordered spine.\n events: %v\n want subsequence: %v", events, wantSubsequence)
	}
}

// ---------------------------------------------------------------------------
// slog capture helpers — reuse the slog.NewJSONHandler capture idiom from
// internal_dispatch_paged_test.go:243 (withSlogLogger attaches the *slog.Logger
// to ctx). These extract the ordered "msg" field sequence.
// ---------------------------------------------------------------------------

// captureSlogEvents runs fn with a ctx carrying a JSON slog handler at Debug
// level (captures everything) and returns the ordered slice of event messages.
func captureSlogEvents(t *testing.T, fn func(ctx context.Context)) []string {
	return captureSlogEventsAt(t, slog.LevelDebug, fn)
}

// captureSlogEventsAt is captureSlogEvents at a chosen minimum level.
func captureSlogEventsAt(t *testing.T, level slog.Level, fn func(ctx context.Context)) []string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	// A locking writer — concurrent workers emit from multiple goroutines.
	lw := &lockingWriter{mu: &mu, b: &buf}
	h := slog.NewJSONHandler(lw, &slog.HandlerOptions{Level: level})
	logger := slog.New(h)
	ctx := withSlogLogger(context.Background(), logger)
	fn(ctx)

	mu.Lock()
	defer mu.Unlock()
	var events []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec.Msg != "" {
			events = append(events, rec.Msg)
		}
	}
	return events
}

type lockingWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w *lockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// slogEventsContain reports whether any captured event equals msg.
func slogEventsContain(events []string, msg string) bool {
	for _, e := range events {
		if e == msg {
			return true
		}
	}
	return false
}

// isSubsequence reports whether want appears as an ordered (not necessarily
// contiguous) subsequence of events.
func isSubsequence(events, want []string) bool {
	i := 0
	for _, e := range events {
		if i < len(want) && e == want[i] {
			i++
		}
	}
	return i == len(want)
}
