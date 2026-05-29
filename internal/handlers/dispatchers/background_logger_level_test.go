// background_logger_level_test.go — 0.30.207 regression guard for the
// background-path DEBUG-leak.
//
// ROOT CAUSE (pre-0.30.207): the L1 refresher / cohort-seed background
// contexts are bare context.Background() derivatives that never run
// through xcontext.WithLogger (unlike the HTTP path's use.Logger
// middleware). Every background-resolve emit site calls
// xcontext.Logger(ctx); on a logger-less context that helper returns a
// HARDCODED slog.LevelDebug handler (plumbing/context.Logger), so the hot
// refresher loop emitted full-dict DEBUG lines even with DEBUG=false.
//
// FIX: seed the background contexts with the level-configured logger via
// xcontext.WithLogger(log). These tests pin the mechanism the fix relies
// on: a WithLogger-decorated context honors the handler's level (Debug
// suppressed at Info, emitted at Debug), while a bare context falls back
// to the hardcoded-Debug logger (the bug we are guarding against).
package dispatchers

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
)

// newLevelLogger builds a JSON logger exactly the way main.go does:
// LevelInfo when DEBUG is off, LevelDebug when on. Writes to buf so the
// test can inspect what was actually emitted.
func newLevelLogger(buf *bytes.Buffer, debugOn bool) *slog.Logger {
	lvl := slog.LevelInfo
	if debugOn {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: lvl}))
}

// TestBackgroundLogger_DebugOff_SuppressesDebug proves that a background
// context seeded via xcontext.WithLogger with the DEBUG=false logger
// suppresses Debug emits — the customer-visible win of the fix.
func TestBackgroundLogger_DebugOff_SuppressesDebug(t *testing.T) {
	var buf bytes.Buffer
	log := newLevelLogger(&buf, false /* DEBUG=false */)

	// Mirror the fix: background ctx carries the level-configured logger.
	ctx := xcontext.BuildContext(context.Background(), xcontext.WithLogger(log))

	// The exact retrieval the hot-path emit sites use.
	got := xcontext.Logger(ctx)
	got.Debug("resolveAndPopulateL1: re-resolved + stored", slog.String("subsystem", "cache"))
	got.Info("kept-line", slog.String("subsystem", "cache"))

	out := buf.String()
	if strings.Contains(out, `"level":"DEBUG"`) {
		t.Fatalf("DEBUG=false must suppress Debug logs on the background path, but a DEBUG line was emitted:\n%s", out)
	}
	if !strings.Contains(out, `"level":"INFO"`) {
		t.Fatalf("Info logging must still be emitted (no content suppressed); got:\n%s", out)
	}
}

// TestBackgroundLogger_DebugOn_EmitsDebug proves the flag still toggles ON
// — DEBUG=true keeps the debug lines (we gate by level, we don't delete
// logging).
func TestBackgroundLogger_DebugOn_EmitsDebug(t *testing.T) {
	var buf bytes.Buffer
	log := newLevelLogger(&buf, true /* DEBUG=true */)

	ctx := xcontext.BuildContext(context.Background(), xcontext.WithLogger(log))

	got := xcontext.Logger(ctx)
	got.Debug("resolved api", slog.String("subsystem", "cache"))

	if !strings.Contains(buf.String(), `"level":"DEBUG"`) {
		t.Fatalf("DEBUG=true must still emit Debug logs; got:\n%s", buf.String())
	}
}

// TestBackgroundLogger_BareContext_FallsBackToDebug documents the bug the
// fix closes: a logger-less context (the pre-0.30.207 background path)
// resolves to plumbing's hardcoded slog.LevelDebug fallback regardless of
// the DEBUG env var. If plumbing ever stops hardcoding Debug here, this
// test will fail and we can re-evaluate whether the WithLogger seeding is
// still required.
func TestBackgroundLogger_BareContext_FallsBackToDebug(t *testing.T) {
	// A bare background context — NO WithLogger. This is exactly what the
	// refresher passed before the fix.
	ctx := context.Background()
	got := xcontext.Logger(ctx)

	// We cannot redirect this logger's writer (it's hardcoded to stderr in
	// plumbing), so assert the LEVEL is enabled rather than capturing
	// output. Enabled(Debug)==true reproduces the leak condition.
	if !got.Enabled(ctx, slog.LevelDebug) {
		t.Fatalf("expected the logger-less fallback to be Debug-enabled (the pre-fix leak condition); " +
			"if plumbing no longer hardcodes LevelDebug here, re-evaluate the WithLogger seeding")
	}
}
