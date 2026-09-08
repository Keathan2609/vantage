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
	"github.com/vantage/control-api/internal/notify"
	"github.com/vantage/control-api/internal/oms"
	"github.com/vantage/control-api/internal/orchestrator"
	"github.com/vantage/control-api/internal/portfolio"
	"github.com/vantage/control-api/internal/quant"
	"github.com/vantage/control-api/internal/ratelimit"
	"github.com/vantage/control-api/internal/reconcile"
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
	OMS          *oms.Service
	Reconciler   *reconcile.Service
	Quant        *quant.Client
	Orchestrator *orchestrator.Service
	Ingestor     *marketdata.Ingestor
	Provider     *marketdata.MockProvider
	EconData     *econdata.Ingestor
	Alerter      *notify.Alerter
	Scheduler    *scheduler.Scheduler
	MarketClock  *domain.MarketClock
	Clock        domain.Clock
	Server       *httpapi.Server

	closers []func()
}

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

	a := &App{Config: cfg, Log: log, Clock: domain.SystemClock{}}

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
	a.Provider = marketdata.NewMockProvider(20250101, a.MarketClock)
	a.Ingestor = marketdata.NewIngestor(a.Store, a.Provider, a.Clock, a.MarketClock)

	// The calendar and news feeds sit behind their own provider interfaces, so
	// a licensed feed later is one implementation rather than a rewrite.
	nowFn := func() time.Time { return a.Clock.Now() }
	a.EconData = econdata.NewIngestor(a.Store,
		econdata.NewMockCalendar(nowFn), econdata.NewMockNews(nowFn))

	// The mock venue keeps its own books, which is what makes reconciliation a
	// real comparison rather than a self-check.
	a.MockBroker = brokermock.New(pool, quoteSource{store: a.Store}, a.MarketClock,
		a.Clock, brokermock.DefaultConfig())

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
	a.OMS, err = oms.New(a.Store, a.Brokers, riskEngine, a.Portfolio, a.Converter,
		a.Clock, a.MarketClock, domain.ModePaper)
	if err != nil {
		return nil, err
	}

	a.Reconciler = reconcile.New(a.Store, a.Brokers, a.Clock)
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

	a.Server, err = httpapi.NewServer(httpapi.Deps{
		Config: cfg, Logger: log, Pool: pool, Store: a.Store, Keyring: a.Keyring,
		Limiter: a.Limiter, Brokers: a.Brokers, OMS: a.OMS, Portfolio: a.Portfolio,
		Ingestor: a.Ingestor, EconData: a.EconData, Alerter: a.Alerter,
		MockBroker: a.MockBroker,
		Reconciler: a.Reconciler, Quant: a.Quant,
		Orchestrator: a.Orchestrator, Clock: a.Clock, MarketClock: a.MarketClock,
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
				a.Log.Warn("start-up reconciliation found discrepancies",
					"account_id", rep.AccountID.String(),
					"discrepancies", len(rep.Discrepancies), "critical", rep.CriticalCount())
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
