// Package unbalanced is the L-SCOPE-COMPLETENESS RED-arm fixture. It is NOT
// compiled into the binary (it lives under a testdata dir the real gate run
// SkipDir's) and NOT a _test.go file (the walker skips those). It is fed to
// the checker as an EXPLICIT root by the self-test to prove the gate
// DISCRIMINATES: the checker must FLAG the two offending ResolvedEntry Put
// sites below while accepting the two well-formed ones.
//
// This reproduces the #118(d) enumeration-incompleteness shape in miniature:
// a class of ResolvedEntry Put sites where SOME sites participate in the
// per-entry metadata discipline (set TTLOverride or carry a waiver) and OTHERS
// silently do not — exactly the seed-Put-missed defect.
package unbalanced

import (
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

func stampedSite(handle *cache.ResolvedCacheStore, key string, encoded []byte, inputs *cache.ResolvedKeyInputs) {
	// WELL-FORMED: sets the enforced field. Must NOT be flagged.
	handle.Put(key, &cache.ResolvedEntry{
		RawJSON:     encoded,
		Inputs:      inputs,
		TTLOverride: 30 * time.Second,
	})
}

func waivedSite(handle *cache.ResolvedCacheStore, key string, encoded []byte, inputs *cache.ResolvedKeyInputs) {
	// WELL-FORMED: carries a waiver with a reason. Must NOT be flagged.
	// scope-waiver:TTLOverride: fixture — identity-free substrate, documented exempt.
	handle.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  inputs,
	})
}

func missedSite(handle *cache.ResolvedCacheStore, key string, encoded []byte, inputs *cache.ResolvedKeyInputs) {
	// OFFENDING (the #118d shape): neither sets TTLOverride nor carries a
	// waiver — the silently-missed Put the gate must FLAG.
	handle.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  inputs,
	})
}

func emptyReasonSite(handle *cache.ResolvedCacheStore, key string, encoded []byte, inputs *cache.ResolvedKeyInputs) {
	// OFFENDING: a waiver with NO reason — a silent opt-out the gate must FLAG.
	// scope-waiver:TTLOverride:
	handle.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  inputs,
	})
}
