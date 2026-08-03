//go:build race

// race_enabled_test.go — Task #331 half (a). Sibling of
// race_disabled_test.go. The Go race detector defines the `race` build
// constraint when (and only when) the binary is built with `-race`
// (stdlib prior art: runtime/race/race.go uses `//go:build race`, and
// internal/race gates its no-op vs real shims on the same constraint).
//
// raceEnabledForTest lets a test ask "am I running under -race?" at
// compile time, with ZERO runtime cost and no dependency on
// testing.Short() (CI may run -short; the two signals are orthogonal).
//
// Sole consumer: TestValidateClusterListShape_Overhead (cluster_list_test.go),
// which keeps a wall-clock perf-budget canary for gross algorithmic
// blowups but SKIPS it under -race because the race detector's memory-
// access instrumentation inflates pure-CPU latency 2-8× (validated
// 2026-06-12: 70-294ms observed on the 2K-item envelope under -race vs a
// <10ms clean budget), which invalidates the budget as a proxy. The
// instrumentation-invariant MECHANISM tooth (exactly-1 materialisation
// pass) runs in BOTH build modes and is the always-on regression signal.
package api

const raceEnabledForTest = true
