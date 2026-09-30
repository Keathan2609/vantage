package risk

import (
	"testing"

	"github.com/vantage/control-api/internal/domain"
)

// The trading authority's numeric ceilings, once they bind.
//
// Four of the five were stored, served and recorded but never compared against
// anything, so an operator who narrowed one was not protected by it. These
// tests are the evidence that each now binds, that none of them can LOOSEN the
// account's own limits, and -- the part that matters most -- that none of them
// can refuse an order that reduces a position.
//
// Each case narrows ONLY the authority and leaves the account's corresponding
// limit wide, so a failure can only have come from the authority. That is the
// whole claim: the authority is a bound in its own right, not a display field.

func TestEachAuthorityCeilingRefusesAnOrderThatBreachesIt(t *testing.T) {
	for _, c := range []struct {
		name  string
		set   func(*Input)
		check domain.RiskCheckName
	}{
		{
			// 0.02 lots requested against an authority capped at 0.01, while
			// the account's own cap stays at 1.
			name: "order quantity",
			set: func(in *Input) {
				in.Intent.Quantity = dec("0.02")
				in.Authority.MaxOrderQuantity = dec("0.01")
			},
			check: domain.CheckAuthorityOrderQuantity,
		},
		{
			// 0.01 lots of gold at ~2648 USD is ~483 ZAR at 18.25. An
			// authority notional of 100 ZAR is below it; the account's stays
			// at 2500.
			name: "order notional",
			set: func(in *Input) {
				in.Authority.MaxOrderNotional = zar("100")
			},
			check: domain.CheckAuthorityOrderNotional,
		},
		{
			name: "position exposure",
			set: func(in *Input) {
				in.Authority.MaxPositionExposure = zar("100")
			},
			check: domain.CheckAuthorityPositionExposure,
		},
		{
			// Realised loss of 20 ZAR against an authority limit of 10, while
			// the account's own limit of 15 would also bind -- so this case
			// additionally loosens the ACCOUNT's limit to 50 to prove the
			// refusal comes from the authority and not from its neighbour.
			name: "daily loss",
			set: func(in *Input) {
				in.Limits.MaxDailyLoss = zar("50")
				in.Authority.MaxDailyLoss = zar("10")
				in.Snapshot.State.RealizedPnL = zar("-20")
				in.Snapshot.State.Equity = zar("480")
				in.Snapshot.State.DayStartEquity = zar("500")
			},
			check: domain.CheckAuthorityDailyLoss,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := baseInput()
			c.set(&in)
			d := evaluate(t, in)

			if r := check(t, d, c.check); r.Passed {
				t.Fatalf("%s passed an order that breaches the trading authority: "+
					"limit %s, observed %s. The ceiling is being displayed, not enforced.",
					c.check, r.Limit, r.Observed)
			}
			if d.Approved {
				t.Fatalf("the decision was APPROVED despite %s failing", c.check)
			}
		})
	}
}

func TestAnAuthorityCeilingNeverLoosensTheAccountsOwnLimit(t *testing.T) {
	// Risk may only reduce. An authority wider than the account's limit must
	// change nothing: the tighter of the two binds, always, and here that is
	// the account's.
	//
	// Written as its own test because the failure mode is silent. A fold that
	// took max() instead of min(), or a check that replaced the account's
	// rather than joining it, would leave every ordinary order still passing
	// and only show up the day an operator relied on the account limit.
	for _, c := range []struct {
		name  string
		set   func(*Input)
		check domain.RiskCheckName
	}{
		{
			name: "order quantity",
			set: func(in *Input) {
				in.Intent.Quantity = dec("0.05")
				in.Limits.MaxOrderQuantity = dec("0.01")   // the account is strict
				in.Authority.MaxOrderQuantity = dec("100") // the authority is not
			},
			check: domain.CheckOrderQuantity,
		},
		{
			name: "order notional",
			set: func(in *Input) {
				in.Limits.MaxOrderNotional = zar("100")
				in.Authority.MaxOrderNotional = zar("100000")
			},
			check: domain.CheckOrderNotional,
		},
		{
			name: "position exposure",
			set: func(in *Input) {
				in.Limits.MaxPerInstrumentExposure = zar("100")
				in.Authority.MaxPositionExposure = zar("100000")
			},
			check: domain.CheckInstrumentExposure,
		},
		{
			name: "daily loss",
			set: func(in *Input) {
				in.Limits.MaxDailyLoss = zar("10")
				in.Authority.MaxDailyLoss = zar("100000")
				in.Snapshot.State.RealizedPnL = zar("-20")
				in.Snapshot.State.Equity = zar("480")
				in.Snapshot.State.DayStartEquity = zar("500")
			},
			check: domain.CheckDailyLoss,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := baseInput()
			c.set(&in)
			d := evaluate(t, in)

			if r := check(t, d, c.check); r.Passed {
				t.Fatalf("the account's own %s passed (limit %s, observed %s) while "+
					"the authority was wide. A wider authority must never relax an "+
					"account limit.", c.check, r.Limit, r.Observed)
			}
			if d.Approved {
				t.Fatalf("the decision was APPROVED despite the account's %s failing", c.check)
			}
		})
	}
}

func TestNoAuthorityCeilingRefusesAnOrderThatReducesAPosition(t *testing.T) {
	// ENGINEERING_GUIDE.md rule 8, applied to the four new checks before they can break
	// it. A check that limits exposure or loss must never refuse a reducing
	// order; three separate checks had already been found trapping an
	// operator in a position, and enforcing the authority is exactly the kind
	// of change that adds a fourth.
	//
	// Every ceiling here is set FAR below what closing this position needs, so
	// each check would fail if it were not exempt. The account holds 0.03 lots
	// long and the order sells exactly 0.03: strictly reducing, no side flip.
	for _, c := range []struct {
		name  string
		set   func(*Input)
		check domain.RiskCheckName
	}{
		{
			name: "order quantity",
			set: func(in *Input) {
				// The position is larger than the authority now permits per
				// order -- which is how a narrowed authority traps a position
				// opened while it was wider.
				in.Authority.MaxOrderQuantity = dec("0.001")
			},
			check: domain.CheckAuthorityOrderQuantity,
		},
		{
			name: "order notional",
			set: func(in *Input) {
				in.Authority.MaxOrderNotional = zar("1")
			},
			check: domain.CheckAuthorityOrderNotional,
		},
		{
			name: "position exposure",
			set: func(in *Input) {
				in.Authority.MaxPositionExposure = zar("1")
			},
			check: domain.CheckAuthorityPositionExposure,
		},
		{
			name: "daily loss",
			set: func(in *Input) {
				in.Authority.MaxDailyLoss = zar("1")
				in.Snapshot.State.RealizedPnL = zar("-20")
				in.Snapshot.State.Equity = zar("480")
				in.Snapshot.State.DayStartEquity = zar("500")
			},
			check: domain.CheckAuthorityDailyLoss,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := withLongPosition(baseInput(), "0.03")
			in.Intent.Side = domain.SideSell
			in.Intent.Quantity = dec("0.03")
			in.Intent.StopLoss = nil
			in.Intent.Source = domain.SourceRiskControl
			c.set(&in)

			if r := check(t, evaluate(t, in), c.check); !r.Passed {
				t.Fatalf("%s refused a position-closing order: %s.\n"+
					"Narrowing the trading authority has locked the account into "+
					"the exposure the ceiling exists to contain.", c.check, r.Message)
			}
		})
	}
}

func TestAnOverClosingSideFlipDoesNotEscapeTheAuthorityCeilings(t *testing.T) {
	// The reducing exemption above must not become a hole. An account long
	// 0.03 selling 0.50 is not reducing: it closes 0.03 and opens 0.47 the
	// other way, and the authority's ceilings must still apply to that.
	in := withLongPosition(baseInput(), "0.03")
	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec("0.50")
	in.Intent.StopLoss = nil
	in.Authority.MaxOrderQuantity = dec("0.10")
	in.Authority.MaxOrderNotional = zar("100")

	d := evaluate(t, in)
	for _, name := range []domain.RiskCheckName{
		domain.CheckAuthorityOrderQuantity,
		domain.CheckAuthorityOrderNotional,
	} {
		if r := check(t, d, name); r.Passed {
			t.Errorf("%s passed an over-closing side flip (limit %s, observed %s); "+
				"the reducing exemption was applied to an order that increases "+
				"exposure in the other direction", name, r.Limit, r.Observed)
		}
	}
}

func TestAnUnsetAuthorityCeilingFailsClosed(t *testing.T) {
	// A zero ceiling cannot reach the engine from the database: every column
	// is NOT NULL with a `> 0` CHECK, and the create handler rejects a
	// non-positive value. So a zero here means the authority was not loaded,
	// and the platform cannot show the order is within an authority it does
	// not have. That is a refusal, not a default of "unlimited" -- treating an
	// absent bound as no bound is the most permissive reading available, and
	// the authority is deliberately an enumeration of what MAY happen.
	in := baseInput()
	in.Authority.MaxOrderQuantity = dec("0")

	d := evaluate(t, in)
	if r := check(t, d, domain.CheckAuthorityOrderQuantity); r.Passed {
		t.Fatal("an unset authority ceiling was read as 'no limit'")
	}
	if d.Approved {
		t.Fatal("an order was approved under an authority with an unset ceiling")
	}
}
