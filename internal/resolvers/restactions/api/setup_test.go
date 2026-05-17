//go:build unit
// +build unit

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

func Example_createRequestOptions() {
	dict := map[string]any{
		"namespaces": []any{"demo-system", "krateo-system", "example-system"},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	all := createRequestOptions(context.Background(), logger, &templates.API{
		Name: "example",
		Path: `${ "/api/v1/namespaces/" + (.) + "/pods" }`,
		DependsOn: &templates.Dependency{
			Name:     "namespaces",
			Iterator: ptr.To(".[]"),
		},
		Headers: []string{
			`${ "X-Namespace: " + (.) }`,
		},
	}, dict)

	for _, el := range all {
		fmt.Println(ptr.Deref(el.Verb, http.MethodGet), el.Path)
		for _, y := range el.Headers {
			fmt.Printf("  %s\n", y)
		}
	}

	// Output:
	// GET /api/v1/namespaces/demo-system/pods
	//   X-Namespace: demo-system
	// GET /api/v1/namespaces/krateo-system/pods
	//   X-Namespace: krateo-system
	// GET /api/v1/namespaces/example-system/pods
	//   X-Namespace: example-system
}

func Example_createRequestOptions_no_iter() {
	dict := map[string]any{
		"namespaces": []any{"demo-system", "krateo-system", "example-system"},
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	all := createRequestOptions(context.Background(), logger, &templates.API{
		Name: "example",
		Path: `${ "/api/v1/namespaces/" + (.namespaces[2]) + "/pods" }`,
		Verb: ptr.To(string(http.MethodPost)),
		DependsOn: &templates.Dependency{
			Name: "namespaces",
		},
	}, dict)

	for _, el := range all {
		fmt.Println(ptr.Deref(el.Verb, http.MethodGet), el.Path)
		for _, y := range el.Headers {
			fmt.Printf("  %s\n", y)
		}
	}

	// Output:
	// POST /api/v1/namespaces/example-system/pods
}
