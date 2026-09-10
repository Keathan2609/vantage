package domain

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Rolling correlation, and the reason UNKNOWN is a first-class answer.
//
// # Why zero is the dangerous default
//
// A portfolio risk check that treats an unmeasurable correlation as zero
// concludes that two positions are independent. That is not a neutral
// assumption -- it is the most permissive one available, and it is wrong
// exactly when it matters: early in an account's life, after a restart, or
// when a new instrument has no history. Gold and silver do not become
// independent because nobody has measured them yet.
//
// So a correlation is either KNOWN, with a coefficient and a sample count, or
// it is not, and the two are different types of answer. Nothing in this file
// returns a coefficient a caller could mistake for a measurement.
//
// # Why staleness is separate from insufficiency
//
// Too few samples means the question cannot be answered. A stale answer means
// it was answered, some time ago, about a market that may since have changed.
// A caller may reasonably treat those differently -- a stale +0.9 is still
// evidence that two instruments move together -- so they are not collapsed.

// CorrelationState says what kind of answer this is.
type CorrelationState string

const (
	// CorrelationKnown means the coefficient is a measurement.
	CorrelationKnown CorrelationState = "KNOWN"
	// CorrelationInsufficientData means there were not enough overlapping
	// observations. The coefficient is meaningless and must not be read.
	CorrelationInsufficientData CorrelationState = "INSUFFICIENT_DATA"
	// CorrelationStale means the measurement is older than the policy allows.
	// The coefficient is still there, because an old measurement is still
	// information, but it is labelled.
	CorrelationStale CorrelationState = "STALE"
	// CorrelationUndefined means one of the two series did not move at all
	// over the window, so its variance is zero and correlation is not defined.
	//
	// A distinct state rather than a zero coefficient: "these never move
	// together" and "one of these never moved" are different facts, and only
	// the first is a statement about their relationship.
	CorrelationUndefined CorrelationState = "UNDEFINED"
)

// Correlation is one pairwise result.
type Correlation struct {
	A, B  string
	State CorrelationState
	// Coefficient is only a measurement when State is KNOWN or STALE. It is
	// deliberately NOT zeroed in the other states, so that a caller reading it
	// without checking gets an obviously unusable value rather than a
	// plausible one.
	Coefficient decimal.Decimal
	Samples     int
	Lookback    time.Duration
	ComputedAt  time.Time
}

// Known reports whether the coefficient may be relied on.
//
// STALE is deliberately excluded: a caller that wants to use an old
// measurement must say so explicitly by looking at State, which makes the
// choice visible in the code that makes it.
func (c Correlation) Known() bool { return c.State == CorrelationKnown }

// Usable reports whether there is a coefficient at all, stale or not.
func (c Correlation) Usable() bool {
	return c.State == CorrelationKnown || c.State == CorrelationStale
}

// Describe explains the result in one sentence.
func (c Correlation) Describe() string {
	switch c.State {
	case CorrelationKnown:
		return fmt.Sprintf("%s and %s moved together at %s over %d observations",
			c.A, c.B, c.Coefficient.StringFixed(3), c.Samples)
	case CorrelationStale:
		return fmt.Sprintf("%s and %s measured at %s over %d observations, but the "+
			"measurement is older than the policy allows",
			c.A, c.B, c.Coefficient.StringFixed(3), c.Samples)
	case CorrelationInsufficientData:
		return fmt.Sprintf("%s and %s: only %d overlapping observations, too few to measure",
			c.A, c.B, c.Samples)
	case CorrelationUndefined:
		return fmt.Sprintf("%s and %s: one of them did not move over the window, so "+
			"correlation is not defined", c.A, c.B)
	default:
		return fmt.Sprintf("%s and %s: no result", c.A, c.B)
	}
}

// CorrelationPolicy configures measurement.
type CorrelationPolicy struct {
	Version string
	// Lookback is the window the observations must fall inside.
	Lookback time.Duration
	// MinSamples is the fewest overlapping observations that support a
	// coefficient. Below it the answer is INSUFFICIENT_DATA.
	//
	// Not a statistical significance test: this is a floor below which the
	// number would be noise dressed as evidence.
	MinSamples int
	// MaxAge is how old a measurement may be before it is STALE.
	MaxAge time.Duration
}

// DefaultCorrelationPolicy is the shipped configuration.
//
// Thirty observations over thirty days. Development defaults, not the output
// of a study, and this comment is the honest place to say so.
func DefaultCorrelationPolicy() CorrelationPolicy {
	return CorrelationPolicy{
		Version:    "correlation-v1",
		Lookback:   30 * 24 * time.Hour,
		MinSamples: 30,
		MaxAge:     6 * time.Hour,
	}
}

// ReturnPoint is one observation of a return, at a time.
//
// Returns rather than prices, deliberately. Two instruments both drifting
// upwards over a year are correlated in price whatever they do day to day, and
// a risk check wants to know whether they move together NOW.
type ReturnPoint struct {
	At     time.Time
	Return decimal.Decimal
}

// CorrelateReturns measures one pair. A pure function.
//
// Observations are paired by timestamp: a return in one series with no
// counterpart in the other is dropped rather than aligned to its nearest
// neighbour. Aligning by proximity would manufacture correlation out of
// sampling artefacts, and the failure would be invisible.
func CorrelateReturns(a, b string, seriesA, seriesB []ReturnPoint,
	now time.Time, policy CorrelationPolicy) Correlation {

	result := Correlation{
		A: a, B: b, Lookback: policy.Lookback, ComputedAt: now,
		State: CorrelationInsufficientData,
	}

	cutoff := now.Add(-policy.Lookback)
	index := make(map[int64]decimal.Decimal, len(seriesA))
	for _, p := range seriesA {
		if p.At.Before(cutoff) || p.At.After(now) {
			continue
		}
		index[p.At.UTC().UnixMilli()] = p.Return
	}

	type pair struct {
		x, y float64
	}
	pairs := make([]pair, 0, len(seriesB))
	for _, p := range seriesB {
		if p.At.Before(cutoff) || p.At.After(now) {
			continue
		}
		x, ok := index[p.At.UTC().UnixMilli()]
		if !ok {
			continue
		}
		xf, _ := x.Float64()
		yf, _ := p.Return.Float64()
		if math.IsNaN(xf) || math.IsNaN(yf) || math.IsInf(xf, 0) || math.IsInf(yf, 0) {
			continue
		}
		pairs = append(pairs, pair{xf, yf})
	}

	result.Samples = len(pairs)
	if result.Samples < policy.MinSamples {
		return result
	}

	// Pearson, computed in float64 and reported as a bounded decimal.
	//
	// Float is acceptable here and nowhere near money: this is a statistic in
	// [-1, 1] used to compare against a threshold, never summed into a
	// balance. The result is rounded to four places so two runs over the same
	// observations produce the same string.
	var sumX, sumY float64
	for _, p := range pairs {
		sumX += p.x
		sumY += p.y
	}
	n := float64(len(pairs))
	meanX, meanY := sumX/n, sumY/n

	var cov, varX, varY float64
	for _, p := range pairs {
		dx, dy := p.x-meanX, p.y-meanY
		cov += dx * dy
		varX += dx * dx
		varY += dy * dy
	}

	// A series that did not move has no variance, and dividing by it would
	// produce NaN or Inf. That is a real and different answer.
	if varX <= 0 || varY <= 0 {
		result.State = CorrelationUndefined
		return result
	}

	r := cov / math.Sqrt(varX*varY)
	// Clamp: floating-point error can push a perfect correlation to 1.0000001,
	// and a coefficient outside [-1, 1] would fail every sanity check
	// downstream for a reason that has nothing to do with the market.
	r = math.Max(-1, math.Min(1, r))

	result.Coefficient = decimal.NewFromFloat(r).Round(4)
	result.State = CorrelationKnown
	if policy.MaxAge > 0 {
		// Age is measured from the NEWEST observation, not from now: a
		// measurement computed a second ago over month-old data is stale, and
		// computing it again does not refresh it.
		newest := time.Time{}
		for _, p := range seriesA {
			if p.At.After(newest) {
				newest = p.At
			}
		}
		if !newest.IsZero() && now.Sub(newest) > policy.MaxAge {
			result.State = CorrelationStale
		}
	}
	return result
}

// CorrelationMatrix is every measured pair, keyed for lookup either way round.
type CorrelationMatrix struct {
	Policy     CorrelationPolicy
	ComputedAt time.Time
	pairs      map[string]Correlation
}

// NewCorrelationMatrix measures every pair in a set of series.
func NewCorrelationMatrix(series map[string][]ReturnPoint, now time.Time,
	policy CorrelationPolicy) *CorrelationMatrix {

	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	// Sorted so the matrix is built in a deterministic order. Nothing here
	// depends on it today, but a replay that produced pairs in map order would
	// be non-reproducible for no visible reason.
	sort.Strings(keys)

	m := &CorrelationMatrix{
		Policy: policy, ComputedAt: now,
		pairs: make(map[string]Correlation, len(keys)*len(keys)/2),
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			a, b := keys[i], keys[j]
			m.pairs[pairKey(a, b)] = CorrelateReturns(a, b, series[a], series[b], now, policy)
		}
	}
	return m
}

// Get returns the pair in either order.
//
// A missing pair is INSUFFICIENT_DATA rather than a zero-valued Correlation,
// so a caller that asks about an instrument the matrix never saw gets the same
// honest answer as one that asks about a pair with too little history.
func (m *CorrelationMatrix) Get(a, b string) Correlation {
	if a == b {
		// An instrument is perfectly correlated with itself. Stating it
		// explicitly stops a caller special-casing the self-pair and getting
		// INSUFFICIENT_DATA for something that is definitionally 1.
		return Correlation{
			A: a, B: b, State: CorrelationKnown,
			Coefficient: decimal.NewFromInt(1),
			Lookback:    m.Policy.Lookback, ComputedAt: m.ComputedAt,
		}
	}
	if c, ok := m.pairs[pairKey(a, b)]; ok {
		return c
	}
	return Correlation{
		A: a, B: b, State: CorrelationInsufficientData,
		Lookback: m.Policy.Lookback, ComputedAt: m.ComputedAt,
	}
}

// Pairs returns every measured pair in a deterministic order.
func (m *CorrelationMatrix) Pairs() []Correlation {
	out := make([]Correlation, 0, len(m.pairs))
	for _, c := range m.pairs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}
