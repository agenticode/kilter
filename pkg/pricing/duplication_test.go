package pricing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// One source of truth, asserted against the SOURCE of the domain packages.
//
// Four domain packages each inlined their own copy of the rates they needed,
// because none of them could edit this one. The copies are gone; these tests
// are what stops them coming back. A reviewer should not have to grep four
// packages to be sure the engine cannot disagree with itself about a price —
// and neither should the next author who needs a rate and finds it easier to
// type the number than to import it.
//
// Two independent checks, because they fail on different mistakes:
//
//   - TestNoRateLiteralsInDomainPackages catches a *known* rate reappearing:
//     the exact number this package owns, written out again somewhere else.
//   - TestNoMoneyLiteralsInDomainPackages catches a *new* rate being born in
//     the wrong place: any money-named binding initialized from a literal,
//     whether or not this package knows the number yet.

// domainPackages are the packages this one is the rate source of truth for.
// cmd/ is deliberately not scanned: it is wired by other hands every round and
// a rate there would be a wiring bug, not a duplicate table.
var domainPackages = []string{"../lambda", "../ecs", "../ebs", "../ec2"}

// canonicalRates is every AWS price pkg/pricing owns, by the name it owns it
// under. If a number here appears as a literal in a domain package, the two
// can drift — one gets a region added, the other does not — and the engine
// starts quoting two prices for one resource.
var canonicalRates = map[string]float64{
	"FallbackCPUHourlyUSD":           FallbackCPUHourlyUSD,
	"FallbackGiBHourlyUSD":           FallbackGiBHourlyUSD,
	"FargateVCPUHourlyUSD":           FargateVCPUHourlyUSD,
	"FargateGBHourlyUSD":             FargateGBHourlyUSD,
	"FargateARMVCPUHourlyUSD":        FargateARMVCPUHourlyUSD,
	"FargateARMGBHourlyUSD":          FargateARMGBHourlyUSD,
	"LambdaRequestUSDPerMillion":     LambdaRequestUSDPerMillion,
	"LambdaRequestUSD":               LambdaRequestUSD,
	"LambdaX86GBSecondUSD":           LambdaX86GBSecondUSD,
	"LambdaARMGBSecondUSD":           LambdaARMGBSecondUSD,
	"EBSGP2GBMonthUSD":               EBSGP2GBMonthUSD,
	"EBSGP3GBMonthUSD":               EBSGP3GBMonthUSD,
	"EBSGP3IOPSMonthUSD":             EBSGP3IOPSMonthUSD,
	"EBSGP3ThroughputMonthUSD":       EBSGP3ThroughputMonthUSD,
	"EBSIO1GBMonthUSD":               EBSIO1GBMonthUSD,
	"EBSIO2GBMonthUSD":               EBSIO2GBMonthUSD,
	"EC2SurplusCreditUSDPerVCPUHour": EC2SurplusCreditUSDPerVCPUHour,
}

// notARate is the complete set of money-named numbers a domain package is
// still allowed to write out, with the reason each is not a price. Adding to
// it is the deliberate decision the tests exist to force: if the next entry
// cannot be justified in one line here, it belongs in canonicalRates and in
// this package instead.
var notARate = map[string]string{
	// A policy floor an operator tunes ("do not roll a deployment to save
	// less than this"), not an AWS rate. It happens to equal the gp2 GB-month
	// price, which is exactly why the value-based check needs this note.
	"ecs.MinMoveMonthlyUSD": "policy floor: smallest saving worth a rolling deployment",
}

// moneyBinding is one place a domain package binds a money-named identifier.
type moneyBinding struct {
	pkg, file string
	line      int
	name      string
	literals  []literal
	src       string
}

type literal struct {
	text string
	val  float64
	ok   bool // parsed as a number
}

func (b moneyBinding) key() string { return b.pkg + "." + b.name }

// moneyBindings parses every non-test file of the domain packages and returns
// every const/var declaration, struct-literal field and assignment whose target
// name names money (contains "USD"), together with the numeric literals in the
// expression it is bound to.
func moneyBindings(t *testing.T) []moneyBinding {
	t.Helper()
	var out []moneyBinding
	for _, dir := range domainPackages {
		pkg := filepath.Base(dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		var sawGo bool
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			sawGo = true
			path := filepath.Join(dir, name)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, body, 0) // no comments: prose may quote a rate
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			record := func(target ast.Expr, value ast.Expr) {
				id := targetName(target)
				if id == "" || !strings.Contains(id, "USD") || value == nil {
					return
				}
				pos := fset.Position(value.Pos())
				end := fset.Position(value.End())
				out = append(out, moneyBinding{
					pkg: pkg, file: pkg + "/" + name, line: fset.Position(target.Pos()).Line,
					name: id, literals: literalsIn(value),
					src: strings.TrimSpace(string(body[pos.Offset:end.Offset])),
				})
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.ValueSpec:
					for i, nm := range v.Names {
						if i < len(v.Values) {
							record(nm, v.Values[i])
						}
					}
				case *ast.KeyValueExpr:
					record(v.Key, v.Value)
				case *ast.AssignStmt:
					for i, lhs := range v.Lhs {
						if i < len(v.Rhs) {
							record(lhs, v.Rhs[i])
						}
					}
				}
				return true
			})
		}
		if !sawGo {
			t.Fatalf("%s: no package source found — the scan would pass vacuously", dir)
		}
	}
	if len(out) == 0 {
		t.Fatal("no money-named bindings found in any domain package: the scan is not looking at real source")
	}
	return out
}

// targetName renders the identifier a value is bound to: `X`, `pkg.X` → X,
// or a struct field key. Anything else has no name to judge.
func targetName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

// literalsIn returns every numeric literal inside an expression.
func literalsIn(e ast.Expr) []literal {
	var out []literal
	ast.Inspect(e, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || (bl.Kind != token.FLOAT && bl.Kind != token.INT) {
			return true
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(bl.Value, "_", ""), 64)
		out = append(out, literal{text: bl.Value, val: v, ok: err == nil})
		return true
	})
	return out
}

// TestNoRateLiteralsInDomainPackages is the "grep shows the number in exactly
// one place" proof. It fails if any price this package owns is written out as
// a literal in a money-named binding of a domain package.
//
// It is scoped to money-named bindings on purpose. 0.10 is both the gp2
// GB-month rate and a perfectly ordinary threshold; flagging every 0.10 in
// four packages would be noise, and a test that cries wolf gets deleted. A
// rate that comes back comes back with a money name on it.
func TestNoRateLiteralsInDomainPackages(t *testing.T) {
	for _, b := range moneyBindings(t) {
		if why, exempt := notARate[b.key()]; exempt {
			t.Logf("allowed: %s = %s (%s)", b.key(), b.src, why)
			continue
		}
		for _, lit := range b.literals {
			if !lit.ok {
				continue
			}
			for rateName, rate := range canonicalRates {
				if lit.val != rate {
					continue
				}
				t.Errorf("%s:%d: %s is bound to the literal %s, which is pricing.%s (%v).\n"+
					"\tThat number lives in pkg/pricing and nowhere else — the two copies drift, and the\n"+
					"\tengine then quotes two prices for one resource. Use pricing.%s, or if this is not\n"+
					"\ta price, add %q to notARate with the reason.",
					b.file, b.line, b.name, lit.text, rateName, rate, rateName, b.key())
			}
		}
	}
}

// TestNoMoneyLiteralsInDomainPackages catches the rate that has not been
// invented yet: a money-named constant born in a domain package instead of
// here. Its value need not match anything this package knows — a rate written
// into pkg/ebs for a volume type pkg/pricing has never heard of is exactly the
// mistake that produced four rate tables in the first place.
func TestNoMoneyLiteralsInDomainPackages(t *testing.T) {
	for _, b := range moneyBindings(t) {
		if _, exempt := notARate[b.key()]; exempt {
			continue
		}
		for _, lit := range b.literals {
			if !lit.ok || isUnitConversion(lit.val) {
				continue
			}
			t.Errorf("%s:%d: %s = %s introduces a money literal in a domain package.\n"+
				"\tPrices live in pkg/pricing (see rates.go). Move the number there and read it from\n"+
				"\there, or if this is a policy threshold rather than an AWS rate, add %q to notARate.",
				b.file, b.line, b.name, b.src, b.key())
		}
	}
}

// isUnitConversion reports whether a literal is a power-of-ten scale factor
// (1e6 requests-per-million, 1000 milli-, 1<<20 …) rather than a quantity of
// money. Unit conversions are arithmetic; they are not rates and there is
// nothing to centralize about them.
func isUnitConversion(v float64) bool {
	if v <= 0 {
		return true // 0 and negatives are guards and sentinels, not prices
	}
	for p := 1.0; p <= 1e12; p *= 10 {
		if v == p {
			return true
		}
	}
	return false
}

// TestCanonicalRatesCoversEveryEmbeddedRate keeps the two tests above honest.
// A rate added to this package but forgotten in canonicalRates would silently
// stop being protected — the duplication check would pass while the number was
// free to be copied anywhere. Every exported ...USD-per-something constant in
// this package must be listed.
func TestCanonicalRatesCoversEveryEmbeddedRate(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, nm := range vs.Names {
					if !nm.IsExported() || !strings.Contains(nm.Name, "USD") || i >= len(vs.Values) {
						continue
					}
					if lits := literalsIn(vs.Values[i]); len(lits) > 0 {
						declared = append(declared, nm.Name)
					}
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no exported USD constants found in pkg/pricing: the scan is broken")
	}
	for _, name := range declared {
		if _, ok := canonicalRates[name]; !ok {
			t.Errorf("pricing.%s is an embedded rate but is missing from canonicalRates: "+
				"nothing stops a domain package copying it", name)
		}
	}
}

// TestRateLiteralCheckActuallyFires is the test's own smoke alarm. A source
// scan that silently matches nothing passes forever; this proves the matcher
// recognizes a re-inlined rate, using a synthetic source rather than a real
// regression.
func TestRateLiteralCheckActuallyFires(t *testing.T) {
	const reinlined = `package ebs
const GP2GBMonthUSD = 0.10
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fake.go", reinlined, 0)
	if err != nil {
		t.Fatal(err)
	}
	var hits int
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Values) == 0 || !strings.Contains(vs.Names[0].Name, "USD") {
			return true
		}
		for _, lit := range literalsIn(vs.Values[0]) {
			if lit.ok && lit.val == EBSGP2GBMonthUSD {
				hits++
			}
		}
		return true
	})
	if hits != 1 {
		t.Fatalf("the duplicate-rate matcher found %d re-inlined rates in a file that has 1", hits)
	}
	if isUnitConversion(EBSGP3IOPSMonthUSD) || isUnitConversion(FargateGBHourlyUSD) {
		t.Fatal("isUnitConversion excuses a real rate: the money-literal check would be blind to it")
	}
	if !isUnitConversion(1e6) || !isUnitConversion(1000) {
		t.Fatal("isUnitConversion rejects a power-of-ten scale factor")
	}
	// The exemption list must name real bindings; a stale entry is a hole.
	seen := map[string]bool{}
	for _, b := range moneyBindings(t) {
		seen[b.key()] = true
	}
	for key := range notARate {
		if !seen[key] {
			t.Errorf("notARate exempts %q, which no longer exists: remove it before it excuses something else", key)
		}
	}
}
