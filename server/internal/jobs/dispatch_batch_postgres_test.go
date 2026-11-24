package jobs

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type dispatchQueryTrace struct {
	count, running, dependencies atomic.Int64
}

func (t *dispatchQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.count.Add(1)
	if strings.Contains(data.SQL, "FROM jobs WHERE status='running'") {
		t.running.Add(1)
	}
	if strings.Contains(data.SQL, "FROM job_deps d JOIN jobs") {
		t.dependencies.Add(1)
	}
	return ctx
}
func (*dispatchQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type dispatchLeaseBarrier struct {
	reached chan struct{}
	resume  chan struct{}
}

func (b *dispatchLeaseBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "UPDATE jobs SET status='running',lease_host=") {
		close(b.reached)
		select {
		case <-b.resume:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*dispatchLeaseBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPostgresDispatchPreservesConcurrentDependencyMerges(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	const repo = "mofua-suki/issue-desk-intake"
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,view) VALUES
 (2,$1,2,'queued',ARRAY['src/a.go'],'declared','{"phase":"implementation","dependencyMerges":{"1":{"pr":10}}}')`, repo); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	barrier := &dispatchLeaseBarrier{reached: make(chan struct{}), resume: make(chan struct{})}
	cfg.Tracer = barrier
	traced := stdlib.OpenDB(*cfg)
	defer traced.Close()
	s, err := New(traced, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		job *Job
		err error
	}
	done := make(chan result, 1)
	a := balanceAssignments(t)
	go func() {
		job, _, err := s.Next(ctx, "gt12", 4, a)
		done <- result{job, err}
	}()
	select {
	case <-barrier.reached:
	case r := <-done:
		t.Fatalf("dispatch ended before lease update: job=%v err=%v", r.job, r.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Reconciliation attempts its update while dispatch holds the cycle lock.
	updateDone := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{dependencyMerges}',view->'dependencyMerges' || '{"1":{"pr":12,"mergeCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"3":{"pr":13}}'::jsonb) WHERE id=2 /* concurrent reconciliation test */`)
		updateDone <- err
	}()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%/* concurrent reconciliation test */')`).Scan(&waiting); err != nil {
			close(barrier.resume)
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-updateDone:
			close(barrier.resume)
			t.Fatalf("reconciliation completed before dispatch: %v", err)
		case <-ctx.Done():
			close(barrier.resume)
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(barrier.resume)
	r := <-done
	if r.err != nil || r.job == nil || r.job.ID != 2 || r.job.Status != "running" {
		t.Fatalf("dispatch job=%v err=%v", r.job, r.err)
	}
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
	var commit string
	var pr int
	var phase, status string
	if err := db.QueryRowContext(ctx, `SELECT view->'dependencyMerges'->'1'->>'mergeCommit',(view->'dependencyMerges'->'3'->>'pr')::int,view->>'phase',status FROM jobs WHERE id=2`).Scan(&commit, &pr, &phase, &status); err != nil {
		t.Fatal(err)
	}
	if commit != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || pr != 13 || phase != "implementation" || status != "running" {
		t.Fatalf("persisted commit=%s pr=%d phase=%s status=%s", commit, pr, phase, status)
	}
}

func TestPostgresFullHostBatchDispatch(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	const repo = "mofua-suki/issue-desk-intake"
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,lease_host,lease_epoch,lease_expires_at) SELECT n,'other/repo',n,'running','gt12',1,$1 FROM generate_series(1,4) n`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	trace := &dispatchQueryTrace{}
	cfg.Tracer = trace
	traced := stdlib.OpenDB(*cfg)
	defer traced.Close()
	traced.SetMaxOpenConns(1)
	s, err := New(traced, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	a := balanceAssignments(t)
	job, lease, err := s.Next(ctx, "gt12", 4, a)
	if err != nil || job != nil || lease != nil {
		t.Fatalf("empty cycle job=%v lease=%v err=%v", job, lease, err)
	}
	if running, dependencies := trace.running.Load(), trace.dependencies.Load(); running != 0 || dependencies != 0 {
		t.Fatalf("empty cycle running selects=%d dependency selects=%d", running, dependencies)
	}
	var previous int64
	for _, n := range []int{10, 1000} {
		if _, err = db.ExecContext(ctx, `DELETE FROM job_events`); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `DELETE FROM jobs WHERE status='queued'`); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source) SELECT 100+n,$1,100+n,'queued',ARRAY['src/a.go'],'declared' FROM generate_series(1,$2::int) n`, repo, n); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) SELECT 100+n,1 FROM generate_series(1,$1::int/2) n`, n); err != nil {
			t.Fatal(err)
		}
		trace.count.Store(0)
		start := time.Now()
		job, _, err := s.Next(ctx, "gt12", 4, a)
		elapsed := time.Since(start)
		count := trace.count.Load()
		if err != nil || job != nil {
			t.Fatalf("job=%v err=%v", job, err)
		}
		var records, dependencies int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE data->>'reason'='no_slot'), count(*) FILTER (WHERE data->>'reason'='dependency') FROM job_events WHERE kind='dispatch_skip'`).Scan(&records, &dependencies); err != nil || records != n/2 || dependencies != n/2 {
			t.Fatalf("slots=%d dependencies=%d want each=%d err=%v", records, dependencies, n/2, err)
		}
		t.Logf("queued=%d queries=%d duration=%s", n, count, elapsed)
		if count > 30 {
			t.Fatalf("too many queries for %d queued jobs: %d", n, count)
		}
		if previous > 0 && count != previous {
			t.Fatalf("query count grew: %d to %d", previous, count)
		}
		previous = count
		trace.count.Store(0)
		job, _, err = s.Next(ctx, "gt12", 4, a)
		if err != nil || job != nil {
			t.Fatalf("repeat job=%v err=%v", job, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE kind='dispatch_skip'`).Scan(&records); err != nil || records != n {
			t.Fatalf("dedup records=%d err=%v", records, err)
		}
	}
}

func TestPostgresUnresolvedDependencyDoesNotReserveScope(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	const repo = "mofua-suki/issue-desk-intake"
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,scope,scope_source,view) VALUES
 (1,$1,1,'queued',0,$2,ARRAY['src/'],'declared','{"dependsOnIssues":[{"repo":"mofua-suki/issue-desk-intake","issue":99}]}'),
 (2,$1,2,'queued',0,$3,ARRAY['src/file.go'],'declared','{"phase":"implementation"}')`, repo, at.Add(-time.Hour), at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := s.Next(ctx, "gt12", 4, balanceAssignments(t))
	if err != nil || job == nil || job.ID != 2 {
		t.Fatalf("job=%v err=%v", job, err)
	}
	var epoch, generation int64
	var expires, phase string
	if err = db.QueryRowContext(ctx, `SELECT (data->>'epoch')::bigint,(data->>'generation')::bigint,data->>'expiresAt',data->>'phase' FROM job_events WHERE job_id=$1 AND kind='leased'`, job.ID).Scan(&epoch, &generation, &expires, &phase); err != nil {
		t.Fatal(err)
	}
	if epoch != job.LeaseEpoch || generation != job.Generation || job.LeaseExpiresAt == nil || expires != *job.LeaseExpiresAt || phase != job.View["phase"] {
		t.Fatalf("leased: epoch=%d generation=%d expires=%s phase=%s job=%+v", epoch, generation, expires, phase, job)
	}
	var status string
	var reservations int
	if err = db.QueryRowContext(ctx, `SELECT status,(SELECT count(*) FROM job_events WHERE job_id=2 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved') FROM jobs WHERE id=1`).Scan(&status, &reservations); err != nil || status != "queued" || reservations != 0 {
		t.Fatalf("older status=%s reservations=%d err=%v", status, reservations, err)
	}
}
