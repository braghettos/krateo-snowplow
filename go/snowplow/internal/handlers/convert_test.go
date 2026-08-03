//go:build unit
// +build unit

package handlers_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/handlers"
	"github.com/stretchr/testify/assert"
)

func TestConvertHandler(t *testing.T) {
	handler := handlers.Converter()

	tests := []struct {
		name           string
		method         string
		body           string
		contentType    string
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "YAML to JSON - Valid Request",
			method:         http.MethodPost,
			body:           "key: value\nanother_key: another_value",
			contentType:    "text/yaml",
			expectedStatus: http.StatusOK,
			expectedBody:   `{"another_key":"another_value","key":"value"}`,
		},

		{
			name:           "JSON to YAML - Valid Request",
			method:         http.MethodPost,
			body:           `{"key":"value","another_key":"another_value"}`,
			contentType:    "application/json",
			expectedStatus: http.StatusOK,
			expectedBody:   "another_key: another_value\nkey: value",
		},

		{
			name:           "Unsupported Accept Header",
			method:         http.MethodPost,
			body:           `key: value`,
			contentType:    "text/plain",
			expectedStatus: http.StatusNotAcceptable,
			expectedBody:   `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"unsupported content type 'text/plain' use 'application/json' or 'application/x-yaml'","reason":"NotAcceptable","code":406}`,
		},

		{
			name:           "Request Body Too Large",
			method:         http.MethodPost,
			body:           string(make([]byte, handlers.MaxBodySize+1)), // Exceeds MaxBodySize
			contentType:    "application/json",
			expectedStatus: http.StatusBadRequest,
			expectedBody:   `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"failed to convert JSON to YAML: yaml: control characters are not allowed","reason":"BadRequest","code":400}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/convert", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", tt.contentType)

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			assert.Equal(t, tt.expectedStatus, res.StatusCode)

			if len(tt.expectedBody) > 0 {
				respBody := strings.TrimSpace(rec.Body.String())
				assert.Equal(t, tt.expectedBody, respBody, "unexpected response body")
			}
		})
	}
}
