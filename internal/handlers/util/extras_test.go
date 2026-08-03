package util_test

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/handlers/util"
)

func TestParseExtras(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		want      map[string]any
		expectErr bool
	}{
		{
			name:      "empty extras",
			query:     "",
			want:      map[string]any{},
			expectErr: false,
		},
		{
			name:      "valid extras",
			query:     `{"count":10,"active":true,"name":"test"}`,
			want:      map[string]any{"count": float64(10), "active": true, "name": "test"},
			expectErr: false,
		},
		{
			name:      "invalid json",
			query:     `{"count":10, "active":true,}`,
			want:      map[string]any{},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// costruiamo l'URL con query param extras
			u := &url.URL{
				Scheme:   "http",
				Host:     "localhost",
				Path:     "/",
				RawQuery: "ignoreme=Hello&extras=" + url.QueryEscape(tt.query),
			}
			req := &http.Request{URL: u}

			got, err := util.ParseExtras(req)
			if (err != nil) != tt.expectErr {
				t.Fatalf("expected error: %v, got: %v", tt.expectErr, err)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got = %v, want = %v", got, tt.want)
			}
		})
	}
}
