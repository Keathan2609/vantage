// Package oms is the order management system: the single path by which any
// order can reach a broker.
//
// # The pipeline
//
// Every order — manual, strategy-generated or autopilot — passes through the
// same ordered sequence of gates. There is no second entry point, no "internal"
// bypass, and no way for strategy or model code to reach a broker adapter
// directly, because nothing outside this package holds a reference to one.
//
//  1. schema validation          11. risk checks
//  2. authentication             12. idempotency claim
//  3. authorisation (role)       13. optimistic version check
//  4. account ownership          14. persistence
//  5. execution-mode check       15. broker adapter
//  6. trading-authority check    16. broker result normalisation
//  7. kill-switch check          17. portfolio and ledger update
//  8. market-data freshness      18. audit event
//  9. market/session check       19. metrics and observability
//  10. instrument-rule validation
//
// # Transaction boundaries
//
// The pipeline runs in three phases, and the split matters:
//
//	Phase A  read-only validation and the risk decision
//	Phase B  one transaction: claim the idempotency key, persist the order and
//	         the decision snapshot, or persist the rejection
//	Phase C  the broker call, with NO database transaction held open
//	Phase D  one transaction: apply the venue's answer to orders, fills,
//	         positions, the ledger and the audit log
//
// Holding a transaction open across the broker call would put a network round
// trip inside a lock on financial rows. Splitting them means a crash between C
// and D leaves an order in a known, recoverable state — FAILED, awaiting
// reconciliation — rather than a lock nobody can clear.
package oms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/crypto"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/portfolio"
	"github.com/vantage/control-api/internal/risk"
	"github.com/vantage/control-api/internal/store"
)

// Service executes the order pipeline.
type Service struct {
	store       *store.Store
	brokers     *broker.Registry
	riskEngine  *risk.Engine
	portfolio   *portfolio.Service
	converter   *fx.Converter
	clock       domain.Clock
	marketClock *domain.MarketClock
	mode        domain.ExecutionMode
}

// New builds the OMS.
//
// The execution mode is fixed at construction from validated configuration and
// stamped onto every order. It is not a per-request parameter, so no caller can
// ask for a different one.
func New(
	s *store.Store,
	brokers *broker.Registry,
	riskEngine *risk.Engine,
	pf *portfolio.Service,
	converter *fx.Converter,
	clock domain.Clock,
	marketClock *domain.MarketClock,
	mode domain.ExecutionMode,
) (*Service, error) {
	if mode != domain.ModePaper {
		return nil, fmt.Errorf("oms: refusing to construct in %s mode: this build is paper-only", mode)
	}
	return &Service{
		store: s, brokers: brokers, riskEngine: riskEngine, portfolio: pf,
		converter: converter, clock: clock, marketClock: marketClock, mode: mode,
	}, nil
}

// PlaceOrderRequest is a validated request to open or close exposure.
type PlaceOrderRequest struct {
	// IdempotencyKey is supplied by the client and is mandatory. Without one
	// a network retry would be indistinguishable from a second order.
	IdempotencyKey  string
	ActorUserID     uuid.UUID
	ActorRole       domain.Role
	AccountID       uuid.UUID
	InstrumentID    string
	Side            domain.OrderSide
	Type            domain.OrderType
	Quantity        decimal.Decimal
	LimitPrice      *decimal.Decimal
	StopPrice       *decimal.Decimal
	StopLoss        *decimal.Decimal
	TakeProfit      *decimal.Decimal
	TimeInForce     domain.TimeInForce
	Source          domain.OrderSource
	StrategyID      *uuid.UUID
	StrategyVersion *int
	// RequestHash fingerprints the caller's payload so the same key used with
	// different parameters is rejected rather than silently satisfied.
	RequestHash   string
	IPAddress     *string
	UserAgent     *string
	RequestID     string
	CorrelationID string
}

// Result is the outcome of a placement attempt.
type Result struct {
	Order      *domain.Order
	Fills      []domain.Fill
	Rejection  *domain.Rejection
	Decision   domain.RiskDecision
	DecisionID *uuid.UUID
	// Duplicate reports that this exact command had already been processed and
	// the stored outcome was returned instead of executing again.
	Duplicate bool
	Snapshot  *portfolio.Snapshot
}

// Accepted reports whether an order reached the venue.
func (r Result) Accepted() bool { return r.Rejection == nil && r.Order != nil }

// Errors the caller distinguishes.
var (
	ErrIdempotencyKeyRequired = errors.New("oms: an idempotency key is required for every order")
	ErrIdempotencyConflict    = errors.New("oms: this idempotency key was already used with different parameters")
	ErrCommandInFlight        = errors.New("oms: an identical order is already being processed")
)

// PlaceOrder runs the full pipeline.
func (s *Service) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (Result, error) {
	log := logging.FromContext(ctx)
	now := s.clock.Now()
	start := time.Now()

	if req.IdempotencyKey == "" {
		return Result{}, ErrIdempotencyKeyRequired
	}

	// ---- Step 3: role authorisation -------------------------------------
	// Checked here as well as in the HTTP layer. A second check costs
	// nothing and means a future internal caller cannot skip it.
	if !req.ActorRole.CanTrade() {
		rej := domain.NewRejection(domain.RejectForbidden,
			"This role may not place orders.", "role", string(req.ActorRole))
		return Result{Rejection: &rej}, nil
	}

	// ---- Step 4: account ownership --------------------------------------
	// Loaded scoped to the caller. A foreign account id returns not-found,
	// which is also the answer for an account that does not exist.
	account, err := s.store.Accounts.AccountForUser(ctx, req.ActorUserID, req.AccountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			rej := domain.NewRejection(domain.RejectAccountNotOwned, "Account not found.")
			return Result{Rejection: &rej}, nil
		}
		return Result{}, fmt.Errorf("oms: load account: %w", err)
	}

	// ---- Step 5: execution mode -----------------------------------------
	if account.Mode != s.mode || s.mode != domain.ModePaper {
		rej := domain.NewRejection(domain.RejectModeNotPermitted,
			"This build executes paper orders only.",
			"account_mode", string(account.Mode), "service_mode", string(s.mode))
		return Result{Rejection: &rej}, nil
	}

	ctxData, rej, err := s.gather(ctx, account, req, now)
	if err != nil {
		return Result{}, err
	}
	if rej != nil {
		return s.persistRejection(ctx, req, account, ctxData, *rej, domain.RiskDecision{}, now)
	}

	// ---- Step 11: risk ---------------------------------------------------
	decision, err := s.riskEngine.Evaluate(ctx, ctxData.riskInput)
	if err != nil {
		return Result{}, fmt.Errorf("oms: risk evaluation: %w", err)
	}
	if !decision.Approved {
		r := rejectionFromDecision(decision)
		return s.persistRejection(ctx, req, account, ctxData, r, decision, now)
	}

	// Risk may only reduce. This assertion is cheap and catches a class of bug
	// that would otherwise place a larger order than was authorised.
	if decision.ApprovedQuantity.GreaterThan(req.Quantity) {
		return Result{}, fmt.Errorf(
			"oms: risk engine returned a larger quantity (%s) than requested (%s); refusing",
			decision.ApprovedQuantity, req.Quantity)
	}

	// ---- Phase B: claim the key and persist ------------------------------
	commandID := uuid.New()
	var order domain.Order
	var decisionID uuid.UUID
	var duplicateResult *store.RegisteredCommand

	err = s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		existing, regErr := s.store.Trading.RegisterCommand(ctx, tx, store.RegisteredCommand{
			AccountID:      account.ID,
			IdempotencyKey: req.IdempotencyKey,
			CommandType:    "place_order",
			RequestHash:    req.RequestHash,
			CommandID:      commandID,
			ActorUserID:    req.ActorUserID,
		})
		switch {
		case errors.Is(regErr, store.ErrIdempotencyConflict):
			return ErrIdempotencyConflict
		case errors.Is(regErr, store.ErrCommandInFlight):
			return ErrCommandInFlight
		case errors.Is(regErr, store.ErrDuplicateCommand):
			duplicateResult = existing
			return nil
		case regErr != nil:
			return regErr
		}

		snapshot := s.buildDecisionSnapshot(req, account, ctxData, decision, "accepted", "", "")
		var derr error
		decisionID, derr = s.store.Research.CreateDecisionTx(ctx, tx, snapshot)
		if derr != nil {
			return derr
		}

		o := domain.Order{
			AccountID:       account.ID,
			UserID:          req.ActorUserID,
			InstrumentID:    req.InstrumentID,
			Mode:            s.mode,
			Side:            req.Side,
			Type:            req.Type,
			TimeInForce:     req.TimeInForce,
			Status:          domain.OrderCreated,
			Quantity:        decision.ApprovedQuantity,
			LimitPrice:      req.LimitPrice,
			StopPrice:       req.StopPrice,
			StopLoss:        req.StopLoss,
			TakeProfit:      req.TakeProfit,
			Source:          req.Source,
			StrategyID:      req.StrategyID,
			StrategyVersion: req.StrategyVersion,
			DecisionID:      &decisionID,
			CommandID:       commandID,
			IdempotencyKey:  req.IdempotencyKey,
			BrokerName:      account.BrokerName,
		}
		created, cerr := s.store.Trading.CreateOrderTx(ctx, tx, o)
		if cerr != nil {
			return cerr
		}

		// CREATED -> VALIDATING -> ACCEPTED. The intermediate state is
		// recorded rather than skipped so the transition history shows the
		// order passed validation rather than jumping straight to acceptance.
		validating, verr := s.store.Trading.TransitionOrderTx(ctx, tx, created.ID, created.Version,
			domain.OrderValidating, "entering validation", "system", &req.ActorUserID)
		if verr != nil {
			return verr
		}
		accepted, aerr := s.store.Trading.TransitionOrderTx(ctx, tx, validating.ID, validating.Version,
			domain.OrderAccepted, "all pre-trade checks passed", "system", &req.ActorUserID)
		if aerr != nil {
			return aerr
		}
		order = accepted

		return s.audit(ctx, tx, req, account, domain.AuditOrderIntent, domain.AuditSuccess,
			"order", order.ID.String(), map[string]any{
				"symbol": ctxData.instrument.Symbol, "side": req.Side, "type": req.Type,
				"quantity": order.Quantity.String(), "source": req.Source,
			})
	})
	if err != nil {
		return Result{}, err
	}

	// A duplicate command returns the original outcome. The venue is never
	// contacted a second time.
	if duplicateResult != nil {
		return s.resultFromStoredCommand(ctx, req, duplicateResult)
	}

	// ---- Phase C: the broker call ----------------------------------------
	adapter, err := s.brokers.Get(account.BrokerName)
	if err != nil {
		return s.failOrder(ctx, req, account, order, domain.NewRejection(
			domain.RejectBrokerUnavailable, "No adapter is available for this broker."), now)
	}

	accountRef := account.ID.String()
	if account.BrokerAcctRef != nil && *account.BrokerAcctRef != "" {
		accountRef = *account.BrokerAcctRef
	}

	ack, brokerErr := adapter.PlaceOrder(ctx, broker.PlaceOrderRequest{
		// The command id is the venue-facing idempotency token. A retry after
		// a lost response presents the same value and the venue recognises it.
		ClientOrderID:       commandID.String(),
		AccountRef:          accountRef,
		Symbol:              req.InstrumentID,
		Side:                req.Side,
		Type:                req.Type,
		Quantity:            order.Quantity,
		LimitPrice:          req.LimitPrice,
		StopPrice:           req.StopPrice,
		StopLoss:            req.StopLoss,
		TakeProfit:          req.TakeProfit,
		TimeInForce:         req.TimeInForce,
		MaxSlippageFraction: ctxData.limits.MaxSlippageFraction,
		SubmittedAt:         now,
	})

	metrics.BrokerCallDuration.WithLabelValues(account.BrokerName, "place_order").
		Observe(time.Since(start).Seconds())

	if brokerErr != nil {
		return s.handleBrokerError(ctx, req, account, order, brokerErr, now)
	}

	// ---- Phase D: apply the venue's answer -------------------------------
	result, err := s.applyAck(ctx, req, account, ctxData, order, ack, decisionID, now)
	if err != nil {
		return Result{}, err
	}

	metrics.OrdersAccepted.WithLabelValues(ctxData.instrument.Symbol, string(req.Source)).Inc()
	log.Info("order accepted",
		"order_id", result.Order.ID, "symbol", ctxData.instrument.Symbol,
		"side", req.Side, "quantity", result.Order.Quantity.String(),
		"status", result.Order.Status, "fills", len(result.Fills),
		"duration_ms", time.Since(start).Milliseconds())

	return result, nil
}

// gatheredContext is everything Phase A collected.
type gatheredContext struct {
	instrument domain.Instrument
	quote      domain.Quote
	health     domain.MarketDataHealth
	market     domain.MarketStatus
	authority  domain.TradingAuthority
	limits     domain.RiskLimits
	snapshot   portfolio.Snapshot
	riskInput  risk.Input
	killSwitch *domain.KillSwitch
	eventName  string
	eventAt    time.Time
	blackout   bool
}

// gather runs steps 6 to 10 and assembles the risk engine's input.
func (s *Service) gather(ctx context.Context, account domain.Account, req PlaceOrderRequest, now time.Time) (gatheredContext, *domain.Rejection, error) {
	var g gatheredContext

	// ---- Step 6: trading authority ---------------------------------------
	authority, err := s.store.Control.ActiveAuthorityForAccount(ctx, account.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			r := domain.NewRejection(domain.RejectNoAuthority,
				"No active trading authority exists for this account.")
			return g, &r, nil
		}
		return g, nil, fmt.Errorf("oms: load authority: %w", err)
	}
	g.authority = authority

	if ok, r := authority.Effective(now); !ok {
		return g, &r, nil
	}
	if !authority.PermitsInstrument(req.InstrumentID) {
		r := domain.NewRejection(domain.RejectInstrumentNotAllowed,
			fmt.Sprintf("Trading authority does not cover %s.", req.InstrumentID),
			"instrument", req.InstrumentID)
		return g, &r, nil
	}
	if !authority.PermitsOrderType(req.Type) {
		r := domain.NewRejection(domain.RejectOrderTypeNotAllowed,
			fmt.Sprintf("Trading authority does not permit %s orders.", req.Type),
			"order_type", string(req.Type))
		return g, &r, nil
	}
	if req.Source != domain.SourceManual {
		if !authority.AutomationEnabled {
			r := domain.NewRejection(domain.RejectAutomationDisabled,
				"Automated trading is switched off for this account.")
			return g, &r, nil
		}
		if !authority.PermitsStrategy(req.StrategyID) {
			r := domain.NewRejection(domain.RejectStrategyNotAllowed,
				"Trading authority does not permit this strategy.")
			return g, &r, nil
		}
	}

	// ---- Step 7: kill switches -------------------------------------------
	ksState, err := s.store.Control.KillSwitchesFor(ctx, account.UserID, account.ID, account.BrokerName, req.StrategyID)
	if err != nil {
		return g, nil, fmt.Errorf("oms: load kill switches: %w", err)
	}
	if blocked, ks := ksState.Blocked(); blocked {
		g.killSwitch = ks
		r := domain.NewRejection(domain.RejectKillSwitch,
			fmt.Sprintf("Trading is halted by a %s kill switch: %s", ks.Scope, ks.Reason),
			"scope", string(ks.Scope), "reason", ks.Reason)
		return g, &r, nil
	}

	// ---- Step 10 (partly): instrument rules ------------------------------
	instrument, err := s.store.Market.Instrument(ctx, req.InstrumentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			r := domain.NewRejection(domain.RejectInstrumentDisabled, "Unknown instrument.")
			return g, &r, nil
		}
		return g, nil, fmt.Errorf("oms: load instrument: %w", err)
	}
	g.instrument = instrument

	if !instrument.Spec.SupportedOrderTypes.Contains(req.Type) {
		r := domain.NewRejection(domain.RejectOrderTypeNotAllowed,
			fmt.Sprintf("%s does not support %s orders.", instrument.Symbol, req.Type),
			"order_type", string(req.Type))
		return g, &r, nil
	}
	normalised := instrument.Spec.NormaliseQuantity(req.Quantity)
	if err := instrument.Spec.ValidateQuantity(normalised); err != nil {
		r := domain.NewRejection(domain.RejectQuantityInvalid, err.Error(),
			"requested", req.Quantity.String(), "normalised", normalised.String())
		return g, &r, nil
	}
	for name, p := range map[string]*decimal.Decimal{
		"limit price": req.LimitPrice, "stop price": req.StopPrice,
		"stop loss": req.StopLoss, "take profit": req.TakeProfit,
	} {
		if p == nil {
			continue
		}
		if err := instrument.Spec.ValidatePrice(*p); err != nil {
			r := domain.NewRejection(domain.RejectPriceInvalid,
				fmt.Sprintf("%s: %v", name, err))
			return g, &r, nil
		}
	}
	if req.Type.RequiresLimitPrice() && req.LimitPrice == nil {
		r := domain.NewRejection(domain.RejectSchemaInvalid,
			fmt.Sprintf("A %s order requires a limit price.", req.Type))
		return g, &r, nil
	}
	if req.Type.RequiresStopPrice() && req.StopPrice == nil {
		r := domain.NewRejection(domain.RejectSchemaInvalid,
			fmt.Sprintf("A %s order requires a stop price.", req.Type))
		return g, &r, nil
	}

	// ---- Step 8: market-data freshness -----------------------------------
	quote, prev, err := s.store.Market.LatestQuote(ctx, req.InstrumentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			r := domain.NewRejection(domain.RejectMarketDataStale,
				fmt.Sprintf("No market data is available for %s.", instrument.Symbol))
			return g, &r, nil
		}
		return g, nil, fmt.Errorf("oms: load quote: %w", err)
	}
	g.quote = quote
	g.health = domain.EvaluateQuoteHealth(quote, prev, domain.DefaultDataQualityPolicy(), now)

	// ---- Step 9: market session ------------------------------------------
	g.market = s.marketClock.Status(now)

	// Remaining inputs for the risk engine.
	limits, err := s.store.Control.RiskLimitsForAccount(ctx, account.ID)
	if err != nil {
		return g, nil, fmt.Errorf("oms: load risk limits: %w", err)
	}
	g.limits = limits

	snapshot, err := s.portfolio.Compute(ctx, account)
	if err != nil {
		return g, nil, fmt.Errorf("oms: portfolio snapshot: %w", err)
	}
	g.snapshot = snapshot

	var existing *domain.Position
	for i := range snapshot.Positions {
		if snapshot.Positions[i].Position.InstrumentID == req.InstrumentID {
			existing = &snapshot.Positions[i].Position
			break
		}
	}

	// Event risk: a high-impact release inside the blackout window.
	blackoutFrom := now
	blackoutTo := now.Add(time.Duration(limits.EventBlackoutBeforeMinutes) * time.Minute)
	events, err := s.store.Research.UpcomingHighImpactEvents(ctx, req.InstrumentID,
		blackoutFrom.Add(-time.Duration(limits.EventBlackoutAfterMinutes)*time.Minute), blackoutTo)
	if err != nil {
		return g, nil, fmt.Errorf("oms: load event risk: %w", err)
	}
	if len(events) > 0 {
		g.blackout = true
		g.eventName = events[0].EventName
		g.eventAt = events[0].ScheduledAt
	}

	g.riskInput = risk.Input{
		Intent: domain.OrderIntent{
			AccountID:    account.ID,
			InstrumentID: req.InstrumentID,
			Side:         req.Side,
			Type:         req.Type,
			Quantity:     normalised,
			LimitPrice:   req.LimitPrice,
			StopPrice:    req.StopPrice,
			StopLoss:     req.StopLoss,
			TakeProfit:   req.TakeProfit,
			TimeInForce:  req.TimeInForce,
			Source:       req.Source,
			StrategyID:   req.StrategyID,
			CreatedAt:    now,
		},
		Account:          account,
		Snapshot:         snapshot,
		Limits:           limits,
		Authority:        authority,
		Instrument:       instrument,
		Quote:            quote,
		Health:           g.health,
		Market:           g.market,
		EventBlackout:    g.blackout,
		EventName:        g.eventName,
		EventAt:          g.eventAt,
		OpenPositions:    snapshot.State.OpenPositions,
		PendingOrders:    snapshot.State.PendingOrders,
		ExistingPosition: existing,
		Now:              now,
	}
	return g, nil, nil
}

// persistRejection records a refusal so it is visible and explainable.
//
// A refused order is still an order: it gets a row, a decision snapshot, a risk
// event and an audit entry. "Why didn't it trade?" is answered from stored
// evidence, not from logs that may have rotated.
func (s *Service) persistRejection(ctx context.Context, req PlaceOrderRequest, account domain.Account,
	g gatheredContext, rej domain.Rejection, decision domain.RiskDecision, now time.Time) (Result, error) {

	commandID := uuid.New()
	var decisionID uuid.UUID
	var rejected domain.Order
	var duplicate *store.RegisteredCommand

	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		existing, regErr := s.store.Trading.RegisterCommand(ctx, tx, store.RegisteredCommand{
			AccountID:      account.ID,
			IdempotencyKey: req.IdempotencyKey,
			CommandType:    "place_order",
			RequestHash:    req.RequestHash,
			CommandID:      commandID,
			ActorUserID:    req.ActorUserID,
		})
		switch {
		case errors.Is(regErr, store.ErrIdempotencyConflict):
			return ErrIdempotencyConflict
		case errors.Is(regErr, store.ErrCommandInFlight):
			return ErrCommandInFlight
		case errors.Is(regErr, store.ErrDuplicateCommand):
			duplicate = existing
			return nil
		case regErr != nil:
			return regErr
		}

		snap := s.buildDecisionSnapshot(req, account, g, decision, "rejected", string(rej.Code), rej.Message)
		var derr error
		decisionID, derr = s.store.Research.CreateDecisionTx(ctx, tx, snap)
		if derr != nil {
			return derr
		}

		// An order row is only created when the instrument is known; a
		// rejection for an unknown instrument has nothing to hang one on.
		if g.instrument.ID != "" {
			qty := req.Quantity
			if qty.LessThanOrEqual(decimal.Zero) {
				qty = g.instrument.Spec.MinQuantity
			}
			o := domain.Order{
				AccountID: account.ID, UserID: req.ActorUserID, InstrumentID: req.InstrumentID,
				Mode: s.mode, Side: req.Side, Type: req.Type, TimeInForce: req.TimeInForce,
				Status: domain.OrderCreated, Quantity: qty,
				LimitPrice: req.LimitPrice, StopPrice: req.StopPrice,
				StopLoss: req.StopLoss, TakeProfit: req.TakeProfit,
				Source: req.Source, StrategyID: req.StrategyID, StrategyVersion: req.StrategyVersion,
				DecisionID: &decisionID, CommandID: commandID,
				IdempotencyKey: req.IdempotencyKey, BrokerName: account.BrokerName,
			}
			created, cerr := s.store.Trading.CreateOrderTx(ctx, tx, o)
			if cerr != nil {
				return cerr
			}
			r, rerr := s.store.Trading.RejectOrderTx(ctx, tx, created.ID, created.Version, rej,
				"system", &req.ActorUserID)
			if rerr != nil {
				return rerr
			}
			rejected = r
		}

		severity := domain.SeverityWarning
		if rej.Code == domain.RejectKillSwitch || rej.Code == domain.RejectDailyLoss ||
			rej.Code == domain.RejectDrawdown {
			severity = domain.SeverityCritical
		}
		checkName := domain.RiskCheckName(rej.Code)
		if decision.FirstFailure != nil {
			checkName = decision.FirstFailure.Name
		}
		var orderID *uuid.UUID
		if rejected.ID != uuid.Nil {
			orderID = &rejected.ID
		}
		if err := s.store.Control.RecordRiskEvent(ctx, tx, domain.RiskEvent{
			AccountID: &account.ID, UserID: &req.ActorUserID, Severity: severity,
			Check: checkName, Code: rej.Code, Message: rej.Message,
			InstrumentID: &req.InstrumentID, StrategyID: req.StrategyID, OrderID: orderID,
			Detail: rej.Detail,
		}); err != nil {
			return err
		}

		if err := s.store.Trading.CompleteCommand(ctx, tx, account.ID, req.IdempotencyKey,
			store.CommandRejected, orderID, map[string]any{
				"code": rej.Code, "message": rej.Message, "detail": rej.Detail,
			}); err != nil {
			return err
		}

		action := domain.AuditOrderRejected
		result := domain.AuditFailure
		if rej.Code == domain.RejectKillSwitch {
			action = domain.AuditBlockedTradeAttempt
			result = domain.AuditBlocked
		}
		return s.auditWithResult(ctx, tx, req, account, action, result, "order",
			orderIDString(orderID), map[string]any{
				"code": rej.Code, "reason": rej.Message,
				"symbol": req.InstrumentID, "source": req.Source,
			})
	})
	if err != nil {
		return Result{}, err
	}
	if duplicate != nil {
		return s.resultFromStoredCommand(ctx, req, duplicate)
	}

	metrics.OrdersRejected.WithLabelValues(req.InstrumentID, string(rej.Code), string(req.Source)).Inc()
	logging.FromContext(ctx).Info("order rejected",
		"code", rej.Code, "reason", rej.Message, "symbol", req.InstrumentID, "source", req.Source)

	res := Result{Rejection: &rej, Decision: decision}
	if rejected.ID != uuid.Nil {
		res.Order = &rejected
	}
	if decisionID != uuid.Nil {
		res.DecisionID = &decisionID
	}
	return res, nil
}

// applyAck folds the venue's response into Vantage's state.
func (s *Service) applyAck(ctx context.Context, req PlaceOrderRequest, account domain.Account,
	g gatheredContext, order domain.Order, ack broker.OrderAck, decisionID uuid.UUID, now time.Time) (Result, error) {

	result := Result{DecisionID: &decisionID}

	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		current, err := s.store.Trading.OrderByIDTx(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		if ack.BrokerOrderID != "" {
			if err := s.store.Trading.SetBrokerOrderIDTx(ctx, tx, current.ID, ack.BrokerOrderID); err != nil {
				return err
			}
			current, err = s.store.Trading.OrderByIDTx(ctx, tx, order.ID)
			if err != nil {
				return err
			}
		}

		submitted, err := s.store.Trading.TransitionOrderTx(ctx, tx, current.ID, current.Version,
			domain.OrderSubmitted, "submitted to "+account.BrokerName, "system", &req.ActorUserID)
		if err != nil {
			return err
		}
		current = submitted

		for _, exec := range ack.Fills {
			fill, applied, err := s.applyFillTx(ctx, tx, account, g.instrument, current, exec, now)
			if err != nil {
				return err
			}
			if applied {
				result.Fills = append(result.Fills, fill)
			}
			current, err = s.store.Trading.OrderByIDTx(ctx, tx, current.ID)
			if err != nil {
				return err
			}
		}

		// Map the venue's status into Vantage's state machine. An unmappable
		// status is not guessed at: the order is left submitted and
		// reconciliation resolves it.
		if target, ok := ack.Status.ToOrderStatus(); ok && target != current.Status {
			if domain.CanTransition(current.Status, target) {
				current, err = s.store.Trading.TransitionOrderTx(ctx, tx, current.ID, current.Version,
					target, "venue reported "+string(ack.Status), "system", &req.ActorUserID)
				if err != nil {
					return err
				}
			}
		}
		result.Order = &current

		if err := s.store.Trading.CompleteCommand(ctx, tx, account.ID, req.IdempotencyKey,
			store.CommandSucceeded, &current.ID, map[string]any{
				"order_id": current.ID, "status": current.Status,
				"filled_quantity": current.FilledQuantity.String(),
			}); err != nil {
			return err
		}

		if err := s.store.Trading.EnqueueOutboxTx(ctx, tx, "order", current.ID.String(),
			"order.submitted", map[string]any{
				"order_id": current.ID, "account_id": account.ID,
				"symbol": g.instrument.Symbol, "status": current.Status,
			}, req.CorrelationID); err != nil {
			return err
		}

		return s.audit(ctx, tx, req, account, domain.AuditOrderSubmitted, domain.AuditSuccess,
			"order", current.ID.String(), map[string]any{
				"broker_order_id": ack.BrokerOrderID, "status": current.Status,
				"symbol": g.instrument.Symbol, "quantity": current.Quantity.String(),
			})
	})
	if err != nil {
		return Result{}, err
	}

	// Refresh the snapshot after the fills so the caller sees post-trade state.
	snap, err := s.portfolio.Compute(ctx, account)
	if err == nil {
		result.Snapshot = &snap
		_ = s.portfolio.RecordEquityPoint(ctx, snap)
	}
	return result, nil
}

// applyFillTx books one execution: the fill, the position and the ledger.
//
// Returns applied=false when the venue replayed a fill Vantage already holds,
// which is normal after a reconnect and must not double-count.
func (s *Service) applyFillTx(ctx context.Context, tx pgx.Tx, account domain.Account,
	inst domain.Instrument, order domain.Order, exec broker.ExecutionReport, now time.Time) (domain.Fill, bool, error) {

	fill := domain.Fill{
		OrderID:       order.ID,
		AccountID:     account.ID,
		InstrumentID:  inst.ID,
		Side:          exec.Side,
		Quantity:      exec.Quantity,
		Price:         exec.Price,
		Commission:    exec.Commission,
		CommissionCcy: exec.CommissionCcy,
		BrokerFillID:  exec.BrokerFillID,
		BrokerName:    account.BrokerName,
		Liquidity:     exec.Liquidity,
		ExecutedAt:    exec.ExecutedAt,
	}

	stored, err := s.store.Trading.AppendFillTx(ctx, tx, fill, s.mode)
	if errors.Is(err, store.ErrDuplicateCommand) {
		return domain.Fill{}, false, nil
	}
	if err != nil {
		return domain.Fill{}, false, err
	}

	// Position: load, apply, persist.
	position, perr := s.store.Trading.OpenPositionTx(ctx, tx, account.ID, inst.ID)
	if errors.Is(perr, store.ErrNotFound) {
		position = domain.Position{
			AccountID:    account.ID,
			InstrumentID: inst.ID,
			Mode:         s.mode,
			Quantity:     decimal.Zero,
			Status:       domain.PositionClosed,
			RealizedPnL:  money.Zero(inst.QuoteCcy),
			Commission:   money.Zero(inst.QuoteCcy),
			Swap:         money.Zero(inst.QuoteCcy),
			StrategyID:   order.StrategyID,
		}
	} else if perr != nil {
		return domain.Fill{}, false, perr
	}

	applied := domain.ApplyFill(position, inst, stored)
	updated := applied.Position
	updated.Mode = s.mode
	updated.RealizedPnL = position.RealizedPnL.MustAdd(applied.RealizedQuote)
	updated.Commission = position.Commission.MustAdd(money.New(stored.Commission, inst.QuoteCcy))
	if updated.StrategyID == nil {
		updated.StrategyID = order.StrategyID
	}

	savedPosition, err := s.store.Trading.UpsertPositionTx(ctx, tx, updated)
	if err != nil {
		return domain.Fill{}, false, err
	}

	// Ledger: realised P&L and commission, converted into the account's
	// currency. A conversion failure aborts the whole transaction rather than
	// booking a number whose currency nobody can name.
	balance, err := s.store.Accounts.BalanceTx(ctx, tx, account.ID, account.Currency)
	if err != nil {
		return domain.Fill{}, false, err
	}

	if !applied.RealizedQuote.IsZero() {
		conv, err := s.converter.Convert(ctx, applied.RealizedQuote, account.Currency)
		if err != nil {
			return domain.Fill{}, false, fmt.Errorf(
				"oms: cannot book realised P&L: no %s/%s rate: %w",
				applied.RealizedQuote.Currency(), account.Currency, err)
		}
		amount := conv.To.RoundLedger()
		balance = balance.MustAdd(amount)
		if _, err := s.store.Accounts.AppendTransactionTx(ctx, tx, domain.Transaction{
			AccountID: account.ID, Type: domain.TxRealizedPnL, Amount: amount,
			BalanceAfter: balance.RoundLedger(), OrderID: &order.ID, FillID: &stored.ID,
			PositionID: &savedPosition.ID, Mode: s.mode,
			Description: fmt.Sprintf("Realised P&L on %s", inst.Symbol),
		}); err != nil {
			return domain.Fill{}, false, err
		}
	}

	if stored.Commission.IsPositive() {
		commissionQuote := money.New(stored.Commission.Neg(), money.Currency(stored.CommissionCcy))
		conv, err := s.converter.Convert(ctx, commissionQuote, account.Currency)
		if err != nil {
			return domain.Fill{}, false, fmt.Errorf("oms: cannot book commission: %w", err)
		}
		amount := conv.To.RoundLedger()
		balance = balance.MustAdd(amount)
		if _, err := s.store.Accounts.AppendTransactionTx(ctx, tx, domain.Transaction{
			AccountID: account.ID, Type: domain.TxCommission, Amount: amount,
			BalanceAfter: balance.RoundLedger(), OrderID: &order.ID, FillID: &stored.ID,
			Mode: s.mode, Description: fmt.Sprintf("Commission on %s", inst.Symbol),
		}); err != nil {
			return domain.Fill{}, false, err
		}
	}

	if err := s.store.Trading.EnqueueOutboxTx(ctx, tx, "fill", stored.ID.String(), "order.filled",
		map[string]any{
			"order_id": order.ID, "account_id": account.ID, "symbol": inst.Symbol,
			"quantity": stored.Quantity.String(), "price": stored.Price.String(),
		}, ""); err != nil {
		return domain.Fill{}, false, err
	}

	metrics.FillsRecorded.WithLabelValues(inst.Symbol, string(stored.Side)).Inc()
	return stored, true, nil
}

// handleBrokerError decides what a failed broker call means.
//
// The critical distinction: a definitive rejection is a closed outcome, while
// an unknown outcome means the order MAY be live at the venue. The second case
// never retries and never assumes; it marks the order FAILED and leaves it for
// reconciliation, because retrying an order that did reach the market doubles
// the position.
func (s *Service) handleBrokerError(ctx context.Context, req PlaceOrderRequest, account domain.Account,
	order domain.Order, brokerErr error, now time.Time) (Result, error) {

	log := logging.FromContext(ctx)

	var rejErr broker.RejectionError
	switch {
	case errors.As(brokerErr, &rejErr):
		rej := domain.NewRejection(domain.RejectBrokerRejected, rejErr.Reason, "venue_code", rejErr.Code)
		return s.rejectAcceptedOrder(ctx, req, account, order, rej)

	case errors.Is(brokerErr, broker.ErrOrderRejected):
		rej := domain.NewRejection(domain.RejectBrokerRejected, brokerErr.Error())
		return s.rejectAcceptedOrder(ctx, req, account, order, rej)

	case errors.Is(brokerErr, broker.ErrInsufficientMargin):
		rej := domain.NewRejection(domain.RejectInsufficientMargin, brokerErr.Error())
		return s.rejectAcceptedOrder(ctx, req, account, order, rej)

	case errors.Is(brokerErr, broker.ErrMarketClosed):
		rej := domain.NewRejection(domain.RejectMarketClosed, brokerErr.Error())
		return s.rejectAcceptedOrder(ctx, req, account, order, rej)

	case errors.Is(brokerErr, broker.ErrVenueUnavailable), errors.Is(brokerErr, broker.ErrRateLimited):
		// Definitely not delivered: safe to report as a clean refusal.
		rej := domain.NewRejection(domain.RejectBrokerUnavailable, brokerErr.Error())
		return s.rejectAcceptedOrder(ctx, req, account, order, rej)

	default:
		log.Error("broker call outcome unknown; marking order for reconciliation",
			"order_id", order.ID, "error", brokerErr.Error())
		return s.failOrder(ctx, req, account, order,
			domain.NewRejection(domain.RejectBrokerUnavailable,
				"The broker's response was lost. This order's true state is unknown and will be reconciled."),
			now)
	}
}

// rejectAcceptedOrder closes out an order the venue definitively refused.
func (s *Service) rejectAcceptedOrder(ctx context.Context, req PlaceOrderRequest, account domain.Account,
	order domain.Order, rej domain.Rejection) (Result, error) {

	var rejected domain.Order
	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		current, err := s.store.Trading.OrderByIDTx(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		rejected, err = s.store.Trading.RejectOrderTx(ctx, tx, current.ID, current.Version, rej,
			"system", &req.ActorUserID)
		if err != nil {
			return err
		}
		if err := s.store.Trading.CompleteCommand(ctx, tx, account.ID, req.IdempotencyKey,
			store.CommandRejected, &rejected.ID,
			map[string]any{"code": rej.Code, "message": rej.Message}); err != nil {
			return err
		}
		return s.auditWithResult(ctx, tx, req, account, domain.AuditOrderRejected, domain.AuditFailure,
			"order", rejected.ID.String(), map[string]any{"code": rej.Code, "reason": rej.Message})
	})
	if err != nil {
		return Result{}, err
	}
	metrics.OrdersRejected.WithLabelValues(req.InstrumentID, string(rej.Code), string(req.Source)).Inc()
	return Result{Order: &rejected, Rejection: &rej}, nil
}

// failOrder marks an order's outcome unknown and hands it to reconciliation.
func (s *Service) failOrder(ctx context.Context, req PlaceOrderRequest, account domain.Account,
	order domain.Order, rej domain.Rejection, now time.Time) (Result, error) {

	var failed domain.Order
	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		current, err := s.store.Trading.OrderByIDTx(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		failed, err = s.store.Trading.TransitionOrderTx(ctx, tx, current.ID, current.Version,
			domain.OrderFailed, rej.Message, "system", &req.ActorUserID)
		if err != nil {
			return err
		}
		if err := s.store.Trading.CompleteCommand(ctx, tx, account.ID, req.IdempotencyKey,
			store.CommandFailed, &failed.ID,
			map[string]any{"code": rej.Code, "message": rej.Message}); err != nil {
			return err
		}
		if err := s.store.Control.RecordRiskEvent(ctx, tx, domain.RiskEvent{
			AccountID: &account.ID, UserID: &req.ActorUserID, Severity: domain.SeverityCritical,
			Check: "broker_outcome_unknown", Code: rej.Code, Message: rej.Message,
			InstrumentID: &req.InstrumentID, OrderID: &failed.ID,
		}); err != nil {
			return err
		}
		if err := s.store.Control.CreateNotification(ctx, tx, store.Notification{
			UserID: req.ActorUserID, AccountID: &account.ID, Severity: "critical",
			Category: "reconciliation", Title: "Order outcome unknown",
			Body: fmt.Sprintf(
				"An order on %s did not return a confirmed result. Automated trading on this account is paused until reconciliation resolves it.",
				req.InstrumentID),
		}); err != nil {
			return err
		}
		return s.auditWithResult(ctx, tx, req, account, domain.AuditOrderRejected, domain.AuditFailure,
			"order", failed.ID.String(), map[string]any{"code": rej.Code, "reason": rej.Message})
	})
	if err != nil {
		return Result{}, err
	}
	metrics.OrdersFailed.WithLabelValues(req.InstrumentID).Inc()
	return Result{Order: &failed, Rejection: &rej}, nil
}

// resultFromStoredCommand returns the outcome of a command already processed.
func (s *Service) resultFromStoredCommand(ctx context.Context, req PlaceOrderRequest, cmd *store.RegisteredCommand) (Result, error) {
	res := Result{Duplicate: true}
	if cmd.ResultOrderID != nil {
		order, err := s.store.Trading.OrderForUser(ctx, req.ActorUserID, *cmd.ResultOrderID)
		if err == nil {
			res.Order = &order
			fills, ferr := s.store.Trading.FillsForOrder(ctx, order.ID)
			if ferr == nil {
				res.Fills = fills
			}
		}
	}
	if cmd.Status == store.CommandRejected && len(cmd.ResultPayload) > 0 {
		var payload struct {
			Code    domain.RejectCode `json:"code"`
			Message string            `json:"message"`
		}
		if err := json.Unmarshal(cmd.ResultPayload, &payload); err == nil {
			rej := domain.NewRejection(payload.Code, payload.Message)
			res.Rejection = &rej
		}
	}
	metrics.DuplicateCommandsSuppressed.WithLabelValues("place_order").Inc()
	logging.FromContext(ctx).Info("duplicate order suppressed",
		"idempotency_key", req.IdempotencyKey, "original_status", cmd.Status)
	return res, nil
}

// buildDecisionSnapshot captures every input the decision depended on.
func (s *Service) buildDecisionSnapshot(req PlaceOrderRequest, account domain.Account,
	g gatheredContext, decision domain.RiskDecision, outcome, code, reason string) domain.DecisionSnapshot {

	marshal := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			return json.RawMessage(`{}`)
		}
		return b
	}

	riskState := map[string]any{"checks": decision.Checks, "approved": decision.Approved}
	if decision.FirstFailure != nil {
		riskState["first_failure"] = decision.FirstFailure
	}

	// Authority is recorded as its SCOPE, never as anything sensitive: what
	// was permitted at decision time, not who holds which credential.
	authorityState := map[string]any{
		"authority_id":        g.authority.ID,
		"active":              g.authority.Active,
		"automation_enabled":  g.authority.AutomationEnabled,
		"allowed_instruments": g.authority.AllowedInstruments,
		"max_order_quantity":  g.authority.MaxOrderQuantity.String(),
		"valid_until":         g.authority.ValidUntil,
	}

	portfolioState := map[string]any{
		"equity":         g.snapshot.State.Equity.String(),
		"balance":        g.snapshot.State.Balance.String(),
		"free_margin":    g.snapshot.State.FreeMargin.String(),
		"gross_exposure": g.snapshot.State.GrossExposure.String(),
		"net_exposure":   g.snapshot.State.NetExposure.String(),
		"open_positions": g.snapshot.State.OpenPositions,
		"day_pnl":        g.snapshot.State.DayPnL().String(),
		"drawdown":       g.snapshot.State.DrawdownFraction().String(),
	}

	eventContext := map[string]any{"blackout": g.blackout}
	if g.blackout {
		eventContext["event"] = g.eventName
		eventContext["scheduled_at"] = g.eventAt
	}

	var confidence decimal.Decimal
	snap := domain.DecisionSnapshot{
		AccountID:       account.ID,
		StrategyID:      req.StrategyID,
		StrategyVersion: req.StrategyVersion,
		InstrumentID:    req.InstrumentID,
		Quote: marshal(map[string]any{
			"bid": g.quote.Bid.String(), "ask": g.quote.Ask.String(),
			"source_time": g.quote.SourceTime, "ingested_at": g.quote.IngestedAt,
			"provider": g.quote.Provider,
		}),
		EventContext:     marshal(eventContext),
		PortfolioContext: marshal(portfolioState),
		RiskState:        marshal(riskState),
		AuthorityState:   marshal(authorityState),
		MarketDataHealth: marshal(map[string]any{
			"state": g.health.State, "issues": g.health.Issues,
			"quote_age_ms":  g.health.QuoteAge.Milliseconds(),
			"market_status": g.market,
		}),
		SignalAction:  signalActionFor(req.Side),
		Confidence:    confidence,
		RequestedQty:  req.Quantity,
		ApprovedQty:   decision.ApprovedQuantity,
		Outcome:       outcome,
		OutcomeCode:   code,
		OutcomeReason: reason,
	}
	return snap
}

func signalActionFor(side domain.OrderSide) domain.SignalAction {
	if side == domain.SideBuy {
		return domain.SignalBuy
	}
	return domain.SignalSell
}

func rejectionFromDecision(d domain.RiskDecision) domain.Rejection {
	if d.FirstFailure == nil {
		return domain.NewRejection(domain.RejectRiskLimit, "The order did not pass risk checks.")
	}
	f := d.FirstFailure
	return domain.NewRejection(f.Code, f.Message,
		"check", string(f.Name), "limit", f.Limit, "observed", f.Observed)
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, req PlaceOrderRequest, account domain.Account,
	action domain.AuditAction, result domain.AuditResult, targetType, targetID string, meta map[string]any) error {
	return s.auditWithResult(ctx, tx, req, account, action, result, targetType, targetID, meta)
}

func (s *Service) auditWithResult(ctx context.Context, tx pgx.Tx, req PlaceOrderRequest, account domain.Account,
	action domain.AuditAction, result domain.AuditResult, targetType, targetID string, meta map[string]any) error {

	raw, err := json.Marshal(meta)
	if err != nil {
		raw = []byte(`{}`)
	}
	actorType := "user"
	if req.Source == domain.SourceStrategy || req.Source == domain.SourceAutopilot {
		actorType = "strategy"
	}
	var target *string
	if targetID != "" {
		target = &targetID
	}
	_, err = s.store.Control.AppendAudit(ctx, tx, domain.AuditEvent{
		ActorUserID:   &req.ActorUserID,
		ActorType:     actorType,
		Action:        action,
		TargetType:    targetType,
		TargetID:      target,
		AccountID:     &account.ID,
		Result:        result,
		RequestID:     req.RequestID,
		CorrelationID: req.CorrelationID,
		IPAddress:     req.IPAddress,
		UserAgent:     req.UserAgent,
		Metadata:      raw,
	})
	return err
}

func orderIDString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// HashRequest fingerprints an order payload for idempotency comparison.
//
// The hash covers every field that changes what the order DOES. It excludes
// the idempotency key itself and transport metadata, so replaying the same
// order over a different connection is recognised as the same command.
func HashRequest(accountID uuid.UUID, instrumentID string, side domain.OrderSide, orderType domain.OrderType,
	qty decimal.Decimal, limitPrice, stopPrice, stopLoss, takeProfit *decimal.Decimal, tif domain.TimeInForce) string {

	str := func(d *decimal.Decimal) string {
		if d == nil {
			return "-"
		}
		return d.String()
	}
	payload := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s|%s",
		accountID, instrumentID, side, orderType, qty.String(),
		str(limitPrice), str(stopPrice), str(stopLoss), str(takeProfit), tif)
	return crypto.SHA256Hex([]byte(payload))
}
