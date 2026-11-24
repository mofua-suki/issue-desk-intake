package anomalies

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"issue-desk-intake/server/internal/store"
	"issue-desk-intake/server/internal/usage"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func testPostgres(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("tests require an explicitly named issue_desk_test database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var database string
	if err = admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil || !strings.HasPrefix(database, "issue_desk_test") {
		admin.Close()
		t.Fatal("test database verification failed")
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "anomalies_test_" + hex.EncodeToString(id[:])
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("test schema cleanup failed: %v", err)
		}
		admin.Close()
	})
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func TestPostgresReadOnlyAnomaliesUsesOldIssueHistory(t *testing.T) {
	db, ctx := testPostgres(t)
	rows := []map[string]any{}
	for _, at := range []time.Time{clock.Add(-60 * 24 * time.Hour), clock.Add(-45 * 24 * time.Hour), clock.Add(-time.Hour)} {
		r := row(at, 1, "実装", 10)
		r["outcome"] = "failed"
		rows = append(rows, r)
	}
	usageStore := usage.Store{DB: db}
	result, e := usageStore.AddRows(ctx, "nas", rows)
	if e != nil || result.Rejected != 0 {
		t.Fatal(result, e)
	}
	s := Service{DB: db, Now: func() time.Time { return clock }}
	var before int
	db.QueryRowContext(ctx, "SELECT count(*) FROM usage_rows").Scan(&before)
	out, e := s.Detect(ctx, clock.Add(-24*time.Hour), clock)
	if e != nil {
		t.Fatal(e)
	}
	hits := out["anomalies"].([]map[string]any)
	if find(hits, "failure") == nil {
		raw, _ := json.Marshal(out)
		t.Fatal(string(raw))
	}
	var after int
	db.QueryRowContext(ctx, "SELECT count(*) FROM usage_rows").Scan(&after)
	if after != before {
		t.Fatal("read-only detection changed stored usage")
	}
	if len(out["limitForecast"].([]map[string]any)) != 4 {
		t.Fatal(out)
	}
}
