// scripts/lint/no_unchecked_unstructured_assert.go — Ship L (0.30.246)
// go/ast lint for the regression class that produced Ship 0.30.233's
// bytesObject defect.
//
// PURPOSE (spec §6.1)
//
// Ship 0.30.233 introduced a raw type-assert at
// internal/cache/crd_discovery_side_effect.go:248:
//
//     u, ok := obj.(*unstructured.Unstructured)
//     if !ok || u == nil { c.discoverySkippedNG.Add(1); return }
//
// Post Ship H5 the streaming-listwatch is the default for every
// dynamic informer per watcher.go:1035-1047; the
// customresourcedefinitions informer therefore delivers *bytesObject
// here, NOT *unstructured.Unstructured. The assertion silently fails
// on every production event, returning early and leaving the per-
// resource composition informer never registered. Phase 6 S4 fast-
// path tripped 5+ minutes (vs the 3.4s pre-regression baseline)
// before the nav-walker finally re-discovered the group.
//
// This lint catches the REGRESSION CLASS: a raw content-bearing
// *unstructured.Unstructured assertion inside a LITERAL informer
// event-handler body (AddFunc / UpdateFunc / DeleteFunc) that does
// not also route through decodeBytesObject or
// fallbackUnstructuredFromIndexer (the H5-aware decode helpers).
//
// SCOPE (architect §6.1)
//
// The lint flags raw asserts inside the LITERAL handler bodies (the
// *ast.FuncLit immediately under the
// clientcache.ResourceEventHandlerFuncs{} or
// cache.ResourceEventHandlerFuncs{} composite literal). The Ship
// 0.30.233 specific defect site at crd_discovery_side_effect.go:248
// is NOT in such a literal body (it is in triggerCRDDiscovery,
// dispatched via channel + worker hop) — so this lint does NOT catch
// the original mistake directly. The remediation for that call-graph
// class is the architectural rule + audit table in the spec §6 +
// the WARN surface added in commit-2 (warnOnceCRDDecodeSkip).
//
// What the lint does catch is the IMMEDIATE syntactic regression:
// any future code that adds a raw content-assert directly inside an
// informer handler body. Per spec §6.1's "Dual-state proof", the
// lint PASSES on Ship-L HEAD's internal/cache/ tree and FAILS on
// scripts/lint/testdata/regression_unchecked_assert.go (the FAIL-
// side fixture).
//
// OQ2 (ratified, spec §11.1): does NOT flag `_ interface{}` AddFunc/
// UpdateFunc/DeleteFunc bodies (controller_health.go, secrets_informer.go).
// Go's blank identifier `_` syntactically forbids reading the parameter,
// so no content-bearing assert is possible. False-positive class
// eliminated by construction.
//
// USAGE
//
//   # PASS on production tree (after Ship L lands)
//   go run scripts/lint/no_unchecked_unstructured_assert.go \
//     --root=$(pwd)/internal/cache
//   # exit 0; no violations
//
//   # FAIL on regression fixture (the FAIL side of the dual-state proof)
//   go run scripts/lint/no_unchecked_unstructured_assert.go \
//     --root=$(pwd)/scripts/lint/testdata
//   # exit 1; violations listed
//
// `//go:build ignore` keeps this file out of `go build ./...` and
// `go vet ./...` (per the established codebase pattern at
// no_parallel_binding_derivation.go:60).

//go:build ignore

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fileAllowlist holds relative paths whose raw *unstructured.Unstructured
// asserts inside literal handler bodies are legitimate (none today).
// Empty until a future false-positive emerges; follows the F4 pattern.
var fileAllowlist = map[string]struct{}{}

// resourceEventHandlerFuncsTypeNames matches the composite-literal
// type for the client-go ResourceEventHandlerFuncs struct. Two
// possible spellings in the codebase:
//   - clientcache.ResourceEventHandlerFuncs (when imported as
//     `clientcache "k8s.io/client-go/tools/cache"`)
//   - cache.ResourceEventHandlerFuncs (rare; not used in snowplow today)
//
// We match by SelectorExpr.Sel.Name == "ResourceEventHandlerFuncs"
// — the package qualifier may be any local import name.
const resourceEventHandlerFuncsTypeName = "ResourceEventHandlerFuncs"

// decodeHelperNames are the H5-aware helper functions a content-
// reading handler body MAY route through. If any of these calls
// appears anywhere inside the FuncLit body, the raw assert (if any)
// is deemed "checked" and NOT flagged.
//
// In practice the H5-safe pattern is to use these helpers as the
// ENTRY to content access — never co-existing with a raw assert in
// the same handler body. But the lint allows both to co-exist
// (false-positive-conservative): the helper's presence signals the
// author knows about the H5 routing inversion.
var decodeHelperNames = map[string]struct{}{
	"decodeBytesObject":              {},
	"fallbackUnstructuredFromIndexer": {},
	"asRuntimeObject":                {},
}

type violation struct {
	File string
	Line int
	Col  int
	Snip string
}

func main() {
	var (
		root    = flag.String("root", "", "directory tree to lint; defaults to walking up from cwd until go.mod is found, then linting <root>/internal/cache")
		verbose = flag.Bool("v", false, "verbose: log every file walked + decisions")
	)
	flag.Parse()

	if *root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "lint: cwd: %v\n", err)
			os.Exit(2)
		}
		projectRoot := findProjectRoot(cwd)
		if projectRoot == "" {
			fmt.Fprintf(os.Stderr, "lint: no go.mod found walking up from %s\n", cwd)
			os.Exit(2)
		}
		*root = filepath.Join(projectRoot, "internal", "cache")
	}

	if _, err := os.Stat(*root); err != nil {
		fmt.Fprintf(os.Stderr, "lint: %s: %v\n", *root, err)
		os.Exit(2)
	}

	var violations []violation

	err := filepath.Walk(*root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			if *verbose {
				fmt.Fprintf(os.Stderr, "lint: skip test file %s\n", path)
			}
			return nil
		}
		rel := relPath(*root, path)
		if _, ok := fileAllowlist[rel]; ok {
			if *verbose {
				fmt.Fprintf(os.Stderr, "lint: skip allowlisted %s\n", rel)
			}
			return nil
		}

		fset := token.NewFileSet()
		// parser.ParseFile reads any .go file regardless of build
		// constraints — that's how we read the regression fixture
		// (//go:build ignore) for the FAIL-side dual-state proof.
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "lint: parse %s: %v\n", path, parseErr)
			os.Exit(2)
		}

		fileViolations := scanFile(fset, f, rel)
		violations = append(violations, fileViolations...)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint: walk: %v\n", err)
		os.Exit(2)
	}

	if len(violations) == 0 {
		if *verbose {
			fmt.Fprintln(os.Stderr, "lint: PASS — no raw *unstructured.Unstructured asserts in informer-handler literal bodies")
		}
		os.Exit(0)
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		return violations[i].Line < violations[j].Line
	})

	fmt.Fprintln(os.Stderr, "lint: FAIL — found raw *unstructured.Unstructured assert(s) inside informer event-handler literal bodies.")
	fmt.Fprintln(os.Stderr, "lint: violations:")
	for _, v := range violations {
		fmt.Fprintf(os.Stderr, "  %s:%d:%d: %s\n", v.File, v.Line, v.Col, v.Snip)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "lint: FIX — route content access through decodeBytesObject")
	fmt.Fprintln(os.Stderr, "lint:        (bytesobject.go:394) or fallbackUnstructuredFromIndexer")
	fmt.Fprintln(os.Stderr, "lint:        instead of a raw type-assert. Production CRD informer")
	fmt.Fprintln(os.Stderr, "lint:        delivery shape post-Ship-H5 is *bytesObject, not")
	fmt.Fprintln(os.Stderr, "lint:        *unstructured.Unstructured — a raw assert silently")
	fmt.Fprintln(os.Stderr, "lint:        drops every event (Ship 0.30.233 defect class).")
	os.Exit(1)
}

// scanFile walks the file AST and reports violations: raw
// *unstructured.Unstructured type-asserts inside literal informer
// event-handler bodies that do NOT also call a decode helper.
func scanFile(fset *token.FileSet, f *ast.File, relPath string) []violation {
	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if !isResourceEventHandlerFuncsLit(cl) {
			return true
		}
		// Each Elts entry is a KeyValueExpr (AddFunc: func(...)...).
		// The Value is the FuncLit handler body we inspect.
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			fl, ok := kv.Value.(*ast.FuncLit)
			if !ok {
				continue
			}
			// OQ2: skip handlers whose ALL parameters are the blank
			// identifier `_`. Such bodies syntactically cannot read
			// the obj — false-positive class eliminated by Go's
			// blank-identifier semantics.
			if allParamsBlank(fl) {
				continue
			}
			// Check this FuncLit body for raw asserts AND for the
			// presence of a decode helper. If both exist, the raw
			// assert is allowed (the author opted into the H5-aware
			// pattern).
			rawAsserts := findRawUnstructuredAsserts(fl.Body)
			if len(rawAsserts) == 0 {
				continue
			}
			if usesDecodeHelper(fl.Body) {
				continue
			}
			handlerKind := ""
			if id, ok := kv.Key.(*ast.Ident); ok {
				handlerKind = id.Name
			}
			for _, ra := range rawAsserts {
				pos := fset.Position(ra.Pos())
				out = append(out, violation{
					File: relPath,
					Line: pos.Line,
					Col:  pos.Column,
					Snip: "raw obj.(*unstructured.Unstructured) inside " + handlerKind + " body — no decodeBytesObject call",
				})
			}
		}
		return true
	})
	return out
}

// isResourceEventHandlerFuncsLit reports whether the composite literal
// is a ResourceEventHandlerFuncs{} construction. Matches the type by
// the SelectorExpr's Sel.Name regardless of import alias.
func isResourceEventHandlerFuncsLit(cl *ast.CompositeLit) bool {
	if cl.Type == nil {
		return false
	}
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel != nil && sel.Sel.Name == resourceEventHandlerFuncsTypeName
}

// allParamsBlank reports whether every parameter of the FuncLit is
// the blank identifier `_`. Go's spec says a `_` parameter is
// declared (so the signature satisfies the interface) but cannot be
// read in the function body — so any handler with all-`_` params is
// content-free by construction.
func allParamsBlank(fl *ast.FuncLit) bool {
	if fl.Type == nil || fl.Type.Params == nil {
		return false
	}
	totalNames := 0
	blankNames := 0
	for _, field := range fl.Type.Params.List {
		for _, n := range field.Names {
			totalNames++
			if n.Name == "_" {
				blankNames++
			}
		}
	}
	return totalNames > 0 && totalNames == blankNames
}

// findRawUnstructuredAsserts returns every TypeAssertExpr in the
// AST node whose target type is *unstructured.Unstructured. Matches
// the SelectorExpr by package-alias-name "unstructured" + Sel.Name
// "Unstructured" — same approach as the F4 lint at
// no_parallel_binding_derivation.go:222-247 (identifier-name match
// rather than full type resolution).
func findRawUnstructuredAsserts(node ast.Node) []ast.Node {
	var out []ast.Node
	ast.Inspect(node, func(n ast.Node) bool {
		ta, ok := n.(*ast.TypeAssertExpr)
		if !ok {
			return true
		}
		if ta.Type == nil {
			return true
		}
		// We look for *unstructured.Unstructured — a StarExpr whose
		// X is a SelectorExpr (unstructured.Unstructured).
		star, ok := ta.Type.(*ast.StarExpr)
		if !ok {
			return true
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name != "unstructured" {
			return true
		}
		if sel.Sel == nil || sel.Sel.Name != "Unstructured" {
			return true
		}
		out = append(out, ta)
		return true
	})
	return out
}

// usesDecodeHelper reports whether the AST node contains a call to
// one of the H5-aware decode helpers. The author opting into the
// helper signals awareness of the bytesObject routing; the lint
// then tolerates a co-existing raw assert.
func usesDecodeHelper(node ast.Node) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Match either bare identifier (decodeBytesObject(...)) or
		// SelectorExpr (cache.decodeBytesObject(...)).
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if _, ok := decodeHelperNames[fn.Name]; ok {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if fn.Sel != nil {
				if _, ok := decodeHelperNames[fn.Sel.Name]; ok {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// relPath returns the path relative to root; falls back to the
// absolute path if filepath.Rel fails.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// findProjectRoot walks up from the given dir until it finds a
// go.mod; returns "" if none found at any ancestor.
func findProjectRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
