package rbac

import (
	"context"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/kubeconfig"
	"github.com/krateoplatformops/snowplow/internal/cache"
	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

type UserCanOptions struct {
	Verb          string
	GroupResource schema.GroupResource
	Namespace     string
}

// UserCan reports whether the user identified by ctx is permitted to
// perform opts.Verb on opts.GroupResource in opts.Namespace.
//
// Cache=on (CACHE_ENABLED=true): routes through EvaluateRBAC →
// informer-cached RBAC types. Zero SubjectAccessReview calls (0.30.4
// Revision 1 binding).
//
// Cache=off (default): falls through to the upstream
// SelfSubjectAccessReview path — preserves the CACHE_ENABLED toggle's
// removability contract (project_redis_removal.md).
func UserCan(ctx context.Context, opts UserCanOptions) (ok bool) {
	log := xcontext.Logger(ctx)

	if !cache.Disabled() {
		ui, err := xcontext.UserInfo(ctx)
		if err != nil {
			log.Error("rbac.UserCan: unable to extract UserInfo", slog.Any("err", err))
			return false
		}
		allowed, evalErr := EvaluateRBAC(ctx, EvaluateOptions{
			Username:  ui.Username,
			Groups:    ui.Groups,
			Verb:      opts.Verb,
			Group:     opts.GroupResource.Group,
			Resource:  opts.GroupResource.Resource,
			Namespace: opts.Namespace,
		})
		if evalErr != nil {
			log.Error("rbac.UserCan: EvaluateRBAC failed", slog.Any("err", evalErr))
			return false
		}
		return allowed
	}

	return userCanViaSAR(ctx, opts)
}

// userCanViaSAR is the upstream cache=off correctness baseline. It
// MUST be reachable only when cache.Disabled() == true — any cache=on
// call here is a Revision 1 binding violation (rollback trigger).
func userCanViaSAR(ctx context.Context, opts UserCanOptions) (ok bool) {
	log := xcontext.Logger(ctx)

	ep, err := xcontext.UserConfig(ctx)
	if err != nil {
		log.Error("unable to get user endpoint", slog.Any("err", err))
		return false
	}

	rc, err := kubeconfig.NewClientConfig(ctx, ep)
	if err != nil {
		log.Error("unable to create user client config", slog.Any("err", err))
		return false
	}

	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		log.Error("unable to create kubernetes clientset", slog.Any("err", err))
		return false
	}

	selfCheck := authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Group:     opts.GroupResource.Group,
				Resource:  opts.GroupResource.Resource,
				Namespace: opts.Namespace,
				Verb:      opts.Verb,
			},
		},
	}

	resp, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().
		Create(context.TODO(), &selfCheck, metav1.CreateOptions{})
	if err != nil {
		log.Error("unable to perform SelfSubjectAccessReviews",
			slog.Any("selfCheck", selfCheck), slog.Any("err", err))
		return false
	}

	log.Debug("SelfSubjectAccessReviews result", slog.Any("response", resp))

	return resp.Status.Allowed
}
