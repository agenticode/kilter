package lambda

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The invariants below are properties of the SOURCE, not of any single
// function, so they are asserted against the source. A reviewer should not have
// to grep to be sure this package cannot actuate, cannot dial out and cannot
// read the clock — and neither should the next person to edit it.

func packageFiles(t *testing.T) (*token.FileSet, map[string]*ast.File, map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	src := map[string]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, b, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files[name], src[name] = f, string(b)
	}
	if len(files) == 0 {
		t.Fatal("no package source found")
	}
	return fset, files, src
}

// Air-gapped: standard library plus this repository, and nothing else. A new
// module dependency here would break the single-static-binary promise, and an
// AWS SDK import would put a network client in the decision path.
func TestNoForeignImports(t *testing.T) {
	_, files, _ := packageFiles(t)
	for name, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s", name, imp.Path.Value)
			}
			if strings.HasPrefix(path, "github.com/agenticode/kilter/") {
				continue
			}
			if strings.Contains(path, ".") { // a dot in the first element ⇒ a module path
				if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
					t.Errorf("%s imports %q: this package is stdlib + intra-repo only", name, path)
				}
			}
			for _, banned := range []string{"net/http", "net/rpc", "os/exec"} {
				if path == banned {
					t.Errorf("%s imports %q: nothing here does I/O to the outside world", name, banned)
				}
			}
		}
	}
}

// No clock. Callers pass `now`; a package that reads time.Now cannot be
// replayed, checkpointed or compared against yesterday's report.
func TestNoClockReads(t *testing.T) {
	_, _, src := packageFiles(t)
	for name, body := range src {
		for _, banned := range []string{"time.Now(", "time.Since(", "time.Until("} {
			if strings.Contains(body, banned) {
				t.Errorf("%s calls %s: this package reads no clock", name, banned)
			}
		}
	}
}

// Advisory only. None of the mutating Lambda APIs may be named anywhere in the
// package's code — not in a seam, not in a constant, not behind a flag.
func TestNoMutatingAPISurface(t *testing.T) {
	_, _, src := packageFiles(t)
	mutating := []string{
		"UpdateFunctionConfiguration", "UpdateFunctionCode", "PublishVersion",
		"PutProvisionedConcurrencyConfig", "PutFunctionConcurrency", "DeleteFunction",
		"CreateAlias", "UpdateAlias", "TagResource", "InvokeFunction",
	}
	for name, body := range src {
		// Prose — a doc comment or the text of the refusal error — is allowed
		// to NAME the API this package will not call; that naming is the
		// point. What must not exist is an IDENTIFIER: a seam method, a
		// function, a field. Only identifiers are scanned.
		code := identifiers(t, name, body)
		for _, m := range mutating {
			if strings.Contains(code, m) {
				t.Errorf("%s references the mutating API %q outside a comment", name, m)
			}
		}
	}
}

// No package-level mutable state. Two package-level vars exist and both are
// fixed tables; anything new needs a deliberate decision, which is what this
// allowlist forces.
func TestNoUnexpectedPackageState(t *testing.T) {
	_, files, _ := packageFiles(t)
	allowed := map[string]bool{"reportLabels": true, "collectedMetrics": true}
	for name, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for _, id := range vs.Names {
					if !allowed[id.Name] {
						t.Errorf("%s declares package-level var %q; this package holds no mutable state "+
							"(add it to the allowlist only if it is an immutable table)", name, id.Name)
					}
				}
			}
		}
	}
}

// identifiers re-parses the file and returns every identifier in it, with
// comments and string literals excluded, so prose naming an API this package
// refuses to call does not trip the mutating-surface check.
func identifiers(t *testing.T, name, body string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, body, 0) // no ParseComments ⇒ comments dropped
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			b.WriteString(id.Name)
			b.WriteByte('\n')
		}
		return true
	})
	return b.String()
}
