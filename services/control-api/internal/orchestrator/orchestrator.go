// Package orchestrator turns strategy signals into order intents.
//
// It is the ONLY component that sits between research and execution, and it is
// deliberately dull. It does not decide whether a trade is a good idea — the
// strategy already offered an opinion and the risk engine will decide what is
// permissible. Its job is to:
//
//  1. refuse to run at all when conditions forbid it (data, reconciliation,
//     market hours, kill switches, authority);
//  2. ask the research service what it thinks;
//  3. size the result against the account's OWN risk budget, never the size
//     the strategy suggested;
//  4. hand the intent to the order pipeline, which applies every gate again.
//
// NO TRADE is a first-class outcome here. Most evaluations end in one, and each
// records why, so "the system did nothing today" is an answerable question
// rather than a worry.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/oms"
	"github.com/vantage/control-api/internal/portfolio"
	"github.com/vantage/control-api/internal/quant"
	"github.com/vantage/control-api/internal/reconcile"
	"github.com/vantage/control-api/internal/risk"
	"github.com/vantage/control-api/internal/store"
)

// Service evaluates strategies and routes their signals.
type Service struct {
	store       *store.Store
	quant       *quant.Client
	oms         *oms.Service
	portfolio   *portfolio.Service
	converter   *fx.Converter
	reconciler  *reconcile.Service
	clock       domain.Clock
	marketClock *domain.MarketClock
}

// New builds the orchestrator.
func New(s *store.Store, q *quant.Client, o *oms.Service, pf *portfolio.Service,
	converter *fx.Converter, rec *reconcile.Service, clock domain.Clock,
	marketClock *domain.MarketClock) *Service {
	return &Service{
		store: s, quant: q, oms: o, portfolio: pf, converter: converter,
		reconciler: rec, clock: clock, marketClock: marketClock,
	}
}

// RunRequest asks for one strategy evaluation.
type RunRequest struct {
	Account      domain.Account
	StrategyID   uuid.UUID
	InstrumentID string
	Version      int
	// Execute routes an actionable signal into the order pipeline. When false
	// the evaluation is a dry run: the signal is recorded, nothing is placed.
	Execute     bool
	ActorUserID uuid.UUID
	ActorRole   domain.Role
	RequestID   string
}

// Outcome is the result of an evaluation.
type Outcome struct {
	Status      domain.StrategyRunStatus
	Action      domain.SignalAction
	Confidence  decimal.Decimal
	Explanation string
	SkipReason  string
	Executed    bool
	Order       *domain.Order
	Rejection   *domain.Rejection
	Indicators  map[string]string
	SignalID    *uuid.UUID
	RunID       uuid.UUID
}

// EvaluateAndRoute runs one strategy and optionally routes its signal.
func (s *Service) EvaluateAndRoute(ctx context.Context, req RunRequest) (Outcome, error) {
	log := logging.FromContext(ctx)
	now := s.clock.Now()
	start := time.Now()

	strategy, err := s.store.Research.Strategy(ctx, req.StrategyID)
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: load strategy: %w", err)
	}

	version := req.Version
	var strategyVersion domain.StrategyVersion
	if version == 0 {
		strategyVersion, err = s.store.Research.LatestStrategyVersion(ctx, req.StrategyID)
	} else {
		strategyVersion, err = s.store.Research.StrategyVersion(ctx, req.StrategyID, version)
	}
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: load strategy version: %w", err)
	}
	version = strategyVersion.Version

	skip := func(reason string) (Outcome, error) {
		out := Outcome{Status: domain.RunSkipped, Action: domain.SignalNoTrade, SkipReason: reason}
		runID, rerr := s.recordRun(ctx, req, version, domain.RunSkipped, reason, "", start, nil)
		if rerr != nil {
			return out, rerr
		}
		// A nil id means the run was already recorded for this bar; the
		// refusal still stands, it simply needs no second row.
		out.RunID = runID
		metrics.StrategyRuns.WithLabelValues(strategy.Key, string(domain.RunSkipped)).Inc()
		log.Info("strategy run skipped",
			"strategy", strategy.Key, "instrument", req.InstrumentID, "reason", reason)
		return out, nil
	}

	// ---- Pre-flight refusals ----------------------------------------------
	// Each of these is a reason NOT to consult the strategy at all. Asking a
	// strategy for an opinion on stale data and then discarding it wastes work
	// and muddies the record of why nothing happened.

	if !strategy.Enabled {
		return skip("strategy is disabled")
	}
	if strategyVersion.Lifecycle != domain.LifecyclePaper {
		return skip(fmt.Sprintf(
			"strategy version is %s; only PAPER versions may generate live signals",
			strategyVersion.Lifecycle))
	}
	if !s.marketClock.Status(now).Tradable() {
		return skip(fmt.Sprintf("market is %s", s.marketClock.Status(now)))
	}

	// A position book known to disagree with the venue makes sizing guesswork.
	blocked, critical, err := s.reconciler.AutomationBlocked(ctx, req.Account.ID)
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: reconciliation check: %w", err)
	}
	if blocked {
		return skip(fmt.Sprintf(
			"%d unresolved reconciliation discrepancies: automated trading is paused until they are resolved",
			len(critical)))
	}

	instrument, err := s.store.Market.Instrument(ctx, req.InstrumentID)
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: load instrument: %w", err)
	}
	if !instrument.Enabled {
		return skip("instrument is not enabled for trading")
	}

	quote, prev, err := s.store.Market.LatestQuote(ctx, req.InstrumentID)
	if err != nil {
		return skip("no market data available for this instrument")
	}
	health := domain.EvaluateQuoteHealth(quote, prev, domain.DefaultDataQualityPolicy(), now)
	if !health.Healthy() {
		// Fail closed. This is the single most important refusal in the file:
		// automated trading on a feed the platform does not trust is how a
		// system trades a price that never existed.
		return skip(fmt.Sprintf("market data is %s (%v)", health.State, health.Issues))
	}

	authority, err := s.store.Control.ActiveAuthorityForAccount(ctx, req.Account.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return skip("no active trading authority for this account")
		}
		return Outcome{}, fmt.Errorf("orchestrator: load authority: %w", err)
	}
	if ok, rej := authority.Effective(now); !ok {
		return skip(rej.Message)
	}
	if !authority.AutomationEnabled {
		return skip("automation is disabled on this account's trading authority")
	}
	if !authority.PermitsInstrument(req.InstrumentID) {
		return skip("trading authority does not cover " + req.InstrumentID)
	}
	if !authority.PermitsStrategy(&req.StrategyID) {
		return skip("trading authority does not permit this strategy")
	}

	ksState, err := s.store.Control.KillSwitchesFor(ctx, req.Account.UserID, req.Account.ID,
		req.Account.BrokerName, &req.StrategyID)
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: kill switches: %w", err)
	}
	if halted, ks := ksState.Blocked(); halted {
		return skip(fmt.Sprintf("%s kill switch active: %s", ks.Scope, ks.Reason))
	}

	// ---- Ask the research service -----------------------------------------
	bars, err := s.store.Market.Bars(ctx, req.InstrumentID, strategyVersion.Timeframe, 300, true)
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: load bars: %w", err)
	}
	if len(bars) < 50 {
		return skip(fmt.Sprintf("only %d complete bars available; the strategy needs more history", len(bars)))
	}

	sessions := s.marketClock.ActiveSessions(now)
	sessionName := "none"
	if len(sessions) > 0 {
		sessionName = string(sessions[0])
	}
	limits, err := s.store.Control.RiskLimitsForAccount(ctx, req.Account.ID)
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: load risk limits: %w", err)
	}
	eventRisk := "none"
	events, err := s.store.Research.UpcomingHighImpactEvents(ctx, req.InstrumentID,
		now.Add(-time.Duration(limits.EventBlackoutAfterMinutes)*time.Minute),
		now.Add(time.Duration(limits.EventBlackoutBeforeMinutes)*time.Minute))
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: event risk: %w", err)
	}
	if len(events) > 0 {
		eventRisk = "high"
	}

	barInputs := make([]quant.BarInput, 0, len(bars))
	for _, b := range bars {
		barInputs = append(barInputs, quant.BarInput{
			OpenTime: b.OpenTime.UTC().Format(time.RFC3339),
			Open:     b.Open.String(), High: b.High.String(),
			Low: b.Low.String(), Close: b.Close.String(), Volume: b.Volume.String(),
		})
	}

	signal, err := s.quant.Signal(ctx, quant.SignalRequest{
		StrategyKey: strategy.Key, Version: version,
		InstrumentID: req.InstrumentID, Timeframe: string(strategyVersion.Timeframe),
		Parameters: strategyVersion.Parameters, Bars: barInputs,
		// Deliberately narrow: session, spread and event risk. No balances, no
		// positions, no identifiers. Research does not need them.
		Context: map[string]any{
			"session":         sessionName,
			"spread_fraction": quote.SpreadFraction().String(),
			"event_risk":      eventRisk,
		},
	})
	if err != nil {
		if _, rerr := s.recordRun(ctx, req, version, domain.RunFailed, "", err.Error(), start, nil); rerr != nil {
			log.Error("could not record failed run", "error", rerr.Error())
		}
		metrics.StrategyRuns.WithLabelValues(strategy.Key, string(domain.RunFailed)).Inc()
		return Outcome{}, err
	}

	metrics.StrategyRunDuration.WithLabelValues(strategy.Key).Observe(time.Since(start).Seconds())
	action := domain.SignalAction(signal.Action)
	metrics.SignalsGenerated.WithLabelValues(strategy.Key, string(action)).Inc()

	barTime := bars[len(bars)-1].OpenTime
	runStatus := domain.RunSucceeded
	if !action.Actionable() {
		runStatus = domain.RunNoSignal
	}

	runID, err := s.recordRun(ctx, req, version, runStatus, "", "", start, &barTime)
	if err != nil {
		return Outcome{}, err
	}
	if runID == uuid.Nil {
		// This bar has already been evaluated — usually by the scheduler, a
		// moment before a manual run asked for the same thing. The per-bar
		// guard did its job; recording a second signal for one bar would
		// double-count the strategy's opinion.
		log.Info("bar already evaluated",
			"strategy", strategy.Key, "instrument", req.InstrumentID,
			"bar_time", barTime.Format(time.RFC3339))
		return Outcome{
			Status: domain.RunSkipped,
			Action: domain.SignalNoTrade,
			SkipReason: fmt.Sprintf(
				"this bar (%s) has already been evaluated for %s; a strategy is "+
					"evaluated once per completed bar",
				barTime.UTC().Format(time.RFC3339), strategy.Key),
		}, nil
	}
	metrics.StrategyRuns.WithLabelValues(strategy.Key, string(runStatus)).Inc()

	outcome := Outcome{
		Status: runStatus, Action: action, Confidence: signal.Confidence,
		Explanation: signal.Explanation, Indicators: signal.Indicators, RunID: runID,
	}

	// Record the signal even when it is not actionable: "the strategy looked
	// and decided not to trade" is information worth keeping.
	features := signal.Features
	if features == nil {
		features = json.RawMessage(`{}`)
	}
	err = s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		id, serr := s.store.Research.CreateSignalTx(ctx, tx, domain.StrategySignal{
			RunID: runID, StrategyID: req.StrategyID, StrategyVersion: version,
			AccountID: &req.Account.ID, InstrumentID: req.InstrumentID,
			Timeframe: strategyVersion.Timeframe, Action: action,
			Confidence: signal.Confidence, SuggestedStop: signal.SuggestedStop,
			SuggestedTarget: signal.SuggestedTarget, Explanation: signal.Explanation,
			Features: features, BarTime: barTime,
		})
		if serr != nil {
			return serr
		}
		outcome.SignalID = &id
		return nil
	})
	if err != nil {
		return outcome, fmt.Errorf("orchestrator: record signal: %w", err)
	}

	if !action.Actionable() {
		log.Info("strategy produced no actionable signal",
			"strategy", strategy.Key, "instrument", req.InstrumentID,
			"action", string(action), "explanation", signal.Explanation)
		return outcome, nil
	}
	if !req.Execute {
		return outcome, nil
	}

	// ---- Size against the account's OWN budget ----------------------------
	side, _ := action.Side()
	sized, rejection, err := s.size(ctx, req.Account, instrument, quote, limits, side, signal)
	if err != nil {
		return outcome, err
	}
	if rejection != nil {
		outcome.Rejection = rejection
		outcome.Status = domain.RunNoSignal
		log.Info("signal not sizeable",
			"strategy", strategy.Key, "instrument", req.InstrumentID, "reason", rejection.Message)
		return outcome, nil
	}

	// ---- Hand to the order pipeline ---------------------------------------
	// The idempotency key is derived from the strategy, version, instrument and
	// BAR TIME. Re-running the same bar therefore produces the same key and is
	// suppressed as a duplicate, which is what stops a scheduler restart from
	// double-trading a signal.
	idempotencyKey := fmt.Sprintf("strat-%s-v%d-%s-%s",
		req.StrategyID.String()[:8], version, req.InstrumentID, barTime.UTC().Format("20060102T150405Z"))

	result, err := s.oms.PlaceOrder(ctx, oms.PlaceOrderRequest{
		IdempotencyKey:  idempotencyKey,
		ActorUserID:     req.ActorUserID,
		ActorRole:       req.ActorRole,
		AccountID:       req.Account.ID,
		InstrumentID:    req.InstrumentID,
		Side:            side,
		Type:            domain.OrderTypeMarket,
		Quantity:        sized.Quantity,
		StopLoss:        sized.StopLoss,
		TakeProfit:      sized.TakeProfit,
		TimeInForce:     domain.TIFGoodTilCancelled,
		Source:          domain.SourceStrategy,
		StrategyID:      &req.StrategyID,
		StrategyVersion: &version,
		RequestHash: oms.HashRequest(req.Account.ID, req.InstrumentID, side,
			domain.OrderTypeMarket, sized.Quantity, nil, nil, sized.StopLoss, sized.TakeProfit,
			domain.TIFGoodTilCancelled),
		RequestID:     req.RequestID,
		CorrelationID: runID.String(),
	})
	if err != nil {
		// An identical bar already produced this order: the idempotency key
		// caught a re-run. That is the guard working, not a failure.
		if errors.Is(err, oms.ErrCommandInFlight) {
			return outcome, nil
		}
		return outcome, fmt.Errorf("orchestrator: place order: %w", err)
	}

	outcome.Order = result.Order
	outcome.Rejection = result.Rejection
	outcome.Executed = result.Accepted()
	return outcome, nil
}

// sizedOrder is a signal converted into an order the account can afford.
type sizedOrder struct {
	Quantity   decimal.Decimal
	StopLoss   *decimal.Decimal
	TakeProfit *decimal.Decimal
}

// size converts a signal into a quantity.
//
// The strategy's suggested size, if it offered one, is ignored entirely. Size
// is derived from the ACCOUNT's risk budget and the distance to the stop, so a
// strategy cannot take a larger position by asking for one.
func (s *Service) size(ctx context.Context, account domain.Account, instrument domain.Instrument,
	quote domain.Quote, limits domain.RiskLimits, side domain.OrderSide,
	signal quant.SignalResponse) (sizedOrder, *domain.Rejection, error) {

	entry := quote.ExecutionPrice(side)

	stop := signal.SuggestedStop
	if stop == nil {
		if limits.RequireStopLoss {
			rej := domain.NewRejection(domain.RejectRiskLimit,
				"This account requires a stop loss, and the strategy did not supply one.")
			return sizedOrder{}, &rej, nil
		}
		return sizedOrder{}, nil, nil
	}

	// A stop on the wrong side of the market is a strategy bug. Refuse rather
	// than "correcting" it into something the strategy did not intend.
	if side == domain.SideBuy && stop.GreaterThanOrEqual(entry) {
		rej := domain.NewRejection(domain.RejectPriceInvalid,
			fmt.Sprintf("The strategy suggested a stop at %s for a buy at %s, which is not below entry.",
				stop, entry))
		return sizedOrder{}, &rej, nil
	}
	if side == domain.SideSell && stop.LessThanOrEqual(entry) {
		rej := domain.NewRejection(domain.RejectPriceInvalid,
			fmt.Sprintf("The strategy suggested a stop at %s for a sell at %s, which is not above entry.",
				stop, entry))
		return sizedOrder{}, &rej, nil
	}

	snapshot, err := s.portfolio.Compute(ctx, account)
	if err != nil {
		return sizedOrder{}, nil, fmt.Errorf("orchestrator: portfolio snapshot: %w", err)
	}

	result, err := risk.Size(ctx, risk.SizingRequest{
		Policy:       risk.SizingPercentRisk,
		Instrument:   instrument,
		Account:      account,
		Equity:       snapshot.State.Equity,
		Side:         side,
		EntryPrice:   entry,
		StopPrice:    stop,
		RiskFraction: limits.MaxRiskPerTradeFraction,
		ToAccountCurrency: func(ctx context.Context, amount money.Amount) (money.Amount, error) {
			conv, cerr := s.converter.Convert(ctx, amount, account.Currency)
			if cerr != nil {
				return money.Amount{}, cerr
			}
			return conv.To, nil
		},
	})
	if err != nil {
		return sizedOrder{}, nil, fmt.Errorf("orchestrator: sizing: %w", err)
	}
	if !result.Feasible {
		// For a small account this is the common, correct outcome.
		rej := domain.NewRejection(domain.RejectQuantityInvalid, result.Reason,
			"policy", string(result.Policy), "risk_budget", result.RiskAmount.String())
		return sizedOrder{}, &rej, nil
	}

	rounded := instrument.Spec.RoundPrice(*stop)
	out := sizedOrder{Quantity: result.Quantity, StopLoss: &rounded}
	if signal.SuggestedTarget != nil {
		target := instrument.Spec.RoundPrice(*signal.SuggestedTarget)
		out.TakeProfit = &target
	}
	return out, nil, nil
}

// recordRun persists the evaluation record.
func (s *Service) recordRun(ctx context.Context, req RunRequest, version int,
	status domain.StrategyRunStatus, skipReason, errMsg string, start time.Time,
	barTime *time.Time) (uuid.UUID, error) {

	run := domain.StrategyRun{
		StrategyID: req.StrategyID, StrategyVersion: version, AccountID: &req.Account.ID,
		InstrumentID: req.InstrumentID, Status: status, SkipReason: skipReason,
		Error: errMsg, DurationMS: time.Since(start).Milliseconds(),
	}
	if v, err := s.store.Research.StrategyVersion(ctx, req.StrategyID, version); err == nil {
		run.Timeframe = v.Timeframe
	} else {
		run.Timeframe = domain.TF1h
	}

	var runID uuid.UUID
	err := s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		var rerr error
		if barTime != nil {
			runID, rerr = s.store.Research.CreateStrategyRunForBarTx(ctx, tx, run, *barTime)
		} else {
			runID, rerr = s.store.Research.CreateStrategyRunTx(ctx, tx, run)
		}
		return rerr
	})
	if errors.Is(err, store.ErrDuplicateCommand) {
		// This bar was already evaluated. Not an error: the guard worked.
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("orchestrator: record run: %w", err)
	}
	return runID, nil
}
