package issueapi

import (
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/store"
	"os"
	"path/filepath"
	"testing"
)

// The NAS deployment uses the PostgreSQL application store. Exercise its
// connection and dialect in the fixture's isolated schema.
func TestFilteredSearchApplicationPostgres(t *testing.T) {
	f := newFixture(t)
	var schema string
	if err := f.pg.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	schema += "_app"
	if _, err := f.pg.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.pg.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Error(err)
		}
	})
	app, err := store.OpenApplicationPostgres(f.ctx, os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"), schema)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err = app.DB().Exec(`CREATE TABLE settings(id TEXT PRIMARY KEY,json TEXT NOT NULL); CREATE TABLE drafts(id TEXT PRIMARY KEY,json TEXT NOT NULL); CREATE TABLE close_reasons(id TEXT PRIMARY KEY,json TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	previous := appsql.SetActive(appsql.Postgres)
	defer appsql.SetActive(previous)
	n, err := numbers.OpenLedger(f.ctx, app.DB(), filepath.Join(t.TempDir(), "state-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := jobs.New(f.pg, jobs.Options{AppDB: app.DB()})
	if err != nil {
		t.Fatal(err)
	}
	f.s.o.AppDB = app.DB()
	f.s.o.Numbers = n
	f.s.o.Jobs = j
	closed := clone(f.api.issue)
	closed["number"] = 2
	closed["state"] = "closed"
	closed["closed_at"] = "2026-10-01T00:00:00Z"
	f.api.issues = []Object{clone(f.api.issue), closed}
	result, err := f.s.search(f.ctx, Object{"state": "open"}, auth.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	if result["total"] != 1 {
		t.Fatalf("result: %v", result)
	}
	baseline, err := f.s.closeBaseline(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.ObjectOrEmpty(baseline["issues"])["owner/repo#2"] != closed["closed_at"] {
		t.Fatalf("baseline: %v", baseline)
	}
	if _, found, err := n.Number(f.ctx, "owner/repo", 2); err != nil || !found {
		t.Fatalf("number: %v %v", found, err)
	}
	rows, err := f.s.list(f.ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d error=%v", len(rows), err)
	}
}
