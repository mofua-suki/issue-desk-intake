package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/usage"
)

func TestBackgroundStopWaitsAndPreservesBusiness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := &App{ctx: ctx, cancel: cancel, Log: func(string) {}}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	app.startBackgroundTask(func(ctx context.Context) { close(started); <-ctx.Done(); close(canceled); <-release })
	<-started
	done := make(chan struct{})
	go func() { app.StopBackground(); close(done) }()
	<-canceled
	select {
	case <-done:
		t.Fatal("停止は実行中の処理を待つ必要があります")
	default:
	}
	if ctx.Err() != nil {
		t.Fatal("業務コンテキストが停止しました")
	}
	if app.startBackgroundTask(func(context.Context) { t.Error("停止後に開始しました") }) {
		t.Fatal("停止後の登録に成功しました")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("終了待ちが完了しません")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); app.StopBackground() }()
	}
	wg.Wait()
	app.Close()
}

// Exercise the production daily callback, including cancellation, usage
// persistence, and concurrent normal shutdown while the child is exiting.
func TestBackgroundStopWaitsForDailyChildPostgres(t *testing.T) {
	app := reviewNotesApp(t)
	if _, err := app.DB.ExecContext(t.Context(), `INSERT INTO repo_notes(repo,kind,data) VALUES('mofua-suki/issue-desk-intake','finding','{"issue":1,"text":"入力検証が不足"}')`); err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	app.Runner.Execute = func(ctx context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
		if p.Arguments[0] == "login" {
			return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return ai.ProcessResult{Stdout: []byte(`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":1,"cached_input_tokens":0}}`)}, ctx.Err()
	}
	app.periodically("test-task", time.Minute, app.reconcileDailyNotes)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("日次処理が開始しない")
	}
	stopped := make(chan struct{})
	go func() { app.StopBackground(); close(stopped) }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("日次処理に停止が届かない")
	}
	if app.ctx.Err() != nil {
		t.Fatal("業務の寿命が停止した")
	}
	select {
	case <-stopped:
		t.Fatal("子処理の終了を待っていない")
	default:
	}
	closed := make(chan struct{})
	go func() { app.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("通常停止が子処理を待っていない")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	for _, done := range []<-chan struct{}{stopped, closed} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("停止が終わらない")
		}
	}
	var count int
	if err := app.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM usage_rows WHERE json::jsonb->>'step'=$1`, usage.ReviewNotesStep).Scan(&count); err != nil || count != 1 {
		t.Fatalf("使用量を保存できない: %d %v", count, err)
	}
}

func TestBackgroundStopPreservesAcceptedFilingHTTPPostgres(t *testing.T) {
	pg := appPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	id := "55555555-5555-5555-5555-555555555555"
	issue := map[string]any{"number": 9, "title": "起票の結果", "state": "open", "body": "確認済みの本文\n<!-- issue-desk:" + id + " -->", "user": map[string]any{"id": 1, "login": "intake"}, "created_at": "2026-11-01T00:00:00.000Z"}
	var armed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	initial := make(chan struct{}, 1)
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "intake"})
		case "/api/v1/users/owner":
			json.NewEncoder(w).Encode(map[string]any{"id": 2, "login": "owner"})
		case "/api/v1/repos/owner/repo/issues":
			if armed.Load() {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				json.NewEncoder(w).Encode([]any{issue})
			} else {
				select {
				case initial <- struct{}{}:
				default:
				}
				json.NewEncoder(w).Encode([]any{})
			}
		case "/api/v1/repos/owner/repo/issues/9":
			json.NewEncoder(w).Encode(issue)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer git.Close()
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	if err := os.MkdirAll(private, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(private, "intake-token")
	admin := filepath.Join(private, "admin-token")
	if err := os.WriteFile(admin, []byte("fixture-admin"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.local.json")
	writeConfig(t, file, map[string]any{"workerId": "nas", "model": "gpt-fixture", "giteaUrl": git.URL, "runtimeDir": "runtime", "intakeTokenFile": token, "tokenFile": admin, "intakeAccount": "intake", "gitUser": "owner", "codex": "/missing-fixture-codex", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"publicUrl": "http://localhost:8787", "port": 8787, "aiConcurrency": 1}})
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	writeConfig(t, filepath.Join(dir, "assignments.json"), map[string]any{"pollSeconds": 300, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": map[string]any{"owner/repo": map[string]any{"rank": 1}}}}, "repositories": map[string]any{}})
	stageApplicationAssets(t, dir)
	config, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	env := useApplicationSchema(t, config)
	local := openAppStore(t, env)
	records, err := consultations.OpenRecords(ctx, local.DB())
	if err != nil {
		t.Fatal(err)
	}
	_, err = records.Change(ctx, "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
		return consultations.Record{"id": id, "project": "owner/repo", "hold": true, "status": "draft", "ready": true, "revision": 0, "questions": []any{}, "messages": []any{}, "preparedJob": map[string]any{"Scope": []any{"a.go"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = local.CreateSession(ctx, auth.Hash("review-session"), auth.Session{CSRF: "review-csrf", ExpiresAt: time.Now().Add(time.Hour)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	local.Close()
	projectID := "22222222-2222-4222-8222-222222222222"
	started, projectRelease, _ := recoveryCodex(t, config, projectID)
	local = openAppStore(t, env)
	projectRaw, _ := json.Marshal(map[string]any{"id": projectID, "status": "thinking", "messages": []any{map[string]any{"role": "user", "text": projectID}}, "revision": 0})
	if _, err = local.DB().ExecContext(ctx, "INSERT INTO projects(id,json) VALUES(?,?)", projectID, string(projectRaw)); err != nil {
		t.Fatal(err)
	}
	local.Close()
	app, err := OpenApp(ctx, config, pg, strings.Repeat("b", 40), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	defer func() { _ = os.WriteFile(projectRelease, nil, 0600) }()
	waitRecoveryProcess(t, ctx, started)
	select {
	case <-initial:
	case <-ctx.Done():
		t.Fatal("起動時照合がない")
	}
	armed.Store(true)
	req := httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+id+"/submit", strings.NewReader(`{"revision":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:8787")
	req.Header.Set("X-CSRF-Token", "review-csrf")
	req.AddCookie(&http.Cookie{Name: "desk_session", Value: "review-session"})
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); app.Mux.ServeHTTP(w, req); result <- w }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("起票照合が開始しない")
	}
	for {
		d, ok, e := app.Records.Get(ctx, "drafts", id)
		if e != nil {
			t.Fatal(e)
		}
		if ok && d["status"] == "submitting" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("受理されない")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stopped := make(chan struct{})
	go func() { app.StopBackground(); close(stopped) }()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("周期停止が終わらない")
	}
	if app.ctx.Err() != nil {
		t.Fatal("業務contextが停止した")
	}
	get := httptest.NewRequest("GET", "http://localhost:8787/api/session", nil)
	get.AddCookie(&http.Cookie{Name: "desk_session", Value: "review-session"})
	w := httptest.NewRecorder()
	app.Mux.ServeHTTP(w, get)
	if w.Code != 200 {
		t.Fatal("周期停止後の業務応答", w.Code, w.Body.String())
	}
	if err = os.WriteFile(projectRelease, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = app.Projects.WaitAccepted(ctx); err != nil {
		t.Fatal(err)
	}
	projectSaved, err := app.Projects.Snapshot(ctx, projectID)
	if err != nil || projectSaved["status"] != "waiting" {
		t.Fatal("受理済みAIの応答を保存できない", projectSaved, err)
	}
	releaseOnce.Do(func() { close(release) })
	if err = app.Engine.Quiesce(ctx); err != nil {
		t.Fatal("受理済みの起票保存を待てない", err)
	}
	select {
	case w := <-result:
		if w.Code != 202 {
			t.Fatal("受理済み起票", w.Code, w.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("起票応答がない")
	}
	d, ok, err := app.Records.Get(ctx, "drafts", id)
	if err != nil || !ok || d["status"] != "submitted" {
		t.Fatal("起票結果なし", d, err)
	}
	var count int
	if err = pg.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE repo='owner/repo' AND issue=9").Scan(&count); err != nil || count != 1 {
		t.Fatal("配布結果なし", count, err)
	}
}

func TestBackgroundStopPreservesAcceptedEnginePostgres(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	env := pgtest.NewIsolated(t)
	local := openAppStore(t, env)
	records, err := consultations.OpenRecords(ctx, local.DB())
	if err != nil {
		t.Fatal(err)
	}
	service, err := consultations.New(consultations.Options{Records: records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	defer finishOnce.Do(func() { close(finish) })
	engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: func(run context.Context, _ consultations.Record) (consultations.Record, error) {
		close(started)
		select {
		case <-finish:
			return consultations.Record{"reply": "受理済みの応答"}, nil
		case <-run.Done():
			return nil, run.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	service.BindEngine(engine)
	app := &App{ctx: ctx, cancel: cancel, Engine: engine, Consultations: service, SQLite: local, Log: func(string) {}}
	defer app.Close()
	id := "44444444-4444-4444-4444-444444444444"
	principal := auth.Principal{Kind: auth.Screen}
	if _, err = service.CreateDraft(ctx, consultations.Record{"tier": "light", "id": id, "project": "owner/repo", "hold": true}, principal, false); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Chat(ctx, id, consultations.Record{"text": "受理した処理", "messageId": id}, principal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("AI開始なし")
	}
	app.periodically("test-task", time.Hour, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	app.StopBackground()
	if ctx.Err() != nil {
		t.Fatal("業務contextを取消した")
	}
	finishOnce.Do(func() { close(finish) })
	deadline := time.After(5 * time.Second)
	for {
		d, ok, e := records.Get(ctx, "drafts", id)
		if e != nil {
			t.Fatal(e)
		}
		if ok && d["status"] == "draft" && len(fleet.List(d["messages"])) == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("AI応答が保存されない", d)
		case <-time.After(10 * time.Millisecond):
		}
	}
	app.Close()
	restored := openAppStore(t, env)
	reopened, err := consultations.OpenRecords(context.Background(), restored.DB())
	if err != nil {
		t.Fatal(err)
	}
	saved, ok, err := reopened.Get(context.Background(), "drafts", id)
	if err != nil || !ok || len(fleet.List(saved["messages"])) != 2 {
		t.Fatal("AI応答の再読込", saved, err)
	}
}

func TestBackgroundConcurrentRegistrationStopClose(t *testing.T) {
	for iteration := 0; iteration < 30; iteration++ {
		ctx, cancel := context.WithCancel(context.Background())
		app := &App{ctx: ctx, cancel: cancel, Log: func(string) {}}
		var starters sync.WaitGroup
		var active atomic.Int32
		begin := make(chan struct{})
		for i := 0; i < 30; i++ {
			starters.Add(1)
			go func() {
				defer starters.Done()
				<-begin
				app.startBackgroundTask(func(ctx context.Context) { active.Add(1); defer active.Add(-1); <-ctx.Done() })
			}()
		}
		stopped, closed := make(chan struct{}), make(chan struct{})
		go func() { <-begin; app.StopBackground(); close(stopped) }()
		go func() { <-begin; app.Close(); close(closed) }()
		close(begin)
		starters.Wait()
		for _, done := range []<-chan struct{}{stopped, closed} {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("並行停止が完了しない")
			}
		}
		if active.Load() != 0 {
			t.Fatal("実行中の子処理を待たなかった", active.Load())
		}
		if app.startBackgroundTask(func(context.Context) { t.Error("停止後に開始した") }) {
			t.Fatal("停止後の登録が成功した")
		}
	}
}

func TestProviderLabelRecoveryPostgres(t *testing.T) {
	app := reviewNotesApp(t)
	ctx := t.Context()
	const id = "11111111-1111-4111-8111-111111111111"
	var labelGets, labelPosts, issuePosts int
	var healthy bool
	var issue map[string]any
	api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		status, body := 200, `[]`
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == "GET":
			labelGets++
			if !healthy {
				status, body = 503, `{}`
			} else {
				body = `[{"id":42,"name":"provider/claude"}]`
			}
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == "POST":
			labelPosts++
			body = `{}`
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == "POST":
			issuePosts++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			labels := fleet.List(payload["labels"])
			if len(labels) != 1 || fmt.Sprint(labels[0]) != "42" {
				t.Fatalf("ラベルなし: %v", payload)
			}
			issue = map[string]any{"number": 9, "title": payload["title"], "body": payload["body"], "state": "open", "user": map[string]any{"id": 1}, "created_at": "2026-11-01T00:00:00Z"}
			b, _ := json.Marshal(issue)
			body = string(b)
		case strings.HasSuffix(r.URL.Path, "/issues/9"):
			b, _ := json.Marshal(issue)
			body = string(b)
		case strings.HasSuffix(r.URL.Path, "/issues"):
			if issue != nil {
				b, _ := json.Marshal([]any{issue})
				body = string(b)
			}
		default:
			t.Errorf("予期しない要求: %s %s", r.Method, r.URL.String())
			status = 404
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	app.Config.Repositories = []string{"owner/repo"}
	app.Gitea = api
	queue, err := jobs.New(app.DB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	config := filing.BuildConfig{Config: map[string]any{"model": "fixture"}, Project: app.Project, ProviderLabels: app.providerLabels}
	svc, err := filing.New(ctx, filing.Options{Records: app.Records, API: api, Jobs: queue, Project: app.Project, Own: func(map[string]any) bool { return true }, Build: func(ctx context.Context, d filing.Record) (filing.Prepared, error) {
		return filing.Prepare(ctx, config, d)
	}, Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "fixture" }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.Records.Change(ctx, "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
		return consultations.Record{"id": id, "project": "owner/repo", "provider": "claude", "title": "件名", "body": "本文", "changeScope": map[string]any{"known": true, "paths": []any{"a.go"}}, "status": "draft", "ready": true, "revision": 0, "questions": []any{}, "messages": []any{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Kind: auth.Screen}
	first, err := svc.Submit(ctx, id, filing.Record{"revision": 0}, p)
	if err == nil || first["status"] != "submitting" || labelGets != 3 || labelPosts != 0 || issuePosts != 0 {
		t.Fatalf("障害時: %v %v GET=%d labelPOST=%d issuePOST=%d", first, err, labelGets, labelPosts, issuePosts)
	}
	saved, ok, err := app.Records.Get(ctx, "drafts", id)
	if err != nil || !ok || saved["status"] != "submitting" {
		t.Fatalf("保存された障害時状態: %v %v", saved, err)
	}
	healthy = true
	for i := 0; i < 2; i++ {
		got, e := svc.SubmitAccepted(ctx, id, filing.Record{"revision": first["revision"]}, p)
		if e != nil || got["status"] != "submitted" || labelPosts != 0 || issuePosts != 1 {
			t.Fatalf("復旧 %d: %v %v labelPOST=%d issuePOST=%d", i, got, e, labelPosts, issuePosts)
		}
	}
	var count int
	if err = app.DB.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE repo=$1 AND issue=$2", "owner/repo", 9).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ジョブ: %d %v", count, err)
	}
}

func TestProviderLabelFailureHandling(t *testing.T) {
	for _, tc := range []struct {
		name, provider, response string
		status                   int
		postError                bool
		success                  bool
	}{
		{"codex", "codex", `{}`, 503, false, true},
		{"forbidden", "claude", `{}`, 403, false, false},
		{"invalid ID", "claude", `[{"name":"provider/claude","id":"bad"}]`, 200, false, false},
		{"uncertain POST", "claude", `[]`, 200, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts, gets := 0, 0
			api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					return nil, errors.New("connection lost")
				}
				gets++
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.response)), Request: r}, nil
			})
			app := App{Config: &Config{Repositories: []string{"owner/repo"}}, Gitea: api, Log: func(string) {}}
			c := filing.BuildConfig{Config: map[string]any{"model": "fixture"}, Project: app.Project, ProviderLabels: app.providerLabels}
			prepared, err := filing.Prepare(t.Context(), c, filing.Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "provider": tc.provider, "title": "件名", "body": "本文", "changeScope": map[string]any{"known": true, "paths": []any{"a.go"}}, "ready": true, "questions": []any{}})
			if tc.success {
				if err != nil || len(prepared.Labels) != 0 || !strings.Contains(prepared.Body, "実行先: Codex") {
					t.Fatalf("Codex: %v %v", prepared, err)
				}
			} else if err == nil || errors.Is(err, filing.ErrFilingReadUnavailable) {
				t.Fatalf("失敗分類: %v", err)
			}
			if gets == 0 || (posts > 0) != tc.postError {
				t.Fatalf("ラベルGET数: %d POST数: %d", gets, posts)
			}
		})
	}
}
