package risk

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// The boundaries of every binding authority ceiling.
//
// A limit is wrong in exactly one place, and it is never the middle. These
// cover the three points that matter for each ceiling -- exactly below,
// exactly AT, exactly above -- because an off-by-one at the boundary is the
// defect a test of "well under" and "well over" cannot see, and because
// `<=` versus `<` is a one-character difference that permits an order the
// operator believed was refused.
//
// AT the ceiling must PASS for four of the five. A ceiling is a maximum, not
// an exclusive bound: an operator who sets MaxOrderQuantity to 0.10 means 0.10
// is allowed, and a platform that refused it would be refusing the operator's
// own stated limit.
//
// The daily-loss ceiling is the exception and is exclusive at its boundary.
// Its code is `daily_loss_limit_reached` and reaching the limit is the
// trigger: an account exactly at its daily loss ceiling has SPENT the budget,
// not almost spent it. That asymmetry is pinned below rather than smoothed
// over, because the two shapes are easy to confuse and only one is right for
// each ceiling.

func TestEveryAuthorityCeilingIsInclusiveAtItsBoundary(t *testing.T) {
	// Each point says where the OBSERVED value sits relative to the ceiling,
	// and states its own expectation rather than deriving one from a flag.
	// The daily-loss ceiling behaves differently from the other four and a
	// derived expectation hid that behind an `inverted` boolean, which is
	// exactly the kind of cleverness that makes a wrong fixture look right.
	type point struct {
		label      string
		set        func(*Input)
		wantPassed bool
	}

	for _, c := range []struct {
		name   string
		check  domain.RiskCheckName
		points []point
	}{
		{
			// The ceiling is fixed at 0.10 lots and the ORDER moves.
			name:  "order quantity",
			check: domain.CheckAuthorityOrderQuantity,
			points: []point{
				{"0.09 under a 0.10 ceiling", func(in *Input) {
					in.Authority.MaxOrderQuantity = dec("0.10")
					in.Intent.Quantity = dec("0.09")
				}, true},
				{"0.10 exactly at a 0.10 ceiling", func(in *Input) {
					in.Authority.MaxOrderQuantity = dec("0.10")
					in.Intent.Quantity = dec("0.10")
				}, true},
				{"0.11 over a 0.10 ceiling", func(in *Input) {
					in.Authority.MaxOrderQuantity = dec("0.10")
					in.Intent.Quantity = dec("0.11")
				}, false},
			},
		},
		{
			// 0.01 lots of a one-ounce contract at the 2648.27 ask, converted
			// at 18.25, is exactly 483.309275 ZAR. The ceiling moves around it
			// by a millionth of a cent -- a margin no float comparison
			// survives, which is the point of testing at this resolution.
			name:  "order notional",
			check: domain.CheckAuthorityOrderNotional,
			points: []point{
				{"483.309275 under a 483.309276 ceiling",
					func(in *Input) { in.Authority.MaxOrderNotional = zar("483.309276") }, true},
				{"483.309275 exactly at its own ceiling",
					func(in *Input) { in.Authority.MaxOrderNotional = zar("483.309275") }, true},
				{"483.309275 over a 483.309274 ceiling",
					func(in *Input) { in.Authority.MaxOrderNotional = zar("483.309274") }, false},
			},
		},
		{
			name:  "position exposure",
			check: domain.CheckAuthorityPositionExposure,
			points: []point{
				{"483.309275 under a 483.309276 ceiling",
					func(in *Input) { in.Authority.MaxPositionExposure = zar("483.309276") }, true},
				{"483.309275 exactly at its own ceiling",
					func(in *Input) { in.Authority.MaxPositionExposure = zar("483.309275") }, true},
				{"483.309275 over a 483.309274 ceiling",
					func(in *Input) { in.Authority.MaxPositionExposure = zar("483.309274") }, false},
			},
		},
		{
			// Projected gross exposure 483.309275 against 500 equity is
			// 0.96661855x exactly.
			name:  "leverage",
			check: domain.CheckAuthorityLeverage,
			points: []point{
				{"0.96661855x under a 0.96661856x ceiling",
					func(in *Input) { in.Authority.MaxLeverage = dec("0.96661856") }, true},
				{"0.96661855x exactly at its own ceiling",
					func(in *Input) { in.Authority.MaxLeverage = dec("0.96661855") }, true},
				{"0.96661855x over a 0.96661854x ceiling",
					func(in *Input) { in.Authority.MaxLeverage = dec("0.96661854") }, false},
			},
		},
		{
			// The ONE ceiling that is exclusive at its boundary, and
			// deliberately so: its code is `daily_loss_limit_reached`, and
			// reaching the limit is the trigger. An account exactly at its
			// daily loss ceiling has spent the budget, not almost spent it.
			name:  "daily loss",
			check: domain.CheckAuthorityDailyLoss,
			points: []point{
				{"9.99 lost under a 10.00 ceiling", func(in *Input) {
					withDayLoss(in, "9.99")
					in.Authority.MaxDailyLoss = zar("10")
				}, true},
				{"10.00 lost, exactly REACHING a 10.00 ceiling", func(in *Input) {
					withDayLoss(in, "10.00")
					in.Authority.MaxDailyLoss = zar("10")
				}, false},
				{"10.01 lost past a 10.00 ceiling", func(in *Input) {
					withDayLoss(in, "10.01")
					in.Authority.MaxDailyLoss = zar("10")
				}, false},
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, pt := range c.points {
				in := baseInput()
				pt.set(&in)
				r := check(t, evaluate(t, in), c.check)
				if r.Passed != pt.wantPassed {
					verb, want := "refused", "permit"
					if r.Passed {
						verb, want = "permitted", "refuse"
					}
					t.Errorf("%s: %s %s it, and must %s.\n"+
						"limit %s, observed %s", pt.label, c.check, verb, want,
						r.Limit, r.Observed)
				}
			}
		})
	}
}

// withDayLoss puts the account exactly `amount` down on the day.
//
// Day P&L is equity minus the day's starting equity, so the loss is expressed
// by moving equity rather than by setting a P&L field that nothing reads.
func withDayLoss(in *Input, amount string) {
	loss := dec(amount)
	in.Snapshot.State.DayStartEquity = zar("500")
	in.Snapshot.State.Equity = zar(decimal.RequireFromString("500").Sub(loss).String())
	in.Snapshot.State.RealizedPnL = zar(loss.Neg().String())
	// Peak equity tracks the day start here, so the drawdown ceiling does not
	// fire alongside and make the failure ambiguous.
	in.Snapshot.State.PeakEquity = zar("500")
	in.Limits.MaxDrawdownFraction = dec("0.99")
	in.Limits.MaxDailyLoss = zar("10000")
}

func TestAnAuthorityCeilingUsesExactDecimalArithmetic(t *testing.T) {
	// 0.1 + 0.2 is 0.30000000000000004 in binary floating point, and an
	// authority capped at 0.3 would then refuse an order of exactly 0.3.
	// Money and quantities are decimal end to end for this reason; the test
	// exists so that a future change to float would fail here rather than in
	// production at a boundary nobody tests by hand.
	in := baseInput()
	in.Instrument.Spec.QuantityStep = dec("0.1")
	in.Instrument.Spec.QuantityPrecision = 1
	in.Instrument.Spec.MinQuantity = dec("0.1")
	in.Intent.Quantity = dec("0.1").Add(dec("0.2"))
	in.Authority.MaxOrderQuantity = dec("0.3")
	in.Limits.MaxOrderQuantity = dec("1")

	if got := in.Intent.Quantity.String(); got != "0.3" {
		t.Fatalf("decimal addition produced %q, not 0.3; the fixture is not "+
			"testing what it claims", got)
	}
	if r := check(t, evaluate(t, in), domain.CheckAuthorityOrderQuantity); !r.Passed {
		t.Errorf("an order of exactly the authority's 0.3 ceiling was refused "+
			"(limit %s, observed %s). That is the floating-point result, not "+
			"the decimal one.", r.Limit, r.Observed)
	}
}

func TestTheNotionalCeilingFailsClosedWithoutAnFXRate(t *testing.T) {
	// The instrument is quoted in USD and the account is in ZAR, so the
	// order's value cannot be expressed in the currency the ceiling is
	// denominated in. No limit denominated in that currency can then be
	// checked, and a platform that guessed would be inventing the one number
	// the control depends on.
	//
	// The engine refuses before it reaches the authority's ceiling, which is
	// why this asserts the DECISION rather than the authority check: the
	// refusal is the same either way, and pretending otherwise would be
	// asserting an evaluation order that carries no meaning.
	in := baseInput()
	engine := newEngine(fixedRateSource{empty: true})
	d, err := engine.Evaluate(t.Context(), in)
	if err != nil {
		t.Fatalf("Evaluate returned an error rather than a refusal: %v", err)
	}
	if d.Approved {
		t.Fatal("an order was approved without an FX rate to value it in the " +
			"account's own currency; every currency-denominated ceiling was " +
			"unchecked and the order went through anyway")
	}
	r := check(t, d, domain.CheckOrderNotional)
	if r.Passed {
		t.Error("the notional check passed without a rate")
	}
	if r.Code != domain.RejectInternalError {
		t.Errorf("a missing rate was reported as %q; it is a refusal to value "+
			"the order, and the code has to say so", r.Code)
	}
}

// --- reducing, flattening, flipping ----------------------------------------
//
// The three cases the brief separates, pinned against the authority's
// ceilings rather than only the account's. The distinction is exact:
//
//	long 0.10, SELL 0.05  -> reducing   (exempt)
//	long 0.10, SELL 0.10  -> flattening (exempt: still no new exposure)
//	long 0.10, SELL 1.00  -> flipping   (NOT exempt: 0.90 of new short)

func TestAPartialCloseIsExemptFromEveryAuthorityCeiling(t *testing.T) {
	in := withLongPosition(baseInput(), "0.10")
	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec("0.05")
	in.Intent.StopLoss = nil
	tightenEveryAuthorityCeiling(&in)

	d := evaluate(t, in)
	for _, name := range domain.AuthorityCeilingChecks {
		if r := check(t, d, name); !r.Passed {
			t.Errorf("%s refused a partial close of 0.05 against a 0.10 "+
				"position: %s", name, r.Message)
		}
	}
}

func TestAFlattenIsExemptFromEveryAuthorityCeiling(t *testing.T) {
	// Exactly the position size. The boundary of the reducing definition, and
	// the case an operator reaches for when they want out entirely.
	in := withLongPosition(baseInput(), "0.10")
	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec("0.10")
	in.Intent.StopLoss = nil
	tightenEveryAuthorityCeiling(&in)

	d := evaluate(t, in)
	for _, name := range domain.AuthorityCeilingChecks {
		if r := check(t, d, name); !r.Passed {
			t.Errorf("%s refused a FLATTEN of exactly the 0.10 position: %s.\n"+
				"An authority that cannot be exited is not a ceiling, it is a trap.",
				name, r.Message)
		}
	}
}

func TestAPositionFlipCannotAbuseTheReducingExemption(t *testing.T) {
	// long 0.10, SELL 1.00. The first 0.10 reduces; the remaining 0.90 opens
	// new short exposure, and that 0.90 must not pass merely because the order
	// started out opposing the position.
	in := withLongPosition(baseInput(), "0.10")
	in.Intent.Side = domain.SideSell
	in.Intent.Quantity = dec("1.00")
	in.Intent.StopLoss = nil
	in.Authority.MaxOrderQuantity = dec("0.10")
	in.Authority.MaxOrderNotional = zar("500")
	in.Authority.MaxPositionExposure = zar("500")

	d := evaluate(t, in)
	if d.Approved {
		t.Fatal("a SELL of 1.00 against a 0.10 long was APPROVED under an " +
			"authority capped at 0.10 lots. The reducing exemption has become " +
			"a way to open 0.90 of new exposure in the other direction.")
	}
	for _, name := range []domain.RiskCheckName{
		domain.CheckAuthorityOrderQuantity,
		domain.CheckAuthorityOrderNotional,
		domain.CheckAuthorityPositionExposure,
	} {
		if r := check(t, d, name); r.Passed {
			t.Errorf("%s passed the over-closing flip (limit %s, observed %s)",
				name, r.Limit, r.Observed)
		}
	}
}

func TestASameSideAddIsNeverExemptFromTheAuthority(t *testing.T) {
	// Holding a long and buying more is not reducing by any reading, and the
	// exemption must not reach it.
	in := withLongPosition(baseInput(), "0.10")
	in.Intent.Side = domain.SideBuy
	in.Intent.Quantity = dec("0.50")
	in.Authority.MaxOrderQuantity = dec("0.10")

	if r := check(t, evaluate(t, in), domain.CheckAuthorityOrderQuantity); r.Passed {
		t.Errorf("adding 0.50 to an existing long passed an authority capped "+
			"at 0.10 (observed %s)", r.Observed)
	}
}

// tightenEveryAuthorityCeiling sets all five far below what the order needs,
// so any check without the reducing exemption fails loudly.
func tightenEveryAuthorityCeiling(in *Input) {
	in.Authority.MaxOrderQuantity = dec("0.001")
	in.Authority.MaxOrderNotional = zar("1")
	in.Authority.MaxPositionExposure = zar("1")
	in.Authority.MaxLeverage = dec("0.0001")
	in.Authority.MaxDailyLoss = zar("1")
	withDayLoss(in, "20")
}
