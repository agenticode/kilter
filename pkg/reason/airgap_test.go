package reason

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

// TestPackageDepsAreStdlibAndIntraRepo is the air gap, checked rather than
// asserted.
//
// The LLM plane is the one place in this tree where a dependency would be
// easy to justify — an SDK is right there, it is well written, and it would
// save a day. It is refused because kilter ships as one air-gapped binary and
// §5.9 requires every deterministic capability to survive with no model at
// all. A vendored HTTP client to a model endpoint would not break that on the
// day it landed; it would break it the first time someone made a tool call
// depend on a live provider, and by then the dependency would be load-bearing.
//
// So the rule is enforced at the import graph, where it is cheap: every
// package this one reaches transitively must be stdlib or kilter's own.
func TestPackageDepsAreStdlibAndIntraRepo(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	var foreign []string
	intra := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "github.com/agenticode/kilter/") {
			intra++
			continue
		}
		// A stdlib import path's first segment carries no dot: "net/http"
		// is stdlib, "github.com/..." and "k8s.io/..." are not.
		first, _, _ := strings.Cut(p, "/")
		if !strings.Contains(first, ".") {
			continue
		}
		foreign = append(foreign, p)
	}
	if intra == 0 {
		t.Fatal("go list -deps found no intra-repo dependency; it is looking at the wrong package")
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		t.Errorf("pkg/reason depends on %d package(s) outside stdlib and this repo: %v\n"+
			"the reasoning plane must add no module dependency: no model SDK, no HTTP client to a model endpoint, "+
			"not behind a build tag", len(foreign), foreign)
	}
}

// TestNoFileHereImportsANetworkOrCloudPackage is the narrower, per-file half.
// The dependency check above would also pass if a file imported net/http and
// never called it; a reasoning package has no business holding a socket at
// all, so the import itself is the violation.
func TestNoFileHereImportsANetworkOrCloudPackage(t *testing.T) {
	forbidden := []string{
		"net/http", "net", "net/url", "os/exec", "crypto/tls",
		"aws-sdk-go", "k8s.io/", "google.golang.org/", "golang.org/x/net",
	}
	for _, path := range packageFiles(t, false) {
		f := parseGo(t, path)
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range forbidden {
				if p == bad || strings.Contains(p, bad) {
					t.Errorf("%s imports %q: the reasoning plane reads a substrate, it does not open sockets "+
						"or talk to a cloud", path, p)
				}
			}
		}
	}
}

// TestNoActuatorSymbolIsReachableFromThisPackage is
// cmd/BRAINWIRE-FINDINGS.md §6's check, pointed here.
//
// The type system makes a write tool unrepresentable from outside this
// package (see Tool's doc comment). What it cannot prevent is a tool body
// compiled *inside* this package closing over a mutating handle — a closure
// defeats any amount of interface narrowing. This is the check that covers
// that hole, and it derives its forbidden set from pkg/ec2's and pkg/rds's
// own actuate*.go sources rather than from a list somebody has to remember to
// update: a new actuator entry point is covered the moment it is written.
func TestNoActuatorSymbolIsReachableFromThisPackage(t *testing.T) {
	forbidden := actuatorSymbols(t)
	for _, canary := range []string{"NewActuator", "Actuator", "ActuatorConfig"} {
		if !forbidden[canary] {
			t.Fatalf("the actuator symbol scan found no %q; it is looking in the wrong place", canary)
		}
	}

	files := packageFiles(t, true)
	if len(files) == 0 {
		t.Fatal("no source was scanned")
	}
	for _, path := range files {
		f := parseGo(t, path)
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if _, isActuator := actuatorPackage(p); isActuator {
				t.Errorf("%s imports %s, which carries actuators; pkg/reason must not link one at all", path, p)
			}
		}
		// Even with no actuator package imported, a bare identifier that
		// matches an actuator entry point is worth failing on: it means
		// somebody wrote Execute/Revert/Apply-shaped code in here.
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if forbidden[sel.Sel.Name] && looksLikeActuation(sel.Sel.Name) {
				t.Errorf("%s names %s, which is an actuator entry point", path, sel.Sel.Name)
			}
			return true
		})
	}
}

// looksLikeActuation narrows the derived set to the identifiers whose
// appearance in this package would actually mean something. pkg/ec2 and
// pkg/rds export plenty of harmless nouns from their actuate files; the
// verbs are the ones that must never appear beside a model.
func looksLikeActuation(name string) bool {
	for _, verb := range []string{"Execute", "Revert", "Apply", "Actuate", "Modify", "Terminate", "Stop", "Delete", "Drain"} {
		if strings.HasPrefix(name, verb) {
			return true
		}
	}
	return false
}

func actuatorPackage(path string) (string, bool) {
	switch path {
	case "github.com/agenticode/kilter/pkg/ec2":
		return "pkg/ec2", true
	case "github.com/agenticode/kilter/pkg/rds":
		return "pkg/rds", true
	case "github.com/agenticode/kilter/pkg/actuate":
		return "pkg/actuate", true
	}
	return "", false
}

// actuatorSymbols is every exported identifier declared in an actuate*.go
// file of an actuator-bearing package.
func actuatorSymbols(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, dir := range []string{"../ec2", "../rds"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "actuate") || !strings.HasSuffix(name, ".go") ||
				strings.HasSuffix(name, "_test.go") {
				continue
			}
			f := parseGo(t, filepath.Join(dir, name))
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					if d.Name.IsExported() {
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

// TestOnlyToolGoConstructsATool keeps the single-constructor claim true.
//
// Tool's fields are unexported, so no other package can build one. Inside
// this package a composite literal would still compile, and `Tool{readOnly:
// true, run: writeSomething}` is exactly the line this design exists to
// prevent. The rule is therefore mechanical: tool.go declares the type and
// builds it; nothing else in the package may write a Tool literal.
func TestOnlyToolGoConstructsATool(t *testing.T) {
	assertLiteralConfinedTo(t, "Tool", "tool.go")
}

// TestOnlyRefusalGoConstructsARefusal is the anti-echo rule, mechanized.
//
// A Refusal carries no free text: its Detail is a table lookup keyed by its
// Code (see refusal.go). That property survives only as long as every refusal
// comes from the two constructors there — one `&Refusal{Code: ..., Detail:
// fmt.Sprintf("limit %q is not a number", arg)}` anywhere else and the whole
// defense is gone, silently, in a line that looks helpful.
func TestOnlyRefusalGoConstructsARefusal(t *testing.T) {
	assertLiteralConfinedTo(t, "Refusal", "refusal.go")
}

func assertLiteralConfinedTo(t *testing.T, typeName, allowed string) {
	t.Helper()
	found := 0
	for _, path := range packageFiles(t, false) {
		f := parseGo(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			name := ""
			switch tp := lit.Type.(type) {
			case *ast.Ident:
				name = tp.Name
			case *ast.SelectorExpr:
				name = tp.Sel.Name
			}
			if name != typeName {
				return true
			}
			found++
			if filepath.Base(path) != allowed {
				t.Errorf("%s builds a %s literal; only %s may, and the type's guarantees live there",
					path, typeName, allowed)
			}
			return true
		})
	}
	if found == 0 {
		t.Fatalf("no %s literal was found anywhere; the scan is not looking where it thinks it is", typeName)
	}
}

// TestEveryRefusalCodeHasDetail keeps the detail table total. A code with no
// entry produces a refusal with an empty detail — a refusal that says nothing
// is barely better than no refusal at all.
func TestEveryRefusalCodeHasDetail(t *testing.T) {
	codes := map[string]bool{}
	f := parseGo(t, "refusal.go")
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Code") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				codes[v] = true
			}
		}
	}
	if len(codes) < 10 {
		t.Fatalf("found only %d refusal codes; the scan is wrong", len(codes))
	}
	for code := range codes {
		if refusalDetail[code] == "" {
			t.Errorf("refusal code %q has no entry in refusalDetail", code)
		}
	}
	for code := range refusalDetail {
		if !codes[code] {
			t.Errorf("refusalDetail carries %q, which is not a declared code", code)
		}
	}
}

// TestTheZeroToolCannotBeRegistered is the runtime half of the by-construction
// argument: the one Tool value another package can produce is inert.
func TestTheZeroToolCannotBeRegistered(t *testing.T) {
	r := registry(t, substrate(t))
	if err := r.Register(Tool{}); err == nil {
		t.Fatal("the zero Tool was registered; a tool with no read-only stamp must be refused")
	}
	if (Tool{}).ReadOnly() {
		t.Fatal("the zero Tool claims to be read-only")
	}
	// And every tool that IS registered carries the stamp.
	for _, d := range r.Tools() {
		if !d.ReadOnly {
			t.Errorf("registered tool %q is not read-only", d.Name)
		}
	}
}

// TestTheNarrowedStoreRefusesToWrite pins the capability boundary at runtime.
// roStore satisfies evidence.Store so pkg/explain and pkg/evidence's helpers
// accept it; the write half of that interface is a hard refusal rather than a
// method nobody happens to call.
func TestTheNarrowedStoreRefusesToWrite(t *testing.T) {
	m := substrate(t)
	ro := roStore{st: m}

	var asStore evidence.Store = ro // compile-time: it is a Store
	err := asStore.Append(evidence.EvidenceEvent{
		At:       t0,
		Kind:     evidence.EventFinding,
		Subject:  evidence.SubjectRef{Cluster: cluster, Kind: evidence.SubjectContainer, Key: containerKey},
		Severity: evidence.SeverityInfo,
	})
	if err != ErrReadOnly {
		t.Fatalf("writing through the narrowed store returned %v, want ErrReadOnly", err)
	}
	// And nothing was written.
	s := evidence.SubjectRef{Cluster: cluster, Kind: evidence.SubjectContainer, Key: containerKey}
	evs, err := m.Events(s, t0, t0.Add(time.Nanosecond), evidence.EventFinding)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("the refused append stored %d event(s)", len(evs))
	}
}

// TestTheToolSurfaceIsReadOnlyAndEnumerated states the §5.2 posture as a test:
// the registry serves exactly the tools this package declares, all read-only,
// and none of them takes a free-text query.
func TestTheToolSurfaceIsReadOnlyAndEnumerated(t *testing.T) {
	r := registry(t, substrate(t))
	want := []string{ToolClusterTimeline, ToolGetDossier, ToolExplain, ToolListSubjects, ToolQueryEvidence}
	sort.Strings(want)
	var got []string
	for _, d := range r.Tools() {
		got = append(got, d.Name)
	}
	if len(got) != len(want) {
		t.Fatalf("registry serves %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry serves %v, want %v (name order)", got, want)
		}
	}
	// Every parameter is a name from a closed set, a bounded integer, or an
	// instant. No parameter is an unbounded string, because an unbounded
	// string argument is a query language waiting to happen.
	for _, name := range got {
		for _, p := range r.byName[name].schema.Params() {
			switch p.kind {
			case kindIdent, kindIdentList:
				if p.maxLen <= 0 || p.maxLen > maxDisplayIdent {
					t.Errorf("tool %q parameter %q is an unbounded string", name, p.name)
				}
			case kindEnum, kindQuantity, kindInstant, kindFlag:
			default:
				t.Errorf("tool %q parameter %q has an unknown kind", name, p.name)
			}
		}
	}
}

func packageFiles(t *testing.T, includeTests bool) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	out := files[:0]
	for _, f := range files {
		if !includeTests && strings.HasSuffix(f, "_test.go") {
			continue
		}
		out = append(out, f)
	}
	return out
}

func parseGo(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}
