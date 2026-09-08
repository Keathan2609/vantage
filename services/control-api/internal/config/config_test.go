package config

import (
	"errors"
	"strings"
	"testing"
)

// validEnv sets the minimum configuration a successful Load requires.
func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("VANTAGE_ENV", "development")
	t.Setenv("VANTAGE_EXECUTION_MODE", "paper")
	t.Setenv("VANTAGE_ENABLED_BROKERS", "mock")
	t.Setenv("VANTAGE_DATABASE_URL", "postgres://u:p@localhost:5432/vantage?sslmode=disable")
	t.Setenv("VANTAGE_DATA_ENCRYPTION_KEY", "ZGV2ZWxvcG1lbnQtb25seS1rZXktMzJieXRlcyEhISE=")
	t.Setenv("VANTAGE_SESSION_SIGNING_KEY", "ZGV2ZWxvcG1lbnQtb25seS1zZXNzaW9uLWtleS0zMmI=")
}

func TestBuildRefusesLiveExecution(t *testing.T) {
	// The single most important test in this package: the build must not be
	// configurable into live or demo execution.
	if BuildAllowsLiveExecution {
		t.Fatal("BuildAllowsLiveExecution must be false in this build")
	}

	for _, mode := range []string{"live", "LIVE", "demo", "real", "production"} {
		t.Run(mode, func(t *testing.T) {
			validEnv(t)
			t.Setenv("VANTAGE_EXECUTION_MODE", mode)
			_, err := Load()
			if !errors.Is(err, ErrLiveExecutionRefused) {
				t.Fatalf("Load with mode %q: err = %v, want ErrLiveExecutionRefused", mode, err)
			}
		})
	}
}

func TestBuildRefusesNonMockBrokers(t *testing.T) {
	for _, brokers := range []string{"exness", "mt5", "mock,exness", "MT5"} {
		t.Run(brokers, func(t *testing.T) {
			validEnv(t)
			t.Setenv("VANTAGE_ENABLED_BROKERS", brokers)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted broker list %q", brokers)
			}
			if !strings.Contains(err.Error(), "only the mock broker") {
				t.Fatalf("unexpected error for %q: %v", brokers, err)
			}
		})
	}
}

func TestLoadValidPaperConfiguration(t *testing.T) {
	validEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ExecutionMode != "paper" {
		t.Errorf("mode = %q, want paper", c.ExecutionMode)
	}
	if !c.SimulatedFunds() {
		t.Error("paper mode must report simulated funds")
	}
	if len(c.EnabledBrokers) != 1 || c.EnabledBrokers[0] != "mock" {
		t.Errorf("brokers = %v, want [mock]", c.EnabledBrokers)
	}
}

func TestDeployedEnvironmentsRejectDevelopmentKeys(t *testing.T) {
	for _, envName := range []string{"staging", "production"} {
		t.Run(envName, func(t *testing.T) {
			validEnv(t)
			t.Setenv("VANTAGE_ENV", envName)
			_, err := Load()
			if err == nil {
				t.Fatal("expected the example development keys to be refused")
			}
			if !strings.Contains(err.Error(), "development keys") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestKeyValidation(t *testing.T) {
	cases := []struct{ name, value, want string }{
		{"missing", "", "required"},
		{"not base64", "not-base64!!", "base64"},
		{"wrong length", "c2hvcnQ=", "32 bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("VANTAGE_DATA_ENCRYPTION_KEY", c.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestRequiredValues(t *testing.T) {
	validEnv(t)
	t.Setenv("VANTAGE_DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected a missing database URL to be rejected")
	}
}

func TestUnknownEnvironmentIsRejected(t *testing.T) {
	validEnv(t)
	t.Setenv("VANTAGE_ENV", "prod-ish")
	if _, err := Load(); err == nil {
		t.Fatal("expected an unrecognised environment to be rejected")
	}
}

func TestQuantTimeoutBounds(t *testing.T) {
	for _, v := range []string{"0", "-1", "500", "abc"} {
		validEnv(t)
		t.Setenv("VANTAGE_QUANT_TIMEOUT_SECONDS", v)
		if _, err := Load(); err == nil {
			t.Errorf("timeout %q should be rejected", v)
		}
	}
}
