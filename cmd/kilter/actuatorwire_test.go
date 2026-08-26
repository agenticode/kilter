package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The actuators stay unreachable from the binary.
//
// pkg/ec2/actuate*.go and pkg/rds/actuate*.go are shipped, tested and
// DELIBERATELY not wired: stopping an instance or modifying a database's
// storage is a decision with an owner, and that decision has not been made.
// Both packages are already linked into `kilter` for their COLLECTORS
// (cmd/kilter/rds.go, cmd/kilter/rdslive.go), so "does the binary import
// pkg/rds" cannot be the question — it does, and must. The question is whether
// any command names an actuation symbol, and this asserts that none does.
//
// The check derives its own forbidden list from the source rather than
// carrying a hand-written one: every exported identifier declared in a file
// called actuate*.go is off limits. A new actuator entry point is therefore
// covered the moment it is written, with no list to remember to update.
//
// This unit added brainsource.go, backtest --cluster, why-cost --db and
// explain --db, all of which reach a bbolt file and a pricing catalog. None of
// them can reach a mutating cloud path, and this is how that is known rather
// than asserted in a comment.
func TestNoActuatorIsReachableFromTheBinary(t *testing.T) {
	forbidden := actuatorSymbols(t)
	// A canary: if the scan ever stops finding declarations — a rename, a
	// moved file, a parse failure swallowed — this test would pass over an
	// empty set and prove nothing.
	for _, canary := range []string{"NewActuator", "Actuator", "ActuatorConfig"} {
		if !forbidden[canary] {
			t.Fatalf("the actuator symbol scan found no %q; it is looking in the wrong place", canary)
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var scanned int
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanned++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		// The local name each actuator-bearing package is imported under,
		// which is not always the last path segment.
		locals := map[string]string{}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			pkg, ok := actuatorPackage(p)
			if !ok {
				continue
			}
			name := filepath.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			locals[name] = pkg
		}
		if len(locals) == 0 {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			pkg, ok := locals[id.Name]
			if !ok {
				return true
			}
			if forbidden[sel.Sel.Name] {
				t.Errorf("%s names %s.%s: that is an actuator, and no mutating AWS path may be "+
					"reachable from the binary", path, pkg, sel.Sel.Name)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no command source was scanned")
	}
}

// actuatorPackage reports whether an import path is a package that carries
// actuators, and is the single place the two are named.
func actuatorPackage(path string) (string, bool) {
	switch path {
	case "github.com/agenticode/kilter/pkg/ec2":
		return "pkg/ec2", true
	case "github.com/agenticode/kilter/pkg/rds":
		return "pkg/rds", true
	}
	return "", false
}

// actuatorSymbols is every exported identifier declared in an actuate*.go file
// of an actuator-bearing package.
func actuatorSymbols(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, dir := range []string{"../../pkg/ec2", "../../pkg/rds"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "actuate") ||
				!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil && d.Name.IsExported() {
						out[d.Name.Name] = true
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() {
								out[s.Name.Name] = true
							}
						case *ast.ValueSpec:
							for _, n := range s.Names {
								if n.IsExported() {
									out[n.Name] = true
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

// TestNoCloudSDKReachesTheNewSources.
//
// The narrower, structural half of the same rule: the files this unit added or
// rewired must not import a cloud SDK at all. backtest --cluster, why-cost --db
// and explain --db read one bbolt file and one pricing catalog; if any of them
// ever grew an AWS client, the seam this prohibition protects would be open
// regardless of which symbols were named.
func TestNoCloudSDKReachesTheNewSources(t *testing.T) {
	for _, path := range []string{"brainsource.go", "backtest.go", "explain.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(p, "aws-sdk-go") || strings.Contains(p, "k8s.io") {
				t.Errorf("%s imports %s; these verbs read a database, not a cloud or a cluster", path, p)
			}
			if _, isActuator := actuatorPackage(p); isActuator {
				t.Errorf("%s imports %s, which carries actuators", path, p)
			}
		}
	}
}
