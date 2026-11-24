package store

import (
	"context"
	"testing"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

// The application store contract runs on a throwaway PostgreSQL schema holding the
// production application schema (internal/pgtest). The test skips itself when
// ISSUE_DESK_TEST_POSTGRES_DSN is unset; no schema is written by hand here.
func TestApplicationContractOnPostgres(t *testing.T) {
	checkApplicationContract(t, applicationSQLite(pgtest.New(t), appsql.Postgres))
}

func TestApplicationConstraintsOnPostgres(t *testing.T) {
	checkApplicationConstraintContract(t, applicationSQLite(pgtest.New(t), appsql.Postgres))
}

func TestApplicationDesktopConcurrentOnPostgres(t *testing.T) {
	s := applicationSQLite(pgtest.New(t), appsql.Postgres)
	checkApplicationDesktopConcurrent(t, s, s)
}

// openPostgresApplication opens a new store (own connection pool) on the schema.
func openPostgresApplication(t *testing.T, env pgtest.Env) *SQLite {
	t.Helper()
	s, err := OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
