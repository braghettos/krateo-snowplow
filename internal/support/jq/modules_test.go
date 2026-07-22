package jq

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/krateoplatformops/plumbing/jqutil"
)

func eval(t *testing.T, query string, data any) any {
	t.Helper()
	s, err := jqutil.Eval(context.Background(), jqutil.EvalOptions{
		Query:        query,
		Data:         data,
		ModuleLoader: ModuleLoader(),
	})
	if err != nil {
		t.Fatalf("jq eval %q: %v", query, err)
	}

	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// jqutil.Eval returns bare strings unquoted; keep them as-is.
		return s
	}
	return out
}

func TestBuiltinHealthNormalize(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"Healthy", "OK"},
		{"running", "OK"},
		{"GREEN", "OK"},
		{true, "OK"},
		{"Degraded", "Warning"},
		{"pending", "Warning"},
		{"CrashLoopBackOff", "Critical"},
		{"failed", "Critical"},
		{false, "Critical"},
		{nil, "Unknown"},
		{"something-else", "Unknown"},
		// Numeric signals: the module's own severity codes (the inverse
		// of health_severity) — 0=OK, 1=Unknown, 2=Warning, 3=Critical;
		// anything unmapped degrades to Unknown.
		{float64(0), "OK"},
		{float64(1), "Unknown"},
		{float64(2), "Warning"},
		{float64(3), "Critical"},
		{float64(4), "Unknown"},
		{float64(-1), "Unknown"},
		{float64(2.5), "Unknown"},
	}
	for _, c := range cases {
		got := eval(t, `include "health"; normalize_health`, c.in)
		if got != c.want {
			t.Errorf("normalize_health(%v) = %v, want %s", c.in, got, c.want)
		}
	}
}

// TestBuiltinHealthNumericRoundTrip pins the documented invariant that
// numeric normalization is the exact inverse of health_severity:
// (health_severity | normalize_health) == normalize_health for any input.
func TestBuiltinHealthNumericRoundTrip(t *testing.T) {
	for _, in := range []any{"healthy", "degraded", "failed", nil, "weird",
		true, false, float64(0), float64(3)} {
		direct := eval(t, `include "health"; normalize_health`, in)
		roundTrip := eval(t, `include "health"; health_severity | normalize_health`, in)
		if direct != roundTrip {
			t.Errorf("round-trip broken for %v: normalize=%v, severity|normalize=%v",
				in, direct, roundTrip)
		}
	}
}

// TestBuiltinWorstHealthNumericMix proves heterogeneous string+numeric
// signals aggregate under one vocabulary.
func TestBuiltinWorstHealthNumericMix(t *testing.T) {
	cases := []struct {
		in   []any
		want string
	}{
		{[]any{float64(0), "healthy"}, "OK"},
		{[]any{float64(0), float64(2), "healthy"}, "Warning"},
		{[]any{"degraded", float64(3)}, "Critical"},
		{[]any{float64(0), float64(9)}, "Unknown"},
	}
	for _, c := range cases {
		got := eval(t, `include "health"; worst_health`, c.in)
		if got != c.want {
			t.Errorf("worst_health(%v) = %v, want %s", c.in, got, c.want)
		}
	}
}

func TestBuiltinWorstHealth(t *testing.T) {
	cases := []struct {
		in   []any
		want string
	}{
		{[]any{"ok", "healthy"}, "OK"},
		{[]any{"ok", "degraded"}, "Warning"},
		{[]any{"ok", "weird", "degraded"}, "Warning"},
		{[]any{"ok", "failed", "degraded"}, "Critical"},
		{[]any{"ok", "weird"}, "Unknown"},
		{[]any{}, "Unknown"},
	}
	for _, c := range cases {
		got := eval(t, `include "health"; worst_health`, c.in)
		if got != c.want {
			t.Errorf("worst_health(%v) = %v, want %s", c.in, got, c.want)
		}
	}
}

func TestBuiltinHealthSummaryAndRollup(t *testing.T) {
	services := []any{
		map[string]any{"org": "a", "service": "s1", "health": "healthy"},
		map[string]any{"org": "a", "service": "s2", "health": "degraded"},
		map[string]any{"org": "b", "service": "s3", "health": "failed"},
		map[string]any{"org": "b", "service": "s4", "health": nil},
	}

	got := eval(t, `include "health"; health_summary`, services)
	want := map[string]any{
		"total": float64(4), "ok": float64(1), "warning": float64(1),
		"critical": float64(1), "unknown": float64(1), "overall": "Critical",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("health_summary = %v, want %v", got, want)
	}

	rollup := eval(t, `include "health"; health_rollup(.org; .health)`, services)
	groups, ok := rollup.([]any)
	if !ok || len(groups) != 2 {
		t.Fatalf("health_rollup = %v, want 2 groups", rollup)
	}
	first := groups[0].(map[string]any)
	if first["key"] != "a" || first["overall"] != "Warning" {
		t.Errorf("group a rollup = %v", first)
	}
	second := groups[1].(map[string]any)
	if second["key"] != "b" || second["overall"] != "Critical" {
		t.Errorf("group b rollup = %v", second)
	}
}

func TestBuiltinUsage(t *testing.T) {
	if got := eval(t, `include "health"; usage_pct(50; 200)`, nil); got != float64(25) {
		t.Errorf("usage_pct(50;200) = %v, want 25", got)
	}
	if got := eval(t, `include "health"; usage_pct(1; 0)`, nil); got != nil {
		t.Errorf("usage_pct(1;0) = %v, want null", got)
	}
	if got := eval(t, `include "health"; usage_health(usage_pct(95; 100); 80; 90)`, nil); got != "Critical" {
		t.Errorf("usage_health = %v, want Critical", got)
	}

	rows := []any{
		map[string]any{"used": float64(40), "capacity": float64(100)},
		map[string]any{"used": float64(45), "capacity": float64(100)},
	}
	got := eval(t, `include "health"; usage_summary(.used; .capacity; 80; 90)`, rows)
	sum, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("usage_summary = %v", got)
	}
	if sum["used"] != float64(85) || sum["capacity"] != float64(200) ||
		sum["pct"] != float64(42.5) || sum["status"] != "OK" {
		t.Errorf("usage_summary = %v", sum)
	}
}

func TestUnknownModuleStillErrors(t *testing.T) {
	_, err := jqutil.Eval(context.Background(), jqutil.EvalOptions{
		Query:        `include "no-such-module"; .`,
		Data:         nil,
		ModuleLoader: ModuleLoader(),
	})
	if err == nil {
		t.Fatal("expected error for unknown module")
	}
}
