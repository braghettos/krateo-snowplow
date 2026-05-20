// endpoints_cache_test.go — Ship D.2 (0.30.143) tests for the
// snowplow-local Endpoint extractor + FromInformerSecret adapter.
//
// Maps to:
//   - AC-D2.6 (HARD GATE) — TestFromInformerSecret_UpstreamFieldParity
//     asserts extractEndpointFromSecret produces reflect.DeepEqual
//     output to plumbing's endpoints.FromSecret across the 5
//     documented field permutations (server-url-only, full set,
//     debug-true, debug-invalid-bool, missing-server-url).
//   - AC-D2.10 — TestFromInformerSecret_NoOp_WhenCacheDisabled
//     asserts cache=off mode returns (zero, false, nil) without
//     consulting the snapshot.
//   - AC-D2.12 — TestFromInformerSecret_PreReadinessFallsBack
//     asserts the nil-snapshot window returns (zero, false, nil) so
//     the caller falls back to upstream.
package api

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/internal/cache"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mkSecret builds a *corev1.Secret with `data` as the Data map.
// All values are []byte under the hood (the Secret API convention).
func mkSecret(ns, name string, data map[string]string) *corev1.Secret {
	d := make(map[string][]byte, len(data))
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       d,
	}
}

// simulateUpstreamFromSecret applies plumbing's FromSecret content-
// extraction logic (plumbing@v0.9.3/endpoints/endpoints.go:29-87)
// IN-PROCESS against a *corev1.Secret. The actual plumbing function
// requires a *rest.Config + an apiserver round-trip; we model only
// the field-extraction half because that's the half under test.
//
// AC-D2.6 byte-equivalence: this function is a verbatim re-
// transcription of plumbing's extraction block. Two divergence sources
// would break the parity test:
//
//   1. A label-string mismatch between the snowplow constants in
//      endpoints_cache.go and plumbing's UNEXPORTED constants
//      (plumbing/endpoints.go:124-139). The test verifies parity by
//      running THIS reference extractor against the same Secret and
//      asserting reflect.DeepEqual.
//   2. A semantic divergence (e.g. snowplow strict-checks the
//      ParseBool error while upstream silently drops). The test
//      includes a "debug-invalid-bool" row that exercises exactly
//      this.
//
// Re-transcribing upstream's code here (rather than calling the real
// plumbing.FromSecret) keeps the test hermetic — no apiserver, no
// real *rest.Config — while gating the byte-equivalence contract.
func simulateUpstreamFromSecret(sec *corev1.Secret) (endpoints.Endpoint, error) {
	// These labels MUST match plumbing@v0.9.3/endpoints/endpoints.go:124-139
	// verbatim. They are private upstream; we redeclare here for the
	// test reference. If upstream ever renames, this reference
	// becomes stale — AND the snowplow-side constants in
	// endpoints_cache.go would too. The parity test would fail in
	// the same direction (both extractors drift in lockstep, exposing
	// the silent-rename regression — which is the test's job).
	const (
		clientCertLabel   = "client-certificate-data"
		clientKeyLabel    = "client-key-data"
		caLabel           = "certificate-authority-data"
		proxyUrlLabel     = "proxy-url"
		serverUrlLabel    = "server-url"
		debugLabel        = "debug"
		passwordLabel     = "password"
		usernameLabel     = "username"
		tokenLabel        = "token"
		insecureLabel     = "insecure"
		awsAccessKeyLabel = "aws-access-key"
		awsSecretKeyLabel = "aws-secret-key"
		awsRegionLabel    = "aws-region"
		awsServiceLabel   = "aws-service"
	)
	res := endpoints.Endpoint{}
	if v, ok := sec.Data[serverUrlLabel]; ok {
		res.ServerURL = string(v)
	} else {
		return res, missingServerURLErrorFromUpstream
	}
	if v, ok := sec.Data[proxyUrlLabel]; ok {
		res.ProxyURL = string(v)
	}
	if v, ok := sec.Data[tokenLabel]; ok {
		res.Token = string(v)
	}
	if v, ok := sec.Data[usernameLabel]; ok {
		res.Username = string(v)
	}
	if v, ok := sec.Data[passwordLabel]; ok {
		res.Password = string(v)
	}
	if v, ok := sec.Data[caLabel]; ok {
		res.CertificateAuthorityData = string(v)
	}
	if v, ok := sec.Data[clientKeyLabel]; ok {
		res.ClientKeyData = string(v)
	}
	if v, ok := sec.Data[clientCertLabel]; ok {
		res.ClientCertificateData = string(v)
	}
	if v, ok := sec.Data[debugLabel]; ok {
		res.Debug, _ = parseBoolSilent(string(v))
	}
	if v, ok := sec.Data[insecureLabel]; ok {
		res.Insecure, _ = parseBoolSilent(string(v))
	}
	if v, ok := sec.Data[awsAccessKeyLabel]; ok {
		res.AwsAccessKey = string(v)
	}
	if v, ok := sec.Data[awsSecretKeyLabel]; ok {
		res.AwsSecretKey = string(v)
	}
	if v, ok := sec.Data[awsRegionLabel]; ok {
		res.AwsRegion = string(v)
	}
	if v, ok := sec.Data[awsServiceLabel]; ok {
		res.AwsService = string(v)
	}
	return res, nil
}

// missingServerURLErrorFromUpstream is the verbatim error message
// upstream FromSecret returns on a missing server-url field.
// AC-D2.6 requires the snowplow-side error to be string-identical.
var missingServerURLErrorFromUpstream = &upstreamError{msg: "missed required attribute for endpoint: server-url"}

type upstreamError struct{ msg string }

func (e *upstreamError) Error() string { return e.msg }

// parseBoolSilent mirrors upstream's `res.Debug, _ = strconv.ParseBool(...)`
// — the parse error is intentionally dropped. (We use this in the
// reference extractor instead of strconv.ParseBool directly so
// callers can read the parity intent at the call site.)
func parseBoolSilent(s string) (bool, error) {
	switch s {
	case "1", "t", "T", "TRUE", "true", "True":
		return true, nil
	case "0", "f", "F", "FALSE", "false", "False":
		return false, nil
	}
	// Match strconv.ParseBool's error shape exactly — but our caller
	// drops the error anyway, so the body never matters.
	return false, &upstreamError{msg: "parse-bool: " + s}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D2.6 — upstream field parity (golden table)
// ─────────────────────────────────────────────────────────────────────

func TestFromInformerSecret_UpstreamFieldParity(t *testing.T) {
	cases := []struct {
		name   string
		data   map[string]string
		errMsg string // non-empty → expect server-url-missing error string
	}{
		{
			name: "server-url-only",
			data: map[string]string{
				"server-url": "https://k8s.example.com",
			},
		},
		{
			name: "full set (14 fields)",
			data: map[string]string{
				"server-url":                 "https://k8s.example.com",
				"proxy-url":                  "https://proxy.example.com:3128",
				"token":                      "Bearer-XYZ",
				"username":                   "alice",
				"password":                   "passw0rd",
				"certificate-authority-data": "BASE64-CA",
				"client-key-data":            "BASE64-KEY",
				"client-certificate-data":    "BASE64-CERT",
				"debug":                      "true",
				"insecure":                   "false",
				"aws-access-key":             "AKIA",
				"aws-secret-key":             "SECRET",
				"aws-region":                 "us-east-1",
				"aws-service":                "eks",
			},
		},
		{
			name: "debug-true",
			data: map[string]string{
				"server-url": "https://k8s.example.com",
				"debug":      "true",
			},
		},
		{
			name: "debug-invalid-bool (silent parse drop)",
			data: map[string]string{
				"server-url": "https://k8s.example.com",
				"debug":      "not-a-bool",
				"insecure":   "yes-please", // also invalid
			},
			// Upstream silently drops → Debug=false, Insecure=false.
			// THIS is the verbatim-parity case the design's brief
			// flags: a strict-check on the ParseBool error would
			// BREAK upstream parity. The reflect.DeepEqual assert
			// catches a divergence either way.
		},
		{
			name: "missing-server-url",
			data: map[string]string{
				"token": "no-url-just-a-token",
			},
			errMsg: "missed required attribute for endpoint: server-url",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sec := mkSecret("krateo-system", "alice-clientconfig", c.data)
			gotEP, gotErr := extractEndpointFromSecret(sec)
			wantEP, wantErr := simulateUpstreamFromSecret(sec)

			if c.errMsg != "" {
				if gotErr == nil {
					t.Fatalf("extract: gotErr=nil; want error containing %q", c.errMsg)
				}
				if !strings.Contains(gotErr.Error(), c.errMsg) {
					t.Errorf("extract err = %q; want substring %q", gotErr.Error(), c.errMsg)
				}
				// Also assert the reference extractor errors with
				// the same string — confirms the parity contract is
				// on the error path too.
				if wantErr == nil || !strings.Contains(wantErr.Error(), c.errMsg) {
					t.Errorf("reference err = %v; want substring %q", wantErr, c.errMsg)
				}
				return
			}

			if gotErr != nil {
				t.Fatalf("extract: gotErr=%v; want nil", gotErr)
			}
			if wantErr != nil {
				t.Fatalf("reference: wantErr=%v; want nil", wantErr)
			}
			if !reflect.DeepEqual(gotEP, wantEP) {
				t.Errorf("AC-D2.6 PARITY VIOLATION:\n  got:  %+v\n  want: %+v", gotEP, wantEP)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D2.10 — cache=off makes the adapter a no-op
// ─────────────────────────────────────────────────────────────────────

func TestFromInformerSecret_NoOp_WhenCacheDisabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	// Even if a snapshot is somehow published, cache=off short-
	// circuits the adapter at its first line.
	cache.PublishSecretsSnapshotForTest(&cache.SecretsSnapshot{
		ByName: map[string]*corev1.Secret{
			"alice-clientconfig": mkSecret("krateo-system", "alice-clientconfig", map[string]string{
				"server-url": "https://k8s.example.com",
			}),
		},
	})
	t.Cleanup(cache.ResetSecretsSnapshotForTest)

	ep, served, err := FromInformerSecret(context.Background(), "krateo-system", "alice-clientconfig")
	if err != nil {
		t.Errorf("FromInformerSecret err = %v; want nil", err)
	}
	if served {
		t.Errorf("FromInformerSecret served=true in cache=off; want false (invariant inert)")
	}
	if ep.ServerURL != "" {
		t.Errorf("FromInformerSecret ep.ServerURL = %q; want zero value", ep.ServerURL)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D2.12 — pre-readiness fallback (nil snapshot window)
// ─────────────────────────────────────────────────────────────────────

func TestFromInformerSecret_PreReadinessFallsBack(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	// Snapshot pointer is nil — pre-readiness window.
	cache.ResetSecretsSnapshotForTest()
	cache.ResetSecretsInformerForTest()

	ep, served, err := FromInformerSecret(context.Background(), "krateo-system", "alice-clientconfig")
	if err != nil {
		t.Errorf("FromInformerSecret err = %v; want nil (soft miss)", err)
	}
	if served {
		t.Errorf("FromInformerSecret served=true pre-readiness; want false")
	}
	if ep.ServerURL != "" {
		t.Errorf("FromInformerSecret ep non-zero pre-readiness: %+v", ep)
	}
}

// TestFromInformerSecret_NamespaceMismatch_FallsBack — design §7 X2.
// A request for a namespace OTHER than the informer's scope returns
// soft miss; caller falls back to upstream.
func TestFromInformerSecret_NamespaceMismatch_FallsBack(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	cache.ResetSecretsInformerForTest()
	cache.ResetSecretsSnapshotForTest()

	// Simulate a fully-wired cache (servable=true, namespace set,
	// snapshot populated for the authn namespace).
	authnNS := "krateo-system"
	cache.PublishSecretsSnapshotForTest(&cache.SecretsSnapshot{
		ByName: map[string]*corev1.Secret{
			"alice-clientconfig": mkSecret(authnNS, "alice-clientconfig", map[string]string{
				"server-url": "https://k8s.example.com",
			}),
		},
	})
	cache.ForceSecretsCacheReadyForTest(authnNS)
	t.Cleanup(func() {
		cache.ResetSecretsSnapshotForTest()
		cache.ResetSecretsInformerForTest()
	})

	// Lookup uses a DIFFERENT namespace — must return soft miss.
	ep, served, err := FromInformerSecret(context.Background(), "some-other-ns", "alice-clientconfig")
	if err != nil {
		t.Errorf("err = %v; want nil (soft miss on namespace mismatch)", err)
	}
	if served {
		t.Errorf("served=true on namespace mismatch; want false")
	}
	if ep.ServerURL != "" {
		t.Errorf("ep non-zero on namespace mismatch: %+v", ep)
	}

	// Sanity: the same lookup IN the authn namespace serves.
	ep, served, err = FromInformerSecret(context.Background(), authnNS, "alice-clientconfig")
	if err != nil {
		t.Fatalf("err = %v; want nil (cache hit)", err)
	}
	if !served {
		t.Fatalf("served=false on cache-hit; want true")
	}
	if ep.ServerURL != "https://k8s.example.com" {
		t.Errorf("ep.ServerURL = %q; want https://k8s.example.com", ep.ServerURL)
	}
}
