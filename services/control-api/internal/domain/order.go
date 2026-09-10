package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// OrderSide is the direction of an order.
type OrderSide string

const (
	SideBuy  OrderSide = "buy"
	SideSell OrderSide = "sell"
)

// Opposite returns the closing side.
func (s OrderSide) Opposite() OrderSide {
	if s == SideBuy {
		return SideSell
	}
	return SideBuy
}

// Valid reports whether the side is one of the two permitted values.
func (s OrderSide) Valid() bool { return s == SideBuy || s == SideSell }

// SignedMultiplier returns +1 for buy and -1 for sell, for exposure maths.
func (s OrderSide) SignedMultiplier() decimal.Decimal {
	if s == SideBuy {
		return decimal.NewFromInt(1)
	}
	return decimal.NewFromInt(-1)
}

// OrderType is the execution instruction.
type OrderType string

const (
	OrderTypeMarket    OrderType = "market"
	OrderTypeLimit     OrderType = "limit"
	OrderTypeStop      OrderType = "stop"
	OrderTypeStopLimit OrderType = "stop_limit"
)

// Valid reports whether the type is recognised by the domain. Whether a
// particular venue accepts it is a separate check against InstrumentSpec.
func (t OrderType) Valid() bool {
	switch t {
	case OrderTypeMarket, OrderTypeLimit, OrderTypeStop, OrderTypeStopLimit:
		return true
	}
	return false
}

// RequiresLimitPrice reports whether a limit price must be supplied.
func (t OrderType) RequiresLimitPrice() bool {
	return t == OrderTypeLimit || t == OrderTypeStopLimit
}

// RequiresStopPrice reports whether a stop trigger price must be supplied.
func (t OrderType) RequiresStopPrice() bool {
	return t == OrderTypeStop || t == OrderTypeStopLimit
}

// TimeInForce controls how long a resting order lives.
type TimeInForce string

const (
	TIFGoodTilCancelled  TimeInForce = "gtc"
	TIFImmediateOrCancel TimeInForce = "ioc"
	TIFFillOrKill        TimeInForce = "fok"
	TIFDay               TimeInForce = "day"
)

// Valid reports whether the TIF is recognised.
func (t TimeInForce) Valid() bool {
	switch t {
	case TIFGoodTilCancelled, TIFImmediateOrCancel, TIFFillOrKill, TIFDay:
		return true
	}
	return false
}

// OrderStatus is a node in the order state machine.
type OrderStatus string

const (
	OrderCreated         OrderStatus = "CREATED"
	OrderValidating      OrderStatus = "VALIDATING"
	OrderAccepted        OrderStatus = "ACCEPTED"
	OrderSubmitted       OrderStatus = "SUBMITTED"
	OrderPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderFilled          OrderStatus = "FILLED"
	OrderRejected        OrderStatus = "REJECTED"
	OrderCancelPending   OrderStatus = "CANCEL_PENDING"
	OrderCancelled       OrderStatus = "CANCELLED"
	OrderExpired         OrderStatus = "EXPIRED"
	OrderFailed          OrderStatus = "FAILED"
)

// legalTransitions is the complete order state machine. Any transition absent
// from this table is impossible: the OMS refuses it and raises an audit event
// rather than silently coercing the order into the requested state.
//
// Two transitions deserve explanation:
//
//   - SUBMITTED -> FAILED covers the case where the broker's response was lost.
//     FAILED is not terminal-with-certainty; it means "Vantage does not know the
//     broker-side outcome". Reconciliation resolves it against broker state.
//   - CANCEL_PENDING -> FILLED is legal because a cancel request races the
//     venue. An order can fill after the user asks to cancel, and pretending
//     otherwise would desynchronise Vantage from the broker.
var legalTransitions = map[OrderStatus]map[OrderStatus]bool{
	OrderCreated: {
		OrderValidating: true,
		OrderRejected:   true,
		OrderFailed:     true,
	},
	OrderValidating: {
		OrderAccepted: true,
		OrderRejected: true,
		OrderFailed:   true,
	},
	OrderAccepted: {
		OrderSubmitted:     true,
		OrderCancelPending: true,
		OrderCancelled:     true,
		OrderRejected:      true,
		OrderFailed:        true,
	},
	OrderSubmitted: {
		OrderPartiallyFilled: true,
		OrderFilled:          true,
		OrderCancelPending:   true,
		OrderCancelled:       true,
		OrderRejected:        true,
		OrderExpired:         true,
		OrderFailed:          true,
	},
	OrderPartiallyFilled: {
		OrderPartiallyFilled: true,
		OrderFilled:          true,
		OrderCancelPending:   true,
		OrderCancelled:       true,
		OrderExpired:         true,
		OrderFailed:          true,
	},
	OrderCancelPending: {
		OrderCancelled:       true,
		OrderPartiallyFilled: true,
		OrderFilled:          true,
		OrderFailed:          true,
	},
	OrderFilled:    {},
	OrderRejected:  {},
	OrderCancelled: {},
	OrderExpired:   {},
	// FAILED is recoverable only through reconciliation, which may establish
	// that the broker in fact filled, cancelled or rejected the order.
	OrderFailed: {
		OrderFilled:          true,
		OrderPartiallyFilled: true,
		OrderCancelled:       true,
		OrderRejected:        true,
	},
}

// Terminal reports whether a status admits no further transitions under normal
// operation. FAILED is not terminal: reconciliation may still resolve it.
func (s OrderStatus) Terminal() bool {
	switch s {
	case OrderFilled, OrderRejected, OrderCancelled, OrderExpired:
		return true
	}
	return false
}

// Open reports whether the order may still consume risk budget or fill.
func (s OrderStatus) Open() bool {
	switch s {
	case OrderCreated, OrderValidating, OrderAccepted, OrderSubmitted,
		OrderPartiallyFilled, OrderCancelPending:
		return true
	}
	return false
}

// CanTransition reports whether from -> to is legal.
func CanTransition(from, to OrderStatus) bool {
	allowed, ok := legalTransitions[from]
	if !ok {
		return false
	}
	return allowed[to]
}

// ErrIllegalTransition is returned when an order is asked to move to a state
// the state machine forbids.
type ErrIllegalTransition struct {
	From OrderStatus
	To   OrderStatus
}

func (e ErrIllegalTransition) Error() string {
	return fmt.Sprintf("illegal order state transition %s -> %s", e.From, e.To)
}

// ExecutionMode records the execution environment an order belongs to. It is
// persisted on every order so a paper order can never be confused with a live
// one, even in a database dump read years later.
type ExecutionMode string

const (
	ModePaper ExecutionMode = "paper"
	ModeDemo  ExecutionMode = "demo"
	ModeLive  ExecutionMode = "live"
)

// Valid reports whether the mode is a recognised value.
func (m ExecutionMode) Valid() bool {
	switch m {
	case ModePaper, ModeDemo, ModeLive:
		return true
	}
	return false
}

// OrderSource records what originated the order.
type OrderSource string

const (
	SourceManual      OrderSource = "manual"
	SourceStrategy    OrderSource = "strategy"
	SourceAutopilot   OrderSource = "autopilot"
	SourceRiskControl OrderSource = "risk_control" // e.g. an operator flatten
)

// OrderIntent is what a strategy or user expresses: a desire to trade. It is
// deliberately distinct from an OrderCommand. An intent has passed no checks
// and confers no authority; it is an input to the pipeline, not an instruction
// to a broker. Strategy code produces intents and nothing else.
type OrderIntent struct {
	ID           uuid.UUID
	AccountID    uuid.UUID
	InstrumentID string
	Side         OrderSide
	Type         OrderType
	Quantity     decimal.Decimal
	LimitPrice   *decimal.Decimal
	StopPrice    *decimal.Decimal
	StopLoss     *decimal.Decimal
	TakeProfit   *decimal.Decimal
	TimeInForce  TimeInForce
	Source       OrderSource
	StrategyID   *uuid.UUID
	StrategyVer  *int
	DecisionID   *uuid.UUID
	CreatedAt    time.Time
}

// OrderCommand is an intent that has been authorised, risk-checked and
// assigned an idempotency identity. Only the OMS constructs one, and only
// after every gate in the pipeline has passed.
type OrderCommand struct {
	CommandID      uuid.UUID
	IdempotencyKey string
	RequestHash    string
	ActorUserID    uuid.UUID
	AccountID      uuid.UUID
	Mode           ExecutionMode
	Intent         OrderIntent
	IssuedAt       time.Time
}

// Order is the persisted lifecycle record.
type Order struct {
	ID              uuid.UUID
	AccountID       uuid.UUID
	UserID          uuid.UUID
	InstrumentID    string
	Symbol          string
	Mode            ExecutionMode
	Side            OrderSide
	Type            OrderType
	TimeInForce     TimeInForce
	Status          OrderStatus
	Quantity        decimal.Decimal
	FilledQuantity  decimal.Decimal
	AvgFillPrice    decimal.Decimal
	LimitPrice      *decimal.Decimal
	StopPrice       *decimal.Decimal
	StopLoss        *decimal.Decimal
	TakeProfit      *decimal.Decimal
	Source          OrderSource
	StrategyID      *uuid.UUID
	StrategyVersion *int
	DecisionID      *uuid.UUID
	CommandID       uuid.UUID
	IdempotencyKey  string
	BrokerName      string
	BrokerOrderID   *string
	RejectReason    *string
	RejectCode      *string
	Version         int64
	// ReplayRunID is the market replay that owned the clock when this order
	// was created, and nil for every order decided on a live feed.
	//
	// Carried on the order rather than inferred later because it is not
	// recoverable afterwards: nothing in a filled order's own data says
	// whether its price came from a dataset or a feed, and once the two are
	// mixed in one account no question about paper-forward behaviour has a
	// clean answer.
	ReplayRunID *uuid.UUID

	// ReconciliationRequired marks an order whose venue-side outcome Vantage
	// cannot determine, or which an unresolved reconciliation issue concerns.
	//
	// Deliberately a flag rather than an order status. FAILED already means
	// "the outcome is unknown", but the name reads as a closed failure, and an
	// operator needs to see uncertainty as uncertainty. Adding a status would
	// have meant widening the state machine's transition table for every
	// ordinary execution in order to describe an exceptional condition.
	ReconciliationRequired bool
	CreatedAt              time.Time
	UpdatedAt              time.Time
	SubmittedAt            *time.Time
	ClosedAt               *time.Time
}

// RemainingQuantity is the unfilled balance.
func (o Order) RemainingQuantity() decimal.Decimal {
	rem := o.Quantity.Sub(o.FilledQuantity)
	if rem.IsNegative() {
		return decimal.Zero
	}
	return rem
}

// Fill is one execution against an order. Fills are append-only: a fill is
// never updated or deleted, because the ledger derives from them.
type Fill struct {
	ID            uuid.UUID
	OrderID       uuid.UUID
	AccountID     uuid.UUID
	InstrumentID  string
	Side          OrderSide
	Quantity      decimal.Decimal
	Price         decimal.Decimal
	Commission    decimal.Decimal
	CommissionCcy string
	Slippage      decimal.Decimal
	BrokerFillID  string
	BrokerName    string
	ExecutedAt    time.Time
	RecordedAt    time.Time
	Liquidity     string
	// IngestSource records how this execution reached Vantage: returned by a
	// PlaceOrder call, polled, or discovered by reconciliation after a lost
	// response. The accounting is identical either way, which is the point,
	// but "we were told at the time" and "we reconstructed this afterwards"
	// are different facts about the same number.
	IngestSource string
	// IssueID names the reconciliation issue that justified importing this
	// fill, where it was imported. Nil for an ordinary execution.
	IssueID *string
}

// RejectCode is a structured, machine-readable rejection reason. Every refusal
// carries one so the UI, the metrics and the audit log agree on why a trade did
// not happen, and so "why did nothing trade today?" is answerable.
type RejectCode string

const (
	RejectSchemaInvalid        RejectCode = "schema_invalid"
	RejectUnauthenticated      RejectCode = "unauthenticated"
	RejectForbidden            RejectCode = "forbidden"
	RejectAccountNotOwned      RejectCode = "account_not_owned"
	RejectModeNotPermitted     RejectCode = "execution_mode_not_permitted"
	RejectNoAuthority          RejectCode = "no_trading_authority"
	RejectAuthorityExpired     RejectCode = "trading_authority_expired"
	RejectAuthorityRevoked     RejectCode = "trading_authority_revoked"
	RejectInstrumentNotAllowed RejectCode = "instrument_not_permitted_by_authority"
	RejectStrategyNotAllowed   RejectCode = "strategy_not_permitted_by_authority"
	RejectOrderTypeNotAllowed  RejectCode = "order_type_not_permitted"
	RejectAutomationDisabled   RejectCode = "automation_disabled"
	RejectKillSwitch           RejectCode = "kill_switch_active"
	RejectMarketDataStale      RejectCode = "market_data_stale"
	RejectMarketDataInvalid    RejectCode = "market_data_invalid"
	RejectMarketClosed         RejectCode = "market_closed"
	RejectInstrumentDisabled   RejectCode = "instrument_disabled"
	RejectQuantityInvalid      RejectCode = "quantity_invalid"
	RejectPriceInvalid         RejectCode = "price_invalid"
	RejectSpreadTooWide        RejectCode = "spread_too_wide"
	RejectRiskLimit            RejectCode = "risk_limit_breached"
	RejectInsufficientMargin   RejectCode = "insufficient_margin"
	RejectDailyLoss            RejectCode = "daily_loss_limit_reached"
	RejectDrawdown             RejectCode = "drawdown_limit_reached"
	RejectMaxPositions         RejectCode = "max_open_positions"
	RejectMaxPendingOrders     RejectCode = "max_pending_orders"
	RejectExposureLimit        RejectCode = "exposure_limit_breached"
	RejectConcentration        RejectCode = "concentration_limit_breached"
	RejectLeverageLimit        RejectCode = "leverage_limit_breached"
	RejectEventRisk            RejectCode = "event_risk_blackout"
	RejectDuplicateCommand     RejectCode = "duplicate_command"
	RejectIdempotencyConflict  RejectCode = "idempotency_key_payload_mismatch"
	RejectStaleVersion         RejectCode = "stale_object_version"
	RejectBrokerRejected       RejectCode = "broker_rejected"
	RejectBrokerUnavailable    RejectCode = "broker_unavailable"
	RejectInternalError        RejectCode = "internal_error"
	// RejectReconciledAbsent closes out an order whose outcome was unknown and
	// which reconciliation then found the venue had never accepted. It is not
	// an internal error: nothing went wrong locally, and the order genuinely
	// did not happen.
	RejectReconciledAbsent RejectCode = "reconciled_absent_at_venue"
	// RejectReconciliationRequired refuses an AUTOMATED order because
	// reconciliation has unresolved divergence on the account.
	//
	// A temporary refusal, not a verdict on the order: nothing is wrong with
	// what was asked for, and the same request will be accepted once the
	// account's records are known to agree with the venue. Manual orders are
	// not refused for this reason -- an operator can see the warning and
	// decide, an algorithm cannot.
	RejectReconciliationRequired RejectCode = "reconciliation_required"
	// RejectAutopilotOff refuses an AUTOMATED order because the global
	// Autopilot switch is off.
	//
	// Distinct from RejectReconciliationRequired on purpose. Both refuse an
	// automated order, but for opposite reasons: reconciliation means the
	// platform does not trust its own records, while this means an operator
	// deliberately switched autonomous trading off. Reporting the wrong one
	// would send someone hunting a divergence that does not exist.
	//
	// Also distinct from a kill switch, which refuses EVERY order including a
	// manual one. Autopilot off leaves manual trading available, which is
	// usually the point of switching it off.
	RejectAutopilotOff RejectCode = "autopilot_off"
)

// Rejection is a structured refusal with an explanation safe to show a user.
type Rejection struct {
	Code    RejectCode
	Message string
	// Detail carries non-sensitive context, e.g. the limit and observed value.
	Detail map[string]string
}

func (r Rejection) Error() string { return fmt.Sprintf("%s: %s", r.Code, r.Message) }

// NewRejection builds a rejection with optional detail pairs.
func NewRejection(code RejectCode, msg string, kv ...string) Rejection {
	d := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		d[kv[i]] = kv[i+1]
	}
	return Rejection{Code: code, Message: msg, Detail: d}
}
