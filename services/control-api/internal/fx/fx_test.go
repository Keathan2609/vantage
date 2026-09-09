package fx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// Conversion decides whether an order's risk can be expressed in the account's
// currency at all, so every failure here becomes a refusal upstream. These
// tests exist because the package had none: the cases that matter are the ones
// where a rate is missing, stale, or has to be assembled from two legs, and
// none of those happen reliably in an end-to-end run against a seeded database
// that always has every rate fresh.

// fakeSource is a rate table with no I/O.
type fakeSource struct {
	rates map[string]Rate
	calls int
}

func (f *fakeSource) LatestFXRate(_ context.Context, base, quote money.Currency) (Rate, error) {
	f.calls++
	if r, ok := f.rates[string(base)+"/"+string(quote)]; ok {
		return r, nil
	}
	return Rate{}, errors.New("fake: no such pair")
}

func at(t time.Time, base, quote money.Currency, rate string) Rate {
	return Rate{
		Base: base, Quote: quote,
		Rate:       decimal.RequireFromString(rate),
		SourceTime: t,
		IngestedAt: t,
		Provider:   "fake",
	}
}

var now = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

func newTestConverter(src RateSource, maxAge time.Duration) *Converter {
	return NewConverter(src, maxAge, func() time.Time { return now })
}

func zar(t *testing.T, v string) money.Amount {
	t.Helper()
	a, err := money.FromString(v, money.ZAR)
	if err != nil {
		t.Fatalf("money.FromString(%q): %v", v, err)
	}
	return a
}

func usd(t *testing.T, v string) money.Amount {
	t.Helper()
	a, err := money.FromString(v, money.USD)
	if err != nil {
		t.Fatalf("money.FromString(%q): %v", v, err)
	}
	return a
}

func TestADirectRateIsUsedAndRecordedAsDirect(t *testing.T) {
	src := &fakeSource{rates: map[string]Rate{
		"USD/ZAR": at(now, money.USD, money.ZAR, "18.25"),
	}}
	c := newTestConverter(src, time.Hour)

	conv, err := c.Convert(context.Background(), usd(t, "100"), money.ZAR)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if conv.Path != "direct" {
		t.Errorf("path = %q, want direct", conv.Path)
	}
	if !conv.To.Decimal().Equal(decimal.RequireFromString("1825")) {
		t.Errorf("converted = %s, want 1825", conv.To.Decimal())
	}
	if conv.To.Currency() != money.ZAR {
		t.Errorf("currency = %q, want ZAR", conv.To.Currency())
	}
	// Provenance travels with the result, because a stored valuation has to be
	// explainable months later.
	if conv.Provider != "fake" || !conv.RateTime.Equal(now) {
		t.Errorf("provenance lost: provider=%q rateTime=%s", conv.Provider, conv.RateTime)
	}
}

func TestAnIdenticalCurrencyNeedsNoRateAtAll(t *testing.T) {
	// An empty rate table on purpose: converting ZAR to ZAR must not consult a
	// source, or an account would become unvaluable the moment its own
	// currency pair went missing.
	src := &fakeSource{rates: map[string]Rate{}}
	c := newTestConverter(src, time.Hour)

	conv, err := c.Convert(context.Background(), zar(t, "500"), money.ZAR)
	if err != nil {
		t.Fatalf("identity conversion failed: %v", err)
	}
	if src.calls != 0 {
		t.Errorf("identity conversion consulted the rate source %d time(s)", src.calls)
	}
	if conv.Path != "identity" || !conv.Rate.Equal(decimal.NewFromInt(1)) {
		t.Errorf("path = %q rate = %s, want identity at 1", conv.Path, conv.Rate)
	}
}

func TestTheInverseIsUsedWhenOnlyTheOppositePairExists(t *testing.T) {
	// The seed stores USD/ZAR, not ZAR/USD. Refusing to invert would make
	// every rand-denominated margin figure unconvertible.
	src := &fakeSource{rates: map[string]Rate{
		"USD/ZAR": at(now, money.USD, money.ZAR, "20"),
	}}
	c := newTestConverter(src, time.Hour)

	conv, err := c.Convert(context.Background(), zar(t, "100"), money.USD)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if conv.Path != "inverse" {
		t.Errorf("path = %q, want inverse", conv.Path)
	}
	if !conv.To.Decimal().Equal(decimal.RequireFromString("5")) {
		t.Errorf("converted = %s, want 5", conv.To.Decimal())
	}
}

func TestACrossThroughUSDIsAssembledWhenNeitherLegIsDirect(t *testing.T) {
	// EUR to ZAR with no EUR/ZAR pair: the pivot is the only route, and the
	// path records that it was crossed rather than observed.
	src := &fakeSource{rates: map[string]Rate{
		"EUR/USD": at(now, money.EUR, money.USD, "1.10"),
		"USD/ZAR": at(now, money.USD, money.ZAR, "18"),
	}}
	c := newTestConverter(src, time.Hour)

	amount, err := money.FromString("100", money.EUR)
	if err != nil {
		t.Fatalf("FromString: %v", err)
	}
	conv, err := c.Convert(context.Background(), amount, money.ZAR)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if conv.Path != "via USD" {
		t.Errorf("path = %q, want via USD", conv.Path)
	}
	if !conv.To.Decimal().Equal(decimal.RequireFromString("1980")) {
		t.Errorf("converted = %s, want 1980 (100 * 1.10 * 18)", conv.To.Decimal())
	}
}

func TestACrossCarriesTheEarlierOfItsTwoLegTimes(t *testing.T) {
	// A cross is only as fresh as its stalest leg. Reporting the newer time
	// would overstate how current the valuation is.
	older := now.Add(-30 * time.Minute)
	src := &fakeSource{rates: map[string]Rate{
		"EUR/USD": at(older, money.EUR, money.USD, "1.10"),
		"USD/ZAR": at(now, money.USD, money.ZAR, "18"),
	}}
	c := newTestConverter(src, time.Hour)

	amount, _ := money.FromString("1", money.EUR)
	conv, err := c.Convert(context.Background(), amount, money.ZAR)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !conv.RateTime.Equal(older) {
		t.Errorf("cross rate time = %s, want the older leg %s", conv.RateTime, older)
	}
}

func TestAStaleDirectRateIsRefusedRatherThanUsed(t *testing.T) {
	// The important half of this: a stale direct rate must NOT fall through to
	// an inverse or a cross that happens to be fresher. Silently valuing a
	// position at a different pair's price is worse than refusing.
	src := &fakeSource{rates: map[string]Rate{
		"USD/ZAR": at(now.Add(-48*time.Hour), money.USD, money.ZAR, "18.25"),
		"ZAR/USD": at(now, money.ZAR, money.USD, "0.0548"),
	}}
	c := newTestConverter(src, time.Hour)

	_, err := c.Convert(context.Background(), usd(t, "100"), money.ZAR)
	if !errors.Is(err, ErrRateStale) {
		t.Fatalf("Convert with a stale direct rate = %v, want ErrRateStale", err)
	}
}

func TestNoRateAtAllFailsAndNeverFallsBackToOne(t *testing.T) {
	// Converting at 1.0 because no rate exists would report a 100 USD margin
	// as 100 ZAR -- roughly an eighteenth of the truth, in the direction that
	// makes an unaffordable trade look affordable.
	src := &fakeSource{rates: map[string]Rate{}}
	c := newTestConverter(src, time.Hour)

	conv, err := c.Convert(context.Background(), usd(t, "100"), money.ZAR)
	if !errors.Is(err, ErrNoRate) {
		t.Fatalf("Convert with no rates = %v, want ErrNoRate", err)
	}
	if !conv.To.Decimal().IsZero() || conv.To.Currency() != "" {
		t.Errorf("a failed conversion returned a usable-looking amount: %s %s",
			conv.To.Decimal(), conv.To.Currency())
	}
}

func TestAnEmptyCurrencyIsRefused(t *testing.T) {
	// money.Amount's zero value has an empty currency deliberately. Converting
	// one would mean an uninitialised field had reached a valuation.
	src := &fakeSource{rates: map[string]Rate{
		"USD/ZAR": at(now, money.USD, money.ZAR, "18.25"),
	}}
	c := newTestConverter(src, time.Hour)

	if _, err := c.Convert(context.Background(), money.Amount{}, money.ZAR); !errors.Is(err, ErrNoRate) {
		t.Errorf("converting a zero-value Amount = %v, want ErrNoRate", err)
	}
	if _, err := c.Convert(context.Background(), usd(t, "1"), ""); !errors.Is(err, ErrNoRate) {
		t.Errorf("converting to an empty currency = %v, want ErrNoRate", err)
	}
}

func TestInvertingAZeroRateIsAnErrorNotAnInfinity(t *testing.T) {
	r := at(now, money.USD, money.ZAR, "0")
	if _, err := r.Inverse(); !errors.Is(err, ErrNoRate) {
		t.Fatalf("Inverse of a zero rate = %v, want ErrNoRate", err)
	}
}

func TestAnInvertedRateSaysSoInItsProvenance(t *testing.T) {
	// Whoever reads a stored valuation should be able to tell an observed rate
	// from a reciprocal one.
	r := at(now, money.USD, money.ZAR, "20")
	inv, err := r.Inverse()
	if err != nil {
		t.Fatalf("Inverse: %v", err)
	}
	if inv.Base != money.ZAR || inv.Quote != money.USD {
		t.Errorf("inverse pair = %s/%s, want ZAR/USD", inv.Base, inv.Quote)
	}
	if inv.Provider == r.Provider {
		t.Error("the inverted rate kept the original provider string verbatim, so a " +
			"reader cannot tell it was derived rather than observed")
	}
	if !inv.SourceTime.Equal(r.SourceTime) {
		t.Error("inverting changed the rate's age")
	}
}

func TestMustConvertOrZeroReturnsATypedZeroOnFailure(t *testing.T) {
	// It exists for display aggregation only. The contract that matters is
	// that the zero carries the TARGET currency, so it can be summed with
	// converted legs instead of panicking on an empty currency.
	src := &fakeSource{rates: map[string]Rate{}}
	c := newTestConverter(src, time.Hour)

	got := c.MustConvertOrZero(context.Background(), usd(t, "100"), money.ZAR)
	if got.Currency() != money.ZAR {
		t.Errorf("currency = %q, want ZAR", got.Currency())
	}
	if !got.IsZero() {
		t.Errorf("value = %s, want 0", got.Decimal())
	}
}

func TestADefaultMaxAgeIsAppliedWhenNoneIsGiven(t *testing.T) {
	// A zero MaxAge would mean every rate is instantly stale and nothing can
	// ever be valued, which is a worse failure than a generous default.
	c := NewConverter(&fakeSource{}, 0, nil)
	if c.MaxAge <= 0 {
		t.Fatalf("MaxAge = %s, want a positive default", c.MaxAge)
	}
}

func TestConversionIsExactAtLedgerPrecision(t *testing.T) {
	// Money never becomes a float. A rate with more decimals than the amount
	// must not round mid-calculation.
	src := &fakeSource{rates: map[string]Rate{
		"USD/ZAR": at(now, money.USD, money.ZAR, "18.2537"),
	}}
	c := newTestConverter(src, time.Hour)

	conv, err := c.Convert(context.Background(), usd(t, "3.33"), money.ZAR)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := decimal.RequireFromString("3.33").Mul(decimal.RequireFromString("18.2537"))
	if !conv.To.Decimal().Equal(want) {
		t.Errorf("converted = %s, want the exact product %s", conv.To.Decimal(), want)
	}
}
