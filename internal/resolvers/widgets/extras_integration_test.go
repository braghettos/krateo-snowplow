//go:build integration
// +build integration

package widgets

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/e2e"
	"github.com/krateoplatformops/snowplow/apis"
	v1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/objects"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestResolveWidgets_Extras is the END-TO-END (kind) proof of the
// extras→widgets parity feature. It drives the REAL widgets.Resolve with an
// Extras map and asserts the resolved status reflects the extras-supplied
// values across all three paths the feature covers:
//
//   - apiRef-fetch parity (step 1): a widget whose apiRef RESTAction `path`
//     references an extras key resolves the right object (button-extras-apiref).
//   - widgetDataTemplate access (step 2): a widgetDataTemplate referencing an
//     extras key yields the expected status.widgetData (button-extras-static).
//   - resourcesRefsTemplate access (step 2, Diego's case): a
//     resourcesRefsTemplate referencing an extras key yields the expected
//     status.resourcesRefs (button-extras-static).
//
// It reuses the package TestMain (widgets_test.go) cluster/CRD/RBAC setup —
// the buttons + restactions CRDs and rbac.namespaces.yaml (devs → cluster-wide
// namespaces get/list) are already installed there.
func TestResolveWidgets_Extras(t *testing.T) {
	const jwtSignKey = "abbracadabbra"

	f := features.New("ExtrasParity").
		Setup(e2e.Logger("test")).
		Setup(e2e.SignUp(e2e.SignUpOptions{
			Username:   "cyberjoker",
			Groups:     []string{"devs"},
			Namespace:  namespace,
			JWTSignKey: jwtSignKey,
		})).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				t.Fatal(err)
			}
			apis.AddToScheme(r.GetScheme())
			r.WithNamespace(namespace)

			// Decode the extras fixtures (widget CRs + the apiRef RESTAction).
			if err := decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(testdataPath, "widgets")), "button.extras.*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
				decoder.MutateNamespace(namespace),
			); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		// Step 2 over BOTH template paths (apiRef-less static widget).
		Assess("static widget: extras in widgetDataTemplate AND resourcesRefsTemplate",
			assertStaticExtras).
		// Step 1: apiRef RESTAction path references an extras key.
		Assess("apiRef widget: extras parametrises the RESTAction fetch path",
			assertApiRefExtras).
		Feature()

	testenv.Test(t, f)
}

// assertStaticExtras resolves button-extras-static with
// extras={tenant, region, names:[...]} and asserts the extras-derived values
// landed in status.widgetData (label/icon) and status.resourcesRefs (one ref
// per extras `names` element, each namespaced by the element).
func assertStaticExtras(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	extras := map[string]any{
		"tenant": "acme-corp",
		"region": "fa-eu",
		"names":  []any{"alpha", "beta"},
	}
	obj := resolveExtrasWidget(ctx, t, c, "button-extras-static", extras)

	// status.widgetData.label == extras.tenant ; .icon == extras.region.
	wd := nestedMap(t, obj, "status", "widgetData")
	if got := wd["label"]; got != "acme-corp" {
		t.Fatalf("status.widgetData.label: extras.tenant must flow into widgetDataTemplate; got %v want acme-corp", got)
	}
	if got := wd["icon"]; got != "fa-eu" {
		t.Fatalf("status.widgetData.icon: extras.region must flow into widgetDataTemplate; got %v want fa-eu", got)
	}

	// status.resourcesRefs.items — one ref per extras names element, each
	// namespaced by the element (Diego's resourcesRefsTemplate case). The
	// resolver rewrites each ref into a snowplow /call loopback path of the
	// shape `/call?apiVersion=v1&namespace=<el>&resource=namespaces`, and sets
	// id to the template's `"ns-"+<el>`. We assert BOTH the per-element id
	// (template-set, stable) AND that the resolved path carries the element's
	// namespace (proving the extras element drove the ref's namespace).
	items := nestedSlice(t, obj, "status", "resourcesRefs", "items")
	if len(items) != 2 {
		t.Fatalf("resourcesRefsTemplate must fan out one ref per extras names element; got %d items want 2 (items=%v)", len(items), items)
	}
	for _, el := range []string{"alpha", "beta"} {
		if !hasRefWithID(items, "ns-"+el) {
			t.Fatalf("expected a resourcesRefs item with id ns-%s (from extras.names element %q); items=%v", el, el, items)
		}
		if !hasRefPathContainingNamespace(items, el) {
			t.Fatalf("expected a resourcesRefs item whose resolved /call path targets namespace=%s (from extras.names); items=%v", el, items)
		}
	}
	return ctx
}

// assertApiRefExtras resolves button-extras-apiref with extras={nsName:"kube-system"};
// the apiRef RESTAction's `path` is ${ "/api/v1/namespaces/" + .nsName }, so a
// correct resolve GETs the kube-system namespace and surfaces its name into
// status.widgetData.label. If extras had NOT reached the path, the GET would
// target a name-less namespaces path and .ns.nsName would be absent.
func assertApiRefExtras(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	extras := map[string]any{"nsName": "kube-system"}
	obj := resolveExtrasWidget(ctx, t, c, "button-extras-apiref", extras)

	wd := nestedMap(t, obj, "status", "widgetData")
	if got := wd["label"]; got != "kube-system" {
		t.Fatalf("apiRef path must be parametrised by extras.nsName: status.widgetData.label got %v want kube-system "+
			"(if nil/empty, extras did not reach the RESTAction fetch path)", got)
	}
	return ctx
}

// (Backward-compat / no-op-when-extras-absent is covered at the unit + cache-
// key layers: TestMergeExtras_NilOrEmptyIsNoOp, TestExtras_NoExtras_Template-
// Unchanged, and TestExtras_WidgetContentKey_EmptyEqualsNil. It is deliberately
// NOT re-asserted here: a static fixture whose widgetDataTemplate writes an
// extras-derived value into a REQUIRED string field (Button.spec label) would,
// with extras absent, write jq null and trip the Button CRD's string-type
// status validation — exercising CRD validation, not the feature's no-op
// property.)

// resolveExtrasWidget fetches the named Button and resolves it through the REAL
// widgets.Resolve with the given extras (AuthnNS set so the apiRef per-user
// endpoint resolves — see resolveWidget in widgets_test.go for the rationale).
func resolveExtrasWidget(ctx context.Context, t *testing.T, c *envconf.Config, name string, extras map[string]any) *unstructured.Unstructured {
	t.Helper()
	r, err := resources.New(c.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	r.WithNamespace(namespace)
	apis.AddToScheme(r.GetScheme())

	res := objects.Get(ctx, v1.ObjectReference{
		Reference: v1.Reference{Name: name, Namespace: namespace},
		Resource:  "buttons", APIVersion: "widgets.templates.krateo.io/v1beta1",
	})
	if res.Err != nil {
		log := xcontext.Logger(ctx)
		log.Error("unable to get widget", slog.Any("err", res.Err))
		t.Fatalf("get widget %q: %v", name, res.Err)
	}

	obj, err := Resolve(ctx, ResolveOptions{
		RC:      c.Client().RESTConfig(),
		AuthnNS: namespace,
		In:      res.Unstructured,
		Extras:  extras,
	})
	if err != nil {
		log := xcontext.Logger(ctx)
		log.Error("unable to resolve widget", slog.Any("err", err))
		t.Fatalf("resolve widget %q with extras=%v: %v", name, extras, err)
	}
	return obj
}

// --- small unstructured accessors (test-local) ---

func nestedMap(t *testing.T, obj *unstructured.Unstructured, fields ...string) map[string]any {
	t.Helper()
	m, ok, err := unstructured.NestedMap(obj.Object, fields...)
	if err != nil {
		t.Fatalf("NestedMap %v: %v", fields, err)
	}
	if !ok {
		return nil
	}
	return m
}

func nestedSlice(t *testing.T, obj *unstructured.Unstructured, fields ...string) []any {
	t.Helper()
	s, ok, err := unstructured.NestedSlice(obj.Object, fields...)
	if err != nil {
		t.Fatalf("NestedSlice %v: %v", fields, err)
	}
	if !ok {
		return nil
	}
	return s
}

func hasRefWithID(items []any, wantID string) bool {
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := m["id"].(string); id == wantID {
			return true
		}
	}
	return false
}

// hasRefPathContainingNamespace reports whether some resolved ref's /call
// loopback path carries `namespace=<ns>` (the resolver rewrites a
// resourcesRefsTemplate ref into /call?apiVersion=…&namespace=<ns>&resource=…).
func hasRefPathContainingNamespace(items []any, ns string) bool {
	want := "namespace=" + ns
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if p, _ := m["path"].(string); strings.Contains(p, want) {
			return true
		}
	}
	return false
}
