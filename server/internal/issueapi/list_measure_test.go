package issueapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type measuredDB struct {
	ns                atomic.Int64
	calls             atomic.Int64
	issueQueries      atomic.Int64
	dependencyQueries atomic.Int64
}
type measureDriver struct {
	base  driver.Driver
	stats *measuredDB
}

func (d measureDriver) Open(name string) (driver.Conn, error) {
	c, e := d.base.Open(name)
	if e != nil {
		return nil, e
	}
	return &measureConn{Conn: c, stats: d.stats}, nil
}

type measureConn struct {
	driver.Conn
	stats *measuredDB
}

func (c *measureConn) CheckNamedValue(v *driver.NamedValue) error {
	if x, ok := c.Conn.(driver.NamedValueChecker); ok {
		return x.CheckNamedValue(v)
	}
	return driver.ErrSkip
}
func (c *measureConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	x, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	start := time.Now()
	r, e := x.ExecContext(ctx, q, a)
	c.stats.ns.Add(int64(time.Since(start)))
	c.stats.calls.Add(1)
	return r, e
}
func (c *measureConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	x, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if strings.Contains(q, "FROM jobs WHERE repo=$1 AND issue=$2") {
		c.stats.issueQueries.Add(1)
	}
	if strings.Contains(q, "FROM job_deps d JOIN jobs p") {
		c.stats.dependencyQueries.Add(1)
	}
	start := time.Now()
	r, e := x.QueryContext(ctx, q, a)
	c.stats.ns.Add(int64(time.Since(start)))
	c.stats.calls.Add(1)
	if e != nil {
		return nil, e
	}
	return &measureRows{Rows: r, stats: c.stats}, nil
}
func (c *measureConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	start := time.Now()
	var tx driver.Tx
	var err error
	if x, ok := c.Conn.(driver.ConnBeginTx); ok {
		tx, err = x.BeginTx(ctx, o)
	} else {
		return nil, fmt.Errorf("measured PostgreSQL connection does not support ConnBeginTx")
	}
	c.stats.ns.Add(int64(time.Since(start)))
	if err != nil {
		return nil, err
	}
	return &measureTx{Tx: tx, stats: c.stats}, nil
}

type measureTx struct {
	driver.Tx
	stats *measuredDB
}

func (t *measureTx) Commit() error {
	start := time.Now()
	err := t.Tx.Commit()
	t.stats.ns.Add(int64(time.Since(start)))
	return err
}
func (t *measureTx) Rollback() error {
	start := time.Now()
	err := t.Tx.Rollback()
	t.stats.ns.Add(int64(time.Since(start)))
	return err
}

type measureRows struct {
	driver.Rows
	stats *measuredDB
}

func (r *measureRows) Close() error {
	start := time.Now()
	err := r.Rows.Close()
	r.stats.ns.Add(int64(time.Since(start)))
	return err
}
func (r *measureRows) Next(v []driver.Value) error {
	start := time.Now()
	e := r.Rows.Next(v)
	r.stats.ns.Add(int64(time.Since(start)))
	return e
}

type timedHistory struct {
	API
	elapsed atomic.Int64
}

func (a *timedHistory) Issues(ctx context.Context, repo, state string) ([]Object, error) {
	start := time.Now()
	issues, e := a.API.Issues(ctx, repo, state)
	a.elapsed.Add(int64(time.Since(start)))
	return issues, e
}

// legacyList reproduces the original list data flow, including the per-row
// baseline reads and the post-assembly number assignment.
func legacyList(ctx context.Context, s *Service) ([]Object, error) {
	out := []Object{}
	lists := []numbers.List{}
	repos := s.repositories()
	for _, repo := range repos {
		if e := s.o.Project(repo); e != nil {
			return nil, e
		}
		issues, e := s.o.Gitea.Issues(ctx, repo, "all")
		if e != nil {
			return nil, e
		}
		l := numbers.List{Project: repo}
		for _, issue := range issues {
			if coordination(repo, issue) {
				continue
			}
			n, ok := number(issue["number"])
			if ok {
				l.Issues = append(l.Issues, numbers.Candidate{Number: n, CreatedAt: fleet.Text(issue["created_at"]), PullRequest: issue["pull_request"] != nil})
			}
			author, e := s.requestAuthorChecked(issue)
			if e != nil {
				return nil, e
			}
			if issue["pull_request"] != nil || !author {
				continue
			}
			var started time.Time
			timer, measuring := ctx.Value(listRowTimerKey{}).(func(time.Duration))
			if measuring {
				started = time.Now()
			}
			r, _, e := s.row(ctx, repo, issue)
			if measuring {
				timer(time.Since(started))
			}
			if e != nil {
				return nil, e
			}
			out = append(out, r)
		}
		lists = append(lists, l)
	}
	if _, e := s.o.Numbers.Assign(ctx, lists, repos, true); e != nil {
		return nil, e
	}
	for _, r := range out {
		n, _ := number(r["number"])
		if d, found, e := s.o.Numbers.Number(ctx, fleet.Text(r["project"]), n); e != nil {
			return nil, e
		} else if found {
			r["desk"] = d
		}
	}
	if e := s.recordCloseBaseline(ctx, out); e != nil {
		return nil, e
	}
	sort.SliceStable(out, func(i, j int) bool { return fleet.Text(out[i]["updatedAt"]) > fleet.Text(out[j]["updatedAt"]) })
	return out, nil
}

type listRowTimerKey struct{}

func TestListMeasurementAndCompatibilityPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement is optional")
	}
	f := newFixture(t)
	tx, e := f.app.DB().Begin()
	if e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= 6702; i++ {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
		if _, e = tx.Exec(`INSERT INTO drafts(id,json) VALUES(?,?)`, id, `{"project":"owner/repo"}`); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= 4436; i++ {
		issue := clone(f.api.issue)
		issue["number"] = i
		issue["body"] = fmt.Sprintf("<!-- issue-desk:%08x-1111-4111-8111-111111111111 -->", i)
		if i > 100 {
			issue["state"] = "closed"
			issue["closed_at"] = "2026-11-01T00:00:00Z"
		}
		f.api.issues = append(f.api.issues, issue)
	}
	// Check both lists before adding jobs: most issues have no job yet.
	for attempt := 1; attempt <= 2; attempt++ {
		rows, err := f.s.list(f.ctx)
		if err != nil || len(rows) != 4436 {
			t.Fatalf("list attempt %d rows=%d error=%v", attempt, len(rows), err)
		}
	}
	// Add the active and closed jobs used by both comparison paths.
	_, e = f.pg.Exec(`INSERT INTO jobs(repo,issue,title,kind,priority,filed_at,scope,scope_source,provider,tier,status,view,updated_at) SELECT repo,n,title,kind,priority,filed_at,scope,scope_source,provider,tier,CASE WHEN n<=100 THEN 'queued' ELSE 'closed' END,view,updated_at FROM jobs CROSS JOIN generate_series(2,4436) n WHERE id=$1`, f.job)
	if e != nil {
		t.Fatal(e)
	}
	stats := &measuredDB{}
	var schema string
	if e = f.pg.QueryRow(`SELECT current_schema()`).Scan(&schema); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	postgresName := fmt.Sprintf("issue_measure_postgres_%d", time.Now().UnixNano())
	sql.Register(postgresName, measureDriver{f.pg.Driver(), stats})
	pg, e := sql.Open(postgresName, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer pg.Close()
	j, e := jobs.New(pg, jobs.Options{AppDB: f.app.DB()})
	if e != nil {
		t.Fatal(e)
	}
	f.s.o.Postgres = pg
	f.s.o.Jobs = j
	history := &timedHistory{API: f.api}
	f.s.o.Gitea = history
	var rowTime time.Duration
	measuredCtx := context.WithValue(f.ctx, listRowTimerKey{}, func(d time.Duration) { rowTime += d })
	measure := func(label string, run func() ([]Object, error)) []Object {
		rowTime = 0
		stats.ns.Store(0)
		stats.calls.Store(0)
		history.elapsed.Store(0)
		start := time.Now()
		rows, e := run()
		elapsed := time.Since(start)
		if e != nil {
			t.Fatal(e)
		}
		db := time.Duration(stats.ns.Load())
		t.Logf("%s total=%v gitea=%v rows_including_db=%v db=%v other_excluding_db=%v db_calls=%d", label, elapsed, time.Duration(history.elapsed.Load()), rowTime, db, elapsed-time.Duration(history.elapsed.Load())-db, stats.calls.Load())
		return rows
	}
	old := measure("before_list", func() ([]Object, error) { return legacyList(measuredCtx, f.s) })
	if stats.issueQueries.Load() < 4436 || stats.dependencyQueries.Load() < 100 {
		t.Fatal("legacy queries were not measured")
	}
	stats.issueQueries.Store(0)
	stats.dependencyQueries.Store(0)
	now := measure("after_list", func() ([]Object, error) { return f.s.list(measuredCtx) })
	if stats.issueQueries.Load() != 0 || stats.dependencyQueries.Load() != 1 {
		t.Fatalf("batch query counts: per-issue=%d dependencies=%d", stats.issueQueries.Load(), stats.dependencyQueries.Load())
	}
	if !reflect.DeepEqual(old, now) {
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(now)
		t.Fatalf("list differs before=%d bytes after=%d bytes equal=%v", len(a), len(b), string(a) == string(b))
	}
	oldSearch := measure("before_search", func() ([]Object, error) {
		rows, e := legacyList(measuredCtx, f.s)
		if e != nil {
			return nil, e
		}
		out := []Object{}
		for _, r := range rows {
			if r["state"] == "open" {
				out = append(out, r)
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			a, b := fleet.Text(out[i]["updatedAt"]), fleet.Text(out[j]["updatedAt"])
			if a == b {
				ai, _ := number(out[i]["number"])
				bi, _ := number(out[j]["number"])
				return ai < bi
			}
			return a > b
		})
		return out, nil
	})
	newSearch := measure("after_search", func() ([]Object, error) {
		result, e := f.s.search(measuredCtx, Object{"state": "open", "limit": "100"}, auth.Principal{})
		if e != nil {
			return nil, e
		}
		if result["total"] != 100 || result["hasMore"] != false {
			t.Fatalf("search metadata %v", result)
		}
		return result["issues"].([]Object), nil
	})
	for _, r := range oldSearch {
		delete(r, "dispatchRecord")
		delete(r, "executionSettings")
	}
	if !reflect.DeepEqual(oldSearch, newSearch) {
		t.Fatal("search differs")
	}
	count, e := f.s.search(f.ctx, Object{"state": "open", "countOnly": "1"}, auth.Principal{})
	if e != nil || !reflect.DeepEqual(count, Object{"total": len(oldSearch)}) {
		t.Fatalf("count: %v %v", count, e)
	}
	page, e := f.s.search(f.ctx, Object{"state": "open", "limit": "10", "page": "2"}, auth.Principal{})
	if e != nil {
		t.Fatal(e)
	}
	if page["total"] != len(oldSearch) || page["hasMore"] != true || !reflect.DeepEqual(page["issues"], oldSearch[10:20]) {
		t.Fatal("page differs")
	}
	closedExpected := []Object{}
	for _, row := range old {
		if row["state"] == "closed" {
			cloned := clone(row)
			delete(cloned, "dispatchRecord")
			delete(cloned, "executionSettings")
			closedExpected = append(closedExpected, cloned)
		}
	}
	sort.SliceStable(closedExpected, func(i, j int) bool {
		a, _ := number(closedExpected[i]["number"])
		b, _ := number(closedExpected[j]["number"])
		return a < b
	})
	closedResult, e := f.s.search(f.ctx, Object{"state": "closed", "limit": "10"}, auth.Principal{})
	if e != nil {
		t.Fatal(e)
	}
	expectedJSON, _ := json.Marshal(closedExpected[:10])
	actualJSON, _ := json.Marshal(closedResult["issues"])
	if closedResult["total"] != len(closedExpected) || closedResult["hasMore"] != true || string(expectedJSON) != string(actualJSON) {
		t.Fatal("closed search differs")
	}
	started := time.Now()
	status, body := f.call("GET", "/api/issues/search?state=open&limit=100", "nas", nil)
	t.Logf("http_search=%v", time.Since(started))
	if status != 200 || fleet.ObjectOrEmpty(body)["total"] != float64(100) {
		t.Fatalf("HTTP search status=%d", status)
	}
	started = time.Now()
	status, body = f.call("GET", "/api/issues", "nas", nil)
	t.Logf("http_list=%v", time.Since(started))
	if status != 200 || len(fleet.Items(body)) != 4436 {
		t.Fatalf("HTTP list status=%d", status)
	}
}

// TestListReadMeasurement compares sequential and bounded reads without a timing threshold.
func TestListReadMeasurement(t *testing.T) {
	repos := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	var calls atomic.Int64
	read := func(ctx context.Context, repo, state string) ([]Object, error) {
		calls.Add(1)
		select {
		case <-time.After(5 * time.Millisecond):
			return []Object{{"number": 1, "repo": repo}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	run := func(parallel bool) ([]Object, time.Duration, uint64, int64) {
		calls.Store(0)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		result := []Object{}
		var reads *listReads
		if parallel {
			reads = newListReads(context.Background(), repos, func(string) error { return nil }, read)
		}
		for _, repo := range repos {
			var rows []Object
			var err error
			if parallel {
				reads.fill()
				rows, err = reads.next()
			} else {
				rows, err = read(context.Background(), repo, "all")
			}
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, rows...)
		}
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		return result, elapsed, after.TotalAlloc - before.TotalAlloc, calls.Load()
	}
	before, bt, ba, bc := run(false)
	after, at, aa, ac := run(true)
	if !reflect.DeepEqual(before, after) || bc != 8 || ac != 8 {
		t.Fatalf("results or calls differ: %v %v", bc, ac)
	}
	t.Logf("8 repos, 1 issue/repo, 5ms/read: sequential=%v calls=%d allocated=%dB; parallel=%v calls=%d allocated=%dB", bt, bc, ba, at, ac, aa)
}
