package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReworkUsesSweepLeaseExpiredEventPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	store, err := jobs.New(db, jobs.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	err = db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,generation,view,result) VALUES('owner/expiry',1,'running','nas',7,$1,1,'{}','{"error":"固定テストが不合格"}') RETURNING id`, now.Add(-time.Minute)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,host,data) VALUES($1,'leased',$2,'nas','{"epoch":7,"generation":1}')`, id, now.Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	row := `{"repo":"owner/expiry","issue":1,"host":"nas","generation":1,"step":"実装","at":"2026-11-01T01:00:00Z","outcome":"completed"}`
	_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES('expiry','owner/expiry',1,$1,$2,'nas','nas')`, now.Add(-2*time.Hour), row)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err = store.Sweep(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{DB: db, Now: func() time.Time { return now }}
	result, err := service.ReworkBreakdown(ctx, url.Values{"from": {"2026-11-01"}, "to": {"2026-11-01"}}, []string{"owner/expiry"})
	if err != nil {
		t.Fatal(err)
	}
	implementation := fleet.Object(fleet.Object(result)["implementation"])
	if fleet.Number(implementation["runs"]) != 1 || fleet.Number(fleet.Object(implementation["reasons"])["fixed-test"]) != 1 {
		t.Fatalf("sweep event was not attributed: %v", implementation)
	}
	if _, err = db.ExecContext(ctx, `UPDATE job_events SET data=jsonb_set(data,'{leaseHost}','"other"') WHERE job_id=$1 AND kind='lease_expired'`, id); err != nil {
		t.Fatal(err)
	}
	result, err = service.ReworkBreakdown(ctx, url.Values{"from": {"2026-11-01"}, "to": {"2026-11-01"}}, []string{"owner/expiry"})
	if err != nil {
		t.Fatal(err)
	}
	implementation = fleet.Object(fleet.Object(result)["implementation"])
	if fleet.Number(fleet.Object(implementation["reasons"])["unknown"]) != 1 || fleet.Number(fleet.Object(implementation["reasons"])["fixed-test"]) != 0 {
		t.Fatalf("another host was attributed: %v", implementation)
	}
}

func TestReworkSnapshotUsesLatestMergeAndCutsOffLaterEvidencePostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	var id int64
	if e := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,kind,pr,status,result,filed_at,view)VALUES('owner/repo',1,'followup',10,'merged','{"mergeCommit":"abc"}','2026-01-01','{"tierHistory":[{"from":"light","to":"heavy"}]}')RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	for _, ev := range []struct{ at, kind, data string }{{"2026-10-31T23:00:00Z", "leased", `{"epoch":1,"generation":0}`}, {"2026-11-01T01:00:00Z", "state", `{"to":"merged","epoch":1}`}, {"2026-11-01T02:00:00Z", "control", `{"action":"retry","fromBase":true}`}} {
		if _, e := db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,host,data)VALUES($1,$2,$3,'nas',$4)`, id, ev.kind, ev.at, []byte(ev.data)); e != nil {
			t.Fatal(e)
		}
	}
	for index, at := range []string{"2026-11-01T00:00:00Z", "2026-11-01T03:00:00Z"} {
		row := AccountRecord{"repo": "owner/repo", "issue": 1, "source": "worker", "host": "nas", "at": at, "step": "実装", "provider": "codex", "generation": 1, "outcome": "completed", "tier": "standard", "inputTokens": float64(100), "outputTokens": float64(10), "cacheReadTokens": float64(0)}
		raw, _ := json.Marshal(row)
		if _, e := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,host,sender)VALUES($1,'owner/repo',1,$2,$3,'nas','nas')`, "row-"+string(rune('a'+index)), at, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,host,sender)VALUES('broken','owner/repo',1,'2026-11-01T00:30:00Z','broken json','nas','nas')`); e != nil {
		t.Fatal(e)
	}
	tx, e := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	_, period, e := ReworkQuery(url.Values{"from": {"2026-11-01"}, "to": {"2026-11-01"}}, time.Now(), []string{"owner/repo"})
	if e != nil {
		t.Fatal(e)
	}
	data, e := ReadReworkRows(ctx, tx, "owner/repo", period)
	if e != nil {
		t.Fatal(e)
	}
	if len(data.Jobs) != 1 || len(data.Events) != 2 || len(data.Usage) != 1 || data.Skipped != 1 {
		t.Fatalf("merge cutoff differs jobs=%d events=%d usage=%d skipped=%d", len(data.Jobs), len(data.Events), len(data.Usage), data.Skipped)
	}
	// A second connection moves the merge outside the requested interval. The
	// transaction must keep its original read-only population and evidence.
	if _, e = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data)VALUES($1,'state','2026-11-03T00:00:00Z','{"to":"merged"}')`, id); e != nil {
		t.Fatal(e)
	}
	repeat, e := ReadReworkRows(ctx, tx, "owner/repo", period)
	if e != nil || len(repeat.Jobs) != 1 {
		t.Fatalf("snapshot changed %v %v", repeat, e)
	}
	tx.Rollback()
	service := &Service{DB: db, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }}
	result, e := service.ReworkBreakdown(ctx, url.Values{"from": {"2026-11-01"}, "to": {"2026-11-01"}}, []string{"owner/repo"})
	if e != nil {
		t.Fatal(e)
	}
	if fleet.Object(fleet.Object(result)["breakdown"])["merged"] != 0 {
		t.Fatal("latest merge event was ignored")
	}
	empty := &Service{Now: service.Now}
	unknown, e := empty.ReworkBreakdown(ctx, url.Values{}, []string{"owner/repo"})
	if e != nil || fleet.Object(unknown)["breakdown"] != nil || len(fleet.List(fleet.Object(unknown)["unavailable"])) != 1 {
		t.Fatalf("missing DB became zero %v %v", unknown, e)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "broken json") {
		t.Fatal("raw database data leaked")
	}
}

type reworkQueryTracer struct{ queries []string }

func (t *reworkQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.queries = append(t.queries, data.SQL)
	return ctx
}
func (t *reworkQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestReworkAlertSkipsImplementationQueriesPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	tracer := &reworkQueryTracer{}
	cfg.Tracer = tracer
	observed := stdlib.OpenDB(*cfg)
	defer observed.Close()
	svc := &Service{DB: observed}
	full, err := svc.ReworkBreakdown(ctx, url.Values{}, []string{"owner/repo"})
	if err != nil || fleet.Object(full)["implementation"] == nil {
		t.Fatal(full, err)
	}
	fullQueries := len(tracer.queries)
	implementationQueries := append([]string(nil), tracer.queries[fullQueries-4:fullQueries-1]...)
	tracer.queries = nil
	alert, err := svc.ReworkBreakdownForAlerts(ctx, url.Values{}, []string{"owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fleet.Object(full)["breakdown"], fleet.Object(alert)["breakdown"]) {
		t.Fatal("breakdowns differ")
	}
	for _, implementationQuery := range implementationQueries {
		for _, query := range tracer.queries {
			if query == implementationQuery {
				t.Fatalf("implementation query was issued: %s", query)
			}
		}
	}
	if fullQueries-len(tracer.queries) != 3 {
		t.Fatalf("full queries %d alert queries %d", fullQueries, len(tracer.queries))
	}
	if _, err = db.ExecContext(ctx, "DROP TABLE job_events CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ReworkBreakdownForAlerts(ctx, url.Values{}, nil); err == nil {
		t.Fatal("read failure accepted")
	}
	unavailable, err := svc.ReworkBreakdown(ctx, url.Values{}, nil)
	if err != nil || len(fleet.List(fleet.Object(unavailable)["unavailable"])) != 1 {
		t.Fatal(unavailable, err)
	}
	observed.Close()
	if _, err = svc.ReworkBreakdownForAlerts(ctx, url.Values{}, nil); err == nil {
		t.Fatal("transaction failure accepted")
	}
}

func TestReworkAlertSyntheticComparisonPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	tracer := &reworkQueryTracer{}
	cfg.Tracer = tracer
	observed := stdlib.OpenDB(*cfg)
	defer observed.Close()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	svc := &Service{DB: observed, Now: func() time.Time { return now }}
	for _, size := range []int{99, 100, 101} {
		for _, extra := range []int{0, 500} {
			repo := fmt.Sprintf("synthetic/r%d_%d", size, extra)
			_, err = db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,kind,pr,status,result,filed_at,view) SELECT $1,n,'followup',10,CASE WHEN n<=$2+$3::int/2 THEN 'merged' ELSE 'queued' END,'{"mergeCommit":"abc"}','2026-10-01','{"tierHistory":[{"from":"heavy","to":"heavy"}]}' FROM generate_series(1,$2::int+$3::int)n`, repo, size, extra)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data) SELECT id,'state',CASE WHEN issue<=$2 THEN '2026-11-07T01:00:00Z'::timestamptz ELSE '2026-09-01T01:00:00Z'::timestamptz END,'{"to":"merged"}' FROM jobs WHERE repo=$1 AND status='merged'`, repo, size)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) SELECT $1||issue,repo,issue,'2026-11-07T00:10:00Z',json_build_object('repo',repo,'issue',issue,'at','2026-11-07T00:10:00Z','step','実装','tier','heavy','generation',1,'inputTokens',100,'outputTokens',10,'cacheReadTokens',0)::text,'nas','nas' FROM jobs WHERE repo=$1`, repo)
			if err != nil {
				t.Fatal(err)
			}
			values := url.Values{"project": {repo}}
			repos := []string{repo}
			var expected any
			for _, full := range []bool{true, false} {
				run := func() {
					var e error
					var result any
					if full {
						result, e = svc.ReworkBreakdown(ctx, values, repos)
					} else {
						result, e = svc.ReworkBreakdownForAlerts(ctx, values, repos)
					}
					if e == nil {
						if full {
							expected = fleet.Object(result)["breakdown"]
						} else if !reflect.DeepEqual(expected, fleet.Object(result)["breakdown"]) {
							t.Fatal("breakdown mismatch")
						}
					}
					if e != nil {
						t.Fatal(e)
					}
				}
				run()
				tracer.queries = nil
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				start := time.Now()
				for i := 0; i < 3; i++ {
					run()
				}
				elapsed := time.Since(start)
				runtime.ReadMemStats(&after)
				t.Logf("population=%d extra_history=%d full=%t warmup=1 repetitions=3 queries=%d elapsed=%s allocated=%d", size, extra, full, len(tracer.queries), elapsed, after.TotalAlloc-before.TotalAlloc)
			}
		}
	}
}
