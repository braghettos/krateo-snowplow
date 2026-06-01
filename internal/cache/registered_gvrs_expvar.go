// registered_gvrs_expvar.go — Ship 1 / 0.30.225 (folds in task #115).
// Exposes the live set of GVRs that have a registered informer at
// /debug/vars under key `snowplow_plurals_registered_gvrs`. The
// value is an envelope:
//
//	{
//	  "count": <int>,
//	  "gvrs":  ["<group>/<version>/<resource>", ...],   // sorted
//	  "last_register_unix_ns": <int64>                   // 0 until first insert
//	}
//
// SHAPE PARITY — mirrors controller_health_expvar.go's
// closure-over-snapshot pattern. `count` is redundant with
// `len(gvrs)` but is cheap to compute and lets the bench tooling
// scrape a single int without parsing the array. `last_register_unix_ns`
// is the ground-truth "informer set last changed" signal Ship 2's
// entry-gate consumes to detect quiescence — two consecutive scrapes
// with identical ns means no informer was added in between.
//
// SHIP 2 PREP — Ship 2's pre-flight ceiling baseline is "the number
// of unique GVRs in the walker corpus at end of Phase 1". This
// expvar is the runtime read-out the bench tooling consumes to
// capture that baseline without standing up a Go test harness. Saved
// to /tmp/Nunique-gvks-0.30.225.txt during Ship 1 deploy validation
// for Ship 2 falsifier (gate: cache.fallthrough.plurals_discovery_hop
// ≤ N_unique_gvks_in_walker_corpus across entire process lifetime).
//
// READ PATH — calls rw.RegisteredGVRs() (phase1.go:390), which
// snapshots rw.informers under RLock. Cheap relative to scrape
// cadence (Prometheus scrapes are O(seconds), informer set is
// bounded by GVR set). The snapshot is sorted in place before
// publication — sort cost is negligible for ~50 entries.
//
// WRITE PATH — lastRegisterNS is updated by NotifyGVRRegistered,
// invoked at the two insert sites in watcher.go after a fresh
// `rw.informers[gvr] = gi` insertion (NOT on idempotent re-entries
// — those early-return before reaching the notify call). Stores
// time.Now().UnixNano() via atomic.Int64; readers consume via
// Load(). Race-free.
//
// CFG-1 (Ship 0.30.163) compliance — under CACHE_ENABLED=false the
// global watcher is nil; the expvar handler returns an empty
// envelope (count=0, gvrs=[], last_register_unix_ns=0). NOT an
// error, NOT a panic. Matches the controller_health_expvar pattern.

package cache

import (
	"expvar"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// RegisteredGVRsSnapshot is the JSON envelope published at
// /debug/vars under `snowplow_plurals_registered_gvrs`. Fields are
// exported so encoding/json (via expvar) can marshal them.
type RegisteredGVRsSnapshot struct {
	Count              int      `json:"count"`
	GVRs               []string `json:"gvrs"`                  // sorted, "group/version/resource"
	LastRegisterUnixNS int64    `json:"last_register_unix_ns"` // 0 until first insert; ns-precision wall clock
}

// lastRegisterNS holds the wall-clock UnixNano of the most recent
// successful GVR insert into rw.informers. Updated by
// NotifyGVRRegistered at the two insert sites in watcher.go
// (addResourceTypeLocked + addResourceTypeMetadataOnlyLocked).
// Zero until the first insert. Reads / writes via atomic.Int64.
var lastRegisterNS atomic.Int64

// NotifyGVRRegistered records that a fresh GVR was just inserted
// into rw.informers. Invoked from the two insert sites in
// watcher.go AFTER `rw.informers[gvr] = gi` succeeded for a NEW
// gvr (the existence-check guards at watcher.go:971 / :728
// early-return for idempotent re-entries, so this notify only
// fires on genuine inserts). Safe to call under rw.mu — atomic
// store, no allocation, no lock.
func NotifyGVRRegistered() {
	lastRegisterNS.Store(time.Now().UnixNano())
}

var registeredGVRsExpvarOnce sync.Once

func init() {
	if Disabled() {
		return
	}
	registerRegisteredGVRsExpvar()
}

// registerRegisteredGVRsExpvar publishes the expvar handle.
// Guarded by sync.Once so it is safe to call from both init() and a
// test helper.
func registerRegisteredGVRsExpvar() {
	registeredGVRsExpvarOnce.Do(func() {
		expvar.Publish("snowplow_plurals_registered_gvrs", expvar.Func(registeredGVRsExpvarValue))
	})
}

// registeredGVRsExpvarValue returns the snapshot envelope. Empty
// envelope (count=0, gvrs=[], last_register_unix_ns=0) when the
// global watcher is nil (cache=off / not yet wired). Never returns
// nil — concrete zero envelope is cleaner JSON for consumers.
func registeredGVRsExpvarValue() any {
	rw := Global()
	if rw == nil {
		return RegisteredGVRsSnapshot{
			Count:              0,
			GVRs:               []string{},
			LastRegisterUnixNS: 0,
		}
	}
	gvrs := rw.RegisteredGVRs()
	out := make([]string, 0, len(gvrs))
	for _, gvr := range gvrs {
		out = append(out, gvr.Group+"/"+gvr.Version+"/"+gvr.Resource)
	}
	sort.Strings(out)
	return RegisteredGVRsSnapshot{
		Count:              len(out),
		GVRs:               out,
		LastRegisterUnixNS: lastRegisterNS.Load(),
	}
}
