// Package arch holds architecture tests: assertions about the SHAPE of the
// codebase rather than about the behaviour of any one function.
//
// These exist because the platform's most important safety properties are
// structural. "Python cannot reach a broker" and "every order goes through one
// pipeline" are not behaviours that a unit test can pin down — they are facts
// about which package may import which, and about how many call sites exist.
// A comment asserting them is a convention; a test asserting them is a
// constraint.
//
// Each test states the property, the reason it matters, and what to do if it
// fails. A failure here is a design decision, not a broken assertion to be
// patched.
package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const modulePath = "github.com/vantage/control-api"

// repoRoot is the control-api module root, two levels up from internal/arch.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// packageImports returns the direct imports of every package in the module.
func packageImports(t *testing.T) map[string][]string {
	t.Helper()
	root := repoRoot(t)

	list := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{join .Imports \",\"}}", "./...")
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	graph := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 2)
		if len(parts) != 2 {
			continue
		}
		var internal []string
		for _, imp := range strings.Split(parts[1], ",") {
			if strings.HasPrefix(imp, modulePath) {
				internal = append(internal, imp)
			}
		}
		graph[parts[0]] = internal
	}
	if len(graph) == 0 {
		t.Fatal("go list returned no packages")
	}
	return graph
}

// TestOnlyTheOMSCanPlaceABrokerOrder is the single most important structural
// assertion in the repository.
//
// The property: exactly one call site in the entire module invokes a broker
// adapter's PlaceOrder. Everything that wants to trade must go through
// internal/oms, which is where the nineteen gates live.
//
// If this fails, a second execution path has been created. That is not a test
// to update — it is the thing the architecture exists to prevent.
func TestOnlyTheOMSCanPlaceABrokerOrder(t *testing.T) {
	root := repoRoot(t)
	adapterCall := regexp.MustCompile(`\badapter\.PlaceOrder\(|\bAdapter\)\.PlaceOrder\(`)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if !adapterCall.Match(body) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		// The OMS is the one permitted caller. The mock venue's own file
		// defines PlaceOrder rather than calling an adapter's.
		if rel == "internal/oms/oms.go" {
			return nil
		}
		offenders = append(offenders, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("a broker adapter's PlaceOrder is called outside internal/oms: %v\n"+
			"Every order must pass the nineteen gates in internal/oms. "+
			"A second path to a venue is the failure this architecture exists to prevent.",
			offenders)
	}
}

// TestBrokerAdapterIsHeldByAnAllowlistOfPackagesOnly restricts who can even
// obtain an adapter.
//
// Calling PlaceOrder requires holding an adapter, so limiting the holders is
// the structural half of the previous test. The allowlist is deliberately
// small and each entry has a reason.
func TestBrokerAdapterIsHeldByAnAllowlistOfPackagesOnly(t *testing.T) {
	permitted := map[string]string{
		modulePath + "/internal/app":         "constructs the registry at start-up",
		modulePath + "/internal/oms":         "the only package that places orders",
		modulePath + "/internal/reconcile":   "reads venue state; the venue is the source of truth",
		modulePath + "/internal/httpapi":     "reports adapter health on the connections endpoint",
		modulePath + "/internal/broker/mock": "implements the interface",
		modulePath + "/internal/broker":      "defines the interface",
	}

	for pkg, imports := range packageImports(t) {
		for _, imp := range imports {
			if imp != modulePath+"/internal/broker" {
				continue
			}
			if _, ok := permitted[pkg]; !ok {
				t.Errorf("package %s imports internal/broker but is not on the allowlist.\n"+
					"Holding an adapter is holding the ability to trade. If this package genuinely "+
					"needs venue state, route it through internal/oms or internal/reconcile instead.", pkg)
			}
		}
	}
}

// TestTheQuantBridgeCannotReachExecution covers the Python boundary from the
// Go side.
//
// internal/quant is the ONLY package that talks to the research service. If it
// imported oms or broker, a research response could be turned into an order
// inside the bridge, without passing anything.
func TestTheQuantBridgeCannotReachExecution(t *testing.T) {
	graph := packageImports(t)
	imports := graph[modulePath+"/internal/quant"]
	if imports == nil {
		t.Fatal("internal/quant was not found in the import graph")
	}

	forbidden := []string{
		modulePath + "/internal/broker",
		modulePath + "/internal/broker/mock",
		modulePath + "/internal/oms",
		modulePath + "/internal/orchestrator",
	}
	for _, imp := range imports {
		for _, bad := range forbidden {
			if imp == bad {
				t.Errorf("internal/quant imports %s.\n"+
					"The research bridge must be able to ASK for a signal and nothing else. "+
					"Turning a research answer into an order is the orchestrator's job, and it "+
					"does so through internal/oms.", imp)
			}
		}
	}
}

// TestDomainHasNoOutwardDependencies keeps the dependency direction honest.
//
// domain holds the rules: money, the order state machine, position maths, the
// audit chain. If it could reach the store, a rule could be satisfied by a
// query, the rules would stop being testable without a database, and a
// circular design would follow.
func TestDomainHasNoOutwardDependencies(t *testing.T) {
	graph := packageImports(t)
	allowed := map[string]bool{
		modulePath + "/internal/money": true,
	}

	for _, imp := range graph[modulePath+"/internal/domain"] {
		if !allowed[imp] {
			t.Errorf("internal/domain imports %s.\n"+
				"domain must depend on nothing but money. A rule that needs I/O to be "+
				"satisfied belongs in a package that is allowed to do I/O.", imp)
		}
	}

	// money is the innermost package and depends on nothing internal at all.
	for _, imp := range graph[modulePath+"/internal/money"] {
		t.Errorf("internal/money imports %s; it must depend on nothing internal", imp)
	}
}

// TestRiskEngineDoesNotReachTheDatabase keeps the risk engine a pure function.
//
// Its purity is what makes a refusal reproducible from a stored decision
// snapshot months later. An engine that could query would produce a verdict
// that depends on the state at replay time rather than at decision time.
func TestRiskEngineDoesNotReachTheDatabase(t *testing.T) {
	graph := packageImports(t)
	for _, imp := range graph[modulePath+"/internal/risk"] {
		if imp == modulePath+"/internal/store" || imp == modulePath+"/internal/db" {
			t.Errorf("internal/risk imports %s.\n"+
				"The risk engine is a pure function of its inputs, which is what makes a "+
				"refusal arguable after the fact. Everything it needs is passed in Input.", imp)
		}
	}
}

// TestNoPackageDependsOnNotify keeps alerting a leaf.
//
// notify imports store, so anything importing notify would gain a transitive
// path to the database. Each producer declares its own narrow Alerter
// interface instead, which is why the wiring lives in internal/app.
func TestNoPackageDependsOnNotify(t *testing.T) {
	graph := packageImports(t)
	permitted := map[string]bool{
		modulePath + "/internal/app":     true,
		modulePath + "/internal/httpapi": true,
		modulePath + "/internal/notify":  true,
	}
	for pkg, imports := range graph {
		if permitted[pkg] {
			continue
		}
		for _, imp := range imports {
			if imp == modulePath+"/internal/notify" {
				t.Errorf("package %s imports internal/notify.\n"+
					"Declare a narrow Alerter interface in the consuming package and let "+
					"internal/app wire it, so notify stays a leaf.", pkg)
			}
		}
	}
}

// TestEveryOrderPlacementGoesThroughTheSameOMSMethod enumerates the callers of
// oms.PlaceOrder and asserts the set is the expected one.
//
// The property this proves is the one the spec asks about directly: an
// automated strategy order and a manual order are the SAME code path. There is
// no "internal" or "fast" variant.
func TestEveryOrderPlacementGoesThroughTheSameOMSMethod(t *testing.T) {
	root := repoRoot(t)
	call := regexp.MustCompile(`\boms\.PlaceOrder\(|\.oms\.PlaceOrder\(`)

	callers := map[string]int{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if n := len(call.FindAll(body, -1)); n > 0 {
			rel, _ := filepath.Rel(root, path)
			callers[filepath.ToSlash(rel)] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	expected := map[string]bool{
		// A manual order, and a manual flatten.
		"internal/httpapi/handlers_trading.go": true,
		// An automated strategy order.
		"internal/orchestrator/orchestrator.go": true,
	}
	for file := range callers {
		if !expected[file] {
			t.Errorf("%s calls oms.PlaceOrder and is not a known entry point.\n"+
				"New entry points are fine, but they must be deliberate: this test is the "+
				"record of how many ways an order can be started.", file)
		}
	}
	for file := range expected {
		if callers[file] == 0 {
			t.Errorf("expected %s to call oms.PlaceOrder and it does not; "+
				"has an execution path moved?", file)
		}
	}
	if callers["internal/orchestrator/orchestrator.go"] == 0 {
		t.Fatal("the orchestrator no longer routes automated orders through the OMS: " +
			"automated trading has acquired its own path")
	}
}

// TestPaperOnlyGuaranteeIsStillCompiledIn re-asserts the build-level gate.
//
// The other three gates are runtime (config validation, the empty registry,
// the database constraints). This one is a constant, and a constant is easy to
// change without noticing.
func TestPaperOnlyGuaranteeIsStillCompiledIn(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "internal", "config", "config.go"))
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	if !strings.Contains(string(body), "BuildAllowsLiveExecution = false") {
		t.Fatal("config.BuildAllowsLiveExecution is no longer a compile-time false. " +
			"This build must not be able to execute live.")
	}

	// And no live adapter has appeared alongside the mock.
	entries, err := os.ReadDir(filepath.Join(root, "internal", "broker"))
	if err != nil {
		t.Fatalf("read broker dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() != "mock" {
			t.Errorf("internal/broker contains an adapter package %q besides mock. "+
				"A live or demo adapter must not be compiled into this build.", e.Name())
		}
	}
}

// TestNoFloatingPointColumnsInTheSchema is the database half of the
// money-is-never-a-float rule.
//
// Go's type system carries the rule in memory: money.Amount wraps a decimal
// and there is no constructor from a float64 in the authoritative path. The
// schema is where that could silently be undone — a single DOUBLE PRECISION
// column would make every value passing through it lossy, and nothing in Go
// would complain.
func TestNoFloatingPointColumnsInTheSchema(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "migrations")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	// Word-boundary matches so "REALLOCATE" or a column named "float_note"
	// does not trip it, and so the message points at the real thing.
	floaty := regexp.MustCompile(`(?i)\b(DOUBLE\s+PRECISION|REAL|FLOAT4|FLOAT8|MONEY)\b`)

	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "--") {
				continue
			}
			if floaty.MatchString(trimmed) {
				t.Errorf("%s declares a floating-point (or MONEY) column:\n  %s\n"+
					"Money, prices, quantities and P&L are NUMERIC. Postgres's MONEY type "+
					"is also excluded: it carries a locale-dependent fractional precision, "+
					"which is not a property a ledger should inherit from a server setting.",
					e.Name(), trimmed)
			}
		}
		found++
	}
	if found == 0 {
		t.Fatal("no migration files were scanned; has the directory moved?")
	}
}

// TestMoneyHasNoFloatConstructor keeps the one type that represents money
// from acquiring a lossy entry point.
func TestMoneyHasNoFloatConstructor(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "internal", "money", "money.go"))
	if err != nil {
		t.Fatalf("read money.go: %v", err)
	}

	// decimal.NewFromFloat is the specific hazard: it accepts a float64 that
	// has already lost precision before the call.
	if strings.Contains(string(body), "NewFromFloat") {
		t.Error("internal/money uses decimal.NewFromFloat.\n" +
			"A money value constructed from a float64 has already lost precision " +
			"before it arrives. Parse from a string instead.")
	}
}
