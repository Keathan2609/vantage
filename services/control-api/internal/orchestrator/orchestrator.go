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
	// replayWindowFn reports the active replay's historical context, or an
	// inactive window. See SetReplayWindow.
	replayWindowFn func() domain.ReplayWindow
}

// SetReplayWindow installs the source of the active replay's historical
// context.
//
// A function rather than an engine reference: the orchestrator must not import
// the replay package, and it has no business knowing that replay exists beyond
// "this run may only see data from here onwards, and may not trade yet".
func (s *Service) SetReplayWindow(fn func() domain.ReplayWindow) {
	s.replayWindowFn = fn
}

// replayWindow reads the active window, or an inactive one.
func (s *Service) replayWindow() domain.ReplayWindow {
	if s.replayWindowFn == nil {
		return domain.NoReplayWindow()
	}
	return s.replayWindowFn()
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

	// alreadyEvaluated reports that this bar had a run row before this call,
	// so the strategy's opinion for it exists but was recorded elsewhere.
	//
	// Distinct from an ordinary skip. A strategy that declined has no opinion
	// and the aggregate can proceed without it; a strategy whose opinion is
	// INVISIBLE makes the aggregate a decision over a partial set, which is
	// not the decision the policy describes.
	alreadyEvaluated bool

	// routing is everything an order would need, kept so that a caller which
	// evaluated SEVERAL strategies can aggregate first and place once.
	//
	// Unexported deliberately. It is for EvaluateInstrument, in this package;
	// the HTTP layer builds its response field by field and must never start
	// serialising this by accident.
	routing routingContext
}

// routingContext is the decision's inputs, carried past the evaluation so an
// order can be placed from an AGGREGATE of several evaluations rather than
// from whichever one happened to run.
//
// `valid` is false whenever no fresh opinion was produced -- a skip, a failed
// evaluation, or a bar this strategy had already been evaluated on. That
// distinction matters: an aggregate built from a partial set of opinions is
// not the decision the policy describes, and the caller refuses rather than
// deciding on whoever answered.
type routingContext struct {
	valid bool

	strategy      domain.Strategy
	version       int
	validRegimes  []string
	instrument    domain.Instrument
	quote         domain.Quote
	limits        domain.RiskLimits
	signal        quant.SignalResponse
	barTime       time.Time
	barsAvailable int
	eventRisk     string
	eventName     string
	eventAt       time.Time
	// sessions is the venue sessions active at the decision instant. Carried
	// so a no-trade snapshot records the same market context an order's
	// snapshot does; session attribution that only covered traded instants
	// would describe the instants that traded, not the day.
	sessions []domain.SessionName
	health   domain.MarketDataHealth
	// scoreKind is what this strategy's confidence value IS -- a raw score or
	// a calibrated probability. Carried so the verdict can record which kind
	// the policy's thresholds were compared against.
	scoreKind string
}

// historyRefusal reports whether a research answer is a refusal to answer for
// want of history, rather than an opinion, and the reason to record.
//
// The two arrive identically -- action "no_trade" at confidence 0 -- and for a
// whole milestone the second was written down as the first. The orchestrator
// loads up to 300 bars and refuses below 50 of its own; the seeded PAPER
// strategies declare 100 and 120. Between those numbers it performed a
// complete evaluation whose outcome was fixed before it started, stored it in
// strategy_runs as `no_signal`, and handed it to the consensus as a fresh
// abstention -- halving its family's weight and making the decision look as
// though five strategies had considered the market.
//
// Measured over the replay fixtures with the standard 60-instant warm-up:
// 280 of 405 recorded signals on `trend_clean` (69%) and 280 of 305 on
// `drawdown` (92%) could not have been anything else. Read back in aggregate
// that said "no strategy fires on a clean trend". Given the history they ask
// for, the same five produce 49 actionable signals on `trend_clean` at a mean
// score of 0.561.
//
// The requirement is deliberately NOT duplicated here. It belongs to the
// strategy, the research plane reports it on every answer, and a second copy
// on this side would be a number that could drift from the code it describes.
func historyRefusal(signal quant.SignalResponse, sent int) (bool, string) {
	if !signal.InsufficientHistory {
		return false, ""
	}
	return true, fmt.Sprintf(
		"insufficient history: the strategy requires %d completed bars and was "+
			"sent %d, so it could not form an opinion. Recorded as a skip rather "+
			"than as a no-trade, because an opinion nobody formed must not enter "+
			"the consensus",
		signal.RequiredBars, sent)
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

	// skipAtBar records a refusal against a specific bar, so the per-bar unique
	// index deduplicates it.
	//
	// The index is PARTIAL -- `WHERE bar_time IS NOT NULL` -- so a refusal
	// recorded with a nil bar time is inserted afresh on every tick. The
	// scheduler evaluates every 30 seconds and an hourly bar lasts 120 ticks,
	// so a condition that persists across a bar writes 120 identical rows
	// instead of one. Nothing prunes `strategy_runs`, and
	// `PurgeStrategyRunsInRange` only deletes rows that HAVE a bar time, so a
	// replay reset cannot clear them either.
	//
	// A refusal that knows which bar it refused should therefore say so. The
	// pre-flight refusals below genuinely do not know -- they happen before the
	// bars are loaded -- and keep the nil.
	skipAtBar := func(reason string, barTime *time.Time) (Outcome, error) {
		out := Outcome{Status: domain.RunSkipped, Action: domain.SignalNoTrade, SkipReason: reason}
		runID, rerr := s.recordRun(ctx, req, version, domain.RunSkipped, reason, "", start, barTime)
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

	skip := func(reason string) (Outcome, error) {
		return skipAtBar(reason, nil)
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
	//
	// Floored at the replay's warm-up start when one is active. Without the
	// floor a replay's decisions are taken over a window that mixes SEEDED
	// history with dataset bars: the indicators, the regime and the features
	// then partly describe the seed, and the result depends on what happened
	// to be in the database rather than on the dataset. That is not
	// reproducible research.
	//
	// Zero outside a replay, which is no floor at all.
	window := s.replayWindow()
	bars, err := s.store.Market.BarsFrom(ctx, req.InstrumentID, strategyVersion.Timeframe,
		300, true, window.Floor())
	if err != nil {
		return Outcome{}, fmt.Errorf("orchestrator: load bars: %w", err)
	}
	if len(bars) < 50 {
		// Keyed on the bar when there is one. This condition persists for as
		// long as the series is short, and an unkeyed skip writes a fresh row
		// on every 30-second tick because the per-bar unique index is partial
		// on `bar_time IS NOT NULL`. With no bars at all there is nothing to
		// key on and the nil is the honest answer.
		return skipAtNewestBar(skip, skipAtBar, bars, fmt.Sprintf(
			"only %d complete bars available; the strategy needs more history", len(bars)))
	}

	// ---- Warm-up ----------------------------------------------------------
	//
	// State is built -- bars ingested, indicators warmed, correlation measured,
	// the regime established -- and no executable intent is produced. A replay
	// that traded its first bar would be trading indicator noise: ADX means
	// nothing for fourteen periods and the volatility baseline averages fifty.
	//
	// Recorded as a SKIP with its reason rather than dropped silently, so a
	// run's warm-up is visible in strategy_runs and cannot be mistaken for a
	// strategy that found nothing.
	if window.InWarmup(now) {
		// Keyed on the bar: a warm-up lasts sixty instants and the scheduler
		// ticks twice a minute throughout, so an unkeyed skip records this
		// reason hundreds of times for the same sixty bars.
		warmupBar := bars[len(bars)-1].OpenTime
		return skipAtBar(fmt.Sprintf(
			"replay warm-up: evaluation begins at %s and this instant is %s; "+
				"indicators, regime and correlation are being built and no "+
				"executable intent is permitted yet",
			window.EvaluationStart.UTC().Format(time.RFC3339),
			now.UTC().Format(time.RFC3339)), &warmupBar)
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
	eventName := ""
	var eventAt time.Time
	if len(events) > 0 {
		eventRisk = "high"
		eventName = events[0].EventName
		eventAt = events[0].ScheduledAt
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
		// Keyed on the bar. The quant circuit breaker opens for 30 seconds and
		// the scheduler ticks every 30 seconds, so a research service that is
		// down records a failure per strategy per instrument indefinitely.
		failedAt := bars[len(bars)-1].OpenTime
		if _, rerr := s.recordRun(ctx, req, version, domain.RunFailed, "", err.Error(), start, &failedAt); rerr != nil {
			log.Error("could not record failed run", "error", rerr.Error())
		}
		metrics.StrategyRuns.WithLabelValues(strategy.Key, string(domain.RunFailed)).Inc()
		return Outcome{}, err
	}

	metrics.StrategyRunDuration.WithLabelValues(strategy.Key).Observe(time.Since(start).Seconds())

	// A strategy that was not given the history it requires has not formed an
	// opinion, and must not be recorded as having formed one.
	//
	// This is checked BEFORE the signal counter and before `valid` is set, so
	// such an answer never becomes a fresh opinion in `opinionsFrom` and never
	// reaches the policy. See historyRefusal for what it cost.
	// Keyed on the bar, because this refusal knows which bar it refused and
	// the condition persists for every tick of it. See skipAtBar.
	if refused, reason := historyRefusal(signal, len(bars)); refused {
		refusedAt := bars[len(bars)-1].OpenTime
		return skipAtBar(reason, &refusedAt)
	}

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
			Status:           domain.RunSkipped,
			Action:           domain.SignalNoTrade,
			alreadyEvaluated: true,
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
		// Everything an order would need, whether or not this evaluation
		// routes one itself. A fresh opinion exists at this point: the run row
		// was written for THIS bar, so the per-bar guard let it through.
		routing: routingContext{
			valid:         true,
			strategy:      strategy,
			version:       version,
			validRegimes:  strategyVersion.ValidRegimes,
			instrument:    instrument,
			quote:         quote,
			limits:        limits,
			signal:        signal,
			barTime:       barTime,
			barsAvailable: len(bars),
			eventRisk:     eventRisk,
			eventName:     eventName,
			eventAt:       eventAt,
			sessions:      sessions,
			health:        health,
			scoreKind:     signal.ScoreKind(),
		},
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

	side, _ := action.Side()
	order, rejection, executed, err := s.place(ctx, placement{
		Account:       req.Account,
		StrategyID:    req.StrategyID,
		ActorUserID:   req.ActorUserID,
		ActorRole:     req.ActorRole,
		RequestID:     req.RequestID,
		CorrelationID: runID.String(),
		rc:            outcome.routing,
		side:          side,
		regime:        signal.MarketRegime(),
	})
	if err != nil {
		return outcome, err
	}
	if rejection != nil && order == nil {
		// Not sizeable. A refusal before the pipeline, recorded the same way.
		outcome.Rejection = rejection
		outcome.Status = domain.RunNoSignal
		log.Info("signal not sizeable",
			"strategy", strategy.Key, "instrument", req.InstrumentID, "reason", rejection.Message)
		return outcome, nil
	}

	outcome.Order = order
	outcome.Rejection = rejection
	outcome.Executed = executed
	return outcome, nil
}

// placement is one order to place, derived either from a single strategy's
// signal or from an aggregate verdict over several.
type placement struct {
	Account       domain.Account
	StrategyID    uuid.UUID
	ActorUserID   uuid.UUID
	ActorRole     domain.Role
	RequestID     string
	CorrelationID string

	// rc is the decision's inputs, from the evaluation that produced the
	// leading opinion. Its signal supplies the stop and target.
	rc     routingContext
	side   domain.OrderSide
	regime domain.Regime

	// maxQuantity caps the sized quantity. Zero means uncapped, and it only
	// ever LOWERS the size -- risk may only reduce, so an aggregate cannot
	// ask for more than the account's own budget allows.
	//
	// Set when the order is permitted only because it reduces an open
	// position: capping at the position's size is what keeps it strictly
	// reducing rather than a side flip that opens fresh exposure.
	maxQuantity decimal.Decimal

	// consensus is the serialised aggregate verdict, or nil when one strategy
	// routed its own signal.
	consensus json.RawMessage
	// idempotencyKey overrides the single-strategy form. Empty uses that form,
	// which is the key existing rows were written under.
	//
	// The aggregated path MUST supply one, and it must not name a strategy --
	// see consensusIdempotencyKey.
	idempotencyKey string
}

// consensusIdempotencyKey is the durable one-decision-per-bar guard.
//
// `command_idempotency` is keyed on (account_id, idempotency_key), so a key
// that is identical for one (account, instrument, bar) makes a second
// orchestrated order for that bar a duplicate the database refuses. That is
// the protection; an in-memory flag would not survive the restart this
// platform is built to survive.
//
// It deliberately does NOT name a strategy, and that is the whole point. The
// single-strategy key is `strat-<strategy>-v<version>-<instrument>-<bar>`,
// which is correct when one strategy routes its own signal and WRONG for an
// aggregate: the strategy an order is attributed to is the strongest counted
// opinion on the routed side, and that can differ between two evaluations of
// the same bar -- a confidence that moved, an opinion the policy discarded the
// second time, or the reducing rescue firing and attributing to a different
// strategy entirely. Each of those would mint a different key and let a second
// order through for a bar that already has one.
func consensusIdempotencyKey(instrumentID string, barTime time.Time) string {
	return fmt.Sprintf("consensus-%s-%s", instrumentID,
		barTime.UTC().Format("20060102T150405Z"))
}

// place sizes an intent against the account's budget and hands it to the OMS.
//
// Returns (nil, rejection, false, nil) when the intent could not be sized at
// all, which for a small account is the common and correct outcome.
func (s *Service) place(ctx context.Context, p placement) (*domain.Order, *domain.Rejection, bool, error) {
	// ---- Size against the account's OWN budget ----------------------------
	sized, rejection, err := s.size(ctx, p.Account, p.rc.instrument, p.rc.quote,
		p.rc.limits, p.side, p.rc.signal)
	if err != nil {
		return nil, nil, false, err
	}
	if rejection != nil {
		return nil, rejection, false, nil
	}

	// The cap, applied after sizing and never before it. Sizing answers "what
	// can this account afford to risk"; the cap answers "how much of that is
	// this order permitted to be". Taking the smaller of the two can only
	// shrink the order, which is the only direction risk may move.
	if p.maxQuantity.IsPositive() && sized.Quantity.GreaterThan(p.maxQuantity) {
		sized.Quantity = p.rc.instrument.Spec.NormaliseQuantity(p.maxQuantity)
	}
	if !sized.Quantity.IsPositive() {
		rej := domain.NewRejection(domain.RejectQuantityInvalid,
			"No quantity survives the account's risk budget and this order's cap.")
		return nil, &rej, false, nil
	}

	// ---- Hand to the order pipeline ---------------------------------------
	// The idempotency key is derived from the strategy, version, instrument and
	// BAR TIME. Re-running the same bar therefore produces the same key and is
	// suppressed as a duplicate, which is what stops a scheduler restart from
	// double-trading a signal.
	version := p.rc.version
	idempotencyKey := p.idempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("strat-%s-v%d-%s-%s",
			p.StrategyID.String()[:8], version, p.rc.instrument.ID,
			p.rc.barTime.UTC().Format("20060102T150405Z"))
	}

	barTime := p.rc.barTime
	result, err := s.oms.PlaceOrder(ctx, oms.PlaceOrderRequest{
		IdempotencyKey:  idempotencyKey,
		ActorUserID:     p.ActorUserID,
		ActorRole:       p.ActorRole,
		AccountID:       p.Account.ID,
		InstrumentID:    p.rc.instrument.ID,
		Side:            p.side,
		Type:            domain.OrderTypeMarket,
		Quantity:        sized.Quantity,
		StopLoss:        sized.StopLoss,
		TakeProfit:      sized.TakeProfit,
		TimeInForce:     domain.TIFGoodTilCancelled,
		Source:          domain.SourceStrategy,
		StrategyID:      &p.StrategyID,
		StrategyVersion: &version,
		RequestHash: oms.HashRequest(p.Account.ID, p.rc.instrument.ID, p.side,
			domain.OrderTypeMarket, sized.Quantity, nil, nil, sized.StopLoss, sized.TakeProfit,
			domain.TIFGoodTilCancelled),
		RequestID:     p.RequestID,
		CorrelationID: p.CorrelationID,

		// The research plane classified the market's shape from these very
		// bars. Passing it on is what lets the decision record the regime it
		// was taken in; without it the OMS can only infer RISK_OFF and
		// EVENT_RISK from its own evidence and every ordinary decision is
		// UNKNOWN.
		ReportedRegime: p.regime,
		// The bar this decision was taken on, so the decision and the signal
		// that produced it join on it. Without this the decision snapshot's
		// bar_time column stayed NULL for every row ever written.
		BarTime: &barTime,
		// How much history that classification rested on. The OMS refuses to
		// characterise a market below domain.MinBarsForRegime and cannot count
		// bars itself without a query per decision.
		BarsAvailable: p.rc.barsAvailable,
		// The aggregate this order came out of, recorded on the snapshot and
		// never consulted by the OMS.
		Consensus: p.consensus,
	})
	if err != nil {
		// An identical bar already produced this order: the idempotency key
		// caught a re-run. That is the guard working, not a failure.
		if errors.Is(err, oms.ErrCommandInFlight) {
			return nil, nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("orchestrator: place order: %w", err)
	}
	return result.Order, result.Rejection, result.Accepted(), nil
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
