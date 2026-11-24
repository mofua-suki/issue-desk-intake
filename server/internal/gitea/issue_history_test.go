package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloneIssueRowsMatchesJSONRoundTrip(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	var deep any = "leaf"
	for i := 0; i < 10000; i++ {
		deep = []any{deep}
	}
	cases := []struct {
		name string
		rows []map[string]any
	}{
		{"nested", []map[string]any{{"list": []any{map[string]any{"number": json.Number("1.20e+3"), "nil": nil, "bool": true}, "text", []any{}}, "emptyNumber": json.Number(""), "nilMap": map[string]any(nil), "nilSlice": []any(nil)}, nil}},
		{"nil rows", nil},
		{"unexpected types", []map[string]any{{"int": 42, "slice": []int{1, 2}}}},
		{"invalid utf8", []map[string]any{{string([]byte{0xff}): string([]byte{0xff})}}},
		{"invalid number", []map[string]any{{"number": json.Number("01")}}},
		{"unsupported", []map[string]any{{"channel": make(chan int)}}},
		{"cycle", []map[string]any{cycle}},
		{"too deep", []map[string]any{{"nested": deep}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, wantErr := json.Marshal(tc.rows)
			var want []map[string]any
			if wantErr == nil {
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.UseNumber()
				wantErr = dec.Decode(&want)
			}
			got, err := cloneIssueRows(tc.rows)
			if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, %v; want %#v, %v", got, err, want, wantErr)
			}
		})
	}
}

func TestIssuesForFilingSharesHistoryAndKeepsClosedSummary(t *testing.T) {
	requests := 0
	client := fixtureClient(t, func(_ *http.Request) (*http.Response, error) {
		requests++
		if requests != 1 {
			return response(200, `[]`), nil
		}
		return response(200, `[
			{"number":1,"state":"closed","title":"closed title","body":"closed body","user":{"id":1}},
			{"number":2,"state":"open","title":"open title","body":"open body","user":{"id":1}},
			{"number":3,"state":"closed","title":"pull","pull_request":{"merged":false}}
		]`), nil
	})
	summary, err := client.IssuesForFiling(t.Context(), "owner/repo")
	if err != nil || len(summary) != 3 {
		t.Fatal(summary, err)
	}
	if summary[0]["pull_request"] == nil || summary[1]["body"] != "open body" || summary[1]["user"] == nil || summary[2]["title"] != "closed title" || summary[2]["state"] != "closed" || summary[2]["number"] != json.Number("1") || len(summary[2]) != 3 {
		t.Fatal(summary)
	}
	summary[2]["title"] = "changed"
	all, err := client.Issues(t.Context(), "owner/repo", "all")
	if err != nil || len(all) != 3 || all[2]["title"] != "closed title" || all[2]["body"] != "closed body" || all[2]["user"] == nil {
		t.Fatal(all, err)
	}
}

func TestIssueHistoryCloneIndependence(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
			<-release
			return response(200, `[{"number":1,"nested":{"items":[{"value":"original"}]}}]`), nil
		}
		return response(200, `[]`), nil
	})
	type result struct {
		rows []map[string]any
		err  error
	}
	read := func(ctx context.Context, ch chan result) {
		rows, err := c.Issues(ctx, "owner/repo", "all")
		ch <- result{rows, err}
	}
	owner, waiter := make(chan result, 1), make(chan result, 1)
	go read(context.Background(), owner)
	<-started
	ctx := &observedWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	go read(ctx, waiter)
	<-ctx.waiting
	close(release)
	a, b := <-owner, <-waiter
	if a.err != nil || b.err != nil {
		t.Fatal(a.err, b.err)
	}
	item := func(rows []map[string]any) map[string]any {
		return rows[0]["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)
	}
	item(a.rows)["value"] = "changed"
	if item(b.rows)["value"] != "original" {
		t.Fatal(b.rows)
	}
	item(b.rows)["value"] = "also changed"
	next, err := c.Issues(context.Background(), "owner/repo", "all")
	if err != nil || item(next)["value"] != "original" {
		t.Fatal(next, err)
	}
	if requests.Load() != 2 {
		t.Fatal("unexpected request count", requests.Load())
	}
}

type blockedIssueJSON struct {
	started chan struct{}
	release chan struct{}
}

func (v blockedIssueJSON) MarshalJSON() ([]byte, error) {
	close(v.started)
	<-v.release
	return []byte(`"value"`), nil
}

func TestIssueHistoryCopyDoesNotBlockOtherRepository(t *testing.T) {
	const marker = "11111111-1111-4111-8111-111111111111"
	for _, waiter := range []bool{false, true} {
		name := "initiator"
		if waiter {
			name = "waiter"
		}
		t.Run(name, func(t *testing.T) {
			fetchStarted, fetchRelease := make(chan struct{}), make(chan struct{})
			var aCalls atomic.Int32
			c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/repo-a/") {
					if aCalls.Add(1) == 1 {
						return response(200, `[{"number":1}]`), nil
					}
					close(fetchStarted)
					<-fetchRelease
					return response(200, `[]`), nil
				}
				return response(200, `[{"number":2,"body":"<!-- issue-desk:`+marker+` -->"}]`), nil
			})
			if _, err := c.Issues(context.Background(), "owner/repo-a", "all"); err != nil {
				t.Fatal(err)
			}
			copyStarted, copyRelease := make(chan struct{}), make(chan struct{})
			c.history.mu.Lock()
			c.history.repos["owner/repo-a"].rows[1]["sync"] = blockedIssueJSON{copyStarted, copyRelease}
			c.history.mu.Unlock()
			var releaseOnce sync.Once
			releaseCopy := func() { releaseOnce.Do(func() { close(copyRelease) }) }
			defer releaseCopy()
			owner := make(chan error, 1)
			go func() {
				var err error
				if waiter {
					_, err = c.IssuesByMarker(context.Background(), "owner/repo-a", marker)
				} else {
					_, err = c.Issues(context.Background(), "owner/repo-a", "all")
				}
				owner <- err
			}()
			<-fetchStarted
			var waiting chan error
			if waiter {
				waiting = make(chan error, 1)
				ctx := &observedWaitContext{Context: context.Background(), waiting: make(chan struct{})}
				go func() { _, err := c.Issues(ctx, "owner/repo-a", "all"); waiting <- err }()
				<-ctx.waiting
			}
			close(fetchRelease)
			<-copyStarted
			other := make(chan error, 1)
			go func() {
				rows, err := c.IssuesByMarker(context.Background(), "owner/repo-b", marker)
				if err == nil && (len(rows) != 1 || fmt.Sprint(rows[0]["number"]) != "2") {
					err = fmt.Errorf("unexpected marker rows: %v", rows)
				}
				other <- err
			}()
			if err := <-other; err != nil {
				t.Fatal(err)
			}
			releaseCopy()
			if err := <-owner; err != nil {
				t.Fatal(err)
			}
			if waiter {
				if err := <-waiting; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestIssueHistoryUsesIncrementalBoundaryAndFiveMinuteReconciliation(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	queries := []string{}
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		queries = append(queries, r.URL.RawQuery)
		switch len(queries) {
		case 1:
			return response(200, `[{"number":2,"state":"open","user":{"login":"owner"}},{"number":1,"state":"open"}]`), nil
		case 2:
			if r.URL.Query().Get("since") != "2026-10-31T23:59:59Z" {
				t.Fatal(r.URL.String())
			}
			return response(200, `[{"number":3,"state":"open"},{"number":1,"state":"closed"}]`), nil
		default:
			if r.URL.Query().Get("since") != "" {
				t.Fatal("全件の期限でも差分取得", r.URL.String())
			}
			return response(200, `[{"number":2,"state":"closed"}]`), nil
		}
	})
	c.history.now = func() time.Time { return now }
	first, e := c.Issues(context.Background(), "owner/repo", "all")
	if e != nil {
		t.Fatal(e)
	}
	first[0]["state"] = "caller-mutation"
	first[0]["user"].(map[string]any)["login"] = "changed"
	now = now.Add(time.Minute)
	second, e := c.Issues(context.Background(), "owner/repo", "all")
	if e != nil || len(second) != 3 {
		t.Fatal(second, e)
	}
	if fmt.Sprint(second[0]["number"]) != "3" || second[1]["state"] != "open" || second[1]["user"].(map[string]any)["login"] != "owner" || second[2]["state"] != "closed" {
		t.Fatal(second)
	}
	now = now.Add(4 * time.Minute)
	third, e := c.Issues(context.Background(), "owner/repo", "all")
	if e != nil || len(third) != 1 || third[0]["state"] != "closed" {
		t.Fatal(third, e)
	}
}
func TestIssueHistorySharesInflightAcrossPublicCopiesAndWaiterCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) != 1 {
			return nil, fmt.Errorf("重複全件取得")
		}
		close(started)
		<-release
		return response(200, `[{"number":1}]`), nil
	})
	copy, e := c.WithPublicURL("https://public.example")
	if e != nil {
		t.Fatal(e)
	}
	owner := make(chan error, 1)
	go func() { _, e := c.Issues(context.Background(), "owner/repo", "all"); owner <- e }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, e = copy.Issues(ctx, "owner/repo", "all"); e != context.DeadlineExceeded {
		t.Fatal(e)
	}
	close(release)
	if e = <-owner; e != nil || requests.Load() != 1 {
		t.Fatal(e, requests.Load())
	}
}
func TestIssueHistoryFailurePreservesBoundaryAndFilteredReadsStaySeparate(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fail := false
	since := []string{}
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("state") == "open" {
			return response(200, `[{"number":9,"state":"open"}]`), nil
		}
		since = append(since, r.URL.Query().Get("since"))
		if fail {
			return response(403, `{"message":"unavailable"}`), nil
		}
		return response(200, `[{"number":1,"state":"closed"}]`), nil
	})
	c.history.now = func() time.Time { return now }
	if _, e := c.Issues(context.Background(), "owner/repo", "all"); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Minute)
	fail = true
	if _, e := c.Issues(context.Background(), "owner/repo", "all"); e == nil {
		t.Fatal("取得失敗を成功扱いしました")
	}
	fail = false
	now = now.Add(time.Minute)
	if _, e := c.Issues(context.Background(), "owner/repo", "open"); e != nil {
		t.Fatal(e)
	}
	rows, e := c.Issues(context.Background(), "owner/repo", "all")
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	if len(since) != 3 || since[1] != since[2] || strings.Contains(fmt.Sprint(rows), "9") {
		t.Fatal(since, rows)
	}
}
func TestIssueHistoryConcurrentReadersUseOneFullRequest(t *testing.T) {
	var calls atomic.Int32
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		return response(200, `[{"number":1}]`), nil
	})
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			rows, e := c.Issues(context.Background(), "owner/repo", "all")
			if e != nil || len(rows) != 1 {
				t.Error(rows, e)
			}
			if len(rows) > 0 {
				rows[0]["number"] = json.Number("999")
			}
		}()
	}
	close(gate)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("同時全件取得を共有していません", calls.Load())
	}
}

type observedWaitContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestIssueHistoryRetriesWhenSharedOwnerIsCanceled(t *testing.T) {
	started := make(chan struct{})
	var requests atomic.Int32
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return response(200, `[{"number":1}]`), nil
	})
	ownerCtx, cancel := context.WithCancel(context.Background())
	owner := make(chan error, 1)
	go func() { _, err := c.Issues(ownerCtx, "owner/repo", "all"); owner <- err }()
	<-started
	waiter := make(chan error, 1)
	waitCtx := &observedWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	go func() {
		rows, err := c.Issues(waitCtx, "owner/repo", "all")
		if err == nil && len(rows) != 1 {
			err = fmt.Errorf("取得件数 %d", len(rows))
		}
		waiter <- err
	}()
	<-waitCtx.waiting
	cancel()
	if err := <-owner; err == nil {
		t.Fatal("取消しを検出しない")
	}
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("継続側が完了しない")
	}
	if requests.Load() != 2 {
		t.Fatalf("取得回数 %d", requests.Load())
	}
}

func TestIssueHistoryMarkerIndex(t *testing.T) {
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	responses := []string{
		`[{"number":1,"body":"<!-- issue-desk:` + a + ` -->","state":"closed"},{"number":2,"body":"<!-- issue-desk:` + b + ` --> <!-- issue-desk:` + a + ` -->"},{"number":3,"body":"none"}]`,
		`[{"number":2,"body":"<!-- issue-desk:` + b + ` -->"},{"number":3,"body":"<!-- issue-desk:` + a + ` -->"},{"number":4,"body":"<!-- issue-desk:` + a + ` -->"}]`,
		`[{"number":1,"body":"removed"}]`, `[]`, `[]`,
	}
	calls := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		v := responses[calls]
		calls++
		return response(200, v), nil
	})
	check := func(key string, want ...string) {
		t.Helper()
		rows, err := c.IssuesByMarker(context.Background(), "owner/repo", key)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, row := range rows {
			got = append(got, fmt.Sprint(row["number"]))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	check(a, "2", "1")
	check(a, "4", "3", "1")
	check(a, "4", "3")
	check(b, "2")
	// A nonmatching cached row must not be cloned during the empty-delta
	// marker lookup: JSON cannot clone a channel if the whole history is used.
	c.history.repos["owner/repo"].rows[1]["cloneGuard"] = make(chan int)
	check(a, "4", "3")
	if calls != 5 {
		t.Fatal(calls)
	}
}
