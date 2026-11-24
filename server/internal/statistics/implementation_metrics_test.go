package statistics

import (
	"context"
	"database/sql"
	"fmt"

	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type implementationTrace struct {
	queries, jobQueries int
	dbTime              time.Duration
	dbBytes             uint64
	queriesSQL          []string
	beginSQL            string
	afterJobs           func()
	hookErr             error
}
type tracePoint struct {
	at          time.Time
	bytes       uint64
	selectQuery bool
}
type traceKey struct{}

func allocatedBytes() uint64 { var m runtime.MemStats; runtime.ReadMemStats(&m); return m.TotalAlloc }
func (trace *implementationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(data.SQL)), "begin") {
		trace.beginSQL = strings.ToLower(data.SQL)
	}
	if !strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") {
		return ctx
	}
	trace.queries++
	trace.queriesSQL = append(trace.queriesSQL, data.SQL)
	if strings.Contains(data.SQL, "FROM jobs WHERE id") {
		trace.jobQueries++
	}
	return context.WithValue(ctx, traceKey{}, tracePoint{time.Now(), allocatedBytes(), true})
}
func (trace *implementationTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if p, ok := ctx.Value(traceKey{}).(tracePoint); ok && p.selectQuery {
		trace.dbTime += time.Since(p.at)
		trace.dbBytes += allocatedBytes() - p.bytes
		if trace.afterJobs != nil && trace.queries == 1 {
			hook := trace.afterJobs
			trace.afterJobs = nil
			hook()
		}
	}
}

func TestPostgresImplementationWorkReduction(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,view,result) SELECT 'o/r',n,'merged',$1::timestamptz - interval '1 day','{}','{}' FROM generate_series(1,600) n`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'host','leased',$1::timestamptz-interval '1 hour','{"epoch":1,"generation":1}'::jsonb FROM jobs UNION ALL SELECT id,'host','state',$1::timestamptz+interval '2 hours','{"epoch":1,"to":"merged"}'::jsonb FROM jobs WHERE issue <= 300 UNION ALL SELECT id,'host','info',$1::timestamptz+interval '3 hours','{}'::jsonb FROM jobs`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) SELECT 'u'||issue||'-'||n,'o/r',issue,$1::timestamptz+n*interval '1 hour','実装',jsonb_build_object('repo','o/r','issue',issue,'at',to_char(($1::timestamptz+n*interval '1 hour') AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),'step','実装','host','host','generation',1,'outcome','completed')::text,'host','test' FROM jobs CROSS JOIN generate_series(0,1) n`, at)
	if err != nil {
		t.Fatal(err)
	}
	// Include histories unrelated to an implementation run: an independent review,
	// projectless usage, a row for an absent issue, and another job's event.
	_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES
		('review','o/r',1,$1,'独立レビュー',jsonb_build_object('repo','o/r','issue',1,'at',to_char($1::timestamptz AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),'step','独立レビュー')::text,'host','test'),
		('account',NULL,NULL,$1,'account','{}','host','test'),
		('unrelated','other/repo',999,$1,'実装',jsonb_build_object('repo','other/repo','issue',999,'at',to_char($1::timestamptz AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),'step','実装')::text,'host','test')`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'other','info',$1,'{}' FROM jobs WHERE issue=600`, at)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	trace := &implementationTrace{}
	trace.afterJobs = func() {
		_, trace.hookErr = db.ExecContext(ctx, `DELETE FROM usage_rows WHERE repo='o/r'`)
	}
	cfg.Tracer = trace
	traced := sql.OpenDB(stdlib.GetConnector(*cfg))
	defer traced.Close()
	traced.SetMaxOpenConns(1)
	if err = traced.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	now := at.Add(72 * time.Hour)
	s := &Service{DB: traced, Now: func() time.Time { return now }}
	q := Query{Before: Period{at, at.Add(24 * time.Hour)}, After: Period{at.Add(24 * time.Hour), at.Add(48 * time.Hour)}}
	value, err := s.Implementation(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	before := value.(map[string]any)["before"].(map[string]any)
	after := value.(map[string]any)["after"].(map[string]any)
	t.Logf("runs before=%v after=%v queries=%d jobQueries=%d", before["runs"], after["runs"], trace.queries, trace.jobQueries)
	if before["runs"] != 600 || after["runs"] != 0 {
		t.Fatalf("unexpected runs: before=%v after=%v", before["runs"], after["runs"])
	}
	if len(before["issues"].([]any)) != 600 || before["reasons"].(map[string]any)["success"] != 300 || before["reasons"].(map[string]any)["unknown"] != 300 || before["unknownDetails"].(map[string]any)["reasonNotRecorded"] != 300 || before["noImplementationRecord"] != 0 || before["skipped"] != 0 {
		t.Fatalf("unexpected aggregate counts: reasons=%v details=%v", before["reasons"], before["unknownDetails"])
	}
	if trace.afterJobs != nil {
		t.Fatal("concurrent update did not run")
	}
	if !strings.Contains(trace.beginSQL, "repeatable read") || !strings.Contains(trace.beginSQL, "read only") {
		t.Fatalf("transaction mode: %s", trace.beginSQL)
	}
	if trace.hookErr != nil {
		t.Fatal(trace.hookErr)
	}
	if trace.queries != 3 {
		t.Fatalf("expected one read each of jobs, events and usage: %v", trace.queriesSQL)
	}
	for _, table := range []string{"FROM jobs j", "FROM job_events e", "FROM usage_rows u"} {
		n := 0
		for _, query := range trace.queriesSQL {
			if strings.Contains(query, table) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s read %d times: %v", table, n, trace.queriesSQL)
		}
	}
	if trace.jobQueries > 2 || trace.queries > 10 {
		t.Fatalf("too many SELECTs: total=%d job=%d", trace.queries, trace.jobQueries)
	}
}

func TestPostgresImplementationMinuteBoundaryAndProject(t *testing.T) {
	db, ctx := testPostgres(t)
	start := time.Date(2026, 11, 1, 23, 59, 0, 0, JST)
	end := start.Add(2 * time.Minute)
	for i := 1; i <= 5; i++ {
		repo := "o/r"
		if i == 5 {
			repo = "other/r"
		}
		var id int64
		if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,generation,view,result) VALUES($1,$2,'merged',1,'{}','{}') RETURNING id`, repo, i).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,host,at,data) VALUES($1,'leased','host',$2,'{"epoch":1}'),($1,'state','host',$3,'{"epoch":1,"to":"merged"}')`, id, start.Add(-time.Hour), end.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		at := start
		if i == 2 {
			at = start.Add(time.Minute)
		}
		if i == 3 {
			at = end
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES($1,$2,$3,$4,'実装',jsonb_build_object('repo',$2::text,'issue',$3::int,'at',$4::timestamptz,'step','実装','host','host','generation',1,'outcome','completed')::text,'host','test')`, fmt.Sprint(i), repo, i, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES('broken','o/r',4,$1,'実装','{','host','test')`, start); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: db, Now: func() time.Time { return end.Add(time.Hour) }}
	got, err := s.Implementation(ctx, Query{Project: "o/r", Before: Period{start, end}, After: Period{end, end.Add(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	before := got.(map[string]any)["before"].(map[string]any)
	days := before["days"].([]any)
	if before["runs"] != 3 || before["skipped"] != 1 || before["unknownIssues"] != 1 || len(days) != 2 || days[0].(map[string]any)["runs"] != 2 || days[1].(map[string]any)["runs"] != 1 {
		t.Fatalf("boundary, project or missing data: %v", before)
	}
	after := got.(map[string]any)["after"].(map[string]any)
	if after["runs"] != 1 {
		t.Fatalf("exclusive end: %v", after)
	}
}
