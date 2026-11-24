package anomalies

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-intake/server/internal/usage"
	"reflect"
	"strings"
	"testing"
	"time"
)

type historyConnector struct {
	current        [][]driver.Value
	calls          int
	fail           int
	closed         int
	pairs          map[string]bool
	oldValues      [][]driver.Value
	iterationError bool
}

func (c *historyConnector) Connect(context.Context) (driver.Conn, error) { return &historyConn{c}, nil }
func (c *historyConnector) Driver() driver.Driver                        { return historyDriver{} }

type historyDriver struct{}

func (historyDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type historyConn struct{ c *historyConnector }

func (*historyConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (*historyConn) Close() error                        { return nil }
func (*historyConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (c *historyConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "at>=$1"):
		return &historyRows{values: c.c.current, c: c.c}, nil
	case strings.Contains(q, "FROM usage_limit_rows"):
		return &historyRows{c: c.c}, nil
	case strings.Contains(q, "FROM usage_rows WHERE at<$1"):
		c.c.calls++
		if c.c.fail == c.c.calls {
			return nil, errors.New("injected query error")
		}
		if len(args) < 3 || len(args) > 513 || len(args)%2 != 1 || strings.Count(q, "(repo=$") != (len(args)-1)/2 {
			return nil, errors.New("bad batch")
		}
		for i := 1; i < len(args); i += 2 {
			key := fmt.Sprint(args[i].Value, "/", args[i+1].Value)
			if c.c.pairs[key] {
				return nil, errors.New("duplicate pair")
			}
			c.c.pairs[key] = true
		}
		return &historyRows{values: c.c.oldValues, c: c.c, iterationError: c.c.iterationError}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", q)
}

type historyRows struct {
	values         [][]driver.Value
	c              *historyConnector
	index          int
	iterationError bool
}

func (*historyRows) Columns() []string { return []string{"json"} }
func (r *historyRows) Close() error    { r.c.closed++; return nil }
func (r *historyRows) Next(dst []driver.Value) error {
	if r.index == len(r.values) {
		if r.iterationError {
			r.iterationError = false
			return errors.New("injected iteration error")
		}
		return io.EOF
	}
	copy(dst, r.values[r.index])
	r.index++
	return nil
}

func TestDetectHistoryReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		values    [][]driver.Value
		iteration bool
	}{
		{"invalid json", [][]driver.Value{{"{"}}, false},
		{"scan", [][]driver.Value{{struct{}{}}}, false},
		{"iteration", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(row(clock.Add(-time.Hour), 1, "実装", 10))
			c := &historyConnector{current: [][]driver.Value{{string(raw)}}, pairs: map[string]bool{}, oldValues: tc.values, iterationError: tc.iteration}
			db := sql.OpenDB(c)
			defer db.Close()
			out, err := (&Service{DB: db}).Detect(context.Background(), clock.Add(-24*time.Hour), clock)
			if err == nil || out != nil || c.closed != 2 {
				t.Fatalf("out=%v err=%v closed=%d", out, err, c.closed)
			}
		})
	}
}

func TestDetectHistoryBatchCounts(t *testing.T) {
	for _, n := range []int{0, 1, 255, 256, 257, 1024} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c := &historyConnector{pairs: map[string]bool{}}
			for i := 0; i < n; i++ {
				r := row(clock.Add(-time.Hour), i/2+1, "実装", 10)
				r["repo"] = fmt.Sprintf("repo-%d", i%2)
				raw, _ := json.Marshal(r)
				c.current = append(c.current, []driver.Value{string(raw)})
			}
			if n > 0 {
				c.current = append(c.current, c.current[0])
			}
			db := sql.OpenDB(c)
			defer db.Close()
			_, err := (&Service{DB: db, Now: func() time.Time { return clock }}).Detect(context.Background(), clock.Add(-24*time.Hour), clock)
			if err != nil {
				t.Fatal(err)
			}
			if c.calls != (n+255)/256 || len(c.pairs) != n {
				t.Fatalf("calls=%d pairs=%d", c.calls, len(c.pairs))
			}
		})
	}
}
func TestDetectHistoryMiddleBatchFailure(t *testing.T) {
	c := &historyConnector{pairs: map[string]bool{}, fail: 2}
	for i := 0; i < 257; i++ {
		raw, _ := json.Marshal(row(clock.Add(-time.Hour), i+1, "実装", 10))
		c.current = append(c.current, []driver.Value{string(raw)})
	}
	db := sql.OpenDB(c)
	defer db.Close()
	out, err := (&Service{DB: db}).Detect(context.Background(), clock.Add(-24*time.Hour), clock)
	if err == nil || out != nil || c.calls != 2 || c.closed != 2 {
		t.Fatalf("out=%v err=%v calls=%d closed=%d", out, err, c.calls, c.closed)
	}
}

type historyPlanNode struct {
	IndexName string            `json:"Index Name"`
	IndexCond string            `json:"Index Cond"`
	Plans     []historyPlanNode `json:"Plans"`
}

func (node historyPlanNode) usesIssueIndex() bool {
	if node.IndexName == "usage_rows_repo_issue" && strings.Contains(node.IndexCond, "(repo =") && strings.Contains(node.IndexCond, "(issue =") {
		return true
	}
	for _, child := range node.Plans {
		if child.usesIssueIndex() {
			return true
		}
	}
	return false
}

func TestPostgresHistoryBatchResultsAndIndex(t *testing.T) {
	db, ctx := testPostgres(t)
	_, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,source,repo,issue,json,sender)
 SELECT 'batch-'||i||'-'||j, $1::timestamptz - (60+j)*interval '1 day', 'nas', 'worker',
 'repo-'||(i%2), i/2+1,
 json_build_object('repo','repo-'||(i%2),'issue',i/2+1,'at',to_char(($1::timestamptz AT TIME ZONE 'UTC') - (60+j)*interval '1 day','YYYY-MM-DD"T"HH24:MI:SS"Z"'),'sample',j)::text, 'test'
 FROM generate_series(0,1023) i CROSS JOIN generate_series(1,10) j`, clock)
	if err != nil {
		t.Fatal(err)
	}
	// Unrelated history makes the indexed target lookup selective.
	if _, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender)
 SELECT 'background-'||i, $1::timestamptz - interval '90 days', 'nas', 'background', i, '{}', 'test'
 FROM generate_series(1,100000) i`, clock); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `ANALYZE usage_rows`); err != nil {
		t.Fatal(err)
	}
	lower := clock.Add(-21 * 24 * time.Hour)
	for _, n := range []int{1, 256, 1024} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			wanted := map[string]map[string]any{}
			targets := make([]map[string]any, 0, n)
			expected := map[string][]map[string]any{}
			for i := range n {
				repo, issue := fmt.Sprintf("repo-%d", i%2), float64(i/2+1)
				target := map[string]any{"repo": repo, "issue": issue}
				key := issueKey(target)
				wanted[key] = target
				targets = append(targets, target)
				// Each issue has ten samples. The oldest timestamp is sample 10.
				for sample := 10; sample >= 1; sample-- {
					at := clock.AddDate(0, 0, -60-sample)
					expected[key] = append(expected[key], map[string]any{
						"repo": repo, "issue": issue, "at": at.Format(time.RFC3339), "sample": float64(sample),
					})
				}
			}
			for start := 0; start+256 <= n; start += 256 {
				query, args := olderHistoryQuery(targets[start:start+256], lower)
				var plan []byte
				if err := db.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&plan); err != nil {
					t.Fatal(err)
				}
				var summary []struct {
					Plan          historyPlanNode `json:"Plan"`
					ExecutionTime float64         `json:"Execution Time"`
				}
				if err := json.Unmarshal(plan, &summary); err != nil || len(summary) != 1 {
					t.Fatalf("invalid EXPLAIN: %v", err)
				}
				if !summary[0].Plan.usesIssueIndex() {
					t.Fatalf("expected indexed lookup on repo and issue: %s", plan)
				}
				t.Logf("targets=%d batch=%d indexed=true execution_ms=%.3f", n, start/256+1, summary[0].ExecutionTime)
			}
			rows, err := readOlderHistory(ctx, db, wanted, lower, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string][]map[string]any{}
			for _, row := range rows {
				key := issueKey(row)
				got[key] = append(got[key], row)
			}
			if len(got) != len(expected) {
				t.Fatalf("got %d issue histories, want %d", len(got), len(expected))
			}
			for key, want := range expected {
				if !reflect.DeepEqual(got[key], want) {
					t.Fatalf("%s: got %v, want %v", key, got[key], want)
				}
			}
		})
	}
}

func TestPostgresHistoryBoundariesAndInvalidJSON(t *testing.T) {
	db, ctx := testPostgres(t)
	lower := clock.Add(-21 * 24 * time.Hour)
	items := []struct {
		key, repo string
		issue     int
		at        time.Time
		raw       string
	}{
		{"a", "a/r", 1, lower.Add(-time.Millisecond), `{"repo":"a/r","issue":1,"at":"2026-10-11T20:59:59.999+09:00","sample":"a"}`},
		{"b", "a/r", 1, lower.Add(-time.Millisecond), `{"repo":"a/r","issue":1,"sample":"b"}`},
		{"c", "a/r", 1, lower, `{"repo":"a/r","issue":1,"sample":"c"}`},
		{"d", "a/r", 1, lower.Add(time.Millisecond), `{"repo":"a/r","issue":1,"sample":"d"}`},
		{"e", "b/r", 1, lower.Add(-time.Hour), `{"repo":"b/r","issue":1,"sample":"e"}`},
		{"f", "a/r", 2, lower.Add(-time.Hour), `{"repo":"a/r","issue":2,"sample":"f"}`},
		{"g", "a/r", 1, clock.Add(-70 * 24 * time.Hour), `{"repo":"a/r","issue":1,"sample":"g"}`},
	}
	for _, v := range items {
		_, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,repo,issue,json,host,sender) VALUES($1,$2,$3,$4,$5,'nas','test')`, v.key, v.at, v.repo, v.issue, v.raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	wanted := map[string]map[string]any{"a/r#1": {"repo": "a/r", "issue": float64(1)}, "b/r#1": {"repo": "b/r", "issue": float64(1)}}
	got, err := readOlderHistory(ctx, db, wanted, lower, nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string][]string{}
	for _, v := range got {
		groups[issueKey(v)] = append(groups[issueKey(v)], v["sample"].(string))
	}
	if !reflect.DeepEqual(groups, map[string][]string{"a/r#1": {"g", "a", "b"}, "b/r#1": {"e"}}) {
		t.Fatal(groups)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,repo,issue,json,host,sender) VALUES('broken',$1,'a/r',1,'{','nas','test')`, lower.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = readOlderHistory(ctx, db, wanted, lower, nil); err == nil {
		t.Fatal("old invalid JSON must fail")
	}
}

// legacyDetect preserves the pre-batch implementation as a regression oracle.
func (s *Service) legacyDetect(ctx context.Context, from, to time.Time) (map[string]any, error) {
	store := usage.Store{DB: s.DB}
	lower := to.Add(-21 * 24 * time.Hour)
	if from.Add(-14 * 24 * time.Hour).Before(lower) {
		lower = from.Add(-14 * 24 * time.Hour)
	}
	all, _, e := store.Read(ctx, "", lower, to.Add(time.Millisecond))
	if e != nil {
		return nil, e
	}
	selected, history := []map[string]any{}, []map[string]any{}
	wanted := map[string]map[string]any{}
	for _, r := range all {
		t := at(r)
		if !t.Before(from) && t.Before(to) {
			selected = append(selected, r)
			if key := issueKey(r); key != "" {
				wanted[key] = r
			}
		}
	}
	for _, r := range all {
		t := at(r)
		if t.Before(to) && (!t.Before(from.Add(-14*24*time.Hour)) || wanted[issueKey(r)] != nil) {
			history = append(history, r)
		}
	}

	for _, target := range wanted {
		older, e := s.DB.QueryContext(ctx, "SELECT json FROM usage_rows WHERE repo=$1 AND issue=$2 AND at<$3 ORDER BY at,key", target["repo"], target["issue"], lower)
		if e != nil {
			return nil, e
		}
		for older.Next() {
			var raw []byte
			if e = older.Scan(&raw); e != nil {
				older.Close()
				return nil, e
			}
			var r map[string]any
			if e = json.Unmarshal(raw, &r); e != nil {
				older.Close()
				return nil, e
			}
			history = append(history, r)
		}
		e = older.Err()
		older.Close()
		if e != nil {
			return nil, e
		}
	}
	hits := Basic(selected, history, s.Weights)
	hits = append(hits, Cache(selected, history)...)
	hits = append(hits, CallTriggers(selected, history)...)
	hits = append(hits, UnitTotals(selected, history, s.Weights)...)
	hits = append(hits, ProviderShifts(selected, history)...)
	seen := map[string]bool{}
	dedup := []map[string]any{}
	for _, h := range hits {
		key := h["causeKey"].(string)
		if !seen[key] {
			dedup = append(dedup, h)
			seen[key] = true
		}
	}
	hits = dedup
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	repeat := IntakeRepeats(all, to, now)
	for _, r := range repeat {
		if r["status"] == "hit" {
			h := map[string]any{}
			for k, v := range r {
				h[k] = v
			}
			h["provisional"] = false
			hits = append(hits, h)
		}
		if p := r["provisional"].(map[string]any); p["status"] == "hit" {
			h := map[string]any{}
			for k, v := range r {
				h[k] = v
			}
			for k, v := range p {
				h[k] = v
			}
			h["provisional"], h["at"] = true, iso(to)
			hits = append(hits, h)
		}
	}
	limitRows := []map[string]any{}
	readReason := ""
	rows, e := s.DB.QueryContext(ctx, "SELECT json FROM usage_limit_rows WHERE at>$1 AND at<=$2 ORDER BY at,key", to.Add(-8*24*time.Hour), to)
	if e != nil {
		readReason = "read_limits_failed"
	} else {
		for rows.Next() {
			var raw []byte
			if e = rows.Scan(&raw); e != nil {
				readReason = "read_limits_failed"
				break
			}
			var r map[string]any
			if json.Unmarshal(raw, &r) != nil {
				continue
			}
			limitRows = append(limitRows, r)
		}
		if rows.Err() != nil {
			readReason = "read_limits_failed"
		}
		rows.Close()
	}
	forecast := LimitForecast(limitRows, to, readReason)
	for _, r := range forecast {
		if r["status"] == "hit" {
			hits = append(hits, r)
		}
	}
	filing := []map[string]any{}
	if s.FilingCheck != nil {
		for _, repo := range s.Repositories {
			read, e := s.FilingCheck(ctx, repo, to)
			r := FilingCheckResult(repo, read, e != nil, to)
			filing = append(filing, r)
			if r["status"] == "hit" {
				hits = append(hits, r)
			}
		}
	}
	cache := FilingCaches(all, to, s.Weights)
	for _, r := range cache {
		if r["status"] == "hit" {
			hits = append(hits, r)
		}
	}
	return map[string]any{"filingCheck": filing, "filingCache": cache, "limitForecast": forecast, "intakeRepeat": repeat, "from": iso(from), "to": iso(to), "rows": len(selected), "anomalies": hits}, nil
}

func TestPostgresDetectMatchesLegacy(t *testing.T) {
	db, ctx := testPostgres(t)
	from := clock.Add(-24 * time.Hour)
	lower := clock.Add(-21 * 24 * time.Hour)
	for i, at := range []time.Time{clock.Add(-70 * 24 * time.Hour), lower.Add(-time.Millisecond), lower, lower.Add(time.Millisecond), from.Add(-time.Millisecond), from, clock.Add(-time.Millisecond), clock} {
		r := row(at, 1, "実装", 10)
		r["outcome"] = "failed"
		if i == 1 {
			delete(r, "model")
		}
		if i == 2 {
			r["at"] = at.In(time.FixedZone("offset", 9*3600)).Format(time.RFC3339Nano)
		}
		raw, _ := json.Marshal(r)
		_, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,repo,issue,json,host,sender) VALUES($1,$2,'o/r',1,$3,'nas','test')`, fmt.Sprintf("r-%d", i), at, string(raw))
		if err != nil {
			t.Fatal(err)
		}
	}
	// Exercise every detector using the same stored rows for both implementations.
	var fixture []map[string]any
	for i := 0; i < 20; i++ {
		old := row(clock.Add(-48*time.Hour), 1000+i, "受付の相談", 100)
		old["source"], old["callTrigger"], old["cacheReadTokens"] = "intake", "initial", float64(90)
		fixture = append(fixture, old)
		current := row(clock.Add(-time.Duration(i+1)*time.Minute), 1000+i, "受付の相談", 100)
		current["source"], current["callTrigger"], current["cacheReadTokens"] = "intake", "repair", float64(80)
		fixture = append(fixture, current)
		old = row(clock.Add(-48*time.Hour), 2000+i, "実装", 1)
		old["provider"] = "claude"
		fixture = append(fixture, old)
	}
	for i := 0; i < 10; i++ {
		fixture = append(fixture, row(clock.Add(-8*24*time.Hour), 3000+i, "実装", 10))
		old := row(midnight(clock).Add(-8*24*time.Hour), 4000+i, "受付の相談", 1)
		old["source"], old["draft"], old["provider"] = "intake", fmt.Sprint(i), "repeat-provider"
		fixture = append(fixture, old)
		for j := 0; j < 3; j++ {
			r := row(midnight(clock).Add(-24*time.Hour), 4000+i, "受付の相談", 1)
			r["source"], r["draft"], r["provider"] = "intake", fmt.Sprint(i), "repeat-provider"
			fixture = append(fixture, r)
		}
	}
	for i := 0; i < 30; i++ {
		fixture = append(fixture, row(clock.Add(-time.Duration(i+1)*time.Minute), 5000, "実装", 10))
		for _, after := range []bool{false, true} {
			when := clock.Add(-48 * time.Hour)
			if after {
				when = clock.Add(-23 * time.Hour)
			}
			r := row(when, 6000+i, usage.FilingCheckStep, 10)
			r["provider"], r["cacheWriteTokens"], r["promptTotalBytes"], r["outcome"], r["promptCache"] = "claude", float64(0), float64(100), "completed", !after
			fixture = append(fixture, r)
		}
	}
	for i := 0; i < 3; i++ {
		r := row(clock.Add(-time.Duration(i+1)*time.Hour), 7000, "競合解消", 100)
		r["outcome"] = "failed"
		fixture = append(fixture, r)
	}
	fixture = append(fixture, row(clock.Add(-time.Hour), 8000, "実装", 1000))
	for i, r := range fixture {
		raw, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		_, e = db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,repo,issue,json,host,sender) VALUES($1,$2,$3,$4,$5,'nas','test')`, fmt.Sprintf("fixture-%d", i), at(r), r["repo"], r["issue"], string(raw))
		if e != nil {
			t.Fatal(e)
		}
	}
	for i, percent := range []int{10, 60} {
		r := map[string]any{"provider": "claude", "limit": "five_hour", "at": iso(clock.Add(time.Duration(i-1) * time.Hour)), "resetsAt": iso(clock.Add(6 * time.Hour)), "percent": percent}
		raw, _ := json.Marshal(r)
		if _, e := db.ExecContext(ctx, `INSERT INTO usage_limit_rows(key,at,host,json,sender) VALUES($1,$2,'nas',$3,'test')`, fmt.Sprintf("limit-%d", i), clock.Add(time.Duration(i-1)*time.Hour), string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	_, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,repo,issue,json,host,sender) VALUES('bad-current',$1,'o/r',1,'{','nas','test')`, from.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{DB: db, Now: func() time.Time { return clock }, Repositories: []string{"o/r"}, FilingCheck: func(context.Context, string, time.Time) (map[string]any, error) {
		return map[string]any{"before": map[string]any{"filing": map[string]any{"comparisonRequests": 1, "newIssuesInPeriod": 1}}, "after": map[string]any{"filing": map[string]any{"comparisonRequests": 2, "newIssuesInPeriod": 1}}}, nil
	}}
	before, err := svc.legacyDetect(ctx, from, clock)
	if err != nil {
		t.Fatal(err)
	}
	after, err := svc.Detect(ctx, from, clock)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, h := range after["anomalies"].([]map[string]any) {
		counts[h["kind"].(string)]++
	}
	t.Logf("anomaly hit counts: %v", counts)
	for kind := range names {
		if counts[kind] == 0 {
			t.Errorf("missing hit for %s; counts=%v", kind, counts)
		}
	}
	if !reflect.DeepEqual(before, after) {
		a, _ := json.Marshal(before)
		b, _ := json.Marshal(after)
		t.Fatalf("legacy=%s\nbatch=%s", a, b)
	}
	if after["rows"] != 86 {
		t.Fatalf("[from,to) and malformed row: %v", after["rows"])
	}
}
