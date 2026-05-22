// scripts/sa-endpoint-shape-proof.go — Ship 0.30.166 pre-deploy proof.
//
// Per `feedback_no_kubectl_jsonpath_for_in_process_reasoning` (persisted
// after the 0.30.165 wrong-defect failure): NO kubectl-jsonpath probes
// of SA wire shape — jsonpath auto-decodes wire-base64, which silently
// double-decodes vs the in-process client-go shape. This program builds
// the EXACT plumbing.endpoints.Endpoint struct the production fix path
// constructs at `internal/dynamic/sa_client.go:100-105` from the projected
// SA volume, and dumps `HasCertAuth()` / `HasCA()` + the byte-level CA
// shape so the architect can verify the precondition for the 0.30.166
// fix is real.
//
// Gate (all three MUST hold or the precondition for the fix is invalid):
//   1. HasCertAuth() == false   (token-auth, no client cert)
//   2. HasCA() == true          (cluster CA is carried on the endpoint)
//   3. CA bytes begin with "-----BEGIN CERTIFICATE-----"
//      (single-decoded PEM — NOT double-base64; the in-process shape
//      `normalizeCAData` was supposed to defend against would FAIL this)
//
// Run inside a pod mounted with the snowplow ServiceAccount:
//   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags='-s -w' \
//     -o /tmp/sa-proof ./scripts/sa-endpoint-shape-proof.go
//   # then kubectl run a probe pod that has /tmp/sa-proof and the
//   # serviceAccountName=snowplow projected SA volume.

//go:build ignore

package main

import (
	"fmt"
	"os"

	"github.com/krateoplatformops/plumbing/endpoints"
)

const (
	saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

func main() {
	tokenBytes, err := os.ReadFile(saTokenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read SA token: %v\n", err)
		os.Exit(1)
	}
	caBytes, err := os.ReadFile(saCAPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read SA CA: %v\n", err)
		os.Exit(1)
	}

	// EXACT shape produced by internal/dynamic/sa_client.go:100-105.
	ep := &endpoints.Endpoint{
		ServerURL:                "https://kubernetes.default.svc",
		Token:                    string(tokenBytes),
		CertificateAuthorityData: string(caBytes),
		Insecure:                 false,
	}

	ca := ep.CertificateAuthorityData
	prefixLen := 64
	if len(ca) < prefixLen {
		prefixLen = len(ca)
	}
	fmt.Printf("ServerURL:                    %s\n", ep.ServerURL)
	fmt.Printf("len(Token):                   %d\n", len(ep.Token))
	fmt.Printf("len(CertificateAuthorityData): %d\n", len(ca))
	fmt.Printf("HasCertAuth():                %t\n", ep.HasCertAuth())
	fmt.Printf("HasCA():                      %t\n", ep.HasCA())
	fmt.Printf("CA first %d bytes:             %q\n", prefixLen, ca[:prefixLen])

	const pemPrefix = "-----BEGIN CERTIFICATE-----"
	gateOK := !ep.HasCertAuth() && ep.HasCA() &&
		len(ca) >= len(pemPrefix) && ca[:len(pemPrefix)] == pemPrefix
	fmt.Printf("\nGATE (HasCertAuth==false && HasCA==true && PEM prefix): %t\n", gateOK)
	if !gateOK {
		os.Exit(2)
	}
}
