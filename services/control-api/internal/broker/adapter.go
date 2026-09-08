// Package broker defines the venue abstraction and the errors every adapter
// must speak.
//
// The interface is shaped by what trading venues actually do, not by what one
// venue's API happens to look like. It assumes:
//
//   - Requests may be lost. Every mutating call carries a client-supplied
//     identifier so the venue (or the adapter) can recognise a retry.
//   - Responses may be lost. An adapter must therefore support asking the
//     venue what it thinks the state is, which is what reconciliation uses.
//   - Fills arrive asynchronously and may be partial, out of order, or
//     replayed after a reconnect.
//   - The venue is the authority on its own state. Where Vantage and the venue
//     disagree, Vantage is wrong until proven otherwise.
//
// MetaTrader 5, a REST venue and a FIX session all fit behind this. What does
// NOT fit — and is deliberately excluded — is any assumption that a call
// returning an error means the order did not reach the market.
package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// Adapter is the interface every broker integration implements.
type Adapter interface {
	// Name is the stable identifier stored on orders and connections.
	Name() string

	// Capabilities describes what this venue supports, so the order pipeline
	// can refuse an unsupported request rather than emulating it.
	Capabilities() Capabilities

	// Health reports connectivity. It must not throw on a disconnected venue;
	// a degraded verdict is a normal answer.
	Health(ctx context.Context) Health

	// PlaceOrder submits an order. The request carries a ClientOrderID which
	// the adapter must present to the venue where the venue supports it, so a
	// retry after a lost response is recognised rather than duplicated.
	//
	// An error from PlaceOrder does NOT mean the order was not placed. It
	// means the outcome is unknown, and the caller must reconcile.
	PlaceOrder(ctx context.Context, req PlaceOrderRequest) (OrderAck, error)

	// CancelOrder requests cancellation. Cancellation races fills; a
	// successful cancel request does not guarantee the order did not fill.
	CancelOrder(ctx context.Context, req CancelOrderRequest) (CancelAck, error)

	// FetchOrder returns the venue's view of one order.
	FetchOrder(ctx context.Context, accountRef, brokerOrderID string) (VenueOrder, error)

	// FetchOrderByClientID resolves an order the venue may have accepted even
	// though the response was lost. This is what makes a lost-response retry
	// safe: the adapter can ask "did you already take this?".
	FetchOrderByClientID(ctx context.Context, accountRef, clientOrderID string) (VenueOrder, error)

	// FetchOpenOrders and FetchPositions provide the venue's authoritative
	// state for reconciliation.
	FetchOpenOrders(ctx context.Context, accountRef string) ([]VenueOrder, error)
	FetchPositions(ctx context.Context, accountRef string) ([]VenuePosition, error)
	FetchAccount(ctx context.Context, accountRef string) (VenueAccount, error)

	// PollExecutions returns executions recorded since the given cursor. A
	// pull model is used rather than a push callback because it is replayable:
	// after a crash the caller resumes from its last processed cursor instead
	// of losing whatever arrived while it was down.
	PollExecutions(ctx context.Context, accountRef string, since time.Time) ([]ExecutionReport, error)
}

// Capabilities describes venue support.
type Capabilities struct {
	SupportedOrderTypes  []domain.OrderType
	SupportedTIF         []domain.TimeInForce
	SupportsPartialFills bool
	SupportsCancel       bool
	SupportsModify       bool
	// SupportsClientOrderID reports whether the venue echoes and deduplicates
	// on a client-supplied id. When false, the adapter must implement its own
	// deduplication and say so, because the guarantee is weaker.
	SupportsClientOrderID bool
	// Netting reports whether the venue nets positions per instrument. Vantage
	// models netting; a hedging venue must be mapped explicitly.
	Netting bool
	// MaxOrdersPerSecond is the venue's documented rate limit, respected by
	// the adapter rather than discovered by being throttled.
	MaxOrdersPerSecond int
}

// SupportsOrderType reports whether the venue accepts an order type.
func (c Capabilities) SupportsOrderType(t domain.OrderType) bool {
	for _, v := range c.SupportedOrderTypes {
		if v == t {
			return true
		}
	}
	return false
}

// HealthState grades venue connectivity.
type HealthState string

const (
	HealthUp       HealthState = "up"
	HealthDegraded HealthState = "degraded"
	HealthDown     HealthState = "down"
)

// Health is an adapter's self-report.
type Health struct {
	State       HealthState
	Message     string
	LastSuccess time.Time
	LastError   string
	LatencyMS   int64
	CircuitOpen bool
	CheckedAt   time.Time
}

// PlaceOrderRequest is a venue-neutral order submission.
type PlaceOrderRequest struct {
	// ClientOrderID is Vantage's idempotency identity for this submission.
	ClientOrderID string
	AccountRef    string
	Symbol        string
	Side          domain.OrderSide
	Type          domain.OrderType
	Quantity      decimal.Decimal
	LimitPrice    *decimal.Decimal
	StopPrice     *decimal.Decimal
	StopLoss      *decimal.Decimal
	TakeProfit    *decimal.Decimal
	TimeInForce   domain.TimeInForce
	// MaxSlippageFraction bounds acceptable slippage on a market order. A
	// venue that cannot honour it must reject rather than fill worse.
	MaxSlippageFraction decimal.Decimal
	SubmittedAt         time.Time
}

// OrderAck is the venue's response to a submission.
type OrderAck struct {
	BrokerOrderID string
	Status        VenueOrderStatus
	AcceptedAt    time.Time
	// Duplicate is true when the venue recognised the ClientOrderID as one it
	// had already accepted. The caller must treat this as success, not as a
	// second order.
	Duplicate bool
	// Fills carries executions the venue reported immediately, which is the
	// common case for a market order.
	Fills []ExecutionReport
}

// CancelOrderRequest asks the venue to cancel.
type CancelOrderRequest struct {
	ClientRequestID string
	AccountRef      string
	BrokerOrderID   string
}

// CancelAck is the venue's response to a cancellation.
type CancelAck struct {
	BrokerOrderID string
	Status        VenueOrderStatus
	CancelledAt   time.Time
	// AlreadyFilled reports the race: the order filled before the cancel
	// landed. Callers must not treat this as an error condition to retry.
	AlreadyFilled bool
}

// VenueOrderStatus is the venue's own vocabulary, mapped explicitly rather
// than assumed to match Vantage's state machine.
type VenueOrderStatus string

const (
	VenueAccepted        VenueOrderStatus = "accepted"
	VenueWorking         VenueOrderStatus = "working"
	VenuePartiallyFilled VenueOrderStatus = "partially_filled"
	VenueFilled          VenueOrderStatus = "filled"
	VenueCancelled       VenueOrderStatus = "cancelled"
	VenueRejected        VenueOrderStatus = "rejected"
	VenueExpired         VenueOrderStatus = "expired"
	VenueUnknown         VenueOrderStatus = "unknown"
)

// ToOrderStatus maps a venue status into Vantage's state machine.
func (v VenueOrderStatus) ToOrderStatus() (domain.OrderStatus, bool) {
	switch v {
	case VenueAccepted, VenueWorking:
		return domain.OrderSubmitted, true
	case VenuePartiallyFilled:
		return domain.OrderPartiallyFilled, true
	case VenueFilled:
		return domain.OrderFilled, true
	case VenueCancelled:
		return domain.OrderCancelled, true
	case VenueRejected:
		return domain.OrderRejected, true
	case VenueExpired:
		return domain.OrderExpired, true
	default:
		// An unknown venue status is never guessed at. It is surfaced so
		// reconciliation can resolve it against a fresh fetch.
		return "", false
	}
}

// VenueOrder is the venue's view of an order.
type VenueOrder struct {
	BrokerOrderID  string
	ClientOrderID  string
	Symbol         string
	Side           domain.OrderSide
	Type           domain.OrderType
	Status         VenueOrderStatus
	Quantity       decimal.Decimal
	FilledQuantity decimal.Decimal
	AvgFillPrice   decimal.Decimal
	LimitPrice     *decimal.Decimal
	StopPrice      *decimal.Decimal
	RejectReason   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// VenuePosition is the venue's view of an open position.
type VenuePosition struct {
	Symbol        string
	Side          domain.OrderSide
	Quantity      decimal.Decimal
	AvgEntryPrice decimal.Decimal
	UnrealizedPnL decimal.Decimal
	Currency      string
	OpenedAt      time.Time
}

// VenueAccount is the venue's view of the account.
type VenueAccount struct {
	AccountRef string
	Currency   string
	Balance    decimal.Decimal
	Equity     decimal.Decimal
	MarginUsed decimal.Decimal
	FreeMargin decimal.Decimal
	Leverage   decimal.Decimal
	FetchedAt  time.Time
}

// ExecutionReport is one execution as the venue reported it.
type ExecutionReport struct {
	BrokerFillID  string
	BrokerOrderID string
	ClientOrderID string
	Symbol        string
	Side          domain.OrderSide
	Quantity      decimal.Decimal
	Price         decimal.Decimal
	Commission    decimal.Decimal
	CommissionCcy string
	Liquidity     string
	ExecutedAt    time.Time
}

// Errors adapters return. Callers branch on these, never on message text.
var (
	// ErrOrderRejected means the venue definitively refused the order. This is
	// a terminal, known outcome.
	ErrOrderRejected = errors.New("broker: order rejected by venue")
	// ErrUnknownOutcome means the request may or may not have reached the
	// venue. The caller must mark the order FAILED and reconcile; it must NOT
	// retry blindly, because a retry could double the position.
	ErrUnknownOutcome = errors.New("broker: outcome unknown, reconciliation required")
	// ErrVenueUnavailable means the venue could not be reached at all and the
	// request definitely was not delivered.
	ErrVenueUnavailable = errors.New("broker: venue unavailable")
	// ErrRateLimited means the venue throttled the request.
	ErrRateLimited = errors.New("broker: rate limited by venue")
	// ErrNotFound means the venue has no record of the referenced object.
	ErrNotFound = errors.New("broker: not found at venue")
	// ErrMarketClosed means the instrument is not tradable right now.
	ErrMarketClosed = errors.New("broker: market closed")
	// ErrInsufficientMargin means the venue refused for want of margin.
	ErrInsufficientMargin = errors.New("broker: insufficient margin at venue")
	// ErrUnsupported means the adapter cannot express the request. It is
	// returned rather than emulating the behaviour client-side.
	ErrUnsupported = errors.New("broker: operation not supported by this venue")
)

// RejectionError carries a venue rejection with its reason.
type RejectionError struct {
	Reason string
	Code   string
}

func (e RejectionError) Error() string {
	return fmt.Sprintf("broker: order rejected by venue: %s", e.Reason)
}

func (e RejectionError) Is(target error) bool { return target == ErrOrderRejected }

// Registry holds the adapters compiled into this build.
//
// Registration is explicit and happens in main. There is no plugin loading and
// no dynamic dispatch by configuration string alone: an adapter that is not in
// the binary cannot be selected, which is one of the three gates keeping this
// build paper-only.
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: map[string]Adapter{}}
}

// Register adds an adapter.
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return errors.New("broker: cannot register a nil adapter")
	}
	if _, exists := r.adapters[a.Name()]; exists {
		return fmt.Errorf("broker: adapter %q is already registered", a.Name())
	}
	r.adapters[a.Name()] = a
	return nil
}

// Get returns an adapter by name.
func (r *Registry) Get(name string) (Adapter, error) {
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("broker: no adapter named %q is compiled into this build", name)
	}
	return a, nil
}

// Names lists the registered adapters.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		out = append(out, n)
	}
	return out
}
