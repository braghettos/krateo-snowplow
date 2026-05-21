// controller_health_race_test.go — Ship Resilience-1 (0.30.162).
// AC-R1.8 race test. Concurrent scheduleControllerHealthRebuild
// (writer) + concurrent expvar reads (lock-free atomic.Pointer
// load) + concurrent Start/Reset (lifecycle).
//
// Pattern mirrors AC-D2.5 (the Secrets informer race test). Run
// with `go test -race -count=1 ./internal/cache/...`.
package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestControllerHealth_Race_RebuildVsRead(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetControllerHealthForTest()
	defer ResetControllerHealthForTest()

	ns := "krateo-system"
	dep := mkDeployment(ns, "ctrl-race")
	ep := mkEndpoints(ns, "ctrl-race", 1)
	pod := mkPod(ns, "ctrl-race", "ctrl-race-aaa", 0)
	mwc := mkMWC("cfg-race", "mutate.race", ns, "ctrl-race", "Fail")
	cli := fake.NewSimpleClientset(dep, ep, pod, mwc)
	restore := SetControllerHealthClientForTest(cli)
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := StartControllerHealthInformer(ctx, nil, []string{ns}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Writers: scheduleControllerHealthRebuild floods, plus a fake
	// pod-update loop forcing the rebuild to walk a moving target
	// (so the indexer load + the snapshot publish overlap reads).
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader 1 — expvar controller_health value.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = controllerHealthExpvarValue()
			}
		}
	}()

	// Reader 2 — expvar webhook_failurepolicy value.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = webhookFailurePolicyExpvarValue()
			}
		}
	}()

	// Reader 3 — ControllerHealthSnapshotLoad + read every field.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s := ControllerHealthSnapshotLoad()
				if s != nil {
					for _, e := range s.Controllers {
						_ = e.Healthy
						_ = e.Reason
					}
					for _, w := range s.Webhooks {
						_ = w.Policy
					}
				}
			}
		}
	}()

	// Reader 4 — ControllerHealthCacheServable.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = ControllerHealthCacheServable()
			}
		}
	}()

	// Writer 1 — scheduleControllerHealthRebuild floods.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				scheduleControllerHealthRebuild()
			}
		}
	}()

	// Writer 2 — pod-restart bumper (creates rebuilds with a
	// moving target so prevRestarts dirty-state varies).
	wg.Add(1)
	go func() {
		defer wg.Done()
		var i int32
		for {
			select {
			case <-stop:
				return
			default:
				i++
				p := mkPod(ns, "ctrl-race", "ctrl-race-aaa", i)
				_, _ = cli.CoreV1().Pods(ns).Update(context.Background(), p, metav1.UpdateOptions{})
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Drain any in-flight rebuild before returning so the deferred
	// Reset doesn't race a goroutine that hasn't exited yet.
	for i := 0; i < 50; i++ {
		if !controllerHealthRebuildLock.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}
