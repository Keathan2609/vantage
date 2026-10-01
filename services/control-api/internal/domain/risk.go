package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// RiskLimits are the hard ceilings an account trades under.
//
// These are enforced by the risk engine, which sits OUTSIDE strategy code and
// cannot be reached by it. A strategy cannot read, widen or disable these
// values; neither can a model, nor Autopilot. Changing them is an authenticated,
// audited user action.
//
// Zero-valued limits mean "not configured" and are treated as unlimited only
// where that is safe (for example MaxOpenPositions); the money and fraction
// limits below are required and are validated on write.
type RiskLimits struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Currency  money.Currency

	// Per-order ceilings.
	MaxOrderQuantity decimal.Decimal
	MaxOrderNotional money.Amount
	// MaxRiskPerTradeFraction bounds the loss implied by an order's stop-loss
	// distance as a fraction of equity. An order without a stop cannot be
	// sized against it, so RequireStopLoss governs whether that is permitted.
	MaxRiskPerTradeFraction decimal.Decimal
	RequireStopLoss         bool

	// Portfolio ceilings.
	MaxOpenPositions         int
	MaxPendingOrders         int
	MaxGrossExposure         money.Amount
	MaxNetExposure           money.Amount
	MaxPerInstrumentExposure money.Amount
	MaxConcentrationFraction decimal.Decimal
	MaxLeverage              decimal.Decimal

	// Loss ceilings. Daily loss resets at the account's trading-day boundary;
	// drawdown is measured against peak equity and does not reset.
	MaxDailyLoss        money.Amount
	MaxDrawdownFraction decimal.Decimal

	// Market-condition ceilings.
	MaxSpreadFraction   decimal.Decimal
	MaxSlippageFraction decimal.Decimal

	// Event-risk policy applied around high-impact economic releases.
	EventBlackoutBeforeMinutes int
	EventBlackoutAfterMinutes  int
	BlockOnHighImpactEvents    bool

	UpdatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int64
}

// DefaultRiskLimitsFor returns conservative defaults sized for a small account.
//
// The defaults are chosen so that a R500 account cannot be destroyed by a
// single bad day: 1% risk per trade, 3% daily loss, 15% maximum drawdown. They
// are intentionally stricter than most retail defaults, because survival of
// capital is the objective and trade frequency is not a goal in itself.
func DefaultRiskLimitsFor(accountID uuid.UUID, ccy money.Currency, equity money.Amount) RiskLimits {
	pct := func(f string) money.Amount {
		return equity.MulDecimal(decimal.RequireFromString(f)).RoundLedger()
	}
	return RiskLimits{
		AccountID:                  accountID,
		Currency:                   ccy,
		MaxOrderQuantity:           decimal.RequireFromString("1"),
		MaxOrderNotional:           pct("5"), // 5x equity notional ceiling
		MaxRiskPerTradeFraction:    decimal.RequireFromString("0.01"),
		RequireStopLoss:            true,
		MaxOpenPositions:           3,
		MaxPendingOrders:           5,
		MaxGrossExposure:           pct("5"),
		MaxNetExposure:             pct("5"),
		MaxPerInstrumentExposure:   pct("3"),
		MaxConcentrationFraction:   decimal.RequireFromString("0.6"),
		MaxLeverage:                decimal.RequireFromString("10"),
		MaxDailyLoss:               pct("0.03"),
		MaxDrawdownFraction:        decimal.RequireFromString("0.15"),
		MaxSpreadFraction:          decimal.RequireFromString("0.0015"),
		MaxSlippageFraction:        decimal.RequireFromString("0.002"),
		EventBlackoutBeforeMinutes: 15,
		EventBlackoutAfterMinutes:  10,
		BlockOnHighImpactEvents:    true,
	}
}

// RiskCheckName identifies one deterministic check.
type RiskCheckName string

const (
	CheckOrderQuantity      RiskCheckName = "max_order_quantity"
	CheckOrderNotional      RiskCheckName = "max_order_notional"
	CheckRiskPerTrade       RiskCheckName = "max_risk_per_trade"
	CheckStopLossRequired   RiskCheckName = "stop_loss_required"
	CheckOpenPositions      RiskCheckName = "max_open_positions"
	CheckPendingOrders      RiskCheckName = "max_pending_orders"
	CheckGrossExposure      RiskCheckName = "max_gross_exposure"
	CheckNetExposure        RiskCheckName = "max_net_exposure"
	CheckInstrumentExposure RiskCheckName = "max_instrument_exposure"
	CheckConcentration      RiskCheckName = "max_concentration"
	// CheckPortfolioCorrelation catches what every other exposure check
	// misses: a book of gold, silver and platinum passes all of them and is
	// one position. Concentration counts three names and sees
	// diversification.
	CheckPortfolioCorrelation RiskCheckName = "portfolio_correlation"
	CheckLeverage             RiskCheckName = "max_leverage"
	CheckMargin               RiskCheckName = "available_margin"
	CheckDailyLoss            RiskCheckName = "max_daily_loss"
	CheckDrawdown             RiskCheckName = "max_drawdown"
	CheckSpread               RiskCheckName = "max_spread"
	CheckEventRisk            RiskCheckName = "event_risk"
	// CheckBookIsValued refuses to open while any open position could not be
	// priced.
	//
	// An unpriced position contributes NOTHING to unrealised P&L, margin used
	// or gross exposure — `portfolio.aggregate` counts it and skips it — so
	// every exposure and loss ceiling below is computed against a book that is
	// missing a position. The numbers do not look wrong; they look good. A
	// stale FX rate is enough to cause it.
	//
	// This is the "fail closed" rule applied to the one input every other
	// check depends on. It is NOT an exposure ceiling, and it exempts reducing
	// orders for the same reason they all do: refusing to let an operator
	// close a position because the platform cannot value it is how you trap
	// someone in the position that is hurting them.
	CheckBookIsValued      RiskCheckName = "book_is_valued"
	CheckAccountTrading    RiskCheckName = "account_trading_enabled"
	CheckInstrumentEnabled RiskCheckName = "instrument_enabled"

	// The trading authority's own numeric ceilings.
	//
	// They are SEPARATE checks rather than a decimal.Min folded into the
	// account's, which is how MaxLeverage was done. Folding hides which bound
	// was hit: an operator who narrowed the authority to 0.10 and then saw
	// "Order quantity 0.50 against a limit of 0.10" could not tell whether
	// the account's limit or the authority's produced it, and those have
	// different fixes and different audit trails. Two checks that must both
	// pass are arithmetically the same as one min(), and they say which.
	//
	// The authority may only ever be MORE restrictive: it never raises a
	// bound, because a check that could loosen the account's own limits would
	// be a way to grant risk rather than to withhold it.
	CheckAuthorityOrderQuantity    RiskCheckName = "authority_max_order_quantity"
	CheckAuthorityOrderNotional    RiskCheckName = "authority_max_order_notional"
	CheckAuthorityPositionExposure RiskCheckName = "authority_max_position_exposure"
	CheckAuthorityDailyLoss        RiskCheckName = "authority_max_daily_loss"
	CheckAuthorityLeverage         RiskCheckName = "authority_max_leverage"
)

// AuthorityCeilingChecks is every check that enforces a trading authority's
// numeric ceiling, in the order the engine evaluates them.
//
// Exported so a test can assert the set is complete rather than restating it,
// and so an operator-facing surface can say which authority fields bind
// without hard-coding a list that drifts. A ceiling added to
// TradingAuthority without an entry here has no enforcement.
var AuthorityCeilingChecks = []RiskCheckName{
	CheckAuthorityDailyLoss,
	CheckAuthorityOrderQuantity,
	CheckAuthorityOrderNotional,
	CheckAuthorityPositionExposure,
	CheckAuthorityLeverage,
}

// RiskCheckResult records one check's verdict. Every check reports its limit
// and the observed value so a rejection is explainable without re-running it.
type RiskCheckResult struct {
	Name     RiskCheckName
	Passed   bool
	Limit    string
	Observed string
	Message  string
	Code     RejectCode
}

// RiskDecision is the aggregate verdict for one order intent.
type RiskDecision struct {
	Approved     bool
	Checks       []RiskCheckResult
	FirstFailure *RiskCheckResult
	EvaluatedAt  time.Time
	// ApprovedQuantity may be smaller than requested when sizing policy caps
	// it. It is never larger: risk may only reduce.
	ApprovedQuantity decimal.Decimal
}

// Failures returns the checks that did not pass.
func (d RiskDecision) Failures() []RiskCheckResult {
	var out []RiskCheckResult
	for _, c := range d.Checks {
		if !c.Passed {
			out = append(out, c)
		}
	}
	return out
}

// RiskEventSeverity grades a recorded risk occurrence.
type RiskEventSeverity string

const (
	SeverityInfo     RiskEventSeverity = "info"
	SeverityWarning  RiskEventSeverity = "warning"
	SeverityCritical RiskEventSeverity = "critical"
)

// RiskEvent is a durable record of a risk-relevant occurrence: a rejection, a
// breached threshold, a kill-switch activation. These drive both the Activity
// timeline and alerting.
type RiskEvent struct {
	ID           uuid.UUID
	AccountID    *uuid.UUID
	UserID       *uuid.UUID
	Severity     RiskEventSeverity
	Check        RiskCheckName
	Code         RejectCode
	Message      string
	InstrumentID *string
	StrategyID   *uuid.UUID
	OrderID      *uuid.UUID
	Detail       map[string]string
	CreatedAt    time.Time
}
