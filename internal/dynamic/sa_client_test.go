// sa_client_test.go — Tag 0.30.9 Sub-scope A: unit tests for the
// snowplow ServiceAccount endpoint provider.
//
// The tests run outside a real cluster (no projected SA volume at
// /var/run/secrets/kubernetes.io/serviceaccount/). To assert the
// behaviour we'd need to either mock the os.ReadFile calls (heavy
// refactor) or accept the error path is the dominant code path under
// `go test`. We choose the latter: the tests confirm that
//   * outside a cluster, ServiceAccountEndpoint returns an error;
//   * the singleton stays nil on error (so a subsequent in-cluster
//     run is not poisoned);
//   * resetSAEndpointForTest clears the singleton.

package dynamic

import (
	"os"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
)

func TestServiceAccountEndpoint_NoTokenFileReturnsError(t *testing.T) {
	resetSAEndpointForTest()
	defer resetSAEndpointForTest()

	// Verify the production token path is absent in this test env.
	if _, err := os.Stat(saTokenPath); err == nil {
		t.Skipf("test environment unexpectedly has SA token at %s; cannot exercise error path", saTokenPath)
	}

	ep, err := ServiceAccountEndpoint()
	if err == nil {
		t.Fatalf("expected error outside cluster; got nil and ep=%+v", ep)
	}
	if ep != nil {
		t.Fatalf("expected nil endpoint on error; got %+v", ep)
	}
}

func TestServiceAccountEndpoint_ErrorPathDoesNotPoisonSingleton(t *testing.T) {
	resetSAEndpointForTest()
	defer resetSAEndpointForTest()

	if _, err := os.Stat(saTokenPath); err == nil {
		t.Skipf("test environment has SA token at %s; cannot exercise error path", saTokenPath)
	}

	// First call: error.
	if _, err := ServiceAccountEndpoint(); err == nil {
		t.Fatalf("expected first-call error outside cluster")
	}

	// Second call: must still error (singleton was NOT cached); a
	// successful-by-chance cache pollution would let a future
	// in-cluster restart see stale data.
	if _, err := ServiceAccountEndpoint(); err == nil {
		t.Fatalf("expected second-call error; got nil — singleton may have been poisoned by an error path")
	}
}

func TestServiceAccountEndpoint_ResetClearsSingleton(t *testing.T) {
	// Plant a synthetic singleton.
	saEndpointMu.Lock()
	saEndpointInstance = &endpoints.Endpoint{ServerURL: "test"}
	saEndpointMu.Unlock()

	resetSAEndpointForTest()

	saEndpointMu.Lock()
	got := saEndpointInstance
	saEndpointMu.Unlock()

	if got != nil {
		t.Fatalf("reset did not clear singleton; got %+v", got)
	}
}
