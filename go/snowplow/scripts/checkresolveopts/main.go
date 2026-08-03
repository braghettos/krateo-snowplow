// Command checkresolveopts is the CI guard for the 0.30.230-class nil-rc
// defect: a ResolveOptions struct literal that omits its rest.Config field
// leaves it zero (nil), which threads 8 calls deep and breaks every Kind=*
// widget /call at cache.GVRFor -> discoverPluralInfo ("plurals discovery:
// nil *rest.Config"). That latent path caused four HARD REVERTs
// (0.30.226 / 0.30.228 / 0.30.229 / fixed-at-root in 0.30.230). This gate
// makes a re-introduction a hard CI failure instead of a production revert.
//
// It is AST-based (not regex): it parses every non-test .go file under the
// scanned roots and inspects each composite literal whose type is one of the
// rest.Config-bearing ResolveOptions structs, asserting the rest.Config field
// (RC or SArc, per type) is present as a keyed element.
//
// LOW FALSE POSITIVE by construction:
//   - Only the FOUR ResolveOptions types that actually carry a *rest.Config
//     field are checked. The two that do not (widgetdatatemplate,
//     resourcesrefstemplate) are deliberately NOT in the table and are never
//     flagged.
//   - Disambiguation is by DEFINING PACKAGE (import path), not by the bare
//     "ResolveOptions" name — so widgetdatatemplate.ResolveOptions can never
//     be confused with widgets.ResolveOptions even though they share a name.
//   - Type is resolved from the literal's package qualifier (pkg.ResolveOptions
//     -> the import that binds `pkg`) or, when unqualified, from the file's own
//     package directory. No reliance on go/types or a build.
//
// COVERAGE BOUNDARY (review-ruled 2026-06-12): this gate checks literal-site
// keyed-field PRESENCE, not data-flow. Every direct keyed literal omitting its
// rest.Config field is caught (zero false-negatives on the 0.30.230 defect
// shape); a deferred assignment (`o := ResolveOptions{}; o.RC = rc`) is
// FLAGGED even though it is correct — if a future site needs that style,
// refactor it to an inline keyed literal rather than suppressing the gate.
// All construction sites at the time of writing are inline keyed literals.
//
// Usage:
//
//	checkresolveopts <root> [<root>...]
//
// Exits 0 when every checked literal sets its rest.Config field; exits 1 and
// prints file:line for each offending literal otherwise.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// requirement maps a DEFINING package import path to the name of the
// *rest.Config field its ResolveOptions struct carries. Only the four
// rest.Config-bearing types are listed; anything not here is never checked.
//
// Keyed by import-path SUFFIX (the package's own directory under the module)
// so the table is independent of the module path prefix and of any import
// alias the caller chooses.
var requirement = map[string]string{
	"internal/resolvers/restactions":     "SArc", // restactions.ResolveOptions
	"internal/resolvers/restactions/api": "RC",   // api.ResolveOptions
	"internal/resolvers/widgets":         "RC",   // widgets.ResolveOptions
	"internal/resolvers/widgets/apiref":  "RC",   // apiref.ResolveOptions
}

// typeName is the bare struct name we look for. Both checked and unchecked
// ResolveOptions types share this name; the requirement table disambiguates
// by defining package.
const typeName = "ResolveOptions"

type finding struct {
	file  string
	line  int
	pkg   string // defining package import-path suffix
	field string // the required-but-missing field
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: checkresolveopts <root> [<root>...]")
		os.Exit(2)
	}

	fset := token.NewFileSet()
	var findings []finding
	var scanned int

	for _, root := range os.Args[1:] {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Never descend into worktrees/vendored copies — they are not
				// part of the gated tree and would double-count.
				base := d.Name()
				if base == ".claude" || base == "vendor" || base == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			fs, perr := checkFile(fset, path)
			if perr != nil {
				return perr
			}
			findings = append(findings, fs...)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "checkresolveopts: walk %s: %v\n", root, err)
			os.Exit(2)
		}
	}

	if len(findings) == 0 {
		fmt.Printf("OK: every rest.Config-bearing ResolveOptions literal sets its rc field (%d files scanned).\n", scanned)
		return
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})
	fmt.Fprintf(os.Stderr, "ERROR: %d ResolveOptions literal(s) omit the required rest.Config field.\n", len(findings))
	fmt.Fprintf(os.Stderr, "       This is the 0.30.230-class nil-rc defect (four HARD REVERTs). A zero\n")
	fmt.Fprintf(os.Stderr, "       rest.Config breaks every Kind=* widget at cache.GVRFor.\n\n")
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "  %s:%d  %s.ResolveOptions{...} is missing field %q\n", f.file, f.line, lastSeg(f.pkg), f.field)
	}
	fmt.Fprintln(os.Stderr, "\n       Thread the SA *rest.Config at the construction site (r.saRC, w.rc, or")
	fmt.Fprintln(os.Stderr, "       rcFromCtx(ctx) for internal-driver sites). See helpers.go rcFromCtx doc.")
	os.Exit(1)
}

// checkFile parses one Go file and returns findings for every checked
// ResolveOptions literal that omits its required rest.Config field.
func checkFile(fset *token.FileSet, path string) ([]finding, error) {
	src, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// The file's own package import-path suffix (for unqualified literals):
	// derive from the file's directory relative to the module root by
	// stripping everything up to and including "snowplow/". WalkDir gives us
	// a path that already starts at the scan root (e.g.
	// "internal/resolvers/widgets/resolve.go"), so the directory IS the
	// import-path suffix we key on.
	ownPkgPath := normalizePkgPath(filepath.Dir(path))

	// Map import-binding-name -> import-path-suffix, so a qualified
	// `alias.ResolveOptions` resolves to the defining package even under an
	// import alias.
	imports := map[string]string{}
	for _, imp := range src.Imports {
		ip := strings.Trim(imp.Path.Value, `"`)
		suffix := importSuffix(ip)
		name := lastSeg(ip)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = suffix
	}

	var out []finding
	ast.Inspect(src, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || cl.Type == nil {
			return true
		}

		var defPkg string
		switch t := cl.Type.(type) {
		case *ast.Ident: // unqualified: ResolveOptions{...} in its own package
			if t.Name != typeName {
				return true
			}
			defPkg = ownPkgPath
		case *ast.SelectorExpr: // qualified: pkg.ResolveOptions{...}
			if t.Sel.Name != typeName {
				return true
			}
			pkgIdent, ok := t.X.(*ast.Ident)
			if !ok {
				return true
			}
			suffix, ok := imports[pkgIdent.Name]
			if !ok {
				return true
			}
			defPkg = suffix
		default:
			return true
		}

		field, checked := requirement[defPkg]
		if !checked {
			// Not a rest.Config-bearing ResolveOptions — never flag.
			return true
		}

		if !hasKeyedField(cl, field) {
			pos := fset.Position(cl.Pos())
			out = append(out, finding{
				file:  pos.Filename,
				line:  pos.Line,
				pkg:   defPkg,
				field: field,
			})
		}
		return true
	})
	return out, nil
}

// hasKeyedField reports whether the composite literal has a keyed element
// `field: ...`. The four checked ResolveOptions types are always constructed
// with keyed fields in this repo; a positional literal (no keys) is itself a
// red flag and is reported as missing the field.
func hasKeyedField(cl *ast.CompositeLit, field string) bool {
	for _, el := range cl.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		if key.Name == field {
			return true
		}
	}
	return false
}

// normalizePkgPath turns a filesystem dir (as seen by WalkDir, which may be
// "internal/..." or "./internal/..." or an absolute path containing
// ".../snowplow/internal/...") into the import-path suffix we key on
// ("internal/...").
func normalizePkgPath(dir string) string {
	dir = filepath.ToSlash(dir)
	if i := strings.LastIndex(dir, "/snowplow/"); i >= 0 {
		return dir[i+len("/snowplow/"):]
	}
	dir = strings.TrimPrefix(dir, "./")
	return dir
}

// importSuffix returns the import-path suffix under the snowplow module for a
// full import path, or the full path if it is not a snowplow-internal import
// (external imports are never ResolveOptions providers).
func importSuffix(ip string) string {
	const marker = "snowplow/"
	if i := strings.Index(ip, marker); i >= 0 {
		return ip[i+len(marker):]
	}
	return ip
}

func lastSeg(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
