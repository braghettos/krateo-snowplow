//go:build unit
// +build unit

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
)

func TestJsonHandler(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		opts   jsonHandlerOptions
		expect map[string]any
		err    bool
	}{
		{
			name:  "valid JSON, no filter",
			input: `{"foo": "bar"}`,
			opts: jsonHandlerOptions{
				key: "test",
				out: make(map[string]any),
			},
			expect: map[string]any{"test": map[string]any{"foo": "bar"}},
			err:    false,
		},
		{
			name:  "invalid JSON",
			input: `{foo: bar}`,
			opts: jsonHandlerOptions{
				key: "test",
				out: make(map[string]any),
			},
			expect: nil,
			err:    true,
		},
		{
			// The stage filter is evaluated against the WRAPPED envelope
			// `{<key>: <response>}`, not the bare response — see
			// handler.go jsonHandlerCore: `pig := {opts.key: tmp}` then
			// EvalValue(filter, pig). This was made deliberate in PR #129
			// (KRA-672): the filter Data was changed from `tmp` to `pig`
			// so production filters reference the stage key (e.g.
			// `[.namespaces.items[] | .metadata.name]`). The filter here
			// must therefore reach through the `test` key. The pre-#129
			// `.foo` form yielded nil because top-level `pig` has no
			// `foo`, only `test`.
			name:  "valid JSON with filter",
			input: `{"foo": "bar", "num": 42}`,
			opts: jsonHandlerOptions{
				key:    "test",
				out:    make(map[string]any),
				filter: ptr.To(".test.foo"),
			},
			expect: map[string]any{"test": "bar"},
			err:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			handler := jsonHandler(ctx, tc.opts)

			inputReader := io.NopCloser(bytes.NewReader([]byte(tc.input)))
			err := handler(inputReader)

			if (err != nil) != tc.err {
				t.Fatalf("unexpected error: %v", err)
			}

			if !tc.err {
				if got, ok := tc.opts.out[tc.opts.key]; !ok || !deepEqual(got, tc.expect[tc.opts.key]) {
					t.Errorf("expected %v, got %v", tc.expect, tc.opts.out)
				}
			}
		})
	}
}

func deepEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return bytes.Equal(aj, bj)
}
