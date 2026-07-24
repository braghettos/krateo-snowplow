// phase1_roots_unthrottled_test.go — #120 falsifier for the boot-critical
// navigation-roots read.
//
// ROOT CAUSE (#120): the roots-read dynamic client at phase1_walk.go was
// built directly from the shared rc (main.go's rest.InClusterConfig()),
// which carries no QPS / RateLimiter — so client-go synthesises its default
// 5-QPS / 10-burst token-bucket limiter for it. On a slow-apiserver boot the
// very first roots read (the frontend-config ConfigMap Get) can lose its
// tryThrottle Wait to the startupProbe ctx deadline BEFORE the request is
// sent — no nav roots, no recursive walk, proactive informer registration
// starved.
//
// FIX (#120): rootsReadRESTConfig(rc) returns a shallow copy of rc with
// QPS=-1 / Burst=0, so the roots client's limiter is nil (client-go only
// builds a bucket when qps > 0) and tryThrottleWithInfo returns immediately
// — the boot-critical roots path can never deadline on the throttle.
//
// These are HERMETIC unit tests: they drive the REAL client-go throttle path
// (dynamic.NewForConfig → Request.tryThrottle → RateLimiter.Wait) against an
// httptest apiserver, exercising the exact production helper
// rootsReadRESTConfig. No cluster.
//
// ARMS:
//   - RED (pre-fix posture): the client built from the UN-tuned rc with a
//     PRE-DRAINED token bucket (the boot condition) + a tight ctx — the read
//     DEADLINES in tryThrottle, proving the limiter is the cause.
//   - GREEN (with fix): the SAME drained-bucket rc passed through
//     rootsReadRESTConfig — the limiter is dropped, so the SAME tight-ctx read
//     COMPLETES.
//   - NON-MUTATION: rootsReadRESTConfig(rc) does NOT mutate the shared rc's
//     QPS / Burst / RateLimiter — the shared-rc safety invariant the fix
//     depends on (rc also backs the informer/metadata/discovery/config-vars/
//     secrets/controller-health clients).

package dispatchers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// configMapReplyServer is a minimal apiserver that answers the roots
// ConfigMap Get with a valid (empty) ConfigMap object. It records whether the
// HTTP handler was ever reached — the discriminator between "throttle
// deadlined before the request left client-go" and "the request actually ran".
type configMapReplyServer struct {
	*httptest.Server
	reached bool
}

func newConfigMapReplyServer(t *testing.T) *configMapReplyServer {
	t.Helper()
	s := &configMapReplyServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.reached = true
		w.Header().Set("Content-Type", "application/json")
		// A well-formed ConfigMap so the dynamic Get decodes cleanly. The
		// test only cares that the read COMPLETES vs deadlines, not the body.
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x","namespace":"krateo-system"},"data":{"config.json":"{}"}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

// drainedBucketConfig builds a *rest.Config pointed at srv whose RateLimiter
// is a real token bucket with its single token already consumed — the
// slow-boot condition where the next Wait must block for a refill. This is
// the client-go default-limiter posture (a bucket that gates requests), made
// deterministic by pre-draining rather than depending on wall-clock QPS.
func drainedBucketConfig(t *testing.T, srv *configMapReplyServer) *rest.Config {
	t.Helper()
	rl := flowcontrol.NewTokenBucketRateLimiter(1, 1)
	if !rl.TryAccept() {
		t.Fatalf("precondition: fresh burst-1 bucket should grant its first token")
	}
	// Bucket now empty; the next Wait blocks ~1s for a refill at QPS=1.
	return &rest.Config{
		Host:        srv.URL,
		RateLimiter: rl,
	}
}

// getRootsConfigMap performs the SAME dynamic ConfigMap Get the roots read
// uses (readFrontendConfig → dynCli.Resource(configMapGVR).Get), through the
// real client-go throttle path, under the given ctx. dynCfg is the config the
// dynamic client is built from — the arm under test.
func getRootsConfigMap(ctx context.Context, dynCfg *rest.Config) error {
	cli, err := k8sdynamic.NewForConfig(dynCfg)
	if err != nil {
		return err
	}
	_, err = cli.Resource(configMapGVR).Namespace("krateo-system").Get(ctx, "x", metav1.GetOptions{})
	return err
}

// TestRootsRead_DrainedLimiter_DeadlinesWithoutFix is the RED arm: the pre-fix
// posture (un-tuned rc + drained default-style bucket) under a tight ctx
// DEADLINES in the client-side throttle before the request is sent.
func TestRootsRead_DrainedLimiter_DeadlinesWithoutFix(t *testing.T) {
	srv := newConfigMapReplyServer(t)
	rc := drainedBucketConfig(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := getRootsConfigMap(ctx, rc) // NO fix: rc used as-is
	if err == nil {
		t.Fatalf("RED arm expected the roots read to fail (throttle should deadline), got nil error")
	}
	// client-go wraps the throttle-time ctx-deadline as a rate-limiter error.
	if !strings.Contains(err.Error(), "rate limiter Wait") {
		t.Fatalf("RED arm expected a client rate-limiter Wait deadline error, got: %v", err)
	}
	if srv.reached {
		t.Fatalf("RED arm: the HTTP request should NEVER have left client-go (throttle deadlined first), but the apiserver handler was reached")
	}
}

// TestRootsRead_Unthrottled_CompletesWithFix is the GREEN arm: the SAME
// drained-bucket rc, once passed through rootsReadRESTConfig (the fix), has no
// limiter — so the SAME tight-ctx read COMPLETES and reaches the apiserver.
func TestRootsRead_Unthrottled_CompletesWithFix(t *testing.T) {
	srv := newConfigMapReplyServer(t)
	rc := drainedBucketConfig(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := getRootsConfigMap(ctx, rootsReadRESTConfig(rc)) // fix applied
	if err != nil {
		t.Fatalf("GREEN arm expected the roots read to complete under the fix, got: %v", err)
	}
	if !srv.reached {
		t.Fatalf("GREEN arm: the apiserver handler should have been reached (no throttle to deadline)")
	}
}

// TestRootsReadRESTConfig_DoesNotMutateSharedConfig pins the load-bearing
// safety invariant: the fix COPIES rc before tuning, so the shared rc — which
// also backs the informer factory, metadata, discovery, config-vars-watch,
// secrets and controller-health clients — keeps its throttling untouched.
func TestRootsReadRESTConfig_DoesNotMutateSharedConfig(t *testing.T) {
	origLimiter := flowcontrol.NewTokenBucketRateLimiter(5, 10)
	rc := &rest.Config{
		Host:        "https://example.invalid",
		QPS:         5,
		Burst:       10,
		RateLimiter: origLimiter,
	}

	got := rootsReadRESTConfig(rc)

	// The COPY carries the un-throttled posture.
	if got.QPS != -1 {
		t.Errorf("roots config QPS = %v, want -1", got.QPS)
	}
	if got.Burst != 0 {
		t.Errorf("roots config Burst = %v, want 0", got.Burst)
	}
	if got.RateLimiter != nil {
		t.Errorf("roots config RateLimiter = %v, want nil (QPS<=0 only disables throttling when RateLimiter is nil)", got.RateLimiter)
	}
	if got == rc {
		t.Errorf("rootsReadRESTConfig returned the SAME pointer as rc; must be a copy")
	}

	// The shared rc is UNTOUCHED — the whole point of copying.
	if rc.QPS != 5 {
		t.Errorf("shared rc.QPS mutated to %v, want 5 (unchanged)", rc.QPS)
	}
	if rc.Burst != 10 {
		t.Errorf("shared rc.Burst mutated to %v, want 10 (unchanged)", rc.Burst)
	}
	if rc.RateLimiter != origLimiter {
		t.Errorf("shared rc.RateLimiter changed; want the original limiter pointer preserved")
	}
}
