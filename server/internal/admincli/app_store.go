package admincli

import (
	"context"
	"errors"
	"os"

	"issue-desk-intake/server/internal/startup"
	"issue-desk-intake/server/internal/store"
)

// appPostgresSchema overrides the application schema (empty means the default); tests use it.
var appPostgresSchema string

// openAppStore opens the application database the same way the running intake
// does: always PostgreSQL (ISSUE_DESK_DATABASE_URL or PG*), with the shared SQL
// dialect switched to PostgreSQL. It refuses to run against a database that has
// not been migrated (the intake would not start on it either).
// Tests replace it to run the commands on a throwaway schema.
var openAppStore = func(ctx context.Context, c *startup.Config) (*store.SQLite, error) {
	// The NAS wrapper snapshots the active writer's environment. The store is
	// PostgreSQL only now, so any other expectation (an old writer still on
	// SQLite, or an unreadable value) is refused before a database is opened.
	if expected := os.Getenv("ISSUE_DESK_EXPECTED_APP_STORE"); expected != "" && expected != "postgres" {
		return nil, errors.New("窓口が確認した受付の保存先がPostgreSQLではありません。受付はPostgreSQL専用です。NASの受付と一時キー窓口を更新してください")
	}
	return startup.OpenPostgresApplicationStore(ctx, c, appPostgresSchema)
}
