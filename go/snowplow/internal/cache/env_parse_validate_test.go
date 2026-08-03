// env_parse_validate_test.go — falsifier for #278-C (Finding 5,
// generalizes #154): the range-validating env-parser variants
// positiveIntFromEnv / int64BytesFromEnv MUST emit a VISIBLE WARN slog
// on parse-reject AND on out-of-range, then fall back to the default —
// whereas the pre-#278-C intFromEnv/int64FromEnv silently swallowed the
// same misconfigurations. The ABSENCE of that WARN line was the #154
// failure-mode (silent 512MiB truncation on a scientific-notation
// RESOLVED_CACHE_MAX_RESIDENT_BYTES).
//
// No cluster. Pure parser-level unit test with a captured slog handler.

package cache

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog installs a text slog handler writing into a buffer as the
// process default for the duration of fn, then restores the prior
// default. Cache tests never run t.Parallel() (they use t.Setenv), so a
// transient global swap is safe here.
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// TestPositiveIntFromEnv_WarnsAndDefaults is the core #278-C falsifier
// for the parallelism/queue/worker knobs. The env name used is the real
// RESOLVED_CACHE_REFRESHER_PARALLELISM knob (envRefresherParallelism) so
// the test pins the exact production wiring the audit named.
func TestPositiveIntFromEnv_WarnsAndDefaults(t *testing.T) {
	const def = 4 // == defaultRefresherParallelism
	cases := []struct {
		name      string
		val       string
		wantVal   int
		wantWarn  bool
		wantEvent string // slog event name expected in the WARN line
	}{
		{name: "negative", val: "-1", wantVal: def, wantWarn: true, wantEvent: "cache.env.out_of_range"},
		{name: "zero", val: "0", wantVal: def, wantWarn: true, wantEvent: "cache.env.out_of_range"},
		{name: "garbage", val: "notanint", wantVal: def, wantWarn: true, wantEvent: "cache.env.parse_rejected"},
		{name: "scientific_1e9", val: "1e9", wantVal: def, wantWarn: true, wantEvent: "cache.env.parse_rejected"},
		{name: "overflow", val: "999999999999999999999", wantVal: def, wantWarn: true, wantEvent: "cache.env.parse_rejected"},
		{name: "valid", val: "8", wantVal: 8, wantWarn: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envRefresherParallelism, tc.val)
			var got int
			out := captureSlog(t, func() {
				got = positiveIntFromEnv(envRefresherParallelism, def)
			})
			if got != tc.wantVal {
				t.Fatalf("positiveIntFromEnv(%q=%q) = %d; want %d", envRefresherParallelism, tc.val, got, tc.wantVal)
			}
			gotWarn := strings.Contains(out, "level=WARN")
			if gotWarn != tc.wantWarn {
				t.Fatalf("WARN-emitted=%v; want %v (log=%q)", gotWarn, tc.wantWarn, out)
			}
			if tc.wantWarn {
				if !strings.Contains(out, tc.wantEvent) {
					t.Fatalf("expected event %q in WARN line; got %q", tc.wantEvent, out)
				}
				if !strings.Contains(out, "key="+envRefresherParallelism) {
					t.Fatalf("WARN line missing key=%s; got %q", envRefresherParallelism, out)
				}
			}
		})
	}
}

// TestPositiveIntFromEnv_UnsetIsSilent confirms the no-WARN path: an
// unset var falls back to the default with NO log (unset is not a
// misconfiguration — it is the normal "use default" case).
func TestPositiveIntFromEnv_UnsetIsSilent(t *testing.T) {
	// Ensure unset.
	t.Setenv(envRefresherParallelism, "")
	var got int
	out := captureSlog(t, func() {
		got = positiveIntFromEnv(envRefresherParallelism, 4)
	})
	if got != 4 {
		t.Fatalf("unset → %d; want default 4", got)
	}
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("unset env must NOT WARN; got %q", out)
	}
}

// TestInt64BytesFromEnv_WarnsRejectsAndAcceptsZero is the #278-C
// falsifier for the byte-cap knobs (incl. the #154
// RESOLVED_CACHE_MAX_RESIDENT_BYTES silent-truncation case). It pins
// three contract points:
//
//  1. scientific notation now PARSES (the original #154 report: "5e8"
//     became the default silently — it must now yield 500000000);
//  2. parse-reject and NEGATIVE values emit a VISIBLE WARN + default;
//  3. ZERO is ACCEPTED (valid resident-bytes kill-switch) with NO WARN.
func TestInt64BytesFromEnv_WarnsRejectsAndAcceptsZero(t *testing.T) {
	const def int64 = 536870912 // 512 MiB — stands in for the #154 default
	cases := []struct {
		name      string
		val       string
		wantVal   int64
		wantWarn  bool
		wantEvent string
	}{
		{name: "scientific_5e8_parses", val: "5e8", wantVal: 500000000, wantWarn: false},
		{name: "scientific_1.5e9_parses", val: "1.5e9", wantVal: 1500000000, wantWarn: false},
		{name: "plain_int", val: "1048576", wantVal: 1048576, wantWarn: false},
		{name: "zero_is_killswitch", val: "0", wantVal: 0, wantWarn: false},
		{name: "negative_rejected", val: "-1", wantVal: def, wantWarn: true, wantEvent: "cache.env.out_of_range"},
		{name: "negative_scientific_rejected", val: "-5e8", wantVal: def, wantWarn: true, wantEvent: "cache.env.out_of_range"},
		{name: "garbage_rejected", val: "notbytes", wantVal: def, wantWarn: true, wantEvent: "cache.env.parse_rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envResolvedCacheMaxResidentBytes, tc.val)
			var got int64
			out := captureSlog(t, func() {
				got = int64BytesFromEnv(envResolvedCacheMaxResidentBytes, def)
			})
			if got != tc.wantVal {
				t.Fatalf("int64BytesFromEnv(%q=%q) = %d; want %d", envResolvedCacheMaxResidentBytes, tc.val, got, tc.wantVal)
			}
			gotWarn := strings.Contains(out, "level=WARN")
			if gotWarn != tc.wantWarn {
				t.Fatalf("WARN-emitted=%v; want %v (log=%q)", gotWarn, tc.wantWarn, out)
			}
			if tc.wantWarn && !strings.Contains(out, tc.wantEvent) {
				t.Fatalf("expected event %q in WARN line; got %q", tc.wantEvent, out)
			}
		})
	}
}
