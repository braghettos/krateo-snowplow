diff --git a/internal/dynamic/client.go b/internal/dynamic/client.go
index 352fab5..c35300d 100644
--- a/internal/dynamic/client.go
+++ b/internal/dynamic/client.go
@@ -15,53 +15,12 @@ import (
 	"k8s.io/client-go/restmapper"
 )
 
-// ClientOption configures NewClient. Used by callers that know
-// up-front they will only call methods with an explicit GVR (i.e.
-// Get / List / Create / Delete with Options.GVR set) — they can opt
-// out of the discovery + restmapper build via WithSkipMapper, which
-// eliminates a per-call DiscoveryClient construction (#123).
-type ClientOption func(*clientCfg)
-
-type clientCfg struct {
-	skipMapper bool
-}
-
-// WithSkipMapper returns a ClientOption that suppresses the
-// DiscoveryClient + DeferredDiscoveryRESTMapper build inside
-// NewClient. Use only when EVERY call against the returned client
-// will pass Options.GVR explicitly — the mapper is consulted only
-// in resourceInterfaceFor when GVK is set OR GVR is empty.
-//
-// The Discover() method is NOT supported on a SkipMapper client
-// (it reads from discoveryClient, which is nil). Callers that need
-// Discover must omit this option.
-func WithSkipMapper() ClientOption {
-	return func(c *clientCfg) { c.skipMapper = true }
-}
-
-func NewClient(rc *rest.Config, opts ...ClientOption) (Client, error) {
-	cfg := clientCfg{}
-	for _, o := range opts {
-		o(&cfg)
-	}
-
+func NewClient(rc *rest.Config) (Client, error) {
 	dynamicClient, err := dynamic.NewForConfig(rc)
 	if err != nil {
 		return nil, err
 	}
 
-	if cfg.skipMapper {
-		// Skip both the DiscoveryClient and the DeferredDiscovery
-		// RESTMapper build. Safe because all entry points (Get /
-		// List / Create / Delete) flow through resourceInterfaceFor
-		// which only consults uc.mapper when opts.GVK is set or
-		// opts.GVR is empty — SkipMapper callers MUST pass GVR.
-		return &unstructuredClient{
-			dynamicClient: dynamicClient,
-			converter:     runtime.DefaultUnstructuredConverter,
-		}, nil
-	}
-
 	discoveryClient, err := discovery.NewDiscoveryClientForConfig(rc)
 	if err != nil {
 		return nil, err
diff --git a/internal/resolvers/crds/schema/schema.go b/internal/resolvers/crds/schema/schema.go
index 7b81bbb..679edee 100644
--- a/internal/resolvers/crds/schema/schema.go
+++ b/internal/resolvers/crds/schema/schema.go
@@ -69,17 +69,17 @@ func ValidateObjectStatus(ctx context.Context, rc *rest.Config, obj map[string]a
 			}}
 	}
 
-	// Ship 2 (production-aim cleanup 2026-06-01) — inlined CRD GET.
-	// The deleted internal/resolvers/crds.Get helper wrapped the
-	// same two-line dynamic.NewClient + Get call below; inlining
-	// removes the indirection AND lets us drop the unused restmapper
-	// build on every /call via dynamic.WithSkipMapper (#123).
-	//
-	// The CRD GVR is constant (apiextensions.k8s.io/v1/CRD) → the
-	// mapper is dead weight here: resourceInterfaceFor only consults
-	// uc.mapper when opts.GVK is set OR opts.GVR is empty. We pass
-	// Options.GVR explicitly so the mapper is never touched.
-	cli, err := dynamic.NewClient(rc, dynamic.WithSkipMapper())
+	// Ship 0.30.231 (2026-06-01) — inlined CRD GET. The deleted
+	// internal/resolvers/crds.Get helper wrapped the same two-line
+	// dynamic.NewClient + Get call below; inlining removes the
+	// indirection. Earlier (Ship 2) attempted to also skip the
+	// mapper build via dynamic.WithSkipMapper, but the contract was
+	// unsafe — resourceInterfaceFor (client.go:146) calls
+	// uc.mapper.RESTMapping unconditionally; the opts.GVR/opts.GVK
+	// branch at line 138 only chooses the source GVK, it does not
+	// skip the mapper. WithSkipMapper has been removed; the mapper
+	// build is a dead allocation here but cannot panic.
+	cli, err := dynamic.NewClient(rc)
 	if err != nil {
 		return err
 	}
