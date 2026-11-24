package deskcleanup

import (
	"context"
	"os"
	"testing"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

// The production process runs on PostgreSQL (appsql.SetActive at startup), so
// the tests expand SQL for PostgreSQL as well.
func TestMain(m *testing.M) {
	appsql.SetActive(appsql.Postgres)
	os.Exit(m.Run())
}

// openStore opens the application store on the isolated test schema.
func openStore(t *testing.T, env pgtest.Env) *store.SQLite {
	t.Helper()
	db, err := store.OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
