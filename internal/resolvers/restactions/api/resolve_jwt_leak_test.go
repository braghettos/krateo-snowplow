// resolve_jwt_leak_test.go — Ship 0.30.164 falsifier (pre-code) for the
// JWT-leak via in-place mutation of the shared Spec.API[].Headers slice.
//
// THE BUG (TRACED, resolve.go:276-282 pre-0.30.164): when ctx carries
// xcontext.WithAccessToken(<user-bearer>) and the api stage's EndpointRef
// is nil (or ExportJWT==true), the resolver appends
// `Authorization: Bearer <T>` directly into apiCall.Headers — and apiCall
// is the SAME pointer the caller's *templates.RESTAction owns
// (opts.Items[i] aliases cr.Spec.API[i]). The CR is mutated in place; the
// dispatcher then marshals the corrupted CR back to the wire and the JWT
// rides out in the /call response body. Empirical proof:
// /tmp/snowplow-runs/ship-307/before/single-user/cache-off/admin/call-namespaces.json:24-37.
//
// THE FIX (0.30.164, resolve.go): shallow-copy apiCall before appending —
// the stage-local copy gets the augmented Headers; the caller's CR slice
// stays untouched.
//
// FALSIFIER (this file, two tests):
//
//   TestResolve_DoesNotMutateInputAPISpecHeaders — single-goroutine: build
//     a stage with Headers=["Accept: application/json"], call Resolve with
//     ctx carrying a fake access token; after return, assert the input
//     stage.Headers has NOT been extended with an Authorization element.
//     Pre-fix FAILS (len==2, second element matches Authorization:.*).
//
//   TestResolve_ConcurrentRequestsDoNotCrossPollinate — 8 goroutines each
//     owning their own *templates.API copy + distinct access token; run
//     under -race. After all complete, assert NO stage.Headers contains
//     ANY Authorization element. Pre-fix FAILS (every stage has its own
//     token appended).

package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/client-go/rest"
)

// hasAuthorizationHeader reports whether headers contains any element
// matching the case-sensitive prefix "Authorization:" — the exact wire
// shape of the leaked entry.
func hasAuthorizationHeader(headers []string) bool {
	for _, h := range headers {
		if strings.HasPrefix(h, "Authorization:") {
			return true
		}
	}
	return false
}

// jwtLeakResolveCtx builds the per-call context used by the leak
// falsifiers: UserInfo + an internal endpoint (so resolveOne short-
// circuits) + the access token under test. The access token is the
// substrate that the resolve.go:276-282 append site uses to construct
// the (pre-fix) Authorization: Bearer <T> Headers entry.
func jwtLeakResolveCtx(username, accessToken string) context.Context {
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username}),
		xcontext.WithAccessToken(accessToken),
	)
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})
	return ctx
}

// jwtLeakResolveOpts is the F1-shaped ResolveOptions for a single-stage
// drive. The widgets LIST is served by the informer pivot (newF1Watcher
// seeds the watcher), so RC's contents are never dereferenced.
func jwtLeakResolveOpts(stage *templates.API) ResolveOptions {
	return ResolveOptions{
		RC:                  &rest.Config{},
		Items:               []*templates.API{stage},
		RESTActionNamespace: "default",
		RESTActionName:      "jwt-leak-falsifier-restaction",
	}
}

// TestResolve_DoesNotMutateInputAPISpecHeaders is the §6.3 falsifier #1.
// The F1 informer fixture lets dispatch succeed via the informer pivot
// (no real HTTP). After Resolve returns, the caller-owned stage pointer
// MUST still carry only its original Headers — no Authorization element
// appended. Pre-fix this FAILS because resolve.go:279 appended the
// Bearer string into the shared slice.
func TestResolve_DoesNotMutateInputAPISpecHeaders(t *testing.T) {
	newF1Watcher(t)

	stage := f1ListStage("widgets")
	stage.Headers = []string{"Accept: application/json"}

	ctx := jwtLeakResolveCtx(f1BroadUser, "fake-bearer-token-for-leak-falsifier")
	_ = Resolve(ctx, jwtLeakResolveOpts(stage))

	if got := len(stage.Headers); got != 1 {
		t.Errorf("input stage.Headers length: got %d, want 1 (Resolve mutated the caller's slice; full: %v)", got, stage.Headers)
	}
	if hasAuthorizationHeader(stage.Headers) {
		t.Errorf("input stage.Headers contains an Authorization element after Resolve: %v", stage.Headers)
	}
}

// TestResolve_ConcurrentRequestsDoNotCrossPollinate is the §6.3 falsifier
// #2 / AC-164.3. N goroutines each own their OWN *templates.API plus a
// distinct access token; run with -race. After all complete, no stage's
// Headers may contain ANY Authorization element. Pre-fix every stage
// carries its own Bearer token appended; the test fails per-worker.
func TestResolve_ConcurrentRequestsDoNotCrossPollinate(t *testing.T) {
	newF1Watcher(t)

	const workers = 8

	type stageBox struct {
		stage *templates.API
		token string
	}
	boxes := make([]*stageBox, workers)
	for i := 0; i < workers; i++ {
		s := f1ListStage(fmt.Sprintf("widgets-%d", i))
		s.Headers = []string{"Accept: application/json"}
		boxes[i] = &stageBox{stage: s, token: fmt.Sprintf("bearer-token-goroutine-%d", i)}
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			defer wg.Done()
			user := f1BroadUser
			if idx%2 == 1 {
				user = f1NarrowUser
			}
			ctx := jwtLeakResolveCtx(user, boxes[idx].token)
			_ = Resolve(ctx, jwtLeakResolveOpts(boxes[idx].stage))
		}()
	}
	wg.Wait()

	for i, b := range boxes {
		if got := len(b.stage.Headers); got != 1 {
			t.Errorf("worker %d: stage.Headers length: got %d, want 1 (full: %v)", i, got, b.stage.Headers)
		}
		if hasAuthorizationHeader(b.stage.Headers) {
			t.Errorf("worker %d: stage.Headers contains an Authorization element: %v", i, b.stage.Headers)
		}
	}
}
