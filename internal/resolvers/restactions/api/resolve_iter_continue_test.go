// resolve_iter_continue_test.go — Task #313 / 0.30.257 falsifiers.
//
// THE CHANGE UNDER TEST (design package C-A + W-A + Cache-A, ratified in
// docs/task-313-restaction-iteration-error-skip-trace-2026-06-11.md):
//
//   C-A: a per-ITEM hard error inside a `dependsOn.iterator` api-stage no
//        longer cancels the errgroup ctx + early-returns the truncated
//        dict. Each worker records its error into a pre-sized index-aligned
//        itemErrs slot and returns nil; the stage runs ALL items; downstream
//        stages run. GENUINE caller cancellation is preserved (gctx still
//        derives from the request ctx; a stage-loop-top ctx.Err() guard
//        aborts downstream stages promptly).
//
//   W-A: dict[call.ErrorKey] is an ACCUMULATING slice (first error
//        []any{e0}, subsequent append) written under dictMu. This also
//        fixes the pre-existing ContinueOnError=true last-wins bug.
//
// HARNESS: a plain httptest server (HTTP, no TLS — httpcall.Do over http://
// needs no CA) answers per-item paths; the iterator fans over an array
// carried in Extras; each item's Path embeds its element so the server can
// return 500 for chosen indices. No apiserver path prefix (/items/...) so
// the informer pivot / dep-recording / internal-rest-config branches are
// all skipped — the dispatch falls straight to httpcall.Do (resolve.go:835,
// StatusFailure at :836). This is the SAME real-Resolve-driving model as
// resolve_jwt_leak_test.go + internal_dispatch_paged_test.go.
//
// RETRY KNOBS: httpcall.Do wraps a RetryClient that retries 5xx
// CLIENT_MAX_RETRIES times (default 5) × CLIENT_BASE_BACKOFF (default
// 500ms) — a GET (idempotent) 500 would otherwise add ~15s per failing
// item. These tests pin CLIENT_MAX_RETRIES=0 so a 500 fails fast.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// iterFailFastRetries pins the httpcall RetryClient knobs so a per-item
// 500 fails on the first attempt — without this a GET 500 retries 5× with
// 500ms exponential backoff (~15s/item) and the iterator tests would crawl.
func iterFailFastRetries(t *testing.T) {
	t.Helper()
	t.Setenv("CLIENT_MAX_RETRIES", "0")
	t.Setenv("CLIENT_BASE_BACKOFF", "1ms")
	t.Setenv("CLIENT_MAX_BACKOFF", "1ms")
}

// iterItemPath is the per-item REST path. The iterator element carries
// `{"ns": "<v>"}`; the stage Path template `${ "/items/" + .ns }` evaluates
// to "/items/<v>" per item. "/items/..." is NOT an apiserver path
// (ParseAPIServerPathToGVR rejects the non-/api(s)/ prefix), so the dispatch
// bypasses the informer pivot / dep recording / internal-rest-config branch
// and reaches httpcall.Do directly.
func iterItemPath(v string) string { return "/items/" + v }

// iterStageVals builds the iterator element array carried in Extras and the
// matching jq iterator query + Path template.
//
//	Extras = {"vals": [{"ns":"v0"},{"ns":"v1"},...]}
//	iterator query = ".vals"   (jqutil.ForEach requires the query to YIELD a
//	                             JSON array — `.vals[]` streams elements and
//	                             json.Unmarshal of the concatenation fails)
//	Path template  = `${ "/items/" + .ns }`
func iterStageVals(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{"ns": fmt.Sprintf("v%d", i)})
	}
	return out
}

// newIterFixture stands up an HTTP server answering /items/<v>. For an index
// in failIdx the server returns HTTP 500 with a small JSON Status-shaped
// body (so httpcall.Do's StatusFailure → response.AsMap yields a non-empty
// map, matching the resolve.go:842-852 asMap branch); otherwise it returns a
// 200 JSON object {"name":"<v>"} so jsonHandlerCore merges it into dict[id].
//
// The optional block channel, when non-nil, makes EVERY item block on
// <-r.Context().Done() — the cancellation falsifier uses it to hold all
// items until the caller ctx is cancelled.
type iterFixture struct {
	server  *httptest.Server
	calls   atomic.Int64
	served  sync.Map // path -> struct{} : which item paths actually got a response
	failIdx map[int]struct{}
	block   bool
	started chan string // item path -> signalled once when an item handler begins (buffered)
}

func newIterFixture(t *testing.T, n int, failIdx map[int]struct{}, block bool) *iterFixture {
	t.Helper()
	f := &iterFixture{
		failIdx: failIdx,
		block:   block,
		started: make(chan string, n+4),
	}
	// Map path -> index for the 500 decision.
	idxOf := map[string]int{}
	for i := 0; i < n; i++ {
		idxOf[iterItemPath(fmt.Sprintf("v%d", i))] = i
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		path := r.URL.Path
		select {
		case f.started <- path:
		default:
		}
		if f.block {
			// Hold until the caller ctx is cancelled; then return a ctx
			// error shape. httpcall.Do's request carries gctx, so a
			// caller-cancel aborts the in-flight request here.
			<-r.Context().Done()
			return
		}
		idx, known := idxOf[path]
		if known {
			if _, fail := f.failIdx[idx]; fail {
				// HTTP 403 (a realistic per-namespace iterator failure —
				// design §2.5.1) with a valid Status-shaped body. A 4xx is
				// returned by the httpcall RetryClient WITHOUT retry/error-
				// wrap (retry.go:104 returns non-5xx/non-429 verbatim), so
				// httpcall.Do reads + unmarshals THIS body into the Status it
				// surfaces — preserving the per-item message so the falsifier
				// can identify WHICH item failed. (A 500 would be error-wrapped
				// by the retry client into a generic "server error" string,
				// discarding the body — still a StatusFailure, but opaque.)
				// response.AsMap then yields a non-empty map (resolve.go asMap
				// branch).
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":403,"reason":"Forbidden","message":"item %d (%s) deliberately failed"}`, idx, path)
				return
			}
		}
		f.served.Store(path, struct{}{})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Object keyed so the stage filter `.<id>` accumulates one entry
		// per successful item (jsonHandlerCore append semantics).
		fmt.Fprintf(w, `{"name":%q}`, strings.TrimPrefix(path, "/items/"))
	}))
	t.Cleanup(srv.Close)
	f.server = srv
	return f
}

// iterStage builds a one-stage RESTAction whose `dependsOn.iterator` fans
// the Extras "vals" array. continueOnError is parameterised; errorKey
// defaults to "error" when key=="".
func iterStage(id, errorKey string, continueOnError *bool) *templates.API {
	s := &templates.API{
		Name: id,
		Path: `${ "/items/" + .ns }`,
		Verb: ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{
			Iterator: ptr.To(".vals"),
		},
		ContinueOnError: continueOnError,
	}
	if errorKey != "" {
		s.ErrorKey = ptr.To(errorKey)
	}
	return s
}

// runIterResolve drives the REAL api.Resolve over the fixture server with
// the given stages + Extras. ctx is the caller-supplied context (so the
// cancellation test can pass a cancellable ctx). The fixture endpoint is
// attached via WithInternalEndpoint so resolveOne returns it without a
// per-user clientconfig Secret lookup.
func runIterResolve(ctx context.Context, f *iterFixture, stages []*templates.API, extras map[string]any) map[string]any {
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: f.server.URL})
	return Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               stages,
		Extras:              extras,
		RESTActionNamespace: "default",
		RESTActionName:      "iter-continue-falsifier",
	})
}

// iterResolveCtx builds the base resolve context: UserInfo (Resolve bails
// without it) on a fresh background context.
func iterResolveCtx() context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "iter-falsifier-user"}),
	)
}

// ---------------------------------------------------------------------------
// Falsifier 1 — TestResolve_IteratorContinuesPastItemError (RED on HEAD prod)
// ---------------------------------------------------------------------------
//
// N=6 items, item index 3 returns 500, ITER_PARALLELISM=1 (deterministic
// ordering so item 3's failure provably precedes 4..N), continueOnError
// UNSET (default false — proves the new continue semantics are NOT gated on
// ContinueOnError). A SECOND downstream stage (no iterator) proves the stage
// loop did not early-return.
//
// POST-FIX assertions:
//   - dict["items"] carries items 0,1,2,4,5 (item 3 absent from the merged
//     successful set);
//   - dict["error"] (W-A) is a slice carrying item 3's error;
//   - dict["downstream"] is present (the 2nd stage ran — no truncation).
//
// RED on HEAD: today item 3's 500 cancels gctx and resolve.go:896 returns the
// truncated dict — items 4,5 never merge AND the downstream stage is absent.
func TestResolve_IteratorContinuesPastItemError(t *testing.T) {
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	const n = 6
	failIdx := map[int]struct{}{3: {}}
	f := newIterFixture(t, n, failIdx, false)

	// Two stages: the iterator stage (items) and a non-iterator downstream
	// stage (downstream) that depends on items, so topologicalSort runs
	// items first, downstream second. The downstream stage hits item path
	// /items/down — a 200 — so its presence proves the loop continued past
	// the errored item stage.
	itemsStage := iterStage("items", "error", nil) // continueOnError UNSET
	itemsStage.Filter = ptr.To(".items")           // wrap → dict["items"] array

	downstream := &templates.API{
		Name:      "downstream",
		Path:      iterItemPath("down"),
		Verb:      ptr.To(http.MethodGet),
		DependsOn: &templates.Dependency{Name: "items"},
		Filter:    ptr.To(".downstream"),
	}

	extras := map[string]any{"vals": iterStageVals(n)}

	dict := runIterResolve(iterResolveCtx(), f, []*templates.API{itemsStage, downstream}, extras)

	// --- W-A: error key is an accumulating slice carrying item 3's error.
	rawErr, hasErr := dict["error"]
	if !hasErr {
		t.Fatalf("RED-EXPECTED-ON-HEAD: dict[\"error\"] absent; want a slice with item 3's error. dict keys=%v", keysOf(dict))
	}
	errSlice, isSlice := rawErr.([]any)
	if !isSlice {
		t.Fatalf("W-A: dict[\"error\"] is %T, want []any (accumulating slice). value=%v", rawErr, rawErr)
	}
	if len(errSlice) != 1 {
		t.Fatalf("W-A: dict[\"error\"] slice len=%d, want 1 (only item 3 failed). value=%v", len(errSlice), errSlice)
	}
	if !errEntryMentions(errSlice[0], "item 3") && !errEntryMentions(errSlice[0], "v3") {
		t.Errorf("W-A: error entry does not identify the failing item: %#v", errSlice[0])
	}

	// --- C-A: successful items 0,1,2,4,5 present; item 3 (v3) absent.
	names := iterMergedNames(t, dict["items"])
	for _, want := range []string{"v0", "v1", "v2", "v4", "v5"} {
		if !names[want] {
			t.Errorf("C-A: successful item %q missing from dict[\"items\"]; the stage truncated after the error. got=%v", want, names)
		}
	}
	if names["v3"] {
		t.Errorf("C-A: failed item v3 unexpectedly present in dict[\"items\"]: %v", names)
	}

	// --- C-A: downstream stage ran (no early return).
	if _, ok := dict["downstream"]; !ok {
		t.Errorf("C-A: downstream stage absent — the per-item error truncated the whole resolve. dict keys=%v", keysOf(dict))
	}
}

// ---------------------------------------------------------------------------
// Falsifier 2 — TestResolve_MultipleItemErrorsAllRecorded (RED on HEAD prod)
// ---------------------------------------------------------------------------
//
// Items 2 AND 5 return 500. continueOnError=TRUE (so even the pre-fix
// continue path runs every item) — the ONLY thing under test is W-A: BOTH
// errors must land in the accumulating slice (len 2). RED on HEAD: the shared
// errorKey is last-wins, so only ONE error survives.
func TestResolve_MultipleItemErrorsAllRecorded(t *testing.T) {
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	const n = 7
	failIdx := map[int]struct{}{2: {}, 5: {}}
	f := newIterFixture(t, n, failIdx, false)

	stage := iterStage("items", "error", ptr.To(true)) // continueOnError TRUE
	stage.Filter = ptr.To(".items")

	extras := map[string]any{"vals": iterStageVals(n)}
	dict := runIterResolve(iterResolveCtx(), f, []*templates.API{stage}, extras)

	rawErr, hasErr := dict["error"]
	if !hasErr {
		t.Fatalf("dict[\"error\"] absent; want a 2-element slice. dict keys=%v", keysOf(dict))
	}
	errSlice, isSlice := rawErr.([]any)
	if !isSlice {
		t.Fatalf("W-A: dict[\"error\"] is %T, want []any. value=%v", rawErr, rawErr)
	}
	if len(errSlice) != 2 {
		t.Fatalf("RED-EXPECTED-ON-HEAD: dict[\"error\"] slice len=%d, want 2 (items 2 AND 5 failed). "+
			"HEAD is last-wins so only one survives. value=%v", len(errSlice), errSlice)
	}

	// Both failing items identifiable in the aggregate.
	joined := fmt.Sprintf("%v", errSlice)
	for _, want := range []string{"item 2", "item 5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("W-A: aggregate error slice omits %q: %v", want, errSlice)
		}
	}
}

// ---------------------------------------------------------------------------
// Falsifier 3 — TestResolve_CallerCancelStillAbortsRemainingItems
//
//	(GREEN on HEAD AND post-fix — C-A regression guard)
//
// ---------------------------------------------------------------------------
//
// The server BLOCKS every item on <-r.Context().Done(). The caller cancels
// its ctx shortly after the first item begins. The stage must NOT resolve all
// N items (genuine cancellation still aborts the in-flight + queued items),
// and ctx.Err() must be context.Canceled. This proves Option C-A preserved
// genuine caller cancellation (we removed the WORKER as a cancel SOURCE, not
// the parent-ctx propagation).
func TestResolve_CallerCancelStillAbortsRemainingItems(t *testing.T) {
	iterFailFastRetries(t)
	// PARALLELISM=1 so at most one item is in-flight; the rest are queued
	// behind SetLimit and must be aborted by the cancelled gctx.
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	const n = 6
	f := newIterFixture(t, n, nil, true) // block=true: every item hangs on ctx.Done

	stage := iterStage("items", "error", ptr.To(true))
	stage.Filter = ptr.To(".items")
	extras := map[string]any{"vals": iterStageVals(n)}

	ctx, cancel := context.WithCancel(iterResolveCtx())

	// Cancel once the first item handler has begun (so cancellation lands
	// mid-stage, not before it starts).
	go func() {
		select {
		case <-f.started:
		case <-time.After(2 * time.Second):
		}
		cancel()
	}()

	done := make(chan map[string]any, 1)
	go func() { done <- runIterResolve(ctx, f, []*templates.API{stage}, extras) }()

	select {
	case <-done:
		// Resolve returned (the cancelled gctx aborted the blocked item).
	case <-time.After(10 * time.Second):
		t.Fatal("FALSIFIER-CANCEL FAIL: Resolve did not return within 10s after caller cancel — " +
			"genuine cancellation no longer propagates through gctx (C-A regression).")
	}

	if ctx.Err() != context.Canceled {
		t.Fatalf("FALSIFIER-CANCEL FAIL: ctx.Err()=%v, want context.Canceled", ctx.Err())
	}

	// The walk must NOT have served all N items. With block=true no item
	// EVER returns a body, so served count stays 0; the discriminating
	// assertion is that the server saw FEWER than N item-starts (the queued
	// items behind SetLimit never started because the gctx was cancelled).
	if got := f.calls.Load(); got >= int64(n) {
		t.Fatalf("FALSIFIER-CANCEL FAIL: server saw %d calls (>= N=%d) despite caller cancel — "+
			"remaining items were NOT aborted.", got, n)
	}
}

// NOTE on the stage-loop-top ctx.Err() guard (#313 C-A §2.1.1): a black-box
// "downstream stage did not dispatch on cancel" falsifier was prototyped and
// REMOVED — it could not discriminate. Without the guard, a cancelled gctx
// still makes stage-2's httpcall.Do(gctx, …) fail BEFORE the request leaves
// the client, so the server observes nothing either way; the guard's win
// (skipping stage-2's createRequestOptions / jq-eval / worker spawn) has no
// black-box server-side signal. Genuine-cancellation PRESERVATION is covered
// by TestResolve_CallerCancelStillAbortsRemainingItems above (the C-A
// regression guard the design names); the loop-top guard itself is a prompt-
// abort optimisation visible in the resolve.go diff.

// ---------------------------------------------------------------------------
// Falsifier 4 — TestResolve_IteratorErrorCollection_Race (-race REQUIRED)
// ---------------------------------------------------------------------------
//
// PARALLELISM=32, a MIX of success + 500s, M concurrent Resolve calls each
// over its OWN stage + fixture. Exercises BOTH the pre-sized itemErrs
// disjoint-index writes AND the W-A dict[errorKey] accumulating-append under
// dictMu, concurrently. Per feedback_shared_vs_copy_is_a_concurrency_change
// this is the mandatory -race proof: an unlocked dict[errorKey] append (or a
// shared itemErrs grow) FAILS under -race; the design's pre-sized slot +
// dictMu-held append PASS.
func TestResolve_IteratorErrorCollection_Race(t *testing.T) {
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "32")

	const (
		workers = 12
		n       = 16
	)
	// Items 1, 4, 9, 13 fail in every worker — a non-trivial multi-fail mix
	// so the W-A append fires repeatedly under concurrency.
	failIdx := map[int]struct{}{1: {}, 4: {}, 9: {}, 13: {}}

	// ONE shared fixture (one httptest server) — its handler is
	// concurrency-safe (atomic counter + sync.Map). All workers hit it
	// concurrently. Each Resolve has its OWN dict, so the race surface under
	// test is (a) the pre-sized itemErrs disjoint-index writes within one
	// errgroup AND (b) the dictMu-held dict[errorKey] append — both exercised
	// at PARALLELISM=32 within each Resolve, across M parallel Resolves.
	f := newIterFixture(t, n, failIdx, false)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			stage := iterStage("items", "error", ptr.To(true))
			stage.Filter = ptr.To(".items")
			extras := map[string]any{"vals": iterStageVals(n)}
			dict := runIterResolve(iterResolveCtx(), f, []*templates.API{stage}, extras)
			// Touch the error slice so the result tree is realised.
			if es, ok := dict["error"].([]any); ok {
				if len(es) != len(failIdx) {
					t.Errorf("worker %d: error slice len=%d, want %d", w, len(es), len(failIdx))
				}
			} else {
				t.Errorf("worker %d: dict[\"error\"] missing or not []any: %T", w, dict["error"])
			}
		}(w)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Cache-A falsifier — TestResolve_IteratorError_BumpsStageErrorSink
// ---------------------------------------------------------------------------
//
// The request-path Cache-A guard (restactions.go / widgets.go /
// widget_content.go) skips the L1 Put when the resolve's StageErrorSink
// Count() > 0. This test proves the SIGNAL the guard consumes: a resolve
// driven with cache.WithStageErrorSink installed on its ctx (exactly what
// the three dispatcher sites now do) bumps the sink on a per-item iterator
// error, and does NOT bump it on a clean resolve. The dispatcher gate itself
// is a one-line `if sink.Count() > 0 { skip Put }` visible in the diff; this
// is the "unit-test the guard seam" path the design cites (§2.5 / Part 3).
//
//	sub-test "error"  — item 2 fails (403) → sink.Count() == 1 → dispatcher
//	                    would SKIP the Put (partial served, not persisted).
//	sub-test "clean"  — no item fails → sink.Count() == 0 → dispatcher Puts.
//
// Composes with C-A: even WITH the per-item error the stage runs all items
// (the sink is bumped from the SAME dict[errorKey]-write site that C-A left
// intact — design §2.5 item 2: "my change must NOT remove the Bump calls").
func TestResolve_IteratorError_BumpsStageErrorSink(t *testing.T) {
	iterFailFastRetries(t)
	t.Setenv("RESOLVER_ITER_PARALLELISM", "1")

	const n = 5

	t.Run("error_bumps_sink", func(t *testing.T) {
		f := newIterFixture(t, n, map[int]struct{}{2: {}}, false)
		stage := iterStage("items", "error", ptr.To(true))
		stage.Filter = ptr.To(".items")
		extras := map[string]any{"vals": iterStageVals(n)}

		ctx, sink := cache.WithStageErrorSink(iterResolveCtx())
		dict := runIterResolve(ctx, f, []*templates.API{stage}, extras)

		if sink.Count() == 0 {
			t.Fatalf("Cache-A: sink.Count()==0 after a per-item error — the dispatcher "+
				"gate would WRONGLY cache the partial result. dict keys=%v", keysOf(dict))
		}
		// C-A composition check: the stage still ran all items (4 successes
		// merged) despite the error — the Bump did not re-introduce truncation.
		names := iterMergedNames(t, dict["items"])
		for _, want := range []string{"v0", "v1", "v3", "v4"} {
			if !names[want] {
				t.Errorf("Cache-A/C-A: successful item %q missing — Bump must not truncate. got=%v", want, names)
			}
		}
	})

	t.Run("clean_no_bump", func(t *testing.T) {
		f := newIterFixture(t, n, nil, false) // no failures
		stage := iterStage("items", "error", ptr.To(true))
		stage.Filter = ptr.To(".items")
		extras := map[string]any{"vals": iterStageVals(n)}

		ctx, sink := cache.WithStageErrorSink(iterResolveCtx())
		_ = runIterResolve(ctx, f, []*templates.API{stage}, extras)

		if sink.Count() != 0 {
			t.Fatalf("Cache-A: sink.Count()=%d on a CLEAN resolve — the dispatcher gate "+
				"would WRONGLY skip caching a fully-successful result (false positive).",
				sink.Count())
		}
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// iterMergedNames extracts the set of `name` fields from the merged stage
// output dict["items"]. After `.items` filter + N-item accumulation the value
// is either a single object (1 item) or a []any of objects.
func iterMergedNames(t *testing.T, v any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	add := func(e any) {
		if m, ok := e.(map[string]any); ok {
			if s, ok := m["name"].(string); ok {
				out[s] = true
			}
		}
	}
	switch vv := v.(type) {
	case []any:
		for _, e := range vv {
			add(e)
		}
	case map[string]any:
		add(vv)
	}
	return out
}

// errEntryMentions reports whether an error entry (a map from response.AsMap,
// or a plain string) contains substr in its message/value.
func errEntryMentions(entry any, substr string) bool {
	switch e := entry.(type) {
	case string:
		return strings.Contains(e, substr)
	case map[string]any:
		if msg, ok := e["message"].(string); ok && strings.Contains(msg, substr) {
			return true
		}
		b, _ := json.Marshal(e)
		return strings.Contains(string(b), substr)
	default:
		return false
	}
}
