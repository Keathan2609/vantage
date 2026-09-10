package domain

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// P&L attribution.
//
// # Why this folds the LEDGER and not positions
//
// "Which strategy made this money?" has to be answered in a way that adds up.
// The previous answer grouped CLOSED POSITIONS by instrument and converted each
// position's P&L into the account currency, skipping any position whose
// conversion failed -- so a missing rate silently removed money from the
// report, and the total was quietly not the account's total.
//
// The ledger has neither problem. Every movement of money is exactly one
// transaction row, already denominated in the account's currency because
// booking converted it once, at the time, with the rate that actually applied.
// Folding those rows means each unit of money is counted exactly once by
// construction: double counting would require a duplicate ledger row, which the
// sequence constraint forbids.
//
// # Why an "unattributed" bucket exists
//
// A deposit has no order, so it has no strategy, no instrument and no session.
// Dropping such a row would make the buckets sum to less than the account, and
// a report that does not reconcile is worse than no report -- it looks
// authoritative. So every entry lands in exactly one bucket, and the ones that
// cannot be attributed land in a bucket that says so.

// AttributionDimension is what the P&L is grouped by.
type AttributionDimension string

const (
	// AttributeByInstrument answers "which market".
	AttributeByInstrument AttributionDimension = "instrument"
	// AttributeByStrategy answers "which strategy", across all its versions.
	AttributeByStrategy AttributionDimension = "strategy"
	// AttributeByStrategyVersion separates versions of one strategy, which is
	// the only way to see whether a promotion helped.
	AttributeByStrategyVersion AttributionDimension = "strategy_version"
	// AttributeBySource separates a human's orders from the autonomous
	// pipeline's. Without it, "the robot lost money" cannot be distinguished
	// from "I lost money while the robot was on".
	AttributeBySource AttributionDimension = "source"
	// AttributeBySession answers "which market session", taken from the
	// decision the order came from rather than recomputed later from a
	// timestamp, which would use today's calendar for last month's trade.
	AttributeBySession AttributionDimension = "session"
	// AttributeByEventContext separates trades decided during an economic
	// blackout window from the rest.
	AttributeByEventContext AttributionDimension = "event_context"
	// AttributeByType is the cost breakdown: realised P&L against commission,
	// swap and fees.
	AttributeByType AttributionDimension = "type"
	// AttributeByRunKind splits PAPER_FORWARD from BACKTEST/REPLAY.
	//
	// The single most important split in the set, and the one a paper-forward
	// programme rests on. A replay writes real orders through the real OMS
	// into the real ledger -- that is why it is evidence -- but those orders
	// were decided against dataset prices at a dataset instant. Mixed into one
	// account with a live-feed session's, they are indistinguishable, and every
	// question about how the platform behaves on a live feed gets a polluted
	// answer.
	AttributeByRunKind AttributionDimension = "run_kind"
	// AttributeByReplayRun separates individual replays, so two runs of the
	// same dataset can be compared from the ledger rather than by capturing an
	// API response.
	AttributeByReplayRun AttributionDimension = "replay_run"
)

// Run kinds. Named to match the vocabulary the milestone briefs use.
const (
	// RunKindPaperForward is an order decided on a live simulated feed. The
	// only kind that is evidence about forward behaviour.
	RunKindPaperForward = "paper_forward"
	// RunKindReplay is an order decided against a dataset at a dataset
	// instant, by a market replay.
	RunKindReplay = "replay"
)

// ErrUnknownDimension means a caller asked to group by something the platform
// does not record.
//
// Fails closed on purpose. Silently falling back to instrument would answer a
// question nobody asked, and the caller would believe the answer.
var ErrUnknownDimension = errors.New("domain: unknown attribution dimension")

// ParseAttributionDimension resolves a request parameter.
func ParseAttributionDimension(s string) (AttributionDimension, error) {
	switch AttributionDimension(strings.ToLower(strings.TrimSpace(s))) {
	case AttributeByInstrument:
		return AttributeByInstrument, nil
	case AttributeByStrategy:
		return AttributeByStrategy, nil
	case AttributeByStrategyVersion:
		return AttributeByStrategyVersion, nil
	case AttributeBySource:
		return AttributeBySource, nil
	case AttributeBySession:
		return AttributeBySession, nil
	case AttributeByEventContext:
		return AttributeByEventContext, nil
	case AttributeByType:
		return AttributeByType, nil
	case AttributeByRunKind:
		return AttributeByRunKind, nil
	case AttributeByReplayRun:
		return AttributeByReplayRun, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownDimension, s)
	}
}

// AttributionDimensions lists every dimension, for a caller offering a choice.
func AttributionDimensions() []AttributionDimension {
	return []AttributionDimension{
		AttributeByInstrument, AttributeByStrategy, AttributeByStrategyVersion,
		AttributeBySource, AttributeBySession, AttributeByEventContext,
		AttributeByType, AttributeByRunKind, AttributeByReplayRun,
	}
}

// UnattributedKey is the bucket for money that cannot be tied to a dimension.
const UnattributedKey = "unattributed"

// LedgerEntry is one ledger row with whatever context could be joined to it.
//
// The context fields are empty when there is none to be had -- a deposit has no
// strategy, and an order placed before decision snapshots existed has no
// session. Empty is therefore meaningful and is never guessed at.
type LedgerEntry struct {
	Sequence int64
	Type     TransactionType
	// Amount is signed and already in the account's currency: a loss is
	// negative, and commission is negative because it left the account.
	Amount money.Amount

	InstrumentID string
	Symbol       string

	// Source is the order's source: manual, strategy, autopilot, risk_control.
	Source string

	StrategyID   string
	StrategyName string
	// StrategyVersion is zero when unknown, which the pair constraint on
	// orders makes equivalent to having no strategy at all.
	StrategyVersion int

	// Session and Blackout come from the decision snapshot the order carried.
	Session  string
	Blackout *bool

	// ReplayRunID is the market replay that owned the clock when the order was
	// created, and empty for an order decided on a live feed.
	//
	// Empty is NOT "unattributed" for this dimension, which is why it is the
	// one field here whose absence is meaningful rather than missing: a
	// deposit has no order and no run, but an order with no run genuinely was
	// paper-forward. The bucketing distinguishes the two.
	ReplayRunID string
}

// AttributionBucket is one group's contribution.
//
// Gross, Costs and Other are kept apart rather than summed into one number
// because they answer different questions and because merging them hides the
// most useful finding a report can produce: a strategy that is profitable
// before costs and loses after them.
type AttributionBucket struct {
	Key   string
	Label string

	// Gross is realised trading P&L only.
	Gross money.Amount
	// Costs is commission, swap and fees, and is NEGATIVE or zero -- the sign
	// the ledger actually holds. Flipping it to a positive "cost" figure here
	// would mean two representations of the same number in one system.
	Costs money.Amount
	// Other is deposits, withdrawals and adjustments: real movements of money
	// that are not trading results and must never be reported as performance.
	Other money.Amount
	// Net is Gross + Costs. Deliberately NOT including Other: a deposit is not
	// a profit.
	Net money.Amount

	// Trades counts realised-P&L entries, which is one per position-closing
	// event rather than one per fill.
	Trades int
	Wins   int
	// WinRate is Wins/Trades to four places, zero when there are no trades.
	WinRate decimal.Decimal
	// Entries is every ledger row in this bucket, including costs and
	// deposits. Trades plus costs will exceed it in no case, but the two are
	// not the same count and conflating them overstates activity.
	Entries int
}

// AttributionReport is the whole answer, including whether it adds up.
type AttributionReport struct {
	Dimension AttributionDimension
	Currency  money.Currency
	Buckets   []AttributionBucket
	// Total is every bucket summed. Its Net is the account's realised
	// trading result over the entries considered.
	Total AttributionBucket

	// Reconciled reports whether the buckets account for every ledger entry
	// the account has. FALSE means the entry set given to Attribute was not
	// the whole ledger -- almost always a limit truncating it -- and the
	// totals therefore describe a window rather than the account.
	//
	// Exposed rather than asserted because the honest response to a truncated
	// ledger is to say so, not to return a number that looks like the
	// account's and is not.
	Reconciled bool
	// LedgerEntries and LedgerNet are the account's own totals, independent of
	// the entries folded here. The comparison is the reconciliation.
	LedgerEntries int
	LedgerNet     money.Amount
	// Discrepancy is LedgerNet minus Total.Net plus Total.Other, zero when
	// reconciled.
	Discrepancy money.Amount
}

// Attribute folds ledger entries into buckets. A pure function.
//
// ledgerEntries and ledgerNet are the account's totals across its WHOLE ledger,
// used only to decide whether this fold reconciles. Pass the same values the
// entries came from and the report reconciles; pass a truncated entry set and
// it says so.
func Attribute(dimension AttributionDimension, ccy money.Currency,
	entries []LedgerEntry, ledgerEntries int, ledgerNet money.Amount) AttributionReport {

	report := AttributionReport{
		Dimension:     dimension,
		Currency:      ccy,
		Total:         newBucket("TOTAL", "All entries", ccy),
		LedgerEntries: ledgerEntries,
		LedgerNet:     ledgerNet,
	}
	if ledgerNet.Currency() == "" {
		report.LedgerNet = money.Zero(ccy)
	}

	order := []string{}
	buckets := map[string]*AttributionBucket{}
	for _, e := range entries {
		key, label := bucketFor(dimension, e)
		b, ok := buckets[key]
		if !ok {
			nb := newBucket(key, label, ccy)
			buckets[key] = &nb
			b = &nb
			order = append(order, key)
		}
		addEntry(b, e)
		addEntry(&report.Total, e)
	}

	report.Buckets = make([]AttributionBucket, 0, len(order))
	for _, key := range order {
		b := buckets[key]
		finishBucket(b)
		report.Buckets = append(report.Buckets, *b)
	}
	finishBucket(&report.Total)

	// Most negative first: the thing costing money is what a report exists to
	// surface, and burying it below the winners is how it gets missed.
	sort.SliceStable(report.Buckets, func(i, j int) bool {
		a, b := report.Buckets[i], report.Buckets[j]
		if !a.Net.Decimal().Equal(b.Net.Decimal()) {
			return a.Net.Decimal().LessThan(b.Net.Decimal())
		}
		return a.Key < b.Key
	})

	// The reconciliation. Total.Net covers trading only, so the comparison
	// against the ledger has to add back the non-trading movements.
	folded := report.Total.Net.MustAdd(report.Total.Other)
	report.Discrepancy = report.LedgerNet.MustAdd(folded.Neg()).RoundLedger()
	report.Reconciled = report.Discrepancy.IsZero() && report.Total.Entries == ledgerEntries
	return report
}

// shortID abbreviates a uuid for a label. The full value stays the key, so
// nothing is lost -- this is only so a table column is readable.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func newBucket(key, label string, ccy money.Currency) AttributionBucket {
	return AttributionBucket{
		Key: key, Label: label,
		Gross: money.Zero(ccy), Costs: money.Zero(ccy),
		Other: money.Zero(ccy), Net: money.Zero(ccy),
	}
}

// addEntry routes one ledger row into exactly one of Gross, Costs and Other.
//
// The default arm is Other rather than Gross. A transaction type this code has
// not seen before is a real movement of money that is not a demonstrated
// trading result, and counting it as performance is the mistake that cannot be
// walked back.
func addEntry(b *AttributionBucket, e LedgerEntry) {
	b.Entries++
	switch e.Type {
	case TxRealizedPnL:
		b.Gross = b.Gross.MustAdd(e.Amount)
		b.Trades++
		if e.Amount.IsPositive() {
			b.Wins++
		}
	case TxCommission, TxSwap, TxFee:
		b.Costs = b.Costs.MustAdd(e.Amount)
	default:
		b.Other = b.Other.MustAdd(e.Amount)
	}
}

func finishBucket(b *AttributionBucket) {
	b.Gross = b.Gross.RoundLedger()
	b.Costs = b.Costs.RoundLedger()
	b.Other = b.Other.RoundLedger()
	b.Net = b.Gross.MustAdd(b.Costs).RoundLedger()
	if b.Trades > 0 {
		b.WinRate = decimal.NewFromInt(int64(b.Wins)).
			Div(decimal.NewFromInt(int64(b.Trades))).Round(4)
	}
}

// bucketFor picks the group, and never invents one.
//
// An entry that carries no value for the requested dimension goes to
// UnattributedKey with a label saying which piece of context was missing, so
// "12.40 unattributed" is a lead rather than a mystery.
func bucketFor(dimension AttributionDimension, e LedgerEntry) (key, label string) {
	switch dimension {
	case AttributeByInstrument:
		if e.InstrumentID == "" {
			return UnattributedKey, "no instrument: not a trading entry"
		}
		if e.Symbol != "" {
			return e.InstrumentID, e.Symbol
		}
		return e.InstrumentID, e.InstrumentID

	case AttributeByStrategy:
		if e.StrategyID == "" {
			return UnattributedKey, "no strategy: a manual or non-trading entry"
		}
		if e.StrategyName != "" {
			return e.StrategyID, e.StrategyName
		}
		return e.StrategyID, e.StrategyID

	case AttributeByStrategyVersion:
		if e.StrategyID == "" || e.StrategyVersion == 0 {
			return UnattributedKey, "no strategy version: a manual or non-trading entry"
		}
		v := strconv.Itoa(e.StrategyVersion)
		name := e.StrategyName
		if name == "" {
			name = e.StrategyID
		}
		return e.StrategyID + "@" + v, name + " v" + v

	case AttributeBySource:
		if e.Source == "" {
			return UnattributedKey, "no order: not a trading entry"
		}
		return e.Source, e.Source

	case AttributeBySession:
		if e.Session == "" {
			return UnattributedKey, "no recorded session: no decision snapshot"
		}
		return e.Session, e.Session

	case AttributeByEventContext:
		if e.Blackout == nil {
			return UnattributedKey, "no recorded event context: no decision snapshot"
		}
		if *e.Blackout {
			return "blackout", "decided during an economic blackout"
		}
		return "clear", "decided outside any blackout window"

	case AttributeByType:
		return string(e.Type), string(e.Type)

	case AttributeByRunKind:
		// An entry with no order at all -- a deposit -- is neither kind. It is
		// not paper-forward evidence and it is not a replay's doing, and
		// filing it as either would put funding on one side of the comparison
		// the whole dimension exists to make.
		if e.Source == "" {
			return UnattributedKey, "no order: neither a forward session nor a replay"
		}
		if e.ReplayRunID != "" {
			return RunKindReplay, "BACKTEST/REPLAY: decided against a dataset, not a live feed"
		}
		return RunKindPaperForward, "PAPER_FORWARD: decided on a live simulated feed"

	case AttributeByReplayRun:
		if e.Source == "" {
			return UnattributedKey, "no order: neither a forward session nor a replay"
		}
		if e.ReplayRunID == "" {
			return RunKindPaperForward, "PAPER_FORWARD: no replay was engaged"
		}
		return e.ReplayRunID, "replay run " + shortID(e.ReplayRunID)

	default:
		// Unreachable through ParseAttributionDimension, which fails closed.
		// Reached only by a caller constructing a dimension by hand, and the
		// safe answer is to attribute nothing rather than to guess.
		return UnattributedKey, "unknown dimension"
	}
}
