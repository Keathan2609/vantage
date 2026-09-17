package risk

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// The reducing matrix: every exposure ceiling against every kind of order.
//
// # Why a matrix rather than more individual cases
//
// Rule 8 has now had to be applied in FOUR separate places -- the notional cap,
// the event blackout, the daily-loss limit, and finally the per-order quantity
// cap. Each was found on its own, long after the last, because each test only
// covered the check it was written for. A matrix fails the moment a fifth
// ceiling is added without the exemption, which is the only way to stop a
// fifth discovery.
//
// # The four kinds, stated exactly
//
//	INCREASING  long 0.03, BUY  0.05  -- same side; never exempt
//	REDUCING    long 0.10, SELL 0.05  -- opposite side, smaller; exempt
//	FLATTENING  long 0.10, SELL 0.10  -- opposite side, equal; exempt
//	FLIPPING    long 0.10, SELL 1.00  -- closes 0.10, OPENS 0.90; NOT exempt
//
// The flip is the case that matters. "Opposite direction" must never become a
// blanket exemption: the 0.90 of new short exposure is new exposure, and every
// ceiling applies to it exactly as it would to an opening order.

// exposureCeilings are the checks whose purpose is to LIMIT exposure or loss,
// and which rule 8 therefore governs.
//
// Listed here rather than derived, so that adding a ceiling means adding a row
// and thinking about it. A check absent from this list is a check nobody has
// decided about.
var exposureCeilings = []struct {
	name domain.RiskCheckName
	// tighten sets the ceiling far below what the order needs, so a check
	// without the exemption fails loudly rather than passing by luck.
	tighten func(*Input)
}{
	{domain.CheckOrderQuantity, func(in *Input) { in.Limits.MaxOrderQuantity = dec("0.001") }},
	{domain.CheckOrderNotional, func(in *Input) { in.Limits.MaxOrderNotional = zar("1") }},
	{domain.CheckInstrumentExposure, func(in *Input) { in.Limits.MaxPerInstrumentExposure = zar("1") }},
	{domain.CheckGrossExposure, func(in *Input) { in.Limits.MaxGrossExposure = zar("1") }},
	{domain.CheckNetExposure, func(in *Input) { in.Limits.MaxNetExposure = zar("1") }},
	{domain.CheckLeverage, func(in *Input) { in.Limits.MaxLeverage = dec("0.0001") }},
	{domain.CheckDailyLoss, func(in *Input) {
		in.Limits.MaxDailyLoss = zar("1")
		withDayLoss(in, "20")
		in.Limits.MaxDailyLoss = zar("1")
	}},
	{domain.CheckDrawdown, func(in *Input) {
		in.Limits.MaxDrawdownFraction = dec("0.001")
		in.Snapshot.State.PeakEquity = zar("500")
		in.Snapshot.State.Equity = zar("400")
	}},

	// The authority's ceilings, which sit beside the account's and must treat
	// a genuine reduction the same way. Consistency between the two levels is
	// the point: an operator should not have to reason about which control
	// refused a flatten.
	{domain.CheckAuthorityOrderQuantity, func(in *Input) { in.Authority.MaxOrderQuantity = dec("0.001") }},
	{domain.CheckAuthorityOrderNotional, func(in *Input) { in.Authority.MaxOrderNotional = zar("1") }},
	{domain.CheckAuthorityPositionExposure, func(in *Input) { in.Authority.MaxPositionExposure = zar("1") }},
	{domain.CheckAuthorityLeverage, func(in *Input) { in.Authority.MaxLeverage = dec("0.0001") }},
	{domain.CheckAuthorityDailyLoss, func(in *Input) {
		in.Authority.MaxDailyLoss = zar("1")
		withDayLoss(in, "20")
		in.Authority.MaxDailyLoss = zar("1")
	}},
}

// orderKind describes one row of the matrix.
type orderKind struct {
	name string
	// position is the open quantity; empty means flat.
	position string
	side     domain.OrderSide
	quantity string
	// exempt is whether rule 8 exempts this order from an exposure ceiling.
	exempt bool
}

var orderKinds = []orderKind{
	{"increasing", "0.03", domain.SideBuy, "0.05", false},
	{"reducing", "0.10", domain.SideSell, "0.05", true},
	{"flattening", "0.10", domain.SideSell, "0.10", true},
	{"flipping", "0.10", domain.SideSell, "1.00", false},
}

func inputFor(k orderKind) Input {
	in := baseInput()
	if k.position != "" {
		in = withLongPosition(in, k.position)
	}
	in.Intent.Side = k.side
	in.Intent.Quantity = dec(k.quantity)
	// A closing order carries no stop, and the per-trade risk check only
	// applies when one is present. Keeping it nil isolates the ceiling.
	in.Intent.StopLoss = nil
	in.Intent.Source = domain.SourceRiskControl
	// Room for the flip's 0.90, so the venue's own step and maximum are not
	// what refuses it.
	in.Instrument.Spec.MaxQuantity = dec("100")
	return in
}

func TestEveryExposureCeilingTreatsEveryOrderKindCorrectly(t *testing.T) {
	for _, ceiling := range exposureCeilings {
		for _, kind := range orderKinds {
			t.Run(fmt.Sprintf("%s/%s", ceiling.name, kind.name), func(t *testing.T) {
				in := inputFor(kind)
				ceiling.tighten(&in)

				r := check(t, evaluate(t, in), ceiling.name)

				if kind.exempt && !r.Passed {
					t.Fatalf("%s refused a %s order (%s %s against a %s long).\n"+
						"limit %s, observed %s.\n"+
						"A check that limits exposure or loss must never refuse an "+
						"order that reduces it -- otherwise the position that "+
						"breached the limit becomes impossible to close, and the "+
						"bigger it is the harder the exit.",
						ceiling.name, kind.name, kind.side, kind.quantity,
						kind.position, r.Limit, r.Observed)
				}
				if !kind.exempt && r.Passed {
					t.Fatalf("%s PASSED a %s order (%s %s against a %s long).\n"+
						"limit %s, observed %s.\n"+
						"An order that opens exposure is not exempt, whichever "+
						"direction it points. A SELL of 1.00 against a 0.10 long "+
						"closes 0.10 and opens 0.90 of new short exposure, and that "+
						"0.90 is new exposure.",
						ceiling.name, kind.name, kind.side, kind.quantity,
						kind.position, r.Limit, r.Observed)
				}
			})
		}
	}
}

func TestTheFlipIsRefusedAsAWholeDecision(t *testing.T) {
	// Not just one check: the DECISION must be refused. A flip that failed a
	// single ceiling while the decision was approved would still reach the
	// venue.
	in := inputFor(orderKind{"flipping", "0.10", domain.SideSell, "1.00", false})
	in.Limits.MaxOrderQuantity = dec("0.10")

	d := evaluate(t, in)
	if d.Approved {
		t.Fatal("a SELL of 1.00 against a 0.10 long was APPROVED under a 0.10 " +
			"per-order cap. The reducing exemption has become a way to open " +
			"0.90 of new exposure in the other direction.")
	}
}

func TestAFlattenIsPermittedAsAWholeDecision(t *testing.T) {
	// The other half, and the one that matters to an operator at the worst
	// possible moment: a position larger than the current per-order cap must
	// still be closable in one order.
	in := inputFor(orderKind{"flattening", "0.50", domain.SideSell, "0.50", true})
	in.Limits.MaxOrderQuantity = dec("0.10")
	in.Authority.MaxOrderQuantity = dec("0.10")

	d := evaluate(t, in)
	for _, name := range []domain.RiskCheckName{
		domain.CheckOrderQuantity, domain.CheckAuthorityOrderQuantity,
	} {
		if r := check(t, d, name); !r.Passed {
			t.Errorf("%s refused a flatten of 0.50 against a 0.50 long under a "+
				"0.10 cap: %s.\nA position opened while the cap was wider would "+
				"be impossible to exit.", name, r.Message)
		}
	}
}

func TestTheReducingDefinitionRequiresBothHalves(t *testing.T) {
	// Pinned directly, because every one of the four rule-8 defects came from
	// one half being applied without the other.
	for _, c := range []struct {
		name     string
		position *domain.Position
		side     domain.OrderSide
		quantity string
		want     bool
	}{
		{"flat account", nil, domain.SideSell, "0.05", false},
		{"same side", longPositionOf("0.10"), domain.SideBuy, "0.05", false},
		{"opposite side, smaller", longPositionOf("0.10"), domain.SideSell, "0.05", true},
		{"opposite side, equal", longPositionOf("0.10"), domain.SideSell, "0.10", true},
		{"opposite side, larger", longPositionOf("0.10"), domain.SideSell, "0.11", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := baseInput()
			in.ExistingPosition = c.position
			in.Intent.Side = c.side
			in.Intent.Quantity = dec(c.quantity)
			in.Intent.StopLoss = nil
			in.Limits.MaxOrderQuantity = dec("0.001")

			// The quantity check is the cheapest witness of the exemption: it
			// is tightened below every quantity above, so it passes only when
			// the order is treated as reducing.
			got := check(t, evaluate(t, in), domain.CheckOrderQuantity).Passed
			if got != c.want {
				t.Fatalf("reducing = %v, want %v. Both halves matter: opposite "+
					"side AND quantity no greater than the open position.", got, c.want)
			}
		})
	}
}

func longPositionOf(quantity string) *domain.Position {
	return &domain.Position{
		InstrumentID: "XAUUSD.m", Side: domain.SideBuy,
		Quantity: decimal.RequireFromString(quantity), Status: domain.PositionOpen,
	}
}
