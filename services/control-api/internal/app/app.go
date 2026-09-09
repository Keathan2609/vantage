// Package app wires the control plane together.
//
// Construction order encodes the dependency direction of the whole platform:
// storage, then the control-plane services that gate execution, then the
// research client, then the HTTP surface. Nothing in the research direction can
// reach back into execution, because it is never handed a reference to it.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/booking"
	"github.com/vantage/control-api/internal/broker"
	brokermock "github.com/vantage/control-api/internal/broker/mock"
	"github.com/vantage/control-api/internal/config"
	"github.com/vantage/control-api/internal/crypto"
	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/econdata"
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/httpapi"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/marketdata"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/notify"
	"github.com/vantage/control-api/internal/oms"
	"github.com/vantage/control-api/internal/orchestrator"
	"github.com/vantage/control-api/internal/portfolio"
	"github.com/vantage/control-api/internal/quant"
	"github.com/vantage/control-api/internal/ratelimit"
	"github.com/vantage/control-api/internal/reconcile"
	"github.com/vantage/control-api/internal/replay"
	"github.com/vantage/control-api/internal/scheduler"
	"github.com/vantage/control-api/internal/store"
)

// App holds every constructed component.
type App struct {
	Config       config.Config
	Log          *logging.Logger
	Pool         *db.Pool
	Store        *store.Store
	Keyring      *crypto.Keyring
	Limiter      ratelimit.Limiter
	Brokers      *broker.Registry
	MockBroker   *brokermock.Broker
	Converter    *fx.Converter
	Portfolio    *portfolio.Service
	Booking      *booking.Service
	OMS          *oms.Service
	Reconciler   *reconcile.Service
	Quant        *quant.Client
	Orchestrator *orchestrator.Service
	Ingestor     *marketdata.Ingestor
	// Provider is the quote source in force. Typed as the interface rather
	// than as the mock, because it is now one of two implementations chosen by
	// configuration.
	Provider marketdata.Provider
	// Replay is non-nil only when VANTAGE_MARKET_DATA_PROVIDER=replay. A nil
	// engine is how every other code path knows replay is not available,
	// rather than by re-reading configuration.
	Replay      *replay.Engine
	EconData    *econdata.Ingestor
	Alerter     *notify.Alerter
	Scheduler   *scheduler.Scheduler
	MarketClock *domain.MarketClock
	Clock       domain.Clock
	Server      *httpapi.Server

	// replayClock is the same object as Clock, kept typed so the engine can
	// engage and disengage it.
	replayClock *replay.SwitchableClock

	closers []func()
}

// replayVenueSeed fixes the mock venue's randomness during a replay.
//
// A constant rather than the run's seed: the run seed is recorded on the run
// so a reader can tie a result to it, but the VENUE's behaviour has to be the
// same across every run of a dataset for two runs to be comparable at all.
// Varying it would reintroduce exactly the non-determinism this exists to
// remove.
const replayVenueSeed int64 = 20270302

// Version metadata, overridden at build time with -ldflags.
var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
)

// Build constructs the application.
func Build(ctx context.Context, cfg config.Config, log *logging.Logger) (*App, error) {
	// Refuse to construct anything at all outside paper mode. This is the
	// third of the three independent gates; the other two are the compile-time
	// constant and the configuration check.
	if cfg.ExecutionMode != "paper" {
		return nil, errors.New("app: this build supports paper execution only")
	}

	// The clock is switchable so a market replay can put this process on
	// dataset time without a second deployment. It behaves exactly as
	// SystemClock until a replay engages, which requires explicit
	// configuration and, outside an auto-start, an authenticated request.
	clock := replay.NewSwitchableClock(domain.SystemClock{})
	a := &App{Config: cfg, Log: log, Clock: clock, replayClock: clock}

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	a.Pool = pool
	a.closers = append(a.closers, pool.Close)
	a.Store = store.New(pool)

	a.Keyring, err = crypto.NewKeyring(cfg.DataEncryptionKeyVersion, cfg.DataEncryptionKey)
	if err != nil {
		return nil, err
	}

	// Rate limiting prefers Redis so limits hold across instances, and falls
	// back to in-process limiting rather than to no limiting at all.
	if cfg.RedisURL != "" {
		rl, rerr := ratelimit.NewRedisLimiter(cfg.RedisURL, time.Now)
		if rerr != nil {
			log.Warn("redis rate limiter unavailable; using in-process limits", "error", rerr.Error())
			mem := ratelimit.NewMemoryLimiter(time.Now)
			a.Limiter = mem
			a.closers = append(a.closers, mem.Close)
		} else {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if perr := rl.Ping(pingCtx); perr != nil {
				log.Warn("redis unreachable; using in-process rate limits", "error", perr.Error())
				_ = rl.Close()
				mem := ratelimit.NewMemoryLimiter(time.Now)
				a.Limiter = mem
				a.closers = append(a.closers, mem.Close)
			} else {
				a.Limiter = rl
				a.closers = append(a.closers, func() { _ = rl.Close() })
				log.Info("rate limiting backed by redis")
			}
			cancel()
		}
	} else {
		mem := ratelimit.NewMemoryLimiter(time.Now)
		a.Limiter = mem
		a.closers = append(a.closers, mem.Close)
		log.Info("rate limiting is in-process (single instance only)")
	}

	// Market calendar, with holidays loaded from the database.
	calendar := domain.ForexMetalsCalendar()
	if holidays, herr := a.Store.Market.Holidays(ctx, calendar.ID); herr == nil {
		calendar.Holidays = holidays
	} else {
		log.Warn("could not load market holidays", "error", herr.Error())
	}
	a.MarketClock, err = domain.NewMarketClock(calendar)
	if err != nil {
		return nil, err
	}

	a.Converter = fx.NewConverter(fx.StoreRateSource{Market: a.Store.Market}, 24*time.Hour,
		func() time.Time { return a.Clock.Now() })
	a.Portfolio = portfolio.New(a.Store, a.Converter, a.Clock)
	// The quote source.
	//
	// Replay is refused outside development and test by config.Load, so
	// reaching this branch at all means an operator asked for it in an
	// environment where it is allowed.
	var replayProvider *marketdata.ReplayProvider
	if cfg.ReplayEnabled() {
		replayProvider = marketdata.NewReplayProvider()
		a.Provider = replayProvider
		log.Warn("market data provider is REPLAY: this process will serve dataset " +
			"prices and, once a run is engaged, dataset time")
	} else {
		a.Provider = marketdata.NewMockProvider(20250101, a.MarketClock)
	}
	a.Ingestor = marketdata.NewIngestor(a.Store, a.Provider, a.Clock, a.MarketClock)

	// The calendar and news feeds sit behind their own provider interfaces, so
	// a licensed feed later is one implementation rather than a rewrite.
	nowFn := func() time.Time { return a.Clock.Now() }
	a.EconData = econdata.NewIngestor(a.Store,
		econdata.NewMockCalendar(nowFn), econdata.NewMockNews(nowFn))

	// Bars are built from the live quote stream. Without this the series is
	// frozen at seed time and every strategy re-evaluates one bar forever --
	// see internal/marketdata/aggregate.go for what that broke.
	//
	// The timeframes are the ones strategy versions actually declare. Building
	// one nobody reads costs a write every two seconds; not building one a
	// strategy declares starves that strategy permanently.
	aggregator := marketdata.NewAggregator(a.Provider.Name(),
		domain.Timeframe("15m"), domain.Timeframe("1h"), domain.Timeframe("4h"))
	a.Ingestor.SetAggregator(aggregator)

	// The mock venue keeps its own books, which is what makes reconciliation a
	// real comparison rather than a self-check.
	// The venue's own randomness -- slippage jitter and latency -- is SEEDED in
	// replay mode.
	//
	// Found by the determinism proof. Two replays of one dataset from a
	// byte-identical database produced identical decisions (665 signals, 250
	// orders, 189 fills, 2.51 lots) and DIFFERENT closing balances: 574.81 and
	// 575.38. The decisions were deterministic; the fill prices were not,
	// because DefaultConfig seeds the venue from time.Now().UnixNano().
	//
	// A replay whose P&L moves between runs cannot be used to compare a
	// strategy against itself, which is the entire point of one. So the venue
	// is given a fixed seed here. It is still adversarial -- slippage,
	// latency, partial fills and margin refusals all still happen -- just
	// reproducibly.
	venueConfig := brokermock.DefaultConfig()
	if cfg.ReplayEnabled() {
		// Seeded, NOT Deterministic. Deterministic would switch jitter and
		// random rejection off entirely, which removes the venue's whole
		// reason for existing: a mock that fills every order at the displayed
		// price produces strategies that only work against that mock. A fixed
		// seed keeps the venue awkward and makes it awkward the same way
		// twice.
		venueConfig.Seed = replayVenueSeed
	}
	a.MockBroker = brokermock.New(pool, quoteSource{store: a.Store}, venueRates{store: a.Store},
		a.MarketClock, a.Clock, venueConfig)

	a.Brokers = broker.NewRegistry()
	if err := a.Brokers.Register(a.MockBroker); err != nil {
		return nil, err
	}
	// Only adapters compiled in can be registered, and only the mock is
	// compiled in. A configuration naming another broker fails at load.
	for _, name := range cfg.EnabledBrokers {
		if _, err := a.Brokers.Get(name); err != nil {
			return nil, fmt.Errorf("app: configured broker %q is not available: %w", name, err)
		}
	}

	riskEngine := newRiskEngine(a.Converter)

	// One booking service, shared by the OMS and by reconciliation.
	//
	// This is the single accounting path: an execution returned by a
	// PlaceOrder call and an execution discovered by reconciliation after a
	// lost response both become a fill, a position and a ledger entry through
	// the same code. An architecture test asserts nothing else appends a fill,
	// which is what makes that guarantee structural.
	a.Booking, err = booking.New(a.Store, a.Converter, domain.ModePaper)
	if err != nil {
		return nil, err
	}

	a.OMS, err = oms.New(a.Store, a.Brokers, riskEngine, a.Portfolio, a.Booking, a.Converter,
		a.Clock, a.MarketClock, domain.ModePaper)
	if err != nil {
		return nil, err
	}

	a.Reconciler = reconcile.New(a.Store, a.Brokers, a.Booking, a.Clock)

	// The halt gate closes the window between reconciliation detecting unsafe
	// divergence and an automated order slipping through. The OMS re-checks it
	// inside the transaction that persists the order, after taking the account
	// row lock that a repair also takes -- so the invariant is enforced by
	// PostgreSQL's serialisation rather than by timing.
	a.OMS.SetHaltGate(a.Reconciler)
	a.Quant = quant.New(cfg.QuantBaseURL, cfg.QuantServiceToken, cfg.QuantTimeout)
	a.Orchestrator = orchestrator.New(a.Store, a.Quant, a.OMS, a.Portfolio, a.Converter,
		a.Reconciler, a.Clock, a.MarketClock)

	// Alerting is wired once every producer exists, and before the scheduler
	// starts, so each source has a sink from its first tick. Each component
	// declares its own narrow Alerter interface, which is why notify can
	// depend on store while nothing depends on notify.
	//
	// The order matters and is the reason this block sits here rather than
	// beside the OMS: an earlier revision attached the alerter to a.Quant
	// before a.Quant was constructed, and the service panicked on start-up.
	a.Alerter = notify.New(a.Store, func() time.Time { return a.Clock.Now() })
	a.Ingestor.SetAlerter(a.Alerter)
	a.OMS.SetAlerter(a.Alerter)
	a.Quant.SetAlerter(a.Alerter)
	a.Reconciler.SetAlerter(a.Alerter)

	a.Scheduler = scheduler.New(scheduler.Deps{
		Store:        a.Store,
		Pool:         pool,
		Ingestor:     a.Ingestor,
		EconData:     a.EconData,
		Orchestrator: a.Orchestrator,
		Reconciler:   a.Reconciler,
		MockBroker:   a.MockBroker,
		Portfolio:    a.Portfolio,
		Clock:        a.Clock,
		MarketClock:  a.MarketClock,
		Log:          log,
	})

	// The replay engine, after the scheduler because it drives it.
	//
	// Constructed only in replay mode. Everything downstream tests for a nil
	// engine, so "replay is unavailable" is a property of the object graph
	// rather than a configuration lookup repeated in five places.
	if replayProvider != nil {
		// The replay drives market data and strategy evaluation, so the
		// scheduler must not also do it on wall-clock intervals.
		a.Scheduler.SetExternallyDriven(true)

		registry, rerr := replay.LoadFixtures()
		if rerr != nil {
			return nil, fmt.Errorf("app: load replay fixtures: %w", rerr)
		}
		a.Replay = replay.NewEngine(registry, replayProvider, a.replayClock, a.Scheduler, log)
		// Bar aggregation is detached for the duration of a run: the dataset
		// already supplies complete bars, and folding one quote per bar into a
		// bar as well would write a degenerate candle over the real one.
		a.Replay.SetAggregatorControl(func() func() {
			a.Ingestor.SetAggregator(nil)
			return func() { a.Ingestor.SetAggregator(aggregator) }
		})
		// A run that cannot possibly trade should say so before it starts.
		// The most common cause by far is a trading authority whose window
		// does not cover the dataset's dates.
		a.Replay.SetPreflight(func(from, to time.Time) []string {
			var warnings []string
			accounts, aerr := a.Store.Accounts.ListAllAccounts(ctx)
			if aerr != nil {
				return []string{"could not check trading authority: " + aerr.Error()}
			}
			covered := false
			for _, account := range accounts {
				authority, autherr := a.Store.Control.ActiveAuthorityForAccount(ctx, account.ID)
				if autherr != nil {
					continue
				}
				if ok, _ := authority.Effective(from); !ok {
					continue
				}
				if ok, _ := authority.Effective(to); !ok {
					continue
				}
				if authority.AutomationEnabled {
					covered = true
				}
			}
			if !covered {
				warnings = append(warnings, fmt.Sprintf(
					"no account has an automation-enabled trading authority covering "+
						"%s to %s, so every strategy run in this replay will be skipped. "+
						"Grant an authority spanning the dataset, or reseed.",
					from.Format(time.RFC3339), to.Format(time.RFC3339)))
			}
			if state, serr := a.Store.Autopilot.Autopilot(ctx); serr == nil && !state.Enabled {
				warnings = append(warnings,
					"the global Autopilot switch is off, so this replay will evaluate "+
						"nothing. Enable it with POST /api/v1/autopilot.")
			}
			return warnings
		})

		// Each run begins from a clean market-data state.
		a.Replay.SetOnStart(func(startCtx context.Context) error {
			return a.Store.Market.PurgeReplayMarketData(startCtx)
		})

		// FX rates are re-stamped at each replay instant, holding their
		// seeded values.
		//
		// The account is ZAR and gold is quoted in USD, so sizing needs a
		// USD/ZAR rate. Seeded rates are dated at seed time, which a dataset
		// deliberately ahead of the seed makes months stale -- and the
		// converter refuses a stale rate, correctly. Holding the rate
		// constant is an explicit assumption: a replay's P&L contains no
		// currency movement.
		replayRates := [][2]money.Currency{
			{money.USD, money.ZAR}, {money.EUR, money.ZAR},
			{money.EUR, money.USD}, {money.GBP, money.USD},
		}
		a.Replay.SetBeforeStep(func(stepCtx context.Context, now time.Time) error {
			for _, pair := range replayRates {
				existing, rerr := a.Store.Market.LatestFXRate(stepCtx, pair[0], pair[1])
				if rerr != nil {
					continue // no seeded rate for this pair; nothing to hold
				}
				if !existing.SourceTime.Before(now) {
					continue // already current at or after this instant
				}
				if uerr := a.Store.Market.UpsertFXRate(stepCtx, pair[0], pair[1],
					existing.Rate, now, "replay"); uerr != nil {
					return uerr
				}
			}
			return nil
		})

		// Every run is recorded before it is announced, so a replay result can
		// be tied to the dataset, code and configuration that produced it
		// after the process that produced it is gone. The closure is where the
		// engine's vocabulary meets the store's: the engine cannot import
		// store, because store sits below it.
		a.Replay.SetRecorder(func(rctx context.Context, run replay.Run, warnings []string) error {
			return a.Store.Replay.RecordRun(rctx, store.ReplayRun{
				ID:            run.ID,
				DatasetID:     run.DatasetID,
				DatasetHash:   run.DatasetHash,
				CodeSHA:       run.CodeSHA,
				ConfigHash:    run.ConfigHash,
				Seed:          run.Seed,
				FromTime:      run.FromTime,
				ToTime:        run.ToTime,
				StartedAt:     run.StartedAt,
				FinishedAt:    run.FinishedAt,
				State:         string(run.State),
				Steps:         run.Counters.Steps,
				BarsProcessed: run.Counters.BarsProcessed,
				StepErrors:    run.Counters.Errors,
				Failure:       run.Error,
				Warnings:      warnings,
			})
		})

		log.Info("market replay is available",
			"datasets", len(registry.IDs()))
	}

	a.Server, err = httpapi.NewServer(httpapi.Deps{
		Config: cfg, Logger: log, Pool: pool, Store: a.Store, Keyring: a.Keyring,
		Limiter: a.Limiter, Brokers: a.Brokers, OMS: a.OMS, Portfolio: a.Portfolio,
		Ingestor: a.Ingestor, EconData: a.EconData, Alerter: a.Alerter,
		MockBroker: a.MockBroker,
		Reconciler: a.Reconciler, Quant: a.Quant,
		Orchestrator: a.Orchestrator, Clock: a.Clock, MarketClock: a.MarketClock,
		Replay:  a.Replay,
		Version: Version, Commit: Commit,
	})
	if err != nil {
		return nil, err
	}

	metrics.SetBuildInfo(Version, Commit, cfg.ExecutionMode)
	return a, nil
}

// Close releases resources in reverse construction order.
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
}

// Run serves HTTP and background work until the context is cancelled.
func (a *App) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	// The public API.
	srv := &http.Server{
		Addr:              a.Config.HTTPAddr,
		Handler:           a.Server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	// Metrics on a separate loopback listener. Trading rates and account
	// identifiers do not belong on the public API surface.
	metricsSrv := &http.Server{
		Addr:              "127.0.0.1:9090",
		Handler:           metricsMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.Log.Info("control plane listening",
			"addr", a.Config.HTTPAddr, "execution_mode", a.Config.ExecutionMode,
			"version", Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.Log.Info("metrics listening", "addr", metricsSrv.Addr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Log.Warn("metrics server stopped", "error", err.Error())
		}
	}()

	// Background work: market data, strategy evaluation, reconciliation,
	// outbox dispatch and session cleanup.
	bgCtx := logging.WithLogger(ctx, a.Log)
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.Scheduler.Run(bgCtx)
	}()

	// Reconcile at start-up before anything trades: the process may have died
	// mid-order last time.
	go func() {
		reports, err := a.Reconciler.RunAll(bgCtx, reconcile.TriggerStartup)
		if err != nil {
			a.Log.Error("start-up reconciliation failed", "error", err.Error())
			return
		}
		for _, rep := range reports {
			if !rep.Clean() {
				a.Log.Warn("start-up reconciliation left unresolved divergence",
					"account_id", rep.AccountID.String(),
					"issues", len(rep.Issues), "repaired", rep.Repaired,
					"critical", rep.CriticalCount())
			}
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		a.Log.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		a.Log.Warn("http shutdown was not graceful", "error", err.Error())
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		a.Log.Warn("metrics shutdown was not graceful", "error", err.Error())
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		a.Log.Info("shutdown complete")
	case <-shutdownCtx.Done():
		a.Log.Warn("background workers did not stop in time")
	}
	return nil
}

func metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", httpapi.MetricsHandler())
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	return mux
}

// quoteSource adapts the store to the mock broker's needs.
//
// The mock venue reads prices and instrument specifications, and nothing else.
// It has no access to accounts, orders or the ledger, mirroring the fact that a
// real venue knows only what it is told.
type quoteSource struct{ store *store.Store }

func (q quoteSource) LatestQuote(ctx context.Context, instrumentID string) (domain.Quote, *domain.Quote, error) {
	return q.store.Market.LatestQuote(ctx, instrumentID)
}

func (q quoteSource) Instrument(ctx context.Context, id string) (domain.Instrument, error) {
	return q.store.Market.Instrument(ctx, id)
}

// venueRates lets the mock venue convert quote-currency amounts into its own
// account currency.
//
// It reads the same rate table Vantage values positions from, which is the
// closest a simulation can get to "the venue and we agree on the exchange
// rate". A real venue would use its own, and the difference would then be a
// legitimate BALANCE_MISMATCH for an operator to look at -- which is exactly
// what the check is for.
type venueRates struct{ store *store.Store }

func (v venueRates) VenueRate(ctx context.Context, base, quote string) (decimal.Decimal, error) {
	r, err := v.store.Market.LatestFXRate(ctx, money.Currency(base), money.Currency(quote))
	if err == nil {
		return r.Rate, nil
	}
	// Try the inverse before giving up: the seed stores USD/ZAR, not ZAR/USD,
	// and a venue refusing to price a close for want of a reciprocal would be
	// an absurd reason to fail an execution.
	inv, invErr := v.store.Market.LatestFXRate(ctx, money.Currency(quote), money.Currency(base))
	if invErr != nil || !inv.Rate.IsPositive() {
		return decimal.Zero, err
	}
	return decimal.NewFromInt(1).Div(inv.Rate), nil
}
