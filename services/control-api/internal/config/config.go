// Package config loads and validates process configuration.
//
// Configuration is the last line of defence for the platform's central safety
// property: this build cannot trade real money. That guarantee is enforced in
// three independent places, so no single mistake can undo it:
//
//  1. BuildAllowsLiveExecution is a compile-time false. Turning it on requires
//     a code change, review and a rebuild — not an environment variable.
//  2. Load rejects any execution mode other than "paper", and rejects any
//     broker adapter other than the mock, before the server starts.
//  3. No live broker adapter exists in the binary. Even a forged configuration
//     would find nothing to route an order to.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// BuildAllowsLiveExecution gates every execution mode beyond paper.
//
// It is a constant, not a variable and not a build tag, so that no runtime
// input can flip it and no linker flag can be talked into it. Enabling live or
// demo execution is a deliberate engineering act that must pass code review,
// and it must not happen before the work described in
// docs/REGULATORY_BOUNDARY.md and docs/COMPLIANCE_READINESS.md is complete.
const BuildAllowsLiveExecution = false

// Environment names the deployment context.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvTest        Environment = "test"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

// Config is the validated process configuration.
type Config struct {
	Env      Environment
	LogLevel string

	HTTPAddr        string
	PublicWebOrigin string

	// DatabaseURL is the runtime connection, using a role with DML rights only.
	DatabaseURL string
	// MigrationDatabaseURL is the schema-owner connection, used exclusively by
	// the migrate command. Separating the two means the serving process cannot
	// alter the schema, drop a table or disable a trigger even if it is
	// compromised. It defaults to DatabaseURL for single-role local setups.
	MigrationDatabaseURL string
	RedisURL             string // optional; empty disables Redis-backed features

	ExecutionMode  string
	EnabledBrokers []string

	// MarketDataProvider selects the quote source: "mock" or "replay".
	//
	// Replay must be asked for explicitly and is refused outside development
	// and test. It is not a data-source preference: engaging a replay puts the
	// WHOLE PROCESS on dataset time, so session state, quote ages, event
	// windows and daily P&L boundaries all move to wherever the dataset sits.
	// Defaulting to it, or letting it be selected in staging, would mean a
	// process that looks normal and is judging live prices against a date in
	// a fixture.
	MarketDataProvider string
	// ReplayAutoStart names a dataset to engage at boot, for a scripted run.
	// Empty is the normal case: a replay is started through the admin API so
	// there is a request, an actor and an audit entry behind it.
	ReplayAutoStart string

	DataEncryptionKey        []byte
	DataEncryptionKeyVersion int
	SessionSigningKey        []byte

	QuantBaseURL      string
	QuantServiceToken string
	QuantTimeout      time.Duration

	// TwelveDataAPIKey authorises historical market-data acquisition.
	//
	// OPTIONAL, deliberately. An absent key makes the provider report
	// MISCONFIGURED and leaves synthetic data, replay and everything already
	// stored working. Refusing to start would take the whole platform down
	// over a feature most of it does not use, and an operator debugging a
	// dead process learns less than one reading a provider status of
	// MISCONFIGURED.
	//
	// SERVER-SIDE ONLY. It is never rendered, never returned by an API, never
	// logged, and never placed in a decision snapshot. configDigest in
	// internal/httpapi is deliberately narrow and does not read it, and
	// TestTheProviderStatusNeverCarriesTheKey pins the API surface.
	TwelveDataAPIKey string
	// TwelveDataBaseURL is FIXED configuration, not a request parameter. A
	// provider URL that a caller could set is an SSRF primitive pointed at
	// whatever the server can reach.
	TwelveDataBaseURL string
	// TwelveDataRequestsPerMinute bounds outbound request rate. The free plan
	// is 8/min at the time of writing; the default is deliberately under it
	// because being throttled by a provider looks like an outage.
	TwelveDataRequestsPerMinute int
	TwelveDataTimeout           time.Duration

	// ResearchDataDir is where research dataset snapshots are written for
	// the Python research plane. Fixed configuration: the API never takes
	// a filesystem path from a request.
	ResearchDataDir string

	// SessionTTL is how long a session remains valid without re-authentication.
	SessionTTL time.Duration
	// SessionIdleTTL expires sessions that stop being used sooner than the
	// absolute TTL.
	SessionIdleTTL time.Duration
}

// ErrLiveExecutionRefused is returned when configuration requests an execution
// mode this build refuses to provide.
var ErrLiveExecutionRefused = errors.New(
	"this build supports paper execution only: live and demo execution are not compiled in")

// ErrReplayRefused is returned when replay is requested outside development.
var ErrReplayRefused = errors.New("market replay is not available in this environment")

// ReplayEnabled reports whether this process may engage a market replay.
func (c Config) ReplayEnabled() bool { return c.MarketDataProvider == "replay" }

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	c := Config{
		Env:                  Environment(env("VANTAGE_ENV", "development")),
		LogLevel:             env("VANTAGE_LOG_LEVEL", "info"),
		HTTPAddr:             env("VANTAGE_HTTP_ADDR", ":8080"),
		PublicWebOrigin:      env("VANTAGE_PUBLIC_WEB_ORIGIN", "http://localhost:3000"),
		DatabaseURL:          env("VANTAGE_DATABASE_URL", ""),
		MigrationDatabaseURL: env("VANTAGE_MIGRATION_DATABASE_URL", ""),
		RedisURL:             env("VANTAGE_REDIS_URL", ""),
		ExecutionMode:        strings.ToLower(env("VANTAGE_EXECUTION_MODE", "paper")),
		MarketDataProvider:   strings.ToLower(env("VANTAGE_MARKET_DATA_PROVIDER", "mock")),
		ReplayAutoStart:      env("VANTAGE_REPLAY_DATASET", ""),
		QuantBaseURL:         env("VANTAGE_QUANT_BASE_URL", "http://localhost:8000"),
		QuantServiceToken:    env("VANTAGE_QUANT_SERVICE_TOKEN", ""),

		TwelveDataAPIKey:            strings.TrimSpace(os.Getenv("TWELVE_DATA_API_KEY")),
		TwelveDataBaseURL:           env("VANTAGE_TWELVE_DATA_BASE_URL", "https://api.twelvedata.com"),
		TwelveDataRequestsPerMinute: envInt("VANTAGE_TWELVE_DATA_RPM", 7),
		TwelveDataTimeout:           15 * time.Second,
		ResearchDataDir:             env("VANTAGE_RESEARCH_DATA_DIR", "../quant/research-data"),
		SessionTTL:                  12 * time.Hour,
		SessionIdleTTL:              60 * time.Minute,
	}

	switch c.Env {
	case EnvDevelopment, EnvTest, EnvStaging, EnvProduction:
	default:
		return Config{}, fmt.Errorf("VANTAGE_ENV %q is not a recognised environment", c.Env)
	}

	// --- Market-data provider gate -------------------------------------------
	switch c.MarketDataProvider {
	case "mock", "replay":
	default:
		return Config{}, fmt.Errorf(
			"VANTAGE_MARKET_DATA_PROVIDER %q is not recognised (mock or replay)",
			c.MarketDataProvider)
	}
	if c.MarketDataProvider == "replay" && c.Env != EnvDevelopment && c.Env != EnvTest {
		// Refused rather than downgraded. A deployment that asked for replay
		// and silently got live data would be worse than one that failed to
		// start: the operator would believe they were watching a replay.
		return Config{}, fmt.Errorf(
			"%w: market replay puts the whole process on dataset time and is "+
				"available in development and test only (VANTAGE_ENV=%q)",
			ErrReplayRefused, c.Env)
	}
	if c.ReplayAutoStart != "" && c.MarketDataProvider != "replay" {
		return Config{}, fmt.Errorf(
			"VANTAGE_REPLAY_DATASET is set but VANTAGE_MARKET_DATA_PROVIDER is %q; "+
				"a dataset cannot be replayed by the mock provider",
			c.MarketDataProvider)
	}

	// --- Execution-mode gate -------------------------------------------------
	if c.ExecutionMode != "paper" {
		return Config{}, fmt.Errorf("%w (VANTAGE_EXECUTION_MODE=%q)",
			ErrLiveExecutionRefused, c.ExecutionMode)
	}
	if !BuildAllowsLiveExecution && c.ExecutionMode != "paper" {
		return Config{}, ErrLiveExecutionRefused
	}

	brokers := splitList(env("VANTAGE_ENABLED_BROKERS", "mock"))
	if len(brokers) == 0 {
		brokers = []string{"mock"}
	}
	for _, b := range brokers {
		if b != "mock" {
			return Config{}, fmt.Errorf(
				"broker adapter %q is not available in this build: only the mock broker is compiled in", b)
		}
	}
	c.EnabledBrokers = brokers

	// --- Required values -----------------------------------------------------
	if c.DatabaseURL == "" {
		return Config{}, errors.New("VANTAGE_DATABASE_URL is required")
	}
	if _, err := url.Parse(c.DatabaseURL); err != nil {
		return Config{}, fmt.Errorf("VANTAGE_DATABASE_URL is not a valid URL: %w", err)
	}
	if c.MigrationDatabaseURL == "" {
		c.MigrationDatabaseURL = c.DatabaseURL
	} else if _, err := url.Parse(c.MigrationDatabaseURL); err != nil {
		return Config{}, fmt.Errorf("VANTAGE_MIGRATION_DATABASE_URL is not a valid URL: %w", err)
	}
	if c.RedisURL != "" {
		if _, err := url.Parse(c.RedisURL); err != nil {
			return Config{}, fmt.Errorf("VANTAGE_REDIS_URL is not a valid URL: %w", err)
		}
	}
	if _, err := url.Parse(c.PublicWebOrigin); err != nil {
		return Config{}, fmt.Errorf("VANTAGE_PUBLIC_WEB_ORIGIN is not a valid URL: %w", err)
	}

	// --- Keys ----------------------------------------------------------------
	var err error
	if c.DataEncryptionKey, err = decodeKey("VANTAGE_DATA_ENCRYPTION_KEY", 32); err != nil {
		return Config{}, err
	}
	if c.SessionSigningKey, err = decodeKey("VANTAGE_SESSION_SIGNING_KEY", 32); err != nil {
		return Config{}, err
	}
	c.DataEncryptionKeyVersion, err = strconv.Atoi(env("VANTAGE_DATA_ENCRYPTION_KEY_VERSION", "1"))
	if err != nil || c.DataEncryptionKeyVersion < 1 {
		return Config{}, errors.New("VANTAGE_DATA_ENCRYPTION_KEY_VERSION must be a positive integer")
	}

	// Development ships with well-known keys so the stack starts with one
	// command. Any deployed environment must not: a known key is no key.
	if c.Env == EnvStaging || c.Env == EnvProduction {
		if isDevelopmentKey(c.DataEncryptionKey) || isDevelopmentKey(c.SessionSigningKey) {
			return Config{}, errors.New(
				"refusing to start: the example development keys are in use outside development")
		}
		if c.QuantServiceToken == "" || c.QuantServiceToken == "development-only-quant-service-token" {
			return Config{}, errors.New(
				"refusing to start: VANTAGE_QUANT_SERVICE_TOKEN is unset or still the development value")
		}
	}

	secs, err := strconv.Atoi(env("VANTAGE_QUANT_TIMEOUT_SECONDS", "15"))
	if err != nil || secs <= 0 || secs > 120 {
		return Config{}, errors.New("VANTAGE_QUANT_TIMEOUT_SECONDS must be between 1 and 120")
	}
	c.QuantTimeout = time.Duration(secs) * time.Second

	return c, nil
}

// IsProduction reports whether hardened defaults (secure cookies, strict CORS)
// apply.
func (c Config) IsProduction() bool {
	return c.Env == EnvProduction || c.Env == EnvStaging
}

// IsDevelopment reports whether development-only affordances are permitted:
// the seed, and the broker fault-injection endpoint. Anything gated on this
// must be harmless if it somehow ran elsewhere, because the gate is the only
// thing standing between it and a deployment.
func (c Config) IsDevelopment() bool {
	return c.Env == EnvDevelopment || c.Env == EnvTest
}

// SimulatedFunds reports whether all balances in this process are simulated.
// It is always true in this build and is surfaced to clients so no interface
// can present paper numbers as real by omission.
func (c Config) SimulatedFunds() bool { return c.ExecutionMode == "paper" }

// TwelveDataConfigured reports whether historical acquisition is possible.
//
// A method rather than a comparison at each call site, so that "is the key
// present" has one answer and cannot drift into three slightly different
// emptiness checks.
func (c Config) TwelveDataConfigured() bool {
	return strings.TrimSpace(c.TwelveDataAPIKey) != ""
}

// envInt reads a bounded integer, falling back rather than failing: a typo in
// a rate limit must not stop the process, and the default is the safe value.
func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func decodeKey(name string, wantLen int) ([]byte, error) {
	raw := os.Getenv(name)
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%s must be base64-encoded: %w", name, err)
	}
	if len(key) != wantLen {
		return nil, fmt.Errorf("%s must decode to %d bytes, got %d", name, wantLen, len(key))
	}
	return key, nil
}

// developmentKeyPrefixes are the recognisable openings of the example keys
// shipped in .env.example.
var developmentKeyPrefixes = []string{"development-only", "development-"}

func isDevelopmentKey(key []byte) bool {
	s := string(key)
	for _, p := range developmentKeyPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
