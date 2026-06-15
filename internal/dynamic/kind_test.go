//go:build integration
// +build integration

package dynamic

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"

	"github.com/krateoplatformops/snowplow/internal/cache"
	xenv "github.com/krateoplatformops/plumbing/env"
)

var (
	testenv     env.Environment
	clusterName string
	namespace   string
)

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	namespace = "demo-system"
	clusterName = "krateo"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		envfuncs.CreateNamespace(namespace),
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.DestroyCluster(clusterName),
	)

	os.Exit(testenv.Run(m))
}

func TestKindFor(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Setup").
		Assess("KindFor", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// KindFor was refactored out of internal/dynamic; cache.KindForGVR is its
			// replacement (returns the Kind string for a GVR).
			got, err := cache.KindForGVR(ctx, schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, c.Client().RESTConfig())
			assert.Nil(t, err)
			assert.Equal(t, "ConfigMap", got)

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}
