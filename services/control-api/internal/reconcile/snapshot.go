package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/store"
)

// Snapshots: the two views being compared, captured explicitly.
//
// # Why capture rather than query as we go
//
// The previous implementation interleaved venue lookups with local queries and
// with repairs. That has three problems, and the third is the serious one:
//
//  1. A repair changes local state, so a later comparison in the same run sees
//     a different local view from an earlier one and the run is not
//     self-consistent.
//  2. The evidence behind a decision is gone once the loop moves on, so an
//     issue records a summary rather than what was actually observed.
//  3. Reconciliation is not reproducible. Given the same divergence, a second
//     run could classify it differently depending on timing — which for a
//     process that writes to the ledger is not acceptable.
//
// Capturing both sides first makes a run a pure function of two snapshots plus
// the repair policy. That is what makes the classifier unit-testable with no
// database and no venue, which is where most of this milestone's test coverage
// lives.

// BrokerSnapshot is the venue's view at one instant.
type BrokerSnapshot struct {
	AccountRef string
	FetchedAt  time.Time
	Orders     []broker.VenueOrder
	Positions  []broker.VenuePosition
	Executions []broker.ExecutionReport
	// Account is the venue's balance view. Optional: an adapter that cannot
	// report it leaves Balances false rather than reporting zeros, because a
	// zero balance and an unknown balance are very different claims.
	Account  broker.VenueAccount
	Balances bool
}

// OrderByBrokerID finds a venue order by its own identifier.
func (s BrokerSnapshot) OrderByBrokerID(id string) (broker.VenueOrder, bool) {
	for _, o := range s.Orders {
		if o.BrokerOrderID != "" && o.BrokerOrderID == id {
			return o, true
		}
	}
	return broker.VenueOrder{}, false
}

// OrderByClientID finds a venue order by the client id Vantage supplied. This
// is how an order whose acknowledgement was lost is discovered to be live.
func (s BrokerSnapshot) OrderByClientID(id string) (broker.VenueOrder, bool) {
	for _, o := range s.Orders {
		if o.ClientOrderID != "" && o.ClientOrderID == id {
			return o, true
		}
	}
	return broker.VenueOrder{}, false
}

// PositionBySymbol finds a venue position.
func (s BrokerSnapshot) PositionBySymbol(symbol string) (broker.VenuePosition, bool) {
	for _, p := range s.Positions {
		if p.Symbol == symbol {
			return p, true
		}
	}
	return broker.VenuePosition{}, false
}

// LocalSnapshot is Vantage's view at one instant.
type LocalSnapshot struct {
	Account    domain.Account
	CapturedAt time.Time
	// Orders covers everything that could still diverge: open orders, orders
	// whose outcome is unknown, and orders flagged for reconciliation. A
	// FILLED order from last week cannot acquire a new divergence, so
	// including it would only add noise.
	Orders    []domain.Order
	Positions []domain.Position
	// FillIDs is the set of broker execution ids already booked, which is what
	// makes "the venue reports an execution we do not have" answerable without
	// a query per execution.
	FillIDs map[string]bool
	// BookedFills maps a broker execution id to what Vantage actually booked
	// for it. Needed to tell an ordinary replay from a venue CONTRADICTING
	// itself: the first is normal operation, the second means one of the two
	// records is wrong about a trade that happened.
	BookedFills map[string]domain.Fill
	// FillsByOrder counts booked executions per order, used to tell a missing
	// execution from a quantity that merely disagrees.
	FillsByOrder  map[uuid.UUID][]domain.Fill
	LedgerBalance decimal.Decimal
	BalanceKnown  bool
}

// OrderByID finds a local order.
func (s LocalSnapshot) OrderByID(id uuid.UUID) (domain.Order, bool) {
	for _, o := range s.Orders {
		if o.ID == id {
			return o, true
		}
	}
	return domain.Order{}, false
}

// OrderByBrokerID finds a local order by the venue identifier it holds.
func (s LocalSnapshot) OrderByBrokerID(id string) (domain.Order, bool) {
	if id == "" {
		return domain.Order{}, false
	}
	for _, o := range s.Orders {
		if o.BrokerOrderID != nil && *o.BrokerOrderID == id {
			return o, true
		}
	}
	return domain.Order{}, false
}

// OrdersByBrokerID returns EVERY local order holding a venue identifier, so an
// ambiguous attribution can be detected rather than resolved by taking the
// first match.
//
// This distinction is the difference between FILL_MISSING_LOCALLY (exactly one
// candidate, provable, automatically repairable) and EXTRA_BROKER_FILL
// (several candidates, operator review). Returning only the first match would
// silently turn the second case into the first.
func (s LocalSnapshot) OrdersByBrokerID(id string) []domain.Order {
	if id == "" {
		return nil
	}
	var out []domain.Order
	for _, o := range s.Orders {
		if o.BrokerOrderID != nil && *o.BrokerOrderID == id {
			out = append(out, o)
		}
	}
	return out
}

// OrderByCommandID finds a local order by the client id it presented.
func (s LocalSnapshot) OrderByCommandID(id string) (domain.Order, bool) {
	for _, o := range s.Orders {
		if o.CommandID.String() == id {
			return o, true
		}
	}
	return domain.Order{}, false
}

// PositionByInstrument finds a local position.
func (s LocalSnapshot) PositionByInstrument(instrumentID string) (domain.Position, bool) {
	for _, p := range s.Positions {
		if p.InstrumentID == instrumentID {
			return p, true
		}
	}
	return domain.Position{}, false
}

// captureBroker reads the venue's view.
//
// Every fetch failure is fatal to the run. A partial snapshot compared against
// a complete local view manufactures divergence: an order the venue holds but
// which was not in the page we managed to read looks exactly like an order the
// venue has never heard of, and that misclassification would halt an account
// or, worse, close out a live order as "never reached the venue".
func (s *Service) captureBroker(ctx context.Context, adapter broker.Adapter,
	accountRef string, since time.Time, resolveClientIDs []string) (BrokerSnapshot, error) {

	snap := BrokerSnapshot{AccountRef: accountRef, FetchedAt: s.clock.Now()}

	orders, err := adapter.FetchOpenOrders(ctx, accountRef)
	if err != nil {
		return snap, fmt.Errorf("fetch venue orders: %w", err)
	}
	snap.Orders = orders

	// FetchOpenOrders returns only OPEN orders, and that is the correct
	// contract -- but it makes the snapshot incomplete in exactly the case
	// reconciliation exists for.
	//
	// An order the venue FILLED is no longer open, so it is absent from that
	// list. Comparing a local order against the list alone therefore cannot
	// distinguish "the venue never heard of this" from "the venue filled it
	// and moved on", and those two conclusions are opposites: one releases the
	// order's risk budget, the other books a position.
	//
	// This was not hypothetical. A lost-response order was classified
	// ORDER_MISSING_AT_BROKER -- "it never reached the market" -- while the
	// venue held it as filled with an execution against it. Only the
	// terminal-state guard in the repair machine stopped it being marked
	// rejected after the fill had been imported.
	//
	// So each local order the list does not cover is resolved by a DIRECT
	// lookup on its client id. A direct not-found is real evidence; absence
	// from a filtered list is not.
	seen := map[string]bool{}
	for _, o := range orders {
		if o.ClientOrderID != "" {
			seen[o.ClientOrderID] = true
		}
	}
	for _, clientID := range resolveClientIDs {
		if clientID == "" || seen[clientID] {
			continue
		}
		seen[clientID] = true
		vo, err := adapter.FetchOrderByClientID(ctx, accountRef, clientID)
		if err == nil {
			snap.Orders = append(snap.Orders, vo)
			continue
		}
		if !errors.Is(err, broker.ErrNotFound) {
			// A lookup that failed for any reason other than "no such order"
			// leaves the snapshot incomplete, and an incomplete snapshot
			// manufactures divergence. Fail the run instead.
			return snap, fmt.Errorf("fetch venue order by client id %s: %w", clientID, err)
		}
	}

	positions, err := adapter.FetchPositions(ctx, accountRef)
	if err != nil {
		return snap, fmt.Errorf("fetch venue positions: %w", err)
	}
	snap.Positions = positions

	// Executions are the evidence that makes a missing fill repairable. Polled
	// from the stored cursor so a crash resumes rather than loses whatever
	// arrived while the process was down.
	executions, err := adapter.PollExecutions(ctx, accountRef, since)
	if err != nil {
		return snap, fmt.Errorf("poll venue executions: %w", err)
	}
	snap.Executions = executions

	account, err := adapter.FetchAccount(ctx, accountRef)
	if err == nil {
		snap.Account = account
		snap.Balances = true
	}
	// A venue that cannot report a balance is not a failed run: the balance
	// comparison is informational, and losing it costs one warning-level check
	// rather than the whole reconciliation.

	return snap, nil
}

// captureLocal reads Vantage's view.
func (s *Service) captureLocal(ctx context.Context, account domain.Account) (LocalSnapshot, error) {
	snap := LocalSnapshot{
		Account:      account,
		CapturedAt:   s.clock.Now(),
		FillIDs:      map[string]bool{},
		BookedFills:  map[string]domain.Fill{},
		FillsByOrder: map[uuid.UUID][]domain.Fill{},
	}

	open, err := s.store.Trading.ListOrdersForUser(ctx, account.UserID, store.OrderFilter{
		AccountID: &account.ID, OpenOnly: true, Limit: 500,
	})
	if err != nil {
		return snap, fmt.Errorf("list local open orders: %w", err)
	}
	snap.Orders = open

	// Orders whose venue-side outcome is unknown. These are not "open" by the
	// state machine but are precisely the ones reconciliation exists to
	// resolve, so leaving them out would make recovery impossible.
	uncertain, err := s.store.Trading.OrdersRequiringReconciliation(ctx, account.ID)
	if err != nil {
		return snap, fmt.Errorf("list orders requiring reconciliation: %w", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, o := range snap.Orders {
		seen[o.ID] = true
	}
	for _, o := range uncertain {
		if !seen[o.ID] {
			snap.Orders = append(snap.Orders, o)
			seen[o.ID] = true
		}
	}

	positions, err := s.store.Trading.OpenPositions(ctx, account.ID)
	if err != nil {
		return snap, fmt.Errorf("list local positions: %w", err)
	}
	snap.Positions = positions

	for _, o := range snap.Orders {
		fills, err := s.store.Trading.FillsForOrder(ctx, o.ID)
		if err != nil {
			return snap, fmt.Errorf("list fills for order %s: %w", o.ID, err)
		}
		snap.FillsByOrder[o.ID] = fills
		for _, f := range fills {
			snap.FillIDs[f.BrokerFillID] = true
			snap.BookedFills[f.BrokerFillID] = f
		}
	}

	// Recent fills beyond the orders above, so an execution belonging to an
	// order that has since closed is still recognised as already booked
	// rather than reported as missing.
	recent, err := s.store.Trading.RecentFills(ctx, account.ID, 500)
	if err != nil {
		return snap, fmt.Errorf("list recent fills: %w", err)
	}
	for _, f := range recent {
		snap.FillIDs[f.BrokerFillID] = true
		snap.BookedFills[f.BrokerFillID] = f
	}

	balance, err := s.store.Accounts.Balance(ctx, account.ID, account.Currency)
	if err == nil {
		snap.LedgerBalance = balance.Decimal()
		snap.BalanceKnown = true
	}

	return snap, nil
}

// evidenceJSON encodes evidence for storage, never failing the caller.
//
// Evidence is supporting detail. Losing it must not abort a repair that is
// otherwise correct, so an encoding failure degrades to a note saying so
// rather than propagating.
func evidenceJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(fmt.Sprintf(`{"encoding_error":%q}`, err.Error()))
	}
	return raw
}

// orderEvidence summarises a local order for an issue's evidence.
func orderEvidence(o domain.Order) map[string]any {
	m := map[string]any{
		"order_id":                o.ID.String(),
		"status":                  string(o.Status),
		"side":                    string(o.Side),
		"type":                    string(o.Type),
		"quantity":                o.Quantity.String(),
		"filled_quantity":         o.FilledQuantity.String(),
		"avg_fill_price":          o.AvgFillPrice.String(),
		"client_order_id":         o.CommandID.String(),
		"instrument_id":           o.InstrumentID,
		"reconciliation_required": o.ReconciliationRequired,
	}
	if o.BrokerOrderID != nil {
		m["broker_order_id"] = *o.BrokerOrderID
	}
	return m
}

// venueOrderEvidence summarises a venue order.
func venueOrderEvidence(o broker.VenueOrder) map[string]any {
	return map[string]any{
		"broker_order_id": o.BrokerOrderID,
		"client_order_id": o.ClientOrderID,
		"symbol":          o.Symbol,
		"side":            string(o.Side),
		"status":          string(o.Status),
		"quantity":        o.Quantity.String(),
		"filled_quantity": o.FilledQuantity.String(),
		"avg_fill_price":  o.AvgFillPrice.String(),
		"reject_reason":   o.RejectReason,
		"updated_at":      o.UpdatedAt,
	}
}

// executionEvidence summarises a venue execution.
func executionEvidence(e broker.ExecutionReport) map[string]any {
	return map[string]any{
		"broker_execution_id": e.BrokerFillID,
		"broker_order_id":     e.BrokerOrderID,
		"client_order_id":     e.ClientOrderID,
		"symbol":              e.Symbol,
		"side":                string(e.Side),
		"quantity":            e.Quantity.String(),
		"price":               e.Price.String(),
		"commission":          e.Commission.String(),
		"commission_currency": e.CommissionCcy,
		"executed_at":         e.ExecutedAt,
	}
}
