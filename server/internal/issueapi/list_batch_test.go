package issueapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFilteredSearchPreservesBaselineAndRefreshesPostgres(t *testing.T) {
	f := newFixture(t)
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
		t.Fatalf("open: %v", result)
	}
	baseline, err := f.s.closeBaseline(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.ObjectOrEmpty(baseline["issues"])["owner/repo#2"] != closed["closed_at"] {
		t.Fatalf("baseline: %v", baseline)
	}
	if _, found, err := f.s.o.Numbers.Number(f.ctx, "owner/repo", 2); err != nil || !found {
		t.Fatalf("closed number: %v %v", found, err)
	}
	f.api.issues[0]["state"] = "closed"
	f.api.issues[0]["closed_at"] = "2026-11-02T00:00:00Z"
	f.api.issues[1]["state"] = "open"
	f.api.issues[1]["title"] = "更新済み"
	delete(f.api.issues[1], "closed_at")
	if _, err = f.pg.Exec(`UPDATE jobs SET status='running' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	result, err = f.s.search(f.ctx, Object{"state": "open"}, auth.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	issues := fleet.Items(result["issues"])
	if result["total"] != 1 || len(issues) != 1 || fleet.ObjectOrEmpty(issues[0])["title"] != "更新済み" {
		t.Fatalf("reopened: %v", result)
	}
	result, err = f.s.search(f.ctx, Object{"state": "closed"}, auth.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	issues = fleet.Items(result["issues"])
	if len(issues) != 1 || fleet.ObjectOrEmpty(issues[0])["status"] != "running" {
		t.Fatalf("job and closure: %v", result)
	}
}

type countingIssueAPI struct {
	*fakeAPI
	calls int
}

func (a *countingIssueAPI) Issues(ctx context.Context, repo, state string) ([]Object, error) {
	a.calls++
	return a.fakeAPI.Issues(ctx, repo, state)
}

func TestSearchHistoryCallsIndependentOfIssueCountPostgres(t *testing.T) {
	f := newFixture(t)
	api := &countingIssueAPI{fakeAPI: f.api}
	f.s.o.Gitea = api
	for _, size := range []int{10, 100} {
		f.api.issues = nil
		for i := 1; i <= size; i++ {
			issue := clone(f.api.issue)
			issue["number"] = i
			f.api.issues = append(f.api.issues, issue)
		}
		api.calls = 0
		result, err := f.s.search(f.ctx, Object{"state": "open", "countOnly": "1"}, auth.Principal{})
		if err != nil || result["total"] != size || api.calls != 1 {
			t.Fatalf("size %d: result=%v calls=%d err=%v", size, result, api.calls, err)
		}
	}
}

func TestListEmptyDoesNotReadConfigurationPostgres(t *testing.T) {
	f := newFixture(t)
	f.api.issues = []Object{}
	calls := 0
	f.s.o.ConfigReader = func() (Object, error) { calls++; return nil, fmt.Errorf("configuration unavailable") }
	rows, err := f.s.list(f.ctx)
	if err != nil || len(rows) != 0 || calls != 0 {
		t.Fatalf("rows=%v calls=%d err=%v", rows, calls, err)
	}
	f.api.issues = []Object{clone(f.api.issue)}
	if _, err = f.s.list(f.ctx); err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestListBatchProjectionPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`INSERT INTO jobs(repo,issue,title,kind,priority,filed_at,scope,scope_source,status,view,updated_at) SELECT repo,2,title,kind,priority,filed_at,scope,scope_source,'queued',view,updated_at FROM jobs WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.Exec(`INSERT INTO job_deps(job_id,depends_on) SELECT id,$1 FROM jobs WHERE repo='owner/repo' AND issue=2`, f.job); err != nil {
		t.Fatal(err)
	}
	one := clone(f.api.issue)
	two := clone(f.api.issue)
	two["number"] = 2
	two["body"] = "<!-- issue-desk:followup -->\n#1 の独立レビューで、"
	two["title"] = "#1 followup"
	f.api.issues = []Object{one, two}
	calls := 0
	f.s.o.ConfigReader = func() (Object, error) { calls++; return f.options.Config, nil }
	timerCalls := 0
	ctx := context.WithValue(f.ctx, listRowTimerKey{}, func(time.Duration) { timerCalls++ })
	rows, err := f.s.list(ctx)
	if timerCalls != 0 {
		t.Fatalf("production list invoked measurement hook %d times", timerCalls)
	}
	if err != nil || len(rows) != 2 || calls != 1 {
		t.Fatalf("rows=%v calls=%d err=%v", rows, calls, err)
	}
	var second Object
	for _, row := range rows {
		if n, _ := number(row["number"]); n == 2 {
			second = row
		}
	}
	if second["status"] != "dependency_waiting" || !strings.Contains(fleet.Text(second["dependencyReason"]), "PRマージ待ち") || !strings.Contains(fleet.Text(second["shownTitle"]), "Gitea番号 1") || second["desk"] == nil || fleet.ObjectOrEmpty(second["origin"])["sources"] == nil {
		t.Fatalf("second=%v", second)
	}
}

func TestListDraftProjectionMatchesDetailAndRefreshesPostgres(t *testing.T) {
	f := newFixture(t)
	id := "00000001-1111-4111-8111-111111111111"
	cases := []string{
		`{"project":"owner/repo","parentSplit":"parent","projectDefinition":{"name":"definition"},"attention":{"project":"owner/repo","number":2},"body":"large ignored text"}`,
		`{"project":"other/repo","proposal":{"source":"owner/repo#3"},"stallFix":{"project":"owner/repo","issue":4}}`,
		`{"project":null,"parentSplit":42,"proposal":[],"attention":{"project":false,"number":{}},"projectDefinition":[1,2]}`,
		`{}`,
	}
	issue := clone(f.api.issue)
	issue["body"] = "<!-- issue-desk:" + id + " -->"
	f.api.issues = []Object{issue, clone(issue)}
	f.api.issues[1]["number"] = 2
	if _, err := f.s.list(f.ctx); err != nil {
		t.Fatal(err)
	}
	p := instrumentDrafts(t, f)
	for _, raw := range cases {
		if _, err := f.app.DB().Exec(`INSERT INTO drafts(id,json) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET json=excluded.json`, id, raw); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{"state=open&limit=100", "state=open&limit=100&countOnly=1"} {
			var previous any
			for _, old := range []bool{true, false, false} {
				*p = draftProbe{old: old}
				status, result := f.call("GET", "/api/issues/search?"+query, "nas", nil)
				if status != 200 || (previous != nil && !reflect.DeepEqual(previous, result)) {
					t.Fatalf("draft %s query %s old=%v: status=%d previous=%v result=%v", raw, query, old, status, previous, result)
				}
				previous = result
			}
		}
		rows, err := f.s.list(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i, item := range f.api.issues {
			expected, _, err := f.s.row(f.ctx, "owner/repo", item)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rows[i], expected) {
				t.Fatalf("draft %s: list=%v detail row=%v", raw, rows[i], expected)
			}
		}
	}
	if _, err := f.app.DB().Exec(`DELETE FROM drafts WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.list(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected, _, err := f.s.row(f.ctx, "owner/repo", issue)
	if err != nil || !reflect.DeepEqual(rows[0], expected) {
		t.Fatalf("missing: %v %v %v", rows[0], expected, err)
	}
	if _, err = f.app.DB().Exec(`DROP TABLE drafts`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.list(f.ctx); err == nil {
		t.Fatal("draft read failure was ignored")
	}
}

// draftProbe observes the application database at the driver boundary. In old
// mode it substitutes one full-document SELECT per requested ID. This is an
// approximation; it does not include row-by-row service overhead or waits.
type draftProbe struct {
	metadataCalls, metadataRows                        map[string]int
	old                                                bool
	calls, fullCalls, rows, fullRows, bytes, fullBytes int
	elapsed                                            time.Duration
	payloads                                           []string
}
type draftProbeDriver struct {
	base driver.Driver
	p    *draftProbe
}

func (d draftProbeDriver) Open(name string) (driver.Conn, error) {
	c, e := d.base.Open(name)
	if e != nil {
		return nil, e
	}
	return &draftProbeConn{Conn: c, p: d.p}, nil
}

type draftProbeConn struct {
	driver.Conn
	p *draftProbe
}

func (c *draftProbeConn) CheckNamedValue(v *driver.NamedValue) error {
	if x, ok := c.Conn.(driver.NamedValueChecker); ok {
		return x.CheckNamedValue(v)
	}
	return driver.ErrSkip
}
func (c *draftProbeConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}
func (c *draftProbeConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c *draftProbeConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	x := c.Conn.(driver.QueryerContext)
	if c.p.metadataCalls != nil {
		for _, table := range []string{"close_reasons", "proposals"} {
			if strings.Contains(strings.ToLower(q), "from "+table) {
				c.p.metadataCalls[table]++
				rows, e := x.QueryContext(ctx, q, a)
				if e != nil {
					return nil, e
				}
				return &draftProbeRows{Rows: rows, p: c.p, table: table}, nil
			}
		}
	}
	draft := strings.Contains(strings.ToLower(q), "from drafts")
	if !draft {
		return x.QueryContext(ctx, q, a)
	}
	if c.p.old && strings.HasPrefix(q, "WITH selected AS MATERIALIZED") {
		result := &draftMemoryRows{}
		for _, arg := range a {
			start := time.Now()
			c.p.calls++
			c.p.fullCalls++
			rows, e := x.QueryContext(ctx, "SELECT id,json FROM drafts WHERE id=$1", []driver.NamedValue{{Ordinal: 1, Value: arg.Value}})
			if e != nil {
				return nil, e
			}
			values := make([]driver.Value, 2)
			e = rows.Next(values)
			if e == nil {
				result.values = append(result.values, values)
				c.p.rows++
				c.p.fullRows++
				c.p.bytes += len(asText(values[1]))
				c.p.fullBytes += len(asText(values[1]))
				c.p.payloads = append(c.p.payloads, asText(values[1]))
			} else if e != io.EOF {
				rows.Close()
				return nil, e
			}
			if e = rows.Close(); e != nil {
				return nil, e
			}
			c.p.elapsed += time.Since(start)
		}
		return result, nil
	}
	c.p.calls++
	start := time.Now()
	rows, e := x.QueryContext(ctx, q, a)
	c.p.elapsed += time.Since(start)
	if e != nil {
		return nil, e
	}
	return &draftProbeRows{Rows: rows, p: c.p}, nil
}
func asText(v driver.Value) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

type draftProbeRows struct {
	table string
	driver.Rows
	p *draftProbe
}

func (r *draftProbeRows) Next(v []driver.Value) error {
	start := time.Now()
	e := r.Rows.Next(v)
	if r.table != "" {
		if e == nil {
			r.p.metadataRows[r.table]++
		}
		return e
	}
	r.p.elapsed += time.Since(start)
	if e == nil {
		r.p.rows++
		if len(v) > 0 {
			r.p.payloads = append(r.p.payloads, asText(v[len(v)-1]))
		}
		n := 0
		for _, x := range v {
			n += len(asText(x))
		}
		r.p.bytes += n
	}
	return e
}

type draftMemoryRows struct {
	values [][]driver.Value
	i      int
}

func (r *draftMemoryRows) Columns() []string { return []string{"id", "json"} }
func (r *draftMemoryRows) Close() error      { return nil }
func (r *draftMemoryRows) Next(v []driver.Value) error {
	if r.i == len(r.values) {
		return io.EOF
	}
	copy(v, r.values[r.i])
	r.i++
	return nil
}
func instrumentDrafts(t *testing.T, f *fixture) *draftProbe {
	t.Helper()
	var schema string
	if e := f.app.DB().QueryRow(`SELECT current_schema()`).Scan(&schema); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	p := &draftProbe{}
	name := fmt.Sprintf("draft_probe_%d", time.Now().UnixNano())
	sql.Register(name, draftProbeDriver{base: f.app.DB().Driver(), p: p})
	db, e := sql.Open(name, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	f.s.o.AppDB = db
	return p
}

// The Gitea fixture is serialized once, then decoded on each simulated response.
type draftFixtureAPI struct {
	API
	raw       []byte
	elapsed   time.Duration
	readBytes int
}

func (a *draftFixtureAPI) Issues(_ context.Context, _ string, _ string) ([]Object, error) {
	start := time.Now()
	var out []Object
	e := json.Unmarshal(a.raw, &out)
	a.elapsed += time.Since(start)
	a.readBytes += len(a.raw)
	return out, e
}
func generateDraftFixture(t *testing.T, f *fixture, drafts, issues, open int) *draftFixtureAPI {
	t.Helper()
	// Three independent fields total 50 KiB per draft, plus the JSON envelope.
	_, e := f.app.DB().Exec(`INSERT INTO drafts(id,json) SELECT lpad(to_hex(n),8,'0')||'-1111-4111-8111-111111111111', json_build_object('project','owner/repo','body',repeat('b',17066),'conversation',repeat('c',17067),'documents',repeat('d',17067))::text FROM generate_series(1,$1) n`, drafts)
	if e != nil {
		t.Fatal(e)
	}
	items := make([]Object, 0, issues)
	for i := 1; i <= issues; i++ {
		v := clone(f.api.issue)
		v["number"] = i
		v["body"] = fmt.Sprintf("<!-- issue-desk:%08x-1111-4111-8111-111111111111 -->", i)
		if i > open {
			v["state"] = "closed"
			v["closed_at"] = "2026-11-01T00:00:00Z"
		}
		items = append(items, v)
	}
	raw, e := json.Marshal(items)
	if e != nil {
		t.Fatal(e)
	}
	api := &draftFixtureAPI{API: f.api, raw: raw}
	f.s.o.Gitea = api
	return api
}

func TestListDraftReadVolumePostgres(t *testing.T) {
	f := newFixture(t)
	generateDraftFixture(t, f, 30, 20, 20)
	p := instrumentDrafts(t, f)
	rows, e := f.s.list(f.ctx)
	if e != nil || len(rows) != 20 {
		t.Fatalf("list rows=%d error=%v", len(rows), e)
	}
	if p.calls != 1 || p.rows != 20 || p.bytes >= 20*1024 {
		t.Fatalf("unexpected draft transfer: %+v", p)
	}
	// The same observer must detect the former per-row full reads.
	*p = draftProbe{old: true}
	_, e = f.s.list(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	if p.fullCalls != 20 || p.fullRows != 20 || p.fullBytes < 20*50000 {
		t.Fatalf("legacy full reads not detected: %+v", p)
	}
}

type multiRepoFixture struct {
	API
	items  map[string][]Object
	errors map[string]error
}

func (a *multiRepoFixture) Issues(_ context.Context, repo, state string) ([]Object, error) {
	if e := a.errors[repo]; e != nil {
		return nil, e
	}
	return a.items[repo], nil
}
func TestListRepoErrorsAndNumberAssignmentPostgres(t *testing.T) {
	f := newFixture(t)
	one := clone(f.api.issue)
	one["body"] = "<!-- issue-desk:00000001-1111-4111-8111-111111111111 -->"
	other := clone(f.api.issue)
	other["number"] = 2
	api := &multiRepoFixture{API: f.api, items: map[string][]Object{"owner/repo": {one}, "owner/other": {other}}, errors: map[string]error{}}
	f.s.o.Gitea = api
	f.s.o.Repositories = func() []string { return []string{"owner/repo", "owner/other"} }
	f.s.o.Project = func(string) error { return nil }
	p := instrumentDrafts(t, f)
	for _, old := range []bool{true, false} {
		*p = draftProbe{old: old}
		result, e := f.s.search(f.ctx, Object{"state": "open", "project": "owner/repo", "countOnly": "1"}, auth.Principal{})
		if e != nil || result["total"] != 1 {
			t.Fatalf("old=%v result=%v error=%v", old, result, e)
		}
		if _, ok, e := f.s.o.Numbers.Number(f.ctx, "owner/other", 2); e != nil || !ok {
			t.Fatalf("other repo number missing: %v %v", ok, e)
		}
	}
	// Compare list and search pages with equal timestamps across repositories.
	third := clone(one)
	third["number"] = 3
	api.items["owner/repo"] = []Object{third, one}
	queryCases := []Object{{"state": "open", "limit": "1", "page": "1"}, {"state": "open", "limit": "1", "page": "2"}, {"state": "open", "limit": "1", "page": "3"}, {"state": "open", "countOnly": "1"}, {"state": "open", "project": "owner/other"}}
	if _, e := f.s.list(f.ctx); e != nil {
		t.Fatal(e)
	}
	for _, query := range queryCases {
		*p = draftProbe{old: true}
		before, e := f.s.search(f.ctx, clone(query), auth.Principal{})
		if e != nil {
			t.Fatal(e)
		}
		*p = draftProbe{}
		after, e := f.s.search(f.ctx, clone(query), auth.Principal{})
		if e != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("query=%v before=%v after=%v error=%v", query, before, after, e)
		}
	}
	*p = draftProbe{old: true}
	before, e := f.s.list(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	*p = draftProbe{}
	after, e := f.s.list(f.ctx)
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("list before=%v after=%v error=%v", before, after, e)
	}
	api.items["owner/repo"] = []Object{one}
	// Both failures are present: the first repository's draft must fail before
	// the second repository's external response is requested.
	if _, e := f.app.DB().Exec(`INSERT INTO drafts(id,json) VALUES($1,$2)`, "00000001-1111-4111-8111-111111111111", "bad json"); e != nil {
		t.Fatal(e)
	}
	api.errors["owner/other"] = fmt.Errorf("later repository error")
	for _, old := range []bool{true, false} {
		*p = draftProbe{old: old}
		_, e := f.s.list(f.ctx)
		if e == nil || strings.Contains(e.Error(), "later repository error") {
			t.Fatalf("old=%v priority=%v", old, e)
		}
	}
}

func TestListDraftErrorOrderWithinRepositoryPostgres(t *testing.T) {
	f := newFixture(t)
	id1 := "00000001-1111-4111-8111-111111111111"
	id2 := "00000002-1111-4111-8111-111111111111"
	first := clone(f.api.issue)
	first["body"] = "<!-- issue-desk:" + id1 + " -->"
	second := clone(first)
	second["number"] = 2
	second["body"] = "<!-- issue-desk:" + id2 + " -->"
	f.api.issues = []Object{first, second}
	for _, entry := range []struct{ id, raw string }{{id1, `{"project":"owner/repo","proposal":{"source":"owner/repo#3","kind":"improvements","id":"x"}}`}, {id2, `@invalid`}} {
		if _, e := f.app.DB().Exec(`INSERT INTO drafts(id,json) VALUES($1,$2)`, entry.id, entry.raw); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := f.app.DB().Exec(`INSERT INTO proposals(id,json) VALUES($1,$2)`, "owner/repo#3:x", "!invalid"); e != nil {
		t.Fatal(e)
	}
	p := instrumentDrafts(t, f)
	var before string
	for _, old := range []bool{true, false} {
		*p = draftProbe{old: old}
		_, e := f.s.list(f.ctx)
		if e == nil || !strings.Contains(e.Error(), "'!'") {
			t.Fatalf("old=%v error=%v", old, e)
		}
		if old {
			before = e.Error()
		} else if e.Error() != before {
			t.Fatalf("before=%q after=%q", before, e)
		}
	}
}

func TestListReadsBoundedOrderedAndCancelled(t *testing.T) {
	repos := []string{"a", "b", "c", "d", "e", "f"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, len(repos))
	release := make(chan struct{})
	var active, maximum atomic.Int32
	read := func(ctx context.Context, repo, state string) ([]Object, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		started <- repo
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if repo == "a" || repo == "b" {
			return nil, fmt.Errorf("error %s", repo)
		}
		return []Object{{"repo": repo}}, nil
	}
	reads := newListReads(ctx, repos, func(string) error { return nil }, read)
	for i := 0; i < listReadLimit; i++ {
		<-started
	}
	if maximum.Load() != listReadLimit {
		t.Fatalf("maximum=%d", maximum.Load())
	}
	select {
	case repo := <-started:
		t.Fatalf("unexpected read %s", repo)
	default:
	}
	close(release)
	_, err := reads.next()
	if err == nil || err.Error() != "error a" {
		t.Fatalf("first error: %v", err)
	}
	_, err = reads.next()
	if err == nil || err.Error() != "error b" {
		t.Fatalf("second error: %v", err)
	}
	cancel()
	reads.fill()
	if _, err = reads.next(); err != context.Canceled {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestListReadsCancelInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, listReadLimit)
	finished := make(chan struct{}, listReadLimit)
	read := func(ctx context.Context, _ string, _ string) ([]Object, error) {
		started <- struct{}{}
		<-ctx.Done()
		finished <- struct{}{}
		return nil, ctx.Err()
	}
	reads := newListReads(ctx, []string{"a", "b", "c", "d", "e"}, func(string) error { return nil }, read)
	for i := 0; i < listReadLimit; i++ {
		<-started
	}
	cancel()
	if _, err := reads.next(); err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
	for i := 0; i < listReadLimit; i++ {
		<-finished
	}
	select {
	case <-started:
		t.Fatal("started unnecessary read")
	default:
	}
}

type historyTransport func(*http.Request) (*http.Response, error)

func (f historyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestListReadsRealHistoryColdWarmAndErrors(t *testing.T) {
	token := filepath.Join(t.TempDir(), "test-token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	transport := historyTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `[{"number":1,"updated_at":"2026-01-01T00:00:00Z"}]`
		if fail.Load() && strings.Contains(r.URL.Path, "/b/") {
			status, body = 404, `{}`
		}
		if r.URL.Query().Get("since") != "" && status == 200 {
			body = `[]`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	makeClient := func() *gitea.Client {
		c, err := gitea.New("http://example.test", token, transport)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	serial, parallel := makeClient(), makeClient()
	repos := []string{"owner/a", "owner/b", "owner/c"}
	run := func(c *gitea.Client, concurrent bool) ([]Object, string) {
		result := []Object{}
		var reads *listReads
		if concurrent {
			reads = newListReads(context.Background(), repos, func(string) error { return nil }, c.Issues)
		}
		for _, repo := range repos {
			var rows []Object
			var err error
			if concurrent {
				reads.fill()
				rows, err = reads.next()
			} else {
				rows, err = c.Issues(context.Background(), repo, "all")
			}
			if err != nil {
				return nil, err.Error()
			}
			result = append(result, rows...)
		}
		return result, ""
	}
	for _, phase := range []string{"cold", "warm", "error"} {
		if phase == "error" {
			fail.Store(true)
		}
		old, oldErr := run(serial, false)
		current, newErr := run(parallel, true)
		if !reflect.DeepEqual(old, current) || oldErr != newErr || (phase == "error" && oldErr == "") {
			t.Fatalf("%s: old=%v %q current=%v %q", phase, old, oldErr, current, newErr)
		}
	}
}

func TestJobUpdateAppearsInListSearchAndDetailPostgres(t *testing.T) {
	f := newFixture(t)
	one := clone(f.api.issue)
	two := clone(one)
	two["number"] = 2
	two["updated_at"] = "2026-11-02T00:00:00Z"
	f.api.issues = []Object{one, two}
	check := func(path string, want []float64, stamp string) {
		t.Helper()
		code, value := f.call("GET", path, "nas", nil)
		rows := fleet.Items(value)
		if strings.Contains(path, "/search?") {
			rows = fleet.Items(fleet.ObjectOrEmpty(value)["issues"])
		}
		if code != 200 || len(rows) != 2 {
			t.Fatalf("%s: %d %v", path, code, value)
		}
		for i, n := range want {
			if fleet.ObjectOrEmpty(rows[i])["number"] != n {
				t.Fatalf("%s: %v", path, value)
			}
		}
		for _, v := range rows {
			if fleet.ObjectOrEmpty(v)["number"] == float64(1) && fleet.ObjectOrEmpty(v)["updatedAt"] != stamp {
				t.Fatalf("%s: %v", path, v)
			}
		}
	}
	if _, err := f.pg.Exec(`UPDATE jobs SET progress_at='2026-11-03 00:00:00+00', updated_at='2026-11-01 00:00:00+00' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	check("/api/issues", []float64{1, 2}, "2026-11-03T00:00:00.000000000Z")
	check("/api/issues/search?sort=updated&order=asc", []float64{2, 1}, "2026-11-03T00:00:00.000000000Z")
	entry, err := f.s.ListEntry(f.ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	code, detail := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "nas", nil)
	if code != 200 || fleet.ObjectOrEmpty(detail)["updatedAt"] != entry["updatedAt"] || entry["updatedAt"] != "2026-11-03T00:00:00.000000000Z" {
		t.Fatalf("entry=%v detail=%v code=%d", entry, detail, code)
	}
	if _, err = f.pg.Exec(`UPDATE jobs SET progress_at=NULL, updated_at='2026-11-01 12:00:00+00' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	check("/api/issues", []float64{2, 1}, "2026-11-01T12:00:00.000000000Z")
	check("/api/issues/search?sort=updated&order=desc", []float64{2, 1}, "2026-11-01T12:00:00.000000000Z")
}

func TestListUpdateTimestampCandidatesPostgres(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		upstream      any
		progress      any
		updated, want string
	}{
		{"2026-11-01T01:00:00+01:00", nil, "2026-11-01T00:00:00Z", "2026-11-01T00:00:00.000000000Z"},
		{"2026-11-01T00:00:00.9Z", "2026-11-01T00:00:00.10Z", "2026-11-01T00:00:00Z", "2026-11-01T00:00:00.900000000Z"},
		{"invalid", nil, "2026-11-01T00:00:00Z", "2026-11-01T00:00:00.000000000Z"},
	}
	for _, tc := range cases {
		f.api.issue["updated_at"] = tc.upstream
		if _, err := f.pg.Exec(`UPDATE jobs SET progress_at=$1,updated_at=$2 WHERE id=$3`, tc.progress, tc.updated, f.job); err != nil {
			t.Fatal(err)
		}
		code, value := f.call("GET", "/api/issues", "nas", nil)
		if code != 200 || fleet.ObjectOrEmpty(fleet.Items(value)[0])["updatedAt"] != tc.want {
			t.Fatalf("case=%+v code=%d value=%v", tc, code, value)
		}
	}
	if _, err := f.pg.Exec(`DELETE FROM jobs WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{nil, "invalid", "2026-11-01T01:00:00+01:00"} {
		f.api.issue["updated_at"] = value
		code, result := f.call("GET", "/api/issues", "nas", nil)
		want := value
		if value == "2026-11-01T01:00:00+01:00" {
			want = "2026-11-01T00:00:00.000000000Z"
		}
		if code != 200 || fleet.ObjectOrEmpty(fleet.Items(result)[0])["updatedAt"] != want {
			t.Fatalf("input=%v result=%v", value, result)
		}
	}
}

func TestJobLifecycleUpdatesListPostgres(t *testing.T) {
	f := newFixture(t)
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	store, err := jobs.New(f.pg, jobs.Options{AppDB: f.app.DB(), Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}},"other":{"maxImplementations":1}},"repositories":{}}`), &raw); err != nil {
		t.Fatal(err)
	}
	assignments, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pg.Exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,1)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pg.Exec(`INSERT INTO jobs(repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at,view) VALUES('owner/repo',2,'running',ARRAY['other/'],'nas',1,$1,'{}')`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	job, _, err := store.Next(f.ctx, "nas", 1, assignments)
	if err != nil || job != nil {
		t.Fatalf("waiting job=%v err=%v", job, err)
	}
	var skips int
	if err = f.pg.QueryRow(`SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='dispatch_skip' AND data->>'reason'='no_slot'`, f.job).Scan(&skips); err != nil || skips != 1 {
		t.Fatalf("no_slot=%d err=%v", skips, err)
	}
	check := func(status, stamp string) {
		t.Helper()
		code, value := f.call("GET", "/api/issues", "nas", nil)
		row := fleet.ObjectOrEmpty(fleet.Items(value)[0])
		if code != 200 || row["status"] != status || row["updatedAt"] != stamp {
			t.Fatalf("status=%d row=%v", code, row)
		}
	}
	check("queued", "2026-11-01T00:00:00.000000000Z")
	if _, err = f.pg.Exec(`DELETE FROM jobs WHERE issue=2`); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Minute)
	job, _, err = store.Next(f.ctx, "nas", 1, assignments)
	if err != nil || job == nil {
		t.Fatalf("claim=%v err=%v", job, err)
	}
	check("running", "2026-11-02T00:01:00.000000000Z")
	at = at.Add(time.Minute)
	if _, _, err = store.Renew(f.ctx, f.job, "nas", job.LeaseEpoch, nil); err != nil {
		t.Fatal(err)
	}
	check("running", "2026-11-02T00:02:00.000000000Z")
	at = at.Add(time.Minute)
	reason := "question"
	if _, err = store.Report(f.ctx, f.job, "nas", jobs.Report{Epoch: job.LeaseEpoch, Status: "held", HoldReason: &reason}); err != nil {
		t.Fatal(err)
	}
	check("waiting", "2026-11-02T00:03:00.000000000Z")
}

func TestListDraftNULMatchesRowPostgres(t *testing.T) {
	f := newFixture(t)
	id := "00000001-1111-4111-8111-111111111111"
	issue := clone(f.api.issue)
	issue["body"] = "<!-- issue-desk:" + id + " -->"
	f.api.issues = []Object{issue}
	if err := f.s.save(f.ctx, "drafts", id, Object{"project": "owner/repo", "body": "before\x00after", "conversation": []any{"\x00"}, "documents": []any{"\x00"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.list(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := f.s.row(f.ctx, "owner/repo", issue)
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], row) {
		t.Fatalf("list=%v row=%v err=%v", rows, row, err)
	}
}

func TestListDraftMalformedMatchesRowErrorPostgres(t *testing.T) {
	f := newFixture(t)
	id := "00000001-1111-4111-8111-111111111111"
	issue := clone(f.api.issue)
	issue["body"] = "<!-- issue-desk:" + id + " -->"
	f.api.issues = []Object{issue}
	if _, err := f.app.DB().Exec(`INSERT INTO drafts(id,json) VALUES($1,$2)`, id, `bad json`); err != nil {
		t.Fatal(err)
	}
	_, _, rowErr := f.s.row(f.ctx, "owner/repo", issue)
	_, listErr := f.s.list(f.ctx)
	if rowErr == nil || listErr == nil || reflect.TypeOf(rowErr) != reflect.TypeOf(listErr) || rowErr.Error() != listErr.Error() {
		t.Fatalf("row=%T %v list=%T %v", rowErr, rowErr, listErr, listErr)
	}
	status, response := f.call("GET", "/api/issues", "nas", nil)
	if status != http.StatusServiceUnavailable || fleet.ObjectOrEmpty(response)["error"] != "Issueの処理を完了できません。" {
		t.Fatalf("status=%d response=%v", status, response)
	}
	t.Logf("row error=%q; HTTP status=%d response=%v", rowErr, status, response)
}

func TestListCloseReasonReadVolumePostgres(t *testing.T) {
	for _, size := range []int{5, 20, 501} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newFixture(t)
			generateDraftFixture(t, f, size, size, 0)
			if _, err := f.app.DB().Exec(`INSERT INTO close_reasons(id,json) SELECT 'owner/repo#'||n,json_build_object('reason','resolved','closedAt','2026-11-01T00:00:00Z')::text FROM generate_series(1,$1) n`, size); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.DB().Exec(`UPDATE drafts SET json=(json::jsonb || jsonb_build_object('proposal',jsonb_build_object('source','owner/repo#1','kind','improvements','id',id)))::text`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.DB().Exec(`INSERT INTO proposals(id,json) SELECT 'owner/repo#1:'||id,json_build_object('proposal',json_build_object('purpose','- owner/repo#1（PR: fixture）'))::text FROM drafts`); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"close_reasons", "proposals"} {
				// Unreferenced records must not be decoded or transferred to the list.
				if _, err := f.app.DB().Exec("INSERT INTO " + table + "(id,json) VALUES('unreferenced','invalid json')"); err != nil {
					t.Fatal(err)
				}
			}
			probe := instrumentDrafts(t, f)
			probe.metadataCalls, probe.metadataRows = map[string]int{}, map[string]int{}
			rows, err := f.s.list(f.ctx)
			if err != nil || len(rows) != size {
				t.Fatalf("size=%d rows=%d err=%v", size, len(rows), err)
			}
			for _, table := range []string{"close_reasons", "proposals"} {
				if probe.metadataCalls[table] != (size+499)/500 || probe.metadataRows[table] != size {
					t.Fatalf("size=%d %s queries=%d rows=%d", size, table, probe.metadataCalls[table], probe.metadataRows[table])
				}
			}
		})
	}
}

func TestListClosedRowsMatchDetailPostgres(t *testing.T) {
	for _, baseline := range []bool{false, true} {
		t.Run(fmt.Sprint(baseline), func(t *testing.T) {
			f := newFixture(t)
			items := make([]Object, 6)
			for i := range items {
				items[i] = clone(f.api.issue)
				items[i]["number"], items[i]["state"], items[i]["closed_at"] = i+1, "closed", "2026-11-01T00:00:00Z"
			}
			id := "11111111-1111-4111-8111-111111111111"
			items[4]["body"] = "<!-- issue-desk:" + id + " -->"
			items[5]["body"] = "<!-- issue-desk:followup -->\n#1 の独立レビューで、"
			f.api.issues = items
			if err := f.s.save(f.ctx, "drafts", id, Object{"project": "owner/repo", "proposal": Object{"source": "owner/repo#1", "kind": "improvements", "id": "batch"}}); err != nil {
				t.Fatal(err)
			}
			if err := f.s.save(f.ctx, "proposals", "owner/repo#1:batch", Object{"proposal": Object{"purpose": "- owner/repo#1（PR: fixture）"}}); err != nil {
				t.Fatal(err)
			}
			for n, v := range map[int]Object{
				1: {"reason": "duplicate", "source": "token", "closedAt": items[0]["closed_at"]},
				3: {"reason": "other_route", "route": Object{"repo": "owner/repo", "pr": 7}, "closedAt": items[2]["closed_at"]},
				4: {"reason": "resolved", "closedAt": "stale"},
			} {
				if err := f.s.save(f.ctx, "close_reasons", fmt.Sprintf("owner/repo#%d", n), v); err != nil {
					t.Fatal(err)
				}
			}
			if baseline {
				if err := f.s.recordCloseBaseline(f.ctx, []Object{{"project": "owner/repo", "number": 2, "state": "closed", "closedAt": items[1]["closed_at"]}}); err != nil {
					t.Fatal(err)
				}
			}
			compare := func() {
				t.Helper()
				expected := map[int64]Object{}
				for _, item := range items {
					row, _, err := f.s.row(f.ctx, "owner/repo", item)
					if err != nil {
						t.Fatal(err)
					}
					expected[int64Number(item["number"])] = row
				}
				rows, err := f.s.list(f.ctx)
				if err != nil || len(rows) != len(items) {
					t.Fatalf("rows=%v err=%v", rows, err)
				}
				for _, row := range rows {
					for _, key := range []string{"closeReason", "origin"} {
						if !reflect.DeepEqual(row[key], expected[int64Number(row["number"])][key]) {
							t.Fatalf("%v %s: list=%v detail=%v", row["number"], key, row[key], expected[int64Number(row["number"])][key])
						}
					}
				}
			}
			compare()
			// A new request must see changed records rather than a stale global cache.
			if err := f.s.save(f.ctx, "close_reasons", "owner/repo#1", Object{"reason": "resolved", "closedAt": items[0]["closed_at"]}); err != nil {
				t.Fatal(err)
			}
			if err := f.s.save(f.ctx, "proposals", "owner/repo#1:batch", Object{"proposal": Object{"purpose": "- owner/repo#2（PR: changed）"}}); err != nil {
				t.Fatal(err)
			}
			compare()
			// A terminal job can still display a close reason while Gitea's
			// Issue is open; batch selection must match the displayed status.
			items[0]["state"] = "open"
			delete(items[0], "closed_at")
			if _, err := f.pg.Exec(`UPDATE jobs SET status='closed' WHERE id=$1`, f.job); err != nil {
				t.Fatal(err)
			}
			if err := f.s.save(f.ctx, "close_reasons", "owner/repo#1", Object{"reason": "duplicate", "source": "token", "closedAt": nil}); err != nil {
				t.Fatal(err)
			}
			compare()
		})
	}
}

func TestListMetadataPreservesRowErrorOrderPostgres(t *testing.T) {
	f := newFixture(t)
	first, second := clone(f.api.issue), clone(f.api.issue)
	first["state"], first["closed_at"] = "closed", "2026-11-01T00:00:00Z"
	second["number"], second["body"] = 2, "<!-- issue-desk:11111111-1111-4111-8111-111111111111 -->"
	f.api.issues = []Object{first, second}
	if _, err := f.app.DB().Exec(`INSERT INTO close_reasons(id,json) VALUES('owner/repo#1','! first close record')`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.save(f.ctx, "drafts", "11111111-1111-4111-8111-111111111111", Object{"project": "owner/repo", "proposal": Object{"source": "owner/repo#1", "kind": "improvements", "id": "bad"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.DB().Exec(`INSERT INTO proposals(id,json) VALUES('owner/repo#1:bad','? later proposal record')`); err != nil {
		t.Fatal(err)
	}
	_, _, expected := f.s.row(f.ctx, "owner/repo", first)
	_, err := f.s.list(f.ctx)
	if expected == nil || err == nil || err.Error() != expected.Error() {
		t.Fatalf("list=%v detail=%v", err, expected)
	}
}

func TestListDependencyWaitingDoesNotReadCloseReasonsPostgres(t *testing.T) {
	for _, edge := range []bool{false, true} {
		t.Run(fmt.Sprint(edge), func(t *testing.T) {
			f := newFixture(t)
			var predecessor int64
			if err := f.pg.QueryRow(`INSERT INTO jobs(repo,issue,status) VALUES('owner/repo',2,'queued') RETURNING id`).Scan(&predecessor); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pg.Exec(`UPDATE jobs SET status='queued',view='{"dependsOnIssues":[{"repo":"owner/repo","issue":2}]}' WHERE id=$1`, f.job); err != nil {
				t.Fatal(err)
			}
			if edge {
				if _, err := f.pg.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)`, f.job, predecessor); err != nil {
					t.Fatal(err)
				}
			}
			issue := clone(f.api.issue)
			issue["state"], issue["closed_at"] = "closed", "2026-11-01T00:00:00Z"
			f.api.issues = []Object{issue}
			// This unused table is intentionally unavailable. Dependency-waiting rows
			// must remain readable without touching closure metadata.
			if _, err := f.app.DB().Exec(`ALTER TABLE close_reasons RENAME TO unavailable_close_reasons`); err != nil {
				t.Fatal(err)
			}
			probe := instrumentDrafts(t, f)
			probe.metadataCalls, probe.metadataRows = map[string]int{}, map[string]int{}
			detail, _, err := f.s.row(f.ctx, "owner/repo", issue)
			if err != nil || detail["status"] != "dependency_waiting" {
				t.Fatalf("detail=%v err=%v", detail, err)
			}
			rows, err := f.s.list(f.ctx)
			if err != nil || len(rows) != 1 || rows[0]["status"] != detail["status"] {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
			if _, ok := rows[0]["closeReason"]; ok {
				t.Fatalf("unused closure reason: %v", rows[0])
			}
			if probe.metadataCalls["close_reasons"] != 0 {
				t.Fatalf("unused closure queries=%d", probe.metadataCalls["close_reasons"])
			}
		})
	}
}
