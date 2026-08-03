package resourcesrefs

import (
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestMapVerbs(t *testing.T) {
	table := []struct {
		in  string
		out []string
	}{
		{"post", []string{"create"}},
		{"Put", []string{"update"}},
		{"gEt", []string{"get"}},
		{"get", []string{"get"}},
		// Empty verb => the full kubeToREST key set. "patch" was added to
		// the verb map in #115 (verbs.go:38/47); this expectation predates
		// that commit and is updated here to the shipped 5-verb behavior
		// (sorted: create, delete, get, patch, update). Task #312.
		{"", []string{"create", "delete", "get", "patch", "update"}},
	}

	for _, tc := range table {
		got := mapVerbs(tc.in)
		sort.Strings(got)

		if diff := cmp.Diff(got, tc.out); len(diff) > 0 {
			t.Fatal(diff)
		}
	}
}
