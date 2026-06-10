// snapshot_authz_memo_expvar.go — Ship L2 (0.30.253), Task #291.
//
// Read-only expvar exposure of the snapshot authz memo counters
// (snapshot_authz_memo.go). Mirrors the sister idiom at
// cache/rbac_snapshot_expvar.go:54 (expvar.Publish + expvar.Func +
// sync.Once) so the F1/F5 falsifiers can read hit-rate, swap count, and
// live entry count without any code path other than /debug/vars.
//
// KEYS:
//   snowplow_authz_memo_hits    — cumulative memo hits (uint64)
//   snowplow_authz_memo_misses  — cumulative memo misses (uint64)
//   snowplow_authz_memo_swaps   — cumulative generation shard swaps (uint64)
//   snowplow_authz_memo_refused — cap-breach refused inserts (uint64) — B5
//   snowplow_authz_memo_entries — live entry count of the current shard (int)
//
// Hit rate (F5 acceptance >= 0.85) = hits / (hits + misses).
//
// REGISTRATION: idempotent via sync.Once; called from main.go's HTTP
// bootstrap next to RegisterRBACSnapshotExpvar(). expvar.Publish panics
// on duplicate keys; the Once prevents that under repeated invocation
// from tests or dual call sites.

package rbac

import (
	"expvar"
	"sync"
)

var authzMemoExpvarOnce sync.Once

// RegisterAuthzMemoExpvar publishes the snowplow_authz_memo_* expvar
// keys. Idempotent under repeated invocation. Called from main.go's HTTP
// mux bootstrap; safe to call from tests via the public name.
func RegisterAuthzMemoExpvar() {
	authzMemoExpvarOnce.Do(func() {
		for name, fn := range authzMemoExpvarFuncs() {
			expvar.Publish(name, fn)
		}
	})
}
