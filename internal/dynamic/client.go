package dynamic

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

func NewClient(rc *rest.Config) (Client, error) {
	dynamicClient, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return nil, err
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(
		cacheddiscovery.NewMemCacheClient(discoveryClient),
	)

	return &unstructuredClient{
		dynamicClient:   dynamicClient,
		discoveryClient: discoveryClient,
		mapper:          mapper,
		converter:       runtime.DefaultUnstructuredConverter,
	}, nil
}

type Options struct {
	Namespace string
	GVK       schema.GroupVersionKind
	GVR       schema.GroupVersionResource
}

type Client interface {
	Get(ctx context.Context, name string, opts Options) (*unstructured.Unstructured, error)
	List(ctx context.Context, opts Options) (*unstructured.UnstructuredList, error)
	Create(ctx context.Context, obj *unstructured.Unstructured, opts Options) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, name string, opts Options) error
	FromUnstructured(in map[string]any, out any) error
	ToUnstructured(in any) (map[string]any, error)
	Discover(ctx context.Context, category string) ([]schema.GroupVersionResource, error)
}

var _ Client = (*unstructuredClient)(nil)

type unstructuredClient struct {
	dynamicClient   *dynamic.DynamicClient
	discoveryClient discovery.DiscoveryInterface
	mapper          *restmapper.DeferredDiscoveryRESTMapper
	converter       runtime.UnstructuredConverter
}

func (uc *unstructuredClient) Create(ctx context.Context, obj *unstructured.Unstructured, opts Options) (*unstructured.Unstructured, error) {
	ri, err := uc.resourceInterfaceFor(opts)
	if err != nil {
		return nil, err
	}

	return ri.Create(ctx, obj, metav1.CreateOptions{})
}

func (uc *unstructuredClient) Get(ctx context.Context, name string, opts Options) (*unstructured.Unstructured, error) {
	ri, err := uc.resourceInterfaceFor(opts)
	if err != nil {
		return nil, err
	}

	return ri.Get(ctx, name, metav1.GetOptions{})
}

func (uc *unstructuredClient) List(ctx context.Context, opts Options) (*unstructured.UnstructuredList, error) {
	ri, err := uc.resourceInterfaceFor(opts)
	if err != nil {
		return nil, err
	}

	return ri.List(ctx, metav1.ListOptions{})
}

func (uc *unstructuredClient) Delete(ctx context.Context, name string, opts Options) error {
	ri, err := uc.resourceInterfaceFor(opts)
	if err != nil {
		return err
	}

	return ri.Delete(ctx, name, metav1.DeleteOptions{})
}

func (uc *unstructuredClient) FromUnstructured(in map[string]any, out any) error {
	return uc.converter.FromUnstructured(in, out)
}

func (uc *unstructuredClient) ToUnstructured(in any) (map[string]any, error) {
	return uc.converter.ToUnstructured(in)
}

func (uc *unstructuredClient) Discover(ctx context.Context, category string) (all []schema.GroupVersionResource, err error) {
	lists, err := uc.discoveryClient.ServerPreferredResources()
	if err != nil {
		return
	}

	for _, list := range lists {
		if len(list.APIResources) == 0 {
			continue
		}

		for _, el := range list.APIResources {
			if !found(el, category) {
				continue
			}

			all = append(all, schema.GroupVersionResource{
				Group:    el.Group,
				Version:  el.Version,
				Resource: el.Name,
			})
		}
	}

	return
}

func (uc *unstructuredClient) resourceInterfaceFor(opts Options) (dynamic.ResourceInterface, error) {
	if opts.GVK.Empty() && !opts.GVR.Empty() {
		gvk, err := uc.mapper.KindFor(opts.GVR)
		if err != nil {
			return nil, err
		}
		opts.GVK = gvk
	}

	restMapping, err := uc.mapper.RESTMapping(opts.GVK.GroupKind(), opts.GVK.Version)
	if err != nil {
		return nil, err
	}

	var ri dynamic.ResourceInterface
	if len(opts.Namespace) == 0 {
		ri = uc.dynamicClient.Resource(restMapping.Resource)
	} else {
		ri = uc.dynamicClient.Resource(restMapping.Resource).
			Namespace(opts.Namespace)
	}
	return ri, nil
}

func found(el metav1.APIResource, str string) bool {
	if strings.EqualFold(el.Name, str) {
		return true
	}

	if strings.EqualFold(el.SingularName, str) {
		return true
	}

	if contains(el.ShortNames, str) {
		return true
	}

	return contains(el.Categories, str)
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}
