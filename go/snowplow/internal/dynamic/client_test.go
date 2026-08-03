//go:build integration
// +build integration

package dynamic

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestDynamicClient(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Setup").
		Assess("New Client", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			got, err := NewClient(c.Client().RESTConfig())
			assert.Nil(t, err)
			assert.NotNil(t, got)

			return ctx
		}).
		Assess("Discover", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig())
			assert.Nil(t, err)
			assert.NotNil(t, cli)

			got, err := cli.Discover(ctx, "secrets")
			assert.Nil(t, err)
			assert.True(t, len(got) > 0)

			return ctx
		}).
		Assess("List", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig())
			assert.Nil(t, err)
			assert.NotNil(t, cli)

			got, err := cli.List(ctx, Options{
				GVR:       schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
				Namespace: namespace,
			})
			assert.Nil(t, err)
			assert.NotNil(t, got)
			assert.True(t, len(got.Items) == 0)

			return ctx
		}).
		Assess("Get", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig())
			assert.Nil(t, err)
			assert.NotNil(t, cli)

			got, err := cli.Get(ctx, namespace, Options{
				GVR:       schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
				Namespace: "",
			})

			assert.Nil(t, err)
			assert.NotNil(t, got)
			assert.Equal(t, namespace, got.GetName())

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}
