//go:build !race

// race_disabled_test.go — Task #331 half (a). Sibling of
// race_enabled_test.go (see that file's header for the full rationale and
// the stdlib `//go:build race` prior art). This is the default build
// (no -race): the wall-clock perf-budget canary in
// TestValidateClusterListShape_Overhead is EXERCISED.
package api

const raceEnabledForTest = false
