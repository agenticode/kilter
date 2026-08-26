package crossover

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is the complete import surface this package may have. It is
// an allowlist, not a denylist, so a future edit that pulls in pkg/plan,
// pkg/actuate, pkg/safety, pkg/provider — or the AWS SDK — fails here rather
// than quietly wiring an advisory report into an actuation path.
func allowedImports() map[string]bool {
	return map[string]bool{
		"fmt": true, "math": true, "sort": true, "strings": true, "time": true,
		"github.com/agenticode/kilter/pkg/binpack": true,
		"github.com/agenticode/kilter/pkg/model":   true,
		"github.com/agenticode/kilter/pkg/pricing": true,
	}
}

// forbiddenNameFragments are the shapes of an actuatable thing. U3 is advisory
// only: it produces a report, and nothing in it may become a step. "Evict" is
// deliberately absent: GateEvictionIntolerant names a property of a pod, not
// something this package does.
func forbiddenNameFragments() []string {
	return []string{"Step", "Actuat", "Execute", "Apply", "Patch", "Revert", "Approve"}
}

// parsePackage returns the non-test files of this package.
func parsePackage(t *testing.T) (*token.FileSet, []*ast.File, []string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		files = append(files, f)
		names = append(names, n)
	}
	if len(files) == 0 {
		t.Fatal("no source files found; the purity test would pass vacuously")
	}
	return fset, files, names
}

// TestPackageIsPureAndAdvisory is the structural half of "advisory only".
func TestPackageIsPureAndAdvisory(t *testing.T) {
	fset, files, names := parsePackage(t)
	t.Logf("checked %d files: %s", len(names), strings.Join(names, ", "))
	allowed := allowedImports()

	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("bad import literal %s", imp.Path.Value)
			}
			if !allowed[path] {
				t.Errorf("%s: import %q is not on the allowlist — a crossover report must not reach an actuation path or a cloud SDK",
					fset.Position(imp.Pos()), path)
			}
		}
	}

	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.SelectorExpr:
				// No clock: callers pass `now`.
				if id, ok := d.X.(*ast.Ident); ok && id.Name == "time" && d.Sel.Name == "Now" {
					t.Errorf("%s: time.Now() in package logic — callers pass now", fset.Position(d.Pos()))
				}
			case *ast.GenDecl:
				// No package-level mutable state. Only const and type may sit
				// at file scope; a package-level var (even a slice of gates) is
				// state every caller shares.
				if d.Tok == token.VAR && isFileScope(f, d) {
					t.Errorf("%s: package-level var declaration — this package holds no mutable state",
						fset.Position(d.Pos()))
				}
			}
			return true
		})
	}

	for _, f := range files {
		for _, decl := range f.Decls {
			for _, name := range declaredNames(decl) {
				for _, frag := range forbiddenNameFragments() {
					if strings.Contains(name, frag) {
						t.Errorf("declaration %q contains %q: U3 is advisory and must not name an action", name, frag)
					}
				}
			}
		}
	}
}

func isFileScope(f *ast.File, target *ast.GenDecl) bool {
	for _, d := range f.Decls {
		if d == ast.Decl(target) {
			return true
		}
	}
	return false
}

func declaredNames(decl ast.Decl) []string {
	var out []string
	switch d := decl.(type) {
	case *ast.FuncDecl:
		out = append(out, d.Name.Name)
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				out = append(out, s.Name.Name)
				if st, ok := s.Type.(*ast.StructType); ok {
					for _, fld := range st.Fields.List {
						for _, n := range fld.Names {
							out = append(out, n.Name)
						}
					}
				}
			case *ast.ValueSpec:
				for _, n := range s.Names {
					out = append(out, n.Name)
				}
			}
		}
	}
	return out
}
