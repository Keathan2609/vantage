package booking

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/money"
)

// Validation tests for the single accounting path.
//
// Every check here is also enforced by a database constraint, and the
// constraints stay. The reason to check first is diagnostic: a constraint
// violation arrives as an aborted transaction naming a constraint, which
// during a reconciliation run also rolls back the repair of every other issue
// in the same unit of work. Checking first turns "the transaction failed" into
// "this execution claims to be a buy against a sell order".

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

var (
	accountID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	orderID   = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	now       = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
)

func testAccount() domain.Account {
	return domain.Account{
		ID: accountID, Mode: domain.ModePaper, Currency: money.ZAR,
		BrokerName: "mock", Enabled: true, TradingEnabled: true,
	}
}

func testInstrument() domain.Instrument {
	return domain.Instrument{ID: "XAUUSD.m", Symbol: "XAUUSD.m", QuoteCcy: money.USD}
}

func testOrder() domain.Order {
	return domain.Order{
		ID: orderID, AccountID: accountID, InstrumentID: "XAUUSD.m",
		Symbol: "XAUUSD.m", Side: domain.SideBuy, Type: domain.OrderTypeMarket,
		Status: domain.OrderSubmitted, Quantity: dec("0.01"),
		FilledQuantity: decimal.Zero, BrokerName: "mock",
	}
}

func validRequest() Request {
	return Request{
		Account:    testAccount(),
		Instrument: testInstrument(),
		Order:      testOrder(),
		Execution: broker.ExecutionReport{
			BrokerFillID: "EXEC-1", BrokerOrderID: "MOCK-1",
			Symbol: "XAUUSD.m", Side: domain.SideBuy,
			Quantity: dec("0.01"), Price: dec("2650.00"),
			Commission: dec("0.10"), CommissionCcy: "USD", ExecutedAt: now,
		},
		Source: SourceExecutionResponse,
		Now:    now,
	}
}

func TestAValidExecutionPassesValidation(t *testing.T) {
	if err := validRequest().Validate(); err != nil {
		t.Fatalf("a well-formed execution was refused: %v", err)
	}
}

// TestAnExecutionWithNoIdentifierIsRefused.
//
// The single most important check. Without an execution id the unique index
// has nothing to key on, so deduplication is impossible and every replay of a
// venue stream becomes a second position.
func TestAnExecutionWithNoIdentifierIsRefused(t *testing.T) {
	req := validRequest()
	req.Execution.BrokerFillID = ""
	if err := req.Validate(); !errors.Is(err, ErrNoBrokerExecutionID) {
		t.Fatalf("expected ErrNoBrokerExecutionID, got %v. Without an identifier a "+
			"replayed execution could not be told from a second trade", err)
	}
}

func TestAnExecutionForADifferentAccountIsRefused(t *testing.T) {
	req := validRequest()
	req.Order.AccountID = uuid.New()
	if err := req.Validate(); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("expected ErrAccountMismatch, got %v. Booking an execution against "+
			"another account's order would move money between accounts", err)
	}
}

func TestAnExecutionForADifferentInstrumentIsRefused(t *testing.T) {
	req := validRequest()
	req.Instrument = domain.Instrument{ID: "EURUSD", Symbol: "EURUSD", QuoteCcy: money.USD}
	if err := req.Validate(); !errors.Is(err, ErrInstrumentMismatch) {
		t.Fatalf("expected ErrInstrumentMismatch, got %v", err)
	}
}

// TestAnExecutionOnTheOppositeSideIsRefused.
//
// A venue reporting the wrong side for a known order id is describing a
// mapping error. Booking it would move the position the wrong way, turning a
// visible issue into a silent loss.
func TestAnExecutionOnTheOppositeSideIsRefused(t *testing.T) {
	req := validRequest()
	req.Execution.Side = domain.SideSell
	if err := req.Validate(); !errors.Is(err, ErrSideMismatch) {
		t.Fatalf("expected ErrSideMismatch, got %v", err)
	}
}

func TestANonPositiveQuantityOrPriceIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*Request)
		want error
	}{
		{"zero quantity", func(r *Request) { r.Execution.Quantity = decimal.Zero }, ErrQuantityNotPositive},
		{"negative quantity", func(r *Request) { r.Execution.Quantity = dec("-0.01") }, ErrQuantityNotPositive},
		{"zero price", func(r *Request) { r.Execution.Price = decimal.Zero }, ErrPriceNotPositive},
		{"negative price", func(r *Request) { r.Execution.Price = dec("-1") }, ErrPriceNotPositive},
		{"negative commission", func(r *Request) { r.Execution.Commission = dec("-0.01") }, ErrCommissionNegative},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := validRequest()
			c.set(&req)
			if err := req.Validate(); !errors.Is(err, c.want) {
				t.Fatalf("expected %v, got %v", c.want, err)
			}
		})
	}
}

// TestAReconciliationImportMustNameItsIssue.
//
// A fill that appeared during recovery with nothing to point at is not
// evidence of anything. The schema enforces the same rule for repair
// transitions; this covers the fill.
func TestAReconciliationImportMustNameItsIssue(t *testing.T) {
	req := validRequest()
	req.Source = SourceReconciliationImport
	req.IssueID = nil
	if err := req.Validate(); !errors.Is(err, ErrMissingIssue) {
		t.Fatalf("expected ErrMissingIssue, got %v", err)
	}

	empty := ""
	req.IssueID = &empty
	if err := req.Validate(); !errors.Is(err, ErrMissingIssue) {
		t.Fatalf("an empty issue id was accepted: %v", err)
	}

	id := "cafe0000-0000-4000-8000-000000000000"
	req.IssueID = &id
	if err := req.Validate(); err != nil {
		t.Fatalf("a reconciliation import naming its issue was refused: %v", err)
	}
}

// TestAnOrdinaryExecutionNeedsNoIssue.
//
// The mirror of the test above: requiring an issue for a normal execution
// would make every live fill fail.
func TestAnOrdinaryExecutionNeedsNoIssue(t *testing.T) {
	for _, source := range []Source{SourceExecutionResponse, SourceExecutionPoll} {
		req := validRequest()
		req.Source = source
		req.IssueID = nil
		if err := req.Validate(); err != nil {
			t.Errorf("%s was refused for having no issue id: %v", source, err)
		}
	}
}

func TestAnUnrecognisedSourceIsRefused(t *testing.T) {
	req := validRequest()
	req.Source = Source("smuggled_in")
	if err := req.Validate(); !errors.Is(err, ErrBadSource) {
		t.Fatalf("expected ErrBadSource, got %v", err)
	}
	// An empty source is also refused rather than defaulted, because the
	// schema's CHECK would otherwise reject it later with less explanation.
	req.Source = ""
	if err := req.Validate(); !errors.Is(err, ErrBadSource) {
		t.Fatalf("an empty source was accepted: %v", err)
	}
}

func TestOnlyTheThreeKnownSourcesAreValid(t *testing.T) {
	// The set must match the schema's CHECK constraint. A source valid here and
	// rejected there produces an aborted transaction instead of a clear
	// refusal.
	valid := []Source{SourceExecutionResponse, SourceReconciliationImport, SourceExecutionPoll}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	for _, s := range []Source{"", "manual", "import", "RECONCILIATION_IMPORT"} {
		if s.Valid() {
			t.Errorf("%q is accepted as a source but the schema's CHECK would refuse it", s)
		}
	}
}

// TestTheBookingServiceRefusesAnyModeButPaper.
//
// The same guard the OMS has, for the same reason: a booking service that
// would happily write live fills is one configuration mistake from doing so,
// and this build has no live adapter to make that mistake meaningful — which
// is exactly why the guard must not be the only thing standing in the way.
func TestTheBookingServiceRefusesAnyModeButPaper(t *testing.T) {
	for _, mode := range []domain.ExecutionMode{domain.ModeLive, domain.ModeDemo, ""} {
		if _, err := New(nil, nil, mode); err == nil {
			t.Errorf("booking.New accepted %q mode", mode)
		}
	}
}

func TestValidationRunsBeforeAnythingIsWritten(t *testing.T) {
	// Apply calls Validate first, so a malformed request cannot reach the
	// database at all. Asserted by calling Apply with a nil transaction: a
	// validation failure must surface as the validation error rather than as a
	// nil-pointer panic from the first query.
	svc, err := New(nil, nil, domain.ModePaper)
	if err != nil {
		t.Fatalf("constructing the booking service: %v", err)
	}

	req := validRequest()
	req.Execution.BrokerFillID = ""

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Apply panicked instead of refusing an invalid request: %v.\n"+
				"Validation must happen before the first query, or a malformed execution "+
				"aborts a transaction that may be repairing other issues", r)
		}
	}()
	if _, err := svc.Apply(nil, nil, req); !errors.Is(err, ErrNoBrokerExecutionID) {
		t.Fatalf("expected ErrNoBrokerExecutionID, got %v", err)
	}
}
