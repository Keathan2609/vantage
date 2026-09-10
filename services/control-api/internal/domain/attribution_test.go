package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// Attribution is only useful if it adds up. These tests are mostly about that:
// every ledger entry landing in exactly one bucket, the buckets summing to the
// account, and the report saying so when they do not.

func zar(s string) money.Amount {
	return money.New(decimal.RequireFromString(s), money.ZAR)
}

func truthy(b bool) *bool { return &b }

// A small ledger that exercises every routing arm: two strategies, two
// versions, a manual order, a deposit with no order at all, and costs.
func sampleLedger() []LedgerEntry {
	momentum := "11111111-1111-1111-1111-111111111111"
	reversion := "22222222-2222-2222-2222-222222222222"
	return []LedgerEntry{
		{Sequence: 1, Type: TxDeposit, Amount: zar("500.00")},
		{Sequence: 2, Type: TxRealizedPnL, Amount: zar("40.00"),
			InstrumentID: "XAUUSD.m", Symbol: "XAUUSD.m", Source: "autopilot",
			StrategyID: momentum, StrategyName: "Momentum", StrategyVersion: 1,
			Session: "london", Blackout: truthy(false)},
		{Sequence: 3, Type: TxCommission, Amount: zar("-2.50"),
			InstrumentID: "XAUUSD.m", Symbol: "XAUUSD.m", Source: "autopilot",
			StrategyID: momentum, StrategyName: "Momentum", StrategyVersion: 1,
			Session: "london", Blackout: truthy(false)},
		{Sequence: 4, Type: TxRealizedPnL, Amount: zar("-15.00"),
			InstrumentID: "XAUUSD.m", Symbol: "XAUUSD.m", Source: "autopilot",
			StrategyID: momentum, StrategyName: "Momentum", StrategyVersion: 2,
			Session: "new_york", Blackout: truthy(true)},
		{Sequence: 5, Type: TxRealizedPnL, Amount: zar("7.25"),
			InstrumentID: "EURUSD", Symbol: "EURUSD", Source: "strategy",
			StrategyID: reversion, StrategyName: "Reversion", StrategyVersion: 1,
			Session: "london_new_york_overlap", Blackout: truthy(false)},
		{Sequence: 6, Type: TxSwap, Amount: zar("-0.75"),
			InstrumentID: "EURUSD", Symbol: "EURUSD", Source: "strategy",
			StrategyID: reversion, StrategyName: "Reversion", StrategyVersion: 1,
			Session: "london_new_york_overlap", Blackout: truthy(false)},
		// A manual order: an instrument and a source, but no strategy.
		{Sequence: 7, Type: TxRealizedPnL, Amount: zar("-3.00"),
			InstrumentID: "XAUUSD.m", Symbol: "XAUUSD.m", Source: "manual"},
	}
}

// The ledger net across the sample: 500 + 40 - 2.50 - 15 + 7.25 - 0.75 - 3.
const sampleLedgerNet = "526.00"

func TestEveryEntryLandsInExactlyOneBucket(t *testing.T) {
	// The property the whole report rests on. An entry in two buckets double
	// counts money; an entry in none makes it vanish. Both produce a report
	// that looks authoritative and is wrong.
	entries := sampleLedger()
	for _, dim := range AttributionDimensions() {
		report := Attribute(dim, money.ZAR, entries, len(entries), zar(sampleLedgerNet))
		counted := 0
		for _, b := range report.Buckets {
			counted += b.Entries
		}
		if counted != len(entries) {
			t.Errorf("%s: buckets hold %d entries, want %d", dim, counted, len(entries))
		}
		if report.Total.Entries != len(entries) {
			t.Errorf("%s: total holds %d entries, want %d", dim, report.Total.Entries, len(entries))
		}
	}
}

func TestTheBucketsReconcileToTheLedgerExactly(t *testing.T) {
	// Not "approximately", and not "to two places after rounding twice".
	entries := sampleLedger()
	for _, dim := range AttributionDimensions() {
		report := Attribute(dim, money.ZAR, entries, len(entries), zar(sampleLedgerNet))
		if !report.Reconciled {
			t.Errorf("%s: not reconciled, discrepancy %s", dim, report.Discrepancy)
		}
		if !report.Discrepancy.IsZero() {
			t.Errorf("%s: discrepancy = %s, want zero", dim, report.Discrepancy)
		}
		// And the buckets themselves sum to the total, independently of the
		// ledger comparison.
		sum := money.Zero(money.ZAR)
		for _, b := range report.Buckets {
			sum = sum.MustAdd(b.Net).MustAdd(b.Other)
		}
		want := report.Total.Net.MustAdd(report.Total.Other)
		if !sum.Decimal().Equal(want.Decimal()) {
			t.Errorf("%s: buckets sum to %s, total says %s", dim, sum, want)
		}
	}
}

func TestATruncatedLedgerReportsItselfUnreconciled(t *testing.T) {
	// This is the case the field exists for. A limit that cuts the ledger
	// produces a fold that is internally consistent and is NOT the account's
	// position, and presenting it as the account's is the worst kind of
	// financial number: precise, plausible and wrong.
	entries := sampleLedger()
	report := Attribute(AttributeByStrategy, money.ZAR, entries[:3],
		len(entries), zar(sampleLedgerNet))

	if report.Reconciled {
		t.Fatal("a truncated fold reported itself reconciled")
	}
	if report.Discrepancy.IsZero() {
		t.Error("a truncated fold reported a zero discrepancy")
	}
	// And it still says what the account's own total is, so the gap is
	// quantified rather than merely flagged.
	if report.LedgerNet.StringFixed() != "526.00" {
		t.Errorf("ledger net = %s, want 526.00", report.LedgerNet.StringFixed())
	}
	if report.LedgerEntries != len(entries) {
		t.Errorf("ledger entries = %d, want %d", report.LedgerEntries, len(entries))
	}
}

func TestADepositIsNeverReportedAsPerformance(t *testing.T) {
	// Funding an account is not a profit. Counting it as one would make every
	// paper account look successful the moment it was seeded.
	report := Attribute(AttributeByStrategy, money.ZAR, sampleLedger(),
		7, zar(sampleLedgerNet))

	var unattributed *AttributionBucket
	for i := range report.Buckets {
		if report.Buckets[i].Key == UnattributedKey {
			unattributed = &report.Buckets[i]
		}
	}
	if unattributed == nil {
		t.Fatal("the deposit and the manual trade were dropped instead of bucketed")
	}
	if unattributed.Other.StringFixed() != "500.00" {
		t.Errorf("other = %s, want the 500.00 deposit", unattributed.Other.StringFixed())
	}
	// Net excludes Other on purpose.
	if unattributed.Net.StringFixed() != "-3.00" {
		t.Errorf("net = %s, want -3.00 (the manual trade alone)", unattributed.Net.StringFixed())
	}
	if report.Total.Other.StringFixed() != "500.00" {
		t.Errorf("total other = %s, want 500.00", report.Total.Other.StringFixed())
	}
	// The headline: total NET is the trading result, 526 - 500 funding.
	if report.Total.Net.StringFixed() != "26.00" {
		t.Errorf("total net = %s, want 26.00", report.Total.Net.StringFixed())
	}
}

func TestGrossAndCostsAreReportedSeparately(t *testing.T) {
	// A strategy that is profitable before costs and loses after them is the
	// most useful thing this report can find, and a single "pnl" column hides
	// it entirely.
	report := Attribute(AttributeByStrategyVersion, money.ZAR, sampleLedger(),
		7, zar(sampleLedgerNet))

	byKey := map[string]AttributionBucket{}
	for _, b := range report.Buckets {
		byKey[b.Label] = b
	}
	m1, ok := byKey["Momentum v1"]
	if !ok {
		t.Fatalf("Momentum v1 missing; got %v", byKey)
	}
	if m1.Gross.StringFixed() != "40.00" {
		t.Errorf("gross = %s, want 40.00", m1.Gross.StringFixed())
	}
	if m1.Costs.StringFixed() != "-2.50" {
		t.Errorf("costs = %s, want -2.50", m1.Costs.StringFixed())
	}
	if m1.Net.StringFixed() != "37.50" {
		t.Errorf("net = %s, want 37.50", m1.Net.StringFixed())
	}
	// And v2 is a separate bucket: without that, a promotion cannot be judged.
	m2, ok := byKey["Momentum v2"]
	if !ok {
		t.Fatal("Momentum v2 was folded into v1")
	}
	if m2.Net.StringFixed() != "-15.00" {
		t.Errorf("v2 net = %s, want -15.00", m2.Net.StringFixed())
	}
}

func TestAnUnrecognisedTransactionTypeIsNotCountedAsProfit(t *testing.T) {
	// The default arm has to be Other, not Gross. A type this code has never
	// seen is a real movement of money that is not a demonstrated trading
	// result, and misfiling it as performance is the mistake that cannot be
	// walked back.
	entries := []LedgerEntry{
		{Sequence: 1, Type: TransactionType("rebate_from_the_future"), Amount: zar("100.00")},
	}
	report := Attribute(AttributeByType, money.ZAR, entries, 1, zar("100.00"))
	if !report.Total.Gross.IsZero() {
		t.Errorf("gross = %s, want zero", report.Total.Gross.StringFixed())
	}
	if report.Total.Other.StringFixed() != "100.00" {
		t.Errorf("other = %s, want 100.00", report.Total.Other.StringFixed())
	}
	if report.Total.Trades != 0 {
		t.Errorf("trades = %d; an unknown type is not a trade", report.Total.Trades)
	}
	if !report.Reconciled {
		t.Error("an unknown type broke the reconciliation")
	}
}

func TestWinRateCountsClosingEventsAndNotCosts(t *testing.T) {
	// Commission is not a losing trade. Counting cost rows as trades would
	// halve every win rate in the platform.
	report := Attribute(AttributeByInstrument, money.ZAR, sampleLedger(),
		7, zar(sampleLedgerNet))
	byKey := map[string]AttributionBucket{}
	for _, b := range report.Buckets {
		byKey[b.Key] = b
	}
	gold := byKey["XAUUSD.m"]
	// Three realised-P&L entries on gold: +40, -15, -3. Plus one commission.
	if gold.Trades != 3 {
		t.Errorf("trades = %d, want 3", gold.Trades)
	}
	if gold.Entries != 4 {
		t.Errorf("entries = %d, want 4 (three trades and a commission)", gold.Entries)
	}
	if gold.Wins != 1 {
		t.Errorf("wins = %d, want 1", gold.Wins)
	}
	if gold.WinRate.String() != "0.3333" {
		t.Errorf("win rate = %s, want 0.3333", gold.WinRate)
	}
}

func TestTheWorstBucketIsReportedFirst(t *testing.T) {
	// What is costing money is what the report exists to surface. Burying it
	// under the winners is how it gets missed.
	report := Attribute(AttributeByStrategyVersion, money.ZAR, sampleLedger(),
		7, zar(sampleLedgerNet))
	if len(report.Buckets) < 2 {
		t.Fatalf("expected several buckets, got %d", len(report.Buckets))
	}
	for i := 1; i < len(report.Buckets); i++ {
		prev, cur := report.Buckets[i-1], report.Buckets[i]
		if prev.Net.Decimal().GreaterThan(cur.Net.Decimal()) {
			t.Fatalf("buckets out of order: %s (%s) before %s (%s)",
				prev.Key, prev.Net, cur.Key, cur.Net)
		}
	}
}

func TestEventContextSeparatesBlackoutFromClearAndFromUnknown(t *testing.T) {
	// Three states, not two. An order with no decision snapshot has an UNKNOWN
	// event context, and calling it "clear" would assert that no economic
	// event was pending when nothing of the kind was recorded.
	report := Attribute(AttributeByEventContext, money.ZAR, sampleLedger(),
		7, zar(sampleLedgerNet))
	keys := map[string]bool{}
	for _, b := range report.Buckets {
		keys[b.Key] = true
	}
	for _, want := range []string{"blackout", "clear", UnattributedKey} {
		if !keys[want] {
			t.Errorf("missing the %q bucket; got %v", want, keys)
		}
	}
}

func TestSessionAttributionUsesTheRecordedSession(t *testing.T) {
	report := Attribute(AttributeBySession, money.ZAR, sampleLedger(),
		7, zar(sampleLedgerNet))
	byKey := map[string]AttributionBucket{}
	for _, b := range report.Buckets {
		byKey[b.Key] = b
	}
	if _, ok := byKey["london_new_york_overlap"]; !ok {
		t.Errorf("the overlap session was not attributed; got %v", byKey)
	}
	// The manual trade and the deposit carry no session and must not be
	// silently assigned one.
	un, ok := byKey[UnattributedKey]
	if !ok {
		t.Fatal("entries with no recorded session were dropped")
	}
	if un.Entries != 2 {
		t.Errorf("unattributed entries = %d, want 2", un.Entries)
	}
}

func TestAnUnknownDimensionIsRefused(t *testing.T) {
	// Fails closed. Falling back to instrument would answer a question nobody
	// asked, and the caller would believe the answer.
	// "REGIME" is deliberately absent: it became a real dimension, and the
	// parser is case-insensitive, so keeping it here would assert the opposite
	// of the intended behaviour.
	for _, bad := range []string{"", "profit", "strategy_name", "../etc/passwd", "volatility"} {
		if _, err := ParseAttributionDimension(bad); !errors.Is(err, ErrUnknownDimension) {
			t.Errorf("ParseAttributionDimension(%q) = %v, want ErrUnknownDimension", bad, err)
		}
	}
	// And every advertised dimension parses, or the API offers a choice it
	// cannot honour.
	for _, d := range AttributionDimensions() {
		if _, err := ParseAttributionDimension(string(d)); err != nil {
			t.Errorf("advertised dimension %q does not parse: %v", d, err)
		}
	}
}

func TestAnEmptyLedgerReconciles(t *testing.T) {
	// A fresh account must not report itself broken.
	report := Attribute(AttributeByStrategy, money.ZAR, nil, 0, money.Amount{})
	if !report.Reconciled {
		t.Errorf("an empty ledger did not reconcile: discrepancy %s", report.Discrepancy)
	}
	if len(report.Buckets) != 0 {
		t.Errorf("buckets = %d, want none", len(report.Buckets))
	}
	// The zero Amount has an empty currency on purpose, so the report has to
	// supply the account's rather than emit "0.00 " with no currency.
	if report.LedgerNet.Currency() != money.ZAR {
		t.Errorf("ledger net currency = %q, want ZAR", report.LedgerNet.Currency())
	}
	if report.Total.Net.Currency() != money.ZAR {
		t.Errorf("total currency = %q, want ZAR", report.Total.Net.Currency())
	}
}

func TestPrimarySessionPrefersTheOverlap(t *testing.T) {
	// The overlap is the most specific answer available: attributing a trade
	// placed during it to "london" alone would lose the fact that New York was
	// also live, which is the thing that made the book deep.
	got := PrimarySession([]SessionName{SessionLondon, SessionNewYork, SessionOverlap})
	if got != SessionOverlap {
		t.Errorf("PrimarySession = %s, want %s", got, SessionOverlap)
	}
	if got := PrimarySession([]SessionName{SessionTokyo}); got != SessionTokyo {
		t.Errorf("PrimarySession = %s, want tokyo", got)
	}
	if got := PrimarySession(nil); got != SessionNoActive {
		t.Errorf("PrimarySession(nil) = %s, want none", got)
	}
	if got := PrimarySession([]SessionName{SessionNoActive}); got != SessionNoActive {
		t.Errorf("PrimarySession = %s, want none", got)
	}
}

// The PAPER_FORWARD versus BACKTEST/REPLAY split is the one a paper-forward
// programme rests on, so it gets its own tests.

func replayAndForwardLedger() []LedgerEntry {
	run := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	momentum := "11111111-1111-1111-1111-111111111111"
	return []LedgerEntry{
		{Sequence: 1, Type: TxDeposit, Amount: zar("500.00")},
		// Decided against a dataset.
		{Sequence: 2, Type: TxRealizedPnL, Amount: zar("30.00"),
			InstrumentID: "XAUUSD.m", Source: "autopilot",
			StrategyID: momentum, StrategyName: "Momentum", StrategyVersion: 1,
			ReplayRunID: run},
		{Sequence: 3, Type: TxCommission, Amount: zar("-1.00"),
			InstrumentID: "XAUUSD.m", Source: "autopilot",
			StrategyID: momentum, StrategyName: "Momentum", StrategyVersion: 1,
			ReplayRunID: run},
		// Decided on a live simulated feed.
		{Sequence: 4, Type: TxRealizedPnL, Amount: zar("-4.00"),
			InstrumentID: "XAUUSD.m", Source: "autopilot",
			StrategyID: momentum, StrategyName: "Momentum", StrategyVersion: 1},
	}
}

func TestAReplayIsNeverCountedAsForwardEvidence(t *testing.T) {
	// The mistake this prevents is the expensive one: reading a replay's
	// profit as evidence about how the platform behaves on a live feed. A
	// replay decides against dataset prices at a dataset instant, so its
	// numbers answer a different question, and mixed into one account they are
	// indistinguishable without this split.
	report := Attribute(AttributeByRunKind, money.ZAR, replayAndForwardLedger(),
		4, zar("525.00"))

	byKey := map[string]AttributionBucket{}
	for _, b := range report.Buckets {
		byKey[b.Key] = b
	}
	replay, ok := byKey[RunKindReplay]
	if !ok {
		t.Fatalf("no replay bucket; got %v", byKey)
	}
	if replay.Net.StringFixed() != "29.00" {
		t.Errorf("replay net = %s, want 29.00", replay.Net.StringFixed())
	}
	forward, ok := byKey[RunKindPaperForward]
	if !ok {
		t.Fatalf("no paper-forward bucket; got %v", byKey)
	}
	if forward.Net.StringFixed() != "-4.00" {
		t.Errorf("forward net = %s, want -4.00", forward.Net.StringFixed())
	}
	// The whole point: the forward result is a LOSS while the total is a
	// profit. A report that merged them would say the opposite of the truth
	// about live behaviour.
	if !forward.Net.IsNegative() || !report.Total.Net.IsPositive() {
		t.Errorf("the split did not separate a losing forward session from a "+
			"profitable replay: forward %s, total %s",
			forward.Net.StringFixed(), report.Total.Net.StringFixed())
	}
	if !report.Reconciled {
		t.Errorf("not reconciled: discrepancy %s", report.Discrepancy)
	}
}

func TestFundingIsNeitherForwardNorReplay(t *testing.T) {
	// A deposit has no order, so it belongs to neither side. Filing it as
	// either would put 500 of funding on one arm of the comparison the
	// dimension exists to make.
	report := Attribute(AttributeByRunKind, money.ZAR, replayAndForwardLedger(),
		4, zar("525.00"))
	for _, b := range report.Buckets {
		if b.Key == UnattributedKey {
			if b.Other.StringFixed() != "500.00" {
				t.Errorf("unattributed other = %s, want the deposit", b.Other.StringFixed())
			}
			if !b.Net.IsZero() {
				t.Errorf("funding leaked into a trading result: %s", b.Net.StringFixed())
			}
			return
		}
	}
	t.Fatal("the deposit was filed as forward or replay")
}

func TestEachReplayRunIsItsOwnBucket(t *testing.T) {
	// Two runs of one dataset have to be comparable from the ledger. Before
	// this, comparing them meant capturing an API response and trusting that
	// nothing changed in between.
	runA := "aaaaaaaa-0000-0000-0000-000000000001"
	runB := "bbbbbbbb-0000-0000-0000-000000000002"
	entries := []LedgerEntry{
		{Sequence: 1, Type: TxRealizedPnL, Amount: zar("10.00"),
			Source: "autopilot", ReplayRunID: runA},
		{Sequence: 2, Type: TxRealizedPnL, Amount: zar("-2.00"),
			Source: "autopilot", ReplayRunID: runB},
		{Sequence: 3, Type: TxRealizedPnL, Amount: zar("1.00"), Source: "manual"},
	}
	report := Attribute(AttributeByReplayRun, money.ZAR, entries, 3, zar("9.00"))

	byKey := map[string]AttributionBucket{}
	for _, b := range report.Buckets {
		byKey[b.Key] = b
	}
	if got := byKey[runA].Net.StringFixed(); got != "10.00" {
		t.Errorf("run A net = %s, want 10.00", got)
	}
	if got := byKey[runB].Net.StringFixed(); got != "-2.00" {
		t.Errorf("run B net = %s, want -2.00", got)
	}
	// An order with no run is paper-forward, NOT unattributed. This is the one
	// dimension where an empty value is an answer rather than a gap, and
	// filing it as unattributed would hide the forward result entirely.
	if got := byKey[RunKindPaperForward].Net.StringFixed(); got != "1.00" {
		t.Errorf("paper-forward net = %s, want 1.00; keys %v", got, byKey)
	}
	if _, unattributed := byKey[UnattributedKey]; unattributed {
		t.Error("an order with no replay run was filed as unattributed")
	}
	if !report.Reconciled {
		t.Errorf("not reconciled: discrepancy %s", report.Discrepancy)
	}
}

func TestTheNewDimensionsStillCountEveryEntryOnce(t *testing.T) {
	entries := replayAndForwardLedger()
	for _, dim := range []AttributionDimension{AttributeByRunKind, AttributeByReplayRun} {
		report := Attribute(dim, money.ZAR, entries, len(entries), zar("525.00"))
		counted := 0
		for _, b := range report.Buckets {
			counted += b.Entries
		}
		if counted != len(entries) {
			t.Errorf("%s: counted %d entries, want %d", dim, counted, len(entries))
		}
		if !report.Reconciled {
			t.Errorf("%s: not reconciled, discrepancy %s", dim, report.Discrepancy)
		}
	}
}

func TestRegimeAttributionUsesTheRecordedRegime(t *testing.T) {
	// The point of storing the regime rather than recomputing it. If this read
	// a live classifier instead, moved thresholds would reattribute
	// historical P&L to conditions the platform never acted in -- and the
	// report would look like evidence while being a re-interpretation.
	entries := []LedgerEntry{
		{Sequence: 1, Type: TxDeposit, Amount: zar("500.00")},
		{Sequence: 2, Type: TxRealizedPnL, Amount: zar("12.00"),
			Source: "autopilot", Regime: RegimeTrending},
		{Sequence: 3, Type: TxRealizedPnL, Amount: zar("-30.00"),
			Source: "autopilot", Regime: RegimeHighVolatility},
		{Sequence: 4, Type: TxRealizedPnL, Amount: zar("-1.00"),
			Source: "manual"}, // no decision, so no regime
	}
	report := Attribute(AttributeByRegime, money.ZAR, entries, 4, zar("481.00"))

	byKey := map[string]AttributionBucket{}
	for _, b := range report.Buckets {
		byKey[b.Key] = b
	}
	if got := byKey[string(RegimeTrending)].Net.StringFixed(); got != "12.00" {
		t.Errorf("TRENDING net = %s, want 12.00", got)
	}
	if got := byKey[string(RegimeHighVolatility)].Net.StringFixed(); got != "-30.00" {
		t.Errorf("HIGH_VOLATILITY net = %s, want -30.00", got)
	}
	// An entry with no recorded regime is unattributed, not folded into
	// UNKNOWN: "the classifier said unknown" and "nothing was recorded" are
	// different facts and only one of them is a market condition.
	un, ok := byKey[UnattributedKey]
	if !ok {
		t.Fatal("an entry with no recorded regime was dropped")
	}
	if un.Net.StringFixed() != "-1.00" {
		t.Errorf("unattributed net = %s, want -1.00", un.Net.StringFixed())
	}
	if !report.Reconciled {
		t.Errorf("not reconciled: discrepancy %s", report.Discrepancy)
	}
	// The label carries the explanation, so a reader does not have to know
	// what HIGH_VOLATILITY means to read the table.
	if !strings.Contains(byKey[string(RegimeHighVolatility)].Label, "volatile") {
		t.Errorf("label = %q, want it to describe the regime",
			byKey[string(RegimeHighVolatility)].Label)
	}
}
