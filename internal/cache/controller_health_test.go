// controller_health_test.go — Ship Resilience-1 (0.30.162). Unit tests
// covering the AC matrix:
//
//   AC-R1.1 — controller_health gauge shape + 5-value Reason enum
//   AC-R1.2 — webhook_failurepolicy info-metric
//   AC-R1.4 — CACHE_ENABLED=false → subsystem inert
//   AC-R1.6 — synth-test gauge transition (covered structurally;
//             kind-cluster end-to-end is in the integration test)
//   AC-R1.7 — zero apiserver writes (structurally: every test uses
//             the fake clientset; any Create/Update/Patch/Delete
//             would be visible in the fake's action log)
//   AC-R1.8 — race-test (separate file)
package cache

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// resetControllerHealthForTest clears every package-level state plus
// the test-injection client.
func resetControllerHealthForTest(t *testing.T) {
	t.Helper()
	ResetControllerHealthForTest()
}

// mkDeployment builds a typed *appsv1.Deployment with the canonical
// selector convention (matchLabels{app: name}). Pods produced by
// helper mkPod below carry the same label so the Pod LIST in
// podRestartCounts matches.
func mkDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
		},
	}
}

// mkEndpoints builds an Endpoints with `ready` ready addresses
// across a single subset.
func mkEndpoints(ns, name string, ready int) *corev1.Endpoints {
	subsets := []corev1.EndpointSubset{}
	if ready > 0 {
		addrs := make([]corev1.EndpointAddress, ready)
		for i := range addrs {
			addrs[i] = corev1.EndpointAddress{IP: "10.0.0.1"}
		}
		subsets = append(subsets, corev1.EndpointSubset{Addresses: addrs})
	}
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Subsets:    subsets,
	}
}

// mkPod builds a pod owned by the Deployment named `dep` with the
// canonical app=name label and one container with the given
// RestartCount.
func mkPod(ns, dep, podName string, restartCount int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      podName,
			Labels:    map[string]string{"app": dep},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "sentinel", RestartCount: restartCount},
			},
		},
	}
}

// mkMWC builds a MutatingWebhookConfiguration with one entry
// pointing at svc (ns, name). policy is "Fail" / "Ignore".
func mkMWC(cfgName, whName, svcNS, svcName, policy string) *admissionv1.MutatingWebhookConfiguration {
	fp := admissionv1.FailurePolicyType(policy)
	sideEffects := admissionv1.SideEffectClassNone
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: cfgName},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name:                    whName,
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			FailurePolicy:           &fp,
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{
					Namespace: svcNS,
					Name:      svcName,
				},
			},
		}},
	}
}

// mkVWC builds a ValidatingWebhookConfiguration shape sibling of
// mkMWC.
func mkVWC(cfgName, whName, svcNS, svcName, policy string) *admissionv1.ValidatingWebhookConfiguration {
	fp := admissionv1.FailurePolicyType(policy)
	sideEffects := admissionv1.SideEffectClassNone
	return &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: cfgName},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    whName,
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			FailurePolicy:           &fp,
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{
					Namespace: svcNS,
					Name:      svcName,
				},
			},
		}},
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.4 — CACHE_ENABLED=false → subsystem inert
// ─────────────────────────────────────────────────────────────────────

func TestStartControllerHealthInformer_CacheDisabled_NoOp(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	resetControllerHealthForTest(t)

	preG := runtime.NumGoroutine()

	// Provide a fake client so any reach into kubernetes.NewForConfig
	// would be caught (defense-in-depth). The Disabled() gate at the
	// top of Start should fire BEFORE buildControllerHealthClient is
	// reached, so this fake should never be touched.
	cli := fake.NewSimpleClientset()
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := StartControllerHealthInformer(ctx, nil, []string{"krateo-system"}); err != nil {
		t.Fatalf("StartControllerHealthInformer: %v", err)
	}

	// AC-R1.4 (a): no goroutine spawned. Allow a small tolerance for
	// goroutines spawned by the test framework itself between the
	// two NumGoroutine reads (none should be spawned by Start).
	if delta := runtime.NumGoroutine() - preG; delta > 0 {
		t.Errorf("runtime.NumGoroutine delta=%d after Start in cache=off mode; want 0", delta)
	}

	// AC-R1.4 (b): no apiserver client constructed. The fake's
	// action log should be empty.
	if acts := cli.Actions(); len(acts) > 0 {
		t.Errorf("fake clientset Actions() len=%d in cache=off mode; want 0; actions=%+v", len(acts), acts)
	}

	// AC-R1.4 (c): expvar closures return empty maps.
	if got := controllerHealthExpvarValue(); !isEmptyControllerMap(got) {
		t.Errorf("controllerHealthExpvarValue=%+v in cache=off mode; want empty map", got)
	}
	if got := webhookFailurePolicyExpvarValue(); !isEmptyWebhookMap(got) {
		t.Errorf("webhookFailurePolicyExpvarValue=%+v in cache=off mode; want empty map", got)
	}

	// AC-R1.4: subsystem reports unservable.
	if ControllerHealthCacheServable() {
		t.Errorf("ControllerHealthCacheServable=true in cache=off mode; want false")
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.5 — empty namespaces → per-ns watches inert, cluster watches run
// ─────────────────────────────────────────────────────────────────────

func TestStartControllerHealthInformer_EmptyNamespaces_WebhooksStillPublished(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	// MWC referencing a service in some namespace — even with empty
	// CONTROLLER_HEALTH_NAMESPACES the webhook config itself is
	// cluster-scoped so the gauge entry MUST appear.
	mwc := mkMWC("cfg-empty", "mutate.test", "krateo-system", "ctrl-x", "Fail")
	cli := fake.NewSimpleClientset(mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{}); err != nil {
		t.Fatalf("StartControllerHealthInformer: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	// Synchronously rebuild so the test does not race the event
	// handler.
	RebuildControllerHealthSnapshotForTest()

	snap := ControllerHealthSnapshotLoad()
	if snap == nil {
		t.Fatalf("ControllerHealthSnapshotLoad: got nil; want a snapshot")
	}
	if len(snap.Webhooks) != 1 {
		t.Errorf("snap.Webhooks len=%d; want 1; snap=%+v", len(snap.Webhooks), snap)
	}
	if len(snap.Controllers) != 0 {
		t.Errorf("snap.Controllers len=%d in empty-ns mode; want 0", len(snap.Controllers))
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.1 — controller_health gauge happy-path (Healthy=1)
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_HealthyState(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	dep := mkDeployment(ns, "ctrl-healthy")
	ep := mkEndpoints(ns, "ctrl-healthy", 1)
	pod := mkPod(ns, "ctrl-healthy", "ctrl-healthy-abc", 0)
	mwc := mkMWC("cfg-ok", "mutate.ok", ns, "ctrl-healthy", "Fail")

	cli := fake.NewSimpleClientset(dep, ep, pod, mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("StartControllerHealthInformer: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	RebuildControllerHealthSnapshotForTest()
	snap := ControllerHealthSnapshotLoad()
	if snap == nil {
		t.Fatalf("nil snapshot")
	}

	key := ns + "/ctrl-healthy"
	e, ok := snap.Controllers[key]
	if !ok {
		t.Fatalf("controllers map missing key %q; got %+v", key, snap.Controllers)
	}
	if e.Healthy != 1 {
		t.Errorf("Healthy=%d; want 1", e.Healthy)
	}
	if e.Reason != controllerReasonHealthy {
		t.Errorf("Reason=%q; want %q", e.Reason, controllerReasonHealthy)
	}
	if e.EndpointReadyCount != 1 {
		t.Errorf("EndpointReadyCount=%d; want 1", e.EndpointReadyCount)
	}
	if e.Name != "ctrl-healthy" {
		t.Errorf("Name=%q; want %q", e.Name, "ctrl-healthy")
	}
	if e.Namespace != ns {
		t.Errorf("Namespace=%q; want %q", e.Namespace, ns)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.1 — Conjunct A (pod restart) fires
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_PodRestartFires(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	dep := mkDeployment(ns, "ctrl-restart")
	ep := mkEndpoints(ns, "ctrl-restart", 1) // endpoints healthy
	pod := mkPod(ns, "ctrl-restart", "ctrl-restart-aaa", 0)
	mwc := mkMWC("cfg-r", "mutate.r", ns, "ctrl-restart", "Fail")
	cli := fake.NewSimpleClientset(dep, ep, pod, mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	// First rebuild establishes baseline restart-count = 0.
	RebuildControllerHealthSnapshotForTest()
	key := ns + "/ctrl-restart"
	if e := ControllerHealthSnapshotLoad().Controllers[key]; e.Healthy != 1 || e.Reason != controllerReasonHealthy {
		t.Fatalf("baseline rebuild: Healthy=%d Reason=%q; want 1 healthy", e.Healthy, e.Reason)
	}

	// Advance the pod's restart count from 0 → 1 and rebuild.
	// fake.NewSimpleClientset's tracker stores the pod by name;
	// Update replaces it.
	pod2 := mkPod(ns, "ctrl-restart", "ctrl-restart-aaa", 1)
	if _, err := cli.CoreV1().Pods(ns).Update(context.Background(), pod2, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod restart: %v", err)
	}
	RebuildControllerHealthSnapshotForTest()

	e := ControllerHealthSnapshotLoad().Controllers[key]
	if e.Healthy != 0 {
		t.Errorf("Healthy=%d; want 0 after pod restart", e.Healthy)
	}
	if e.Reason != controllerReasonPodRestart {
		t.Errorf("Reason=%q; want %q after pod restart", e.Reason, controllerReasonPodRestart)
	}
	if e.PodRestartCount != 1 {
		t.Errorf("PodRestartCount=%d; want 1", e.PodRestartCount)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.1 — Conjunct B (endpoints zero) fires
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_EndpointsEmptyFires(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	dep := mkDeployment(ns, "ctrl-noep")
	ep := mkEndpoints(ns, "ctrl-noep", 0) // empty subsets
	mwc := mkMWC("cfg-noep", "mutate.noep", ns, "ctrl-noep", "Fail")
	cli := fake.NewSimpleClientset(dep, ep, mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	RebuildControllerHealthSnapshotForTest()
	key := ns + "/ctrl-noep"
	e := ControllerHealthSnapshotLoad().Controllers[key]
	if e.Healthy != 0 {
		t.Errorf("Healthy=%d; want 0", e.Healthy)
	}
	if e.Reason != controllerReasonNoEndpoints {
		t.Errorf("Reason=%q; want %q", e.Reason, controllerReasonNoEndpoints)
	}
	if e.EndpointReadyCount != 0 {
		t.Errorf("EndpointReadyCount=%d; want 0", e.EndpointReadyCount)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.1 — both conjuncts fire → Reason="both"
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_BothConjunctsFire(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	dep := mkDeployment(ns, "ctrl-both")
	ep := mkEndpoints(ns, "ctrl-both", 0)
	pod := mkPod(ns, "ctrl-both", "ctrl-both-aaa", 0)
	mwc := mkMWC("cfg-b", "mutate.b", ns, "ctrl-both", "Fail")
	cli := fake.NewSimpleClientset(dep, ep, pod, mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	// Baseline rebuild → Reason="endpoints-zero-ready" (Conjunct B
	// only — A has not yet established baseline restart count).
	RebuildControllerHealthSnapshotForTest()

	// Bump pod restart count.
	pod2 := mkPod(ns, "ctrl-both", "ctrl-both-aaa", 1)
	if _, err := cli.CoreV1().Pods(ns).Update(context.Background(), pod2, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod restart: %v", err)
	}

	// Second rebuild — both conjuncts now fire.
	RebuildControllerHealthSnapshotForTest()

	key := ns + "/ctrl-both"
	e := ControllerHealthSnapshotLoad().Controllers[key]
	if e.Healthy != 0 {
		t.Errorf("Healthy=%d; want 0", e.Healthy)
	}
	if e.Reason != controllerReasonBoth {
		t.Errorf("Reason=%q; want %q", e.Reason, controllerReasonBoth)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.1 — discovery via webhook config (no static list)
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_DiscoveryViaWebhook(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	// Controller A exists; controller B is referenced by a webhook
	// config but the Deployment doesn't exist — Resilience-1 should
	// surface A as healthy and B as "unwired".
	depA := mkDeployment(ns, "ctrl-a")
	epA := mkEndpoints(ns, "ctrl-a", 1)
	podA := mkPod(ns, "ctrl-a", "ctrl-a-aaa", 0)
	mwcA := mkMWC("cfg-a", "mutate.a", ns, "ctrl-a", "Fail")
	mwcB := mkMWC("cfg-b", "mutate.b", ns, "ctrl-missing", "Ignore")
	cli := fake.NewSimpleClientset(depA, epA, podA, mwcA, mwcB)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	RebuildControllerHealthSnapshotForTest()
	snap := ControllerHealthSnapshotLoad()

	aKey := ns + "/ctrl-a"
	bKey := ns + "/ctrl-missing"

	if e, ok := snap.Controllers[aKey]; !ok || e.Healthy != 1 {
		t.Errorf("ctrl-a: ok=%v Healthy=%d; want true,1", ok, e.Healthy)
	}
	e, ok := snap.Controllers[bKey]
	if !ok {
		t.Fatalf("ctrl-missing: not in snapshot; want unwired entry")
	}
	if e.Healthy != 0 || e.Reason != controllerReasonUnwired {
		t.Errorf("ctrl-missing: Healthy=%d Reason=%q; want 0,%q", e.Healthy, e.Reason, controllerReasonUnwired)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.2 — webhook_failurepolicy gauge
// ─────────────────────────────────────────────────────────────────────

func TestWebhookFailurePolicyGauge(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	mwc1 := mkMWC("cfg-fail", "mutate.fail", ns, "svc", "Fail")
	mwc2 := mkMWC("cfg-ignore", "mutate.ignore1", ns, "svc", "Ignore")
	vwc := mkVWC("cfg-val", "validate.ig", ns, "svc", "Ignore")

	cli := fake.NewSimpleClientset(mwc1, mwc2, vwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	RebuildControllerHealthSnapshotForTest()
	snap := ControllerHealthSnapshotLoad()

	want := map[string]WebhookFailurePolicyEntry{
		"mutate.fail":    {Policy: "Fail", Configuration: "cfg-fail", Type: "Mutating"},
		"mutate.ignore1": {Policy: "Ignore", Configuration: "cfg-ignore", Type: "Mutating"},
		"validate.ig":    {Policy: "Ignore", Configuration: "cfg-val", Type: "Validating"},
	}
	if len(snap.Webhooks) != len(want) {
		t.Fatalf("snap.Webhooks len=%d; want %d; got=%+v", len(snap.Webhooks), len(want), snap.Webhooks)
	}
	for k, w := range want {
		got, ok := snap.Webhooks[k]
		if !ok {
			t.Errorf("snap.Webhooks missing %q; got=%+v", k, snap.Webhooks)
			continue
		}
		if got != w {
			t.Errorf("snap.Webhooks[%q]=%+v; want %+v", k, got, w)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.6 — synth-test gauge transition 1→0 via Endpoints scale-to-0
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_SynthTransition_HealthyToEndpointsZero(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	dep := mkDeployment(ns, "fake-controller-resilience1")
	ep := mkEndpoints(ns, "fake-controller-resilience1", 1)
	pod := mkPod(ns, "fake-controller-resilience1", "fake-controller-resilience1-aaa", 0)
	mwc := mkMWC("fake-controller-resilience1", "mutate.fake-controller-resilience1.krateo.io",
		ns, "fake-controller-resilience1", "Fail")

	cli := fake.NewSimpleClientset(dep, ep, pod, mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	RebuildControllerHealthSnapshotForTest()
	key := ns + "/fake-controller-resilience1"
	if e := ControllerHealthSnapshotLoad().Controllers[key]; e.Healthy != 1 {
		t.Fatalf("pre-transition: Healthy=%d; want 1", e.Healthy)
	}

	// Drive scale-to-0: empty out Endpoints subsets (this is what
	// the Service endpoint controller does on Deployment scale=0).
	ep0 := mkEndpoints(ns, "fake-controller-resilience1", 0)
	if _, err := cli.CoreV1().Endpoints(ns).Update(context.Background(), ep0, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update endpoints scale-to-0: %v", err)
	}

	// Poll for transition within the AC-R1.6 budget (30s; in unit
	// test we expect ~immediate via the event handler).
	deadline := time.Now().Add(3 * time.Second)
	var observedHealthy int
	var observedReason string
	for time.Now().Before(deadline) {
		// In the real binary, the informer event handler schedules
		// the rebuild; in the unit test we drive it synchronously
		// to keep the test cheap & deterministic.
		RebuildControllerHealthSnapshotForTest()
		e := ControllerHealthSnapshotLoad().Controllers[key]
		observedHealthy = e.Healthy
		observedReason = e.Reason
		if e.Healthy == 0 && e.Reason == controllerReasonNoEndpoints {
			return // PASS
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("transition not observed: last Healthy=%d Reason=%q; want 0/%q",
		observedHealthy, observedReason, controllerReasonNoEndpoints)
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.7 — zero apiserver writes (structural: scan action log)
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_NoApiserverWrites(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	dep := mkDeployment(ns, "ctrl-w")
	ep := mkEndpoints(ns, "ctrl-w", 1)
	pod := mkPod(ns, "ctrl-w", "ctrl-w-aaa", 0)
	mwc := mkMWC("cfg-w", "mutate.w", ns, "ctrl-w", "Fail")
	cli := fake.NewSimpleClientset(dep, ep, pod, mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	RebuildControllerHealthSnapshotForTest()
	// One extra rebuild to ensure repeated rebuilds do not introduce
	// writes either.
	RebuildControllerHealthSnapshotForTest()

	for _, a := range cli.Actions() {
		v := a.GetVerb()
		switch v {
		case "create", "update", "patch", "delete", "deletecollection":
			t.Errorf("controller_health subsystem issued write verb %q on resource %q; want zero writes (HG-2)",
				v, a.GetResource().Resource)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.1 — multi-namespace scope honors inScope filter
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_OutOfScopeNamespaceIgnored(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	nsIn := "krateo-system"
	nsOut := "other-ns"
	// Webhook in OUT-OF-SCOPE namespace. Should NOT create a
	// controller-health entry (the per-ns factory isn't watching
	// other-ns). The webhook entry itself still appears in the
	// webhooks gauge because MWC is cluster-scoped.
	mwc := mkMWC("cfg-oos", "mutate.oos", nsOut, "ctrl-oos", "Fail")
	cli := fake.NewSimpleClientset(mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{nsIn}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	RebuildControllerHealthSnapshotForTest()
	snap := ControllerHealthSnapshotLoad()
	if len(snap.Controllers) != 0 {
		t.Errorf("snap.Controllers len=%d; want 0 (webhook ns out-of-scope); got=%+v",
			len(snap.Controllers), snap.Controllers)
	}
	if _, ok := snap.Webhooks["mutate.oos"]; !ok {
		t.Errorf("snap.Webhooks missing webhook from out-of-scope ns; cluster-scoped watch must still record it")
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.1 — idempotent re-call
// ─────────────────────────────────────────────────────────────────────

func TestStartControllerHealthInformer_Idempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	cli := fake.NewSimpleClientset()
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{"krateo-system"}); err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	// 2nd call MUST be a no-op (idempotent contract).
	preG := runtime.NumGoroutine()
	if err := StartControllerHealthInformer(ctx, nil, []string{"krateo-system"}); err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if delta := runtime.NumGoroutine() - preG; delta > 1 {
		// Allow ±1 for noise.
		t.Errorf("idempotent re-call spawned goroutines (delta=%d); want 0", delta)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-R1.4 — Disabled() acts as a hard gate even after a successful
// pre-flag Start (operator flips at runtime).
// ─────────────────────────────────────────────────────────────────────

func TestControllerHealth_DisabledHidesSnapshot(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	resetControllerHealthForTest(t)

	ns := "krateo-system"
	dep := mkDeployment(ns, "ctrl-flip")
	ep := mkEndpoints(ns, "ctrl-flip", 1)
	pod := mkPod(ns, "ctrl-flip", "ctrl-flip-aaa", 0)
	mwc := mkMWC("cfg-flip", "mutate.flip", ns, "ctrl-flip", "Fail")
	cli := fake.NewSimpleClientset(dep, ep, pod, mwc)
	restore := SetControllerHealthClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { resetControllerHealthForTest(t) })

	RebuildControllerHealthSnapshotForTest()
	if isEmptyControllerMap(controllerHealthExpvarValue()) {
		t.Fatalf("pre-flip: expvar value is empty; want populated")
	}

	// Flip CACHE_ENABLED → false at runtime. The expvar closure
	// MUST publish an empty map (defense in depth — even if the
	// snapshot is still in memory, Disabled() shadows it).
	t.Setenv("CACHE_ENABLED", "false")
	if !isEmptyControllerMap(controllerHealthExpvarValue()) {
		t.Errorf("controllerHealthExpvarValue not empty after CACHE_ENABLED=false flip")
	}
	if !isEmptyWebhookMap(webhookFailurePolicyExpvarValue()) {
		t.Errorf("webhookFailurePolicyExpvarValue not empty after CACHE_ENABLED=false flip")
	}
}

// ─────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────

func isEmptyControllerMap(v any) bool {
	m, ok := v.(map[string]ControllerHealthEntry)
	return ok && len(m) == 0
}
func isEmptyWebhookMap(v any) bool {
	m, ok := v.(map[string]WebhookFailurePolicyEntry)
	return ok && len(m) == 0
}

// Silence the unused import linter when the test runs without
// reaching for the action prefix (the testing.Action interface
// satisfies our action-log scan in TestControllerHealth_NoApiserverWrites).
var _ = clienttesting.Action(nil)

// Compile-time assertion that the fake clientset satisfies the
// kubernetes.Interface (defense-in-depth: if k8s.io/client-go's
// fake package changes shape, this fails the build instead of a
// surprise at runtime).
var _ kubernetes.Interface = (*fake.Clientset)(nil)

// Silence "imported but unused" warnings for tests that may not
// always exercise the atomic.Uint64 type alias path.
var _ = atomic.Uint64{}
