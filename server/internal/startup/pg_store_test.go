package startup

import (
	"context"
	"fmt"
	"os"
	"testing"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

// このパッケージの試験は本番と同じPostgreSQL方言で動かす。
func TestMain(m *testing.M) {
	appsql.SetActive(appsql.Postgres)
	code := m.Run()
	if recoveryBuildDir != "" {
		if err := os.RemoveAll(recoveryBuildDir); err != nil {
			fmt.Fprintln(os.Stderr, "remove recovery CLI:", err)
			code = 1
		}
	}
	os.Exit(code)
}

// openAppStore opens the application store on the throwaway schema of env.
// Opening it twice on the same env reads the state the first handle saved.
func openAppStore(t testing.TB, env pgtest.Env) *store.SQLite {
	t.Helper()
	s, err := store.OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newAppStore opens the application store on a fresh PostgreSQL schema (the test
// is skipped when ISSUE_DESK_TEST_POSTGRES_DSN is unset).
func newAppStore(t testing.TB) *store.SQLite {
	t.Helper()
	return openAppStore(t, pgtest.NewIsolated(t))
}

// useApplicationSchema points OpenApp (and Run) at a throwaway application schema
// of the PostgreSQL test server instead of the production one.
func useApplicationSchema(t testing.TB, config *Config) pgtest.Env {
	t.Helper()
	env := pgtest.NewIsolated(t)
	config.AppPostgresDSN, config.ApplicationSchema = env.DSN, env.Schema
	return env
}
