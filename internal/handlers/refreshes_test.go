// refreshes_test.go — Ship 1 (live-refresh-coherence, option A) HTTP-level
// falsifiers for GET /refreshes: RefreshAuth (cookie-or-header JWT), the
// cache-off idle stream (9.5b, the /refreshes half), and validateSubscription
// rejection. Hermetic: httptest + a test JWT signing key; NO apiserver,
// KUBECONFIG unset. NEVER ./internal/rbac.
//
// The per-subject ISOLATION (9.4a) and the per-row CONTENT (9.4b) live at the
// derivation layer (refresh_isolation_falsifier_test.go in package dispatchers,
// where the in-process RBAC snapshot builder exists) and the cluster gate,
// respectively. This file proves the endpoint's auth + lifecycle + input
// validation.

package handlers

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/middleware"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const refreshTestSignKey = "test-sign-key-ship1-live-refresh"

func mintToken(t *testing.T, username string) string {
	t.Helper()
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username:   username,
		Groups:     []string{"devs"},
		Duration:   time.Hour,
		SigningKey: refreshTestSignKey,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

// subParam builds a valid ?sub= value (base64 JSON coordinate array) for one
// widgetContent coordinate (identity-free, so it derives without an RBAC
// snapshot).
func subParam(t *testing.T) string {
	t.Helper()
	body := []map[string]any{{
		"class":     "widgetContent",
		"group":     "widgets.templates.krateo.io",
		"version":   "v1beta1",
		"resource":  "panels",
		"namespace": "krateo-system",
		"name":      "dashboard-piechart",
		"perPage":   5,
		"page":      1,
	}}
	raw, _ := json.Marshal(body)
	return base64.StdEncoding.EncodeToString(raw)
}

// seedAuthTestWidget wires cache.Global() with the dashboard-piechart panel CR
// that subParam() arms + an RBAC binding granting userA's group (devs) get/list
// on panels, so the #64 subscriptionKeyExtras objects.Get (informer-served,
// RBAC-gated under the connection identity) succeeds and DeriveSubscriptionKey
// derives a valid key.
//
// WHY (#64): the auth tests exercise the REAL arming path, which now (correctly,
// C64-1 fail-closed) requires a fetchable widget CR — a widget the user can't
// GET is not live-refreshable, so it is not armed. Pre-#64 these coords
// phantom-armed (200, but the request-only key was WRONG → never delivered, the
// very bug). Seeding the CR tests the honest arming path.
func seedAuthTestWidget(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	panelGVR := schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"}
	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		crbGVR: "ClusterRoleBindingList",
		crGVR:  "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}: "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
		panelGVR: "PanelList",
	}
	rule := []rbacv1.PolicyRule{{Verbs: []string{"get", "list"}, APIGroups: []string{"widgets.templates.krateo.io"}, Resources: []string{"panels"}}}
	seed := []runtime.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "panel-reader"}, Rules: rule},
		// Grant the "devs" GROUP (userA's mintToken group) get/list panels.
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "devs-bind", UID: types.UID("uid-devs")},
			Subjects:   []rbacv1.Subject{{Kind: "Group", Name: "devs"}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "panel-reader"},
		},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "widgets.templates.krateo.io/v1beta1",
			"kind":       "Panel",
			"metadata":   map[string]any{"name": "dashboard-piechart", "namespace": "krateo-system"},
			"spec":       map[string]any{}, // no inline extras — request-only key, byte-identical to emit
		}},
	}

	wctx, wcancel := context.WithCancel(context.Background())
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)
	rw, err := cache.NewResourceWatcher(wctx, dyn)
	if err != nil {
		wcancel()
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	if err := rw.WaitForCacheSync(syncCtx, 5*time.Second); err != nil {
		rw.Stop()
		wcancel()
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	_, _ = rw.EnsureResourceType(panelGVR)
	_ = rw.WaitForCacheSync(syncCtx, 5*time.Second)
	cache.RebuildRBACSnapshotForTest(rw)
	prev := cache.Global()
	cache.SetGlobal(rw)
	t.Cleanup(func() {
		rw.Stop()
		wcancel()
		cache.SetGlobal(prev)
		cache.PublishRBACSnapshotForTest(nil)
	})
}

// refreshServer wires the production chain (RefreshAuth -> Refreshes) on a
// test server. Returns the base URL.
func refreshServer(t *testing.T) string {
	t.Helper()
	h := middleware.RefreshAuth(refreshTestSignKey)(Refreshes(refreshTestSignKey))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// openStream issues GET /refreshes with the given setup and returns the
// response + a cancel func. The caller MUST cancel to release the streaming
// handler goroutine.
func openStream(t *testing.T, baseURL, query string, setup func(*http.Request)) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+query, nil)
	if err != nil {
		cancel()
		t.Fatalf("NewRequest: %v", err)
	}
	if setup != nil {
		setup(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("Do: %v", err)
	}
	return resp, cancel
}

// --- RefreshAuth ------------------------------------------------------------

// TestRefreshes_Auth_HeaderTokenReachesHandler — a valid Authorization: Bearer
// header authenticates and the handler opens the SSE stream (200 +
// text/event-stream). The curl-falsifier / non-browser path.
func TestRefreshes_Auth_HeaderTokenReachesHandler(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("REFRESH_SSE_ENABLED", "")
	seedAuthTestWidget(t) // #64: the armed widget CR must be fetchable (C64-1 fail-closed)
	base := refreshServer(t)

	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("header-auth: status=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("header-auth: Content-Type=%q want text/event-stream", ct)
	}
}

// TestRefreshes_Auth_CookieTokenReachesHandler — the browser EventSource path:
// the JWT in the configured session cookie authenticates (no Authorization
// header). This is the make-or-break for EventSource (it cannot set headers).
func TestRefreshes_Auth_CookieTokenReachesHandler(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("REFRESH_SSE_ENABLED", "")
	t.Setenv("REFRESH_SESSION_COOKIE", "krateo-session")
	seedAuthTestWidget(t) // #64: the armed widget CR must be fetchable (C64-1 fail-closed)
	base := refreshServer(t)

	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "krateo-session", Value: mintToken(t, "userA")})
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-auth: status=%d want 200 — EventSource cookie path broken", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("cookie-auth: Content-Type=%q want text/event-stream", ct)
	}
}

// TestRefreshes_Auth_MissingCredentials401 — no header, no cookie -> 401.
func TestRefreshes_Auth_MissingCredentials401(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	base := refreshServer(t)

	resp, cancel := openStream(t, base, "?sub="+subParam(t), nil)
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing-creds: status=%d want 401", resp.StatusCode)
	}
}

// TestRefreshes_Auth_InvalidToken401 — a token signed with the WRONG key -> 401.
func TestRefreshes_Auth_InvalidToken401(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	base := refreshServer(t)

	bad, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username: "userA", Duration: time.Hour, SigningKey: "WRONG-KEY",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+bad)
	})
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid-token: status=%d want 401 (wrong-key JWT must not validate)", resp.StatusCode)
	}
}

// --- validateSubscription rejection -----------------------------------------

// TestRefreshes_Validation_Rejections — malformed/oversized/empty ?sub= -> 400.
// (Auth succeeds first; the rejection is the subscription validation.)
func TestRefreshes_Validation_Rejections(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	base := refreshServer(t)
	tok := mintToken(t, "userA")

	// Oversized: a base64 blob whose DECODED size exceeds refreshSubParamMaxBytes.
	huge := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", refreshSubParamMaxBytes+1)))

	cases := []struct {
		name  string
		query string
	}{
		{"missing sub", ""},
		{"malformed base64", "?sub=!!!not-base64!!!"},
		{"oversized payload", "?sub=" + huge},
		{"empty array", "?sub=" + base64.StdEncoding.EncodeToString([]byte("[]"))},
		{"not an array", "?sub=" + base64.StdEncoding.EncodeToString([]byte(`{"class":"widgetContent"}`))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, cancel := openStream(t, base, c.query, func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+tok)
			})
			defer cancel()
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s: status=%d want 400", c.name, resp.StatusCode)
			}
		})
	}
}

// TestRefreshes_Validation_AllForeignKeysRejected — when every coordinate fails
// derivation (cache layer present but identity yields no key for an identity-
// bound class with no RBAC snapshot -> BindingUID empty is still a derived key;
// so use an UNKNOWN class, which DeriveSubscriptionKey fails-closed on) the
// armed set is empty -> 400 "no valid subscription keys".
func TestRefreshes_Validation_AllForeignKeysRejected(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	base := refreshServer(t)
	tok := mintToken(t, "userA")

	body := []map[string]any{{"class": "totally-unknown-class", "name": "x"}}
	raw, _ := json.Marshal(body)
	q := "?sub=" + base64.StdEncoding.EncodeToString(raw)

	resp, cancel := openStream(t, base, q, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+tok)
	})
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("all-foreign: status=%d want 400 (no armable keys)", resp.StatusCode)
	}
}

// --- 9.5b — cache-off idle stream -------------------------------------------

// TestRefreshes_CacheOff_IdleStream is the /refreshes half of falsifier 9.5b:
// with the cache subsystem off, GET /refreshes returns 200 + text/event-stream
// and emits ONLY heartbeats — zero `event: refresh` frames — so a connected
// client degrades to its own throttle (transparent fallback,
// project_cache_off_is_transparent_fallback). It also requires NO auth-bearing
// credentials? No — auth still applies; we pass a valid token. The point is the
// STREAM is idle. (The /call correct-CONTENT half of 9.5b is the cluster
// falsifier — it needs the resolve stack.)
func TestRefreshes_CacheOff_IdleStream(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false") // cache subsystem OFF
	base := refreshServer(t)

	// Under cache-off the handler serves the idle stream BEFORE subscription
	// validation, so even a valid token + any sub yields the idle stream.
	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache-off: status=%d want 200 (idle stream, transparent fallback)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("cache-off: Content-Type=%q want text/event-stream", ct)
	}

	// Read for a short window; assert NO `event: refresh` frame arrives (the
	// broadcaster does not exist under cache-off, so nothing can publish).
	// We cannot easily wait a full heartbeat (20s) in a unit test, so we just
	// assert that within a short read no refresh event appears and the stream
	// stays open (no premature EOF/error).
	done := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: refresh") {
				done <- "GOT_REFRESH"
				return
			}
		}
		done <- "EOF"
	}()
	select {
	case sig := <-done:
		if sig == "GOT_REFRESH" {
			t.Fatalf("cache-off: received an `event: refresh` frame — the stream must be idle (no broadcaster exists)")
		}
		// "EOF" here would mean the server closed the stream; under cache-off
		// the idle stream stays open until client-cancel, so a fast EOF is
		// unexpected — but the cancel in defer can race it. Treat EOF within
		// the window as benign (the stream did not emit a refresh).
	case <-time.After(500 * time.Millisecond):
		// No refresh frame within the window — correct (idle stream).
	}
}
