// Q-OOM-FIX (v0.25.313 RCA, 2026-05-08) — unit tests for the no-op
// UPDATE filter in ResourceWatcher.handleEvent (Patch C).
//
// Upstream controllers re-emit UPDATE at ~4.5/s sustained for objects
// whose etcd-tracked state did NOT change (resourceVersion equal). Each
// such event used to trigger:
//   1. A fresh L2 raw-cache write (SetForGVR).
//   2. An enqueue onto rw.eventCh → workqueue → L1 refresh.
//
// Patch C drops these events as soon as the equality is detected. Tests
// here lock the contract that the filter:
//   1. Fires for UPDATE with same resourceVersion (no enqueue).
//   2. Does NOT fire for UPDATE with different resourceVersion.
//   3. Does NOT fire for ADD or DELETE.
package cache

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// newWatcherForHandleEventTest builds a ResourceWatcher with a tiny event
// channel, the synced flag flipped on, and no Cache. handleEvent's L2
// raw-write branch is guarded by `if rw.cache != nil`, so leaving it nil
// keeps the test focused on the eventCh enqueue contract.
func newWatcherForHandleEventTest(t *testing.T) *ResourceWatcher {
	t.Helper()
	rw := &ResourceWatcher{
		watched:     make(map[string]bool),
		dynamicGVRs: make(map[string]schema.GroupVersionResource),
		eventCh:     make(chan l1Event, 4),
	}
	rw.synced.Store(true)
	return rw
}

// makeUnstructured builds a minimal *unstructured.Unstructured at the
// given (gvr, ns/name, rv).
func makeUnstructured(gvr schema.GroupVersionResource, ns, name, rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(gvr.GroupVersion().String())
	u.SetKind("X")
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetResourceVersion(rv)
	return u
}

func drainEventCh(ch chan l1Event) []l1Event {
	out := []l1Event{}
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestHandleEvent_NoopResourceVersionFiltered — Patch C primary contract.
// UPDATE event whose old/new resourceVersion match must be dropped: no
// l1Event reaches eventCh, and the WatchEventsNoopFiltered counter ticks.
func TestHandleEvent_NoopResourceVersionFiltered(t *testing.T) {
	rw := newWatcherForHandleEventTest(t)
	gvr := schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1", Resource: "compositions"}

	old := makeUnstructured(gvr, "ns1", "comp-a", "12345")
	new := makeUnstructured(gvr, "ns1", "comp-a", "12345")

	noopBefore := GlobalMetrics.WatchEventsNoopFiltered.Load()
	updateBefore := GlobalMetrics.WatchEventsUpdate.Load()

	rw.handleEvent(context.Background(), gvr, old, new, "update")

	if got := GlobalMetrics.WatchEventsNoopFiltered.Load(); got != noopBefore+1 {
		t.Fatalf("WatchEventsNoopFiltered: got delta %d, want 1", got-noopBefore)
	}
	if got := GlobalMetrics.WatchEventsUpdate.Load(); got != updateBefore+1 {
		t.Fatalf("WatchEventsUpdate: got delta %d, want 1 (counter still ticks; only the work is skipped)", got-updateBefore)
	}
	events := drainEventCh(rw.eventCh)
	if len(events) != 0 {
		t.Fatalf("expected NO l1 enqueue on no-op UPDATE, got %d events: %+v", len(events), events)
	}
}

// TestHandleEvent_DifferentResourceVersionPasses — guard against false
// positives. UPDATE with distinct resourceVersions must enqueue normally.
func TestHandleEvent_DifferentResourceVersionPasses(t *testing.T) {
	rw := newWatcherForHandleEventTest(t)
	gvr := schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1", Resource: "compositions"}

	old := makeUnstructured(gvr, "ns1", "comp-b", "100")
	new := makeUnstructured(gvr, "ns1", "comp-b", "101")

	noopBefore := GlobalMetrics.WatchEventsNoopFiltered.Load()
	rw.handleEvent(context.Background(), gvr, old, new, "update")

	if got := GlobalMetrics.WatchEventsNoopFiltered.Load(); got != noopBefore {
		t.Fatalf("WatchEventsNoopFiltered must NOT increment for genuine UPDATE; got delta %d", got-noopBefore)
	}
	events := drainEventCh(rw.eventCh)
	if len(events) != 1 {
		t.Fatalf("expected 1 l1 enqueue on real UPDATE, got %d", len(events))
	}
	if events[0].eventType != "update" {
		t.Fatalf("enqueue eventType = %q, want update", events[0].eventType)
	}
}

// TestHandleEvent_AddNotFiltered — ADD passes oldObj=nil, so the type
// assertion to *unstructured.Unstructured fails and the filter is a
// no-op. This test pins that contract so an inadvertent change to the
// filter (e.g. moving it before the eventType check) doesn't silently
// drop ADD events.
func TestHandleEvent_AddNotFiltered(t *testing.T) {
	rw := newWatcherForHandleEventTest(t)
	gvr := schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1", Resource: "compositions"}

	new := makeUnstructured(gvr, "ns1", "comp-c", "200")

	noopBefore := GlobalMetrics.WatchEventsNoopFiltered.Load()
	rw.handleEvent(context.Background(), gvr, nil, new, "add")

	if got := GlobalMetrics.WatchEventsNoopFiltered.Load(); got != noopBefore {
		t.Fatalf("ADD must not be filtered; noop counter delta = %d", got-noopBefore)
	}
	events := drainEventCh(rw.eventCh)
	if len(events) != 1 || events[0].eventType != "add" {
		t.Fatalf("ADD: expected 1 enqueue with type=add, got %+v", events)
	}
}

// TestHandleEvent_DeleteNotFiltered — DELETE also passes oldObj=nil and
// must continue reaching the eventCh.
func TestHandleEvent_DeleteNotFiltered(t *testing.T) {
	rw := newWatcherForHandleEventTest(t)
	gvr := schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1", Resource: "compositions"}

	obj := makeUnstructured(gvr, "ns1", "comp-d", "300")

	noopBefore := GlobalMetrics.WatchEventsNoopFiltered.Load()
	rw.handleEvent(context.Background(), gvr, nil, obj, "delete")

	if got := GlobalMetrics.WatchEventsNoopFiltered.Load(); got != noopBefore {
		t.Fatalf("DELETE must not be filtered; noop counter delta = %d", got-noopBefore)
	}
	events := drainEventCh(rw.eventCh)
	if len(events) != 1 || events[0].eventType != "delete" {
		t.Fatalf("DELETE: expected 1 enqueue with type=delete, got %+v", events)
	}
}
