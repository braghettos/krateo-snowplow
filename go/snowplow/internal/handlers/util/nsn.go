package util

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/types"
)

func ParseNamespacedName(req *http.Request) (nsn types.NamespacedName, err error) {
	nsn.Name = req.URL.Query().Get("name")
	if len(nsn.Name) == 0 {
		err = fmt.Errorf("missing 'name' query parameter")
		return
	}

	nsn.Namespace = req.URL.Query().Get("namespace")
	if len(nsn.Namespace) == 0 {
		err = fmt.Errorf("missing 'namespace' query parameter")
		return
	}

	return
}
