package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type usageReadTrace struct{ reads atomic.Int64 }

func (t *usageReadTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT json FROM usage_rows ") {
		t.reads.Add(1)
	}
	return ctx
}
func (*usageReadTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// BenchmarkUsageReviewAlertHTTP measures the authenticated HTTP endpoint against
// PostgreSQL. Run through scripts/with-test-postgres.ts, not with production data.
func BenchmarkUsageReviewAlertHTTP(b *testing.B) {
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		b.Fatal("isolated PostgreSQL required; use scripts/with-test-postgres.ts")
	}
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	trace := &usageReadTrace{}
	var local, usage *sql.DB
	handler, db, token := workerFixtureOptions(b, true, func(o *WorkerRouteOptions) {
		local = o.AppDB
		var schema string
		if err := o.DB.QueryRow("SELECT current_schema()").Scan(&schema); err != nil {
			b.Fatal(err)
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			b.Fatal(err)
		}
		cfg.RuntimeParams["search_path"] = schema
		cfg.Tracer = trace
		usage = sql.OpenDB(stdlib.GetConnector(*cfg))
		b.Cleanup(func() { usage.Close() })
		o.DB = usage
		o.Now = func() time.Time { return now }
	})
	for i := 1; i <= 100; i++ {
		raw, err := json.Marshal(map[string]any{"round": i, "draftId": fmt.Sprint("review-", i), "startedAt": "2026-10-01T15:00:00Z", "treatmentIssues": []any{map[string]any{"mergedAt": "2026-10-27T00:00:00Z"}}})
		if err != nil {
			b.Fatal(err)
		}
		if _, err = local.Exec("INSERT INTO usage_review_rounds(round,json) VALUES(?,?)", i, string(raw)); err != nil {
			b.Fatal(err)
		}
		if i <= 90 {
			if _, err = local.Exec("INSERT INTO usage_review_acks(round,at) VALUES(?,?)", i, "2026-11-01T00:00:00Z"); err != nil {
				b.Fatal(err)
			}
		}
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		at := now.Add(-time.Duration(i%700) * time.Hour).Format(time.RFC3339Nano)
		raw, err := json.Marshal(map[string]any{"at": at, "provider": "claude", "costUsd": float64(1), "repo": "owner/repo", "issue": i + 1})
		if err != nil {
			b.Fatal(err)
		}
		if _, err = tx.Exec("INSERT INTO usage_rows(key,at,host,source,repo,json,sender) VALUES($1,$2,'nas','intake','owner/repo',$3,'nas')", fmt.Sprint(i), at, string(raw)); err != nil {
			b.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	request := func() {
		r := httptest.NewRequest("GET", "http://localhost:8787/api/usage-review/alerts", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			b.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		var rows []any
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || rows == nil {
			b.Fatalf("invalid response %s: %v", w.Body.String(), err)
		}
	}
	request() // establish connections outside the timed region
	trace.reads.Store(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		request()
	}
	b.StopTimer()
	if trace.reads.Load() == 0 {
		b.Fatal("usage read was not observed")
	}
	b.ReportMetric(float64(trace.reads.Load())/float64(b.N), "usage-queries/op")
}
