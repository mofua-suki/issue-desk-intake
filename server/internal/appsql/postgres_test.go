package appsql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPostgresGeneratedSQL(t *testing.T) {
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL test DSN is not configured")
	}
	u, e := url.Parse(dsn)
	if e != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("dedicated test database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal("database open failed")
	}
	defer admin.Close()
	var name string
	if e = admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&name); e != nil || !strings.HasPrefix(name, "issue_desk_test") {
		t.Fatal("database verification failed")
	}
	var id [8]byte
	if _, e = rand.Read(id[:]); e != nil {
		t.Fatal("random generation failed")
	}
	schema := "appsql_test_" + hex.EncodeToString(id[:])
	if _, e = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal("schema creation failed")
	}
	defer func() {
		c, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, e := admin.ExecContext(c, "DROP SCHEMA "+schema+" CASCADE"); e != nil {
			t.Error("schema cleanup failed")
		}
	}()
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	db, e := sql.Open("pgx", u.String())
	if e != nil {
		t.Fatal("database open failed")
	}
	defer db.Close()
	identity, _ := Postgres.Identity()
	binary, _ := Postgres.Binary()
	if _, e = db.ExecContext(ctx, "CREATE TABLE items (id "+identity+", data "+binary+" NOT NULL, source_rowid BIGINT UNIQUE, CHECK (source_rowid > 0))"); e != nil {
		t.Fatal("table creation failed")
	}
	q, e := Postgres.InsertIgnore("items", []string{"data", "source_rowid"}, "VALUES (?, ?)")
	if e != nil {
		t.Fatal("SQL generation failed")
	}
	for i := 0; i < 2; i++ {
		if _, e = db.ExecContext(ctx, q, []byte{1}, int64(1)); e != nil {
			t.Fatal("insert failed")
		}
	}
	if _, e = db.ExecContext(ctx, q, []byte{2}, int64(-1)); e == nil {
		t.Fatal("CHECK violation was ignored")
	}
	selectSQL, e := Postgres.InsertIgnore("items", []string{"data", "source_rowid"}, "SELECT data, source_rowid FROM items WHERE source_rowid = ?")
	if e != nil {
		t.Fatal("SQL generation failed")
	}
	if _, e = db.ExecContext(ctx, selectSQL, 1); e != nil {
		t.Fatal("insert select failed")
	}
	order, _ := Postgres.RowOrder(true)
	var count int
	if e = db.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT * FROM items ORDER BY "+order+") AS ordered").Scan(&count); e != nil || count != 1 {
		t.Fatal("unexpected result")
	}
	for _, value := range []int64{3, 2} {
		if _, e = db.ExecContext(ctx, q, []byte{1}, value); e != nil {
			t.Fatal("insert failed")
		}
	}
	for _, desc := range []bool{false, true} {
		order, _ := Postgres.RowOrder(desc)
		rows, err := db.QueryContext(ctx, "SELECT source_rowid FROM items ORDER BY "+order)
		if err != nil {
			t.Fatal("query failed")
		}
		var values []int64
		for rows.Next() {
			var v int64
			if rows.Scan(&v) != nil {
				t.Fatal("scan failed")
			}
			values = append(values, v)
		}
		if rows.Err() != nil {
			t.Fatal("read failed")
		}
		rows.Close()
		want := []int64{1, 2, 3}
		if desc {
			want = []int64{3, 2, 1}
		}
		if !reflect.DeepEqual(values, want) {
			t.Fatal("unexpected ordering")
		}
	}

}
