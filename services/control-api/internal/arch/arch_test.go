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

// readSource concatenates every non-test .go file under a package directory.
//
// Used by the tests that assert a package does NOT mention something. Reading
// the source is cruder than parsing it, and deliberately so: a regex over the
// text catches a reference written any way at all, where an AST walk invites
// arguments about which node types count.
func readSource(t *testing.T, pkgDir string) string {
	t.Helper()
	var b strings.Builder
	dir := filepath.Join(repoRoot(t), filepath.FromSlash(pkgDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", pkgDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		b.Write(body)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		t.Fatalf("no source found in %s; the test would pass vacuously", pkgDir)
	}
	return b.String()
}

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
		// booking imports the package for the ExecutionReport TYPE only. It
		// never holds an Adapter, which the test below asserts separately --
		// so this entry does not widen who can reach a venue.
		modulePath + "/internal/booking": "uses broker.ExecutionReport; holds no adapter",
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

// TestBookingHoldsNoBrokerAdapter is the other half of booking's allowlist
// entry.
//
// booking is on the allowlist because it needs broker.ExecutionReport, a plain
// data type describing an execution the venue reported. That justification
// only holds while booking cannot reach a venue at all, so it is asserted
// rather than assumed: a future change that gave the booking service an
// Adapter field or a Registry would create a second path to a venue, and the
// allowlist entry would silently have permitted it.
func TestBookingHoldsNoBrokerAdapter(t *testing.T) {
	source := readSource(t, "internal/booking")

	for _, forbidden := range []string{
		"broker.Adapter",
		"broker.Registry",
		"broker.NewRegistry",
		".PlaceOrder(",
		".CancelOrder(",
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("internal/booking references %q.\n"+
				"It is on the broker import allowlist ONLY because it uses the "+
				"ExecutionReport data type. Holding an adapter, or calling one, would "+
				"make it a second path to a venue -- which is the thing internal/oms "+
				"exists to be the only one of.", forbidden)
		}
	}
}

// TestOnlyBookingAppendsFills asserts there is exactly one accounting path.
//
// This is the structural guarantee behind the claim that an execution
// discovered by reconciliation is booked identically to one returned by a
// PlaceOrder call. Two implementations could not be kept identical by
// intention; one implementation cannot diverge from itself.
//
// The specific failure this prevents: a reconciliation importer with its own
// INSERT that wrote a fill and a position but skipped the ledger entry. The
// schema would not object -- no constraint ties a position to a transaction --
// and the account would carry a position no money movement explains.
func TestOnlyBookingAppendsFills(t *testing.T) {
	var offenders []string

	err := filepath.Walk(filepath.Join(repoRoot(t), "internal"),
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(body)
			if !strings.Contains(text, "AppendFillTx(") &&
				!strings.Contains(text, "INSERT INTO fills") {
				return nil
			}

			rel, err := filepath.Rel(repoRoot(t), path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			switch rel {
			case "internal/booking/booking.go":
				// The one accounting path.
				return nil
			case "internal/store/trading.go":
				// Defines AppendFillTx. The statement lives with every other
				// SQL statement, which is where it belongs.
				return nil
			}
			offenders = append(offenders, rel)
			return nil
		})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("a fill is appended outside internal/booking: %v\n"+
			"Every execution -- returned by a PlaceOrder call or discovered by "+
			"reconciliation after a lost response -- must go through the one accounting "+
			"path, so the position, ledger and audit invariants hold for both.",
			offenders)
	}
}

// TestEveryReconciliationHandlerUsesTheOperationsScope.
//
// Reconciliation is the incident-response surface, and ADMIN is the incident
// role -- deliberately barred from placing orders, and the only role permitted
// to resolve a divergence. But an admin owns no trading account in this build,
// so a handler scoped by ownership (accountForRequest) answers "Account not
// found" to the operator.
//
// That is not a cosmetic problem: it made the admin-only resolve endpoint
// unreachable, and then made the run endpoint unreachable too, which was found
// only because seven security tests silently SKIPPED for want of an issue to
// act on. A skip reads as a pass.
//
// So the rule is asserted rather than remembered: every handler that takes an
// accountID on a reconciliation or operations route resolves it with
// accountForOperations.
func TestEveryReconciliationHandlerUsesTheOperationsScope(t *testing.T) {
	source := readSource(t, "internal/httpapi")

	// Each handler that serves a reconciliation or operations route.
	handlers := []string{
		"handleRunReconciliation",
		"handleReconciliationStatus",
		"handleReconciliationIssues",
		"handleReconciliationIssue",
		"handleResolveReconciliationIssue",
	}

	for _, name := range handlers {
		start := strings.Index(source, "func (s *Server) "+name+"(")
		if start < 0 {
			t.Errorf("handler %s not found; if it was renamed, update this test rather "+
				"than deleting the assertion", name)
			continue
		}
		// The handler body up to the next top-level func.
		end := strings.Index(source[start+1:], "\nfunc ")
		body := source[start:]
		if end > 0 {
			body = source[start : start+1+end]
		}

		if !strings.Contains(body, "accountForOperations") &&
			strings.Contains(body, "accountID") {
			t.Errorf("%s resolves its account with something other than "+
				"accountForOperations.\n"+
				"An admin owns no trading account, so ownership scoping makes this "+
				"route unreachable for the role that exists to respond to an incident.",
				name)
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
