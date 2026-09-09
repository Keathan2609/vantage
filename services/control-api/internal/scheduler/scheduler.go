// Package scheduler runs the control plane's periodic work.
//
// Everything here is leased. Before any instance performs scheduled work it
// takes a database-backed lease, so running two control planes does not double
// the strategy evaluations, double the reconciliations, or double-dispatch the
// outbox. A lease expires, so an instance that dies holding one does not block
// the others indefinitely.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	brokermock "github.com/vantage/control-api/internal/broker/mock"
	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/econdata"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/marketdata"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/orchestrator"
	"github.com/vantage/control-api/internal/portfolio"
	"github.com/vantage/control-api/internal/reconcile"
	"github.com/vantage/control-api/internal/store"
)

// Intervals for the periodic jobs.
//
// Market data ticks fast enough that a 15-second staleness threshold is
// meaningful; everything else runs at a pace where the work is worth the query.
const (
	marketDataInterval     = 2 * time.Second
	strategyInterval       = 30 * time.Second
	reconciliationInterval = 5 * time.Minute
	// reconciliationTimeout bounds one whole pass over every account. Shorter
	// than the interval so an overrunning pass is abandoned rather than
	// queued behind the next one.
	reconciliationTimeout = 4 * time.Minute
	outboxInterval        = 3 * time.Second
	cleanupInterval       = 1 * time.Hour
	econDataInterval      = 15 * time.Minute
	// replayBackfillLookback is how much provider history each replay step
	// offers the store. Wide enough to cover a strategy's warm-up (the
	// longest declared required_bars at the longest timeframe) with room to
	// spare, because a short lookback starves a strategy in a way that looks
	// like the strategy declining to trade.
	replayBackfillLookback = 60 * 24 * time.Hour
	leaseTTL               = 2 * time.Minute
)

// Deps are the scheduler's collaborators.
type Deps struct {
	Store        *store.Store
	Pool         *db.Pool
	Ingestor     *marketdata.Ingestor
	EconData     *econdata.Ingestor
	Orchestrator *orchestrator.Service
	Reconciler   *reconcile.Service
	MockBroker   *brokermock.Broker
	Portfolio    *portfolio.Service
	Clock        domain.Clock
	MarketClock  *domain.MarketClock
	Log          *logging.Logger
}

// Scheduler runs periodic work.
type Scheduler struct {
	deps     Deps
	holderID string
	// externallyDriven suppresses the market-data and strategy loops.
	//
	// Set when a market replay owns the pipeline. Without it BOTH the
	// two-second ingestion loop and the replay's own step ingest, which was a
	// real defect: each replay instant produced two identical quotes, the
	// data-quality policy correctly reported `duplicate_tick`, and 75 of 685
	// strategy runs were skipped for degraded data that the harness itself had
	// manufactured.
	//
	// The other loops -- reconciliation, outbox, cleanup -- keep running,
	// because a replay is meant to exercise the real system's background
	// behaviour, not replace it.
	externallyDriven bool
}

// SetExternallyDriven stops the scheduler driving market data and strategies
// itself, leaving that to a market replay.
func (s *Scheduler) SetExternallyDriven(driven bool) { s.externallyDriven = driven }

// New builds a scheduler.
func New(deps Deps) *Scheduler {
	return &Scheduler{
		deps:     deps,
		holderID: fmt.Sprintf("control-api-%d", time.Now().UnixNano()),
	}
}

// Run starts every job and blocks until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	log := s.deps.Log
	log.Info("scheduler starting", "holder", s.holderID)

	if s.externallyDriven {
		// A market replay owns market data and strategy evaluation. Running
		// the interval loops as well would double-ingest every quote and
		// evaluate strategies at wall-clock intervals against dataset time.
		log.Warn("scheduler: market data and strategy loops are suppressed; " +
			"a market replay is driving the pipeline")
	} else {
		// Market data drives the resting-order engine: a limit or stop can
		// only trigger on a price that actually occurred.
		go s.deps.Ingestor.Run(ctx, marketDataInterval, func(tickCtx context.Context) {
			s.processRestingOrders(tickCtx)
		})

		go s.loop(ctx, "strategies", strategyInterval, s.runStrategies)
	}
	go s.loop(ctx, "reconciliation", reconciliationInterval, s.runReconciliation)
	go s.loop(ctx, "outbox", outboxInterval, s.dispatchOutbox)
	go s.loop(ctx, "cleanup", cleanupInterval, s.cleanup)

	// The calendar and news feeds refresh on their own cadence: a schedule of
	// macroeconomic releases does not change by the second, and a failed
	// refresh leaves the last known calendar in force rather than silently
	// removing every blackout.
	if s.deps.EconData != nil {
		go s.deps.EconData.Run(ctx, econDataInterval, func() time.Time { return s.deps.Clock.Now() })
	}

	<-ctx.Done()
	log.Info("scheduler stopped")
}

// loop runs a job on an interval, recovering from panics so one bad job cannot
// take the scheduler down with it.
func (s *Scheduler) loop(ctx context.Context, name string, interval time.Duration, job func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						s.deps.Log.Error("scheduled job panicked", "job", name, "panic", fmt.Sprint(rec))
					}
				}()
				jobCtx, cancel := context.WithTimeout(ctx, interval*3)
				defer cancel()
				if err := job(jobCtx); err != nil && !errors.Is(err, context.Canceled) {
					s.deps.Log.Error("scheduled job failed", "job", name, "error", err.Error())
				}
			}()
		}
	}
}

// withLease runs fn only if this instance can take the named lease.
func (s *Scheduler) withLease(ctx context.Context, key string, fn func(context.Context) error) error {
	acquired := false
	err := s.deps.Pool.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO scheduler_leases (lease_key, holder, acquired_at, expires_at)
			VALUES ($1, $2, now(), now() + $3::interval)
			ON CONFLICT (lease_key) DO UPDATE
			SET holder = EXCLUDED.holder, acquired_at = now(), expires_at = EXCLUDED.expires_at
			WHERE scheduler_leases.expires_at < now() OR scheduler_leases.holder = $2`,
			key, s.holderID, leaseTTL.String())
		if err != nil {
			return err
		}
		acquired = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return fmt.Errorf("scheduler: acquire lease %q: %w", key, err)
	}
	if !acquired {
		// Another instance holds it. Not an error.
		return nil
	}
	return fn(ctx)
}

// processRestingOrders lets the simulated venue trigger resting orders against
// the price that just arrived, then folds any resulting executions into
// Vantage's records.
func (s *Scheduler) processRestingOrders(ctx context.Context) {
	if s.deps.MockBroker == nil {
		return
	}
	accounts, err := s.deps.Store.Accounts.ListAllAccounts(ctx)
	if err != nil {
		s.deps.Log.Error("could not list accounts for resting-order processing", "error", err.Error())
		return
	}
	for _, account := range accounts {
		accountRef := account.ID.String()
		if account.BrokerAcctRef != nil && *account.BrokerAcctRef != "" {
			accountRef = *account.BrokerAcctRef
		}
		reports, err := s.deps.MockBroker.ProcessRestingOrders(ctx, accountRef)
		if err != nil {
			s.deps.Log.Error("resting-order processing failed",
				"account_id", account.ID.String(), "error", err.Error())
			continue
		}
		if len(reports) > 0 {
			s.deps.Log.Info("resting orders triggered",
				"account_id", account.ID.String(), "executions", len(reports))
			// The executions are picked up by the next reconciliation, which
			// is the same path a real venue's asynchronous fills would take.
			if _, err := s.deps.Reconciler.Run(ctx, account, reconcile.TriggerScheduled); err != nil {
				s.deps.Log.Error("post-execution reconciliation failed", "error", err.Error())
			}
		}
	}
}

// runStrategies evaluates every enabled PAPER strategy for every account whose
// authority permits it.
func (s *Scheduler) runStrategies(ctx context.Context) error {
	return s.withLease(ctx, "strategy_runner", s.runStrategiesUnleased)
}

// runStrategiesUnleased is the body of the strategy job.
//
// Split out so a market replay can call the same code without the lease, which
// is measured against wall time and therefore meaningless when the clock is
// driven by a dataset. See ReplayStep.
func (s *Scheduler) runStrategiesUnleased(ctx context.Context) error {
	return func(ctx context.Context) error {
		log := s.deps.Log
		now := s.deps.Clock.Now()

		// The global Autopilot switch, checked before anything else.
		//
		// The OMS enforces it too, in the order transaction, which is what
		// actually makes it safe. This check exists so that switching
		// autopilot off stops the WORK as well as the orders: without it the
		// scheduler would keep evaluating strategies every thirty seconds,
		// calling the research service, and recording refusals nobody asked
		// for -- a quiet system that is still busy.
		autopilot, err := s.deps.Store.Autopilot.Autopilot(ctx)
		if err != nil {
			return fmt.Errorf("scheduler: read autopilot state: %w", err)
		}
		if !autopilot.Enabled {
			return nil
		}

		// Nothing to do while the market is closed, and evaluating anyway
		// would fill the run log with skips.
		if !s.deps.MarketClock.Status(now).Tradable() {
			return nil
		}

		accounts, err := s.deps.Store.Accounts.ListAllAccounts(ctx)
		if err != nil {
			return err
		}
		strategies, err := s.deps.Store.Research.ListStrategies(ctx)
		if err != nil {
			return err
		}

		for _, account := range accounts {
			if !account.Enabled || !account.TradingEnabled {
				continue
			}
			authority, err := s.deps.Store.Control.ActiveAuthorityForAccount(ctx, account.ID)
			if err != nil {
				continue // no authority: nothing automated may run
			}
			if !authority.AutomationEnabled {
				continue
			}

			owner, err := s.deps.Store.Users.UserByID(ctx, account.UserID)
			if err != nil {
				continue
			}

			for _, strategy := range strategies {
				if !strategy.Enabled || strategy.HighRisk {
					continue
				}
				if !authority.PermitsStrategy(&strategy.ID) {
					continue
				}
				version, err := s.deps.Store.Research.LatestStrategyVersion(ctx, strategy.ID)
				if err != nil || version.Lifecycle != domain.LifecyclePaper {
					continue
				}
				for _, instrumentID := range version.Instruments {
					if !authority.PermitsInstrument(instrumentID) {
						continue
					}
					outcome, err := s.deps.Orchestrator.EvaluateAndRoute(ctx, orchestrator.RunRequest{
						Account: account, StrategyID: strategy.ID, InstrumentID: instrumentID,
						Version: version.Version, Execute: true,
						ActorUserID: owner.ID, ActorRole: owner.Role,
						RequestID: "scheduler",
					})
					if err != nil {
						log.Error("strategy evaluation failed",
							"strategy", strategy.Key, "instrument", instrumentID,
							"error", err.Error())
						continue
					}
					if outcome.Executed {
						log.Info("strategy order placed",
							"strategy", strategy.Key, "instrument", instrumentID,
							"action", string(outcome.Action))
					}
				}
			}
		}
		return nil
	}(ctx)
}

// runReconciliation compares every account against its venue.
//
// Three separate mechanisms keep this bounded, and each covers a case the
// others do not:
//
//   - the LEASE stops two control-plane instances reconciling at once
//   - the per-account ADVISORY LOCK stops a scheduled run overlapping a manual
//     one, or overlapping the previous tick if it is still going
//   - the TIMEOUT here stops a venue that accepts a connection and then never
//     answers from holding the lease until the process restarts
//
// Without the timeout the first two are worthless: a run blocked on a socket
// holds both the lease and the lock indefinitely, and reconciliation silently
// stops happening while reporting no error at all.
//
// Deliberately shorter than the interval, so a tick that overruns is abandoned
// rather than queued behind the next one.
func (s *Scheduler) runReconciliation(ctx context.Context) error {
	return s.withLease(ctx, "reconciliation", func(ctx context.Context) error {
		bounded, cancel := context.WithTimeout(ctx, reconciliationTimeout)
		defer cancel()

		_, err := s.deps.Reconciler.RunAll(bounded, reconcile.TriggerScheduled)
		if errors.Is(err, context.DeadlineExceeded) {
			// Reported, not swallowed. A reconciliation pass that ran out of
			// time left some accounts unchecked, and their readiness will say
			// RECONCILIATION_REQUIRED until a later pass reaches them -- which
			// is the correct outcome, but only if someone is told why.
			s.deps.Log.Warn("reconciliation pass exceeded its timeout",
				"timeout", reconciliationTimeout.String(),
				"consequence", "accounts not reached this pass report "+
					"RECONCILIATION_REQUIRED until a later pass completes")
			return nil
		}
		return err
	})
}

// dispatchOutbox publishes durable side effects.
//
// Delivery is at-least-once by design: a message may be published twice if the
// process dies between the side effect and marking it published. Consumers are
// therefore required to be idempotent, which is why every event carries the
// identifier of the thing it describes.
func (s *Scheduler) dispatchOutbox(ctx context.Context) error {
	return s.withLease(ctx, "outbox_dispatch", func(ctx context.Context) error {
		return s.deps.Pool.InTx(ctx, func(tx pgx.Tx) error {
			messages, err := s.deps.Store.Trading.ClaimOutboxBatch(ctx, tx, 50)
			if err != nil {
				return err
			}
			for _, msg := range messages {
				if err := s.handleOutboxMessage(ctx, tx, msg); err != nil {
					s.deps.Log.Warn("outbox message failed",
						"id", msg.ID, "event", msg.EventType, "error", err.Error())
					if merr := s.deps.Store.Trading.MarkOutboxFailed(ctx, tx,
						msg.ID, msg.Attempts, err.Error()); merr != nil {
						return merr
					}
					continue
				}
				if err := s.deps.Store.Trading.MarkOutboxPublished(ctx, tx, msg.ID); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// handleOutboxMessage turns an event into a user-visible notification.
func (s *Scheduler) handleOutboxMessage(ctx context.Context, tx pgx.Tx, msg store.OutboxMessage) error {
	switch msg.EventType {
	case "order.filled", "order.submitted":
		// Fills and submissions are visible in the orders table already;
		// emitting a notification for each would be noise. The event exists so
		// that a future consumer (email, push, webhook) has a durable feed.
		return nil
	default:
		s.deps.Log.Debug("outbox message with no handler", "event", msg.EventType)
		return nil
	}
}

// cleanup prunes expired rows and refreshes gauges.
func (s *Scheduler) cleanup(ctx context.Context) error {
	return s.withLease(ctx, "cleanup", func(ctx context.Context) error {
		// Sessions are kept a week past expiry so a security investigation can
		// still see them, then removed.
		removed, err := s.deps.Store.Users.DeleteExpiredSessions(ctx, 7*24*time.Hour)
		if err != nil {
			return err
		}
		if removed > 0 {
			s.deps.Log.Info("pruned expired sessions", "count", removed)
		}

		accounts, err := s.deps.Store.Accounts.ListAllAccounts(ctx)
		if err != nil {
			return err
		}
		for _, account := range accounts {
			snapshot, err := s.deps.Portfolio.Compute(ctx, account)
			if err != nil {
				continue
			}
			equity, _ := snapshot.State.Equity.Decimal().Float64()
			drawdown, _ := snapshot.State.DrawdownFraction().Float64()
			metrics.AccountEquity.WithLabelValues(account.ID.String(),
				string(account.Currency)).Set(equity)
			metrics.AccountDrawdown.WithLabelValues(account.ID.String()).Set(drawdown)

			// Roll the daily-loss reference point at the venue's own day
			// boundary, not local midnight.
			if rolled, err := s.deps.Portfolio.RollTradingDayIfNeeded(ctx, account,
				tradingDayBoundary(s.deps.Clock.Now())); err == nil && rolled {
				s.deps.Log.Info("trading day rolled", "account_id", account.ID.String())
			}
		}
		return nil
	})
}

// tradingDayBoundary returns the most recent 21:00 UTC, which approximates the
// FX rollover. The calendar's own New York boundary is authoritative for market
// hours; this is the daily-loss accounting boundary and is deliberately simple.
func tradingDayBoundary(now time.Time) time.Time {
	utc := now.UTC()
	boundary := time.Date(utc.Year(), utc.Month(), utc.Day(), 21, 0, 0, 0, time.UTC)
	if utc.Before(boundary) {
		boundary = boundary.Add(-24 * time.Hour)
	}
	return boundary
}

// ReplayStep performs one deterministic tick of the pipeline.
//
// # Why this is a method on the real scheduler
//
// The acceptance requirement for market replay is that it drives the SAME
// pipeline autonomous trading uses. That rules out a harness that calls
// ingestion and the orchestrator itself: such a harness proves the harness
// works, and every difference between it and the scheduler is a difference
// nobody notices until it matters.
//
// So this calls exactly the job functions the interval loops call, in the
// order the running system does. What replay changes is what MOVES the clock:
// a ticker in ordinary operation, the dataset here. Nothing else differs.
//
// # Why the lease is bypassed
//
// runStrategies takes the `strategy_runner` lease so two instances cannot
// double-run. During a replay there is one process by construction, and the
// lease's two-minute TTL is measured against wall time while the replay clock
// may cover days in seconds -- so a lease taken at replay-hour one would still
// be held at replay-hour ninety, and every later step would silently decline.
// The lease is the wrong mechanism for a run whose clock is not wall time.
//
// It is bypassed by calling the job body rather than by weakening withLease,
// so ordinary operation is untouched.
func (s *Scheduler) ReplayStep(ctx context.Context, onPhase func(phase string)) error {
	phase := func(name string) {
		if onPhase != nil {
			onPhase(name)
		}
	}

	// 1. Ingestion, data quality and bar aggregation -- the real IngestOnce.
	phase("ingest")
	if err := s.deps.Ingestor.IngestOnce(ctx); err != nil {
		return fmt.Errorf("scheduler: replay ingest: %w", err)
	}

	// 1b. Bars from the provider's own history.
	//
	// A replay dataset IS the bar series, so bars arrive this way rather than
	// being aggregated back out of one quote per bar -- which would write a
	// degenerate candle over a real one. The lookback is generous because a
	// strategy needs a warm-up: the store upserts, so re-offering a bar the
	// database already holds costs a write and changes nothing.
	phase("backfill")
	if err := s.deps.Ingestor.BackfillBars(ctx, replayBackfillLookback); err != nil {
		return fmt.Errorf("scheduler: replay backfill: %w", err)
	}

	// 2. Resting limit and stop orders, which can only trigger on a price that
	//    actually occurred. Driven by market data in ordinary operation too.
	phase("resting_orders")
	s.processRestingOrders(ctx)

	// 3. Strategy evaluation, orchestration, risk, authority, OMS, venue.
	phase("strategies")
	if err := s.runStrategiesUnleased(ctx); err != nil {
		return fmt.Errorf("scheduler: replay strategies: %w", err)
	}

	// 4. The outbox, so notifications and projections produced by this step
	//    are dispatched before the next one -- matching the ordinary system,
	//    where the outbox runs an order of magnitude more often than
	//    strategies.
	phase("outbox")
	if err := s.dispatchOutbox(ctx); err != nil {
		// Logged rather than fatal, exactly as the interval loop does: a
		// failed dispatch must not stop trading.
		s.deps.Log.Warn("replay outbox dispatch failed", "error", err.Error())
	}
	return nil
}

// ReplayReconcile runs reconciliation once, without its lease.
//
// Separate from ReplayStep because reconciliation runs on a five-minute
// interval in ordinary operation, not on every tick, and a replay that
// reconciled after every bar would not resemble the running system. A
// failure-injection scenario calls this at the point it wants to prove
// recovery.
func (s *Scheduler) ReplayReconcile(ctx context.Context) error {
	if s.deps.Reconciler == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, reconciliationTimeout)
	defer cancel()
	_, err := s.deps.Reconciler.RunAll(ctx, reconcile.TriggerScheduled)
	return err
}
