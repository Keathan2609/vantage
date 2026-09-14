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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/booking"
	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/crypto"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
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
	booking     *booking.Service
	converter   *fx.Converter
	clock       domain.Clock
	marketClock *domain.MarketClock
	// regimePolicy is the versioned threshold set for regime inference.
	regimePolicy domain.RegimePolicy
	// correlationRiskPolicy is the versioned threshold set for the
	// correlation check.
	correlationRiskPolicy domain.CorrelationRiskPolicy
	// correlations is the most recent measured matrix, or nil.
	//
	// Held rather than computed per order: measuring it needs a query over
	// bars for every instrument, and doing that inside the order path would
	// put a multi-instrument scan between a signal and a fill. Refreshed on a
	// schedule, and nil until the first refresh -- which the risk check
	// records as a gap rather than as an absence of correlation.
	correlations *domain.CorrelationMatrix
	// regimeTrackers hold the hysteresis state, one per instrument.
	//
	// Per instrument because a spread blowing out on gold says nothing about
	// EURUSD, and a single shared tracker would let one instrument's
	// conditions suppress trading on another. Guarded by a mutex because the
	// scheduler and a manual order can arrive at once.
	regimeMu       sync.Mutex
	regimeTrackers map[string]*domain.RegimeTracker

	// replayRun reports the market replay that currently owns the clock, or
	// nil. A function rather than an engine reference: the OMS must not import
	// the replay package, and it has no business knowing that replay exists
	// beyond "this order was not decided on a live feed".
	replayRun func() *uuid.UUID
	mode      domain.ExecutionMode
	alerter   Alerter
	halt      HaltGate
}

// Alerter is the subset of internal/notify the OMS needs, declared here so
// oms does not import notify.
type Alerter interface {
	OrderOutcomeUnknown(ctx context.Context, accountID uuid.UUID, orderID, symbol, cause string)
	BrokerFailure(ctx context.Context, brokerName, operation string, unknownOutcome bool, cause string)
	DailyLossThreshold(ctx context.Context, accountID uuid.UUID, loss, limit, currency string, breached bool)
}

// SetAlerter attaches an alerter after construction.
func (s *Service) SetAlerter(a Alerter) { s.alerter = a }

// HaltGate answers "may automation trade on this account right now", inside
// the caller's transaction.
//
// Declared here rather than imported so oms does not depend on reconcile: the
// OMS must not be able to reach reconciliation's repair machinery, only to ask
// it one question.
type HaltGate interface {
	AutomationBlockedTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID) (bool, string, error)
}

// SetHaltGate attaches the reconciliation halt check.
//
// Optional at construction so the OMS can be built in a test without a
// reconciliation service. A nil gate means no halt is enforced, which is
// correct for a unit test and would be wrong in the running system -- so
// internal/app wires it unconditionally and an architecture test asserts it.
func (s *Service) SetHaltGate(g HaltGate) { s.halt = g }

// errAutomationHalted unwinds Phase B when reconciliation has halted the
// account. A sentinel rather than a rejection because it must roll the
// transaction back: the order was never claimed, so its idempotency key stays
// available for a retry after the halt clears.
var errAutomationHalted = errors.New("oms: automated trading is halted for this account")

// errAutopilotOff unwinds Phase B when the global Autopilot switch is off.
//
// Separate from errAutomationHalted so the refusal an operator sees names the
// actual cause. "Reconciliation required" when the real reason is "you turned
// autopilot off" would send someone looking for a divergence that does not
// exist.
var errAutopilotOff = errors.New("oms: the global Autopilot switch is off")

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
	bk *booking.Service,
	converter *fx.Converter,
	clock domain.Clock,
	marketClock *domain.MarketClock,
	mode domain.ExecutionMode,
) (*Service, error) {
	if mode != domain.ModePaper {
		return nil, fmt.Errorf("oms: refusing to construct in %s mode: this build is paper-only", mode)
	}
	if bk == nil {
		return nil, errors.New("oms: a booking service is required: the OMS does not book " +
			"executions itself, so that reconciliation and ordinary execution provably " +
			"share one accounting path")
	}
	return &Service{
		store: s, brokers: brokers, riskEngine: riskEngine, portfolio: pf, booking: bk,
		converter: converter, clock: clock, marketClock: marketClock, mode: mode,
		regimePolicy:          domain.DefaultRegimePolicy(),
		regimeTrackers:        map[string]*domain.RegimeTracker{},
		correlationRiskPolicy: domain.DefaultCorrelationRiskPolicy(),
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

	// ReportedRegime is the research plane's classification of the market's
	// SHAPE, from the bars it was sent. Empty for a manual order, which is
	// correct: a human clicking buy has no strategy evaluation behind them
	// and the platform should not invent one.
	//
	// It is one input to the regime actually recorded -- the OMS's own
	// evidence (feed health, spread, event proximity, drawdown) can override
	// it with RISK_OFF or EVENT_RISK, because those are conditions the
	// research plane cannot see.
	ReportedRegime domain.Regime
	// BarsAvailable is how much history the classification rested on. Below
	// domain.MinBarsForRegime the verdict is UNKNOWN rather than a guess, and
	// the OMS cannot count bars itself without a query per decision.
	BarsAvailable int
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
		// A daily-loss refusal is the one risk outcome an operator must be
		// told about rather than discover in a list: it means no further
		// orders will be accepted today.
		if s.alerter != nil {
			for _, check := range decision.Checks {
				if check.Name == domain.CheckDailyLoss && !check.Passed {
					s.alerter.DailyLossThreshold(ctx, account.ID,
						check.Observed, check.Limit, string(account.Currency), true)
					break
				}
			}
		}
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
	var haltReason string

	err = s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		// The account row lock, taken first, as in every transaction that
		// writes an account's financial state. See store.LockAccountTx.
		//
		// It also serialises this transaction against a reconciliation repair,
		// which is what makes the halt check below durable rather than
		// timing-dependent.
		if err := s.store.Accounts.LockAccountTx(ctx, tx, account.ID); err != nil {
			return err
		}

		// The reconciliation halt, re-checked INSIDE the transaction.
		//
		// Checking it in Phase A before opening a transaction leaves a real
		// window: an automated order can pass the check microseconds before a
		// reconciliation repair writes a critical issue, and still commit
		// afterwards. The window is small, and "small" is not a property to
		// rely on when the consequence is an automated order placed against a
		// position book known to be wrong.
		//
		// Reading it here, after the account lock that the repair also takes,
		// closes the window with PostgreSQL's own serialisation: either this
		// order commits before the issue exists, or it sees the issue.
		//
		// Manual orders are deliberately NOT gated. An operator can see the
		// warning and decide; an algorithm cannot.
		if req.Source != domain.SourceManual && req.Source != domain.SourceRiskControl {
			// The global Autopilot switch, read in the same transaction and
			// for the same reason.
			//
			// It is a SEPARATE control from the reconciliation halt and from
			// the kill switch: the halt is per-account and automatic, the kill
			// switch stops every order including an operator's, and this stops
			// only the autonomous pipeline. An operator turning autopilot off
			// to take over by hand must not have to disable a protection to do
			// it.
			//
			// Checked here rather than only in the scheduler because the
			// scheduler is not the only caller. A strategy evaluation
			// triggered through the API would otherwise bypass the switch
			// entirely, which is exactly the "bypass OMS" case this milestone
			// forbids.
			autopilot, aerr := s.store.Autopilot.AutopilotEnabledTx(ctx, tx)
			if aerr != nil {
				return aerr
			}
			if !autopilot {
				haltReason = "the global Autopilot switch is off"
				return errAutopilotOff
			}

			if s.halt != nil {
				blocked, why, herr := s.halt.AutomationBlockedTx(ctx, tx, account.ID)
				if herr != nil {
					return herr
				}
				if blocked {
					haltReason = why
					return errAutomationHalted
				}
			}
		}

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
			ReplayRunID:     s.currentReplayRun(),
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
	if errors.Is(err, errAutopilotOff) {
		// Refused, not failed, and the idempotency key stays free: the same
		// request is legitimate once autopilot is switched back on.
		rej := domain.NewRejection(domain.RejectAutopilotOff,
			"Autonomous trading is switched off. This order came from an automated "+
				"source and was refused. Manual trading is unaffected.")
		metrics.OrdersRejected.WithLabelValues(
			req.InstrumentID, string(domain.RejectAutopilotOff), string(req.Source)).Inc()
		logging.FromContext(ctx).Info("automated order refused: autopilot is off",
			"account_id", account.ID.String(), "source", string(req.Source))
		return Result{Rejection: &rej}, nil
	}
	if errors.Is(err, errAutomationHalted) {
		// Refused, not failed. The order was never persisted and its
		// idempotency key is still free, so the caller may retry once the
		// account is safe -- which is the correct shape for a temporary
		// refusal and the reason this is not recorded as a rejected order.
		rej := domain.NewRejection(domain.RejectReconciliationRequired,
			"Automated trading is halted for this account: "+haltReason+
				". Manual trading is unaffected. Resolve the reconciliation issue to resume.")
		metrics.OrdersRejected.WithLabelValues(
			req.InstrumentID, string(domain.RejectReconciliationRequired), string(req.Source)).Inc()
		logging.FromContext(ctx).Warn("automated order refused: reconciliation halt",
			"account_id", account.ID.String(), "source", string(req.Source), "reason", haltReason)
		return Result{Rejection: &rej}, nil
	}
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
	// regime is the market's shape as acted on, after hysteresis.
	regime domain.RegimeAssessment
	// sessions are the liquidity sessions live at decision time. Recorded
	// because P&L attributed to a session has to use the session that was
	// live THEN: recomputing it later from the order's timestamp would judge
	// an old trade against a calendar that has since been amended.
	sessions []domain.SessionName
}

// SetReplayRunSource installs the function that reports the market replay
// currently owning the clock.
//
// Nil, and a nil result, are both legitimate and mean the same thing: this
// order was decided on a live feed. That is the ordinary case, so the absence
// of the tag must not need configuring.
func (s *Service) SetReplayRunSource(fn func() *uuid.UUID) {
	s.replayRun = fn
}

// currentReplayRun tags an order with the replay that produced it.
//
// Read at order-creation time rather than passed in by the caller: a manual
// order placed while a replay is engaged was ALSO decided against dataset
// prices, and asking each caller to remember that is how half of them forget.
func (s *Service) currentReplayRun() *uuid.UUID {
	if s.replayRun == nil {
		return nil
	}
	return s.replayRun()
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
	g.sessions = s.marketClock.ActiveSessions(now)

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

		// Correlation. The book is every OTHER open instrument's exposure:
		// the same instrument is governed by the per-instrument ceiling, and
		// applying both rules to one fact would refuse every scale-in.
		Correlations:      s.correlationMatrix(),
		CorrelatedBook:    correlatedBook(snapshot, req.InstrumentID),
		CorrelationPolicy: s.correlationRiskPolicy,
	}

	// Last, because the regime is a statement about all of the above together:
	// the feed, the event window and the account's own drawdown. Inferring it
	// from any one of them alone would be a weaker and different claim.
	g.regime = s.regimeFor(req, g)

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
				ReplayRunID: s.currentReplayRun(),
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

	apply := func(tx pgx.Tx) error {
		// Reset on a retry: a rolled-back attempt may have appended fills to
		// the result before it failed, and replaying would double-count them.
		result = Result{DecisionID: &decisionID}
		return s.applyAckTx(ctx, tx, req, account, g, order, ack, &result, now)
	}

	// A deadlock rolls the whole transaction back, so replaying it is safe --
	// the venue's answer is still in `ack`, in memory. One retry is attempted
	// because the lock order (see store.LockAccountTx) should make a deadlock
	// impossible; a second failure means something is wrong that retrying will
	// not fix.
	err := s.store.Pool().InTx(ctx, apply)
	if errors.Is(err, store.ErrDeadlock) {
		logging.FromContext(ctx).Warn(
			"deadlock while persisting an accepted order; retrying once",
			"order_id", order.ID, "error", err.Error())
		metrics.OrderPersistDeadlocks.Inc()
		err = s.store.Pool().InTx(ctx, apply)
	}
	if err != nil {
		// THE DANGEROUS CASE. The venue accepted this order and we could not
		// write down what happened. The outcome is therefore UNKNOWN, exactly
		// as it is when the broker's response is lost -- and it must be
		// recorded that way.
		//
		// Returning the raw error here (which is what this code did until a
		// concurrency test produced six HTTP 500s from one burst of orders)
		// leaves the order in its pre-submission state while the venue holds a
		// live order, with nothing marking it for reconciliation and nothing
		// blocking automation. That is the one state this system is built to
		// never be in.
		logging.FromContext(ctx).Error(
			"could not persist an order the venue accepted; marking the outcome unknown",
			"order_id", order.ID, "broker_order_id", ack.BrokerOrderID, "error", err.Error())
		return s.failOrder(ctx, req, account, order, domain.NewRejection(
			domain.RejectInternalError,
			"The venue accepted this order but Vantage could not record the result. "+
				"Its true state is unknown and will be resolved by reconciliation."),
			now)
	}

	// Refresh the snapshot after the fills so the caller sees post-trade state.
	snap, err := s.portfolio.Compute(ctx, account)
	if err == nil {
		result.Snapshot = &snap
		_ = s.portfolio.RecordEquityPoint(ctx, snap)
	}
	return result, nil
}

// applyAckTx is the body of Phase D: everything the venue's answer changes,
// written in one transaction.
func (s *Service) applyAckTx(ctx context.Context, tx pgx.Tx, req PlaceOrderRequest,
	account domain.Account, g gatheredContext, order domain.Order, ack broker.OrderAck,
	result *Result, now time.Time) error {
	{
		// The account's row lock is taken FIRST, before the order, the
		// position, the fill or the ledger. See store.LockAccountTx for the
		// deadlock this prevents: without it, the FK on `fills` takes an
		// implicit KEY SHARE on this row early and the explicit FOR UPDATE
		// arrives late, with the positions lock in between — which is a cycle
		// that Postgres resolves by killing one of the transactions.
		if err := s.store.Accounts.LockAccountTx(ctx, tx, account.ID); err != nil {
			return err
		}

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
	}
}

// applyFillTx books one execution through the shared accounting path.
//
// The accounting itself lives in internal/booking, which reconciliation also
// uses when it imports an execution discovered after a lost response. That is
// not a refactor for tidiness: an architecture test asserts booking is the
// only package that appends a fill, which is what makes "an imported fill goes
// through the same invariants as a live one" a structural guarantee rather
// than a claim.
//
// Returns applied=false when the venue replayed a fill Vantage already holds,
// which is normal after a reconnect and must not double-count.
func (s *Service) applyFillTx(ctx context.Context, tx pgx.Tx, account domain.Account,
	inst domain.Instrument, order domain.Order, exec broker.ExecutionReport, now time.Time) (domain.Fill, bool, error) {

	result, err := s.booking.Apply(ctx, tx, booking.Request{
		Account:    account,
		Instrument: inst,
		Order:      order,
		Execution:  exec,
		Source:     booking.SourceExecutionResponse,
		Now:        now,
	})
	if err != nil {
		return domain.Fill{}, false, err
	}
	return result.Fill, result.Applied, nil
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

	decision := ClassifyBrokerError(brokerErr)

	if decision.OutcomeKnown {
		return s.rejectAcceptedOrder(ctx, req, account, order, decision.Rejection)
	}

	logging.FromContext(ctx).Error(
		"broker call outcome unknown; marking order for reconciliation",
		"order_id", order.ID, "error", brokerErr.Error())
	return s.failOrder(ctx, req, account, order, decision.Rejection, now)
}

// BrokerErrorDecision is what a failed broker call means.
//
// OutcomeKnown is the only field that matters for safety. When it is true the
// order definitively did not reach the market and can be closed out, freeing
// its risk budget. When it is false the order MAY be live at the venue, and
// closing it out would release budget for a position that exists.
type BrokerErrorDecision struct {
	// OutcomeKnown is true only where the venue's answer was definitive.
	OutcomeKnown bool
	Rejection    domain.Rejection
}

// ClassifyBrokerError decides what a failed broker call means.
//
// # Why this is a separate pure function
//
// It is the branch table on which "never retry an unknown outcome" rests, and
// getting one case wrong means an order that reached the market is recorded as
// refused — after which its risk budget is released and the position it opened
// is invisible. The previous audit noted that three of these six branches were
// reached by no test at all.
//
// Splitting the decision from the write makes every branch reachable without a
// database, a venue, or an order to write to. The default case is the important
// one: anything not explicitly recognised as definitive is UNKNOWN. A new
// broker error added to the adapter package therefore fails closed, which is
// the opposite of what an `errors.Is` chain ending in "assume rejected" would
// do.
func ClassifyBrokerError(brokerErr error) BrokerErrorDecision {
	var rejErr broker.RejectionError
	switch {
	case errors.As(brokerErr, &rejErr):
		// The venue named a reason. The most definitive answer available.
		return BrokerErrorDecision{
			OutcomeKnown: true,
			Rejection: domain.NewRejection(domain.RejectBrokerRejected, rejErr.Reason,
				"venue_code", rejErr.Code),
		}

	case errors.Is(brokerErr, broker.ErrOrderRejected):
		return BrokerErrorDecision{
			OutcomeKnown: true,
			Rejection:    domain.NewRejection(domain.RejectBrokerRejected, brokerErr.Error()),
		}

	case errors.Is(brokerErr, broker.ErrInsufficientMargin):
		return BrokerErrorDecision{
			OutcomeKnown: true,
			Rejection:    domain.NewRejection(domain.RejectInsufficientMargin, brokerErr.Error()),
		}

	case errors.Is(brokerErr, broker.ErrMarketClosed):
		return BrokerErrorDecision{
			OutcomeKnown: true,
			Rejection:    domain.NewRejection(domain.RejectMarketClosed, brokerErr.Error()),
		}

	case errors.Is(brokerErr, broker.ErrVenueUnavailable), errors.Is(brokerErr, broker.ErrRateLimited):
		// The request was definitely not delivered: the adapter could not
		// reach the venue, or the venue refused it before looking at it. Safe
		// to report as a clean refusal.
		return BrokerErrorDecision{
			OutcomeKnown: true,
			Rejection:    domain.NewRejection(domain.RejectBrokerUnavailable, brokerErr.Error()),
		}

	default:
		// Includes ErrUnknownOutcome, a timeout, a lost response, and anything
		// this switch has never heard of. FAILED means "Vantage does not know",
		// and reconciliation resolves it against venue state. It is never
		// retried: retrying an order that did reach the market doubles the
		// position.
		return BrokerErrorDecision{
			OutcomeKnown: false,
			Rejection: domain.NewRejection(domain.RejectBrokerUnavailable,
				"The broker's response was lost. This order's true state is unknown "+
					"and will be reconciled."),
		}
	}
}

// rejectAcceptedOrder closes out an order the venue definitively refused.
func (s *Service) rejectAcceptedOrder(ctx context.Context, req PlaceOrderRequest, account domain.Account,
	order domain.Order, rej domain.Rejection) (Result, error) {

	var rejected domain.Order
	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		// Same declared lock order as Phase D: the account row first.
		if err := s.store.Accounts.LockAccountTx(ctx, tx, account.ID); err != nil {
			return err
		}
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
		// Same declared lock order as Phase D: the account row first.
		if err := s.store.Accounts.LockAccountTx(ctx, tx, account.ID); err != nil {
			return err
		}
		current, err := s.store.Trading.OrderByIDTx(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		failed, err = s.store.Trading.TransitionOrderTx(ctx, tx, current.ID, current.Version,
			domain.OrderFailed, rej.Message, "system", &req.ActorUserID)
		if err != nil {
			return err
		}

		// Mark the uncertainty explicitly.
		//
		// FAILED already means "the venue-side outcome is unknown", but the
		// name reads as a closed failure, and an operator needs to see
		// uncertainty AS uncertainty. The flag is what makes it queryable, so
		// the order appears in the operations view, contributes to the
		// readiness verdict, and is picked up by the next reconciliation run
		// even though the state machine no longer considers it open.
		//
		// Set in the same transaction as the transition. Setting it afterwards
		// would leave a window in which an order's outcome is unknown and
		// nothing says so.
		if err := s.store.Trading.SetOrderReconciliationRequiredTx(
			ctx, tx, current.ID, true); err != nil {
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
	if s.alerter != nil {
		// Raised outside the transaction: an alerting failure must not roll
		// back the record that the order failed.
		s.alerter.OrderOutcomeUnknown(ctx, account.ID, failed.ID.String(),
			req.InstrumentID, rej.Message)
		s.alerter.BrokerFailure(ctx, account.BrokerName, "place_order", true, rej.Message)
	}
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

// SetCorrelationMatrix installs a freshly measured matrix.
//
// Called by the scheduler on an interval and by a replay step, so the
// measurement moves with the market rather than being computed once at boot.
// Guarded by the same mutex as the regime trackers: both are decision-time
// state that a scheduler tick and a manual order can reach at once.
func (s *Service) SetCorrelationMatrix(m *domain.CorrelationMatrix) {
	s.regimeMu.Lock()
	defer s.regimeMu.Unlock()
	s.correlations = m
}

// correlationMatrix reads the current matrix, or nil.
func (s *Service) correlationMatrix() *domain.CorrelationMatrix {
	s.regimeMu.Lock()
	defer s.regimeMu.Unlock()
	return s.correlations
}

// CorrelationPolicy exposes the thresholds for a reader that wants to explain
// a verdict without re-deriving them.
func (s *Service) CorrelationPolicy() domain.CorrelationRiskPolicy {
	return s.correlationRiskPolicy
}

// correlatedBook is every OTHER instrument's open exposure.
//
// Excludes the proposed instrument: adding to an existing gold position is a
// per-instrument exposure question, and counting it here would apply two
// different rules to one fact and refuse every scale-in.
func correlatedBook(snapshot portfolio.Snapshot, proposed string) []domain.CorrelatedExposure {
	out := make([]domain.CorrelatedExposure, 0, len(snapshot.Positions))
	for _, p := range snapshot.Positions {
		if p.Position.InstrumentID == proposed {
			continue
		}
		// An UNVALUED position is skipped rather than counted as zero.
		//
		// Zero would understate exposure at exactly the wrong moment -- a
		// position the platform cannot price is not a position it can rule
		// out as a correlation risk. Skipping it means the check does not
		// fire, which is a gap; counting it as zero would mean the check
		// fires and reports safety, which is a false statement.
		if !p.Valued {
			continue
		}
		out = append(out, domain.CorrelatedExposure{
			InstrumentID: p.Position.InstrumentID,
			Exposure:     p.NotionalValue.Decimal().Abs(),
			Side:         p.Position.Side,
		})
	}
	return out
}

// regimeFor infers the market regime and applies hysteresis.
//
// Called at the END of gather, once the feed, the event window and the
// portfolio snapshot are all in hand: the regime is a statement about all of
// them together, and inferring it from any one alone would be a different and
// weaker claim.
//
// The tracker is stateful, so this is where flapping is suppressed. Without
// it a spread oscillating either side of a threshold would switch a strategy
// set on and off on consecutive bars, and the platform would trade the
// threshold rather than the market.
func (s *Service) regimeFor(req PlaceOrderRequest, g gatheredContext) domain.RegimeAssessment {
	evidence := domain.RegimeEvidence{
		Reported: req.ReportedRegime,
		Health:   g.health,
		// The account's configured acceptable spread stands in for a measured
		// normal. See RegimeEvidence.SpreadBaseline for why that is weaker
		// than it sounds and what would fix it.
		SpreadBaseline:   g.limits.MaxSpreadFraction,
		Blackout:         g.blackout,
		EventName:        g.eventName,
		DrawdownFraction: g.snapshot.State.DrawdownFraction(),
		// Not yet measured. Recorded as zero rather than guessed, and the
		// classifier reports the check as passing on zero -- which is honest:
		// nothing has observed instability, as opposed to observing stability.
		ProviderReconnects: 0,
		HasQuote:           !g.quote.SourceTime.IsZero(),
		BarsAvailable:      req.BarsAvailable,
	}

	assessment := domain.InferRegime(evidence, s.regimePolicy)

	s.regimeMu.Lock()
	tracker, ok := s.regimeTrackers[req.InstrumentID]
	if !ok {
		tracker = domain.NewRegimeTracker(s.regimePolicy)
		s.regimeTrackers[req.InstrumentID] = tracker
	}
	held := tracker.Observe(assessment)
	s.regimeMu.Unlock()

	return held
}

// ClearCorrelationMatrix drops the measured matrix.
//
// Called when a replay starts. A matrix measured over the SEEDED history would
// otherwise be the one in force for the replay's first instants, so a run's
// risk decisions would depend on what happened to be in the database rather
// than on its own declared data.
func (s *Service) ClearCorrelationMatrix() {
	s.regimeMu.Lock()
	defer s.regimeMu.Unlock()
	s.correlations = nil
}

// ResetRegimeTrackers clears the hysteresis memory for every instrument.
//
// Called when a replay rewinds. A tracker carrying confirmations from a
// previous pass would make the first bars of the next one depend on which run
// preceded them, and a replay whose result depends on history is not
// reproducible -- which is the entire claim a replay makes.
func (s *Service) ResetRegimeTrackers() {
	s.regimeMu.Lock()
	defer s.regimeMu.Unlock()
	s.regimeTrackers = map[string]*domain.RegimeTracker{}
}

// CurrentRegime reports the regime in force for an instrument, for a reader
// that wants it without placing an order.
func (s *Service) CurrentRegime(instrumentID string) domain.Regime {
	s.regimeMu.Lock()
	defer s.regimeMu.Unlock()
	if tracker, ok := s.regimeTrackers[instrumentID]; ok {
		return tracker.Current()
	}
	return domain.RegimeUnknown
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
		// The market's state at decision time, which is where market_status
		// already lived. `session` is the single label attribution groups by
		// and `sessions` is the full set it was reduced from, so the
		// collapsing is visible rather than assumed.
		MarketDataHealth: marshal(map[string]any{
			"state": g.health.State, "issues": g.health.Issues,
			"quote_age_ms":  g.health.QuoteAge.Milliseconds(),
			"market_status": g.market,
			"session":       domain.PrimarySession(g.sessions),
			"sessions":      g.sessions,
		}),
		// The regime AS ACTED ON, after hysteresis, with the checks behind it.
		// Recorded rather than recomputed later: moved thresholds would
		// otherwise reattribute historical P&L to regimes the platform never
		// acted in.
		Regime:              g.regime.Regime,
		RegimePolicyVersion: g.regime.PolicyVersion,
		RegimeReasons:       marshal(g.regime.Reasons),

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
