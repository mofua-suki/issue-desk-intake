package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/statistics"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatsDraftReadOnceHTTP(t *testing.T) {
	calls := 0
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) {
			calls++
			return []map[string]any{}, nil
		}
	})
	status, result := workerRequest(t, handler, token, "GET", "/api/stats", "")
	if status != 200 || calls != 1 {
		t.Fatalf("status=%d calls=%d result=%v", status, calls, result)
	}
	for _, name := range []string{"inputBudget", "consultationBudget", "tiers"} {
		if result[name] == nil {
			t.Errorf("%s unavailable: %v", name, result["unavailable"])
		}
	}
}

func TestStatsDraftSnapshotIsolation(t *testing.T) {
	issues := make([]any, 1, 4)
	issues[0] = map[string]any{"number": 1}
	source := []map[string]any{{"name": "b", "issues": issues, "nested": map[string]any{"values": []string{"original"}}}, {"name": "a"}}
	read := statsDraftSnapshot(func(context.Context) ([]map[string]any, error) { return source, nil })
	first, err := read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(first, func(i, j int) bool { return first[i]["name"].(string) < first[j]["name"].(string) })
	first[1]["nested"].(map[string]any)["values"].([]string)[0] = "changed"
	first[1]["issues"].([]any)[0].(map[string]any)["number"] = 2
	first[1]["issues"] = append(first[1]["issues"].([]any), "added")
	if issues[:cap(issues)][1] != nil {
		t.Fatalf("append changed the source backing array: %v", issues[:cap(issues)])
	}
	second, err := read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"name": "b", "issues": []any{map[string]any{"number": 1}}, "nested": map[string]any{"values": []string{"original"}}}, {"name": "a"}}
	if !reflect.DeepEqual(source, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("source=%v second=%v", source, second)
	}
}

func TestStatsDraftReadErrorSharedHTTP(t *testing.T) {
	readErr := errors.New("draft read failed")
	calls := 0
	read := statsDraftSnapshot(func(context.Context) ([]map[string]any, error) { calls++; return nil, readErr })
	for i := 0; i < 2; i++ {
		if _, err := read(context.Background()); err != readErr {
			t.Fatalf("error=%v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	calls = 0
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { calls++; return nil, readErr }
	})
	status, result := workerRequest(t, handler, token, "GET", "/api/stats?tables=inputBudget,tiers,consultationBudget", "")
	if status != 200 || calls != 1 {
		t.Fatalf("status=%d calls=%d result=%v", status, calls, result)
	}
	for _, name := range []string{"inputBudget", "tiers", "consultationBudget"} {
		if result[name] != nil || !strings.Contains(fmt.Sprint(result["unavailable"]), name) {
			t.Errorf("%s: %v", name, result)
		}
	}
}

func TestStatsDraftBackendUnsetHTTP(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) { o.StatisticsDrafts = nil })
	status, result := workerRequest(t, handler, token, "GET", "/api/stats?tables=inputBudget,tiers,consultationBudget", "")
	if status != 200 {
		t.Fatalf("status=%d result=%v", status, result)
	}
	for _, name := range []string{"inputBudget", "tiers", "consultationBudget"} {
		if result[name] != nil || !strings.Contains(fmt.Sprint(result["unavailable"]), name) {
			t.Errorf("%s: %v", name, result)
		}
	}
}

func TestOperationsModelsAndStatisticsUseDelegatedIdentity(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		// The production schema carries the legacy stall_fixes table (draft,key,stall,
		// filed_at) that statistics cannot read as json yet. Remove it from the isolated
		// PostgreSQL fixture to keep this test focused on delegation.
		if _, err := o.AppDB.Exec("DROP TABLE stall_fixes"); err != nil {
			t.Fatal(err)
		}
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { return []map[string]any{}, nil }
		o.ModelProviders = func(context.Context) ([]map[string]any, error) {
			return []map[string]any{{"id": "codex", "ready": true}, {"id": "claude", "ready": true}}, nil
		}
	})
	for _, path := range []string{"/api/stats?tables=usage,tiers,followups,usageReview,priorityReview,testSelection,scope,waits,stalls,reviewNotes,filingCheck,giteaReads,usageLimits", "/api/stats/overview?range=today", "/api/v1/ops/usage-anomalies", "/api/model-overrides", "/api/provider-disables"} {
		status, response := workerRequest(t, handler, token, "GET", path, "")
		if status != 200 {
			t.Fatalf("%s: %d %v", path, status, response)
		}
		if strings.HasPrefix(path, "/api/stats?tables=") {
			if unavailable, ok := response["unavailable"].([]any); !ok || len(unavailable) != 0 {
				t.Fatal(response)
			}
		}
	}
	status, result := workerRequest(t, handler, token, "POST", "/api/model-overrides/intake", `{"provider":"claude","model":"some-model"}`)
	if status != 409 {
		t.Fatal(status, result)
	}
	status, result = workerRequest(t, handler, token, "POST", "/api/provider-disables/claude", `{}`)
	if status != 200 || result["set"] == nil {
		t.Fatal(status, result)
	}
	status, result = workerRequest(t, handler, token, "POST", "/api/provider-disables/codex", `{}`)
	if status != 400 {
		t.Fatal(status, result)
	}
	status, result = workerRequest(t, handler, token, "POST", "/api/provider-disables/claude/cancel", `{}`)
	if status != 200 || result["cancelled"] != true {
		t.Fatal(status, result)
	}
	plain, _, plainToken := workerFixture(t)
	for _, path := range []string{"/api/stats", "/api/v1/ops/usage-anomalies", "/api/provider-disables"} {
		status, _ := workerRequest(t, plain, plainToken, "GET", path, "")
		if status != 403 {
			t.Fatal(path, status)
		}
	}
}

func TestStatsSummaryAndDetailsHTTP(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { return []map[string]any{}, nil }
	})
	for _, path := range []string{"/api/stats?tables=usage,tiers,implementation,followups", "/api/stats?tables=usage,tiers,implementation,followups&details=true", "/api/stats/rework", "/api/stats/rework?details=true"} {
		status, result := workerRequest(t, handler, token, "GET", path, "")
		if status != 200 {
			t.Fatalf("%s: %d %v", path, status, result)
		}
		detail := strings.Contains(path, "details=true")
		if strings.HasPrefix(path, "/api/stats?") {
			usage, ok := result["usage"].(map[string]any)
			if !ok {
				t.Fatalf("usage unavailable: %v", result)
			}
			for _, period := range []string{"before", "after"} {
				p := usage[period].(map[string]any)
				for _, key := range []string{"drafts", "sessions", "repairs", "contextReadPaths", "issues"} {
					_, exists := p[key]
					if exists != detail {
						t.Errorf("%s %s.%s exists=%v", path, period, key, exists)
					}
				}
			}
			tiers := result["tiers"].(map[string]any)
			_, exists := tiers["rows"]
			if exists != detail {
				t.Errorf("%s tiers.rows exists=%v", path, exists)
			}
		} else {
			breakdown, ok := result["breakdown"].(map[string]any)
			if !ok {
				t.Fatalf("breakdown unavailable: %v", result)
			}
			_, exists := breakdown["issues"]
			if exists != detail {
				t.Errorf("%s breakdown.issues exists=%v", path, exists)
			}
			impl := result["implementation"].(map[string]any)
			_, exists = impl["issues"]
			if exists != detail {
				t.Errorf("%s implementation.issues exists=%v", path, exists)
			}
		}
	}
	for _, path := range []string{"/api/stats?details=1", "/api/stats/rework?details=true&details=false"} {
		status, _ := workerRequest(t, handler, token, "GET", path, "")
		if status != 400 {
			t.Errorf("%s: %d", path, status)
		}
	}
}

func TestStatsSummaryReworkAccountingFixture(t *testing.T) {
	raw, err := os.ReadFile("../statistics/testdata/rework-accounting.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Input struct {
			Jobs, Events, Usage []statistics.AccountRecord
			Period, Weights     statistics.AccountRecord
			Skipped             int
		}
		Implementation statistics.AccountRecord
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	input := fixture.Input
	breakdown := statistics.BuildReworkBreakdown(input.Jobs, input.Events, input.Usage, input.Weights, input.Period, input.Skipped)
	report := map[string]any{"breakdown": breakdown, "implementation": fixture.Implementation}
	before, _ := json.Marshal(report)
	if _, ok := breakdown["issues"]; !ok {
		t.Fatal("fixture has no issues")
	}
	summarizeStats(reflect.ValueOf(report), "")
	if _, ok := breakdown["issues"]; ok {
		t.Fatal("breakdown issues retained")
	}
	if _, ok := fixture.Implementation["issues"]; ok {
		t.Fatal("implementation issues retained")
	}
	groups := breakdown["followups"].(statistics.AccountRecord)["groups"].([]statistics.AccountRecord)
	for _, group := range groups {
		if _, ok := group["issues"]; ok {
			t.Fatal("followup issues retained")
		}
	}
	after, _ := json.Marshal(report)
	t.Logf("repository rework fixture compact JSON: details=%d summary=%d bytes", len(before), len(after))
}

func TestStatsSummaryPreservesMissingAndCounts(t *testing.T) {
	period := map[string]any{"drafts": []any{1}, "sessions": []any{}, "repairs": []any{}, "contextReadPaths": []any{}, "contextReadPathsOmitted": 0, "issues": []any{1}, "omittedIssues": 0, "total": map[string]any{"missing": 2, "value": nil, "zero": 0}}
	report := map[string]any{"usage": map[string]any{"before": period}, "tiers": map[string]any{"rows": []any{1}, "total": 1, "omitted": 0}, "fixedTestUsage": map[string]any{"before": map[string]any{"rows": []any{map[string]any{"host": "nas"}}}}}
	summarizeStats(reflect.ValueOf(report), "")
	for _, key := range []string{"drafts", "sessions", "repairs", "contextReadPaths", "contextReadPathsOmitted", "issues", "omittedIssues"} {
		if _, ok := period[key]; ok {
			t.Errorf("%s retained", key)
		}
	}
	total := period["total"].(map[string]any)
	if total["missing"] != 2 || total["value"] != nil || total["zero"] != 0 {
		t.Fatal(total)
	}
	fixed := report["fixedTestUsage"].(map[string]any)["before"].(map[string]any)
	if _, ok := fixed["rows"]; !ok {
		t.Fatal("aggregated fixed test rows removed")
	}
}

func TestStatsDetailsSelectionWithoutDatabase(t *testing.T) {
	for _, details := range []bool{false, true} {
		period := map[string]any{"drafts": []any{1}, "sessions": []any{1}, "repairs": []any{1}, "contextReadPaths": []any{1}, "issues": []any{1}}
		tiers := map[string]any{"rows": []any{1}}
		impl := map[string]any{"issues": []any{1}}
		leak := map[string]any{"issues": []any{1}}
		report := map[string]any{"usage": map[string]any{"before": period}, "tiers": tiers, "implementation": map[string]any{"before": impl}, "testSelection": map[string]any{"leaks": map[string]any{"items": []any{leak}}}}
		applyStatsDetails(report, details)
		for _, key := range []string{"drafts", "sessions", "repairs", "contextReadPaths", "issues"} {
			if _, ok := period[key]; ok != details {
				t.Errorf("details=%v usage.%s exists=%v", details, key, ok)
			}
		}
		if _, ok := tiers["rows"]; ok != details {
			t.Errorf("details=%v tiers.rows exists=%v", details, ok)
		}
		if _, ok := impl["issues"]; ok != details {
			t.Errorf("details=%v implementation.issues exists=%v", details, ok)
		}
		if _, ok := leak["issues"]; !ok {
			t.Error("unrelated testSelection issues removed")
		}
	}
}

func TestStatsStopChecksSelection(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { return []map[string]any{}, nil }
	})
	for _, path := range []string{"/api/stats?tables=stopChecks", "/api/stats"} {
		status, result := workerRequest(t, handler, token, "GET", path, "")
		if status != 200 {
			t.Fatalf("%s: %d %v", path, status, result)
		}
		report, ok := result["stopChecks"].(map[string]any)
		if !ok || len(report["rows"].([]any)) == 0 {
			t.Fatalf("%s: %v", path, result)
		}
	}
}

// statsHTTPFixture uses the same route and fleet service as RegisterWorkerRoutes,
// while exposing the route's state to coordinate concurrent requests.
func statsHTTPFixture(t *testing.T, edit func(*WorkerRouteOptions)) (*WorkerRoutes, string) {
	t.Helper()
	var options WorkerRouteOptions
	_, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { return []map[string]any{}, nil }
		if edit != nil {
			edit(o)
		}
		options = *o
	})
	if options.Now == nil {
		options.Now = time.Now
	}
	return &WorkerRoutes{options: options, fleet: &fleet.Service{DB: options.DB, FleetPath: options.FleetPath, AssignmentsPath: options.AssignmentsPath, Now: options.Now, ReleasePolicy: options.ReleasePolicy, WebhookVerifier: options.WebhookVerifier}}, token
}

func statsHTTP(s *WorkerRoutes, token, method, path string, ctx context.Context) *httptest.ResponseRecorder {
	return statsHTTPHandler(http.HandlerFunc(s.statsRoute), token, method, path, ctx)
}

func statsHTTPHandler(handler http.Handler, token, method, path string, ctx context.Context) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:8787"+path, nil)
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set(contract.Header, contract.Version)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func statsAwaitWaiters(s *WorkerRoutes, key string, n int) {
	for {
		s.statsCache.mu.Lock()
		f := s.statsCache.flights[key]
		found := f != nil && f.waiters >= n
		s.statsCache.mu.Unlock()
		if found {
			return
		}
		runtime.Gosched()
	}
}

func TestStatsCacheRoutesAndQueryHTTP(t *testing.T) {
	var calls atomic.Int32
	handler, db, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { calls.Add(1); return []map[string]any{}, nil }
	})
	for _, paths := range [][2]string{{"/api/stats?tables=tiers&project=owner%2Frepo", "/api/stats?project=owner%2Frepo&tables=tiers"}, {"/api/stats/overview?range=today&project=owner%2Frepo", "/api/stats/overview?project=owner%2Frepo&range=today"}, {"/api/stats/rework?project=owner%2Frepo&details=true", "/api/stats/rework?details=true&project=owner%2Frepo"}} {
		a := statsHTTPHandler(handler, token, "GET", paths[0], nil)
		count := calls.Load()
		if strings.HasPrefix(paths[0], "/api/stats/rework") {
			if _, err := db.Exec("ALTER TABLE jobs RENAME TO jobs_stats_unavailable"); err != nil {
				t.Fatal(err)
			}
		}
		b := statsHTTPHandler(handler, token, "GET", paths[1], nil)
		if a.Code != 200 || b.Code != 200 || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) || calls.Load() != count {
			t.Fatalf("%s: %d %d calls=%d/%d", paths[0], a.Code, b.Code, count, calls.Load())
		}
		if strings.HasPrefix(paths[0], "/api/stats/rework") {
			failed := statsHTTPHandler(handler, token, "GET", paths[1]+"&x=other", nil)
			if failed.Code != 200 || !strings.Contains(failed.Body.String(), "共有DBを読めませんでした") {
				t.Fatalf("rework did not recompute: %d %s", failed.Code, failed.Body.String())
			}
			if _, err := db.Exec("ALTER TABLE jobs_stats_unavailable RENAME TO jobs"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("draft reads=%d, want 2", calls.Load())
	}
}

func TestStatsCacheQuerySeparationHTTP(t *testing.T) {
	var calls atomic.Int32
	s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { calls.Add(1); return []map[string]any{}, nil }
	})
	paths := []string{"/api/stats?tables=usage", "/api/stats?tables=rework", "/api/stats?tables=tiers", "/api/stats?tables=tiers&project=owner%2Frepo", "/api/stats?tables=tiers&beforeFrom=2026-01-01&beforeTo=2026-01-02&afterFrom=2026-01-03&afterTo=2026-01-04", "/api/stats?tables=tiers&details=true", "/api/stats?tables=tiers&x=1&x=2", "/api/stats?tables=tiers&x=2&x=1", "/api/stats?tables=tiers&x=1", "/api/stats?tables=tiers&x=", "/api/stats/overview?range=today", "/api/stats/overview?range=7d", "/api/stats/rework?details=true"}
	for _, p := range paths {
		w := statsHTTP(s, token, "GET", p, nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
		r := httptest.NewRequest("GET", p, nil)
		key := r.URL.Path + "?" + r.URL.Query().Encode()
		s.statsCache.mu.Lock()
		_, ok := s.statsCache.entries[key]
		s.statsCache.mu.Unlock()
		if !ok {
			t.Fatalf("missing entry %s: %s", p, w.Body.String())
		}
	}
	s.statsCache.mu.Lock()
	n := len(s.statsCache.entries)
	s.statsCache.mu.Unlock()
	if n != len(paths) || calls.Load() != 10 {
		t.Fatalf("got %d entries, want %d; draft reads %d, want 10", n, len(paths), calls.Load())
	}
}

func TestStatsCacheExpiryAndCapacity(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.Now = clock
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { calls.Add(1); return []map[string]any{}, nil }
	})
	path := "/api/stats?tables=tiers"
	for i := 0; i < 2; i++ {
		if w := statsHTTP(s, token, "GET", path, nil); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	mu.Lock()
	now = now.Add(60 * time.Second)
	mu.Unlock()
	statsHTTP(s, token, "GET", path, nil)
	if calls.Load() != 2 {
		t.Fatalf("expiry: %d", calls.Load())
	}
	for i := 0; i < 16; i++ {
		mu.Lock()
		now = now.Add(time.Second)
		mu.Unlock()
		statsHTTP(s, token, "GET", fmt.Sprintf("%s&x=%d", path, i), nil)
	}
	s.statsCache.mu.Lock()
	n := len(s.statsCache.entries)
	s.statsCache.mu.Unlock()
	if n != 16 {
		t.Fatalf("capacity %d", n)
	}
	statsHTTP(s, token, "GET", path, nil)
	if calls.Load() != 19 {
		t.Fatalf("eviction: %d", calls.Load())
	}
	other, token2 := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { calls.Add(1); return []map[string]any{}, nil }
	})
	statsHTTP(other, token2, "GET", path, nil)
	if calls.Load() != 20 {
		t.Fatalf("route isolation: %d", calls.Load())
	}
	// A full cache must not evict an in-progress calculation.
	started, release := make(chan struct{}), make(chan struct{})
	var active atomic.Int32
	busy, busyToken := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(ctx context.Context) ([]map[string]any, error) {
			if active.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return []map[string]any{}, nil
		}
	})
	done := make(chan *httptest.ResponseRecorder, 2)
	go func() { done <- statsHTTP(busy, busyToken, "GET", path, nil) }()
	<-started
	for i := 0; i < 17; i++ {
		w := statsHTTP(busy, busyToken, "GET", fmt.Sprintf("/api/stats/rework?x=%d", i), nil)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	go func() { done <- statsHTTP(busy, busyToken, "GET", path, nil) }()
	statsAwaitWaiters(busy, path, 2)
	close(release)
	first, second := <-done, <-done
	if active.Load() != 1 || first.Code != 200 || second.Code != 200 || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("in-flight eviction: %d", active.Load())
	}

}

func TestStatsConcurrentRequestsHTTP(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(ctx context.Context) ([]map[string]any, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
				return []map[string]any{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	path := "/api/stats?tables=tiers"
	done := make(chan *httptest.ResponseRecorder, 2)
	go func() { done <- statsHTTP(s, token, "GET", path, nil) }()
	<-started
	go func() { done <- statsHTTP(s, token, "GET", path, nil) }()
	statsAwaitWaiters(s, path, 2)
	close(release)
	a, b := <-done, <-done
	if calls.Load() != 1 || a.Code != 200 || b.Code != 200 || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatalf("calls=%d codes=%d,%d", calls.Load(), a.Code, b.Code)
	}
}

func TestStatsSharedCalculationCancellationHTTP(t *testing.T) {
	for _, cancelFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(cancelFirst), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
				o.StatisticsDrafts = func(ctx context.Context) ([]map[string]any, error) {
					if calls.Add(1) == 1 {
						close(started)
					}
					select {
					case <-release:
						return []map[string]any{}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			})
			p := "/api/stats?tables=tiers"
			c1, x1 := context.WithCancel(context.Background())
			defer x1()
			c2, x2 := context.WithCancel(context.Background())
			defer x2()
			a, b := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
			go func() { a <- statsHTTP(s, token, "GET", p, c1) }()
			<-started
			go func() { b <- statsHTTP(s, token, "GET", p, c2) }()
			statsAwaitWaiters(s, p, 2)
			if cancelFirst {
				x1()
				<-a
			} else {
				x2()
				<-b
			}
			close(release)
			var got *httptest.ResponseRecorder
			if cancelFirst {
				got = <-b
			} else {
				got = <-a
			}
			if got.Code != 200 || calls.Load() != 1 {
				t.Fatalf("code=%d calls=%d", got.Code, calls.Load())
			}
		})
	}
	started, stopped := make(chan struct{}), make(chan struct{})
	releaseOld, finishedOld := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(ctx context.Context) ([]map[string]any, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				close(stopped)
				<-releaseOld
				close(finishedOld)
				return nil, ctx.Err()
			}
			return []map[string]any{}, nil
		}
	})
	p := "/api/stats?tables=tiers"
	ctx, cancel := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 2)
	go func() { done <- statsHTTP(s, token, "GET", p, ctx) }()
	<-started
	go func() { done <- statsHTTP(s, token, "GET", p, ctx2) }()
	statsAwaitWaiters(s, p, 2)
	cancel()
	<-done
	cancel2()
	<-done
	<-stopped
	w := statsHTTP(s, token, "GET", p, nil)
	close(releaseOld)
	<-finishedOld
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("code=%d calls=%d", w.Code, calls.Load())
	}
}

func TestStatsFailuresAreNotCachedHTTP(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) {
			calls.Add(1)
			if fail.Load() {
				return nil, errors.New("read failure")
			}
			return []map[string]any{}, nil
		}
	})
	path := "/api/stats?tables=tiers"
	a := statsHTTP(s, token, "GET", path, nil)
	if a.Code != 200 || !strings.Contains(a.Body.String(), "tiersの集計を読み取れませんでした") {
		t.Fatalf("partial: %d %s", a.Code, a.Body.String())
	}
	fail.Store(false)
	b := statsHTTP(s, token, "GET", path, nil)
	if b.Code != 200 || calls.Load() != 2 || strings.Contains(b.Body.String(), "tiersの集計を読み取れませんでした") {
		t.Fatalf("recovery: %d calls=%d %s", b.Code, calls.Load(), b.Body.String())
	}
	var bad atomic.Bool
	bad.Store(true)
	var count atomic.Int32
	s2, token2 := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) {
			count.Add(1)
			return []map[string]any{{"id": "d", "createdAt": time.Now().Format(time.RFC3339), "project": "owner/repo", "tier": map[string]any{"level": "standard"}, "issues": []any{map[string]any{"project": "owner/repo", "number": 1}}}}, nil
		}
		o.DeskNumber = func(string, int64) any {
			if bad.Load() {
				return make(chan int)
			}
			return 1
		}
	})
	path = "/api/stats?tables=tiers&details=true"
	first := statsHTTP(s2, token2, "GET", path, nil)
	if first.Code != 200 || first.Body.Len() != 0 {
		t.Fatalf("marshal failure: %d %q", first.Code, first.Body.String())
	}
	bad.Store(false)
	second := statsHTTP(s2, token2, "GET", path, nil)
	if second.Code != 200 || !json.Valid(second.Body.Bytes()) || count.Load() != 2 {
		t.Fatalf("marshal recovery: %d count=%d %s", second.Code, count.Load(), second.Body.String())
	}
	// Rework translates a database read error into a 200 with unavailable.
	s3, token3 := statsHTTPFixture(t, nil)
	if _, err := s3.options.DB.Exec("ALTER TABLE jobs RENAME TO jobs_stats_unavailable"); err != nil {
		t.Fatal(err)
	}
	failed := statsHTTP(s3, token3, "GET", "/api/stats/rework", nil)
	if failed.Code != 200 || !strings.Contains(failed.Body.String(), "共有DBを読めませんでした") {
		t.Fatalf("rework failure: %d %s", failed.Code, failed.Body.String())
	}
	if _, err := s3.options.DB.Exec("ALTER TABLE jobs_stats_unavailable RENAME TO jobs"); err != nil {
		t.Fatal(err)
	}
	recovered := statsHTTP(s3, token3, "GET", "/api/stats/rework", nil)
	if recovered.Code != 200 || strings.Contains(recovered.Body.String(), "共有DBを読めませんでした") {
		t.Fatalf("rework recovery: %d %s", recovered.Code, recovered.Body.String())
	}

	var panicOnce atomic.Bool
	panicOnce.Store(true)
	s4, token4 := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) {
			if panicOnce.Swap(false) {
				panic("test calculation failure")
			}
			return []map[string]any{}, nil
		}
	})
	if w := statsHTTP(s4, token4, "GET", "/api/stats?tables=tiers", nil); w.Code != 503 {
		t.Fatalf("panic status: %d", w.Code)
	}
	if w := statsHTTP(s4, token4, "GET", "/api/stats?tables=tiers", nil); w.Code != 200 {
		t.Fatalf("panic recovery status: %d", w.Code)
	}

}

func TestStatsCacheAuthorizationHTTP(t *testing.T) {
	s, token := statsHTTPFixture(t, nil)
	path := "/api/stats?tables=tiers"
	if w := statsHTTP(s, token, "GET", path, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	plain := "idk_" + strings.Repeat("b", 43)
	if _, err := s.options.AppDB.Exec("INSERT INTO api_tokens(id,name,hash,issued_at) VALUES(?,?,?,?)", "unprivileged", "other", auth.Hash(plain), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token, method, path string
		code                int
	}{{"", "GET", path, 401}, {plain, "GET", path, 403}, {token, "POST", path, 405}, {token, "GET", "/api/stats?tables=unknown", 400}, {token, "GET", "/api/stats?details=oops", 400}, {token, "GET", "/api/stats/overview?range=invalid", 400}, {token, "GET", "/api/stats/rework?project=unknown", 400}} {
		if w := statsHTTP(s, tc.token, tc.method, tc.path, nil); w.Code != tc.code {
			t.Errorf("%s %s: %d want %d %s", tc.method, tc.path, w.Code, tc.code, w.Body.String())
		}
	}
	// Details were checked before the rework query in the original route.
	w := statsHTTP(s, token, "GET", "/api/stats/rework?details=invalid&project=unknown", nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "detailsはtrueまたはfalse") {
		t.Fatalf("validation order: %d %s", w.Code, w.Body.String())
	}

}

func TestStatsCachedDetailsIsolationHTTP(t *testing.T) {
	at := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	var gate atomic.Bool
	started, release := make(chan struct{}, 2), make(chan struct{})
	s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.Now = func() time.Time { return at }
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) {
			calls.Add(1)
			if gate.Load() {
				started <- struct{}{}
				<-release
			}
			return []map[string]any{{"id": "draft-1", "createdAt": at.Format(time.RFC3339), "project": "owner/repo", "tier": map[string]any{"level": "standard"}, "issues": []any{map[string]any{"project": "owner/repo", "number": 1}}}}, nil
		}
	})
	for _, detail := range []string{"true", "false", "true", "false"} {
		path := "/api/stats?tables=tiers,usage&details=" + detail
		w := statsHTTP(s, token, "GET", path, nil)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get(contract.Header) != contract.Version {
			t.Fatalf("headers/code %d %v", w.Code, w.Header())
		}
		var v map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		tiers := v["tiers"].(map[string]any)
		if tiers["total"] != float64(1) {
			t.Fatalf("total=%v", tiers["total"])
		}
		rows, ok := tiers["rows"]
		if ok != (detail == "true") {
			t.Fatalf("details=%s rows=%v", detail, tiers)
		}
		if ok && len(rows.([]any)) != 1 {
			t.Fatal(rows)
		}
		if v["coverage"] != nil {
			t.Fatalf("coverage should be null: %v", v["coverage"])
		}
		usage := v["usage"].(map[string]any)
		after := usage["after"].(map[string]any)
		_, exists := after["issues"]
		if exists != (detail == "true") {
			t.Fatalf("details=%s usage=%v", detail, after)
		}
		if detail == "true" {
			issues, ok := after["issues"].([]any)
			if !ok || len(issues) != 0 {
				t.Fatalf("empty issues: %v", after["issues"])
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
	gate.Store(true)
	type response struct {
		detail string
		w      *httptest.ResponseRecorder
	}
	responses := make(chan response, 2)
	for _, detail := range []string{"true", "false"} {
		go func(d string) {
			responses <- response{d, statsHTTP(s, token, "GET", "/api/stats?tables=tiers,usage&details="+d+"&x=concurrent", nil)}
		}(detail)
	}
	<-started
	<-started
	close(release)
	for i := 0; i < 2; i++ {
		got := <-responses
		w := got.w
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get(contract.Header) != contract.Version {
			t.Fatal(w.Code, w.Header())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		tiers := body["tiers"].(map[string]any)
		_, exists := tiers["rows"]
		if tiers["total"] != float64(1) || exists != (got.detail == "true") || body["coverage"] != nil {
			t.Fatalf("concurrent %s: %v", got.detail, body)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("concurrent calls=%d", calls.Load())
	}
}
