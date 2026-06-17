// Package util holds small, dependency-free helpers for parsing the /call
// HTTP request: the optional extras JSON context, the target GVR and
// namespaced name, call-path pagination, and an ETA formatter. It is a leaf
// package shared by the handlers and resolvers.
package util

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func ParseExtras(req *http.Request) (res map[string]any, err error) {
	res = map[string]any{}

	extrasParam := req.URL.Query().Get("extras")
	if extrasParam == "" {
		return
	}

	err = json.Unmarshal([]byte(extrasParam), &res)
	if err != nil {
		err = fmt.Errorf("invalid 'extras' parameter: %w", err)
		return
	}

	return
}
