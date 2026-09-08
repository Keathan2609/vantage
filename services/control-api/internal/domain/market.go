package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Quote is a two-sided price for an instrument at a point in time.
//
// Two timestamps are always carried. SourceTime is what the data provider says;
// IngestedAt is when Vantage received it. Freshness decisions use both: a
// provider that stamps its own clock forward cannot make stale data look live,
// and a provider whose clock lags cannot make live data look stale.
type Quote struct {
	InstrumentID string
	Symbol       string
	Bid          decimal.Decimal
	Ask          decimal.Decimal
	SourceTime   time.Time
	IngestedAt   time.Time
	Provider     string
}

// Mid returns the midpoint price.
func (q Quote) Mid() decimal.Decimal {
	return q.Bid.Add(q.Ask).Div(decimal.NewFromInt(2))
}

// Spread returns ask-bid in price units.
func (q Quote) Spread() decimal.Decimal { return q.Ask.Sub(q.Bid) }

// SpreadFraction returns the spread as a fraction of the mid price. This is the
// form risk limits are expressed in, so the same threshold is meaningful for a
// $2,000 gold price and a 1.08 EURUSD price.
func (q Quote) SpreadFraction() decimal.Decimal {
	mid := q.Mid()
	if mid.IsZero() {
		return decimal.Zero
	}
	return q.Spread().Div(mid)
}

// Age returns how long ago the quote was ingested, relative to now.
func (q Quote) Age(now time.Time) time.Duration { return now.Sub(q.IngestedAt) }

// ExecutionPrice returns the side of the book an order of the given side
// crosses: a buy lifts the ask, a sell hits the bid. Using the mid for
// execution is a classic backtest-inflating error and is never done here.
func (q Quote) ExecutionPrice(side OrderSide) decimal.Decimal {
	if side == SideBuy {
		return q.Ask
	}
	return q.Bid
}

// Bar is an OHLCV candle over a fixed timeframe. OpenTime is the inclusive
// start of the interval and CloseTime the exclusive end, both UTC.
type Bar struct {
	InstrumentID string
	Timeframe    Timeframe
	OpenTime     time.Time
	CloseTime    time.Time
	Open         decimal.Decimal
	High         decimal.Decimal
	Low          decimal.Decimal
	Close        decimal.Decimal
	Volume       decimal.Decimal
	Complete     bool
	Provider     string
}

// Timeframe is a bar interval.
type Timeframe string

const (
	TF1m  Timeframe = "1m"
	TF5m  Timeframe = "5m"
	TF15m Timeframe = "15m"
	TF1h  Timeframe = "1h"
	TF4h  Timeframe = "4h"
	TF1d  Timeframe = "1d"
)

// Duration returns the wall-clock length of one bar.
func (t Timeframe) Duration() (time.Duration, error) {
	switch t {
	case TF1m:
		return time.Minute, nil
	case TF5m:
		return 5 * time.Minute, nil
	case TF15m:
		return 15 * time.Minute, nil
	case TF1h:
		return time.Hour, nil
	case TF4h:
		return 4 * time.Hour, nil
	case TF1d:
		return 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown timeframe %q", t)
	}
}

// ParseTimeframe validates a timeframe string.
func ParseTimeframe(s string) (Timeframe, error) {
	tf := Timeframe(s)
	if _, err := tf.Duration(); err != nil {
		return "", err
	}
	return tf, nil
}

// DataQualityState classifies a feed's usability for automated trading.
type DataQualityState string

const (
	DataQualityOK       DataQualityState = "ok"
	DataQualityDegraded DataQualityState = "degraded"
	DataQualityStale    DataQualityState = "stale"
	DataQualityInvalid  DataQualityState = "invalid"
	DataQualityNoData   DataQualityState = "no_data"
)

// TradableForAutomation reports whether automated execution may proceed. Only
// a fully healthy feed qualifies: degraded, stale, invalid and absent feeds all
// fail closed. Manual paper trading may still be permitted under a degraded
// feed, with the state surfaced in the UI.
func (s DataQualityState) TradableForAutomation() bool { return s == DataQualityOK }

// DataQualityIssue is a specific defect detected in a feed.
type DataQualityIssue string

const (
	IssueStaleQuote         DataQualityIssue = "stale_quote"
	IssueCrossedBook        DataQualityIssue = "crossed_book"
	IssueNonPositivePrice   DataQualityIssue = "non_positive_price"
	IssueSpreadAbnormal     DataQualityIssue = "spread_abnormal"
	IssueTimestampRegressed DataQualityIssue = "timestamp_regressed"
	IssueDuplicateTick      DataQualityIssue = "duplicate_tick"
	IssueMissingBars        DataQualityIssue = "missing_bars"
	IssueProviderDown       DataQualityIssue = "provider_down"
	IssueExcessiveLatency   DataQualityIssue = "excessive_latency"
	IssueFutureTimestamp    DataQualityIssue = "future_timestamp"
)

// MarketDataHealth is the freshness and integrity verdict for one instrument.
// The order pipeline consults this before any automated order is accepted.
type MarketDataHealth struct {
	InstrumentID string
	Symbol       string
	State        DataQualityState
	Issues       []DataQualityIssue
	LastQuoteAt  time.Time
	QuoteAge     time.Duration
	Spread       decimal.Decimal
	SpreadPct    decimal.Decimal
	Provider     string
	EvaluatedAt  time.Time
}

// Healthy reports whether automation may use this feed.
func (h MarketDataHealth) Healthy() bool { return h.State.TradableForAutomation() }

// HasIssue reports whether a specific defect was detected.
func (h MarketDataHealth) HasIssue(i DataQualityIssue) bool {
	for _, v := range h.Issues {
		if v == i {
			return true
		}
	}
	return false
}

// DataQualityPolicy holds the thresholds used to classify a feed.
type DataQualityPolicy struct {
	// MaxQuoteAge beyond which a quote is stale and automation stops.
	MaxQuoteAge time.Duration
	// DegradedQuoteAge beyond which a quote is suspect but not yet stale.
	DegradedQuoteAge time.Duration
	// MaxSpreadFraction beyond which the book is considered abnormal.
	MaxSpreadFraction decimal.Decimal
	// MaxClockSkewAhead tolerated when a provider timestamps into the future.
	MaxClockSkewAhead time.Duration
}

// DefaultDataQualityPolicy is deliberately conservative. A five-second-old
// quote is not tradable by an automated system that can act in milliseconds.
func DefaultDataQualityPolicy() DataQualityPolicy {
	return DataQualityPolicy{
		MaxQuoteAge:       10 * time.Second,
		DegradedQuoteAge:  3 * time.Second,
		MaxSpreadFraction: decimal.NewFromFloat(0.005), // 0.5% of mid
		MaxClockSkewAhead: 2 * time.Second,
	}
}

// ErrNoQuote indicates no quote exists for an instrument.
var ErrNoQuote = errors.New("no quote available for instrument")

// EvaluateQuoteHealth classifies a single quote against the policy. It is a
// pure function of its inputs so it can be exhaustively unit-tested, including
// the boundary conditions that decide whether real money may move.
func EvaluateQuoteHealth(q Quote, prev *Quote, policy DataQualityPolicy, now time.Time) MarketDataHealth {
	h := MarketDataHealth{
		InstrumentID: q.InstrumentID,
		Symbol:       q.Symbol,
		LastQuoteAt:  q.IngestedAt,
		QuoteAge:     now.Sub(q.IngestedAt),
		Spread:       q.Spread(),
		SpreadPct:    q.SpreadFraction(),
		Provider:     q.Provider,
		EvaluatedAt:  now,
		State:        DataQualityOK,
	}

	// Structural invalidity outranks staleness: a crossed or non-positive book
	// is never usable regardless of how recently it arrived.
	if q.Bid.LessThanOrEqual(decimal.Zero) || q.Ask.LessThanOrEqual(decimal.Zero) {
		h.Issues = append(h.Issues, IssueNonPositivePrice)
		h.State = DataQualityInvalid
	}
	if q.Bid.GreaterThan(q.Ask) {
		h.Issues = append(h.Issues, IssueCrossedBook)
		h.State = DataQualityInvalid
	}
	if q.SourceTime.After(now.Add(policy.MaxClockSkewAhead)) {
		h.Issues = append(h.Issues, IssueFutureTimestamp)
		h.State = DataQualityInvalid
	}
	if prev != nil && q.SourceTime.Before(prev.SourceTime) {
		h.Issues = append(h.Issues, IssueTimestampRegressed)
		h.State = DataQualityInvalid
	}
	if prev != nil && q.SourceTime.Equal(prev.SourceTime) &&
		q.Bid.Equal(prev.Bid) && q.Ask.Equal(prev.Ask) {
		h.Issues = append(h.Issues, IssueDuplicateTick)
		if h.State == DataQualityOK {
			h.State = DataQualityDegraded
		}
	}
	if h.State == DataQualityInvalid {
		return h
	}

	if h.QuoteAge > policy.MaxQuoteAge {
		h.Issues = append(h.Issues, IssueStaleQuote)
		h.State = DataQualityStale
		return h
	}
	if policy.MaxSpreadFraction.IsPositive() && h.SpreadPct.GreaterThan(policy.MaxSpreadFraction) {
		h.Issues = append(h.Issues, IssueSpreadAbnormal)
		h.State = DataQualityDegraded
	}
	if h.QuoteAge > policy.DegradedQuoteAge {
		h.Issues = append(h.Issues, IssueExcessiveLatency)
		if h.State == DataQualityOK {
			h.State = DataQualityDegraded
		}
	}
	return h
}
