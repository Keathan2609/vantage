package portfolio

import "github.com/vantage/control-api/internal/money"

// exposure is the result of summing a set of valued positions.
//
// # Why this is separated from Compute
//
// Compute reads six tables before it reaches this arithmetic, so testing the
// arithmetic through Compute means standing up a database and seeding an
// account, a quote, an FX rate and a position for every case. The result is
// that the cases nobody wants to seed -- an unvalued position, a long and a
// short in the same instrument, exposure that nets to zero across currencies
// while remaining large in gross -- are the cases that never get tested, and
// those are precisely the ones where understating risk is expensive.
//
// So the arithmetic is a pure function of already-valued positions. It has no
// store, no clock and no context, and every invariant below is asserted
// directly.
type exposure struct {
	Unrealized money.Amount
	MarginUsed money.Amount
	// Gross is the sum of absolute notionals: how much market the account is
	// actually facing. Net is the signed sum: which way it leans.
	Gross money.Amount
	Net   money.Amount

	ByInstrument map[string]money.Amount
	ByCurrency   map[money.Currency]money.Amount

	// Unvalued counts positions that could not be priced. Any value above
	// zero means every figure above UNDERSTATES the account's real exposure,
	// which is why callers surface it rather than treating it as a detail.
	Unvalued int
}

// aggregate sums valued positions in the account currency.
//
// An unvalued position is counted in Unvalued and contributes nothing else. It
// is deliberately not treated as zero exposure: a position that cannot be
// priced is usually one whose instrument has just lost its feed, which is
// exactly when pretending it is flat is most dangerous.
func aggregate(views []PositionView, ccy money.Currency) exposure {
	agg := exposure{
		Unrealized:   money.Zero(ccy),
		MarginUsed:   money.Zero(ccy),
		Gross:        money.Zero(ccy),
		Net:          money.Zero(ccy),
		ByInstrument: map[string]money.Amount{},
		ByCurrency:   map[money.Currency]money.Amount{},
	}

	for _, view := range views {
		if !view.Valued {
			agg.Unvalued++
			continue
		}

		agg.Unrealized = agg.Unrealized.MustAdd(view.UnrealizedPnL)
		agg.MarginUsed = agg.MarginUsed.MustAdd(view.MarginUsed)
		agg.Gross = agg.Gross.MustAdd(view.NotionalValue.Abs())

		signed := view.NotionalValue.MulDecimal(view.Position.Side.SignedMultiplier())
		agg.Net = agg.Net.MustAdd(signed)

		agg.ByInstrument[view.Position.InstrumentID] = addTo(
			agg.ByInstrument[view.Position.InstrumentID], view.NotionalValue.Abs(), ccy)

		// Currency exposure is attributed to the instrument's QUOTE currency:
		// a long XAUUSD position is, among other things, a short USD position,
		// and netting that against other USD exposure is the point.
		agg.ByCurrency[view.Instrument.QuoteCcy] = addTo(
			agg.ByCurrency[view.Instrument.QuoteCcy], signed, ccy)
	}

	return agg
}

// addTo adds to a map entry that may not exist yet.
//
// money.Amount's zero value has an EMPTY currency on purpose, so it cannot
// silently join a calculation as "0 ZAR". A missing map entry is therefore not
// usable as an accumulator and has to be initialised to a real zero first.
func addTo(cur, delta money.Amount, ccy money.Currency) money.Amount {
	if cur.Currency() == "" {
		cur = money.Zero(ccy)
	}
	return cur.MustAdd(delta)
}
