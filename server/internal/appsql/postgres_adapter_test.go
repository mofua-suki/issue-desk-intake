package appsql_test

import (
	"context"
	"errors"
	"testing"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

// The generated JSON / TableExists / write-lock SQL and the transaction adapter run
// on the production application schema (skipped without ISSUE_DESK_TEST_POSTGRES_DSN).
func TestPostgresJSONTableExistsAndTransactions(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO settings(id,json) VALUES('docs','{"a":{"b":"x","n":7}}')`); err != nil {
		t.Fatal(err)
	}
	text, _ := appsql.Postgres.JSONText("json", "$.a.b")
	number, _ := appsql.Postgres.JSONNumber("json", "$.a.n")
	var got string
	if err := db.QueryRowContext(ctx, "SELECT "+text+" FROM settings WHERE id='docs' AND "+number+"=$1", 7).Scan(&got); err != nil || got != "x" {
		t.Fatalf("json read failed: %q %v", got, err)
	}
	exists, _ := appsql.Postgres.TableExists()
	for name, want := range map[string]int{"settings": 1, "missing": 0} {
		var n int
		if err := db.QueryRowContext(ctx, exists, name).Scan(&n); err != nil || n != want {
			t.Fatalf("table exists wrong for %s: %d %v", name, n, err)
		}
	}
	begin, _ := appsql.Postgres.BeginWrite()
	lock, _ := appsql.Postgres.WriteLock()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{begin, lock, "COMMIT"} {
		args := []any{}
		if q == lock {
			args = append(args, "name")
		}
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	// Within commits on success and rolls back on callback failure.
	insert := func(tx appsql.Tx, id string) error {
		_, err := tx.Exec(ctx, "INSERT INTO settings(id,json) VALUES($1,'{}')", id)
		return err
	}
	store := appsql.SQLStore{DB: db}
	if err := appsql.Within(ctx, store, nil, func(tx appsql.Tx) error { return insert(tx, "kept") }); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("fail")
	if err := appsql.Within(ctx, store, nil, func(tx appsql.Tx) error {
		if err := insert(tx, "dropped"); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("callback error lost: %v", err)
	}
	for id, want := range map[string]int{"kept": 1, "dropped": 0} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE id=$1", id).Scan(&n); err != nil || n != want {
			t.Fatalf("%s: count %d %v", id, n, err)
		}
	}
}
