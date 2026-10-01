package mock

// Deterministic fault injection for the paper venue.
//
// The point of these modes is that OMS and reconciliation behaviour is
// exercised against a venue that actually misbehaves, rather than against a
// hand-mocked adapter. A test that stubs the adapter proves the test's
// assumptions; a test that makes the real venue lose a response proves the
// pipeline survives a lost response.
//
// Every mode is exact and repeatable. There is no probability here: a mode is
// armed, it fires the configured number of times, and then it disarms. A
// flaky fault is worse than no fault, because a failure nobody can reproduce
// gets attributed to the test rather than to the code.
//
// The modes deliberately include the two cases that are dangerous to get
// wrong:
//
//	FaultLostResponse   the venue ACCEPTS the order and the answer is lost
//	FaultTimeout        the request times out with the outcome unknown
//
// Both must leave the order FAILED and must never be retried automatically.
// Reconciliation is what resolves them, and `FetchOrderByClientID` is how it
// finds an order the venue took while we were not listening.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
)

// Fault identifies one injected failure mode.
type Fault string

const (
	// FaultTimeout makes the call exceed its deadline. The outcome is
	// unknown: nothing was written at the venue, but the caller cannot know
	// that.
	FaultTimeout Fault = "timeout"

	// FaultRejection makes the venue refuse definitively. Nothing happened,
	// so a retry would be safe -- which is exactly why this must be
	// distinguishable from a timeout.
	FaultRejection Fault = "rejection"

	// FaultLatency delays the call by a fixed duration without failing it.
	FaultLatency Fault = "latency"

	// FaultPartialFill forces a split fill regardless of order size.
	FaultPartialFill Fault = "partial_fill"

	// FaultLostResponse is the dangerous one: the order IS accepted and
	// recorded at the venue, and then the response is dropped. The caller
	// sees ErrUnknownOutcome and must not resubmit.
	FaultLostResponse Fault = "lost_response"

	// FaultDisconnect makes the venue unreachable, as a dropped connection
	// would.
	FaultDisconnect Fault = "disconnect"

	// FaultInsufficientMargin makes the venue refuse for margin. This is a
	// venue-side refusal, distinct from the local risk engine's refusal, and
	// both paths must be exercised.
	FaultInsufficientMargin Fault = "insufficient_margin"

	// FaultInvalidQuantity makes the venue reject the size as unacceptable.
	FaultInvalidQuantity Fault = "invalid_quantity"

	// FaultMarketClosed makes the venue refuse because its market is shut,
	// even when the local market clock believes otherwise. The two can
	// legitimately disagree, and the venue wins.
	FaultMarketClosed Fault = "market_closed"

	// FaultStaleQuote makes the venue refuse to trade on the price it holds.
	FaultStaleQuote Fault = "stale_quote"

	// FaultSlippage applies a fixed, exaggerated adverse move to the fill so
	// slippage handling and accounting are visible.
	FaultSlippage Fault = "slippage"

	// FaultSpreadExpansion widens the venue's effective spread by a
	// multiplier, which is what actually happens around a release.
	FaultSpreadExpansion Fault = "spread_expansion"
)

// AllFaults is every mode, for tests that assert each one is handled.
var AllFaults = []Fault{
	FaultTimeout, FaultRejection, FaultLatency, FaultPartialFill,
	FaultLostResponse, FaultDisconnect, FaultInsufficientMargin,
	FaultInvalidQuantity, FaultMarketClosed, FaultStaleQuote,
	FaultSlippage, FaultSpreadExpansion,
}

// Valid reports whether a fault name is recognised.
func (f Fault) Valid() bool {
	for _, known := range AllFaults {
		if f == known {
			return true
		}
	}
	return false
}

// FaultSpec arms one mode.
type FaultSpec struct {
	// Remaining is how many more calls the fault applies to. Arm() sets it;
	// each firing decrements it. Zero means disarmed.
	Remaining int
	// Delay is used by FaultLatency, and as the pause before failing for
	// FaultTimeout.
	Delay time.Duration
	// Factor is used by FaultSlippage (adverse fraction) and
	// FaultSpreadExpansion (spread multiplier).
	Factor decimal.Decimal
	// Fraction is used by FaultPartialFill as the portion filled.
	Fraction decimal.Decimal
}

// FaultInjector holds the armed modes for a venue.
//
// It is separate from Config because Config describes how the venue normally
// behaves, while this describes a temporary lie told to one caller. Mixing
// them would make it easy to ship a permanently broken venue.
type FaultInjector struct {
	mu     sync.Mutex
	armed  map[Fault]*FaultSpec
	fired  map[Fault]int
	enable bool
}

// NewFaultInjector returns a disabled injector.
//
// Disabled by default and enabled only by a test or a development request: a
// venue that can be made to lie in production is not a paper venue, it is a
// bug waiting for a deployment.
func NewFaultInjector() *FaultInjector {
	return &FaultInjector{
		armed: map[Fault]*FaultSpec{},
		fired: map[Fault]int{},
	}
}

// Enable turns injection on. Nothing fires while it is off, whatever is armed.
func (i *FaultInjector) Enable(on bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.enable = on
}

// Enabled reports whether injection is on.
func (i *FaultInjector) Enabled() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.enable
}

// Arm schedules a fault for the next `times` calls it applies to.
func (i *FaultInjector) Arm(f Fault, times int, spec FaultSpec) error {
	if !f.Valid() {
		return fmt.Errorf("mock: unknown fault %q", f)
	}
	if times <= 0 {
		return fmt.Errorf("mock: fault %q must be armed for at least one call", f)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	spec.Remaining = times
	i.armed[f] = &spec
	i.enable = true
	return nil
}

// Disarm removes a fault.
func (i *FaultInjector) Disarm(f Fault) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.armed, f)
}

// Reset disarms everything and clears the firing counts.
func (i *FaultInjector) Reset() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.armed = map[Fault]*FaultSpec{}
	i.fired = map[Fault]int{}
	i.enable = false
}

// Fired reports how many times a fault has fired since the last Reset.
func (i *FaultInjector) Fired(f Fault) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fired[f]
}

// Armed lists the currently armed faults and their remaining counts.
func (i *FaultInjector) Armed() map[Fault]int {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := map[Fault]int{}
	for f, spec := range i.armed {
		if spec.Remaining > 0 {
			out[f] = spec.Remaining
		}
	}
	return out
}

// take consumes one firing of a fault, returning its spec when it applies.
func (i *FaultInjector) take(f Fault) (FaultSpec, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.enable {
		return FaultSpec{}, false
	}
	spec, ok := i.armed[f]
	if !ok || spec.Remaining <= 0 {
		return FaultSpec{}, false
	}
	spec.Remaining--
	i.fired[f]++
	out := *spec
	if spec.Remaining == 0 {
		delete(i.armed, f)
	}
	return out, true
}

// peek reports whether a fault is armed without consuming it. Used by the
// modes that modify a successful result rather than replacing it.
func (i *FaultInjector) peek(f Fault) (FaultSpec, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.enable {
		return FaultSpec{}, false
	}
	spec, ok := i.armed[f]
	if !ok || spec.Remaining <= 0 {
		return FaultSpec{}, false
	}
	return *spec, true
}

// Faults exposes the injector so a test can arm modes.
func (b *Broker) Faults() *FaultInjector { return b.faults }

// errInjectedLostResponse is returned internally by preSubmit to tell
// PlaceOrder to persist the order and then drop the answer.
var errInjectedLostResponse = errors.New("mock: injected lost response")

// preSubmit applies every fault that replaces a submission's outcome.
//
// Returned error semantics matter and mirror a real venue:
//
//	ErrOrderRejected      definitive: nothing happened, a retry is safe
//	ErrUnknownOutcome     the answer is lost: NEVER retry, reconcile
//	ErrVenueUnavailable   unreachable: nothing was sent
//	ErrMarketClosed       refused by the venue's own calendar
//	ErrInsufficientMargin refused by the venue's own margin check
func (b *Broker) preSubmit(ctx context.Context) error {
	if spec, ok := b.faults.take(FaultDisconnect); ok {
		_ = spec
		return fmt.Errorf("%w: injected disconnect", broker.ErrVenueUnavailable)
	}
	if spec, ok := b.faults.take(FaultTimeout); ok {
		delay := spec.Delay
		if delay <= 0 {
			delay = 5 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: injected timeout, request cancelled in flight",
				broker.ErrUnknownOutcome)
		case <-time.After(delay):
		}
		// A timeout is an UNKNOWN outcome, not a rejection. This is the
		// distinction the whole FAILED state exists for.
		return fmt.Errorf("%w: injected timeout", broker.ErrUnknownOutcome)
	}
	if _, ok := b.faults.take(FaultRejection); ok {
		return fmt.Errorf("%w: injected rejection", broker.ErrOrderRejected)
	}
	if _, ok := b.faults.take(FaultInsufficientMargin); ok {
		return fmt.Errorf("%w: injected margin refusal", broker.ErrInsufficientMargin)
	}
	if _, ok := b.faults.take(FaultInvalidQuantity); ok {
		return fmt.Errorf("%w: injected invalid quantity", broker.ErrOrderRejected)
	}
	if _, ok := b.faults.take(FaultMarketClosed); ok {
		return fmt.Errorf("%w: injected venue-side market closure", broker.ErrMarketClosed)
	}
	if _, ok := b.faults.take(FaultStaleQuote); ok {
		return fmt.Errorf("%w: injected stale quote at the venue", broker.ErrOrderRejected)
	}
	if spec, ok := b.faults.take(FaultLatency); ok {
		delay := spec.Delay
		if delay <= 0 {
			delay = 250 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: cancelled during injected latency", broker.ErrUnknownOutcome)
		case <-time.After(delay):
		}
		return nil
	}
	// Consumed AFTER the order is written, by PlaceOrder, so the venue really
	// does hold an order nobody heard about.
	if _, ok := b.faults.peek(FaultLostResponse); ok {
		return errInjectedLostResponse
	}
	return nil
}

// consumeLostResponse fires the lost-response fault once the order exists at
// the venue.
func (b *Broker) consumeLostResponse() bool {
	_, ok := b.faults.take(FaultLostResponse)
	return ok
}

// injectedSlippage returns the adverse fraction to apply, if armed.
func (b *Broker) injectedSlippage() (decimal.Decimal, bool) {
	spec, ok := b.faults.peek(FaultSlippage)
	if !ok || spec.Factor.IsZero() {
		return decimal.Zero, false
	}
	return spec.Factor, true
}

// injectedSpreadMultiplier returns the spread multiplier to apply, if armed.
func (b *Broker) injectedSpreadMultiplier() (decimal.Decimal, bool) {
	spec, ok := b.faults.peek(FaultSpreadExpansion)
	if !ok || !spec.Factor.IsPositive() {
		return decimal.Zero, false
	}
	return spec.Factor, true
}

// injectedPartialFill returns the forced fill fraction, if armed.
func (b *Broker) injectedPartialFill() (decimal.Decimal, bool) {
	spec, ok := b.faults.peek(FaultPartialFill)
	if !ok || !spec.Fraction.IsPositive() {
		return decimal.Zero, false
	}
	return spec.Fraction, true
}
