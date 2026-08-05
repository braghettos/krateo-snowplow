package cache

import (
	"os"
	"testing"
)

// The cache tests build SharedIndexInformers from in-memory fake ListerWatchers
// (e.g. fakeListWatch). client-go >= v0.32 enables the WatchListClient feature by
// default, which makes the reflector do a streaming WatchList that blocks in
// WaitForCacheSync awaiting an initial-events bookmark the fakes never emit —
// hanging tests until the go test timeout. Disable it for the whole package so the
// informers use the classic List+Watch path the fakes implement.
func TestMain(m *testing.M) {
	os.Setenv("KUBE_FEATURE_WatchListClient", "false")
	os.Exit(m.Run())
}
