package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// StrategyFamily groups strategies by the market behaviour they attempt to
// exploit. It is metadata for research and correlation analysis: two trend
// strategies on the same instrument are not two independent bets.
type StrategyFamily string

const (
	FamilyTrendFollowing   StrategyFamily = "trend_following"
	FamilyMomentum         StrategyFamily = "momentum"
	FamilyMeanReversion    StrategyFamily = "mean_reversion"
	FamilyBreakout         StrategyFamily = "breakout"
	FamilyVolatility       StrategyFamily = "volatility"
	FamilyMultiTimeframe   StrategyFamily = "multi_timeframe"
	FamilySession          StrategyFamily = "session"
	FamilyStatistical      StrategyFamily = "statistical"
	FamilyEventDriven      StrategyFamily = "event_driven"
	FamilyEnsemble         StrategyFamily = "ensemble"
	FamilyMachineLearning  StrategyFamily = "machine_learning"
	FamilyHighRiskResearch StrategyFamily = "high_risk_research"
)

// StrategyLifecycle is the promotion ladder. Promotion is evidence-gated and
// one step at a time; nothing self-promotes.
type StrategyLifecycle string

const (
	LifecycleDraft      StrategyLifecycle = "DRAFT"
	LifecycleResearch   StrategyLifecycle = "RESEARCH"
	LifecycleBacktested StrategyLifecycle = "BACKTESTED"
	LifecycleValidated  StrategyLifecycle = "VALIDATED"
	LifecyclePaper      StrategyLifecycle = "PAPER"
	LifecycleDemo       StrategyLifecycle = "DEMO"
	LifecycleLive       StrategyLifecycle = "LIVE"
	LifecycleRetired    StrategyLifecycle = "RETIRED"
)

// lifecycleOrder defines the permitted forward path. Retirement is reachable
// from anywhere; every other move is one rung at a time.
var lifecycleOrder = []StrategyLifecycle{
	LifecycleDraft, LifecycleResearch, LifecycleBacktested,
	LifecycleValidated, LifecyclePaper, LifecycleDemo, LifecycleLive,
}

// MaxPermittedLifecycle is the ceiling this build enforces. DEMO and LIVE are
// structurally unreachable: no adapter exists that could execute them, and the
// promotion API refuses them regardless of caller privilege.
const MaxPermittedLifecycle = LifecyclePaper

// CanPromote reports whether from -> to is a legal lifecycle move in this
// build, with a reason when it is not.
func CanPromote(from, to StrategyLifecycle) (bool, string) {
	if to == LifecycleRetired {
		return true, ""
	}
	fi, ti := -1, -1
	for i, l := range lifecycleOrder {
		if l == from {
			fi = i
		}
		if l == to {
			ti = i
		}
	}
	if fi < 0 || ti < 0 {
		return false, "unknown lifecycle state"
	}
	if ti > fi+1 {
		return false, "lifecycle may only advance one stage at a time"
	}
	if ti < fi {
		return false, "lifecycle may not move backwards; retire and re-promote instead"
	}
	if ti == fi {
		return false, "strategy is already in this lifecycle state"
	}
	ceiling := -1
	for i, l := range lifecycleOrder {
		if l == MaxPermittedLifecycle {
			ceiling = i
		}
	}
	if ti > ceiling {
		return false, "this build cannot promote beyond " + string(MaxPermittedLifecycle) +
			"; live execution is not available"
	}
	return true, ""
}

// Strategy is a named trading approach.
type Strategy struct {
	ID          uuid.UUID
	Key         string // stable identifier used by the quant service
	Name        string
	Family      StrategyFamily
	Description string
	// HighRisk marks approaches whose exposure profile is inherently dangerous
	// (grid, martingale, averaging down). They are disabled by default and
	// remain subject to every account-level ceiling.
	HighRisk    bool
	Enabled     bool
	OwnerUserID uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// StrategyVersion binds a parameter set to an immutable code identity. A
// backtest result is meaningless without knowing exactly which code and which
// parameters produced it, so results reference a version, never a strategy.
type StrategyVersion struct {
	ID           uuid.UUID
	StrategyID   uuid.UUID
	Version      int
	CodeHash     string // hash of the quant-service strategy implementation
	GitSHA       string
	Parameters   json.RawMessage
	Timeframe    Timeframe
	Instruments  []string
	Lifecycle    StrategyLifecycle
	ValidRegimes []string
	Notes        string
	CreatedBy    uuid.UUID
	CreatedAt    time.Time
	PromotedAt   *time.Time
	RetiredAt    *time.Time
}

// SignalAction is a strategy's directional conclusion.
type SignalAction string

const (
	SignalBuy     SignalAction = "buy"
	SignalSell    SignalAction = "sell"
	SignalHold    SignalAction = "hold"
	SignalClose   SignalAction = "close"
	SignalNoTrade SignalAction = "no_trade"
)

// Actionable reports whether the signal implies opening exposure.
func (a SignalAction) Actionable() bool { return a == SignalBuy || a == SignalSell }

// Side maps an actionable signal to an order side.
func (a SignalAction) Side() (OrderSide, bool) {
	switch a {
	case SignalBuy:
		return SideBuy, true
	case SignalSell:
		return SideSell, true
	}
	return "", false
}

// StrategySignal is the ONLY thing a strategy produces. It is an opinion, not
// an instruction: it carries no authority, no size that risk must honour, and
// no path to a broker. The orchestrator decides what, if anything, happens next.
type StrategySignal struct {
	ID              uuid.UUID
	StrategyID      uuid.UUID
	StrategyVersion int
	AccountID       *uuid.UUID
	InstrumentID    string
	Timeframe       Timeframe
	Action          SignalAction
	// Confidence in [0,1]. It is a strategy's self-assessment and is treated as
	// a ranking input, never as a probability of profit.
	Confidence      decimal.Decimal
	SuggestedStop   *decimal.Decimal
	SuggestedTarget *decimal.Decimal
	Explanation     string
	Features        json.RawMessage
	BarTime         time.Time
	GeneratedAt     time.Time
	RunID           uuid.UUID
}

// StrategyRunStatus records how a scheduled evaluation ended.
type StrategyRunStatus string

const (
	RunSucceeded StrategyRunStatus = "succeeded"
	RunNoSignal  StrategyRunStatus = "no_signal"
	RunSkipped   StrategyRunStatus = "skipped"
	RunFailed    StrategyRunStatus = "failed"
)

// StrategyRun is one evaluation of one strategy version.
type StrategyRun struct {
	ID              uuid.UUID
	StrategyID      uuid.UUID
	StrategyVersion int
	AccountID       *uuid.UUID
	InstrumentID    string
	Timeframe       Timeframe
	Status          StrategyRunStatus
	SignalID        *uuid.UUID
	SkipReason      string
	Error           string
	DurationMS      int64
	StartedAt       time.Time
	FinishedAt      time.Time
}

// DecisionSnapshot is the immutable record of WHY an order intent existed.
//
// It captures every input the decision depended on, so a trade can be
// reconstructed and challenged months later. It never contains secrets: no
// credentials, no session identifiers, no encryption keys. What it does contain
// is quotes, indicator values, portfolio state, risk state and authority state
// at decision time.
type DecisionSnapshot struct {
	ID               uuid.UUID
	AccountID        uuid.UUID
	StrategyID       *uuid.UUID
	StrategyVersion  *int
	ModelID          *uuid.UUID
	ModelVersion     *int
	InstrumentID     string
	BarTime          *time.Time
	Quote            json.RawMessage
	Indicators       json.RawMessage
	Features         json.RawMessage
	EventContext     json.RawMessage
	PortfolioContext json.RawMessage
	RiskState        json.RawMessage
	// Regime is the market's shape AS IT WAS when this decision was taken,
	// after hysteresis. Stored rather than recomputed: a later recalculation
	// under moved thresholds would silently reattribute historical P&L to
	// regimes the platform never acted on.
	Regime Regime
	// RegimePolicyVersion identifies the thresholds that produced it, so two
	// decisions can only be compared on regime when both used the same ones.
	RegimePolicyVersion string
	// RegimeReasons is every check behind the verdict, including the ones that
	// passed. A verdict listing only its triggers cannot be argued with.
	RegimeReasons    json.RawMessage
	AuthorityState   json.RawMessage
	MarketDataHealth json.RawMessage
	SignalAction     SignalAction
	Confidence       decimal.Decimal
	RequestedQty     decimal.Decimal
	ApprovedQty      decimal.Decimal
	Outcome          string // accepted | rejected | no_trade
	OutcomeCode      string
	OutcomeReason    string
	CreatedAt        time.Time
}

// StrategyAllocation caps how much of an account's risk budget one strategy may
// consume, so a single misbehaving strategy cannot occupy the whole account.
type StrategyAllocation struct {
	ID               uuid.UUID
	AccountID        uuid.UUID
	StrategyID       uuid.UUID
	StrategyVersion  int
	Enabled          bool
	MaxRiskFraction  decimal.Decimal
	MaxOpenPositions int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
