package util

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func ParseGVR(req *http.Request) (gvr schema.GroupVersionResource, err error) {
	api := req.URL.Query().Get("apiVersion")
	if len(api) == 0 {
		err = fmt.Errorf("missing 'apiVersion' query parameter")
		return
	}

	res := req.URL.Query().Get("resource")
	if len(res) == 0 {
		err = fmt.Errorf("missing 'resource' query parameter")
		return
	}

	gv, err := schema.ParseGroupVersion(api)
	if err != nil {
		return gvr, err
	}

	gvr = gv.WithResource(res)
	return
}
