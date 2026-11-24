package appdb_test

import (
	"context"
	"sort"
	"testing"

	"issue-desk-intake/server/internal/appdb"
	"issue-desk-intake/server/internal/pgtest"
)

// The PostgreSQL schema is consistent with itself: every table exists with the
// shared source_rowid column, no column uses a 32-bit integer type, and
// CreateSchema can run again on an existing schema without changing it.
func TestCreatedSchemaIsSelfConsistent(t *testing.T) {
	env := pgtest.NewIsolated(t)
	db := env.DB
	rows, err := db.Query(`SELECT table_name, column_name, data_type, column_default FROM information_schema.columns WHERE table_schema=current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	tables := map[string]bool{}
	rowidDefault := map[string]bool{}
	for rows.Next() {
		var table, column, kind string
		var def *string
		if err := rows.Scan(&table, &column, &kind, &def); err != nil {
			t.Fatal(err)
		}
		tables[table] = true
		if kind == "integer" || kind == "smallint" {
			t.Errorf("%s.%s is %s; integers must be bigint", table, column, kind)
		}
		if column == "source_rowid" && def != nil && len(*def) > 0 {
			rowidDefault[table] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := appdb.TableNames()
	sort.Strings(want)
	for _, name := range want {
		if !tables[name] {
			t.Errorf("table %s was not created", name)
		}
		if !rowidDefault[name] {
			t.Errorf("table %s has no source_rowid default", name)
		}
	}
	if len(tables) != len(want) {
		t.Errorf("schema has %d tables, appdb.TableNames lists %d", len(tables), len(want))
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := appdb.CreateSchema(context.Background(), tx, env.Schema); err != nil {
		t.Fatalf("CreateSchema must be idempotent: %v", err)
	}
}
