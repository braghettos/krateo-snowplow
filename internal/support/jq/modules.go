// Package jq provides the shared gojq module loader used across snowplow's
// resolvers. ModuleLoader builds (once) a loader from the directory named by
// the JQ_MODULES_PATH env var so jq expressions in RESTActions and widgets
// can import common helper modules.
package jq

import (
	"os"
	"strings"
	"sync"

	"github.com/itchyny/gojq"
	"github.com/krateoplatformops/plumbing/jqutil"
)

const (
	EnvModulesPath = "JQ_MODULES_PATH"
)

var (
	once         sync.Once
	cachedLoader gojq.ModuleLoader
)

func ModuleLoader() gojq.ModuleLoader {
	once.Do(func() {
		basePath, ok := os.LookupEnv(EnvModulesPath)
		if !ok {
			return
		}

		basePath = strings.TrimSpace(basePath)
		if basePath == "" {
			return
		}

		cachedLoader = jqutil.DirModuleLoader(basePath)
	})
	return cachedLoader
}
