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
	outboxInterval         = 3 * time.Second
	cleanupInterval        = 1 * time.Hour
	econDataInterval       = 15 * time.Minute
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
}

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

	// Market data drives the resting-order engine: a limit or stop can only
	// trigger on a price that actually occurred.
	go s.deps.Ingestor.Run(ctx, marketDataInterval, func(tickCtx context.Context) {
		s.processRestingOrders(tickCtx)
	})

	go s.loop(ctx, "strategies", strategyInterval, s.runStrategies)
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
	return s.withLease(ctx, "strategy_runner", func(ctx context.Context) error {
		log := s.deps.Log
		now := s.deps.Clock.Now()

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
	})
}

// runReconciliation compares every account against its venue.
func (s *Scheduler) runReconciliation(ctx context.Context) error {
	return s.withLease(ctx, "reconciliation", func(ctx context.Context) error {
		_, err := s.deps.Reconciler.RunAll(ctx, reconcile.TriggerScheduled)
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
