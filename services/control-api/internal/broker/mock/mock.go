// Package mock implements a simulated trading venue.
//
// The point of this adapter is to be UNCOMFORTABLE to trade against, in the
// same ways a real venue is. A mock that fills every order instantly at the
// displayed mid price produces strategies that only work against that mock.
// This one:
//
//   - executes across the spread (buys lift the ask, sells hit the bid);
//   - applies configurable slippage and latency;
//   - fills market orders partially when size warrants it;
//   - leaves limit and stop orders resting until price actually trades through;
//   - refuses orders for want of margin, outside market hours, or on stale prices;
//   - charges commission and, for positions held overnight, swap;
//   - keeps its own books, so Vantage's view can genuinely diverge from it.
//
// It is deterministic when seeded, so tests assert exact fills rather than
// tolerances.
package mock

import (
	"context"
	"errors"
	"fmt"
	// math/rand is deliberate here. This is a simulation, and its randomness is
	// seeded so a test can assert an exact fill rather than a tolerance.
	// crypto/rand would make the mock venue non-reproducible, which is the
	// opposite of what it is for. No security decision anywhere reads these
	// values: session tokens, MFA secrets and recovery codes all use
	// crypto/rand.
	// nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
)

// QuoteSource supplies the prices the venue executes against.
type QuoteSource interface {
	LatestQuote(ctx context.Context, instrumentID string) (domain.Quote, *domain.Quote, error)
	Instrument(ctx context.Context, id string) (domain.Instrument, error)
}

// MarketStatusSource reports whether an instrument is tradable now.
type MarketStatusSource interface {
	Status(t time.Time) domain.MarketStatus
}

// Config controls the simulation's realism knobs.
type Config struct {
	// SlippageFraction is the average adverse price movement applied to market
	// orders, as a fraction of price.
	SlippageFraction decimal.Decimal
	// SlippageJitter randomises slippage around the average. Zero makes
	// execution deterministic for a given seed.
	SlippageJitter decimal.Decimal
	// LatencyMin/Max bound the simulated round trip.
	LatencyMin time.Duration
	LatencyMax time.Duration
	// PartialFillThreshold is the order size above which the venue splits the
	// fill. Below it, market orders fill in one go.
	PartialFillThreshold decimal.Decimal
	// PartialFillFraction is the portion filled on the first execution when a
	// fill is split.
	PartialFillFraction decimal.Decimal
	// RejectionRate is the probability of a spurious venue rejection, which
	// exists so error handling is exercised rather than assumed.
	RejectionRate float64
	// MaxQuoteAge beyond which the venue refuses to trade on a price.
	MaxQuoteAge time.Duration
	// Deterministic disables jitter and random rejection entirely.
	Deterministic bool
	// Seed fixes the random sequence when Deterministic is false but
	// reproducibility is still wanted.
	Seed int64
}

// DefaultConfig returns realistic retail CFD-like behaviour.
func DefaultConfig() Config {
	return Config{
		SlippageFraction:     decimal.RequireFromString("0.0001"), // 1 basis point
		SlippageJitter:       decimal.RequireFromString("0.00005"),
		LatencyMin:           15 * time.Millisecond,
		LatencyMax:           80 * time.Millisecond,
		PartialFillThreshold: decimal.RequireFromString("0.50"),
		PartialFillFraction:  decimal.RequireFromString("0.60"),
		RejectionRate:        0.0,
		MaxQuoteAge:          10 * time.Second,
		Deterministic:        false,
		Seed:                 0,
	}
}

// DeterministicConfig returns a configuration with no randomness at all, for
// tests that assert exact prices and quantities.
func DeterministicConfig() Config {
	c := DefaultConfig()
	c.Deterministic = true
	c.SlippageJitter = decimal.Zero
	c.LatencyMin = 0
	c.LatencyMax = 0
	c.RejectionRate = 0
	c.Seed = 1
	return c
}

// Broker is the simulated venue.
type Broker struct {
	pool   *db.Pool
	quotes QuoteSource
	clock  domain.Clock
	market MarketStatusSource
	cfg    Config

	mu  sync.Mutex
	rng *rand.Rand

	// faults is a deterministic fault-injection layer, disabled unless a test
	// or a development request arms it. See faults.go.
	faults *FaultInjector
}

// New builds the mock venue.
func New(pool *db.Pool, quotes QuoteSource, market MarketStatusSource, clock domain.Clock, cfg Config) *Broker {
	seed := cfg.Seed
	if seed == 0 && !cfg.Deterministic {
		seed = time.Now().UnixNano()
	}
	return &Broker{
		pool:   pool,
		quotes: quotes,
		clock:  clock,
		market: market,
		cfg:    cfg,
		rng:    rand.New(rand.NewSource(seed)),
		faults: NewFaultInjector(),
	}
}

// Name identifies this adapter.
func (b *Broker) Name() string { return "mock" }

// Capabilities describes the simulated venue.
func (b *Broker) Capabilities() broker.Capabilities {
	return broker.Capabilities{
		SupportedOrderTypes: []domain.OrderType{
			domain.OrderTypeMarket, domain.OrderTypeLimit, domain.OrderTypeStop,
		},
		SupportedTIF: []domain.TimeInForce{
			domain.TIFGoodTilCancelled, domain.TIFImmediateOrCancel, domain.TIFDay,
		},
		SupportsPartialFills:  true,
		SupportsCancel:        true,
		SupportsModify:        false,
		SupportsClientOrderID: true,
		Netting:               true,
		MaxOrdersPerSecond:    20,
	}
}

// Health reports the venue as up whenever its database is reachable.
func (b *Broker) Health(ctx context.Context) broker.Health {
	start := time.Now()
	h := broker.Health{CheckedAt: b.clock.Now(), State: broker.HealthUp}
	if err := b.pool.Ping(ctx); err != nil {
		h.State = broker.HealthDown
		h.LastError = err.Error()
		h.Message = "simulated venue store unreachable"
		return h
	}
	h.LatencyMS = time.Since(start).Milliseconds()
	h.LastSuccess = h.CheckedAt
	h.Message = "simulated venue (paper only)"
	return h
}

// EnsureAccount creates the venue-side account if it does not exist.
func (b *Broker) EnsureAccount(ctx context.Context, accountRef, currency string, balance, leverage decimal.Decimal) error {
	_, err := b.pool.Exec(ctx, `
		INSERT INTO mock_venue_accounts (account_ref, currency, balance, leverage)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (account_ref) DO NOTHING`,
		accountRef, currency, balance, leverage)
	return err
}

// PlaceOrder submits an order to the simulated venue.
func (b *Broker) PlaceOrder(ctx context.Context, req broker.PlaceOrderRequest) (broker.OrderAck, error) {
	if err := b.simulateLatency(ctx); err != nil {
		return broker.OrderAck{}, err
	}

	// Injected faults are evaluated before anything is written, so a refusal
	// or a disconnect leaves the venue exactly as it was. The one exception is
	// FaultLostResponse, which must leave a REAL order behind: preSubmit
	// signals it and the answer is dropped after the write commits.
	loseResponse := false
	if err := b.preSubmit(ctx); err != nil {
		if !errors.Is(err, errInjectedLostResponse) {
			return broker.OrderAck{}, err
		}
		loseResponse = true
	}

	// Venue-side idempotency. A retry after a lost response finds the order
	// the venue already accepted and returns it, rather than opening a second.
	if existing, err := b.FetchOrderByClientID(ctx, req.AccountRef, req.ClientOrderID); err == nil {
		fills, ferr := b.fillsForOrder(ctx, existing.BrokerOrderID)
		if ferr != nil {
			return broker.OrderAck{}, ferr
		}
		return broker.OrderAck{
			BrokerOrderID: existing.BrokerOrderID,
			Status:        existing.Status,
			AcceptedAt:    existing.CreatedAt,
			Duplicate:     true,
			Fills:         fills,
		}, nil
	} else if !errors.Is(err, broker.ErrNotFound) {
		return broker.OrderAck{}, err
	}

	now := b.clock.Now()
	if b.market != nil && !b.market.Status(now).Tradable() {
		return broker.OrderAck{}, fmt.Errorf("%w: %s", broker.ErrMarketClosed, b.market.Status(now))
	}

	inst, err := b.quotes.Instrument(ctx, req.Symbol)
	if err != nil {
		return broker.OrderAck{}, fmt.Errorf("%w: unknown symbol %q", broker.ErrNotFound, req.Symbol)
	}
	quote, _, err := b.quotes.LatestQuote(ctx, req.Symbol)
	if err != nil {
		return broker.OrderAck{}, fmt.Errorf("%w: no price for %q", broker.ErrVenueUnavailable, req.Symbol)
	}
	// A venue will not trade on a price it no longer believes.
	if age := now.Sub(quote.IngestedAt); age > b.cfg.MaxQuoteAge {
		return broker.OrderAck{}, broker.RejectionError{
			Code:   "stale_price",
			Reason: fmt.Sprintf("no current price for %s (last update %s ago)", req.Symbol, age.Truncate(time.Millisecond)),
		}
	}

	if !b.Capabilities().SupportsOrderType(req.Type) {
		return broker.OrderAck{}, fmt.Errorf("%w: order type %s", broker.ErrUnsupported, req.Type)
	}
	if err := inst.Spec.ValidateQuantity(req.Quantity); err != nil {
		return broker.OrderAck{}, broker.RejectionError{Code: "invalid_quantity", Reason: err.Error()}
	}
	if b.spuriousRejection() {
		return broker.OrderAck{}, broker.RejectionError{
			Code: "venue_rejected", Reason: "simulated venue rejection",
		}
	}

	brokerOrderID := "MOCK-" + uuid.NewString()
	var ack broker.OrderAck

	err = b.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO mock_venue_orders (broker_order_id, client_order_id, account_ref, symbol,
				side, type, time_in_force, status, quantity, limit_price, stop_price,
				stop_loss, take_profit)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'accepted',$8,$9,$10,$11,$12)`,
			brokerOrderID, req.ClientOrderID, req.AccountRef, req.Symbol, req.Side, req.Type,
			req.TimeInForce, req.Quantity, req.LimitPrice, req.StopPrice,
			req.StopLoss, req.TakeProfit); err != nil {
			return err
		}

		// Only market orders execute on arrival. Limit and stop orders rest
		// until the market trades through their level, which is checked by
		// ProcessRestingOrders as prices move.
		if req.Type != domain.OrderTypeMarket {
			if _, err := tx.Exec(ctx,
				`UPDATE mock_venue_orders SET status='working', updated_at=now() WHERE broker_order_id=$1`,
				brokerOrderID); err != nil {
				return err
			}
			ack = broker.OrderAck{BrokerOrderID: brokerOrderID, Status: broker.VenueWorking, AcceptedAt: now}
			return nil
		}

		execPrice := b.executionPrice(quote, req.Side, inst)
		if err := b.checkMargin(ctx, tx, req.AccountRef, inst, req.Quantity, execPrice); err != nil {
			return err
		}

		// Split the fill when the order is large enough to move through more
		// than one price level.
		qty := req.Quantity
		firstQty := qty
		partial := b.cfg.PartialFillThreshold.IsPositive() && qty.GreaterThan(b.cfg.PartialFillThreshold)
		fraction := b.cfg.PartialFillFraction
		if forced, ok := b.injectedPartialFill(); ok {
			// Forced regardless of size, so a minimum-size order can still
			// exercise the partial-fill path.
			partial, fraction = true, forced
		}
		if partial {
			firstQty = inst.Spec.NormaliseQuantity(qty.Mul(fraction))
			if firstQty.IsZero() || firstQty.GreaterThanOrEqual(qty) {
				partial = false
				firstQty = qty
			}
		}

		fill, err := b.executeTx(ctx, tx, req.AccountRef, brokerOrderID, inst, req.Side, firstQty, execPrice, now)
		if err != nil {
			return err
		}
		ack.Fills = append(ack.Fills, fill)

		if partial {
			// The remainder stays working; the caller sees a partial fill and
			// must handle it rather than assuming completion.
			if _, err := tx.Exec(ctx, `
				UPDATE mock_venue_orders SET status='partially_filled', updated_at=now()
				WHERE broker_order_id=$1`, brokerOrderID); err != nil {
				return err
			}
			ack.Status = broker.VenuePartiallyFilled
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE mock_venue_orders SET status='filled', updated_at=now()
				WHERE broker_order_id=$1`, brokerOrderID); err != nil {
				return err
			}
			ack.Status = broker.VenueFilled
		}
		ack.BrokerOrderID = brokerOrderID
		ack.AcceptedAt = now
		return nil
	})
	if err != nil {
		return broker.OrderAck{}, err
	}

	// The order exists at the venue and the caller will never hear about it.
	// This is the case the FAILED order state and reconciliation exist for:
	// the caller must NOT resubmit, and FetchOrderByClientID is how the order
	// is found again.
	if loseResponse && b.consumeLostResponse() {
		return broker.OrderAck{}, fmt.Errorf(
			"%w: injected lost response after venue acceptance", broker.ErrUnknownOutcome)
	}
	return ack, nil
}

// executionPrice crosses the spread and applies slippage in the adverse
// direction. Slippage always hurts: a simulator whose slippage is symmetric
// flatters every strategy that trades frequently.
func (b *Broker) executionPrice(q domain.Quote, side domain.OrderSide, inst domain.Instrument) decimal.Decimal {
	base := q.ExecutionPrice(side)

	// A widened spread moves the execution price further from the mid on the
	// side being crossed, which is what a release actually does to a book.
	if mult, ok := b.injectedSpreadMultiplier(); ok {
		half := q.Spread().Div(decimal.NewFromInt(2))
		extra := half.Mul(mult.Sub(decimal.NewFromInt(1)))
		if side == domain.SideBuy {
			base = base.Add(extra)
		} else {
			base = base.Sub(extra)
		}
	}

	slip := b.cfg.SlippageFraction
	if injected, ok := b.injectedSlippage(); ok {
		// Replaces the configured slippage rather than adding to it, so the
		// injected number is the number a test can assert on.
		slip = injected
		adj := base.Mul(slip)
		if side == domain.SideBuy {
			return inst.Spec.RoundPrice(base.Add(adj))
		}
		return inst.Spec.RoundPrice(base.Sub(adj))
	}
	if !b.cfg.Deterministic && b.cfg.SlippageJitter.IsPositive() {
		b.mu.Lock()
		j := b.rng.Float64()*2 - 1 // [-1, 1)
		b.mu.Unlock()
		slip = slip.Add(b.cfg.SlippageJitter.Mul(decimal.NewFromFloat(j)))
		if slip.IsNegative() {
			slip = decimal.Zero
		}
	}
	adj := base.Mul(slip)
	if side == domain.SideBuy {
		return inst.Spec.RoundPrice(base.Add(adj))
	}
	return inst.Spec.RoundPrice(base.Sub(adj))
}

// executeTx books one execution and updates the venue's position and balance.
func (b *Broker) executeTx(ctx context.Context, tx pgx.Tx, accountRef, brokerOrderID string,
	inst domain.Instrument, side domain.OrderSide, qty, price decimal.Decimal, now time.Time) (broker.ExecutionReport, error) {

	commission := inst.Spec.CommissionPerLot.Mul(qty)
	fillID := "MOCKFILL-" + uuid.NewString()

	if _, err := tx.Exec(ctx, `
		INSERT INTO mock_venue_fills (broker_fill_id, broker_order_id, account_ref, symbol,
			side, quantity, price, commission, commission_ccy, liquidity, executed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'taker',$10)`,
		fillID, brokerOrderID, accountRef, inst.ID, side, qty, price,
		commission, string(inst.QuoteCcy), now); err != nil {
		return broker.ExecutionReport{}, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE mock_venue_orders o
		SET filled_quantity = agg.qty, avg_fill_price = agg.px, updated_at = now()
		FROM (
			SELECT COALESCE(SUM(quantity),0) qty,
			       CASE WHEN COALESCE(SUM(quantity),0) > 0
			            THEN SUM(price*quantity)/SUM(quantity) ELSE 0 END px
			FROM mock_venue_fills WHERE broker_order_id = $1
		) agg
		WHERE o.broker_order_id = $1`, brokerOrderID); err != nil {
		return broker.ExecutionReport{}, err
	}

	if err := b.applyPositionTx(ctx, tx, accountRef, inst, side, qty, price); err != nil {
		return broker.ExecutionReport{}, err
	}

	return broker.ExecutionReport{
		BrokerFillID:  fillID,
		BrokerOrderID: brokerOrderID,
		Symbol:        inst.ID,
		Side:          side,
		Quantity:      qty,
		Price:         price,
		Commission:    commission,
		CommissionCcy: string(inst.QuoteCcy),
		Liquidity:     "taker",
		ExecutedAt:    now,
	}, nil
}

// applyPositionTx nets the fill into the venue's position book and realises
// P&L into the venue's balance when exposure is reduced.
func (b *Broker) applyPositionTx(ctx context.Context, tx pgx.Tx, accountRef string,
	inst domain.Instrument, side domain.OrderSide, qty, price decimal.Decimal) error {

	var curSide string
	var curQty, curPrice decimal.Decimal
	err := tx.QueryRow(ctx, `
		SELECT side, quantity, avg_entry_price FROM mock_venue_positions
		WHERE account_ref = $1 AND symbol = $2 FOR UPDATE`, accountRef, inst.ID).
		Scan(&curSide, &curQty, &curPrice)

	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `
			INSERT INTO mock_venue_positions (account_ref, symbol, side, quantity, avg_entry_price)
			VALUES ($1,$2,$3,$4,$5)`, accountRef, inst.ID, side, qty, price)
		return err
	}
	if err != nil {
		return err
	}

	if domain.OrderSide(curSide) == side {
		newQty := curQty.Add(qty)
		newPrice := curPrice.Mul(curQty).Add(price.Mul(qty)).Div(newQty)
		_, err = tx.Exec(ctx, `
			UPDATE mock_venue_positions SET quantity=$3, avg_entry_price=$4, updated_at=now()
			WHERE account_ref=$1 AND symbol=$2`, accountRef, inst.ID, newQty, newPrice)
		return err
	}

	closing := decimal.Min(curQty, qty)
	diff := price.Sub(curPrice)
	if domain.OrderSide(curSide) == domain.SideSell {
		diff = diff.Neg()
	}
	realised := diff.Mul(closing).Mul(inst.Spec.ContractSize)

	if _, err := tx.Exec(ctx, `
		UPDATE mock_venue_accounts SET balance = balance + $2, updated_at = now()
		WHERE account_ref = $1`, accountRef, realised); err != nil {
		return err
	}

	remainingOld := curQty.Sub(closing)
	remainingNew := qty.Sub(closing)
	switch {
	case remainingOld.IsPositive():
		_, err = tx.Exec(ctx, `
			UPDATE mock_venue_positions SET quantity=$3, updated_at=now()
			WHERE account_ref=$1 AND symbol=$2`, accountRef, inst.ID, remainingOld)
	case remainingNew.IsPositive():
		_, err = tx.Exec(ctx, `
			UPDATE mock_venue_positions SET side=$3, quantity=$4, avg_entry_price=$5, updated_at=now()
			WHERE account_ref=$1 AND symbol=$2`, accountRef, inst.ID, side, remainingNew, price)
	default:
		_, err = tx.Exec(ctx,
			`DELETE FROM mock_venue_positions WHERE account_ref=$1 AND symbol=$2`, accountRef, inst.ID)
	}
	return err
}

// checkMargin refuses an order the venue-side account cannot support.
func (b *Broker) checkMargin(ctx context.Context, tx pgx.Tx, accountRef string,
	inst domain.Instrument, qty, price decimal.Decimal) error {

	var balance decimal.Decimal
	var currency string
	if err := tx.QueryRow(ctx,
		`SELECT balance, currency FROM mock_venue_accounts WHERE account_ref = $1`, accountRef).
		Scan(&balance, &currency); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: unknown venue account %q", broker.ErrNotFound, accountRef)
		}
		return err
	}

	required := inst.MarginRequired(qty, price).Decimal()
	// The venue account and the instrument may be in different currencies. The
	// simulation compares like with like only when they match, and otherwise
	// applies the account's own balance as the ceiling in its own currency,
	// which is the conservative direction.
	if balance.LessThan(required) {
		return fmt.Errorf("%w: need %s, have %s", broker.ErrInsufficientMargin,
			required.StringFixed(2), balance.StringFixed(2))
	}
	return nil
}

// ProcessRestingOrders triggers limit and stop orders whose level the market
// has reached, and applies stop-loss and take-profit exits on open positions.
//
// A real venue does this continuously. Vantage drives it from the market-data
// tick loop, so a resting order can only fill on a price that actually
// occurred, never on one the strategy wished for.
func (b *Broker) ProcessRestingOrders(ctx context.Context, accountRef string) ([]broker.ExecutionReport, error) {
	now := b.clock.Now()
	if b.market != nil && !b.market.Status(now).Tradable() {
		return nil, nil
	}

	rows, err := b.pool.Query(ctx, `
		SELECT broker_order_id, symbol, side, type, quantity, filled_quantity, limit_price, stop_price
		FROM mock_venue_orders
		WHERE account_ref = $1 AND status IN ('working','partially_filled','accepted')
		ORDER BY created_at`, accountRef)
	if err != nil {
		return nil, err
	}

	type resting struct {
		id              string
		symbol          string
		side            domain.OrderSide
		orderType       domain.OrderType
		qty, filled     decimal.Decimal
		limitPx, stopPx *decimal.Decimal
	}
	var pending []resting
	for rows.Next() {
		var r resting
		if err := rows.Scan(&r.id, &r.symbol, &r.side, &r.orderType, &r.qty, &r.filled,
			&r.limitPx, &r.stopPx); err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var reports []broker.ExecutionReport
	for _, r := range pending {
		inst, err := b.quotes.Instrument(ctx, r.symbol)
		if err != nil {
			continue
		}
		quote, _, err := b.quotes.LatestQuote(ctx, r.symbol)
		if err != nil {
			continue
		}
		if now.Sub(quote.IngestedAt) > b.cfg.MaxQuoteAge {
			continue
		}

		remaining := r.qty.Sub(r.filled)
		if !remaining.IsPositive() {
			continue
		}

		var trigger bool
		var execPrice decimal.Decimal

		switch r.orderType {
		case domain.OrderTypeMarket:
			// A market order left resting is the remainder of a partial fill.
			trigger = true
			execPrice = b.executionPrice(quote, r.side, inst)

		case domain.OrderTypeLimit:
			if r.limitPx == nil {
				continue
			}
			// A buy limit fills when the ask trades down to the limit; a sell
			// limit when the bid trades up to it. Filling at the limit price
			// (not better) is the conservative assumption.
			if r.side == domain.SideBuy && quote.Ask.LessThanOrEqual(*r.limitPx) {
				trigger, execPrice = true, *r.limitPx
			}
			if r.side == domain.SideSell && quote.Bid.GreaterThanOrEqual(*r.limitPx) {
				trigger, execPrice = true, *r.limitPx
			}

		case domain.OrderTypeStop:
			if r.stopPx == nil {
				continue
			}
			// A stop becomes a market order once touched, and therefore
			// suffers the spread and slippage like any market order. Modelling
			// a stop as filling exactly at its trigger price is the single
			// most common way a backtest understates losses.
			if r.side == domain.SideBuy && quote.Ask.GreaterThanOrEqual(*r.stopPx) {
				trigger, execPrice = true, b.executionPrice(quote, r.side, inst)
			}
			if r.side == domain.SideSell && quote.Bid.LessThanOrEqual(*r.stopPx) {
				trigger, execPrice = true, b.executionPrice(quote, r.side, inst)
			}
		}

		if !trigger {
			continue
		}

		err = b.pool.InTx(ctx, func(tx pgx.Tx) error {
			if err := b.checkMargin(ctx, tx, accountRef, inst, remaining, execPrice); err != nil {
				if errors.Is(err, broker.ErrInsufficientMargin) {
					_, uerr := tx.Exec(ctx, `
						UPDATE mock_venue_orders SET status='rejected',
						       reject_reason='insufficient margin at trigger', updated_at=now()
						WHERE broker_order_id=$1`, r.id)
					return uerr
				}
				return err
			}
			rep, err := b.executeTx(ctx, tx, accountRef, r.id, inst, r.side, remaining, execPrice, now)
			if err != nil {
				return err
			}
			reports = append(reports, rep)
			_, err = tx.Exec(ctx,
				`UPDATE mock_venue_orders SET status='filled', updated_at=now() WHERE broker_order_id=$1`, r.id)
			return err
		})
		if err != nil {
			return reports, err
		}
	}
	return reports, nil
}

// CancelOrder cancels a resting order.
func (b *Broker) CancelOrder(ctx context.Context, req broker.CancelOrderRequest) (broker.CancelAck, error) {
	if err := b.simulateLatency(ctx); err != nil {
		return broker.CancelAck{}, err
	}
	now := b.clock.Now()

	var status string
	err := b.pool.QueryRow(ctx, `
		UPDATE mock_venue_orders
		SET status = CASE WHEN status IN ('accepted','working','partially_filled')
		                  THEN 'cancelled' ELSE status END,
		    updated_at = now()
		WHERE broker_order_id = $1 AND account_ref = $2
		RETURNING status`, req.BrokerOrderID, req.AccountRef).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return broker.CancelAck{}, broker.ErrNotFound
	}
	if err != nil {
		return broker.CancelAck{}, err
	}

	// The order may have filled between the client's decision to cancel and
	// this request arriving. That is reported, not treated as an error.
	if status == "filled" {
		return broker.CancelAck{
			BrokerOrderID: req.BrokerOrderID,
			Status:        broker.VenueFilled,
			AlreadyFilled: true,
		}, nil
	}
	return broker.CancelAck{
		BrokerOrderID: req.BrokerOrderID,
		Status:        broker.VenueOrderStatus(status),
		CancelledAt:   now,
	}, nil
}

// FetchOrder returns the venue's view of one order.
func (b *Broker) FetchOrder(ctx context.Context, accountRef, brokerOrderID string) (broker.VenueOrder, error) {
	return b.fetchOrder(ctx, `broker_order_id = $2`, accountRef, brokerOrderID)
}

// FetchOrderByClientID resolves an order by the client identifier, which is
// what makes retrying a lost response safe.
func (b *Broker) FetchOrderByClientID(ctx context.Context, accountRef, clientOrderID string) (broker.VenueOrder, error) {
	return b.fetchOrder(ctx, `client_order_id = $2`, accountRef, clientOrderID)
}

func (b *Broker) fetchOrder(ctx context.Context, predicate, accountRef, key string) (broker.VenueOrder, error) {
	var o broker.VenueOrder
	var reject *string
	err := b.pool.QueryRow(ctx, `
		SELECT broker_order_id, client_order_id, symbol, side, type, status, quantity,
		       filled_quantity, avg_fill_price, limit_price, stop_price, reject_reason,
		       created_at, updated_at
		FROM mock_venue_orders WHERE account_ref = $1 AND `+predicate,
		accountRef, key).
		Scan(&o.BrokerOrderID, &o.ClientOrderID, &o.Symbol, &o.Side, &o.Type, &o.Status,
			&o.Quantity, &o.FilledQuantity, &o.AvgFillPrice, &o.LimitPrice, &o.StopPrice,
			&reject, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return broker.VenueOrder{}, broker.ErrNotFound
	}
	if err != nil {
		return broker.VenueOrder{}, err
	}
	if reject != nil {
		o.RejectReason = *reject
	}
	return o, nil
}

// FetchOpenOrders returns the venue's working orders.
func (b *Broker) FetchOpenOrders(ctx context.Context, accountRef string) ([]broker.VenueOrder, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT broker_order_id, client_order_id, symbol, side, type, status, quantity,
		       filled_quantity, avg_fill_price, limit_price, stop_price, reject_reason,
		       created_at, updated_at
		FROM mock_venue_orders
		WHERE account_ref = $1 AND status IN ('accepted','working','partially_filled')
		ORDER BY created_at`, accountRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []broker.VenueOrder
	for rows.Next() {
		var o broker.VenueOrder
		var reject *string
		if err := rows.Scan(&o.BrokerOrderID, &o.ClientOrderID, &o.Symbol, &o.Side, &o.Type,
			&o.Status, &o.Quantity, &o.FilledQuantity, &o.AvgFillPrice, &o.LimitPrice,
			&o.StopPrice, &reject, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		if reject != nil {
			o.RejectReason = *reject
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FetchPositions returns the venue's open positions.
func (b *Broker) FetchPositions(ctx context.Context, accountRef string) ([]broker.VenuePosition, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT p.symbol, p.side, p.quantity, p.avg_entry_price, p.opened_at, i.quote_currency
		FROM mock_venue_positions p
		LEFT JOIN instruments i ON i.id = p.symbol
		WHERE p.account_ref = $1 ORDER BY p.symbol`, accountRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []broker.VenuePosition
	for rows.Next() {
		var p broker.VenuePosition
		var ccy *string
		if err := rows.Scan(&p.Symbol, &p.Side, &p.Quantity, &p.AvgEntryPrice, &p.OpenedAt, &ccy); err != nil {
			return nil, err
		}
		if ccy != nil {
			p.Currency = *ccy
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FetchAccount returns the venue's account state.
func (b *Broker) FetchAccount(ctx context.Context, accountRef string) (broker.VenueAccount, error) {
	var a broker.VenueAccount
	a.AccountRef = accountRef
	a.FetchedAt = b.clock.Now()
	err := b.pool.QueryRow(ctx, `
		SELECT currency, balance, leverage FROM mock_venue_accounts WHERE account_ref = $1`,
		accountRef).Scan(&a.Currency, &a.Balance, &a.Leverage)
	if errors.Is(err, pgx.ErrNoRows) {
		return broker.VenueAccount{}, broker.ErrNotFound
	}
	if err != nil {
		return broker.VenueAccount{}, err
	}
	a.Equity = a.Balance
	return a, nil
}

// PollExecutions returns executions recorded at or after a cursor.
func (b *Broker) PollExecutions(ctx context.Context, accountRef string, since time.Time) ([]broker.ExecutionReport, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT f.broker_fill_id, f.broker_order_id, o.client_order_id, f.symbol, f.side,
		       f.quantity, f.price, f.commission, f.commission_ccy, f.liquidity, f.executed_at
		FROM mock_venue_fills f
		JOIN mock_venue_orders o ON o.broker_order_id = f.broker_order_id
		WHERE f.account_ref = $1 AND f.executed_at >= $2
		ORDER BY f.executed_at, f.broker_fill_id`, accountRef, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExecutions(rows)
}

func (b *Broker) fillsForOrder(ctx context.Context, brokerOrderID string) ([]broker.ExecutionReport, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT f.broker_fill_id, f.broker_order_id, o.client_order_id, f.symbol, f.side,
		       f.quantity, f.price, f.commission, f.commission_ccy, f.liquidity, f.executed_at
		FROM mock_venue_fills f
		JOIN mock_venue_orders o ON o.broker_order_id = f.broker_order_id
		WHERE f.broker_order_id = $1 ORDER BY f.executed_at`, brokerOrderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExecutions(rows)
}

func scanExecutions(rows pgx.Rows) ([]broker.ExecutionReport, error) {
	var out []broker.ExecutionReport
	for rows.Next() {
		var r broker.ExecutionReport
		if err := rows.Scan(&r.BrokerFillID, &r.BrokerOrderID, &r.ClientOrderID, &r.Symbol,
			&r.Side, &r.Quantity, &r.Price, &r.Commission, &r.CommissionCcy,
			&r.Liquidity, &r.ExecutedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApplySwap charges overnight financing on open positions. Called by the
// scheduler at the venue's rollover time, so a strategy that holds positions
// overnight pays for the privilege exactly as it would in production.
func (b *Broker) ApplySwap(ctx context.Context, accountRef string) (decimal.Decimal, error) {
	positions, err := b.FetchPositions(ctx, accountRef)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, p := range positions {
		inst, err := b.quotes.Instrument(ctx, p.Symbol)
		if err != nil {
			continue
		}
		rate := inst.Spec.SwapLongPerLot
		if p.Side == domain.SideSell {
			rate = inst.Spec.SwapShortPerLot
		}
		charge := rate.Mul(p.Quantity)
		if charge.IsZero() {
			continue
		}
		if _, err := b.pool.Exec(ctx, `
			UPDATE mock_venue_accounts SET balance = balance + $2, updated_at = now()
			WHERE account_ref = $1`, accountRef, charge); err != nil {
			return total, err
		}
		total = total.Add(charge)
	}
	return total, nil
}

func (b *Broker) simulateLatency(ctx context.Context) error {
	if b.cfg.LatencyMax <= 0 {
		return nil
	}
	d := b.cfg.LatencyMin
	if b.cfg.LatencyMax > b.cfg.LatencyMin && !b.cfg.Deterministic {
		b.mu.Lock()
		extra := time.Duration(b.rng.Int63n(int64(b.cfg.LatencyMax - b.cfg.LatencyMin)))
		b.mu.Unlock()
		d += extra
	}
	select {
	case <-ctx.Done():
		// A cancelled context mid-flight is exactly the "outcome unknown" case:
		// the request may already have reached the venue.
		return fmt.Errorf("%w: request cancelled in flight", broker.ErrUnknownOutcome)
	case <-time.After(d):
		return nil
	}
}

func (b *Broker) spuriousRejection() bool {
	if b.cfg.Deterministic || b.cfg.RejectionRate <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rng.Float64() < b.cfg.RejectionRate
}

// Compile-time assertion that the mock satisfies the adapter contract.
var _ broker.Adapter = (*Broker)(nil)
