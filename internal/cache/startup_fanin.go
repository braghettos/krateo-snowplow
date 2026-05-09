// Q-OOM-STARTUP-FANIN (0.25.329) — bounded informer registration fan-in.
//
// Background. ResourceWatcher.Start used to register every persisted GVR
// in registerFromRedis (~33 GVRs in production) and then call factory.Start
// once. dynamicinformer.SharedInformerFactory.Run launches one goroutine
// per informer; each runs an initial paginated LIST and decodes the result
// concurrently with all peers. The Q-OOM-COMPLETION RCA captured in pprof
// runs (0.25.327, c1+c2) showed the inuse_space peak during this window
// exceeding 12 GiB at scale, breaching the 16 GiB pod limit on two
// sequential cold starts.
//
// Mechanism. We fan in informer registration in batches of `fanin`, calling
// factory.Start (idempotent — only newly-registered informers are started)
// after each batch and waiting for WaitForCacheSync before admitting the
// next. This caps the LIST-decode peak to `fanin` concurrent pipelines at
// the cost of a slightly longer startup wall time.
//
// Default. fanin defaults to 8 and is overridable via STARTUP_INFORMER_FANIN.
// Per PM amendment A8, NEVER fall back to unbounded — invalid / unset /
// non-positive values resolve to the default.
package cache

import (
	"os"
	"strconv"
	"strings"
)

// envStartupInformerFanin is the env knob name for bounded startup
// informer fan-in. Surface in chart values via the existing configmap
// passthrough mechanism (chart-debt — not a blocker for 0.25.329).
const envStartupInformerFanin = "STARTUP_INFORMER_FANIN"

// defaultStartupInformerFanin is the production-tested default. 8
// concurrent LIST-decode pipelines keep peak inuse_space well below the
// 12 GiB OOM threshold observed in 0.25.327 c1+c2 while still completing
// 33-GVR hydration in ~30-60 s (4-5 batches).
const defaultStartupInformerFanin = 8

// startupInformerFanin returns the bounded fan-in for startup informer
// registration. Valid env values (positive integers) override the default;
// anything else resolves to defaultStartupInformerFanin.
func startupInformerFanin() int {
	raw := strings.TrimSpace(os.Getenv(envStartupInformerFanin))
	if raw == "" {
		return defaultStartupInformerFanin
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		// PM A8: never unbounded. Fall back to the safe default.
		return defaultStartupInformerFanin
	}
	return n
}
