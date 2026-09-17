// Package metrics defines the Prometheus instruments the control plane emits.
//
// The metric set is chosen so that the questions an operator actually asks
// during an incident can be answered from the dashboard: is the platform
// refusing trades, and why? Is market data fresh? Is the venue answering? Has
// anything been suppressed as a duplicate? Is anyone failing to log in?
//
// Cardinality is kept deliberately low. Labels are bounded sets — symbols,
// reject codes, sources — never user identifiers, order identifiers or free
// text, which would turn a metrics backend into an unbounded index.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "vantage"

// Registry is the collector registry this process exposes.
var Registry = prometheus.NewRegistry()

var factory = promauto.With(Registry)

// HTTP metrics.
var (
	HTTPRequests = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "http", Name: "requests_total",
		Help: "HTTP requests by route, method and status class.",
	}, []string{"route", "method", "status"})

	HTTPDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
		Help:    "HTTP request duration by route.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"route", "method"})

	HTTPInFlight = factory.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "http", Name: "requests_in_flight",
		Help: "HTTP requests currently being served.",
	})
)

// Trading metrics.
var (
	OrdersAccepted = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "trading", Name: "orders_accepted_total",
		Help: "Orders that passed every pre-trade gate and reached the venue.",
	}, []string{"symbol", "source"})

	// OrdersRejected is labelled by reject code, which is what makes "why is
	// nothing trading today?" a single query rather than a log investigation.
	OrdersRejected = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "trading", Name: "orders_rejected_total",
		Help: "Orders refused, by structured reject code.",
	}, []string{"symbol", "code", "source"})

	OrdersFailed = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "trading", Name: "orders_failed_total",
		Help: "Orders whose venue outcome is unknown and awaiting reconciliation.",
	}, []string{"symbol"})

	// OrderPersistDeadlocks must stay at zero. A non-zero value means two
	// order-placement transactions formed a lock cycle, which the declared
	// lock order (store.LockAccountTx) is supposed to make impossible. It is
	// counted rather than only logged so that a regression is visible on a
	// dashboard instead of buried in a log line.
	OrderPersistDeadlocks = factory.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "trading", Name: "order_persist_deadlocks_total",
		Help: "Deadlocks while persisting an accepted order. Expected to be zero.",
	})

	FillsRecorded = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "trading", Name: "fills_total",
		Help: "Executions recorded.",
	}, []string{"symbol", "side"})

	// DuplicateExecutionsSuppressed counts venue executions refused as already
	// booked. Rising is HEALTHY: it means the unique index on
	// (broker_name, broker_fill_id) is doing its job while a venue replays a
	// stream after a reconnect. Rising with no reconnect is worth a look.
	DuplicateExecutionsSuppressed = factory.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "trading", Name: "duplicate_executions_suppressed_total",
		Help: "Venue executions recognised as already booked and not applied twice.",
	})

	// RecoveredFills counts executions booked by reconciliation rather than
	// received from a PlaceOrder response. Every one of these is a trade that
	// happened at the venue and would otherwise have been missing from the
	// ledger, so a non-zero value is both good news and a signal that
	// something upstream lost a response.
	RecoveredFills = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "recovered_fills_total",
		Help: "Executions discovered and booked by reconciliation.",
	}, []string{"symbol"})

	// AutomationAllowed is 1 when automated trading may run anywhere, 0 when
	// every account is halted or unreconciled. The single number to alert on:
	// a dashboard showing a healthy process and a zero here is describing a
	// system that is up and not trading.
	AutomationAllowed = factory.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "automation_allowed",
		Help: "1 when automated trading is permitted, 0 when halted or unreconciled.",
	})

	// HaltedAccounts counts accounts whose automation is stopped.
	HaltedAccounts = factory.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "halted_accounts",
		Help: "Accounts whose automated trading is currently stopped.",
	})

	// ReconciliationIssues counts issues raised, by type and severity.
	ReconciliationIssues = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "issues_total",
		Help: "Reconciliation issues raised, by type and severity.",
	}, []string{"type", "severity"})

	// ReconciliationRepairs counts repairs applied, by type and by whether a
	// human authorised them.
	ReconciliationRepairs = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "repairs_total",
		Help: "Reconciliation repairs applied, by issue type and actor.",
	}, []string{"type", "actor"})

	// ReconciliationOverlapsPrevented counts runs that declined to start
	// because another run held the account's lock. Expected to be small and
	// non-zero: it means overlap prevention is working, not that anything is
	// wrong.
	ReconciliationOverlapsPrevented = factory.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "overlaps_prevented_total",
		Help: "Reconciliation runs skipped because one was already in progress.",
	})

	// DuplicateCommandsSuppressed rising is healthy — it means idempotency is
	// doing its job. It rising sharply means a client is retrying too eagerly.
	DuplicateCommandsSuppressed = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "trading", Name: "duplicate_commands_suppressed_total",
		Help: "Financial commands recognised as duplicates and not re-executed.",
	}, []string{"command_type"})

	BrokerCallDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "broker", Name: "call_duration_seconds",
		Help:    "Broker adapter call latency.",
		Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	}, []string{"broker", "operation"})

	BrokerErrors = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "broker", Name: "errors_total",
		Help: "Broker adapter errors by kind.",
	}, []string{"broker", "operation", "kind"})

	BrokerUp = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "broker", Name: "up",
		Help: "1 when the broker adapter reports healthy.",
	}, []string{"broker"})
)

// Risk metrics.
var (
	RiskRejections = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "risk", Name: "rejections_total",
		Help: "Pre-trade risk refusals by check.",
	}, []string{"check"})

	KillSwitchActive = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "risk", Name: "kill_switch_active",
		Help: "1 while a kill switch is active in the given scope.",
	}, []string{"scope"})

	AccountDrawdown = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "risk", Name: "account_drawdown_fraction",
		Help: "Current drawdown from peak equity, as a fraction.",
	}, []string{"account"})

	AccountEquity = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "risk", Name: "account_equity",
		Help: "Account equity in the account's own currency (simulated in paper mode).",
	}, []string{"account", "currency"})

	RiskUtilisation = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "risk", Name: "limit_utilisation_fraction",
		Help: "How much of a given risk limit is consumed, as a fraction.",
	}, []string{"account", "limit"})
)

// Market-data metrics.
var (
	QuoteAge = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "quote_age_seconds",
		Help: "Age of the most recent quote per instrument.",
	}, []string{"symbol"})

	DataQuality = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "healthy",
		Help: "1 when an instrument's feed is healthy enough for automated trading.",
	}, []string{"symbol"})

	DataQualityIssues = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "issues_total",
		Help: "Data-quality defects detected, by kind.",
	}, []string{"symbol", "issue"})

	ProviderLatency = factory.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "provider_latency_seconds",
		Help:    "Latency between a provider's source timestamp and ingestion.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 30},
	}, []string{"provider"})

	// External historical-data acquisition. Separate from the quote-path
	// metrics above because the failure modes differ: a quote feed degrades,
	// a history provider rate-limits and paginates.
	MarketDataProviderRequests = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "provider_requests_total",
		Help: "Outbound requests to an external market-data provider.",
	}, []string{"provider"})

	MarketDataProviderFailures = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "provider_failures_total",
		Help: "Failed provider requests, by kind.",
	}, []string{"provider", "kind"})

	MarketDataRateLimitEvents = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "provider_rate_limited_total",
		Help: "Times a provider reported its rate limit reached.",
	}, []string{"provider"})

	// Ingested and ignored are counted separately on purpose. A backfill that
	// reports thousands of bars ingested when it re-fetched an existing range
	// would hide that it did no work.
	MarketDataBarsIngested = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "bars_ingested_total",
		Help: "Bars written to the local store from a provider.",
	}, []string{"provider", "instrument", "timeframe"})

	MarketDataDuplicatesIgnored = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "bars_duplicate_total",
		Help: "Bars already present locally when a provider returned them again.",
	}, []string{"provider", "instrument", "timeframe"})

	MarketDataInvalidBars = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "bars_invalid_total",
		Help: "Provider bars refused before storage.",
	}, []string{"provider", "instrument"})

	MarketDataSyncDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "sync_duration_seconds",
		Help:    "Wall time of a backfill, sync or repair run.",
		Buckets: []float64{0.5, 1, 5, 15, 60, 300, 900, 3600},
	}, []string{"provider", "kind"})

	MarketDataLatestAge = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "marketdata", Name: "latest_bar_age_seconds",
		Help: "Age of the newest locally stored bar per instrument and timeframe.",
	}, []string{"instrument", "timeframe"})
)

// Strategy, model and reconciliation metrics.
var (
	StrategyRuns = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "strategy", Name: "runs_total",
		Help: "Strategy evaluations by outcome.",
	}, []string{"strategy", "status"})

	SignalsGenerated = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "strategy", Name: "signals_total",
		Help: "Signals produced, by action.",
	}, []string{"strategy", "action"})

	StrategyRunDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "strategy", Name: "run_duration_seconds",
		Help:    "Time to evaluate one strategy.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"strategy"})

	ModelInferenceDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "ml", Name: "inference_duration_seconds",
		Help:    "Model inference latency.",
		Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	}, []string{"model"})

	ModelPredictions = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "ml", Name: "predictions_total",
		Help: "Model predictions produced.",
	}, []string{"model"})

	ReconciliationRuns = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "runs_total",
		Help: "Reconciliation runs by outcome.",
	}, []string{"trigger", "status"})

	ReconciliationMismatches = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "mismatches_total",
		Help: "Discrepancies found between Vantage and a venue.",
	}, []string{"kind", "severity"})

	UnresolvedDiscrepancies = factory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "reconciliation", Name: "unresolved_discrepancies",
		Help: "Open discrepancies awaiting resolution.",
	}, []string{"account"})
)

// Security metrics.
var (
	AuthFailures = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "security", Name: "auth_failures_total",
		Help: "Failed authentication attempts by reason.",
	}, []string{"reason"})

	AuthSuccesses = factory.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "security", Name: "auth_successes_total",
		Help: "Successful authentications.",
	})

	RateLimited = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "security", Name: "rate_limited_total",
		Help: "Requests refused by rate limiting.",
	}, []string{"bucket"})

	AuthorizationDenied = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "security", Name: "authorization_denied_total",
		Help: "Requests refused by authorisation, including cross-tenant attempts.",
	}, []string{"reason"})
)

// Build information, so a dashboard can show what is actually deployed.
// Alerting. AlertsRaised counts every event the platform judged worth telling
// someone about; AlertsSuppressed counts the repeats a cooldown swallowed. The
// ratio is the signal: a kind that is almost all suppressions has a cooldown
// that is too short or a detector that is too twitchy.
var (
	AlertsRaised = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "alerts", Name: "raised_total",
		Help: "Events raised, by kind and severity.",
	}, []string{"kind", "severity"})

	AlertsSuppressed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "alerts", Name: "suppressed_total",
		Help: "Repeat events suppressed by a cooldown, by kind.",
	}, []string{"kind"})
)

var BuildInfo = factory.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: namespace, Name: "build_info",
	Help: "Build metadata. Always 1; the labels carry the information.",
}, []string{"version", "commit", "execution_mode"})

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// SetBuildInfo records build metadata once at start-up.
func SetBuildInfo(version, commit, executionMode string) {
	BuildInfo.WithLabelValues(version, commit, executionMode).Set(1)
}
