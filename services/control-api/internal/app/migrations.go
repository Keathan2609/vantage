package app

import (
	"context"

	"github.com/vantage/control-api/internal/db"
)

// PendingMigrationCount reports how many schema migrations the binary carries
// that the database has not applied.
//
// The serving process checks this and refuses to start when it is non-zero. A
// service running against a schema it does not recognise fails in ways that
// look like data corruption rather than like the deployment mistake it is.
func (a *App) PendingMigrationCount(ctx context.Context) (int, error) {
	pending, err := db.PendingMigrations(ctx, a.Pool)
	if err != nil {
		return 0, err
	}
	return len(pending), nil
}
