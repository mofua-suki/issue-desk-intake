package startup

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/projects"
)

type recoveryIssueAPI struct {
	entered, release chan struct{}
	once             sync.Once
}

func (f *recoveryIssueAPI) Issue(ctx context.Context, _ string, _ int64) (map[string]any, error) {
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[string]any{"id": float64(100), "number": float64(1), "title": "依頼", "body": "本文", "user": map[string]any{"login": "intake"}, "state": "closed", "closed_at": "2026-11-01T00:00:00Z", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-01T00:00:00Z"}, nil
}
func (f *recoveryIssueAPI) Issues(context.Context, string, string) ([]map[string]any, error) {
	return []map[string]any{}, nil
}
func (f *recoveryIssueAPI) Comments(context.Context, string, int64) ([]map[string]any, error) {
	return []map[string]any{{"id": float64(1), "user": map[string]any{"login": "intake"}, "body": "<!-- gcr:issue-desk-close-request:11111111-1111-4111-8111-111111111111 -->"}}, nil
}
func (f *recoveryIssueAPI) Do(context.Context, string, string, any, any) error { return nil }
func (f *recoveryIssueAPI) Pages(context.Context, string) ([]map[string]any, error) {
	return []map[string]any{}, nil
}
func (f *recoveryIssueAPI) Comment(context.Context, string, int64, string) (map[string]any, error) {
	return nil, nil
}
func (f *recoveryIssueAPI) SetIssueState(context.Context, string, int64, string) error { return nil }

func TestBackgroundStopsRecoveryLoopsWithoutCancelingAcceptedPostgres(t *testing.T) {
	app := reviewNotesApp(t)
	ctx := app.ctx
	projectStarted, projectRelease := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(projectRelease) })
	project, err := projects.New(projects.Options{AppDB: app.SQLite.DB(), Auth: &auth.Authenticator{}, Repositories: func() []string { return []string{"owner/repo"} }, Answer: func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		close(projectStarted)
		select {
		case <-projectRelease:
			return map[string]any{"reply": "復旧した応答", "ready": false, "questions": []any{"確認"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	app.Projects = project
	pid := "22222222-2222-4222-8222-222222222222"
	projectRecord, _ := json.Marshal(map[string]any{"id": pid, "status": "thinking", "messages": []any{}, "revision": 0})
	if _, err = app.SQLite.DB().ExecContext(ctx, "INSERT INTO projects(id,json) VALUES(?,?)", pid, string(projectRecord)); err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(app.DB, jobs.Options{AppDB: app.SQLite.DB()})
	if err != nil {
		t.Fatal(err)
	}
	nums, err := numbers.OpenLedger(ctx, app.SQLite.DB(), filepath.Join(t.TempDir(), "unused-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	issueAPI := &recoveryIssueAPI{entered: make(chan struct{}), release: make(chan struct{})}
	var issueRelease sync.Once
	defer issueRelease.Do(func() { close(issueAPI.release) })
	issues, err := issueapi.Register(http.NewServeMux(), issueapi.Options{AppDB: app.SQLite.DB(), Postgres: app.DB, Auth: &auth.Authenticator{}, Gitea: issueAPI, Numbers: nums, Jobs: queue, Config: map[string]any{"workerId": "nas"}, Project: func(string) error { return nil }, Own: func(user map[string]any) bool { return user["login"] == "intake" }})
	if err != nil {
		t.Fatal(err)
	}
	app.Issues = issues
	iid := "11111111-1111-4111-8111-111111111111"
	issueRecord, _ := json.Marshal(map[string]any{"id": iid, "status": "closed", "project": "owner/repo", "number": 1, "notePending": true})
	if _, err = app.SQLite.DB().ExecContext(ctx, "INSERT INTO close_requests(id,json) VALUES(?,?)", iid, string(issueRecord)); err != nil {
		t.Fatal(err)
	}
	app.startRecoveryLoops()
	for _, ch := range []<-chan struct{}{projectStarted, issueAPI.entered} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("復旧処理が開始しない")
		}
	}
	stopped := make(chan struct{})
	go func() { app.StopBackground(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("復旧ループが停止しない")
	}
	if ctx.Err() != nil {
		t.Fatal("業務寿命が停止した")
	}
	releaseOnce.Do(func() { close(projectRelease) })
	issueRelease.Do(func() { close(issueAPI.release) })
	if err = project.WaitAccepted(ctx); err != nil {
		t.Fatal(err)
	}
	if err = issues.WaitAccepted(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := project.Snapshot(ctx, pid)
	if err != nil || saved["status"] != "waiting" {
		t.Fatal("Projectの復旧結果が未保存", saved, err)
	}
	var raw string
	if err = app.SQLite.DB().QueryRowContext(ctx, "SELECT json FROM close_requests WHERE id=?", iid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var closed map[string]any
	if err = json.Unmarshal([]byte(raw), &closed); err != nil || closed["notePending"] != false {
		t.Fatal("Issueの復旧結果が未保存", closed, err)
	}
}
