// retired_flags_test.go — #57 retired-flag startup audit unit tests.
//
// Covers PM-gate condition C2: the severity split (value "false" => Warn,
// any other value => Info), warn-once-per-flag-per-process semantics, and
// the zero-logs-when-absent invariant.

package cache_test

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// capturedRecord is one slog record the test handler retained.
type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// captureHandler is a minimal slog.Handler that records every emitted
// record (level, message, string attrs) for assertions. Safe for the
// concurrent-callers test (mutex-guarded append).
type captureHandler struct {
	mu      sync.Mutex
	records *[]capturedRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, capturedRecord{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// newCaptureLogger returns a logger backed by captureHandler plus the
// slice it appends to.
func newCaptureLogger() (*slog.Logger, *[]capturedRecord) {
	recs := &[]capturedRecord{}
	return slog.New(&captureHandler{records: recs}), recs
}

// findAuditRecords filters captured records to the audit line for a flag.
func findAuditRecords(recs []capturedRecord, flag string) []capturedRecord {
	var out []capturedRecord
	for _, r := range recs {
		if r.msg == "config.retired_flag_ignored" && r.attrs["flag"] == flag {
			out = append(out, r)
		}
	}
	return out
}

// TestAuditRetiredFlags_FalseLogsWarn — C2: a retired flag set to "false"
// is a silent behavior change (operator asked for OFF, now gets ON) and
// MUST log at slog.Warn with flag name + ignored-notice + new behavior +
// remediation.
func TestAuditRetiredFlags_FalseLogsWarn(t *testing.T) {
	cache.ResetRetiredFlagAuditForTest()
	t.Cleanup(cache.ResetRetiredFlagAuditForTest)
	t.Setenv("PREWARM_ENABLED", "false")

	log, recs := newCaptureLogger()
	cache.AuditRetiredFlags(log)

	got := findAuditRecords(*recs, "PREWARM_ENABLED")
	if len(got) != 1 {
		t.Fatalf("PREWARM_ENABLED=false: want exactly 1 audit line; got %d (%+v)", len(got), *recs)
	}
	r := got[0]
	if r.level != slog.LevelWarn {
		t.Fatalf("PREWARM_ENABLED=false: want slog.Warn; got %v", r.level)
	}
	// The line must name the flag, the ignored status, the new behavior,
	// and a remediation hint (C2 wording requirements).
	if r.attrs["flag"] != "PREWARM_ENABLED" {
		t.Fatalf("audit line missing flag name; attrs=%v", r.attrs)
	}
	if r.attrs["status"] != "ignored" {
		t.Fatalf("audit line must report status=ignored; attrs=%v", r.attrs)
	}
	if r.attrs["effective_behavior"] == "" {
		t.Fatalf("audit line must state the new effective behavior; attrs=%v", r.attrs)
	}
	if r.attrs["remediation"] == "" {
		t.Fatalf("WARN audit line must carry a remediation hint; attrs=%v", r.attrs)
	}
}

// TestAuditRetiredFlags_TrueLogsInfo — C2: a retired flag set to "true"
// (or anything other than "false") is a harmless no-op and logs at
// slog.Info, NOT Warn.
func TestAuditRetiredFlags_TrueLogsInfo(t *testing.T) {
	cache.ResetRetiredFlagAuditForTest()
	t.Cleanup(cache.ResetRetiredFlagAuditForTest)
	t.Setenv("RESOLVER_USE_INFORMER", "true")

	log, recs := newCaptureLogger()
	cache.AuditRetiredFlags(log)

	got := findAuditRecords(*recs, "RESOLVER_USE_INFORMER")
	if len(got) != 1 {
		t.Fatalf("RESOLVER_USE_INFORMER=true: want exactly 1 audit line; got %d (%+v)", len(got), *recs)
	}
	r := got[0]
	if r.level != slog.LevelInfo {
		t.Fatalf("RESOLVER_USE_INFORMER=true: want slog.Info; got %v", r.level)
	}
	if r.level == slog.LevelWarn {
		t.Fatalf("RESOLVER_USE_INFORMER=true must NOT log at Warn (it is a no-op)")
	}
}

// TestAuditRetiredFlags_OtherValueLogsInfo — any non-"false" value (here
// "1") is Info, confirming the discriminator is exactly the "false" value.
func TestAuditRetiredFlags_OtherValueLogsInfo(t *testing.T) {
	cache.ResetRetiredFlagAuditForTest()
	t.Cleanup(cache.ResetRetiredFlagAuditForTest)
	t.Setenv("PREWARM_ENABLED", "1")

	log, recs := newCaptureLogger()
	cache.AuditRetiredFlags(log)

	got := findAuditRecords(*recs, "PREWARM_ENABLED")
	if len(got) != 1 || got[0].level != slog.LevelInfo {
		t.Fatalf("PREWARM_ENABLED=1: want exactly 1 Info line; got %+v", got)
	}
}

// TestAuditRetiredFlags_FalseCaseInsensitive — "False"/"FALSE" must also
// trip the WARN branch (operators may not write lowercase).
func TestAuditRetiredFlags_FalseCaseInsensitive(t *testing.T) {
	cache.ResetRetiredFlagAuditForTest()
	t.Cleanup(cache.ResetRetiredFlagAuditForTest)
	t.Setenv("PREWARM_ENABLED", "False")

	log, recs := newCaptureLogger()
	cache.AuditRetiredFlags(log)

	got := findAuditRecords(*recs, "PREWARM_ENABLED")
	if len(got) != 1 || got[0].level != slog.LevelWarn {
		t.Fatalf("PREWARM_ENABLED=False: want exactly 1 Warn line; got %+v", got)
	}
}

// TestAuditRetiredFlags_AbsentEmitsNothing — the zero-logs invariant:
// when NO retired flag is present, the audit emits nothing. (t.Setenv is
// not used so the process env governs; both retired names must be unset
// for the assertion to be meaningful — we explicitly clear them.)
func TestAuditRetiredFlags_AbsentEmitsNothing(t *testing.T) {
	cache.ResetRetiredFlagAuditForTest()
	t.Cleanup(cache.ResetRetiredFlagAuditForTest)
	// Ensure both retired names are unset for this test. t.Setenv with a
	// later Unsetenv is awkward; instead assert that the audit emits no
	// config.retired_flag_ignored line for any known retired name.
	for _, name := range cache.RetiredFlagNamesForTest() {
		// Defensive: if the ambient environment somehow carries one, skip
		// — the CI/dev env must not set these (they are retired).
		if _, present := os.LookupEnv(name); present {
			t.Skipf("ambient env carries retired flag %q; cannot assert the absent path", name)
		}
	}

	log, recs := newCaptureLogger()
	cache.AuditRetiredFlags(log)

	for _, r := range *recs {
		if r.msg == "config.retired_flag_ignored" {
			t.Fatalf("absent path: audit must emit nothing; got %+v", r)
		}
	}
}

// TestAuditRetiredFlags_WarnOncePerFlag — warn-once semantics: repeated
// AuditRetiredFlags calls (and concurrent ones) emit at most one line per
// flag for the process lifetime.
func TestAuditRetiredFlags_WarnOncePerFlag(t *testing.T) {
	cache.ResetRetiredFlagAuditForTest()
	t.Cleanup(cache.ResetRetiredFlagAuditForTest)
	t.Setenv("PREWARM_ENABLED", "false")
	t.Setenv("RESOLVER_USE_INFORMER", "false")

	log, recs := newCaptureLogger()

	// Many calls, several concurrent — the per-flag sync.Once must collapse
	// them to one line each.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.AuditRetiredFlags(log)
		}()
	}
	wg.Wait()
	// A few more serial calls for good measure.
	cache.AuditRetiredFlags(log)
	cache.AuditRetiredFlags(log)

	for _, flag := range []string{"PREWARM_ENABLED", "RESOLVER_USE_INFORMER"} {
		if got := findAuditRecords(*recs, flag); len(got) != 1 {
			t.Fatalf("warn-once: flag %q must emit exactly one line across repeated/concurrent calls; got %d",
				flag, len(got))
		}
	}
}

// TestAuditRetiredFlags_NilLoggerNoPanic — a nil logger is a no-op (a
// mis-wired caller must not panic at boot).
func TestAuditRetiredFlags_NilLoggerNoPanic(t *testing.T) {
	cache.ResetRetiredFlagAuditForTest()
	t.Cleanup(cache.ResetRetiredFlagAuditForTest)
	t.Setenv("PREWARM_ENABLED", "false")
	cache.AuditRetiredFlags(nil) // must not panic
}
