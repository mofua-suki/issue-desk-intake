package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/leadership"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/runtimegate"
)

type handoffLeaseFunc func()

func (f handoffLeaseFunc) Release() { f() }

func handoffAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("交代の完了境界へ到達しない")
	}
}

func handoffMinimal(t *testing.T) (*App, *Handoff, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: t.TempDir(), Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{ctx: ctx, cancel: cancel, Gate: gate}
	t.Cleanup(app.Close)
	releases := &atomic.Int32{}
	h, err := NewHandoff(app, handoffLeaseFunc(func() { releases.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	return app, h, releases
}

func TestHandoffDeadlineDuplicatesAndLateCompletion(t *testing.T) {
	app, h, releases := handoffMinimal(t)
	entered, finish, late := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	app.startBackgroundTask(func(ctx context.Context) {
		<-ctx.Done()
		close(entered)
		<-finish
		close(late)
	})
	var logs []string
	h.log = func(line string) { logs = append(logs, line) }
	parent, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	results := make(chan error, 21)
	go func() { results <- h.Run(parent) }()
	handoffAwait(t, entered)
	for i := 0; i < 20; i++ {
		go func() { results <- h.Run(context.Background()) }()
	}
	first := <-results
	var failure *HandoffError
	if !errors.As(first, &failure) || !errors.Is(first, context.DeadlineExceeded) || failure.Stage != "stage1" || failure.Class != "timeout" || !strings.Contains(strings.Join(failure.Pending, ","), "background") {
		t.Fatal("期限切れを正しく返さない", first)
	}
	for i := 0; i < 20; i++ {
		if result := <-results; result != first {
			t.Fatal("重複通知が同じ結果を共有しない", result)
		}
	}
	if app.ctx.Err() != nil || releases.Load() != 0 {
		t.Fatal("失敗後に業務寿命・ロックを終了した")
	}
	once.Do(func() { close(finish) })
	handoffAwait(t, late)
	if h.Run(context.Background()) != first || releases.Load() != 0 {
		t.Fatal("遅延完了後に再開始した")
	}
	if err := app.Gate.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"ai", "filing", "project"} {
		if _, release, err := app.Gate.Acquire(context.Background(), kind); err == nil {
			release()
			t.Fatal("失敗・drain再照合後に新規開始した", kind)
		}
	}
	h.HTTP.mu.Lock()
	stopped := h.HTTP.stopped
	h.HTTP.mu.Unlock()
	if stopped || len(logs) != 2 || !strings.Contains(logs[1], "result=failed class=timeout elapsed_ms=") {
		t.Fatal("段1失敗後に段2へ進んだ、または記録が不足", logs)
	}
}

func TestHandoffCommonDeadlineAndMonotonicRecords(t *testing.T) {
	_, h, releases := handoffMinimal(t)
	var mu sync.Mutex
	var deadlines []time.Time
	var remaining []time.Duration
	var logs []string
	h.log = func(line string) { logs = append(logs, line) }
	observe := func(step handoffStep, delay bool) handoffStep {
		return handoffStep{step.name, func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				return errors.New("共通期限がない")
			}
			mu.Lock()
			deadlines = append(deadlines, deadline)
			remaining = append(remaining, time.Until(deadline))
			mu.Unlock()
			if delay {
				time.Sleep(20 * time.Millisecond)
			}
			return step.run(ctx)
		}}
	}
	for i := range h.stage1 {
		h.stage1[i] = observe(h.stage1[i], true)
	}
	for i := range h.stage2 {
		h.stage2[i] = observe(h.stage2[i], false)
	}
	for i := range h.accepted {
		h.accepted[i] = observe(h.accepted[i], false)
	}
	h.resources, h.lease = observe(h.resources, false), observe(h.lease, false)
	started := time.Now()
	if err := h.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 || len(deadlines) < 6 || deadlines[0].Sub(started) < HandoffLimit-time.Second || deadlines[0].Sub(started) > HandoffLimit+time.Second {
		t.Fatal("60秒の上限または呼出し数", deadlines, releases.Load())
	}
	for _, deadline := range deadlines {
		if deadline != deadlines[0] {
			t.Fatal("期限または単調時計を再生成した", deadlines)
		}
	}
	if remaining[len(remaining)-1] >= remaining[0]-10*time.Millisecond {
		t.Fatal("残り時間が減少しない", remaining)
	}
	if len(logs) != 4 {
		t.Fatal("段の開始・完了が不足", logs)
	}
	previous := int64(-1)
	for i, line := range logs {
		var stage, result, class string
		var elapsed int64
		if n, _ := fmt.Sscanf(line, "handoff stage=%s result=%s class=%s elapsed_ms=%d", &stage, &result, &class, &elapsed); n != 4 || elapsed < previous {
			t.Fatal("安全な記録と単調な経過時間", line)
		}
		want := []string{"stage1/start", "stage1/complete", "stage2/start", "stage2/complete"}[i]
		if stage+"/"+result != want || class != "none" {
			t.Fatal("記録の順序", line)
		}
		previous = elapsed
	}
	deploy, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nas", "deploy-server.sh"))
	if err != nil || !strings.Contains(string(deploy), "HANDOFF_MS=75000") || HandoffLimit != 60000*time.Millisecond {
		t.Fatal("出荷する配置手順との予約時間の互換性", err)
	}
}

func TestHandoffFailureCancellationAtEveryWait(t *testing.T) {
	// Inject an error or a contextless delayed completion at every coordinator
	// wait. Actual service and storage contracts are exercised below with OpenApp.
	for _, target := range []string{"start_fence", "background", "consultations", "notifications", "stage1_idle", "http_boundary", "sse", "http_requests", "gate_idle", "projects", "issues", "resources", "lease"} {
		for _, outcome := range []string{"failure", "cancel", "timeout"} {
			if target == "http_requests" && outcome == "failure" {
				continue // handlers return HTTP responses, not coordinator errors
			}
			t.Run(target+"/"+outcome, func(t *testing.T) {
				app, h, releases := handoffMinimal(t)
				noop := func(context.Context) error { return nil }
				h.stage1 = append(h.stage1, handoffStep{"consultations", noop}, handoffStep{"notifications", noop})
				h.stage2 = append(h.stage2, handoffStep{"sse", noop})
				h.accepted = append(h.accepted, handoffStep{"projects", noop}, handoffStep{"issues", noop})
				entered, finish, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var finishOnce sync.Once
				defer finishOnce.Do(func() { close(finish) })
				parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				injected := func(context.Context) error {
					close(entered)
					defer close(completed)
					if outcome == "failure" {
						return errors.New("SECRET request-body credential")
					}
					<-finish // deliberately ignores cancellation, like a contextless operation
					return nil
				}
				patch := func(steps []handoffStep) {
					for i := range steps {
						if steps[i].name == target {
							original := steps[i].run
							steps[i].run = func(ctx context.Context) error {
								if err := original(ctx); err != nil {
									return err
								}
								return injected(ctx)
							}
						}
					}
				}
				patch(h.stage1)
				patch(h.stage2)
				// The two gate-idle waits use the same callback; distinguish the
				// first (stage1) from the accepted-work barrier in stage2.
				if target == "stage1_idle" || target == "gate_idle" {
					original := h.accepted[0].run
					var calls atomic.Int32
					h.accepted[0].run = func(ctx context.Context) error {
						if err := original(ctx); err != nil {
							return err
						}
						n := calls.Add(1)
						if target == "stage1_idle" && n == 1 || target == "gate_idle" && n == 2 {
							return injected(ctx)
						}
						return nil
					}
				} else {
					patch(h.accepted)
				}
				if target == "http_requests" {
					// Keep a real admitted handler unfinished so the production HTTP
					// Wait, rather than a substituted wait, observes cancellation.
					go h.HTTP.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { _ = injected(context.Background()) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/write", nil))
					handoffAwait(t, entered)
				}
				if target == "resources" {
					h.resources.run = injected
				}
				if target == "lease" {
					h.lease.run = injected
				}
				var logs []string
				h.log = func(line string) { logs = append(logs, line) }
				result := make(chan error, 1)
				go func() { result <- h.Run(parent) }()
				handoffAwait(t, entered)
				if outcome == "cancel" {
					cancel()
				}
				err := <-result
				var failure *HandoffError
				if !errors.As(err, &failure) || len(failure.Pending) == 0 || strings.Contains(err.Error()+strings.Join(logs, ""), "SECRET") {
					t.Fatal("失敗・未完了または安全なログ", err, logs)
				}
				wantClass := map[string]string{"failure": "operation_failed", "cancel": "cancelled", "timeout": "timeout"}[outcome]
				if failure.Class != wantClass {
					t.Fatal("失敗分類", failure)
				}
				if releases.Load() != 0 {
					t.Fatal("未完了のままロック返却")
				}
				if target != "lease" && app.ctx.Err() != nil {
					t.Fatal("完了前に資源解放")
				}
				finishOnce.Do(func() { close(finish) })
				handoffAwait(t, completed)
				if h.Run(context.Background()) != err || releases.Load() != 0 {
					t.Fatal("遅延完了で次段へ進んだ")
				}
			})
		}
	}
}

func TestHandoffHTTPConcurrentAdmissionAndWrites(t *testing.T) {
	var h HandoffHTTP
	var accepted, saved atomic.Int32
	finish := make(chan struct{})
	entered := make(chan struct{}, 100)
	handler := h.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted.Add(1)
		entered <- struct{}{}
		<-finish
		if r.Context().Err() != nil {
			t.Error("段2で受理済み要求を取り消した")
		}
		saved.Add(1)
		w.WriteHeader(201)
	}))
	var requests sync.WaitGroup
	for i := 0; i < 100; i++ {
		requests.Add(1)
		go func() {
			defer requests.Done()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/write", nil))
			if w.Code != 201 && (w.Code != 503 || w.Header().Get("Retry-After") != "2") {
				t.Error("受理境界の応答", w.Code)
			}
		}()
	}
	<-entered
	h.Begin()
	h.Begin()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("書き込みを待たなかった", err)
	}
	close(finish)
	requests.Wait()
	if err := h.Wait(context.Background()); err != nil || accepted.Load() == 0 || accepted.Load() != saved.Load() {
		t.Fatal("受理済みの保存数", err, accepted.Load(), saved.Load())
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/write", nil))
	if w.Code != 503 || w.Header().Get("Retry-After") != "2" {
		t.Fatal("返却後の新規要求", w.Code)
	}
}

// A flush barrier observes a real authenticated SSE connection before cutover.
type handoffFlushRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func (w *handoffFlushRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.flushed) })
}

func TestHandoffOpenAppTwoStagesPostgres(t *testing.T) {
	pg := appPostgres(t)
	app, _, _, pc, _ := binaryAppFixture(t, pg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.SQLite.CreateSession(ctx, auth.Hash("handoff-session"), auth.Session{CSRF: "handoff-csrf", ExpiresAt: time.Now().Add(time.Hour)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	background, finishBackground := make(chan struct{}), make(chan struct{})
	var backgroundOnce sync.Once
	defer backgroundOnce.Do(func() { close(finishBackground) })
	app.startBackgroundTask(func(ctx context.Context) { <-ctx.Done(); close(background); <-finishBackground })
	var releases atomic.Int32
	h, err := NewHandoff(app, handoffLeaseFunc(func() {
		var ready bool
		if err := pg.QueryRowContext(ctx, "SELECT ready FROM hosts WHERE host='nas'").Scan(&ready); err != nil || !ready {
			t.Error("返却前に受理済みHTTPを書き込まない", ready, err)
		}
		if app.ctx.Err() == nil || app.SQLite.DB().PingContext(ctx) == nil {
			t.Error("保存資源より先にロックを返した")
		}
		releases.Add(1)
	}))
	if err != nil {
		t.Fatal(err)
	}
	writeEntered, finishWrite, writeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var writeOnce sync.Once
	defer writeOnce.Do(func() { close(finishWrite) })
	handler := h.HTTP.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && r.URL.Path == "/api/v1/hosts/nas" {
			close(writeEntered)
			<-finishWrite
			if r.Context().Err() != nil {
				t.Error("受理済み書き込みのcontextを取り消した")
			}
		}
		app.Mux.ServeHTTP(w, r)
	}))
	request := func(method, path, body string, screen bool) *http.Request {
		r := httptest.NewRequest(method, app.Config.PublicURL+path, strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(contract.Header, contract.Version)
		if screen {
			r.AddCookie(&http.Cookie{Name: "desk_session", Value: "handoff-session"})
			r.Header.Set("Origin", app.Config.PublicURL)
			r.Header.Set("X-CSRF-Token", "handoff-csrf")
		} else {
			r.Header.Set("Authorization", "Bearer "+pc)
		}
		return r
	}
	writeResponse := httptest.NewRecorder()
	go func() {
		defer close(writeDone)
		handler.ServeHTTP(writeResponse, request("PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2,"capabilities":{"repositories":["owner/repo"]}}`, false))
	}()
	handoffAwait(t, writeEntered)
	sse := &handoffFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	sseDone := make(chan struct{})
	go func() { defer close(sseDone); handler.ServeHTTP(sse, request("GET", "/api/events", "", true)) }()
	handoffAwait(t, sse.flushed)
	nextDone := make(chan struct{})
	next := httptest.NewRecorder()
	go func() {
		defer close(nextDone)
		handler.ServeHTTP(next, request("POST", "/api/v1/jobs/next", `{"capacity":2,"waitSeconds":60}`, false))
	}()
	for {
		h.HTTP.mu.Lock()
		active := h.HTTP.active
		h.HTTP.mu.Unlock()
		if active == 3 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("長時間待機が受理されない", ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	// This ticket represents filing accepted before the permanent fence. Only
	// live filing/project tickets, never an AI turn alone, permit continuation.
	accepted, releaseFiling, err := app.Gate.Acquire(ctx, "filing")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFiling()
	stage2 := make(chan struct{})
	var logs []string
	h.log = func(line string) {
		logs = append(logs, line)
		if strings.Contains(line, "stage=stage2 result=start") {
			close(stage2)
		}
	}
	result := make(chan error, 1)
	go func() { result <- h.Run(ctx) }()
	handoffAwait(t, background)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request("GET", "/api/session", "", true))
	if w.Code != 200 || app.ctx.Err() != nil {
		_, _, sessionErr := app.SQLite.Session(ctx, auth.Hash("handoff-session"))
		t.Fatal("段1の業務応答とactiveの寿命", w.Code, w.Body.String(), "request context", ctx.Err(), "app context", app.ctx.Err(), "session read", sessionErr)
	}
	// Wait for the actual gate fence, without assuming goroutine scheduling.
	for {
		_, release, err := app.Gate.Acquire(ctx, "project")
		if err != nil {
			break
		}
		release()
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
	continuing, releaseAI, err := app.Gate.Acquire(accepted, "ai")
	if err != nil || continuing.Err() != nil {
		t.Fatal("受理済み起票の継続", err)
	}
	releaseAI()
	if err := app.Gate.Tick(); err != nil {
		t.Fatal(err)
	}
	if _, release, err := app.Gate.Acquire(ctx, "filing"); err == nil {
		release()
		t.Fatal("drain解除で開始抑止が解けた")
	}
	releaseFiling()
	backgroundOnce.Do(func() { close(finishBackground) })
	handoffAwait(t, stage2)
	// Wait until the synchronized boundary has actually switched.
	for {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, request("GET", "/api/session", "", true))
		if w.Code == 503 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
	if w.Header().Get("Retry-After") != "2" {
		t.Fatal("段2のRetry-After", w.Header())
	}
	handoffAwait(t, sseDone)
	handoffAwait(t, nextDone)
	if next.Code != 204 || releases.Load() != 0 || app.ctx.Err() != nil {
		t.Fatal("長時間待機終了と受理済みHTTP保護", next.Code, releases.Load(), app.ctx.Err())
	}
	writeOnce.Do(func() { close(finishWrite) })
	handoffAwait(t, writeDone)
	if writeResponse.Code != 200 {
		t.Fatal("受理済み書き込み", writeResponse.Code, writeResponse.Body.String())
	}
	if err := <-result; err != nil || releases.Load() != 1 {
		t.Fatal("交代完了", err, releases.Load())
	}
	app.Close() // overlaps the normal cleanup owner without a second actual close
	if h.Run(context.Background()) != nil || releases.Load() != 1 || len(logs) != 4 {
		t.Fatal("完了通知を再実行", releases.Load(), logs)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, request("PUT", "/api/v1/hosts/nas", `{"ready":false}`, false))
	var ready bool
	if err := pg.QueryRowContext(ctx, "SELECT ready FROM hosts WHERE host='nas'").Scan(&ready); w.Code != 503 || err != nil || !ready {
		t.Fatal("返却後に業務書き込み", w.Code, ready, err)
	}
}

func TestHandoffAutoSubmitPreservesSavedStatePostgres(t *testing.T) {
	pg := appPostgres(t)
	app, _, _, _, _ := binaryAppFixture(t, pg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Replace only the AI boundary: use the shipped Engine, Records and Filing
	// with their real Gate and PostgreSQL formats, without an AI process.
	app.Engine.Close()
	started, finish, submitted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	defer finishOnce.Do(func() { close(finish) })
	var submitErr error
	engine, err := consultations.NewEngine(app.ctx, consultations.EngineOptions{Service: app.Consultations,
		Turn: func(run context.Context, _ consultations.Record) (consultations.Record, error) {
			close(started)
			select {
			case <-finish:
				return consultations.Record{"reply": "保存された回答", "title": "件名", "body": "本文", "ready": true, "questions": []any{}}, nil
			case <-run.Done():
				return nil, run.Err()
			}
		},
		Submit: func(run context.Context, d consultations.Record) (consultations.Record, error) {
			defer close(submitted)
			result, err := app.Filing.SubmitAccepted(run, d["id"].(string), consultations.Record{"revision": d["revision"]}, auth.Principal{Kind: auth.Screen})
			submitErr = err
			return result, err
		}, Report: func(error) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	app.Engine = engine
	app.Consultations.BindEngine(engine)
	id := "77777777-7777-4777-8777-777777777777"
	draft, err := app.Records.Change(ctx, "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
		return consultations.Record{"id": id, "project": "owner/repo", "status": "thinking", "revision": 0, "hold": false, "messages": []any{}, "questions": []any{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Start(ctx, id, draft, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	handoffAwait(t, started)
	var releases atomic.Int32
	h, err := NewHandoff(app, handoffLeaseFunc(func() { releases.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	app.startBackgroundTask(func(stop context.Context) { <-stop.Done(); close(blocked); <-finish })
	result := make(chan error, 1)
	go func() { result <- h.Run(ctx) }()
	handoffAwait(t, blocked)
	// Observe the gate fence before releasing the AI result, ensuring Submit
	// begins after filing admission has closed, rather than merely after Run.
	for {
		_, release, err := app.Gate.Acquire(ctx, "filing")
		if err != nil {
			break
		}
		release()
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
	finishOnce.Do(func() { close(finish) })
	handoffAwait(t, submitted)
	var problem *contract.Problem
	if !errors.As(submitErr, &problem) || problem.Status != 503 || problem.Code != "SERVER_DRAINING" {
		t.Fatal("自動起票の受理を区別しない", submitErr)
	}
	if err := <-result; err != nil || releases.Load() != 1 {
		t.Fatal("想定内の新規開始拒否が交代を失敗させた", err)
	}
	restored := openAppStore(t, pgtest.Env{DSN: app.Config.AppPostgresDSN, Schema: app.Config.ApplicationSchema})
	records, err := consultations.OpenRecords(ctx, restored.DB())
	if err != nil {
		t.Fatal(err)
	}
	saved, ok, err := records.Get(ctx, "drafts", id)
	if err != nil || !ok || saved["status"] != "auto_ready" || saved["ready"] != true || saved["attempted"] == true || saved["issue"] != nil || len(saved["messages"].([]any)) != 1 {
		t.Fatal("次の書き手へ再開状態を保持しない", saved, err)
	}
	var count int
	if err := pg.QueryRowContext(ctx, "SELECT count(*) FROM jobs").Scan(&count); err != nil || count != 0 {
		t.Fatal("抑止後に新規送信・配布した", count, err)
	}
}

func TestHandoffRealLeaseAndNormalCleanupPostgres(t *testing.T) {
	env := pgtest.NewIsolated(t)
	app, _, _ := handoffMinimal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, ok, err := leadership.Try(ctx, env.DSN, leadership.Options{})
	if err != nil || !ok {
		t.Fatal("隔離DBのロックを取得できない", err)
	}
	defer lease.Release()
	h, err := NewHandoff(app, lease)
	if err != nil {
		t.Fatal(err)
	}
	// Normal cleanup and the handoff owner share App.closeOnce. Begin the
	// overlap only after accepted-work barriers have completed.
	original := h.resources.run
	h.resources.run = func(ctx context.Context) error {
		var cleanup sync.WaitGroup
		cleanup.Add(1)
		go func() { defer cleanup.Done(); app.Close() }()
		err := original(ctx)
		cleanup.Wait()
		return err
	}
	if err = h.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var cleanup sync.WaitGroup
	for i := 0; i < 10; i++ {
		cleanup.Add(1)
		go func() { defer cleanup.Done(); lease.Release(); app.Close() }()
	}
	cleanup.Wait()
	next, acquired, err := leadership.Try(ctx, env.DSN, leadership.Options{})
	if err != nil || !acquired {
		t.Fatal("最後に書き手ロックを返していない", err)
	}
	next.Release()
}

func TestHandoffResourceCloseFailureKeepsLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: dir, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Tick(); err != nil {
		t.Fatal(err)
	}
	app := &App{ctx: ctx, cancel: cancel, Gate: gate}
	defer app.Close()
	var releases atomic.Int32
	h, err := NewHandoff(app, handoffLeaseFunc(func() { releases.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	// A real resource failure, not an injected return: final Gate heartbeat
	// cannot be written when its runtime state directory has disappeared.
	if err := os.RemoveAll(filepath.Join(dir, "state")); err != nil {
		t.Fatal(err)
	}
	var failure *HandoffError
	if err := h.Run(context.Background()); !errors.As(err, &failure) || failure.Stage != "stage2" || failure.Operation != "resources" || failure.Class != "operation_failed" || releases.Load() != 0 {
		t.Fatal("資源解放失敗後にロックを返した", err)
	}
}

func TestHandoffAcceptedFilingAndDatabaseSavePostgres(t *testing.T) {
	pg := appPostgres(t)
	app, _, dir, _, _ := binaryAppFixture(t, pg)
	// The auxiliary filing service below has a separate HTTP history fixture.
	// Finish OpenApp's initial recovery before adding its historical record;
	// otherwise two different fixture histories could reconcile the same row.
	app.StopBackground()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := "88888888-8888-4888-8888-888888888888"
	issue := map[string]any{"number": 9, "title": "受理済みの起票", "body": "保存する本文\n<!-- issue-desk:" + id + " -->", "state": "open", "user": map[string]any{"id": 1}, "created_at": "2026-11-01T00:00:00.000Z"}
	entered, finish := make(chan struct{}), make(chan struct{})
	var finishOnce, enteredOnce sync.Once
	defer finishOnce.Do(func() { close(finish) })
	var posts atomic.Int32
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			posts.Add(1)
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-finish:
			case <-r.Context().Done():
				return
			}
			json.NewEncoder(w).Encode([]any{issue})
		case "/api/v1/repos/owner/repo/issues/9":
			json.NewEncoder(w).Encode(issue)
		default:
			w.WriteHeader(404)
		}
	}))
	defer git.Close()
	client, err := gitea.New(git.URL, filepath.Join(dir, "intake-token"), nil) // known fixture file
	if err != nil {
		t.Fatal(err)
	}
	service, err := filing.New(ctx, filing.Options{Records: app.Records, Jobs: app.Jobs, API: client, Acquire: app.Gate.Acquire, Project: app.Project, Own: app.Own,
		Build: func(context.Context, consultations.Record) (filing.Prepared, error) {
			return filing.Prepared{}, errors.New("送信結果不明の起票は再生成しない")
		},
		Check: func(context.Context, consultations.Record) error {
			return errors.New("保存済みpreparedJobを再検証しない")
		},
		Link: func(repo string, n int64) string { return fmt.Sprintf("/%s/issues/%d", repo, n) },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.Records.Change(ctx, "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
		return consultations.Record{"id": id, "project": "owner/repo", "status": "submit_unknown", "attempted": true, "revision": 0, "messages": []any{}, "preparedJob": map[string]any{"Scope": []any{"server/a.go"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	filingResult := make(chan error, 1)
	go func() {
		_, err := service.SubmitAccepted(app.ctx, id, consultations.Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
		filingResult <- err
	}()
	handoffAwait(t, entered) // Gate ticket and durable submitting intent already exist
	tx, err := app.SQLite.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE drafts SET json=json WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err := tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	reader := openAppStore(t, pgtest.Env{DSN: app.Config.AppPostgresDSN, Schema: app.Config.ApplicationSchema})
	records, err := consultations.OpenRecords(ctx, reader.DB())
	if err != nil {
		t.Fatal(err)
	}
	var releases atomic.Int32
	h, err := NewHandoff(app, handoffLeaseFunc(func() {
		saved, found, err := records.Get(ctx, "drafts", id)
		if err != nil || !found || saved["status"] != "submitted" {
			t.Error("ロック返却前の起票結果保存", saved, err)
		}
		var count int
		if err := pg.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE repo='owner/repo' AND issue=9").Scan(&count); err != nil || count != 1 {
			t.Error("ロック返却前の配布結果保存", count, err)
		}
		releases.Add(1)
	}))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- h.Run(ctx) }()
	for {
		_, release, err := app.Gate.Acquire(ctx, "filing")
		if err != nil {
			break
		}
		release()
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
	finishOnce.Do(func() { close(finish) })
	for {
		var blocked int
		if err := pg.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))", blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			break
		}
		select {
		case err := <-filingResult:
			t.Fatal("保存待機に到達せず終了", err)
		default:
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-result:
		t.Fatal("起票の保存完了前に交代した", err)
	default:
	}
	if app.ctx.Err() != nil || releases.Load() != 0 {
		t.Fatal("保存待機中に寿命・ロックを終了")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-filingResult; err != nil {
		t.Fatal("受理済み起票の継続", err)
	}
	if err := <-result; err != nil || releases.Load() != 1 || posts.Load() != 0 {
		t.Fatal("保存完了後の交代・結果不明の再投稿禁止", err, releases.Load(), posts.Load())
	}
}

// The real coordinator must stop recovery and drain admitted handlers before
// entering the WaitGroup-based issue service wait.
func TestHandoffDrainsRecoveryAndRequestsBeforeIssueWait(t *testing.T) {
	app, _, _ := handoffMinimal(t)
	app.Issues = &issueapi.Service{}
	h, err := NewHandoff(app, handoffLeaseFunc(func() {}))
	if err != nil {
		t.Fatal(err)
	}
	// The zero-value service is sufficient for its empty WaitGroup; prevent resource
	// cleanup from calling Close on this deliberately uninitialized service.
	app.Issues = nil
	backgroundStarted, backgroundStopped, releaseBackground := make(chan struct{}), make(chan struct{}), make(chan struct{})
	app.startBackgroundTask(func(ctx context.Context) {
		close(backgroundStarted)
		<-ctx.Done()
		<-releaseBackground
		close(backgroundStopped)
	})
	handoffAwait(t, backgroundStarted)
	requestStarted, releaseRequest, requestDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := h.HTTP.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(requestStarted); <-releaseRequest }))
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/issue/close", nil))
		close(requestDone)
	}()
	handoffAwait(t, requestStarted)
	issueWait := make(chan struct{})
	for i, step := range h.accepted {
		if step.name == "issues" {
			h.accepted[i].run = func(ctx context.Context) error { close(issueWait); return step.run(ctx) }
		}
	}
	done := make(chan error, 1)
	go func() { done <- h.Run(context.Background()) }()
	select {
	case <-issueWait:
		t.Fatal("復旧停止前に待機した")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseBackground)
	handoffAwait(t, backgroundStopped)
	// Observe the actual HTTP fence before allowing the admitted request to
	// finish: no new handler may register issue work once the wait begins.
	deadline := time.After(5 * time.Second)
	for {
		h.HTTP.mu.Lock()
		stopped := h.HTTP.stopped
		h.HTTP.mu.Unlock()
		if stopped {
			break
		}
		select {
		case <-deadline:
			t.Fatal("HTTP の受付が停止しない")
		case <-time.After(time.Millisecond):
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/issue/close", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("新規要求が交代境界を通過した", w.Code)
	}
	select {
	case <-issueWait:
		t.Fatal("受付済み要求の終了前に待機した")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRequest)
	handoffAwait(t, requestDone)
	handoffAwait(t, issueWait)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
