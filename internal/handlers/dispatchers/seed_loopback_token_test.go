// seed_loopback_token_test.go — #57 (C-a) falsifier for the prewarm seed's
// authn-token install + the degrade-not-fail posture.
//
// The seed acquires an authn JWT (via the wired provider = authn.Client.Token)
// and installs it on the prewarm ctx (xcontext.WithAccessToken) so a nested
// loopback /call carries a bearer snowplow's middleware accepts. C-a: a
// token-acquisition error MUST degrade (WARN + expvar counter, token-less ctx)
// — NEVER fail the boot/warmup.

package dispatchers

import (
	"context"
	"errors"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
)

func TestInstallSeedLoopbackToken_InstallsWhenProviderSucceeds(t *testing.T) {
	t.Cleanup(func() { SetSeedLoopbackTokenProvider(nil) })
	SetSeedLoopbackTokenProvider(func(context.Context) (string, error) {
		return "authn-jwt-xyz", nil
	})

	got := installSeedLoopbackToken(context.Background())
	if tok, _ := xcontext.AccessToken(got); tok != "authn-jwt-xyz" {
		t.Fatalf("seed ctx AccessToken = %q, want the provider's JWT installed", tok)
	}
}

func TestInstallSeedLoopbackToken_DegradesOnProviderError(t *testing.T) {
	t.Cleanup(func() { SetSeedLoopbackTokenProvider(nil) })
	before := SeedLoopbackTokenErrTotal()
	SetSeedLoopbackTokenProvider(func(context.Context) (string, error) {
		return "", errors.New("authn returned 403: no allowlist CR")
	})

	// MUST NOT panic / fail — returns the ctx UNCHANGED (token-less seed).
	got := installSeedLoopbackToken(context.Background())
	if tok, _ := xcontext.AccessToken(got); tok != "" {
		t.Fatalf("on a token error the seed ctx must carry NO access token (degrade); got %q", tok)
	}
	if SeedLoopbackTokenErrTotal() != before+1 {
		t.Fatalf("a token-acquisition error must bump SeedLoopbackTokenErrTotal (C-a observability); before=%d after=%d",
			before, SeedLoopbackTokenErrTotal())
	}
}

func TestInstallSeedLoopbackToken_DegradesOnEmptyToken(t *testing.T) {
	t.Cleanup(func() { SetSeedLoopbackTokenProvider(nil) })
	before := SeedLoopbackTokenErrTotal()
	SetSeedLoopbackTokenProvider(func(context.Context) (string, error) {
		return "", nil // no error but empty — still a degrade (counted)
	})
	got := installSeedLoopbackToken(context.Background())
	if tok, _ := xcontext.AccessToken(got); tok != "" {
		t.Fatalf("empty token must not be installed; got %q", tok)
	}
	if SeedLoopbackTokenErrTotal() != before+1 {
		t.Fatalf("an empty token must bump the degrade counter; before=%d after=%d", before, SeedLoopbackTokenErrTotal())
	}
}

func TestInstallSeedLoopbackToken_NoProviderIsPreR1NoOp(t *testing.T) {
	SetSeedLoopbackTokenProvider(nil) // authn not configured
	before := SeedLoopbackTokenErrTotal()
	got := installSeedLoopbackToken(context.Background())
	if tok, _ := xcontext.AccessToken(got); tok != "" {
		t.Fatalf("with no provider wired the seed ctx must be unchanged (pre-#57 token-less); got %q", tok)
	}
	// No provider ≠ an error — the degrade counter must NOT tick (this is the
	// configured-off path, not a failure).
	if SeedLoopbackTokenErrTotal() != before {
		t.Fatalf("no-provider is the configured-off path, not an error — counter must not tick; before=%d after=%d",
			before, SeedLoopbackTokenErrTotal())
	}
}
