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

	FillsRecorded = factory.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "trading", Name: "fills_total",
		Help: "Executions recorded.",
	}, []string{"symbol", "side"})

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
