package reconcile

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/money"
)

// Classifier tests.
//
// The classifier is a pure function of two snapshots, which is the point: every
// branch is reachable here with no database and no venue, including the ones a
// live mock venue almost never produces. The previous audit found five defects
// with integration tests and noted that `handleBrokerError`'s branch table was
// three-sixths covered; this is the level at which that gap closes.
//
// Each test names the real-world scenario it stands for, using the scenario
// letters from the milestone brief where they apply.

var (
	testNow    = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	tolerance  = decimal.RequireFromString("2.00")
	testSymbol = "XAUUSD.m"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// localOrder builds a Vantage order for a snapshot.
func localOrder(status domain.OrderStatus, quantity, filled string, brokerOrderID *string) domain.Order {
	return domain.Order{
		ID:             uuid.New(),
		AccountID:      testAccount().ID,
		InstrumentID:   testSymbol,
		Symbol:         testSymbol,
		Side:           domain.SideBuy,
		Type:           domain.OrderTypeMarket,
		Status:         status,
		Quantity:       dec(quantity),
		FilledQuantity: dec(filled),
		AvgFillPrice:   decimal.Zero,
		CommandID:      uuid.New(),
		BrokerName:     "mock",
		BrokerOrderID:  brokerOrderID,
		CreatedAt:      testNow,
	}
}

var fixedAccountID = uuid.MustParse("11111111-1111-4111-8111-111111111111")

func testAccount() domain.Account {
	return domain.Account{
		ID:         fixedAccountID,
		UserID:     uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Mode:       domain.ModePaper,
		Currency:   money.ZAR,
		BrokerName: "mock",
		Enabled:    true, TradingEnabled: true,
	}
}

// localWith assembles a local snapshot from orders.
func localWith(orders ...domain.Order) LocalSnapshot {
	snap := LocalSnapshot{
		Account:      testAccount(),
		CapturedAt:   testNow,
		Orders:       orders,
		FillIDs:      map[string]bool{},
		BookedFills:  map[string]domain.Fill{},
		FillsByOrder: map[uuid.UUID][]domain.Fill{},
		BalanceKnown: false,
	}
	return snap
}

// execution builds a venue execution report.
func execution(id, brokerOrderID, clientOrderID string, side domain.OrderSide,
	quantity, price string, at time.Time) broker.ExecutionReport {

	return broker.ExecutionReport{
		BrokerFillID:  id,
		BrokerOrderID: brokerOrderID,
		ClientOrderID: clientOrderID,
		Symbol:        testSymbol,
		Side:          side,
		Quantity:      dec(quantity),
		Price:         dec(price),
		Commission:    dec("0.10"),
		CommissionCcy: "USD",
		ExecutedAt:    at,
	}
}

// findingsOfType filters a classification result.
func findingsOfType(findings []Finding, t domain.IssueType) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

func requireOne(t *testing.T, findings []Finding, want domain.IssueType) Finding {
	t.Helper()
	matched := findingsOfType(findings, want)
	if len(matched) != 1 {
		types := make([]string, 0, len(findings))
		for _, f := range findings {
			types = append(types, string(f.Type))
		}
		t.Fatalf("expected exactly one %s finding, got %d; all findings: %v",
			want, len(matched), types)
	}
	return matched[0]
}

func requireNone(t *testing.T, findings []Finding, unwanted domain.IssueType) {
	t.Helper()
	if n := len(findingsOfType(findings, unwanted)); n != 0 {
		t.Fatalf("expected no %s findings, got %d", unwanted, n)
	}
}

// ---------------------------------------------------------------------------
// Scenario C / D: the venue filled and Vantage did not record it
// ---------------------------------------------------------------------------

// TestAnAttributableMissingExecutionIsRepairable is the central recovery case.
//
// Scenario D from the brief: the broker accepted and filled, and the local
// transaction failed after venue execution. The order sits in ACCEPTED with no
// fills, and the venue reports an execution against its broker order id.
func TestAnAttributableMissingExecutionIsRepairable(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderAccepted, "0.01", "0", &venueID)
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			execution("EXEC-1", venueID, order.CommandID.String(),
				domain.SideBuy, "0.01", "2650.00", testNow),
		},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueFillMissingLocally)

	if f.Repair == nil {
		t.Fatal("the finding carries no repair plan, so recovery would stop at detection")
	}
	if f.Repair.Action != domain.ActionImportBrokerFill {
		t.Errorf("repair action is %s; expected IMPORT_BROKER_FILL", f.Repair.Action)
	}
	if f.Repair.Execution == nil || f.Repair.Execution.BrokerFillID != "EXEC-1" {
		t.Error("the repair plan does not carry the execution to import")
	}
	if f.OrderID == nil || *f.OrderID != order.ID {
		t.Error("the finding is not attributed to the order it belongs to")
	}
	if domain.PolicyFor(f.Type).Repair != domain.RepairAutomaticallySafe {
		t.Error("an attributable missing execution is not automatically repairable")
	}
}

// TestAnExecutionIsAttributedByClientOrderIDWhenTheVenueIDIsMissing.
//
// The lost-acknowledgement case: Vantage never recorded a venue order id
// because the response never arrived, so attribution has to work from the
// client id Vantage supplied.
func TestAnExecutionIsAttributedByClientOrderIDWhenTheVenueIDIsMissing(t *testing.T) {
	order := localOrder(domain.OrderAccepted, "0.01", "0", nil)
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			execution("EXEC-2", "MOCK-9", order.CommandID.String(),
				domain.SideBuy, "0.01", "2650.00", testNow),
		},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueFillMissingLocally)
	if f.OrderID == nil || *f.OrderID != order.ID {
		t.Fatal("an execution carrying our own client order id was not attributed to " +
			"the order that supplied it, which is exactly the lost-acknowledgement case")
	}
}

// TestAnUnchangedReplayIsSilent is idempotence at the classification level.
//
// Scenario F. The execution cursor overlaps by a minute on purpose, so every
// poll re-sees recent executions -- this is the normal steady state, not an
// event. An earlier version raised an info issue for each one, which meant a
// five-minute loop created dozens an hour that said nothing and reported them
// as "repaired", making the run report's repair count meaningless.
func TestAnUnchangedReplayIsSilent(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderFilled, "0.01", "0.01", &venueID)
	local := localWith(order)
	local.FillIDs["EXEC-1"] = true
	local.BookedFills["EXEC-1"] = domain.Fill{
		BrokerFillID: "EXEC-1", Side: domain.SideBuy,
		Quantity: dec("0.01"), Price: dec("2650.00"),
	}

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			execution("EXEC-1", venueID, order.CommandID.String(),
				domain.SideBuy, "0.01", "2650.00", testNow),
		},
	}

	findings := Classify(local, remote, tolerance)
	requireNone(t, findings, domain.IssueFillMissingLocally)
	requireNone(t, findings, domain.IssueDuplicateExecutionReport)
	if len(findings) != 0 {
		t.Fatalf("an unchanged replay produced %d findings: %+v", len(findings), findings)
	}
}

// TestAVenueContradictingItselfIsCriticalAndNotRepairable.
//
// The case the DUPLICATE_EXECUTION_REPORT type now exists for: the venue
// re-reports an execution id with a different quantity. The unique index
// refused the second copy, so nothing was written twice -- but one of the two
// records is wrong about a trade that happened, and there is nothing safe to
// write either way.
func TestAVenueContradictingItselfIsCriticalAndNotRepairable(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderFilled, "0.02", "0.01", &venueID)
	local := localWith(order)
	local.FillIDs["EXEC-1"] = true
	local.BookedFills["EXEC-1"] = domain.Fill{
		BrokerFillID: "EXEC-1", Side: domain.SideBuy,
		Quantity: dec("0.01"), Price: dec("2650.00"),
	}

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			// Same id, different quantity.
			execution("EXEC-1", venueID, order.CommandID.String(),
				domain.SideBuy, "0.02", "2650.00", testNow),
		},
	}

	f := requireOne(t, Classify(local, remote, tolerance),
		domain.IssueDuplicateExecutionReport)
	if f.Severity != domain.SeverityCriticalIssue {
		t.Errorf("a venue contradicting its own execution report is %s severity", f.Severity)
	}
	if f.Repair != nil {
		t.Fatal("a contradicted execution carries a repair plan. Booking the new version " +
			"doubles the position; overwriting the old one edits an append-only fill")
	}
	// Both versions must be in the evidence, or an operator cannot see the
	// contradiction they are being asked to resolve.
	if f.LocalEvidence["quantity"] != "0.01" {
		t.Errorf("the booked quantity is missing from the evidence: %v", f.LocalEvidence)
	}
	if f.BrokerEvidence["quantity"] != "0.02" {
		t.Errorf("the re-reported quantity is missing from the evidence: %v", f.BrokerEvidence)
	}
}

// ---------------------------------------------------------------------------
// Scenario J: an execution with no safe local match
// ---------------------------------------------------------------------------

// TestAnUnattributableExecutionIsNeverAutomaticallyRepaired is the milestone's
// separate acceptance criterion.
func TestAnUnattributableExecutionIsNeverAutomaticallyRepaired(t *testing.T) {
	local := localWith() // Vantage has no orders at all.

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			execution("EXEC-ORPHAN", "MOCK-UNKNOWN", "",
				domain.SideBuy, "0.05", "2650.00", testNow),
		},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueExtraBrokerFill)
	if f.Repair != nil {
		t.Fatal("an unattributable execution carries a repair plan. Importing it would " +
			"require guessing a parent order, which writes a real position and a real " +
			"profit or loss against the wrong one")
	}
	if domain.PolicyFor(f.Type).Repair == domain.RepairAutomaticallySafe {
		t.Fatal("EXTRA_BROKER_FILL is classified as automatically safe")
	}
	if f.Severity != domain.SeverityCriticalIssue {
		t.Errorf("an unattributable execution is %s severity; expected critical", f.Severity)
	}
}

// TestAnAmbiguousExecutionIsNotResolvedByTakingTheFirstMatch.
//
// Two local orders hold the same venue order id -- which should not happen, and
// is precisely why it must not be resolved silently. Returning the first match
// would turn an ambiguous attribution into a confident wrong one.
func TestAnAmbiguousExecutionIsNotResolvedByTakingTheFirstMatch(t *testing.T) {
	venueID := "MOCK-DUPLICATE"
	first := localOrder(domain.OrderAccepted, "0.01", "0", &venueID)
	second := localOrder(domain.OrderAccepted, "0.01", "0", &venueID)
	local := localWith(first, second)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			execution("EXEC-AMBIG", venueID, "", domain.SideBuy, "0.01", "2650.00", testNow),
		},
	}

	findings := Classify(local, remote, tolerance)
	requireNone(t, findings, domain.IssueFillMissingLocally)

	f := requireOne(t, findings, domain.IssueExtraBrokerFill)
	if f.Repair != nil {
		t.Fatal("an execution matching two orders carries a repair plan")
	}
	ids, ok := f.Evidence["candidate_order_ids"].([]string)
	if !ok || len(ids) != 2 {
		t.Errorf("the evidence does not name both candidate orders, so an operator "+
			"cannot see what the ambiguity is: %v", f.Evidence)
	}
}

// TestAnExecutionOnTheWrongSideIsNotAttributed.
//
// A venue reporting the opposite side for an order id we recognise is
// describing a mapping error. Booking it would move the position the wrong
// way, so refusing to attribute it turns a silent loss into a visible issue.
func TestAnExecutionOnTheWrongSideIsNotAttributed(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderAccepted, "0.01", "0", &venueID) // a BUY
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			execution("EXEC-SELL", venueID, order.CommandID.String(),
				domain.SideSell, "0.01", "2650.00", testNow),
		},
	}

	findings := Classify(local, remote, tolerance)
	requireNone(t, findings, domain.IssueFillMissingLocally)
	if len(findingsOfType(findings, domain.IssueExtraBrokerFill)) != 1 {
		t.Fatal("a sell execution reported against a buy order was attributed to it, " +
			"or was not reported at all")
	}
}

// TestAnExecutionWithNoIdentifierCanNeverBeBooked.
//
// Without an execution id, deduplication is impossible: a second sighting
// could not be told from a second trade. There is no safe way to book it.
func TestAnExecutionWithNoIdentifierCanNeverBeBooked(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderAccepted, "0.01", "0", &venueID)
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			execution("", venueID, order.CommandID.String(),
				domain.SideBuy, "0.01", "2650.00", testNow),
		},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueExtraBrokerFill)
	if f.Repair != nil {
		t.Fatal("an execution with no identifier carries a repair plan; booking it would " +
			"make every replay indistinguishable from a second trade")
	}
}

// ---------------------------------------------------------------------------
// Scenario G: out-of-order execution reports
// ---------------------------------------------------------------------------

func TestOutOfOrderExecutionsAreRecordedButHarmless(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderAccepted, "0.02", "0", &venueID)
	local := localWith(order)

	later := execution("EXEC-LATER", venueID, order.CommandID.String(),
		domain.SideBuy, "0.01", "2650.00", testNow)
	earlier := execution("EXEC-EARLIER", venueID, order.CommandID.String(),
		domain.SideBuy, "0.01", "2649.00", testNow.Add(-time.Minute))

	remote := BrokerSnapshot{
		FetchedAt:  testNow,
		Executions: []broker.ExecutionReport{later, earlier},
	}

	findings := Classify(local, remote, tolerance)

	f := requireOne(t, findings, domain.IssueOutOfOrderExecutionReport)
	if f.Severity != domain.SeverityInfoIssue {
		t.Errorf("an out-of-order execution is %s severity. Booking is commutative -- "+
			"the fill aggregate is recomputed from the fills rather than incremented -- "+
			"so arrival order cannot change the result", f.Severity)
	}
	if domain.PolicyFor(f.Type).Halt != domain.HaltNone {
		t.Error("an out-of-order execution halts automation; it is harmless by construction")
	}
	// Both executions are still importable: order does not affect the outcome.
	if n := len(findingsOfType(findings, domain.IssueFillMissingLocally)); n != 2 {
		t.Errorf("expected both executions to be importable, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Scenario I: a local order the venue has never heard of
// ---------------------------------------------------------------------------

// TestAnOrderWithNoVenueIDTheVenueDeniesIsClosedOut.
//
// Provable: the order holds no venue identifier and a direct lookup by client
// id found nothing, so it never reached the market. This is the case the
// previous audit found the hard way, when six such orders permanently consumed
// an account's pending-order budget.
func TestAnOrderWithNoVenueIDTheVenueDeniesIsClosedOut(t *testing.T) {
	order := localOrder(domain.OrderAccepted, "0.01", "0", nil)
	local := localWith(order)
	remote := BrokerSnapshot{FetchedAt: testNow}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueOrderMissingAtBroker)
	if f.Repair == nil || f.Repair.Action != domain.ActionMarkNotExecuted {
		t.Fatal("an order that provably never reached the market is not closed out, so it " +
			"would consume the pending-order budget forever")
	}
	if f.Repair.TargetStatus != domain.OrderRejected {
		t.Errorf("target status is %s; expected REJECTED", f.Repair.TargetStatus)
	}
	if f.Repair.RejectCode != domain.RejectReconciledAbsent {
		t.Errorf("reject code is %q; expected reconciled_absent_at_venue so the reason is "+
			"machine-readable", f.Repair.RejectCode)
	}
}

// TestAnOrderHoldingAVenueIDTheVenueDeniesIsNotClosedOut.
//
// The dangerous mirror image. Closing this out would free the risk budget for
// a position that may well exist, so it is a VENUE_ID_MISMATCH and halts the
// whole connection: every other repair depends on that mapping.
func TestAnOrderHoldingAVenueIDTheVenueDeniesIsNotClosedOut(t *testing.T) {
	venueID := "MOCK-GHOST"
	order := localOrder(domain.OrderSubmitted, "0.01", "0", &venueID)
	local := localWith(order)
	remote := BrokerSnapshot{FetchedAt: testNow}

	findings := Classify(local, remote, tolerance)
	requireNone(t, findings, domain.IssueOrderMissingAtBroker)

	f := requireOne(t, findings, domain.IssueVenueIDMismatch)
	if f.Repair != nil {
		t.Fatal("an order whose venue id the venue denies is repaired automatically. " +
			"Closing it out would release risk budget for a position that may exist")
	}
	if domain.PolicyFor(f.Type).Halt != domain.HaltBrokerConnection {
		t.Error("an identifier mismatch does not halt the connection, yet every repair " +
			"on that connection relies on the mapping being sound")
	}
}

// ---------------------------------------------------------------------------
// Scenario H: the venue says cancelled, Vantage says submitted
// ---------------------------------------------------------------------------

func TestAStatusDifferenceWithAgreeingQuantitiesIsAdopted(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderSubmitted, "0.01", "0", &venueID)
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders: []broker.VenueOrder{{
			BrokerOrderID: venueID, ClientOrderID: order.CommandID.String(),
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueCancelled,
			Quantity: dec("0.01"), FilledQuantity: decimal.Zero,
		}},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueOrderStatusMismatch)
	if f.Repair == nil {
		t.Fatal("the venue is authoritative about its own order states and the quantities " +
			"agree, so this should be adopted")
	}
	if f.Repair.TargetStatus != domain.OrderCancelled {
		t.Errorf("target status is %s; expected CANCELLED", f.Repair.TargetStatus)
	}
}

// TestAStatusDifferenceWithDISAGREEINGQuantitiesIsAMissingExecution.
//
// The distinction that makes automatic status repair safe at all. Adopting
// FILLED while the quantities disagree would leave an order marked filled with
// no executions behind it, and the invariant "position quantity equals
// fill-derived quantity" would stop holding.
func TestAStatusDifferenceWithDisagreeingQuantitiesIsAMissingExecution(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderSubmitted, "0.01", "0", &venueID)
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders: []broker.VenueOrder{{
			BrokerOrderID: venueID, ClientOrderID: order.CommandID.String(),
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueFilled,
			Quantity: dec("0.01"), FilledQuantity: dec("0.01"),
			AvgFillPrice: dec("2650.00"),
		}},
		// Deliberately NO execution report, so nothing is attributable.
	}

	findings := Classify(local, remote, tolerance)
	requireNone(t, findings, domain.IssueOrderStatusMismatch)

	f := requireOne(t, findings, domain.IssuePartialFillMismatch)
	if f.Repair != nil {
		t.Fatal("a status difference with disagreeing quantities carries a repair plan. " +
			"Relabelling the order would leave it FILLED with no fills behind it")
	}
	if domain.PolicyFor(f.Type).Repair == domain.RepairAutomaticallySafe {
		t.Fatal("PARTIAL_FILL_MISMATCH is automatically repairable")
	}
}

func TestAVenueStatusThatContradictsATerminalStateIsCritical(t *testing.T) {
	venueID := "MOCK-1"
	// Vantage has it FILLED; the venue says cancelled. One of the two records
	// is wrong about a terminal outcome.
	order := localOrder(domain.OrderFilled, "0.01", "0.01", &venueID)
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders: []broker.VenueOrder{{
			BrokerOrderID: venueID, ClientOrderID: order.CommandID.String(),
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueCancelled,
			Quantity: dec("0.01"), FilledQuantity: dec("0.01"),
		}},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueOrderStatusMismatch)
	if f.Severity != domain.SeverityCriticalIssue {
		t.Errorf("severity is %s; a venue contradicting a terminal state Vantage recorded "+
			"is critical", f.Severity)
	}
	if f.Repair != nil {
		t.Fatal("reconciliation would overwrite a terminal state")
	}
}

// ---------------------------------------------------------------------------
// External activity, and orders Vantage never placed
// ---------------------------------------------------------------------------

// TestAVenueOrderWithNoClientIDIsExternalActivity.
//
// Vantage supplies a client order id on every order it places, so a venue
// order without one did not originate here. Reporting it as external activity
// rather than adopting it is the difference between an honest record and a
// fabricated one.
func TestAVenueOrderWithNoClientIDIsExternalActivity(t *testing.T) {
	local := localWith()
	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders: []broker.VenueOrder{{
			BrokerOrderID: "MOCK-MANUAL", ClientOrderID: "",
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueWorking,
			Quantity: dec("0.10"),
		}},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueExternalBrokerActivity)
	if domain.PolicyFor(f.Type).Repair != domain.RepairUnresolvableAutomatically {
		t.Error("external broker activity is not classified as unresolvable; Vantage must " +
			"never record a trade it did not place as though it had")
	}
}

// TestAVenueOrderWithAClientIDVantageDoesNotKnowIsMissingLocally.
//
// It carries one of our client ids, so it probably WAS ours and the record was
// lost. Still not adoptable automatically: there is no risk decision,
// authority check or intent to attach it to.
func TestAVenueOrderWithAClientIDVantageDoesNotKnowIsMissingLocally(t *testing.T) {
	local := localWith()
	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders: []broker.VenueOrder{{
			BrokerOrderID: "MOCK-LOST", ClientOrderID: uuid.New().String(),
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueWorking,
			Quantity: dec("0.01"),
		}},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueOrderMissingLocally)
	if f.Repair != nil {
		t.Fatal("an order Vantage has no record of carries a repair plan")
	}
	if !domain.ActionAllowed(f.Type, domain.ActionLinkBrokerOrder) {
		t.Error("an operator cannot LINK this to a Vantage order, which is the one " +
			"judgement that could resolve it")
	}
}

// ---------------------------------------------------------------------------
// Scenario K: position mismatch
// ---------------------------------------------------------------------------

func TestAPositionMismatchIsNeverRepairedAutomatically(t *testing.T) {
	local := localWith()
	local.Positions = []domain.Position{{
		ID: uuid.New(), AccountID: fixedAccountID, InstrumentID: testSymbol,
		Side: domain.SideBuy, Quantity: dec("0.05"), Status: domain.PositionOpen,
		AvgEntryPrice: dec("2650.00"),
	}}

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Positions: []broker.VenuePosition{{
			Symbol: testSymbol, Side: domain.SideBuy, Quantity: dec("0.08"),
			AvgEntryPrice: dec("2650.00"),
		}},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssuePositionMismatch)
	if f.Repair != nil {
		t.Fatal("a position mismatch carries a repair plan. A position is the consequence " +
			"of executions, never an input: writing one to match the venue would leave a " +
			"position no sequence of fills explains")
	}
	if f.Severity != domain.SeverityCriticalIssue {
		t.Errorf("a position mismatch is %s severity; risk sizing is computed from this",
			f.Severity)
	}
}

func TestAPositionTheVenueDoesNotHoldIsReported(t *testing.T) {
	local := localWith()
	local.Positions = []domain.Position{{
		ID: uuid.New(), AccountID: fixedAccountID, InstrumentID: testSymbol,
		Side: domain.SideBuy, Quantity: dec("0.05"), Status: domain.PositionOpen,
	}}
	remote := BrokerSnapshot{FetchedAt: testNow}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssuePositionMismatch)
	if f.PositionID == nil {
		t.Error("the finding does not name the position, so an operator cannot find it")
	}
}

func TestAgreeingPositionBooksProduceNoFinding(t *testing.T) {
	local := localWith()
	local.Positions = []domain.Position{{
		ID: uuid.New(), AccountID: fixedAccountID, InstrumentID: testSymbol,
		Side: domain.SideBuy, Quantity: dec("0.05"), Status: domain.PositionOpen,
	}}
	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Positions: []broker.VenuePosition{{
			Symbol: testSymbol, Side: domain.SideBuy, Quantity: dec("0.05"),
		}},
	}
	if findings := Classify(local, remote, tolerance); len(findings) != 0 {
		t.Fatalf("agreeing snapshots produced %d findings: %+v", len(findings), findings)
	}
}

// ---------------------------------------------------------------------------
// Scenario L: balance mismatch
// ---------------------------------------------------------------------------

func TestABalanceWithinToleranceIsNotAFinding(t *testing.T) {
	local := localWith()
	local.LedgerBalance = dec("500.00")
	local.BalanceKnown = true

	remote := BrokerSnapshot{
		FetchedAt: testNow, Balances: true,
		Account: broker.VenueAccount{Currency: "ZAR", Balance: dec("501.50")},
	}
	requireNone(t, Classify(local, remote, tolerance), domain.IssueBalanceMismatch)
}

func TestABalanceBeyondToleranceIsReportedButDoesNotHalt(t *testing.T) {
	local := localWith()
	local.LedgerBalance = dec("500.00")
	local.BalanceKnown = true

	remote := BrokerSnapshot{
		FetchedAt: testNow, Balances: true,
		Account: broker.VenueAccount{Currency: "ZAR", Balance: dec("420.00")},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueBalanceMismatch)
	if domain.PolicyFor(f.Type).Halt != domain.HaltNone {
		t.Error("a balance mismatch halts automation. In paper mode the venue computes " +
			"its own balance, so this would halt permanently by design")
	}
	if f.Repair != nil {
		t.Fatal("a balance mismatch carries a repair plan. A balance is the running total " +
			"of an append-only ledger; there is no way to correct it directly")
	}
}

func TestACurrencyMismatchIsCriticalRatherThanConverted(t *testing.T) {
	local := localWith()
	local.LedgerBalance = dec("500.00")
	local.BalanceKnown = true

	remote := BrokerSnapshot{
		FetchedAt: testNow, Balances: true,
		Account: broker.VenueAccount{Currency: "USD", Balance: dec("500.00")},
	}

	f := requireOne(t, Classify(local, remote, tolerance), domain.IssueBalanceMismatch)
	if f.Severity != domain.SeverityCriticalIssue {
		t.Errorf("a currency mismatch is %s severity; converting one to the other would "+
			"hide a configuration error behind an exchange rate", f.Severity)
	}
}

func TestAnUnknownBalanceIsNotComparedAgainstZero(t *testing.T) {
	local := localWith()
	local.BalanceKnown = false
	remote := BrokerSnapshot{FetchedAt: testNow, Balances: false}
	requireNone(t, Classify(local, remote, tolerance), domain.IssueBalanceMismatch)
}

// ---------------------------------------------------------------------------
// Unknown execution state
// ---------------------------------------------------------------------------

// TestAnUncertainOrderTheVenueCannotResolveStaysUnknown.
//
// The state the brief insists must not be tidied away: recording it as
// rejected would free risk budget for a position that may exist, and recording
// it as filled would invent one that may not.
func TestAnUncertainOrderTheVenueCannotResolveStaysUnknown(t *testing.T) {
	venueID := "MOCK-UNRESOLVED"
	order := localOrder(domain.OrderFailed, "0.01", "0", &venueID)
	order.ReconciliationRequired = true
	local := localWith(order)
	remote := BrokerSnapshot{FetchedAt: testNow}

	findings := Classify(local, remote, tolerance)

	f := requireOne(t, findings, domain.IssueUnknownExecutionState)
	if f.Repair != nil {
		t.Fatal("an unknown execution state carries a repair plan; both tidy answers are " +
			"wrong in a way that costs money")
	}
	if domain.PolicyFor(f.Type).Halt != domain.HaltAccount {
		t.Error("an unknown execution state does not halt the account")
	}
	// It must not ALSO be reported as a plain missing order, which would
	// double-count and offer an unsafe automatic repair.
	requireNone(t, findings, domain.IssueOrderMissingAtBroker)
}

func TestAnUncertainOrderTheVenueDoesKnowIsClassifiedSpecifically(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderFailed, "0.01", "0", &venueID)
	order.ReconciliationRequired = true
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders: []broker.VenueOrder{{
			BrokerOrderID: venueID, ClientOrderID: order.CommandID.String(),
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueCancelled,
			Quantity: dec("0.01"), FilledQuantity: decimal.Zero,
		}},
	}

	findings := Classify(local, remote, tolerance)
	requireNone(t, findings, domain.IssueUnknownExecutionState)
	requireOne(t, findings, domain.IssueOrderStatusMismatch)
}

// ---------------------------------------------------------------------------
// Fingerprints: the property that makes a 5-minute loop usable
// ---------------------------------------------------------------------------

// TestFingerprintsAreStableAcrossRuns.
//
// Reconciliation runs on a schedule, so the same divergence is detected
// repeatedly. If the fingerprint included an observed value or a timestamp,
// every re-detection would be a new issue and the operator view would hold
// hundreds of rows for one problem within an hour.
func TestFingerprintsAreStableAcrossRuns(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderSubmitted, "0.02", "0", &venueID)
	local := localWith(order)

	makeRemote := func(filled string, at time.Time) BrokerSnapshot {
		return BrokerSnapshot{
			FetchedAt: at,
			Orders: []broker.VenueOrder{{
				BrokerOrderID: venueID, ClientOrderID: order.CommandID.String(),
				Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueWorking,
				Quantity: dec("0.02"), FilledQuantity: dec(filled),
			}},
		}
	}

	first := requireOne(t, Classify(local, makeRemote("0.01", testNow), tolerance),
		domain.IssuePartialFillMismatch)
	// A later run, a different observed quantity, a different timestamp.
	second := requireOne(t,
		Classify(local, makeRemote("0.015", testNow.Add(time.Hour)), tolerance),
		domain.IssuePartialFillMismatch)

	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("the same problem produced two fingerprints (%s vs %s). A scheduled run "+
			"would create a new issue every five minutes",
			first.Fingerprint, second.Fingerprint)
	}
}

func TestDifferentProblemsGetDifferentFingerprints(t *testing.T) {
	a := localOrder(domain.OrderAccepted, "0.01", "0", nil)
	b := localOrder(domain.OrderAccepted, "0.01", "0", nil)
	local := localWith(a, b)
	remote := BrokerSnapshot{FetchedAt: testNow}

	findings := findingsOfType(Classify(local, remote, tolerance),
		domain.IssueOrderMissingAtBroker)
	if len(findings) != 2 {
		t.Fatalf("expected two findings, got %d", len(findings))
	}
	if findings[0].Fingerprint == findings[1].Fingerprint {
		t.Fatal("two different orders produced the same fingerprint, so the partial " +
			"unique index would collapse them into one issue and hide one of the problems")
	}
}

// ---------------------------------------------------------------------------
// Whole-snapshot properties
// ---------------------------------------------------------------------------

func TestAgreeingSnapshotsProduceNothing(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderSubmitted, "0.01", "0", &venueID)
	local := localWith(order)
	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders: []broker.VenueOrder{{
			BrokerOrderID: venueID, ClientOrderID: order.CommandID.String(),
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueWorking,
			Quantity: dec("0.01"), FilledQuantity: decimal.Zero,
		}},
	}
	if findings := Classify(local, remote, tolerance); len(findings) != 0 {
		t.Fatalf("agreeing snapshots produced %d findings: %+v", len(findings), findings)
	}
}

// TestClassificationIsDeterministic.
//
// A run writes to the ledger. Given the same divergence it must reach the same
// conclusion every time, or a repair applied on one run and withheld on the
// next would be indistinguishable from a real change in venue state.
func TestClassificationIsDeterministic(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderAccepted, "0.01", "0", &venueID)
	local := localWith(order)
	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Executions: []broker.ExecutionReport{
			execution("EXEC-1", venueID, order.CommandID.String(),
				domain.SideBuy, "0.01", "2650.00", testNow),
		},
	}

	first := Classify(local, remote, tolerance)
	for i := 0; i < 20; i++ {
		again := Classify(local, remote, tolerance)
		if len(again) != len(first) {
			t.Fatalf("run %d produced %d findings; the first produced %d",
				i, len(again), len(first))
		}
		for j := range first {
			if again[j].Type != first[j].Type ||
				again[j].Fingerprint != first[j].Fingerprint {
				t.Fatalf("run %d finding %d differs: %s/%s vs %s/%s",
					i, j, again[j].Type, again[j].Fingerprint,
					first[j].Type, first[j].Fingerprint)
			}
		}
	}
}

// TestEveryFindingCarriesAFingerprintAndADescription.
//
// A finding with no fingerprint cannot be deduplicated and would be inserted
// afresh every run; one with no description leaves the operator nothing to act
// on. Both are cheap to get wrong when adding a new check.
func TestEveryFindingCarriesAFingerprintAndADescription(t *testing.T) {
	venueID := "MOCK-1"
	orphan := localOrder(domain.OrderAccepted, "0.01", "0", nil)
	ghost := localOrder(domain.OrderSubmitted, "0.01", "0", &venueID)
	local := localWith(orphan, ghost)
	local.Positions = []domain.Position{{
		ID: uuid.New(), AccountID: fixedAccountID, InstrumentID: testSymbol,
		Side: domain.SideBuy, Quantity: dec("0.05"), Status: domain.PositionOpen,
	}}
	local.LedgerBalance = dec("500.00")
	local.BalanceKnown = true

	remote := BrokerSnapshot{
		FetchedAt: testNow, Balances: true,
		Account: broker.VenueAccount{Currency: "ZAR", Balance: dec("100.00")},
		Orders: []broker.VenueOrder{{
			BrokerOrderID: "MOCK-EXTERNAL", ClientOrderID: "",
			Symbol: testSymbol, Side: domain.SideSell, Status: broker.VenueWorking,
			Quantity: dec("0.01"),
		}},
		Executions: []broker.ExecutionReport{
			execution("EXEC-ORPHAN", "MOCK-NOBODY", "", domain.SideBuy,
				"0.01", "2650.00", testNow),
		},
	}

	findings := Classify(local, remote, tolerance)
	if len(findings) < 5 {
		t.Fatalf("expected findings across several checks, got %d", len(findings))
	}
	for _, f := range findings {
		if f.Fingerprint == "" {
			t.Errorf("%s finding has no fingerprint, so it would be re-inserted every run",
				f.Type)
		}
		if len(f.Description) < 40 {
			t.Errorf("%s finding has a %d-character description; the operator has to act "+
				"on this", f.Type, len(f.Description))
		}
		if !f.Type.Valid() {
			t.Errorf("%s is not a classified issue type", f.Type)
		}
		// A repair plan on a type the policy calls unsafe would be applied by
		// persistAndRepair, so the two must never disagree.
		if f.Repair != nil && domain.PolicyFor(f.Type).Repair != domain.RepairAutomaticallySafe {
			t.Errorf("%s carries a repair plan but its policy is %s; the plan would be "+
				"ignored, or worse, applied", f.Type, domain.PolicyFor(f.Type).Repair)
		}
	}
}

// TestAFilledOrderAbsentFromTheOpenOrdersListIsNotCalledMissing.
//
// A defect this milestone found and fixed.
//
// FetchOpenOrders returns only OPEN orders, which is the correct contract. But
// an order the venue FILLED is no longer open, so it is absent from that list
// -- and the classifier concluded "the venue never heard of this, mark it not
// executed". That releases the risk budget for a position that exists.
//
// It happened on a real lost-response order: the venue held it as filled with
// an execution against it, and reconciliation raised ORDER_MISSING_AT_BROKER.
// Only the repair machine's terminal-state guard stopped it being marked
// rejected after the fill had already been imported.
//
// Two defences, both asserted here: captureBroker resolves such orders by a
// direct client-id lookup, and the classifier independently refuses to call an
// order missing when an execution references it.
func TestAFilledOrderAbsentFromTheOpenOrdersListIsNotCalledMissing(t *testing.T) {
	order := localOrder(domain.OrderFailed, "0.01", "0", nil)
	order.ReconciliationRequired = true
	local := localWith(order)

	// The venue's OPEN orders list is empty, because the order filled. The
	// execution is the only evidence that it ever reached the market.
	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders:    nil,
		Executions: []broker.ExecutionReport{
			execution("EXEC-FILLED", "MOCK-7", order.CommandID.String(),
				domain.SideBuy, "0.01", "2650.00", testNow),
		},
	}

	findings := Classify(local, remote, tolerance)

	requireNone(t, findings, domain.IssueOrderMissingAtBroker)
	requireNone(t, findings, domain.IssueUnknownExecutionState)

	// What it IS: an execution to import.
	f := requireOne(t, findings, domain.IssueFillMissingLocally)
	if f.Repair == nil || f.Repair.Action != domain.ActionImportBrokerFill {
		t.Fatal("the execution proving the order reached the market is not importable")
	}
}

// TestAnOrderWithABookedFillIsNotCalledMissing.
//
// The same protection where the execution predates the polling window: Vantage
// would not hold a fill for an order the venue never accepted, so a booked
// fill is equally good proof that the order reached the market.
func TestAnOrderWithABookedFillIsNotCalledMissing(t *testing.T) {
	order := localOrder(domain.OrderPartiallyFilled, "0.02", "0.01", nil)
	local := localWith(order)
	local.FillsByOrder[order.ID] = []domain.Fill{{
		OrderID: order.ID, BrokerFillID: "EXEC-OLD",
		Quantity: dec("0.01"), Price: dec("2650.00"),
	}}
	local.FillIDs["EXEC-OLD"] = true

	// Empty venue snapshot: no open order, no execution in the window.
	remote := BrokerSnapshot{FetchedAt: testNow}

	findings := Classify(local, remote, tolerance)
	requireNone(t, findings, domain.IssueOrderMissingAtBroker)
}

// TestOneProblemProducesOneIssue.
//
// A missing execution makes the filled quantities disagree, so a naive
// classifier reports BOTH: FILL_MISSING_LOCALLY, which says "this is safe to
// import automatically", and PARTIAL_FILL_MISMATCH, which says "nothing
// specific to book, review it yourself". Those are contradictory instructions
// for one problem, and the second also kept the order flagged as uncertain
// after the import had resolved it.
func TestOneProblemProducesOneIssue(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderSubmitted, "0.01", "0", &venueID)
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		// The venue says filled, and supplies the execution that explains it.
		Orders: []broker.VenueOrder{{
			BrokerOrderID: venueID, ClientOrderID: order.CommandID.String(),
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueFilled,
			Quantity: dec("0.01"), FilledQuantity: dec("0.01"),
			AvgFillPrice: dec("2650.00"),
		}},
		Executions: []broker.ExecutionReport{
			execution("EXEC-1", venueID, order.CommandID.String(),
				domain.SideBuy, "0.01", "2650.00", testNow),
		},
	}

	findings := Classify(local, remote, tolerance)

	requireOne(t, findings, domain.IssueFillMissingLocally)
	requireNone(t, findings, domain.IssuePartialFillMismatch)
}

// TestAnUnexplainedQuantityDifferenceIsStillReported.
//
// The mirror of the test above: suppressing PARTIAL_FILL_MISMATCH must depend
// on there being an execution that actually explains the difference. Without
// one there is nothing to import, and the operator must be told.
func TestAnUnexplainedQuantityDifferenceIsStillReported(t *testing.T) {
	venueID := "MOCK-1"
	order := localOrder(domain.OrderSubmitted, "0.02", "0", &venueID)
	local := localWith(order)

	remote := BrokerSnapshot{
		FetchedAt: testNow,
		Orders: []broker.VenueOrder{{
			BrokerOrderID: venueID, ClientOrderID: order.CommandID.String(),
			Symbol: testSymbol, Side: domain.SideBuy, Status: broker.VenueWorking,
			Quantity: dec("0.02"), FilledQuantity: dec("0.01"),
		}},
		// No execution report: the venue reports a total it will not decompose.
	}

	findings := Classify(local, remote, tolerance)
	requireNone(t, findings, domain.IssueFillMissingLocally)
	f := requireOne(t, findings, domain.IssuePartialFillMismatch)
	if f.Repair != nil {
		t.Fatal("an unexplained quantity difference carries a repair plan")
	}
}
