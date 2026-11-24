package startup

import (
	"context"
	"encoding/json"
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

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
)

func openLifecycleApp(t *testing.T, handler http.HandlerFunc, seed func(*Config)) *App {
	t.Helper()
	pg := appPostgres(t)
	git := httptest.NewServer(handler)
	t.Cleanup(git.Close)
	dir := t.TempDir()
	token := filepath.Join(dir, "fixture-token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.local.json")
	writeConfig(t, file, map[string]any{"workerId": "nas", "giteaUrl": git.URL, "runtimeDir": "runtime", "tokenFile": filepath.Join(dir, "absent"), "intakeTokenFile": token, "intakeAccount": "intake", "gitUser": "owner", "projectCreation": map[string]any{"owner": "owner"}, "codex": "/missing-fixture-codex", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"publicUrl": "http://localhost:8787", "port": 8787, "aiConcurrency": 1}})
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"giteaUrl": git.URL, "repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	assignments := shippedAssignments(t)
	delete(assignments, "repositoryGroups") // The fixture only allows owner/repo.
	writeConfig(t, filepath.Join(dir, "assignments.json"), assignments)
	stageApplicationAssets(t, dir)
	config, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	useApplicationSchema(t, config)
	if seed != nil {
		seed(config)
	}
	app, err := OpenApp(context.Background(), config, pg, strings.Repeat("d", 40), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func waitRecoveryProcess(t *testing.T, ctx context.Context, started string) {
	t.Helper()
	for {
		if _, err := os.Stat(started); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("復旧AI未開始")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func lifecycleGitea(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/user":
		fmt.Fprint(w, `{"id":1,"login":"intake"}`)
	case "/api/v1/users/owner":
		fmt.Fprint(w, `{"id":2,"login":"owner"}`)
	case "/api/v1/repos/owner/repo/issues":
		fmt.Fprint(w, `[]`)
	default:
		http.NotFound(w, r)
	}
}

func TestBackgroundOpenAppAcceptedAIStoredPostgres(t *testing.T) {
	app := openLifecycleApp(t, lifecycleGitea, nil)
	// Only the turn implementation is replaced; the OpenApp service and storage remain wired.
	app.Engine.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	engine, err := consultations.NewEngine(app.ctx, consultations.EngineOptions{Service: app.Consultations, Turn: func(ctx context.Context, _ consultations.Record) (consultations.Record, error) {
		close(started)
		select {
		case <-release:
			return consultations.Record{"reply": "保存された応答"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	app.Engine = engine
	app.Consultations.BindEngine(engine)
	ctx := context.Background()
	id := "44444444-4444-4444-8444-444444444444"
	p := auth.Principal{Kind: auth.Screen}
	if _, err = app.Consultations.CreateDraft(ctx, consultations.Record{"tier": "light", "id": id, "project": "owner/repo", "hold": true}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Consultations.Chat(ctx, id, consultations.Record{"text": "相談", "messageId": id}, p); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("AI未開始")
	}
	app.StopBackground()
	if app.ctx.Err() != nil {
		t.Fatal("業務寿命が停止")
	}
	releaseOnce.Do(func() { close(release) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d, found, e := app.Records.Get(ctx, "drafts", id)
		if e != nil {
			t.Fatal(e)
		}
		if found && d["status"] == "draft" && len(fleet.List(d["messages"])) == 2 && strings.Contains(fmt.Sprint(d["messages"]), "保存された応答") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("受理済みAI応答が保存されない")
}

func TestBackgroundOpenAppPendingBoundaryPostgres(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repos/owner/repo/issues/1" {
			startOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"number":1,"state":"open","title":"first"}`)
			return
		}
		lifecycleGitea(w, r)
	}
	seed := func(config *Config) {
		db, err := openApplicationStore(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for n := int64(1); n <= 2; n++ {
			entry := map[string]any{"repo": "owner/repo", "number": n, "draftId": "fixture", "preparedJob": jobs.NewJob{Repo: "owner/repo", Issue: n, Title: "test", Scope: []string{"a.go"}}}
			raw, _ := json.Marshal(entry)
			if _, err := db.DB().Exec("INSERT INTO job_filings(key,json) VALUES(?,?)", fmt.Sprintf("owner/repo#%d", n), string(raw)); err != nil {
				t.Fatal(err)
			}
		}
	}
	app := openLifecycleApp(t, handler, seed)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("周期配布未開始")
	}
	stopped := make(chan struct{})
	go func() { app.StopBackground(); close(stopped) }()
	<-app.backgroundCtx.Done()
	releaseOnce.Do(func() { close(release) })
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("周期停止未完了")
	}
	one, err := app.Jobs.ByIssue(context.Background(), "owner/repo", 1)
	if err != nil || one == nil {
		t.Fatal("1件目未保存", err)
	}
	two, err := app.Jobs.ByIssue(context.Background(), "owner/repo", 2)
	if err != nil || two != nil {
		t.Fatal("2件目が開始", err)
	}
	var count int
	if err = app.SQLite.DB().QueryRow("SELECT count(*) FROM job_filings WHERE key=?", "owner/repo#2").Scan(&count); err != nil || count != 1 {
		t.Fatal("2件目未保持", count, err)
	}
}

func TestBackgroundOpenAppRecoveryLifetimesPostgres(t *testing.T) {
	var issueCalls atomic.Int32
	issueStarted := make(chan struct{})
	issueRelease := make(chan struct{})
	var istart, irelease sync.Once
	t.Cleanup(func() { irelease.Do(func() { close(issueRelease) }) })
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/1":
			issueCalls.Add(1)
			istart.Do(func() { close(issueStarted) })
			select {
			case <-issueRelease:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":100,"number":1,"title":"依頼","body":"本文","user":{"id":1,"login":"intake"},"state":"closed","closed_at":"2026-11-01T00:00:00Z","created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z"}`)
		case "/api/v1/repos/owner/repo/issues/1/comments":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"id":1,"user":{"id":1,"login":"intake"},"body":"<!-- gcr:issue-desk-close-request:11111111-1111-4111-8111-111111111111 -->"}]`)
		default:
			lifecycleGitea(w, r)
		}
	}
	ctx := context.Background()
	pid := "22222222-2222-4222-8222-222222222222"
	iid := "11111111-1111-4111-8111-111111111111"
	var started, release, launches string
	app := openLifecycleApp(t, handler, func(config *Config) {
		started, release, launches = recoveryCodex(t, config, pid)
		db, err := openApplicationStore(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		project, _ := json.Marshal(map[string]any{"id": pid, "status": "thinking", "messages": []any{map[string]any{"role": "user", "text": pid}}, "revision": 0, "provider": "codex"})
		if _, err = db.DB().ExecContext(ctx, "INSERT INTO projects(id,json) VALUES(?,?)", pid, string(project)); err != nil {
			t.Fatal(err)
		}
		issue, _ := json.Marshal(map[string]any{"id": iid, "status": "closed", "project": "owner/repo", "number": 1, "notePending": true})
		if _, err = db.DB().ExecContext(ctx, "INSERT INTO close_requests(id,json) VALUES(?,?)", iid, string(issue)); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600); irelease.Do(func() { close(issueRelease) }) })
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	waitRecoveryProcess(t, waitCtx, started)
	select {
	case <-issueStarted:
	case <-waitCtx.Done():
		t.Fatal("Issue復旧未開始")
	}
	stopped := make(chan struct{})
	go func() { app.StopBackground(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("復旧ループ停止待ち")
	}
	if app.ctx.Err() != nil {
		t.Fatal("業務寿命が停止")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	irelease.Do(func() { close(issueRelease) })
	if err := app.Projects.WaitAccepted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.Issues.WaitAccepted(ctx); err != nil {
		t.Fatal(err)
	}
	assertRecoveryLaunches(t, launches, 1)
	if n := issueCalls.Load(); n != 1 {
		t.Fatalf("Issue 復旧の到達回数 = %d, want 1", n)
	}
	snapshot, err := app.Projects.Snapshot(ctx, pid)
	if err != nil || snapshot["status"] != "waiting" {
		t.Fatal("Project復旧結果", snapshot, err)
	}
	var raw string
	if err = app.SQLite.DB().QueryRowContext(ctx, "SELECT json FROM close_requests WHERE id=?", iid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err = json.Unmarshal([]byte(raw), &saved); err != nil || saved["notePending"] != false {
		t.Fatal("Issue復旧結果", saved, err)
	}
}

func TestBackgroundOpenAppDailyChildShutdownPostgres(t *testing.T) {
	app := openLifecycleApp(t, lifecycleGitea, nil)
	// Wait for the startup daily pass before arranging a new due pass.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := app.DB.QueryRow(`SELECT count(*) FROM repo_note_processing WHERE repo=$1`, "owner/repo").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 && !app.reviewNotesRunning.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("起動時の日次処理未完了")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := app.DB.Exec(`DELETE FROM repo_note_processing WHERE repo=$1`, "owner/repo"); err != nil {
		t.Fatal(err)
	}
	app.reviewNotesDates = reviewNotesAttempts{}
	if _, err := app.DB.Exec(`INSERT INTO repo_notes(repo,kind,data) VALUES('owner/repo','finding','{"issue":1,"text":"入力検証が不足"}')`); err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
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
	app.periodically("test-task", time.Hour, app.reconcileDailyNotes)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("日次子処理未開始")
	}
	stopped := make(chan struct{})
	go func() { app.StopBackground(); close(stopped) }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("日次子処理に取消しなし")
	}
	closed := make(chan struct{})
	go func() { app.Close(); close(closed) }()
	select {
	case <-stopped:
		t.Fatal("日次子処理を待っていない")
	default:
	}
	select {
	case <-closed:
		t.Fatal("通常停止が日次子処理を待っていない")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	for _, ch := range []<-chan struct{}{stopped, closed} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("停止未完了")
		}
	}
	var count int
	if err := app.DB.QueryRow(`SELECT count(*) FROM usage_rows WHERE json::jsonb->>'step'=$1`, usage.ReviewNotesStep).Scan(&count); err != nil || count != 1 {
		t.Fatal("使用量未保存", count, err)
	}
}

func TestBusinessExclusiveStartOncePostgres(t *testing.T) {
	existing := openLifecycleApp(t, lifecycleGitea, nil)
	existing.StopBackground()
	// A second instance uses the same deployed configuration and application store.
	app, err := OpenBusinessApp(context.Background(), existing.Config, existing.DB, existing.Revision, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	results := make(chan error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- app.StartExclusive(context.Background()) }()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err = <-results; err != nil {
			t.Fatal(err)
		}
	}
	gate, bg := app.currentGate(), app.backgroundCtx
	if err = app.StartExclusive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.currentGate() != gate || app.backgroundCtx != bg {
		t.Fatal("単独実行が二重に開始された")
	}
	app.StopBackground()
	if app.ctx.Err() != nil {
		t.Fatal("業務の寿命が取消された")
	}
	if _, err = app.Values(); err != nil {
		t.Fatal(err)
	}
}
