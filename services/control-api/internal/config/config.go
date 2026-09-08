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

	DataEncryptionKey        []byte
	DataEncryptionKeyVersion int
	SessionSigningKey        []byte

	QuantBaseURL      string
	QuantServiceToken string
	QuantTimeout      time.Duration

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
		QuantBaseURL:         env("VANTAGE_QUANT_BASE_URL", "http://localhost:8000"),
		QuantServiceToken:    env("VANTAGE_QUANT_SERVICE_TOKEN", ""),
		SessionTTL:           12 * time.Hour,
		SessionIdleTTL:       60 * time.Minute,
	}

	switch c.Env {
	case EnvDevelopment, EnvTest, EnvStaging, EnvProduction:
	default:
		return Config{}, fmt.Errorf("VANTAGE_ENV %q is not a recognised environment", c.Env)
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
