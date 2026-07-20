package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
)

func TestSanitizeID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc-123", "abc-123"},
		{"portal-req.42_a", "portal-req.42_a"},
		{"portal:req/42", ""},
		{"has spaces", ""},
		{"bad\nnewline", ""},
		{"quote\"", ""},
		{strings.Repeat("a", 129), ""},
		{strings.Repeat("a", 128), strings.Repeat("a", 128)},
	}
	for _, c := range cases {
		if got := SanitizeID(c.in); got != c.want {
			t.Errorf("SanitizeID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMiddlewarePropagatesInboundID(t *testing.T) {
	var seen string
	h := Middleware()(http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
		seen = CorrelationID(req.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/call", nil)
	req.Header.Set(HeaderCorrelationID, "portal-abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "portal-abc-123" {
		t.Errorf("context correlation id = %q, want %q", seen, "portal-abc-123")
	}
	if got := rec.Header().Get(HeaderCorrelationID); got != "portal-abc-123" {
		t.Errorf("echoed header = %q, want %q", got, "portal-abc-123")
	}
}

func TestMiddlewareMintsWhenAbsent(t *testing.T) {
	var seen string
	h := Middleware()(http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
		seen = CorrelationID(req.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/call", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("expected a minted correlation id, got empty")
	}
	if got := rec.Header().Get(HeaderCorrelationID); got != seen {
		t.Errorf("echoed header %q != context id %q", got, seen)
	}
}

func TestMiddlewareRejectsHostileID(t *testing.T) {
	var seen string
	h := Middleware()(http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
		seen = CorrelationID(req.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/call", nil)
	req.Header.Set(HeaderCorrelationID, `evil" injection`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen == `evil" injection` || seen == "" {
		t.Errorf("hostile id must be replaced, got %q", seen)
	}
}

func TestEmitStructuredRecord(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))

	ctx := xcontext.BuildContext(context.Background(), xcontext.WithLogger(log))
	ctx = WithCorrelationID(ctx, "corr-42")

	Emit(ctx, Event{
		Action:    "call",
		Verb:      "POST",
		Group:     "composition.krateo.io",
		Version:   "v1alpha1",
		Resource:  "fireworksapps",
		Name:      "demo",
		Namespace: "team-a",
		Outcome:   "success",
		Code:      200,
	})

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("emitted record is not JSON: %v (%s)", err, buf.String())
	}

	audit, ok := rec["audit"].(map[string]any)
	if !ok {
		t.Fatalf("missing audit group in record: %s", buf.String())
	}
	if audit["kind"] != EventKind {
		t.Errorf("kind = %v, want %q", audit["kind"], EventKind)
	}
	if audit["correlationId"] != "corr-42" {
		t.Errorf("correlationId = %v, want corr-42", audit["correlationId"])
	}
	if audit["resource"] != "fireworksapps" || audit["outcome"] != "success" {
		t.Errorf("unexpected audit payload: %v", audit)
	}
}
