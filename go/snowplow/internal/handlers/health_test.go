package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockNSGetter is kept around for signature compatibility — Ship 0.5.5
// HealthCheck ignores the namespace getter, but callers in main.go
// still pass one. The test verifies the handler does NOT depend on it.
func mockNSGetter() (string, error) {
	return "test-namespace", nil
}

// TestHealthCheck_AliveOnly verifies the Ship 0.5.5 contract:
//   - 200 OK
//   - Content-Type application/json
//   - Body is exactly {"status":"alive"} (17 bytes, no namespace, no build)
//   - Handler does NOT depend on rest.InClusterConfig or nsgetter
func TestHealthCheck_AliveOnly(t *testing.T) {
	tests := []struct {
		name     string
		nsgetter func() (string, error)
	}{
		{
			name:     "with non-nil nsgetter (should be ignored)",
			nsgetter: mockNSGetter,
		},
		{
			name:     "with nil nsgetter (should NOT panic)",
			nsgetter: nil,
		},
	}

	const wantBody = `{"status":"alive"}`

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()

			handler := HealthCheck("test-service", "v1.0.0", tc.nsgetter)
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
			}

			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("expected Content-Type application/json, got %q", got)
			}

			body := bytes.TrimRight(rec.Body.Bytes(), "\n")
			if string(body) != wantBody {
				t.Errorf("expected body %q, got %q", wantBody, string(body))
			}
		})
	}
}
