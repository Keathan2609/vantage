package domain

import (
	"testing"
	"time"
)

func baseQuote(now time.Time) Quote {
	return Quote{
		InstrumentID: "XAUUSD",
		Symbol:       "XAUUSD",
		Bid:          dec("2000.00"),
		Ask:          dec("2000.30"),
		SourceTime:   now,
		IngestedAt:   now,
		Provider:     "mock",
	}
}

func TestEvaluateQuoteHealth_FreshQuoteIsOK(t *testing.T) {
	now := time.Date(2025, 7, 8, 12, 0, 0, 0, time.UTC)
	h := EvaluateQuoteHealth(baseQuote(now), nil, DefaultDataQualityPolicy(), now)
	if h.State != DataQualityOK {
		t.Fatalf("state = %s (issues %v), want ok", h.State, h.Issues)
	}
	if !h.Healthy() {
		t.Error("a fresh, well-formed quote must be tradable by automation")
	}
}

func TestEvaluateQuoteHealth_StalenessFailsClosed(t *testing.T) {
	now := time.Date(2025, 7, 8, 12, 0, 0, 0, time.UTC)
	policy := DefaultDataQualityPolicy()

	q := baseQuote(now.Add(-11 * time.Second))
	h := EvaluateQuoteHealth(q, nil, policy, now)
	if h.State != DataQualityStale {
		t.Fatalf("state = %s, want stale", h.State)
	}
	if h.Healthy() {
		t.Error("a stale feed must not be tradable by automation")
	}
	if !h.HasIssue(IssueStaleQuote) {
		t.Errorf("expected stale_quote issue, got %v", h.Issues)
	}

	// Just inside the threshold is degraded (latency noted) but not stale.
	h2 := EvaluateQuoteHealth(baseQuote(now.Add(-4*time.Second)), nil, policy, now)
	if h2.State != DataQualityDegraded {
		t.Errorf("4s-old quote: state = %s, want degraded", h2.State)
	}
	if h2.Healthy() {
		t.Error("degraded feeds must also fail closed for automation")
	}
}

func TestEvaluateQuoteHealth_StructuralInvalidityOutranksFreshness(t *testing.T) {
	now := time.Date(2025, 7, 8, 12, 0, 0, 0, time.UTC)
	policy := DefaultDataQualityPolicy()

	cases := []struct {
		name  string
		mut   func(*Quote)
		issue DataQualityIssue
	}{
		{"crossed book", func(q *Quote) { q.Bid, q.Ask = dec("2001"), dec("2000") }, IssueCrossedBook},
		{"zero bid", func(q *Quote) { q.Bid = dec("0") }, IssueNonPositivePrice},
		{"negative ask", func(q *Quote) { q.Ask = dec("-1") }, IssueNonPositivePrice},
		{"future timestamp", func(q *Quote) { q.SourceTime = now.Add(30 * time.Second) }, IssueFutureTimestamp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := baseQuote(now)
			c.mut(&q)
			h := EvaluateQuoteHealth(q, nil, policy, now)
			if h.State != DataQualityInvalid {
				t.Fatalf("state = %s, want invalid", h.State)
			}
			if !h.HasIssue(c.issue) {
				t.Errorf("expected issue %s, got %v", c.issue, h.Issues)
			}
			if h.Healthy() {
				t.Error("invalid market data must never be tradable")
			}
		})
	}
}

func TestEvaluateQuoteHealth_TimestampRegressionAndDuplicates(t *testing.T) {
	now := time.Date(2025, 7, 8, 12, 0, 0, 0, time.UTC)
	policy := DefaultDataQualityPolicy()

	prev := baseQuote(now)
	regressed := baseQuote(now)
	regressed.SourceTime = now.Add(-5 * time.Second)
	h := EvaluateQuoteHealth(regressed, &prev, policy, now)
	if h.State != DataQualityInvalid || !h.HasIssue(IssueTimestampRegressed) {
		t.Errorf("out-of-order quote: state = %s issues = %v", h.State, h.Issues)
	}

	dup := baseQuote(now)
	h2 := EvaluateQuoteHealth(dup, &prev, policy, now)
	if !h2.HasIssue(IssueDuplicateTick) {
		t.Errorf("expected duplicate_tick, got %v", h2.Issues)
	}
	if h2.Healthy() {
		t.Error("a duplicated tick degrades the feed and must fail closed")
	}
}

func TestEvaluateQuoteHealth_AbnormalSpread(t *testing.T) {
	now := time.Date(2025, 7, 8, 12, 0, 0, 0, time.UTC)
	q := baseQuote(now)
	q.Ask = dec("2050.00") // 2.5% spread, far beyond the 0.5% policy
	h := EvaluateQuoteHealth(q, nil, DefaultDataQualityPolicy(), now)
	if !h.HasIssue(IssueSpreadAbnormal) {
		t.Errorf("expected spread_abnormal, got %v", h.Issues)
	}
	if h.Healthy() {
		t.Error("an abnormal spread must block automated trading")
	}
}

func TestQuote_ExecutionPriceCrossesTheSpread(t *testing.T) {
	q := Quote{Bid: dec("2000.00"), Ask: dec("2000.30")}
	if got := q.ExecutionPrice(SideBuy); !got.Equal(dec("2000.30")) {
		t.Errorf("buy executes at %s, want the ask 2000.30", got)
	}
	if got := q.ExecutionPrice(SideSell); !got.Equal(dec("2000.00")) {
		t.Errorf("sell executes at %s, want the bid 2000.00", got)
	}
	if !q.Mid().Equal(dec("2000.15")) {
		t.Errorf("mid = %s, want 2000.15", q.Mid())
	}
}

func TestTimeframeDuration(t *testing.T) {
	if d, err := TF15m.Duration(); err != nil || d != 15*time.Minute {
		t.Errorf("15m = %v, %v", d, err)
	}
	if _, err := Timeframe("3s").Duration(); err == nil {
		t.Error("unknown timeframes must error rather than default")
	}
}
