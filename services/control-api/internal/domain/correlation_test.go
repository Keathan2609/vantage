package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The point of these tests is the one thing a correlation implementation can
// get catastrophically wrong: reporting zero when it means "unknown". A risk
// check reading zero concludes two positions are independent, which is the
// most permissive conclusion available and is wrong exactly when history is
// thin -- early in an account's life, after a restart, or for a new
// instrument.

var corrNow = time.Date(2027, 3, 10, 12, 0, 0, 0, time.UTC)

// series builds n hourly observations ending an hour before `corrNow`, each
// return produced by f.
func series(n int, f func(i int) string) []ReturnPoint {
	out := make([]ReturnPoint, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ReturnPoint{
			At:     corrNow.Add(-time.Duration(n-i) * time.Hour),
			Return: decimal.RequireFromString(f(i)),
		})
	}
	return out
}

func TestPerfectPositiveCorrelation(t *testing.T) {
	// Two instruments moving identically. The number itself matters less than
	// that it lands in the range a reject threshold can act on.
	a := series(60, func(i int) string { return []string{"0.01", "-0.02", "0.03", "-0.01"}[i%4] })
	b := series(60, func(i int) string { return []string{"0.02", "-0.04", "0.06", "-0.02"}[i%4] })

	c := CorrelateReturns("XAUUSD", "XAGUSD", a, b, corrNow, DefaultCorrelationPolicy())
	if !c.Known() {
		t.Fatalf("state = %s, want KNOWN: %s", c.State, c.Describe())
	}
	if c.Coefficient.LessThan(decimal.RequireFromString("0.99")) {
		t.Errorf("coefficient = %s, want ~1", c.Coefficient)
	}
	if c.Samples != 60 {
		t.Errorf("samples = %d, want 60", c.Samples)
	}
}

func TestPerfectNegativeCorrelation(t *testing.T) {
	a := series(60, func(i int) string { return []string{"0.01", "-0.02", "0.03", "-0.01"}[i%4] })
	b := series(60, func(i int) string { return []string{"-0.01", "0.02", "-0.03", "0.01"}[i%4] })

	c := CorrelateReturns("XAUUSD", "USDZAR", a, b, corrNow, DefaultCorrelationPolicy())
	if !c.Known() {
		t.Fatalf("state = %s, want KNOWN", c.State)
	}
	if c.Coefficient.GreaterThan(decimal.RequireFromString("-0.99")) {
		t.Errorf("coefficient = %s, want ~-1", c.Coefficient)
	}
}

func TestNearZeroCorrelation(t *testing.T) {
	// Alternating against constant-magnitude opposite pairs: unrelated by
	// construction. The assertion is loose because the point is "not
	// correlated", not a specific number.
	a := series(80, func(i int) string { return []string{"0.01", "-0.01"}[i%2] })
	b := series(80, func(i int) string { return []string{"0.01", "0.01", "-0.01", "-0.01"}[i%4] })

	c := CorrelateReturns("A", "B", a, b, corrNow, DefaultCorrelationPolicy())
	if !c.Known() {
		t.Fatalf("state = %s, want KNOWN", c.State)
	}
	if c.Coefficient.Abs().GreaterThan(decimal.RequireFromString("0.30")) {
		t.Errorf("coefficient = %s, want near zero", c.Coefficient)
	}
}

func TestInsufficientSamplesIsNotZero(t *testing.T) {
	// THE test. Ten observations cannot answer the question, and the answer
	// must not be a coefficient that reads as "independent".
	a := series(10, func(i int) string { return "0.01" })
	b := series(10, func(i int) string { return "0.02" })

	c := CorrelateReturns("A", "B", a, b, corrNow, DefaultCorrelationPolicy())
	if c.State != CorrelationInsufficientData {
		t.Fatalf("state = %s, want INSUFFICIENT_DATA", c.State)
	}
	if c.Known() {
		t.Error("an unmeasurable correlation reported itself as known")
	}
	if c.Usable() {
		t.Error("an unmeasurable correlation reported itself as usable")
	}
	if c.Samples != 10 {
		t.Errorf("samples = %d, want the 10 that were found", c.Samples)
	}
	if !strings.Contains(c.Describe(), "too few") {
		t.Errorf("description does not say why: %q", c.Describe())
	}
}

func TestNonOverlappingSeriesHaveNoSamples(t *testing.T) {
	// Two series measured at different instants overlap nowhere. Aligning them
	// by proximity would manufacture a correlation out of a sampling
	// artefact, and the failure would be invisible.
	a := series(60, func(i int) string { return "0.01" })
	b := make([]ReturnPoint, 0, 60)
	for _, p := range series(60, func(i int) string { return "0.02" }) {
		b = append(b, ReturnPoint{At: p.At.Add(17 * time.Minute), Return: p.Return})
	}

	c := CorrelateReturns("A", "B", a, b, corrNow, DefaultCorrelationPolicy())
	if c.State != CorrelationInsufficientData {
		t.Fatalf("state = %s, want INSUFFICIENT_DATA", c.State)
	}
	if c.Samples != 0 {
		t.Errorf("samples = %d, want 0: nothing overlapped", c.Samples)
	}
}

func TestAFlatSeriesIsUndefinedRatherThanUncorrelated(t *testing.T) {
	// "These never move together" and "one of these never moved" are
	// different facts, and only the first is a statement about a
	// relationship. Reporting zero for the second would be a measurement that
	// was never taken.
	a := series(60, func(i int) string { return []string{"0.01", "-0.01"}[i%2] })
	flat := series(60, func(i int) string { return "0" })

	c := CorrelateReturns("A", "FLAT", a, flat, corrNow, DefaultCorrelationPolicy())
	if c.State != CorrelationUndefined {
		t.Fatalf("state = %s, want UNDEFINED", c.State)
	}
	if c.Known() || c.Usable() {
		t.Error("an undefined correlation reported itself usable")
	}
	if !strings.Contains(c.Describe(), "did not move") {
		t.Errorf("description does not say why: %q", c.Describe())
	}
}

func TestObservationsOutsideTheLookbackAreExcluded(t *testing.T) {
	// A window that silently included everything would make the lookback
	// configuration decorative, and a correlation measured over a year would
	// be reported as one measured over a month.
	policy := DefaultCorrelationPolicy()
	policy.Lookback = 24 * time.Hour

	a := series(60, func(i int) string { return "0.01" }) // 60 hours back
	b := series(60, func(i int) string { return "0.02" })

	c := CorrelateReturns("A", "B", a, b, corrNow, policy)
	if c.Samples > 25 {
		t.Errorf("samples = %d over a 24-hour lookback of hourly data", c.Samples)
	}
}

func TestAnOldMeasurementIsStaleAndNotSilentlyFresh(t *testing.T) {
	// Age is measured from the newest OBSERVATION, not from when the
	// calculation ran. Recomputing over month-old data does not refresh it,
	// and a caller told "computed one second ago" would be misled.
	policy := DefaultCorrelationPolicy()
	policy.MaxAge = time.Hour

	// 60 hourly points ending 40 hours ago.
	shift := 40 * time.Hour
	a := make([]ReturnPoint, 0, 60)
	b := make([]ReturnPoint, 0, 60)
	for i := 0; i < 60; i++ {
		at := corrNow.Add(-shift - time.Duration(60-i)*time.Hour)
		a = append(a, ReturnPoint{At: at, Return: decimal.RequireFromString([]string{"0.01", "-0.02"}[i%2])})
		b = append(b, ReturnPoint{At: at, Return: decimal.RequireFromString([]string{"0.02", "-0.04"}[i%2])})
	}

	c := CorrelateReturns("A", "B", a, b, corrNow, policy)
	if c.State != CorrelationStale {
		t.Fatalf("state = %s, want STALE", c.State)
	}
	// The coefficient survives, because an old measurement is still evidence.
	if c.Coefficient.IsZero() {
		t.Error("a stale measurement discarded its coefficient")
	}
	if !c.Usable() {
		t.Error("a stale measurement is not usable, so old evidence is thrown away")
	}
	if c.Known() {
		t.Error("a stale measurement reported itself as KNOWN")
	}
}

func TestChangingCorrelationIsReflectedInTheWindow(t *testing.T) {
	// Historical correlation is information, not certainty. A pair that used
	// to move together and no longer does must measure differently over the
	// recent window than over the whole history -- otherwise the lookback is
	// measuring the past and reporting it as the present.
	//
	// First half: identical. Second half: opposed.
	n := 120
	a := make([]ReturnPoint, 0, n)
	b := make([]ReturnPoint, 0, n)
	for i := 0; i < n; i++ {
		at := corrNow.Add(-time.Duration(n-i) * time.Hour)
		move := []string{"0.01", "-0.02", "0.03"}[i%3]
		a = append(a, ReturnPoint{At: at, Return: decimal.RequireFromString(move)})
		if i < n/2 {
			b = append(b, ReturnPoint{At: at, Return: decimal.RequireFromString(move)})
		} else {
			b = append(b, ReturnPoint{At: at, Return: decimal.RequireFromString(move).Neg()})
		}
	}

	long := DefaultCorrelationPolicy()
	long.Lookback = 200 * time.Hour
	short := DefaultCorrelationPolicy()
	short.Lookback = 45 * time.Hour
	short.MinSamples = 20

	whole := CorrelateReturns("A", "B", a, b, corrNow, long)
	recent := CorrelateReturns("A", "B", a, b, corrNow, short)

	if !whole.Known() || !recent.Known() {
		t.Fatalf("states: whole %s, recent %s", whole.State, recent.State)
	}
	if !recent.Coefficient.IsNegative() {
		t.Errorf("the recent window measured %s; the pair has been opposed throughout it",
			recent.Coefficient)
	}
	if !recent.Coefficient.LessThan(whole.Coefficient) {
		t.Errorf("the recent window (%s) is not more negative than the whole history (%s), "+
			"so the lookback is not measuring the present",
			recent.Coefficient, whole.Coefficient)
	}
}

func TestTheMatrixReportsAnInstrumentAgainstItselfAsOne(t *testing.T) {
	m := NewCorrelationMatrix(map[string][]ReturnPoint{}, corrNow, DefaultCorrelationPolicy())
	c := m.Get("XAUUSD", "XAUUSD")
	if !c.Known() || !c.Coefficient.Equal(decimal.NewFromInt(1)) {
		t.Errorf("self-correlation = %s %s, want KNOWN 1", c.State, c.Coefficient)
	}
}

func TestTheMatrixReportsAnUnseenPairAsInsufficientRatherThanZero(t *testing.T) {
	// The most likely way a zero leaks into a risk decision: asking about a
	// pair the matrix never measured and getting a zero-valued struct.
	m := NewCorrelationMatrix(map[string][]ReturnPoint{}, corrNow, DefaultCorrelationPolicy())
	c := m.Get("XAUUSD", "EURUSD")
	if c.State != CorrelationInsufficientData {
		t.Fatalf("state = %s, want INSUFFICIENT_DATA", c.State)
	}
	if c.Known() || c.Usable() {
		t.Error("an unseen pair reported itself usable")
	}
}

func TestTheMatrixIsSymmetricAndDeterministic(t *testing.T) {
	input := map[string][]ReturnPoint{
		"XAUUSD": series(60, func(i int) string { return []string{"0.01", "-0.02"}[i%2] }),
		"XAGUSD": series(60, func(i int) string { return []string{"0.02", "-0.04"}[i%2] }),
		"EURUSD": series(60, func(i int) string { return []string{"-0.01", "0.01"}[i%2] }),
	}
	m := NewCorrelationMatrix(input, corrNow, DefaultCorrelationPolicy())

	if !m.Get("XAUUSD", "XAGUSD").Coefficient.Equal(m.Get("XAGUSD", "XAUUSD").Coefficient) {
		t.Error("the matrix is not symmetric")
	}
	// Determinism: map iteration order must not reach the output, or a replay
	// over the same data would produce different pairs on different runs.
	first := NewCorrelationMatrix(input, corrNow, DefaultCorrelationPolicy()).Pairs()
	second := NewCorrelationMatrix(input, corrNow, DefaultCorrelationPolicy()).Pairs()
	if len(first) != 3 {
		t.Fatalf("got %d pairs for three instruments, want 3", len(first))
	}
	for i := range first {
		if first[i].A != second[i].A || first[i].B != second[i].B ||
			!first[i].Coefficient.Equal(second[i].Coefficient) {
			t.Fatalf("pair %d differs between two identical builds", i)
		}
	}
}

func TestACoefficientNeverEscapesItsBounds(t *testing.T) {
	// Floating-point error can push a perfect correlation past 1, and a
	// coefficient outside [-1, 1] would fail every downstream sanity check
	// for a reason unrelated to the market.
	a := series(200, func(i int) string { return decimal.NewFromInt(int64(i)).Div(decimal.NewFromInt(1000)).String() })
	b := series(200, func(i int) string { return decimal.NewFromInt(int64(i)).Div(decimal.NewFromInt(1000)).String() })

	c := CorrelateReturns("A", "B", a, b, corrNow, DefaultCorrelationPolicy())
	one := decimal.NewFromInt(1)
	if c.Coefficient.GreaterThan(one) || c.Coefficient.LessThan(one.Neg()) {
		t.Errorf("coefficient = %s, outside [-1, 1]", c.Coefficient)
	}
}
