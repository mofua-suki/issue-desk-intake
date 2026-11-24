// Package appcutover holds the PostgreSQL-side flag that says the application
// tables (consultations, sessions, settings, ...) live in PostgreSQL. It has no
// SQLite dependency, so the intake binary can read it at startup. The switch-over
// (2026-11-04) is done; the flag stays as the startup check.
package appcutover

import (
	"context"
	"database/sql"
)

// FlagTable is the switch-over flag: one row means the application tables are
// authoritative in PostgreSQL. It sits in the migration schema, so it exists only
// after a migration has been verified and committed.
const FlagTable = "issue_desk_app_migration.app_store_cutover"

// Done reports whether the switch-over flag exists in PostgreSQL.
func Done(ctx context.Context, db *sql.DB) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('`+FlagTable+`') IS NOT NULL`).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+FlagTable).Scan(&rows); err != nil {
		return false, err
	}
	return rows > 0, nil
}
