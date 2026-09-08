// Package httpapi exposes the control plane over HTTP.
//
// Routing rules that hold everywhere in this package:
//
//   - The API is versioned under /api/v1. Breaking a contract means adding a
//     version, not changing a response in place.
//   - Every route is explicitly either public or authenticated. There is no
//     "authenticate if a cookie happens to be present" behaviour, because that
//     pattern turns an authentication bug into an authorisation bug.
//   - State-changing routes carry CSRF protection and an operation-specific
//     rate limit. Read routes carry a generous shared limit.
//   - Handlers never load a user-owned object by id alone; they load it scoped
//     to the caller. See internal/store.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/vantage/control-api/internal/broker"
	brokermock "github.com/vantage/control-api/internal/broker/mock"
	"github.com/vantage/control-api/internal/config"
	"github.com/vantage/control-api/internal/crypto"
	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/econdata"
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
	"github.com/vantage/control-api/internal/store"
)

// Deps are the server's collaborators.
type Deps struct {
	Config       config.Config
	Logger       *logging.Logger
	Pool         *db.Pool
	Store        *store.Store
	Keyring      *crypto.Keyring
	Limiter      ratelimit.Limiter
	Brokers      *broker.Registry
	OMS          *oms.Service
	Portfolio    *portfolio.Service
	Ingestor     *marketdata.Ingestor
	EconData     *econdata.Ingestor
	Alerter      *notify.Alerter
	MockBroker   *brokermock.Broker
	Reconciler   *reconcile.Service
	Quant        *quant.Client
	Orchestrator *orchestrator.Service
	Clock        domain.Clock
	MarketClock  *domain.MarketClock
	Version      string
	Commit       string
}

// Server serves the control-plane API.
type Server struct {
	cfg          config.Config
	log          *logging.Logger
	pool         *db.Pool
	store        *store.Store
	keyring      *crypto.Keyring
	limiter      ratelimit.Limiter
	brokers      *broker.Registry
	oms          *oms.Service
	portfolio    *portfolio.Service
	ingestor     *marketdata.Ingestor
	econData     *econdata.Ingestor
	alerter      *notify.Alerter
	mockBroker   *brokermock.Broker
	reconciler   *reconcile.Service
	quant        *quant.Client
	orchestrator *orchestrator.Service
	clock        domain.Clock
	marketClock  *domain.MarketClock
	version      string
	commit       string
	router       chi.Router
	startedAt    time.Time
}

// NewServer wires the router.
func NewServer(d Deps) (*Server, error) {
	if d.Config.ExecutionMode != "paper" {
		return nil, errors.New("httpapi: refusing to serve in a non-paper execution mode")
	}
	s := &Server{
		cfg: d.Config, log: d.Logger, pool: d.Pool, store: d.Store, keyring: d.Keyring,
		limiter: d.Limiter, brokers: d.Brokers, oms: d.OMS, portfolio: d.Portfolio,
		ingestor: d.Ingestor, econData: d.EconData, alerter: d.Alerter,
		mockBroker: d.MockBroker,
		reconciler: d.Reconciler, quant: d.Quant,
		orchestrator: d.Orchestrator,
		clock:        d.Clock, marketClock: d.MarketClock,
		version: d.Version, commit: d.Commit, startedAt: time.Now(),
	}
	s.router = s.routes()
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(s.requestIDMiddleware)
	r.Use(s.recoverer)
	r.Use(s.securityHeaders)
	r.Use(s.corsMiddleware)
	r.Use(s.observability)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(bodyLimit(256 * 1024))

	// Operational endpoints. Liveness and readiness are unauthenticated so an
	// orchestrator can reach them; they expose no account data. Metrics are
	// bound to the internal listener rather than published here.
	r.Get("/health/live", s.handleLiveness)
	r.Get("/health/ready", s.handleReadiness)
	r.Get("/version", s.handleVersion)

	r.Route("/api/v1", func(api chi.Router) {
		// ---- Public --------------------------------------------------------
		api.Group(func(pub chi.Router) {
			pub.Use(s.rateLimit(ratelimit.RuleLogin))
			pub.Post("/auth/login", s.handleLogin)
		})

		// ---- Partially authenticated (MFA challenge only) -------------------
		api.Group(func(mfa chi.Router) {
			mfa.Use(s.requirePartialAuth)
			mfa.Use(s.csrfProtect)
			mfa.Use(s.rateLimit(ratelimit.RuleMFA))
			mfa.Post("/auth/mfa/verify", s.handleMFAVerify)
			mfa.Post("/auth/logout", s.handleLogout)
		})

		// ---- Authenticated reads --------------------------------------------
		api.Group(func(auth chi.Router) {
			auth.Use(s.requireAuth)
			auth.Use(s.rateLimit(ratelimit.RuleRead))

			auth.Get("/auth/session", s.handleSession)
			auth.Get("/auth/sessions", s.handleListSessions)

			auth.Get("/accounts", s.handleListAccounts)
			auth.Get("/accounts/{accountID}", s.handleGetAccount)
			auth.Get("/accounts/{accountID}/portfolio", s.handlePortfolio)
			auth.Get("/accounts/{accountID}/equity-history", s.handleEquityHistory)
			auth.Get("/accounts/{accountID}/transactions", s.handleTransactions)
			auth.Get("/accounts/{accountID}/attribution", s.handleAttribution)

			auth.Get("/instruments", s.handleListInstruments)
			auth.Get("/instruments/{instrumentID}", s.handleGetInstrument)
			auth.Get("/market/quotes", s.handleQuotes)
			auth.Get("/market/health", s.handleMarketHealth)
			auth.Get("/market/bars", s.handleBars)
			auth.Get("/market/status", s.handleMarketStatus)

			auth.Get("/orders", s.handleListOrders)
			auth.Get("/orders/{orderID}", s.handleGetOrder)
			auth.Get("/positions", s.handleListPositions)

			auth.Get("/risk/limits/{accountID}", s.handleGetRiskLimits)
			auth.Get("/risk/events/{accountID}", s.handleRiskEvents)
			auth.Get("/authority/{accountID}", s.handleGetAuthority)
			auth.Get("/kill-switches", s.handleListKillSwitches)

			auth.Get("/strategies", s.handleListStrategies)
			auth.Get("/strategies/{strategyID}", s.handleGetStrategy)
			auth.Get("/signals", s.handleListSignals)
			auth.Get("/decisions/{accountID}", s.handleListDecisions)
			auth.Get("/decisions/{accountID}/{decisionID}", s.handleGetDecision)

			auth.Get("/backtests", s.handleListBacktests)
			auth.Get("/backtests/{backtestID}", s.handleGetBacktest)
			auth.Get("/ml/models", s.handleListModels)
			auth.Get("/ml/datasets", s.handleListDatasets)

			auth.Get("/calendar", s.handleCalendar)
			auth.Get("/news", s.handleNews)
			auth.Get("/scanner", s.handleScanner)

			auth.Get("/activity/{accountID}", s.handleActivity)
			auth.Get("/audit", s.handleAudit)
			auth.Get("/notifications", s.handleNotifications)
			auth.Get("/connections", s.handleConnections)
			auth.Get("/reconciliation/{accountID}", s.handleReconciliationStatus)
		})

		// ---- Trading (trader role, CSRF, tight limits) -----------------------
		api.Group(func(trade chi.Router) {
			trade.Use(s.requireAuth)
			trade.Use(s.csrfProtect)
			trade.Use(s.requireRole(domain.RoleTrader))

			trade.With(s.rateLimit(ratelimit.RuleOrderPlace)).
				Post("/orders", s.handlePlaceOrder)
			trade.With(s.rateLimit(ratelimit.RuleOrderCancel)).
				Post("/orders/{orderID}/cancel", s.handleCancelOrder)
			trade.With(s.rateLimit(ratelimit.RuleOrderPlace)).
				Post("/positions/{positionID}/flatten", s.handleFlattenPosition)
			trade.With(s.rateLimit(ratelimit.RuleResearch)).
				Post("/backtests", s.handleRunBacktest)
			trade.With(s.rateLimit(ratelimit.RuleResearch)).
				Post("/ml/train", s.handleTrainModel)
			trade.With(s.rateLimit(ratelimit.RuleResearch)).
				Post("/strategies/{strategyID}/run", s.handleRunStrategy)
		})

		// ---- Control-plane changes (trader or admin, strict limits) ----------
		api.Group(func(ctrl chi.Router) {
			ctrl.Use(s.requireAuth)
			ctrl.Use(s.csrfProtect)
			ctrl.Use(s.requireRole(domain.RoleTrader, domain.RoleAdmin))

			// Kill switches get their own generous limit: an operator must
			// never be throttled out of stopping trading.
			ctrl.With(s.rateLimit(ratelimit.RuleKillSwitch)).
				Post("/kill-switches", s.handleActivateKillSwitch)
			ctrl.With(s.rateLimit(ratelimit.RuleKillSwitch)).
				Post("/kill-switches/{killSwitchID}/deactivate", s.handleDeactivateKillSwitch)

			ctrl.With(s.rateLimit(ratelimit.RuleControlChange)).
				Put("/risk/limits/{accountID}", s.handleUpdateRiskLimits)
			ctrl.With(s.rateLimit(ratelimit.RuleControlChange)).
				Post("/authority", s.handleCreateAuthority)
			ctrl.With(s.rateLimit(ratelimit.RuleControlChange)).
				Post("/authority/{authorityID}/revoke", s.handleRevokeAuthority)
			ctrl.With(s.rateLimit(ratelimit.RuleControlChange)).
				Patch("/authority/{authorityID}", s.handleUpdateAuthority)
			ctrl.With(s.rateLimit(ratelimit.RuleControlChange)).
				Post("/accounts/{accountID}/trading-enabled", s.handleSetTradingEnabled)
			ctrl.With(s.rateLimit(ratelimit.RuleControlChange)).
				Post("/reconciliation/{accountID}/run", s.handleRunReconciliation)
			ctrl.With(s.rateLimit(ratelimit.RuleControlChange)).
				Post("/strategies/{strategyID}/promote", s.handlePromoteStrategy)
			ctrl.With(s.rateLimit(ratelimit.RuleControlChange)).
				Post("/strategies/{strategyID}/enabled", s.handleSetStrategyEnabled)

			ctrl.With(s.rateLimit(ratelimit.RuleRead)).
				Post("/notifications/{notificationID}/read", s.handleMarkNotificationRead)
		})

		// ---- Account security (any authenticated user, own account only) -----
		api.Group(func(sec chi.Router) {
			sec.Use(s.requireAuth)
			sec.Use(s.csrfProtect)
			sec.Use(s.rateLimit(ratelimit.RulePasswordChange))

			sec.Post("/auth/password", s.handleChangePassword)
			sec.Post("/auth/mfa/enroll", s.handleMFAEnroll)
			sec.Post("/auth/mfa/activate", s.handleMFAActivate)
			sec.Post("/auth/mfa/disable", s.handleMFADisable)
			sec.Post("/auth/sessions/{sessionID}/revoke", s.handleRevokeSession)
		})

		// ---- Development-only ------------------------------------------------
		// Broker fault injection. Gated on the environment inside each
		// handler as well as here, because a route that exists only in
		// development is one deployment away from existing everywhere.
		if s.cfg.IsDevelopment() {
			api.Group(func(dev chi.Router) {
				dev.Use(s.requireAuth)
				dev.Use(s.csrfProtect)
				dev.Use(s.requireRole(domain.RoleTrader, domain.RoleAdmin))
				dev.Use(s.rateLimit(ratelimit.RuleControlChange))

				dev.Get("/dev/broker-faults", s.handleListBrokerFaults)
				dev.Post("/dev/broker-faults", s.handleArmBrokerFault)
				dev.Post("/dev/broker-faults/reset", s.handleResetBrokerFaults)
			})
		}

		// ---- Administration --------------------------------------------------
		api.Group(func(admin chi.Router) {
			admin.Use(s.requireAuth)
			admin.Use(s.csrfProtect)
			admin.Use(s.requireRole(domain.RoleAdmin))
			admin.Use(s.rateLimit(ratelimit.RuleControlChange))

			admin.Get("/admin/users", s.handleAdminListUsers)
			admin.Post("/admin/users/{userID}/role", s.handleAdminSetRole)
			admin.Post("/admin/users/{userID}/disabled", s.handleAdminSetDisabled)
			admin.Get("/admin/audit/verify", s.handleVerifyAuditChain)
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "No such endpoint.")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed",
			"That method is not allowed on this endpoint.")
	})
	return r
}

// MetricsHandler returns the Prometheus handler.
//
// It is served on a SEPARATE internal listener, not on the public API. Metrics
// carry account identifiers and trading rates; publishing them alongside the
// API would leak operational detail to anyone who can reach the service.
func MetricsHandler() http.Handler {
	return promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics:   true,
		MaxRequestsInFlight: 5,
	})
}

// ---------------------------------------------------------------------------
// Operational handlers
// ---------------------------------------------------------------------------

type livenessResponse struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
}

// handleLiveness answers whether the process is running. It deliberately
// touches no dependency: a liveness probe that fails when the database is slow
// causes an orchestrator to restart a healthy process mid-incident.
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, livenessResponse{
		Status: "alive",
		Uptime: time.Since(s.startedAt).Truncate(time.Second).String(),
	})
}

type readinessResponse struct {
	Status        string            `json:"status"`
	ExecutionMode string            `json:"execution_mode"`
	Checks        map[string]string `json:"checks"`
	Degraded      []string          `json:"degraded,omitempty"`
}

// handleReadiness reports whether the service can serve traffic.
//
// The distinction from liveness matters: the database is required, so losing it
// makes the service not ready. Redis and the quant service are not required —
// the platform degrades without them — so they are reported but do not fail
// the probe.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	resp := readinessResponse{
		Status:        "ready",
		ExecutionMode: s.cfg.ExecutionMode,
		Checks:        map[string]string{},
	}
	status := http.StatusOK

	if err := s.pool.Ping(ctx); err != nil {
		resp.Checks["database"] = "unavailable"
		resp.Status = "not_ready"
		status = http.StatusServiceUnavailable
	} else {
		resp.Checks["database"] = "ok"
	}

	for _, name := range s.brokers.Names() {
		adapter, err := s.brokers.Get(name)
		if err != nil {
			continue
		}
		h := adapter.Health(ctx)
		resp.Checks["broker:"+name] = string(h.State)
		if h.State == broker.HealthUp {
			metrics.BrokerUp.WithLabelValues(name).Set(1)
		} else {
			metrics.BrokerUp.WithLabelValues(name).Set(0)
			resp.Degraded = append(resp.Degraded, "broker:"+name)
		}
	}

	if s.quant != nil {
		if err := s.quant.Health(ctx); err != nil {
			resp.Checks["quant"] = "unavailable"
			// Research being down stops signal generation but leaves manual
			// paper trading and every read path working.
			resp.Degraded = append(resp.Degraded, "quant")
		} else {
			resp.Checks["quant"] = "ok"
		}
	}

	writeJSON(w, r, status, resp)
}

type versionResponse struct {
	Version              string `json:"version"`
	Commit               string `json:"commit"`
	ExecutionMode        string `json:"execution_mode"`
	SimulatedFunds       bool   `json:"simulated_funds"`
	LiveTradingAvailable bool   `json:"live_trading_available"`
}

// handleVersion reports build identity and, explicitly, that live trading is
// not available. Clients render a banner from this rather than assuming.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, versionResponse{
		Version:              s.version,
		Commit:               s.commit,
		ExecutionMode:        s.cfg.ExecutionMode,
		SimulatedFunds:       s.cfg.SimulatedFunds(),
		LiveTradingAvailable: config.BuildAllowsLiveExecution,
	})
}
