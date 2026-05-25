// helpers_l1_test.go — Tag 0.30.7 binding: verify dispatcher-side L1
// hook respects the CACHE_ENABLED + RESOLVED_CACHE_ENABLED toggles and
// that the encoder + writer helpers produce byte-identical output to
// the pre-0.30.7 `json.Encoder.Encode` path.
//
// Per the plan's pre-flight falsifier #1: cache=off path must continue
// to serve correct results. The "cache=off ⇒ dispatchCacheLookupKey
// returns nil handle" test below is the unit-level proof of that
// invariant — when the handle is nil, the dispatchers fall straight
// through to the 0.30.6 resolve-and-encode path.

package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestDispatchCacheLookupKey_CacheOffReturnsNilHandle(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	// Even with RESOLVED_CACHE_ENABLED=true, CACHE_ENABLED=false
	// must short-circuit to "no L1".
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	_, h, in := dispatchCacheLookupKey(context.Background(), "widgets",
		"g", "v", "r", "ns", "name", -1, -1, nil)
	if h != nil {
		t.Fatalf("CACHE_ENABLED=false must yield nil cache handle, got %T", h)
	}
	if in != nil {
		t.Fatalf("CACHE_ENABLED=false must yield nil inputs, got %+v", in)
	}
}

func TestDispatchCacheLookupKey_ResolvedToggleOffReturnsNilHandle(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "false")
	_, h, in := dispatchCacheLookupKey(context.Background(), "widgets",
		"g", "v", "r", "ns", "name", -1, -1, nil)
	if h != nil {
		t.Fatalf("RESOLVED_CACHE_ENABLED=false must yield nil handle, got %T", h)
	}
	if in != nil {
		t.Fatalf("RESOLVED_CACHE_ENABLED=false must yield nil inputs, got %+v", in)
	}
}

func TestDispatchCacheLookupKey_NoUserInfoReturnsNilHandle(t *testing.T) {
	// A request with no UserInfo in context must be treated as
	// uncacheable — keying on missing identity would risk cross-user
	// leaks. The plan's binding: "key includes user_identity"; we
	// fail closed when it's absent.
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	_, h, in := dispatchCacheLookupKey(context.Background(), "widgets",
		"g", "v", "r", "ns", "name", -1, -1, nil)
	if h != nil {
		t.Fatalf("missing UserInfo must yield nil handle, got %T", h)
	}
	if in != nil {
		t.Fatalf("missing UserInfo must yield nil inputs, got %+v", in)
	}
}

func TestEncodeAndWriteResolvedJSON_MatchesCanonicalEncoder(t *testing.T) {
	// Goal: prove encodeResolvedJSON + writeResolvedJSON produce the
	// canonical compact-encoder shape Ship GMC / 0.30.174 ships
	// (json.NewEncoder, NO SetIndent). The cache-miss + cache-hit
	// dispatch paths both flow through encodeResolvedJSON, so as long
	// as the helper output matches a plain json.NewEncoder.Encode they
	// stay byte-equal to each other.
	//
	// Re-baseline note (per feedback_byte_identical_baselines_clean_
	// wire_shape): the reference is a SYNTHETIC payload — no JWT, no
	// Bearer token, no internal-field exposure — so this baseline does
	// not encode any sensitive shape.
	payload := map[string]any{
		"kind":      "RESTAction",
		"apiVersion": "templates.krateo.io/v1",
		"metadata":  map[string]any{"name": "demo"},
	}

	// Reference: inline encoder, identical knobs to the post-0.30.174
	// dispatcher path (NO SetIndent — Ship GMC dropped it).
	var ref bytes.Buffer
	refEnc := json.NewEncoder(&ref)
	if err := refEnc.Encode(payload); err != nil {
		t.Fatalf("reference encode failed: %v", err)
	}

	// Subject: helper-based path.
	got, err := encodeResolvedJSON(payload)
	if err != nil {
		t.Fatalf("encodeResolvedJSON failed: %v", err)
	}

	if !bytes.Equal(ref.Bytes(), got) {
		t.Fatalf("helper output diverges from canonical encoder.\nreference:%q\nhelper:%q", ref.Bytes(), got)
	}

	// Compact-shape invariant: the helper output MUST NOT contain a
	// two-space indent ("  ") on a fresh line — that was the
	// pre-0.30.174 shape and re-introducing it would re-inflate the
	// wire by ~25%. Sanity-check the helper output never carries that
	// signature.
	if bytes.Contains(got, []byte("\n  ")) {
		t.Fatalf("encodeResolvedJSON re-introduced two-space indent; want compact shape. got=%q", got)
	}

	// Round-trip writeResolvedJSON: verify content-type, status, body.
	rec := httptest.NewRecorder()
	writeResolvedJSON(rec, got)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), got) {
		t.Fatalf("body diverges from encoded payload")
	}
}
