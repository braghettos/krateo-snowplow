// refreshauth_test.go — M17 [SEC]: hermetic falsifiers for middleware.RefreshAuth,
// the cookie-or-header JWT gate for GET /refreshes.
//
// RefreshAuth's SECURITY contract (refreshauth.go header):
//   (a) it reads the JWT from the `Authorization: Bearer` header OR the
//       configured session cookie (REFRESH_SESSION_COOKIE, default
//       "krateo-session") — a custom cookie name is honored and the default
//       name is rejected once a custom one is configured;
//   (b) NO TOKEN-IN-URL: a valid JWT presented ONLY in the query string is
//       NOT accepted (would leak in logs/referrer) → 401;
//   (c) an expired JWT → 401 (jwtutil.Validate returns ErrTokenExpired).
//
// RED arm (TestRefreshAuth_RED_QueryStringMustNotAuthenticate): a plausible
// wrong middleware that ALSO reads the token from the query string authenticates
// the token-in-URL request (reaches the terminal handler). The real RefreshAuth
// on the SAME request 401s — proving arm (b) is discriminating.
//
// Hermetic: httptest + a test signing key. NO apiserver.

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/handlers/middleware"
)

const refreshAuthTestKey = "test-signing-key-for-refreshauth-m17-2026-07-30"

// mintRefreshToken issues a JWT signed with the test key. A negative duration
// yields an already-expired token (jwtutil.Validate → ErrTokenExpired).
func mintRefreshToken(t *testing.T, username string, dur time.Duration) string {
	t.Helper()
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username:   username,
		Groups:     []string{"devs"},
		Duration:   dur,
		SigningKey: refreshAuthTestKey,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

// runRefreshAuth drives one request through middleware.RefreshAuth wrapping a
// terminal handler that records (a) whether it was reached and (b) the resolved
// username. Returns the status code and the reached/username observations.
func runRefreshAuth(t *testing.T, req *http.Request) (status int, reached bool, username string) {
	t.Helper()
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if ui, err := xcontext.UserInfo(r.Context()); err == nil {
			username = ui.Username
		}
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.RefreshAuth(refreshAuthTestKey)(terminal)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, reached, username
}

// --- M17(a): cookie-name configuration --------------------------------------

// TestRefreshAuth_CustomCookieHonored_DefaultRejected — when
// REFRESH_SESSION_COOKIE names a CUSTOM cookie, a JWT in that cookie
// authenticates (200, identity placed) while the SAME JWT in the DEFAULT
// "krateo-session" cookie is NOT read → 401. This proves the cookie name is
// config-driven (feedback_no_special_cases), not hardcoded.
func TestRefreshAuth_CustomCookieHonored_DefaultRejected(t *testing.T) {
	t.Setenv("REFRESH_SESSION_COOKIE", "my-portal-session")
	tok := mintRefreshToken(t, "userA", time.Hour)

	t.Run("custom cookie honored", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/refreshes?sub=x", nil)
		req.AddCookie(&http.Cookie{Name: "my-portal-session", Value: tok})
		status, reached, user := runRefreshAuth(t, req)
		if status != http.StatusOK || !reached {
			t.Fatalf("M17(a): custom cookie must authenticate; status=%d reached=%v", status, reached)
		}
		if user != "userA" {
			t.Fatalf("M17(a): identity must be placed on ctx; got username=%q", user)
		}
	})

	t.Run("default cookie rejected when custom configured", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/refreshes?sub=x", nil)
		req.AddCookie(&http.Cookie{Name: "krateo-session", Value: tok}) // the DEFAULT name
		status, reached, _ := runRefreshAuth(t, req)
		if status != http.StatusUnauthorized || reached {
			t.Fatalf("M17(a): with a custom cookie configured, the default 'krateo-session' cookie must "+
				"NOT authenticate; status=%d reached=%v", status, reached)
		}
	})
}

// TestRefreshAuth_DefaultCookieHonoredWhenUnset — with REFRESH_SESSION_COOKIE
// unset the default "krateo-session" cookie authenticates (the deployed default
// path). Complements the custom-name arm above.
func TestRefreshAuth_DefaultCookieHonoredWhenUnset(t *testing.T) {
	t.Setenv("REFRESH_SESSION_COOKIE", "") // unset → default
	tok := mintRefreshToken(t, "userA", time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/refreshes?sub=x", nil)
	req.AddCookie(&http.Cookie{Name: "krateo-session", Value: tok})
	status, reached, user := runRefreshAuth(t, req)
	if status != http.StatusOK || !reached || user != "userA" {
		t.Fatalf("M17(a): default cookie must authenticate when REFRESH_SESSION_COOKIE unset; "+
			"status=%d reached=%v user=%q", status, reached, user)
	}
}

// --- M17(b): no-token-in-URL ------------------------------------------------

// TestRefreshAuth_QueryParamTokenRejected — a VALID JWT presented ONLY in the
// query string (?token= / ?access_token= / ?jwt=) is NOT accepted → 401, and
// the terminal handler is NEVER reached. The token must not travel in the URL
// (log/referrer leak). Table over the common query-key spellings.
func TestRefreshAuth_QueryParamTokenRejected(t *testing.T) {
	tok := mintRefreshToken(t, "userA", time.Hour)

	for _, key := range []string{"token", "access_token", "jwt", "authorization"} {
		t.Run(key, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/refreshes?"+key+"="+tok, nil)
			status, reached, _ := runRefreshAuth(t, req)
			if status != http.StatusUnauthorized || reached {
				t.Fatalf("M17(b): a valid JWT ONLY in query param %q must be rejected 401 (no token-in-URL); "+
					"status=%d reached=%v", key, status, reached)
			}
		})
	}
}

// --- M17(c): expired JWT ----------------------------------------------------

// TestRefreshAuth_ExpiredTokenRejected — an expired JWT (in the header) → 401.
// Also confirmed via the cookie transport, since both channels funnel into the
// same jwtutil.Validate.
func TestRefreshAuth_ExpiredTokenRejected(t *testing.T) {
	t.Setenv("REFRESH_SESSION_COOKIE", "krateo-session")
	expired := mintRefreshToken(t, "userA", -1*time.Hour) // ExpiresAt in the past

	t.Run("header transport", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/refreshes?sub=x", nil)
		req.Header.Set("Authorization", "Bearer "+expired)
		status, reached, _ := runRefreshAuth(t, req)
		if status != http.StatusUnauthorized || reached {
			t.Fatalf("M17(c): expired JWT (header) must 401; status=%d reached=%v", status, reached)
		}
	})

	t.Run("cookie transport", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/refreshes?sub=x", nil)
		req.AddCookie(&http.Cookie{Name: "krateo-session", Value: expired})
		status, reached, _ := runRefreshAuth(t, req)
		if status != http.StatusUnauthorized || reached {
			t.Fatalf("M17(c): expired JWT (cookie) must 401; status=%d reached=%v", status, reached)
		}
	})
}

// --- M17 RED: query-string auth must be caught ------------------------------

// queryReadingAuth is a SHADOW wrong middleware that ALSO reads the token from
// the query string (?token=) in addition to header/cookie — the exact
// vulnerability M17(b) guards against. It exists only to prove the M17(b)
// assertion is discriminating.
func queryReadingAuth(signingKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var token string
			if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
				token = h[7:]
			}
			if token == "" {
				if ck, err := r.Cookie("krateo-session"); err == nil {
					token = ck.Value
				}
			}
			if token == "" {
				token = r.URL.Query().Get("token") // THE BUG: token-in-URL
			}
			if token == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			ui, err := jwtutil.Validate(signingKey, token)
			if err != nil {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			ctx := xcontext.BuildContext(r.Context(), xcontext.WithUserInfo(ui))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// TestRefreshAuth_RED_QueryStringMustNotAuthenticate is the RED proof for
// M17(b). A token-in-URL request:
//   - against the SHADOW query-reading middleware → 200 + reached (the bug);
//   - against the REAL middleware.RefreshAuth        → 401 + NOT reached.
//
// The divergence proves the token-in-URL assertion catches the vulnerable impl.
func TestRefreshAuth_RED_QueryStringMustNotAuthenticate(t *testing.T) {
	tok := mintRefreshToken(t, "userA", time.Hour)
	newReq := func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/refreshes?token="+tok, nil)
	}

	// (1) The wrong impl authenticates the token-in-URL request — RED confirmed.
	var wrongReached bool
	wrongTerminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		wrongReached = true
		w.WriteHeader(http.StatusOK)
	})
	wrongRec := httptest.NewRecorder()
	queryReadingAuth(refreshAuthTestKey)(wrongTerminal).ServeHTTP(wrongRec, newReq())
	if wrongRec.Code != http.StatusOK || !wrongReached {
		t.Fatalf("RED setup invalid: the query-reading shadow impl should have authenticated the "+
			"token-in-URL request; status=%d reached=%v", wrongRec.Code, wrongReached)
	}

	// (2) The real middleware rejects the SAME request — the property under test.
	status, reached, _ := runRefreshAuth(t, newReq())
	if status != http.StatusUnauthorized || reached {
		t.Fatalf("RED: middleware.RefreshAuth must NOT authenticate a token-in-URL request "+
			"(the shadow impl did); status=%d reached=%v", status, reached)
	}
}
