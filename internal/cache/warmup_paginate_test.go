package cache

import (
	"context"
	"fmt"
	goruntime "runtime"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// pagedListClient is a minimal dynamic.Interface that delegates List to a
// caller-supplied function and panics on every other method. We can't use
// dynamicfake here because its List path runs the result through
// scheme.Convert(*UnstructuredList → *Unstructured) + ToList, which strips
// the list-level metadata.continue token — making it impossible to assert
// pagination behavior.
type pagedListClient struct {
	listFn func(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
}

func (c *pagedListClient) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &pagedListResource{listFn: c.listFn}
}

type pagedListResource struct {
	listFn func(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
}

func (r *pagedListResource) Namespace(string) dynamic.ResourceInterface { return r }
func (r *pagedListResource) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return r.listFn(ctx, opts)
}
func (r *pagedListResource) Create(context.Context, *unstructured.Unstructured, metav1.CreateOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not used")
}
func (r *pagedListResource) Update(context.Context, *unstructured.Unstructured, metav1.UpdateOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not used")
}
func (r *pagedListResource) UpdateStatus(context.Context, *unstructured.Unstructured, metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	panic("not used")
}
func (r *pagedListResource) Delete(context.Context, string, metav1.DeleteOptions, ...string) error {
	panic("not used")
}
func (r *pagedListResource) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	panic("not used")
}
func (r *pagedListResource) Get(context.Context, string, metav1.GetOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not used")
}
func (r *pagedListResource) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	panic("not used")
}
func (r *pagedListResource) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not used")
}
func (r *pagedListResource) Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error) {
	panic("not used")
}
func (r *pagedListResource) ApplyStatus(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	panic("not used")
}

// TestWarmGVR_PaginatedStreaming pins the streaming-LIST contract added by
// Q-OOM-WARMER (0.25.320): warmGVR must paginate via Limit/Continue tokens
// and must NOT accumulate items across pages. We assert (a) every page is
// served via a separate List call, (b) the per-call Limit equals
// warmGVRPageSize, (c) warmGVR drives the loop until the server emits an
// empty Continue token, (d) per-call Continue equals the prior page's token.
func TestWarmGVR_PaginatedStreaming(t *testing.T) {
	const totalObjects = int(warmGVRPageSize)*2 + 137 // 1137: pages of 500 / 500 / 137
	gvr := schema.GroupVersionResource{Group: "test.krateo.io", Version: "v1", Resource: "widgets"}

	var (
		listCalls  atomic.Int64
		seenLimits []int64
		seenConts  []string
	)
	listFn := func(_ context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		listCalls.Add(1)
		seenLimits = append(seenLimits, opts.Limit)
		seenConts = append(seenConts, opts.Continue)

		var start int
		fmt.Sscanf(opts.Continue, "%d", &start)
		end := start + int(opts.Limit)
		if end > totalObjects {
			end = totalObjects
		}
		out := &unstructured.UnstructuredList{}
		out.SetAPIVersion("test.krateo.io/v1")
		out.SetKind("WidgetList")
		for i := start; i < end; i++ {
			obj := unstructured.Unstructured{}
			obj.SetAPIVersion("test.krateo.io/v1")
			obj.SetKind("Widget")
			obj.SetName(fmt.Sprintf("w-%04d", i))
			obj.SetNamespace(fmt.Sprintf("ns-%d", i%5))
			out.Items = append(out.Items, obj)
		}
		if end < totalObjects {
			out.SetContinue(fmt.Sprintf("%d", end))
		} else {
			out.SetContinue("")
		}
		return out, nil
	}

	mc := NewMem(0)
	w := NewWarmer(mc, nil)
	w.warmGVR(context.Background(), &pagedListClient{listFn: listFn}, gvr)

	if got, want := listCalls.Load(), int64(3); got != want {
		t.Fatalf("listCalls = %d, want %d (1137 items / pageSize=500)", got, want)
	}
	for i, l := range seenLimits {
		if l != warmGVRPageSize {
			t.Errorf("call[%d] Limit = %d, want %d (warmGVRPageSize)", i, l, warmGVRPageSize)
		}
	}
	wantConts := []string{"", "500", "1000"}
	for i, c := range seenConts {
		if c != wantConts[i] {
			t.Errorf("call[%d] Continue = %q, want %q", i, c, wantConts[i])
		}
	}
}

// TestWarmGVR_StopsOnEmptyContinue pins the loop-termination contract: the
// Warmer must stop the moment the server returns an empty Continue token.
// Without this, a slow drift to "continue token never empties" would loop
// forever and silently re-list the same page.
func TestWarmGVR_StopsOnEmptyContinue(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "test.krateo.io", Version: "v1", Resource: "stops"}
	listGVK := schema.GroupVersionKind{Group: "test.krateo.io", Version: "v1", Kind: "StopList"}

	scheme := apiruntime.NewScheme()
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{gvr: "StopList"},
	)

	var calls atomic.Int64
	fc.PrependReactor("list", gvr.Resource, func(action clienttesting.Action) (bool, apiruntime.Object, error) {
		calls.Add(1)
		out := &unstructured.UnstructuredList{}
		out.SetGroupVersionKind(listGVK)
		// Single tiny page, immediately empty Continue.
		obj := unstructured.Unstructured{}
		obj.SetAPIVersion("test.krateo.io/v1")
		obj.SetKind("Stop")
		obj.SetName("only")
		obj.SetNamespace("ns")
		out.Items = append(out.Items, obj)
		out.SetContinue("")
		return true, out, nil
	})

	mc := NewMem(0)
	w := NewWarmer(mc, nil)
	w.warmGVR(context.Background(), fc, gvr)

	if got := calls.Load(); got != 1 {
		t.Fatalf("listCalls = %d, want 1 (must terminate on empty continue)", got)
	}
}

// TestWarmGVR_ListErrorAborts pins the abort-on-error contract. A failed
// page must NOT cause the loop to spin or repeat — warmGVR returns and lets
// the next reconcile retry. (Pre-fix this was a single failed call; the
// post-fix loop must preserve that semantic.)
func TestWarmGVR_ListErrorAborts(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "test.krateo.io", Version: "v1", Resource: "errs"}
	listGVK := schema.GroupVersionKind{Group: "test.krateo.io", Version: "v1", Kind: "ErrList"}

	scheme := apiruntime.NewScheme()
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{gvr: "ErrList"},
	)

	var calls atomic.Int64
	fc.PrependReactor("list", gvr.Resource, func(action clienttesting.Action) (bool, apiruntime.Object, error) {
		calls.Add(1)
		return true, nil, fmt.Errorf("synthetic LIST failure")
	})

	mc := NewMem(0)
	w := NewWarmer(mc, nil)
	w.warmGVR(context.Background(), fc, gvr)

	if got := calls.Load(); got != 1 {
		t.Fatalf("listCalls = %d, want 1 (must abort, not retry)", got)
	}
}

// TestWarmFanout pins the env-knob contract for the Warmer fan-in cap added
// by Q-OOM-WARMER (0.25.320). Default is 2 (capped to GOMAXPROCS for tiny
// pods); custom values are clamped to [1, GOMAXPROCS]; malformed env falls
// back to default. The clamp is what guarantees misconfiguration cannot
// regress past the pre-fix ceiling.
func TestWarmFanout(t *testing.T) {
	max := goruntime.GOMAXPROCS(0)
	if max < 2 {
		t.Skipf("GOMAXPROCS=%d too small to exercise the cap", max)
	}

	type tc struct {
		name string
		env  string
		set  bool
		want int
	}
	cases := []tc{
		{name: "unset_uses_default_2", set: false, want: min2(max)},
		{name: "explicit_1", env: "1", set: true, want: 1},
		{name: "explicit_2", env: "2", set: true, want: min2(max)},
		{name: "above_max_clamps_to_max", env: fmt.Sprintf("%d", max+10), set: true, want: max},
		{name: "negative_falls_back_to_default", env: "-3", set: true, want: min2(max)},
		{name: "zero_falls_back_to_default", env: "0", set: true, want: min2(max)},
		{name: "garbage_falls_back_to_default", env: "abc", set: true, want: min2(max)},
		{name: "whitespace_trimmed", env: "  3  ", set: true, want: min3(max)},
		{name: "empty_string_uses_default", env: "", set: true, want: min2(max)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// t.Setenv("KEY","") still leaves the var "set" for os.LookupEnv
			// purposes, but warmFanout treats empty string as "use default"
			// so this is the same code path as truly unset. Either way the
			// expected result is the default.
			t.Setenv("SNOWPLOW_WARM_FANOUT", c.env)
			if got := warmFanout(); got != c.want {
				t.Errorf("warmFanout() = %d, want %d (env=%q set=%v max=%d)",
					got, c.want, c.env, c.set, max)
			}
		})
	}
}

func min2(max int) int {
	if 2 > max {
		return max
	}
	return 2
}

func min3(max int) int {
	if 3 > max {
		return max
	}
	return 3
}
