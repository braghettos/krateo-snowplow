// authn_test.go — unit coverage for the ported authn.Client (the SA→authn
// token exchange the #57 prewarm seed uses to authenticate its nested loopback
// /call). Stub HTTP transport + a temp token file; no network, no cluster.

package authn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc adapts a func to http.RoundTripper for a stub transport.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// makeJWT builds an unsigned-but-shaped JWT with the given exp (seconds).
func makeJWT(exp int64) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payloadJSON, _ := json.Marshal(map[string]any{"exp": exp})
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	return hdr + "." + payload + ".sig"
}

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return p
}

// TestToken_ExchangesAndCaches: a cache miss reads the SA token, POSTs it to
// /serviceaccount/login with the SA token as Bearer, extracts accessToken; a
// second call within the cache window does NOT re-exchange.
func TestToken_ExchangesAndCaches(t *testing.T) {
	tokenPath := writeTokenFile(t, "sa-token-abc")
	issued := makeJWT(time.Now().Add(time.Hour).Unix())

	var calls atomic.Int64
	c := New("http://authn.test:8082", tokenPath).WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			// Verify the exchange request shape.
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if !strings.HasSuffix(r.URL.Path, "/serviceaccount/login") {
				t.Errorf("path = %s, want .../serviceaccount/login", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer sa-token-abc" {
				t.Errorf("Authorization = %q, want the SA token bearer", got)
			}
			body, _ := json.Marshal(map[string]string{"accessToken": issued})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
		}),
	})

	got, err := c.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != issued {
		t.Fatalf("Token = %q, want the issued JWT", got)
	}
	// Second call within the window → cached, no re-exchange.
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("expected exactly 1 exchange (then cache hit); got %d", n)
	}
}

// TestToken_RefreshesNearExpiry: a token within refreshSkew of expiry triggers
// a re-exchange.
func TestToken_RefreshesNearExpiry(t *testing.T) {
	tokenPath := writeTokenFile(t, "sa-token")
	var calls atomic.Int64
	c := New("http://authn.test:8082", tokenPath).WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			// Each issued token expires in 30s — inside the 60s refreshSkew,
			// so it is ALWAYS considered near-expiry → every Token() re-exchanges.
			body, _ := json.Marshal(map[string]string{"accessToken": makeJWT(time.Now().Add(30 * time.Second).Unix())})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
		}),
	})
	for i := 0; i < 3; i++ {
		if _, err := c.Token(context.Background()); err != nil {
			t.Fatalf("Token #%d: %v", i, err)
		}
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("near-expiry token must re-exchange each call; got %d exchanges, want 3", n)
	}
}

// TestToken_AuthnErrorPropagates: a non-200 from authn surfaces as an error
// (the seed degrades on it — C-a — but the client returns the error so the
// caller can WARN+expvar).
func TestToken_AuthnErrorPropagates(t *testing.T) {
	tokenPath := writeTokenFile(t, "sa-token")
	c := New("http://authn.test:8082", tokenPath).WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("no allowlist CR")), Header: make(http.Header)}, nil
		}),
	})
	if _, err := c.Token(context.Background()); err == nil {
		t.Fatalf("a 403 from authn must surface as an error")
	}
}

// TestToken_MissingTokenFile: a missing projected token file surfaces an error
// (degrade path, not a panic).
func TestToken_MissingTokenFile(t *testing.T) {
	c := New("http://authn.test:8082", "/nonexistent/token/path")
	if _, err := c.Token(context.Background()); err == nil {
		t.Fatalf("a missing token file must surface as an error")
	}
}

// TestToken_EmptyAccessToken: an authn 200 with no accessToken is an error.
func TestToken_EmptyAccessToken(t *testing.T) {
	tokenPath := writeTokenFile(t, "sa-token")
	c := New("http://authn.test:8082", tokenPath).WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}),
	})
	if _, err := c.Token(context.Background()); err == nil {
		t.Fatalf("a 200 with empty accessToken must surface as an error")
	}
}

// TestToken_ReReadsRotatedTokenFile: on a re-exchange the client reads the
// token file FRESH (projected tokens rotate on disk).
func TestToken_ReReadsRotatedTokenFile(t *testing.T) {
	tokenPath := writeTokenFile(t, "rotated-1")
	seen := make(chan string, 4)
	c := New("http://authn.test:8082", tokenPath).WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen <- r.Header.Get("Authorization")
			body, _ := json.Marshal(map[string]string{"accessToken": makeJWT(time.Now().Add(30 * time.Second).Unix())})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
		}),
	})
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	// Rotate the on-disk token, then force a re-exchange (near-expiry).
	if err := os.WriteFile(tokenPath, []byte("rotated-2"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("Token (rotated): %v", err)
	}
	got1, got2 := <-seen, <-seen
	if got1 != "Bearer rotated-1" || got2 != "Bearer rotated-2" {
		t.Fatalf("client must re-read the rotated token file; saw %q then %q", got1, got2)
	}
}
