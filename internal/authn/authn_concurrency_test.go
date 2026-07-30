// authn_concurrency_test.go — L6 hermetic falsifiers for the authn.Client
// exchange-and-cache critical section:
//
//	(a) TestToken_ConcurrentExchange_SingleFlight (-race) — 50 goroutines racing
//	    a cold cache must produce EXACTLY ONE upstream token exchange; the
//	    remaining 49 observe the just-cached JWT. Guarded by the Token() mutex
//	    (tokenLock/tokenUnlock var-seam). RED arm: neuter the seam to no-ops →
//	    every goroutine enters exchange() → >1 exchange under -race.
//	(b) TestToken_ClockSkewBoundary — with an INJECTED clock, a cached JWT is
//	    served until exactly refreshSkew (60s) before its exp, and a re-read one
//	    tick past that boundary (advancing the clock to exp-60s) re-exchanges.
//
// Pure in-memory: stub transport + temp token file, injected c.now clock. No
// network, no wall-clock sleeps.

package authn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedTransport is a stub RoundTripper that, on each call, records the
// exchange, announces its arrival on arrived, then blocks until release is
// closed. This lets the test hold every in-flight exchange OPEN simultaneously
// so a missing mutex (concurrent exchange()) is observable, with no sleeps.
type gatedTransport struct {
	calls   atomic.Int64
	arrived chan struct{}
	release chan struct{}
	issued  string
}

func (g *gatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g.calls.Add(1)
	// Announce this exchange began (non-blocking: arrived is buffered wide).
	select {
	case g.arrived <- struct{}{}:
	default:
	}
	<-g.release // hold the exchange open until the test releases it
	body, _ := json.Marshal(map[string]string{"accessToken": g.issued})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
}

// runConcurrentExchange drives the shared-single-flight scenario and returns
// the number of upstream exchanges that actually occurred. Reused by the GREEN
// assertion and the RED arm.
func runConcurrentExchange(t *testing.T, goroutines int) int64 {
	t.Helper()
	tokenPath := writeTokenFile(t, "sa-token-concurrent")
	g := &gatedTransport{
		arrived: make(chan struct{}, goroutines),
		release: make(chan struct{}),
		issued:  makeJWT(time.Now().Add(time.Hour).Unix()),
	}
	c := New("http://authn.test:8082", tokenPath).WithHTTPClient(&http.Client{Transport: g})

	start := make(chan struct{})
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // all goroutines block here so they contend on the cold cache
			tok, err := c.Token(context.Background())
			if err != nil {
				t.Errorf("goroutine %d Token: %v", idx, err)
				return
			}
			tokens[idx] = tok
		}(i)
	}
	close(start) // unleash the herd against the empty cache

	// Wait for at least one exchange to be IN FLIGHT, then release everyone.
	// GREEN: exactly one goroutine reaches the transport (the rest wait on the
	// mutex, then observe the cached token). RED (mutex neutered): many are in
	// flight simultaneously and all are counted after release.
	<-g.arrived
	close(g.release)
	wg.Wait()

	// Every goroutine must have gotten the SAME issued JWT (a torn/empty token
	// would mean the cache handed back a half-written value — a data race).
	for i, tok := range tokens {
		if tok != g.issued {
			t.Errorf("goroutine %d got %q, want the single issued JWT", i, tok)
		}
	}
	return g.calls.Load()
}

// TestToken_ConcurrentExchange_SingleFlight (falsifier a). Run with -race.
func TestToken_ConcurrentExchange_SingleFlight(t *testing.T) {
	const goroutines = 50
	n := runConcurrentExchange(t, goroutines)
	if n != 1 {
		t.Fatalf("concurrent cold-cache Token: %d exchanges, want exactly 1 (the mutex must single-flight the exchange)", n)
	}
}

// TestToken_ConcurrentExchange_RedArm_MutexNeutered proves the (a) falsifier
// DISCRIMINATES: with the tokenLock/tokenUnlock seam transiently neutered to
// no-ops (removing the mutual exclusion Token() relies on), the same 50
// goroutines race into exchange() concurrently and produce MORE THAN ONE
// exchange — the RED the guarded path prevents. Restores the real seam after.
//
// It is env-gated (AUTHN_RED_ARM=1) because neutering the mutex ALSO
// unsynchronises the c.cached/c.expiry writes, so under `go test -race` the
// detector (correctly) reports the data race and fails the run — the whole
// point of the falsifier. We do NOT commit an always-run test that reds the
// -race suite (established repo convention: the committed test is GREEN and the
// RED is reproducible on demand). Observed RED on 2026-07-30:
//
//	$ AUTHN_RED_ARM=1 go test ./internal/authn/ -race -count=1 \
//	    -run TestToken_ConcurrentExchange_RedArm_MutexNeutered -v
//	  authn_concurrency_test.go: RED arm: mutex neutered → 9 concurrent exchanges
//	  WARNING: DATA RACE  ... (*Client).Token authn.go:100  c.cached = jwt
//	  --- FAIL (race detected)   [count 9 >> 1, race on cache write]
//
// Restore (real seam) → single-flights to exactly 1, -race clean (the GREEN
// TestToken_ConcurrentExchange_SingleFlight above).
func TestToken_ConcurrentExchange_RedArm_MutexNeutered(t *testing.T) {
	if os.Getenv("AUTHN_RED_ARM") != "1" {
		t.Skip("RED arm (mutex-neutered) — set AUTHN_RED_ARM=1 to reproduce; it intentionally reds -race")
	}
	origLock, origUnlock := tokenLock, tokenUnlock
	tokenLock = func(*Client) {}   // neuter: no mutual exclusion
	tokenUnlock = func(*Client) {} // neuter
	t.Cleanup(func() { tokenLock, tokenUnlock = origLock, origUnlock })

	const goroutines = 50
	n := runConcurrentExchange(t, goroutines)
	if n <= 1 {
		t.Fatalf("RED arm expected >1 concurrent exchange with the mutex neutered, got %d — the (a) falsifier does not discriminate", n)
	}
	t.Logf("RED arm: mutex neutered → %d concurrent exchanges (guarded path single-flights to 1)", n)
}

// TestToken_ClockSkewBoundary (falsifier b). Drives the refreshSkew boundary
// with an INJECTED clock: a cached JWT is served for reads strictly before
// exp-refreshSkew, and a read AT/after exp-refreshSkew re-exchanges. No
// wall-clock: the test pins and advances c.now itself.
func TestToken_ClockSkewBoundary(t *testing.T) {
	tokenPath := writeTokenFile(t, "sa-token")

	// A fixed clock the test advances by hand.
	var mu sync.Mutex
	nowT := time.Unix(1_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return nowT }
	advance := func(d time.Duration) { mu.Lock(); nowT = nowT.Add(d); mu.Unlock() }

	// The issued token expires 600s after the initial clock reading.
	const lifetime = 600 * time.Second
	exp := nowT.Add(lifetime)

	var calls atomic.Int64
	c := New("http://authn.test:8082", tokenPath).WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			body, _ := json.Marshal(map[string]string{"accessToken": makeJWT(exp.Unix())})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
		}),
	})
	c.now = clock

	// First read: cold → 1 exchange.
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("initial Token: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cold read: %d exchanges, want 1", calls.Load())
	}

	// Advance to JUST INSIDE the skew boundary: exp-refreshSkew minus 1s. The
	// cache-valid predicate is now().Before(exp - refreshSkew); at exp-61s that
	// is TRUE → served from cache, no re-exchange.
	advance(lifetime - refreshSkew - 1*time.Second) // now = exp - 61s
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("near-boundary Token: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("read at exp-61s must be a cache HIT; got %d exchanges, want 1", calls.Load())
	}

	// Advance one more tick to reach the boundary exactly (now = exp-60s =
	// exp-refreshSkew). now().Before(exp-refreshSkew) is FALSE at equality →
	// re-exchange.
	advance(1 * time.Second) // now = exp - 60s == exp - refreshSkew
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("boundary Token: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("read at exactly exp-refreshSkew (exp-60s) must RE-EXCHANGE; got %d exchanges, want 2", calls.Load())
	}
}
