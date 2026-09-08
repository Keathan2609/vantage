// Command control-api is the Vantage control plane.
//
// The control plane owns everything that decides whether money is allowed to
// move: authentication, authorisation, trading authority, kill switches, risk
// enforcement, order management, broker interaction, the portfolio ledger,
// reconciliation and audit. It is deliberately smaller and more deterministic
// than the research plane, and it is the only component with a path to a
// broker adapter.
//
// This build executes PAPER orders against an in-process mock broker. There is
// no live adapter compiled in; see internal/config for the three independent
// gates that enforce that.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata" // embed the IANA database: market sessions must resolve on Windows too

	// Embeds the IANA time zone database in the binary. The market clock
	// resolves zone names (New York, for the FX and metals calendar) at
	// construction, and the deployed image is `scratch` with no
	// /usr/share/zoneinfo to read. Without this the platform would fail to
	// start rather than silently use UTC — but paying ~450KB to remove the
	// filesystem dependency entirely is the better trade.
	_ "time/tzdata"

	"github.com/vantage/control-api/internal/config"
	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/logging"
)

const usage = `vantage-control-api - Vantage control plane

This build executes simulated PAPER orders against a mock venue. No live broker
adapter is compiled in and no real funds can move.

Usage:
  control-api serve         Run the HTTP API (default)
  control-api migrate       Apply pending schema migrations, then exit
  control-api migrate-status Show applied and pending migrations
  control-api seed          Load deterministic development data
  control-api verify-audit  Recompute and verify the audit hash chain
  control-api healthcheck   Probe the local server (used by the container healthcheck)
`

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = runServe(ctx)
	case "migrate":
		err = runMigrate(ctx)
	case "migrate-status":
		err = runMigrateStatus(ctx)
	case "seed":
		err = runSeed(ctx)
	case "verify-audit":
		err = runVerifyAudit(ctx)
	case "healthcheck":
		err = runHealthcheck(ctx)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		// Configuration failures are reported plainly: they happen before the
		// logger exists, and an operator reading a crash loop needs the reason
		// on the first line.
		fmt.Fprintf(os.Stderr, "vantage: %s failed: %v\n", cmd, err)
		os.Exit(1)
	}
}

// loadConfig reads configuration and builds the logger.
func loadConfig() (config.Config, *logging.Logger, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, err
	}
	log := logging.New(logging.Options{
		Level:   cfg.LogLevel,
		Service: "control-api",
		Env:     string(cfg.Env),
	})
	return cfg, log, nil
}

func runMigrate(ctx context.Context) error {
	cfg, log, err := loadConfig()
	if err != nil {
		return err
	}

	pool, err := db.Open(ctx, cfg.MigrationDatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	start := time.Now()
	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		log.Info("schema is up to date")
		return nil
	}
	for _, m := range applied {
		log.Info("migration applied",
			"version", m.Version, "name", m.Name, "checksum", m.Checksum[:12])
	}
	log.Info("migrations complete", "count", len(applied), "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func runMigrateStatus(ctx context.Context) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.MigrationDatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := db.AppliedMigrations(ctx, pool)
	if err != nil {
		return err
	}
	pending, err := db.PendingMigrations(ctx, pool)
	if err != nil {
		return err
	}

	fmt.Printf("applied (%d):\n", len(applied))
	for _, a := range applied {
		fmt.Printf("  %04d  %-28s  %s  %s\n",
			a.Version, a.Name, a.Checksum[:12], a.AppliedAt.UTC().Format(time.RFC3339))
	}
	fmt.Printf("pending (%d):\n", len(pending))
	for _, p := range pending {
		fmt.Printf("  %04d  %-28s  %s\n", p.Version, p.Name, p.Checksum[:12])
	}
	if len(pending) > 0 {
		return errors.New("schema is not up to date")
	}
	return nil
}
