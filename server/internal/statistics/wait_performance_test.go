package statistics

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

func TestWaitScopeCachePreservesCollisionPolicyAndBoundsPairs(t *testing.T) {
	ctx := context.Background()
	a := &fleet.Assignments{}
	scopes := [][]string{nil, {}, {"*"}, {"src/"}, {"SRC/file.go"}, {"srcOther/a.go"}, {"あ/"}, {"あ/項目.go"}, {"file.go"}, {"FILE.GO"}, {"src/a_test.go"}, {"tests/"}, {"tests/helper.go"}, {"src/a.go", "src/a_test.go"}}
	cache := newWaitScopeCache(a)
	for left, x := range scopes {
		for right, y := range scopes {
			one, two := &waitJob{ID: int64(left + 1), Repo: "o/r", Scope: x}, &waitJob{ID: int64(right + 1), Repo: "o/r", Scope: y}
			got, err := cache.overlap(ctx, one, two)
			if err != nil || got != jobs.ScopeOverlap(x, y, nil) {
				t.Fatalf("%v / %v: %v %v", x, y, got, err)
			}
		}
	}
	// 正規化済みキーと組結果を再利用し、前後期間の比較でも作り直さない。
	key := waitScopeKey{ID: 4}
	keys := cache.prepared[key]
	pairs := len(cache.pairs)
	if _, err := cache.overlap(ctx, &waitJob{ID: 4, Repo: "o/r", Scope: scopes[3]}, &waitJob{ID: 5, Repo: "o/r", Scope: scopes[4]}); err != nil {
		t.Fatal(err)
	}
	if len(cache.pairs) != pairs || len(keys) == 0 || &keys[0] != &cache.prepared[key][0] {
		t.Fatal("比較キーを再生成しました")
	}
	for id := int64(100); id < waitScopePairLimit+200; id++ {
		if _, err := cache.overlap(ctx, &waitJob{ID: 1, Repo: "o/r", Scope: nil}, &waitJob{ID: id, Repo: "o/r", Scope: []string{"other.go"}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.pairs) != waitScopePairLimit || len(cache.order) != waitScopePairLimit {
		t.Fatal("組キャッシュが上限を守っていません", len(cache.pairs), len(cache.order))
	}
}

func TestWaitScopeExemptionsAndCancellationPreservePolicy(t *testing.T) {
	a := &fleet.Assignments{Output: map[string]any{"dispatch": map[string]any{"scopeExemptPaths": []any{"SHARED/"}, "scopeExemptRepositories": map[string]any{"O/R": []any{"*.same"}}}}}
	cache := newWaitScopeCache(a)
	left, right := &waitJob{ID: 1, Repo: "o/r", Scope: []string{"shared/", "*.same", "src/"}}, &waitJob{ID: 2, Repo: "O/R", Scope: []string{"SHARED/", "*.same", "other/"}}
	got, err := cache.overlap(context.Background(), left, right)
	if err != nil || got != jobs.ScopeOverlap(left.Scope, right.Scope, a.Exempt("o/r")) || got {
		t.Fatal("免除範囲の扱いが変わりました", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = cache.overlap(ctx, left, right); !errors.Is(err, context.Canceled) {
		t.Fatal("cached結果で取消を無視しました", err)
	}
}

func TestPostgresWaitScopeLoadKeepsPopulationAndMissing(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,title,scope,filed_at) SELECT 'o/r',n,'待ち集計',ARRAY(SELECT 'src/job'||n||'/path'||p||'.go' FROM generate_series(1,80) p),$1 FROM generate_series(1,20) n`, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'nas','leased',filed_at+issue*interval '1 second','{}'::jsonb FROM jobs`); err != nil {
		t.Fatal(err)
	}
	service := &Service{DB: db, Assignments: &fleet.Assignments{Implementers: map[string][]string{"o/r": {"nas"}}, Hosts: map[string]*fleet.Host{"nas": {Max: 20}}}}
	result, err := service.Waits(ctx, Query{Project: "o/r", Before: Period{at, at.Add(time.Minute)}, After: Period{at.Add(time.Minute), at.Add(2 * time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	before := result.(map[string]any)["before"].(map[string]any)
	start, publish := before["startWait"].(map[string]any), before["publishWait"].(map[string]any)
	if start["count"] != 20 || start["missing"] != 0 || start["medianMs"] != float64(10500) || publish["count"] != 20 || publish["missing"] != 20 {
		t.Fatal("集計母数・欠測・中央値が変わりました", before)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if value, err := service.Waits(canceled, Query{}); err == nil || value != nil {
		t.Fatal("取消した集計を成功として返しました", value, err)
	}
}

func waitLoadJobs(count, paths int) (map[int64]*waitJob, Period, *fleet.Assignments) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	all := map[int64]*waitJob{}
	for n := 1; n <= count; n++ {
		id := int64(n)
		scope := []string{}
		for p := 0; p < paths; p++ {
			scope = append(scope, fmt.Sprintf("src/job%d/path%d.go", n, p))
		}
		all[id] = &waitJob{ID: id, Repo: "o/r", Filed: at, Scope: scope, Events: []event{{Job: id, At: at.Add(time.Duration(n) * time.Second), Kind: "leased", Host: "nas", Data: map[string]any{}}}}
	}
	return all, Period{at, at.Add(time.Duration(count+1) * time.Second)}, &fleet.Assignments{Implementers: map[string][]string{"o/r": {"nas"}}, Hosts: map[string]*fleet.Host{"nas": {Max: count}}}
}

func TestWaitCPUStopsOnCancellationWithoutPartialResults(t *testing.T) {
	all, period, a := waitLoadJobs(100, 100)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelDuringWork{Context: base, cancel: cancel, remaining: 1000}
	started := time.Now()
	starts, publishes, err := waitEntries(ctx, all, period, "o/r", a, newWaitScopeCache(a))
	if !errors.Is(err, context.Canceled) || starts != nil || publishes != nil || ctx.remaining > 0 {
		t.Fatal("取消後に途中結果を成功として返しました", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("CPU集計が期限後も継続しました")
	}
}

type cancelDuringWork struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *cancelDuringWork) Err() error {
	c.remaining--
	if c.remaining == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestWaitScopeCalculationKeepsHTTPResponsiveOnOneCPU(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	all, period, a := waitLoadJobs(60, 80)
	var computing atomic.Bool
	var observed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if computing.Load() {
			observed.Store(true)
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	server.Client().Timeout = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		first := true
		// 集計が最初のHTTP到着より先に終わっても、集計中の応答を観測するまで
		// 新しいキャッシュで実際の集計を繰り返す。
		for {
			computing.Store(true)
			if first {
				close(started)
				first = false
			}
			_, _, err := waitEntries(ctx, all, period, "o/r", a, newWaitScopeCache(a))
			computing.Store(false)
			if err != nil || observed.Load() {
				done <- err
				return
			}
		}
	}()
	<-started
	max := time.Duration(0)
	samples := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if !observed.Load() {
				t.Fatal("CPU集計中のHTTPを観測できませんでした")
			}
			t.Logf("60jobs×80scope/1CPU: HTTP%d回 最大%s", samples, max)
			return
		default:
		}
		start := time.Now()
		response, err := server.Client().Get(server.URL)
		if err != nil {
			t.Fatal("集計中にHTTPが応答しません", err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal(response.StatusCode)
		}
		if elapsed := time.Since(start); elapsed > max {
			max = elapsed
		}
		samples++
	}
}

func TestWaitsHistoryReadScaling(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var counts []int
	for _, offset := range []int{9, 0} {
		all := map[int64]*waitJob{}
		for n := 0; n < 2; n++ {
			base := at.Add(time.Duration(n) * 100 * time.Second)
			id := int64(n + 1)
			all[id] = &waitJob{ID: id, Repo: "o/r", Filed: base.Add(time.Duration(offset) * time.Second), Scope: []string{"a/"}, Events: []event{
				{At: base.Add(10 * time.Second), Kind: "leased", Host: "nas"},
				{At: base.Add(11 * time.Second), Kind: "pr_created"},
				{At: base.Add(12 * time.Second), Kind: "step_start", Data: map[string]any{"label": "レビュー"}},
				{At: base.Add(14 * time.Second), Kind: "step_end", Data: map[string]any{"label": "レビュー"}},
				{At: base.Add(15 * time.Second), Kind: "state", Data: map[string]any{"to": "merged"}},
			}}
			events := []event{}
			for i := 1; i < 10; i++ {
				events = append(events, event{At: base.Add(time.Duration(i) * time.Second), Kind: "state", Host: "other", Data: map[string]any{"to": "merged"}})
			}
			all[int64(n+3)] = &waitJob{ID: int64(n + 3), Repo: "other/r", Filed: base, Scope: []string{"b/"}, Events: events}
		}
		a := &fleet.Assignments{Implementers: map[string][]string{"o/r": {"nas"}}, Hosts: map[string]*fleet.Host{"nas": {Max: 1}}}
		q := Query{Project: "o/r", Before: Period{at, at.Add(100 * time.Second)}, After: Period{at.Add(100 * time.Second), at.Add(200 * time.Second)}}
		work := &statisticsWork{}
		ctx := context.WithValue(context.Background(), statisticsWorkKey{}, work)
		value, err := (&Service{Assignments: a}).calculateWaits(ctx, q, all, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"before", "after"} {
			period := fleet.Object(fleet.Object(value)[p])
			start := fleet.Object(period["startWait"])
			publish := fleet.Object(period["publishWait"])
			causes := fleet.Object(start["causes"])
			idle := fleet.Object(causes["idle"])
			review := fleet.Object(fleet.Object(publish["causes"])["review"])
			if start["count"] != 1 || start["missing"] != 0 || idle["ms"] != float64((10-offset)*1000) || idle["jobs"] != 1 || publish["count"] != 1 || publish["missing"] != 0 || review["ms"] != float64(2000) || review["jobs"] != 1 {
				t.Fatal(p, start, publish)
			}
		}
		expectedIntervals := 2 * (10 - offset)
		if work.waitStartIntervals != expectedIntervals {
			t.Fatalf("start intervals=%d, want %d", work.waitStartIntervals, expectedIntervals)
		}
		// The same 28 events produce one or ten start intervals per period.
		// This fixed bound includes index construction, both period scans, and
		// every binary-search probe. A per-interval scan of the nine-event
		// histories exceeds it in the ten-interval case.
		if work.waitEvents < 2*28 || work.waitEvents > 320 {
			t.Fatalf("history references=%d", work.waitEvents)
		}
		t.Logf("offset=%d history references=%d", offset, work.waitEvents)
		counts = append(counts, work.waitEvents)
	}
	if counts[1] <= counts[0] {
		t.Fatalf("indexed probes were not counted: %v", counts)
	}
}

type deadlineTestContext struct{ context.Context }

func (c deadlineTestContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

type waitTestConnector struct {
	query func(context.Context, string) error
}
type waitTestDriver struct{}
type waitTestConn struct {
	query func(context.Context, string) error
}
type waitTestTx struct{}
type waitTestRows struct{}

func (c waitTestConnector) Connect(context.Context) (driver.Conn, error) {
	return waitTestConn(c), nil
}
func (waitTestConnector) Driver() driver.Driver         { return waitTestDriver{} }
func (waitTestDriver) Open(string) (driver.Conn, error) { return waitTestConn{}, nil }
func (waitTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (waitTestConn) Close() error              { return nil }
func (waitTestConn) Begin() (driver.Tx, error) { return waitTestTx{}, nil }
func (waitTestConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return waitTestTx{}, nil
}
func (c waitTestConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.query != nil {
		if err := c.query(ctx, query); err != nil {
			return nil, err
		}
	}
	return waitTestRows{}, nil
}
func (waitTestTx) Commit() error               { return nil }
func (waitTestTx) Rollback() error             { return nil }
func (waitTestRows) Columns() []string         { return nil }
func (waitTestRows) Close() error              { return nil }
func (waitTestRows) Next([]driver.Value) error { return io.EOF }

func TestWaitsStageTimeoutLogging(t *testing.T) {
	for _, stage := range []string{"jobs", "events", "dependencies", "timeline", "before", "after", "summary"} {
		t.Run(stage, func(t *testing.T) {
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			dbStage := stage == "jobs" || stage == "events" || stage == "dependencies"
			work := &statisticsWork{stage: func(current string) {
				if !dbStage && current == stage {
					cancel()
				}
			}}
			ctx := context.WithValue(deadlineTestContext{parent}, statisticsWorkKey{}, work)
			db := sql.OpenDB(waitTestConnector{query: func(ctx context.Context, query string) error {
				if dbStage && ((stage == "jobs" && strings.Contains(query, "FROM jobs")) || (stage == "events" && strings.Contains(query, "FROM job_events")) || (stage == "dependencies" && strings.Contains(query, "FROM job_deps"))) {
					cancel()
					return ctx.Err()
				}
				return nil
			}})
			defer db.Close()
			svc := &Service{DB: db}
			var result any
			var err error
			if dbStage {
				var jobs map[int64]*waitJob
				jobs, err = svc.readWait(ctx, time.Now())
				if jobs != nil {
					result = jobs
				}
			} else {
				result, err = svc.calculateWaits(ctx, Query{}, map[int64]*waitJob{}, false)
			}
			if result != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("partial result=%v err=%v", result, err)
			}
			if !strings.Contains(logs.String(), "stage="+stage) || !strings.Contains(logs.String(), "context deadline exceeded") {
				t.Fatal(logs.String())
			}
		})
	}
}

func TestWaitsSameTimeEventsRespectRecordedOrder(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		kinds      []string
		idle, full float64
	}{
		{"merged_last", []string{"leased", "closed"}, 10000, 0},
		{"leased_last", []string{"closed", "leased"}, 5000, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []event{}
			for _, kind := range tc.kinds {
				events = append(events, event{At: at.Add(5 * time.Second), Kind: kind, Host: "nas"})
			}
			all := map[int64]*waitJob{1: {ID: 1, Repo: "o/r", Filed: at, Scope: []string{"a/"}, Events: []event{{At: at.Add(10 * time.Second), Kind: "leased", Host: "nas"}}}, 2: {ID: 2, Repo: "other/r", Filed: at, Scope: []string{"b/"}, Events: events}}
			a := &fleet.Assignments{Implementers: map[string][]string{"o/r": {"nas"}}, Hosts: map[string]*fleet.Host{"nas": {Max: 1}}}
			period := Period{at, at.Add(time.Minute)}
			result, err := (&Service{Assignments: a}).calculateWaits(context.Background(), Query{Project: "o/r", Before: period, After: period}, all, false)
			if err != nil {
				t.Fatal(err)
			}
			start := fleet.Object(fleet.Object(fleet.Object(result)["before"])["startWait"])
			causes := fleet.Object(start["causes"])
			if fleet.Object(causes["idle"])["ms"] != tc.idle || fleet.Object(causes["slotFull"])["ms"] != tc.full || start["missing"] != 0 {
				t.Fatal(start)
			}
		})
	}
}
