// controller_health_integration_test.go — Ship Resilience-1
// (0.30.162). AC-R1.6 / HG-1 integration test driven by an actual
// kind cluster. Build-tag `integration` so it does NOT run as part
// of the default `go test ./...` invocation; CI does NOT run this.
//
// Invocation:
//
//   export PATH="/opt/homebrew/bin:$PATH"
//   export KUBECONFIG="$(kind get kubeconfig --name resilience1-synth | tr -d '\r')"
//   # KUBECONFIG above is a YAML blob, not a path; use this form:
//   kind get kubeconfig --name resilience1-synth > /tmp/resilience1-kubeconfig
//   KUBECONFIG=/tmp/resilience1-kubeconfig CACHE_ENABLED=true \
//     go test -tags=integration -count=1 -v -run TestHG1 \
//     ./internal/cache/
//
// Pre-stage assumed: the kind cluster `resilience1-synth` exists
// with the fake-controller-resilience1 manifests applied per
// /tmp/snowplow-runs/0.30.162/before/synth-test/fake-controller.yaml.
//
// The test:
//   1. Builds rest.Config from the kubeconfig
//   2. Starts the Resilience-1 informer for ns "krateo-system"
//   3. Asserts initial Healthy=1
//   4. kubectl scale --replicas=0 → assert 1->0 within 30s
//   5. kubectl scale --replicas=1 → assert 0->1 within 60s
//
//go:build integration
// +build integration

package cache

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

func TestHG1_GaugeTransition_KindCluster(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	kc := os.Getenv("KUBECONFIG")
	if kc == "" {
		t.Skip("KUBECONFIG not set; skipping integration synth-test")
	}
	rc, err := clientcmd.BuildConfigFromFlags("", kc)
	if err != nil {
		t.Fatalf("BuildConfigFromFlags: %v", err)
	}

	ResetControllerHealthForTest()
	defer ResetControllerHealthForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := StartControllerHealthInformer(ctx, rc, []string{"krateo-system"}); err != nil {
		t.Fatalf("StartControllerHealthInformer: %v", err)
	}

	// Wait for initial sync.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if ControllerHealthCacheServable() {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ControllerHealthCacheServable() {
		t.Fatalf("ControllerHealthCacheServable=false after 45s")
	}

	key := "krateo-system/fake-controller-resilience1"
	probe := func() (int, string, int) {
		s := ControllerHealthSnapshotLoad()
		if s == nil {
			return -1, "no-snapshot", -1
		}
		e := s.Controllers[key]
		return e.Healthy, e.Reason, e.EndpointReadyCount
	}

	h, r, ep := probe()
	t.Logf("pre-scale: Healthy=%d Reason=%q EndpointReadyCount=%d", h, r, ep)
	if h != 1 {
		t.Fatalf("pre-scale Healthy=%d; want 1", h)
	}

	// Drive scale-to-0.
	scaleDown := exec.Command("kubectl", "--context", "kind-resilience1-synth",
		"-n", "krateo-system", "scale", "deploy/fake-controller-resilience1", "--replicas=0")
	if out, err := scaleDown.CombinedOutput(); err != nil {
		t.Fatalf("scale --replicas=0: %v\n%s", err, out)
	}
	t0 := time.Now()

	var flipDown time.Duration
	observed := -1
	for time.Since(t0) < 30*time.Second {
		h, r, ep := probe()
		observed = h
		if h == 0 && r == controllerReasonNoEndpoints {
			flipDown = time.Since(t0)
			t.Logf("PASS scale-to-0: t+%s Healthy=%d Reason=%q EndpointReadyCount=%d",
				flipDown, h, r, ep)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if observed != 0 {
		t.Fatalf("scale-to-0 transition not observed within 30s; last Healthy=%d", observed)
	}

	// Restore.
	scaleUp := exec.Command("kubectl", "--context", "kind-resilience1-synth",
		"-n", "krateo-system", "scale", "deploy/fake-controller-resilience1", "--replicas=1")
	if out, err := scaleUp.CombinedOutput(); err != nil {
		t.Fatalf("scale --replicas=1: %v\n%s", err, out)
	}
	t1 := time.Now()

	var flipUp time.Duration
	observed = -1
	for time.Since(t1) < 60*time.Second {
		h, r, ep := probe()
		observed = h
		if h == 1 && r == controllerReasonHealthy {
			flipUp = time.Since(t1)
			t.Logf("PASS scale-to-1: t+%s Healthy=%d Reason=%q EndpointReadyCount=%d",
				flipUp, h, r, ep)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if observed != 1 {
		t.Fatalf("scale-to-1 transition not observed within 60s; last Healthy=%d", observed)
	}

	t.Logf("---HG-1 summary---")
	t.Logf("scale-to-0 flip latency: %s", flipDown)
	t.Logf("scale-to-1 flip latency: %s", flipUp)
}
