package restactions

import (
	"context"
	"errors"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// Q-PANEL-PROBE (0.25.326). The probe at restactions.go observes the
// OUTER-JQ "unable to resolve filter" site. Each test resets the three
// PanelProbe* counters, builds a synthetic dict, and asserts which
// counter bumped. Observe-only: no behavior change is exercised.

func resetPanelProbeCounters() {
	cache.GlobalMetrics.PanelProbeManagedEmpty.Store(0)
	cache.GlobalMetrics.PanelProbeManagedPopulated.Store(0)
	cache.GlobalMetrics.PanelProbePathMalformed.Store(0)
}

// dictWithManaged wraps a single composition-shaped object whose
// .status.managed equals the supplied value. Mirrors the unfiltered
// dict shape produced by api.Resolve when an api[] entry named
// `getComposition` returns a CR with metadata + status.
func dictWithManaged(managed any) map[string]any {
	return map[string]any{
		"getComposition": map[string]any{
			"metadata": map[string]any{
				"namespace": "demo-system",
				"name":      "bench-app-05-906",
			},
			"status": map[string]any{
				"managed": managed,
			},
		},
	}
}

func TestPanelProbe_FiresOnNilManaged(t *testing.T) {
	resetPanelProbeCounters()
	t.Cleanup(resetPanelProbeCounters)

	panelProbeOnFilterError(context.Background(), "ra-x",
		dictWithManaged(nil), errors.New("jq: unable to resolve filter"))

	if got := cache.GlobalMetrics.PanelProbeManagedEmpty.Load(); got != 1 {
		t.Errorf("ManagedEmpty: got %d, want 1", got)
	}
	if got := cache.GlobalMetrics.PanelProbeManagedPopulated.Load(); got != 0 {
		t.Errorf("ManagedPopulated: got %d, want 0", got)
	}
	if got := cache.GlobalMetrics.PanelProbePathMalformed.Load(); got != 0 {
		t.Errorf("PathMalformed: got %d, want 0", got)
	}
}

func TestPanelProbe_FiresOnEmptyArrayManaged(t *testing.T) {
	resetPanelProbeCounters()
	t.Cleanup(resetPanelProbeCounters)

	panelProbeOnFilterError(context.Background(), "ra-x",
		dictWithManaged([]any{}), errors.New("jq fail"))

	if got := cache.GlobalMetrics.PanelProbeManagedEmpty.Load(); got != 1 {
		t.Errorf("ManagedEmpty: got %d, want 1", got)
	}
}

func TestPanelProbe_FiresOnMalformedManaged(t *testing.T) {
	resetPanelProbeCounters()
	t.Cleanup(resetPanelProbeCounters)

	// non-array, non-nil value → path_malformed=true.
	panelProbeOnFilterError(context.Background(), "ra-x",
		dictWithManaged("not-an-array"), errors.New("jq fail"))

	if got := cache.GlobalMetrics.PanelProbePathMalformed.Load(); got != 1 {
		t.Errorf("PathMalformed: got %d, want 1", got)
	}
	if got := cache.GlobalMetrics.PanelProbeManagedEmpty.Load(); got != 0 {
		t.Errorf("ManagedEmpty: got %d, want 0", got)
	}

	// Also test with a map value (another malformed shape).
	resetPanelProbeCounters()
	panelProbeOnFilterError(context.Background(), "ra-x",
		dictWithManaged(map[string]any{"oops": true}), errors.New("jq fail"))
	if got := cache.GlobalMetrics.PanelProbePathMalformed.Load(); got != 1 {
		t.Errorf("PathMalformed (map shape): got %d, want 1", got)
	}
}

func TestPanelProbe_DoesNotFireOnPopulatedManaged(t *testing.T) {
	resetPanelProbeCounters()
	t.Cleanup(resetPanelProbeCounters)

	managed := []any{
		map[string]any{"path": "v1/Pod/foo", "ref": map[string]any{"name": "p1"}},
		map[string]any{"path": "apps/v1/Deployment/bar", "ref": map[string]any{"name": "d1"}},
	}

	panelProbeOnFilterError(context.Background(), "ra-x",
		dictWithManaged(managed), errors.New("jq fail"))

	if got := cache.GlobalMetrics.PanelProbeManagedPopulated.Load(); got != 1 {
		t.Errorf("ManagedPopulated: got %d, want 1", got)
	}
	if got := cache.GlobalMetrics.PanelProbeManagedEmpty.Load(); got != 0 {
		t.Errorf("ManagedEmpty: got %d, want 0 (populated should NOT bump empty)", got)
	}
	if got := cache.GlobalMetrics.PanelProbePathMalformed.Load(); got != 0 {
		t.Errorf("PathMalformed: got %d, want 0", got)
	}
}

func TestPanelProbe_NoStatusManagedKey_DoesNotFire(t *testing.T) {
	resetPanelProbeCounters()
	t.Cleanup(resetPanelProbeCounters)

	// dict whose entry has .status but NO .managed key — probe must skip.
	dict := map[string]any{
		"getCfg": map[string]any{
			"metadata": map[string]any{"name": "x"},
			"status":   map[string]any{"phase": "Ready"},
		},
		"otherScalar":  "string",
		"arrayValue":   []any{1, 2, 3},
		"nilValue":     nil,
		"nestedNoStat": map[string]any{"metadata": map[string]any{"name": "y"}},
	}

	panelProbeOnFilterError(context.Background(), "ra-x",
		dict, errors.New("jq fail"))

	if got := cache.GlobalMetrics.PanelProbeManagedEmpty.Load(); got != 0 {
		t.Errorf("ManagedEmpty: got %d, want 0", got)
	}
	if got := cache.GlobalMetrics.PanelProbeManagedPopulated.Load(); got != 0 {
		t.Errorf("ManagedPopulated: got %d, want 0", got)
	}
	if got := cache.GlobalMetrics.PanelProbePathMalformed.Load(); got != 0 {
		t.Errorf("PathMalformed: got %d, want 0", got)
	}
}

func TestPanelProbe_LongFilterErrorTruncated(t *testing.T) {
	// The probe MUST truncate filter_error to 200 chars to keep slog
	// payload bounded. This test exercises the size-cap branch; the
	// log line itself is fire-and-forget but the function path is
	// the same that emits structured fields.
	resetPanelProbeCounters()
	t.Cleanup(resetPanelProbeCounters)

	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	panelProbeOnFilterError(context.Background(), "ra-x",
		dictWithManaged(nil), errors.New(string(long)))

	if got := cache.GlobalMetrics.PanelProbeManagedEmpty.Load(); got != 1 {
		t.Errorf("ManagedEmpty after long-error: got %d, want 1", got)
	}
}
