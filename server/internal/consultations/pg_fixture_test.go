package consultations

import (
	"context"
	"os"
	"testing"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

// このパッケージの試験は本番と同じPostgreSQL方言で動かす。
func TestMain(m *testing.M) {
	appsql.SetActive(appsql.Postgres)
	os.Exit(m.Run())
}

// openPostgresStore opens the application store on a throwaway PostgreSQL schema
// (skips the test when ISSUE_DESK_TEST_POSTGRES_DSN is unset).
func openPostgresStore(t *testing.T) *store.SQLite {
	t.Helper()
	env := pgtest.NewIsolated(t)
	s, err := store.OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
