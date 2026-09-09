package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/vantage/control-api/internal/app"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/marketdata"
	"github.com/vantage/control-api/internal/seed"
)

// runServe starts the control plane.
func runServe(ctx context.Context) error {
	cfg, log, err := loadConfig()
	if err != nil {
		return err
	}

	// Stated plainly at every start-up. An operator reading logs should never
	// have to infer which mode the platform is in.
	log.Info("starting Vantage control plane",
		"version", app.Version, "commit", app.Commit,
		"env", string(cfg.Env), "execution_mode", cfg.ExecutionMode,
		"live_trading_available", false,
		"note", "This build executes simulated PAPER orders against a mock venue. "+
			"No real broker adapter is compiled in and no real funds can move.")

	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	// Refuse to serve against a schema the binary does not recognise. A
	// running service on a stale schema fails in ways that look like data
	// corruption rather than a deployment mistake.
	pending, err := application.PendingMigrationCount(ctx)
	if err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf(
			"%d schema migrations are pending; run 'control-api migrate' before serving", pending)
	}

	return application.Run(ctx)
}

// runSeed loads deterministic development data.
func runSeed(ctx context.Context) error {
	cfg, log, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Env != "development" && cfg.Env != "test" {
		// The seed creates users with published passwords. Loading it into a
		// deployed environment would create known-credential accounts.
		return fmt.Errorf("refusing to seed in %s: seed data contains development-only credentials", cfg.Env)
	}

	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	// The seeder generates a price history, which needs the mock provider
	// specifically. Seeding a database configured for replay is a
	// contradiction: the dataset supplies the history.
	mockProvider, ok := application.Provider.(*marketdata.MockProvider)
	if !ok {
		return fmt.Errorf(
			"seed: this process is configured for market replay, which supplies its " +
				"own history. Seed with VANTAGE_MARKET_DATA_PROVIDER=mock")
	}

	result, err := seed.Run(ctx, seed.Deps{
		Store:       application.Store,
		Pool:        application.Pool,
		MockBroker:  application.MockBroker,
		Provider:    mockProvider,
		EconData:    application.EconData,
		MarketClock: application.MarketClock,
		Clock:       application.Clock,
		Log:         log,
	})
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Vantage development data loaded.")
	fmt.Println()
	fmt.Println("  DEVELOPMENT ONLY credentials — these accounts exist solely on this machine")
	fmt.Println("  and must never be created in any deployed environment:")
	fmt.Println()
	for _, u := range result.Users {
		fmt.Printf("    %-8s  %-28s  %s\n", u.Role, u.Email, u.Password)
	}
	fmt.Println()
	fmt.Printf("  Paper account : %s (%s %s simulated)\n",
		result.AccountName, result.StartingBalance, result.Currency)
	fmt.Printf("  Instruments   : %d seeded, %s is the primary market\n",
		result.Instruments, result.PrimaryInstrument)
	fmt.Printf("  Historical    : %d bars across %d timeframes\n", result.Bars, result.Timeframes)
	fmt.Printf("  Strategies    : %d registered, %d promoted to PAPER\n",
		result.Strategies, result.PaperStrategies)
	fmt.Printf("  Calendar      : %d economic events, %d headlines\n", result.Events, result.News)
	fmt.Println()
	fmt.Println("  All balances and trades are SIMULATED. No funds exist and none can move.")
	fmt.Println()
	return nil
}

// runVerifyAudit recomputes the audit hash chain.
func runVerifyAudit(ctx context.Context) error {
	cfg, log, err := loadConfig()
	if err != nil {
		return err
	}
	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	events, err := application.Store.Control.AuditChainSlice(ctx, 1, 100000)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Println("audit chain is empty")
		return nil
	}

	ok, brokenAt := domain.VerifyChain(events)
	headHash, headSeq, err := application.Store.Control.AuditHead(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("events checked : %d\n", len(events))
	fmt.Printf("head sequence  : %d\n", headSeq)
	fmt.Printf("head hash      : %s\n", headHash)
	if !ok {
		fmt.Printf("VERIFICATION FAILED at index %d", brokenAt)
		if brokenAt >= 0 && brokenAt < len(events) {
			fmt.Printf(" (sequence %d, occurred %s)",
				events[brokenAt].Sequence, events[brokenAt].OccurredAt.UTC().Format(time.RFC3339))
		}
		fmt.Println()
		fmt.Println()
		fmt.Println("The audit log has been altered, deleted from, or reordered since it was written.")
		return fmt.Errorf("audit chain verification failed")
	}
	fmt.Println("verified       : yes")
	fmt.Println()
	fmt.Println("Note: hash chaining proves the log has not been altered in place. It does not")
	fmt.Println("prevent alteration by a party with database write access, and it cannot detect")
	fmt.Println("the truncation of the most recent entries unless the head hash is recorded")
	fmt.Println("off-box. See docs/SECURITY.md.")
	return nil
}

// runHealthcheck probes the local server. Used as the container healthcheck, so
// it depends on nothing but the HTTP endpoint.
func runHealthcheck(ctx context.Context) error {
	addr := os.Getenv("VANTAGE_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health/ready", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness returned %d: %s", resp.StatusCode, string(body))
	}
	fmt.Println(string(body))
	return nil
}
