package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/hostdrift"
)

func TestHandoffContextPreservesParent(t *testing.T) {
	type valueKey struct{}
	parent, cancel := context.WithTimeout(context.WithValue(context.Background(), valueKey{}, "value"), time.Minute)
	defer cancel()
	if HandoffDone(parent) != nil {
		t.Fatal("unattached context has a notification")
	}
	signal := make(chan struct{})
	ctx := WithHandoff(parent, signal)
	if HandoffDone(ctx) != signal || ctx.Value(valueKey{}) != "value" || ctx.Done() != parent.Done() {
		t.Fatal("notification or parent context was lost")
	}
	deadline, ok := ctx.Deadline()
	want, _ := parent.Deadline()
	if !ok || !deadline.Equal(want) {
		t.Fatal("parent deadline was lost")
	}
	if HandoffDone(WithHandoff(ctx, nil)) != nil {
		t.Fatal("nil notification did not override parent")
	}
	close(signal)
	if ctx.Err() != nil || parent.Err() != nil {
		t.Fatal("handoff canceled the request")
	}
	select {
	case <-ctx.Done():
		t.Fatal("handoff closed request Done")
	default:
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("parent cancellation was lost")
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if WithHandoff(expired, signal).Err() != context.DeadlineExceeded {
		t.Fatal("parent expiry was lost")
	}
}

func TestHandoffGuardUsesCurrentState(t *testing.T) {
	var handingOff atomic.Bool
	calls := 0
	handler := RejectDuringHandoff(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Next", "called")
		w.WriteHeader(http.StatusAccepted)
	}), handingOff.Load)
	for _, state := range []bool{false, true, false} {
		handingOff.Store(state)
		before := calls
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/business", nil))
		if !state {
			if calls != before+1 || w.Code != 202 || w.Header().Get("X-Next") != "called" || w.Header().Get("Retry-After") != "" {
				t.Fatal("normal request did not reach next handler", w)
			}
			continue
		}
		if calls != before || w.Code != 503 || w.Header().Get("Retry-After") != "2" || w.Header().Get("X-Next") != "" {
			t.Fatal("handoff request was not rejected", w)
		}
		assertHandoffHeaders(t, w)
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing problem security header")
		}
		var actual contract.Problem
		if err := json.Unmarshal(w.Body.Bytes(), &actual); err != nil {
			t.Fatal(err)
		}
		want := contract.NewProblem(503, actual.Message, "")
		if actual.Message == "" || actual.Code != want.Code || actual.Recovery != want.Recovery || actual.ContractVersion != contract.Version || actual.Retryable != want.Retryable || actual.SafeToChange != want.SafeToChange {
			t.Fatalf("unexpected problem envelope: %+v", actual)
		}
	}
}

// Observe the notification lookup at the real wait select, after the initial
// dispatch attempt. This avoids sleeping to guess whether the handler is idle.
type handoffWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *handoffWaitContext) Value(key any) any {
	if _, ok := key.(handoffContextKey); ok {
		ctx.once.Do(func() { close(ctx.waiting) })
	}
	return ctx.Context.Value(key)
}

func handoffNextRequest(ctx context.Context, token, body string) *http.Request {
	r := httptest.NewRequest("POST", "http://localhost:8787/api/v1/jobs/next", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(contract.Header, contract.Version)
	return r
}

func startHandoffRequest(t *testing.T, handler http.Handler, r *http.Request) (*httptest.ResponseRecorder, <-chan struct{}) {
	t.Helper()
	w := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.ServeHTTP(w, r)
	}()
	return w, finished
}

func awaitHandoff(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the expected boundary within 5 seconds")
	}
}

func assertHandoffHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Header().Get(contract.Header) != contract.Version || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatal("existing response headers were lost", w.Header())
	}
}

func assertHandoffEmpty(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("expected bodyless 204, got %d %s", w.Code, w.Body.String())
	}
	assertHandoffHeaders(t, w)
}

func TestHandoffNextWakesIdleRequest(t *testing.T) {
	handler, _, token := workerFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	signal := make(chan struct{})
	ctx := &handoffWaitContext{Context: WithHandoff(parent, signal), waiting: make(chan struct{})}
	w, finished := startHandoffRequest(t, handler, handoffNextRequest(ctx, token, `{"capacity":2,"waitSeconds":60}`))
	t.Cleanup(func() { cancel(); awaitHandoff(t, finished) })
	awaitHandoff(t, ctx.waiting)
	close(signal)
	awaitHandoff(t, finished)
	assertHandoffEmpty(t, w)
	if parent.Err() != nil || ctx.Err() != nil {
		t.Fatal("notification canceled the request context")
	}
}

func TestHandoffNextAlreadyNotified(t *testing.T) {
	handler, _, token := workerFixture(t)
	signal := make(chan struct{})
	close(signal)
	ctx, cancel := context.WithCancel(WithHandoff(context.Background(), signal))
	defer cancel()
	w, finished := startHandoffRequest(t, handler, handoffNextRequest(ctx, token, `{"capacity":2,"waitSeconds":60}`))
	t.Cleanup(func() { cancel(); awaitHandoff(t, finished) })
	awaitHandoff(t, finished)
	assertHandoffEmpty(t, w)
}

func TestHandoffNextRequestCancellation(t *testing.T) {
	handler, _, token := workerFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &handoffWaitContext{Context: WithHandoff(parent, make(chan struct{})), waiting: make(chan struct{})}
	w, finished := startHandoffRequest(t, handler, handoffNextRequest(ctx, token, `{"capacity":2,"waitSeconds":60}`))
	t.Cleanup(func() { cancel(); awaitHandoff(t, finished) })
	awaitHandoff(t, ctx.waiting)
	cancel()
	awaitHandoff(t, finished)
	if ctx.Err() != context.Canceled || w.Body.Len() != 0 || len(w.Header()) != 0 {
		t.Fatal("cancellation did not terminate silently", w)
	}
}

func TestHandoffNextWithoutNotificationKeepsPolling(t *testing.T) {
	handler, _, token := workerFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &handoffWaitContext{Context: parent, waiting: make(chan struct{})}
	started := time.Now()
	w, finished := startHandoffRequest(t, handler, handoffNextRequest(ctx, token, `{"capacity":2,"waitSeconds":1}`))
	t.Cleanup(func() { cancel(); awaitHandoff(t, finished) })
	awaitHandoff(t, ctx.waiting)
	awaitHandoff(t, finished)
	if time.Since(started) < time.Second {
		t.Fatal("request without notification stopped waiting early")
	}
	assertHandoffEmpty(t, w)
}

func TestHandoffNextWithoutNotificationFindsNewJob(t *testing.T) {
	handler, db, token := workerFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &handoffWaitContext{Context: parent, waiting: make(chan struct{})}
	w, finished := startHandoffRequest(t, handler, handoffNextRequest(ctx, token, `{"capacity":2,"waitSeconds":60}`))
	t.Cleanup(func() { cancel(); awaitHandoff(t, finished) })
	awaitHandoff(t, ctx.waiting)
	id := prepareHandoffJob(t, handler, db, token)
	awaitHandoff(t, finished)
	assertHandoffJob(t, w, db, id)
}

func prepareHandoffJob(t *testing.T, handler http.Handler, db *sql.DB, token string) int64 {
	t.Helper()
	status, body := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2,"capabilities":{"repositories":["owner/repo"]}}`)
	if status != 200 {
		t.Fatal(status, body)
	}
	var id int64
	err := db.QueryRow(`INSERT INTO jobs(repo,issue,title,scope,view) VALUES('owner/repo',1,'handoff test',ARRAY['server/a.go'],'{"reviewHolds":0,"reviewHoldsLowerBound":0,"reviewHoldsUnknown":false}') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertHandoffJob(t *testing.T, w *httptest.ResponseRecorder, db *sql.DB, id int64) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("expected leased job, got %d %s", w.Code, w.Body.String())
	}
	assertHandoffHeaders(t, w)
	var body struct {
		Job struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"job"`
		Lease struct {
			Epoch     int64     `json:"epoch"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"lease"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var status, host string
	var epoch int64
	var expires time.Time
	if err := db.QueryRow("SELECT status,lease_host,lease_epoch,lease_expires_at FROM jobs WHERE id=$1", id).Scan(&status, &host, &epoch, &expires); err != nil {
		t.Fatal(err)
	}
	if body.Job.ID != id || body.Job.Status != "running" || status != "running" || host != "nas" || body.Lease.Epoch < 1 || body.Lease.Epoch != epoch || !body.Lease.ExpiresAt.Equal(expires) {
		t.Fatalf("response and persisted lease differ: %+v (%s, %s, %d, %s)", body, status, host, epoch, expires)
	}
}

func TestHandoffNextExistingImmediateResponses(t *testing.T) {
	handler, db, token := workerFixture(t)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, handoffNextRequest(context.Background(), token, `{"capacity":2}`))
	assertHandoffEmpty(t, w)
	id := prepareHandoffJob(t, handler, db, token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, handoffNextRequest(context.Background(), token, `{"capacity":2}`))
	assertHandoffJob(t, w, db, id)
}

func TestHandoffNextKeepsAcquiredLease(t *testing.T) {
	handler, db, token := workerFixture(t)
	id := prepareHandoffJob(t, handler, db, token)
	// Hold the delivery read while the acquisition transaction is in flight.
	// Handoff stops idle waiting, not the preparation of a selected job.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("LOCK TABLE usage_rows IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	signal := make(chan struct{})
	ctx, cancel := context.WithCancel(WithHandoff(context.Background(), signal))
	defer cancel()
	w, finished := startHandoffRequest(t, handler, handoffNextRequest(ctx, token, `{"capacity":2,"waitSeconds":60}`))
	t.Cleanup(func() { cancel(); awaitHandoff(t, finished) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE relation='usage_rows'::regclass AND NOT granted)").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatch did not reach the delivery read within 5 seconds")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-finished:
		t.Fatal("request completed before the delivery barrier")
	default:
	}
	close(signal)
	if ctx.Err() != nil {
		t.Fatal("notification canceled acquired job operations")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	awaitHandoff(t, finished)
	assertHandoffJob(t, w, db, id)
}

func TestHandoffNextPreservesRejections(t *testing.T) {
	handler, _, token := workerFixture(t)
	signal := make(chan struct{})
	close(signal)
	ctx := WithHandoff(context.Background(), signal)
	for _, tc := range []struct {
		name, body string
		status     int
		change     func(*http.Request)
	}{
		{"invalid capacity", `{"capacity":0}`, 400, nil},
		{"invalid wait", `{"capacity":2,"waitSeconds":61}`, 400, nil},
		{"invalid repos", `{"capacity":2,"repos":null}`, 400, nil},
		{"wrong host", `{"capacity":2,"host":"other"}`, 403, nil},
		{"unauthenticated", `{"capacity":2}`, 401, func(r *http.Request) { r.Header.Set("Authorization", "Bearer invalid") }},
		{"wrong origin", `{"capacity":2}`, 403, func(r *http.Request) { r.Header.Set("Origin", "http://other.example") }},
		{"wrong method", `{"capacity":2}`, 405, func(r *http.Request) { r.Method = "GET" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := handoffNextRequest(ctx, token, tc.body)
			if tc.change != nil {
				tc.change(r)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("expected %d, got %d %s", tc.status, w.Code, w.Body.String())
			}
			assertHandoffHeaders(t, w)
			var problem contract.Problem
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || problem.Code == "" || problem.ContractVersion != contract.Version {
				t.Fatalf("existing problem envelope lost: %+v %v", problem, err)
			}
		})
	}
}

func TestHandoffNextPreservesServiceFailure(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, false, func(options *WorkerRouteOptions) {
		options.HostDrift = &hostdrift.Service{DB: options.DB, SettingsVersion: func(context.Context) (string, error) {
			return "", contract.NewProblem(502, "設定の照合先を読み込めません。", "")
		}}
	})
	signal := make(chan struct{})
	close(signal)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, handoffNextRequest(WithHandoff(context.Background(), signal), token, `{"capacity":2,"waitSeconds":60}`))
	if w.Code != 502 {
		t.Fatalf("notification masked service failure: %d %s", w.Code, w.Body.String())
	}
	assertHandoffHeaders(t, w)
	var problem contract.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || problem.Code != "TEMPORARY_UNAVAILABLE" || problem.ContractVersion != contract.Version {
		t.Fatalf("existing service failure envelope lost: %+v %v", problem, err)
	}
}

func TestHandoffRejectsMissingStateReaderAtConstruction(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil handoff reader accepted")
		}
	}()
	RejectDuringHandoff(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil)
}
