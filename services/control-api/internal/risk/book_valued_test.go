package risk

import (
	"testing"

	"github.com/vantage/control-api/internal/domain"
)

// A book that cannot be priced must not be traded into.
//
// `portfolio.aggregate` counts an unvaluable position and then skips it: it
// adds nothing to unrealised P&L, nothing to margin used, nothing to gross
// exposure. So every ceiling in the engine is computed against a book that is
// missing a position, and they all pass MORE easily than they should. The
// snapshot does not look broken; it looks healthy.
//
// A stale FX rate reaches this in a day -- the converter's maximum age is 24
// hours and nothing in the running system refreshes a rate -- and until this
// check existed, `Snapshot.UnvaluedPositions` was read by one JSON field and
// nothing else.

func TestAnUnpriceablePositionRefusesNewExposure(t *testing.T) {
	in := baseInput()
	in.Snapshot.UnvaluedPositions = 1

	d := evaluate(t, in)
	c := check(t, d, domain.CheckBookIsValued)
	if c.Passed {
		t.Fatal("opening was allowed while a position could not be priced; " +
			"every exposure figure in that decision understates the real book")
	}
	if d.Approved {
		t.Fatal("expected refusal while the book cannot be valued")
	}
	if c.Code != domain.RejectBookNotValued {
		t.Errorf("refusal code is %q, which the terminal cannot present as the "+
			"specific reason", c.Code)
	}
}

// The other half, and the one that matters more.
//
// Refusing to let an operator CLOSE a position because the platform cannot
// price it would hold them in the position precisely while the system admits
// it does not know what that position is worth. Three separate checks were
// once found doing exactly this, which is why the rule is written out in
// engine.go.
func TestAnUnpriceableBookStillAllowsClosing(t *testing.T) {
	in := baseInput()
	in.Snapshot.UnvaluedPositions = 1

	// An open long, and a sell for no more than its size: strictly reducing.
	existing := domain.Position{
		AccountID:    in.Intent.AccountID,
		InstrumentID: in.Intent.InstrumentID,
		Side:         domain.SideBuy,
		Quantity:     dec("0.05"),
	}
	in.ExistingPosition = &existing
	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec("0.05")

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckBookIsValued); !c.Passed {
		t.Fatal("a reducing order was refused because the book could not be " +
			"valued, which traps the operator in the position")
	}
}

// A healthy book must not trip it, or the check would refuse everything and
// be removed by the next person to notice.
func TestAFullyPricedBookPassesTheValuationCheck(t *testing.T) {
	in := baseInput()
	in.Snapshot.UnvaluedPositions = 0

	d := evaluate(t, in)
	if c := check(t, d, domain.CheckBookIsValued); !c.Passed {
		t.Fatal("a fully priced book failed the valuation check")
	}
}
