package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// makeSelfSignedPEM returns a freshly-generated self-signed CERTIFICATE
// PEM block. The cert is not committed to disk; it lives only in the
// test process's memory. Test fixtures derive from this PEM by
// applying single- or double-base64 wrapping.
func makeSelfSignedPEM(t *testing.T, cn string) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageCertSign,
		IsCA:         true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestNormalizeCAData_CorrectlyShapedSingleBase64PEM_PassThrough —
// the correctly-shaped contract: a single base64 encoding of PEM
// bytes (what plumbing/transport.go:59 is designed to decode in one
// pass). normalizeCAData must return the input unchanged.
func TestNormalizeCAData_CorrectlyShapedSingleBase64PEM_PassThrough(t *testing.T) {
	pemBytes := makeSelfSignedPEM(t, "single-base64")
	input := []byte(base64.StdEncoding.EncodeToString(pemBytes))

	got := normalizeCAData(input)
	if string(got) != string(input) {
		t.Fatalf("single-base64 PEM should be passthrough; got mutated bytes (len in=%d, len out=%d)", len(input), len(got))
	}
	// Round-trip: single-decode then pem.Decode must succeed.
	dec, err := base64.StdEncoding.DecodeString(string(got))
	if err != nil {
		t.Fatalf("single-decode of returned value failed: %v", err)
	}
	if block, _ := pem.Decode(dec); block == nil {
		t.Fatalf("returned value did not single-decode to PEM")
	}
}

// TestNormalizeCAData_DoubleBase64PEM_Normalized — the empirical
// broken shape (live <user>-clientconfig Secrets). normalizeCAData
// must return the inner (single-base64) form so that plumbing's one
// DecodeString call yields raw PEM.
func TestNormalizeCAData_DoubleBase64PEM_Normalized(t *testing.T) {
	pemBytes := makeSelfSignedPEM(t, "double-base64")
	single := base64.StdEncoding.EncodeToString(pemBytes) // base64-of-PEM
	input := []byte(base64.StdEncoding.EncodeToString([]byte(single)))

	got := normalizeCAData(input)
	if string(got) == string(input) {
		t.Fatalf("double-base64 PEM should be normalized; got passthrough")
	}
	if string(got) != single {
		t.Fatalf("expected inner single-base64 form; got len=%d want len=%d", len(got), len(single))
	}
	// Round-trip: plumbing's one DecodeString must yield raw PEM.
	dec, err := base64.StdEncoding.DecodeString(string(got))
	if err != nil {
		t.Fatalf("plumbing-equivalent single-decode failed: %v", err)
	}
	if block, _ := pem.Decode(dec); block == nil {
		t.Fatalf("normalized value did not single-decode to PEM")
	}
	// AppendCertsFromPEM must accept the round-trip (the real defect
	// site: discarded return value becomes true now).
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(dec) {
		t.Fatalf("AppendCertsFromPEM returned false for normalized PEM")
	}
}

// TestNormalizeCAData_Empty_Empty — empty CA field is a valid
// no-CA-configured signal; normalizeCAData must passthrough.
func TestNormalizeCAData_Empty_Empty(t *testing.T) {
	got := normalizeCAData([]byte{})
	if len(got) != 0 {
		t.Fatalf("empty input should yield empty output; got len=%d", len(got))
	}
	got2 := normalizeCAData(nil)
	if len(got2) != 0 {
		t.Fatalf("nil input should yield empty output; got len=%d", len(got2))
	}
}

// TestNormalizeCAData_Garbage_Garbage — non-base64 non-PEM bytes
// must passthrough so plumbing surfaces the upstream error verbatim.
func TestNormalizeCAData_Garbage_Garbage(t *testing.T) {
	input := []byte("not base64 !!! and not PEM either")
	got := normalizeCAData(input)
	if string(got) != string(input) {
		t.Fatalf("garbage input should passthrough; got mutated")
	}
}

// TestNormalizeCAData_SingleNonPEMString — single-base64 of bytes
// that are NOT PEM. normalizeCAData must passthrough (plumbing will
// decode once, find no PEM, and `AppendCertsFromPEM` will fail —
// but that is plumbing's contract, not ours to second-guess).
func TestNormalizeCAData_SingleNonPEMString(t *testing.T) {
	input := []byte(base64.StdEncoding.EncodeToString([]byte("definitely not a PEM body, just some prose")))
	got := normalizeCAData(input)
	if string(got) != string(input) {
		t.Fatalf("single-base64 non-PEM should passthrough; got mutated (len in=%d, len out=%d)", len(input), len(got))
	}
}

// TestNormalizeCAData_MultiplePEMBlocks — multi-cert PEM bundles
// must survive normalization with all blocks preserved.
func TestNormalizeCAData_MultiplePEMBlocks(t *testing.T) {
	a := makeSelfSignedPEM(t, "multi-a")
	b := makeSelfSignedPEM(t, "multi-b")
	bundle := append(a, b...)
	// Test BOTH shapes (single-base64 passthrough; double-base64 normalized).
	singleIn := []byte(base64.StdEncoding.EncodeToString(bundle))
	gotSingle := normalizeCAData(singleIn)
	if string(gotSingle) != string(singleIn) {
		t.Fatalf("single-base64 multi-PEM passthrough mutated")
	}

	doubleIn := []byte(base64.StdEncoding.EncodeToString(singleIn))
	gotDouble := normalizeCAData(doubleIn)
	if string(gotDouble) != string(singleIn) {
		t.Fatalf("double-base64 multi-PEM should normalize to single form")
	}
	// Confirm both certs survive parse.
	dec, err := base64.StdEncoding.DecodeString(string(gotDouble))
	if err != nil {
		t.Fatalf("normalized multi-PEM single-decode failed: %v", err)
	}
	count := 0
	rest := dec
	for {
		block, r := pem.Decode(rest)
		if block == nil {
			break
		}
		if strings.Contains(block.Type, "CERTIFICATE") {
			count++
		}
		rest = r
	}
	if count != 2 {
		t.Fatalf("expected 2 CERTIFICATE blocks after normalization; got %d", count)
	}
}
