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
	"testing"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
)

// OpenApp, its background scanner, the shipped model/account configuration,
// the actual Gitea client, filing outbox, application PostgreSQL and shared DB are all connected.
func TestBackgroundStopPreservesProposalFilingPostgres(t *testing.T) {
	pg := appPostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	assignments := shippedAssignments(t)
	worker := assignments["workerAccounts"].(map[string]any)["nas"]
	var mu sync.Mutex
	posts := 0
	merged := false
	var created map[string]any
	started, release := make(chan struct{}), make(chan struct{})
	var gitURL string
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repos/owner/repo/issues" && r.Method == "POST" {
			close(started)
			<-release
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var out any
		switch r.URL.Path {
		case "/api/v1/user":
			out = map[string]any{"id": 1, "login": "intake"}
		case "/api/v1/users/owner":
			out = map[string]any{"id": 2, "login": "owner"}
		case "/api/v1/repos/owner/repo/issues":
			if r.Method == "POST" {
				payload := map[string]any{}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				posts++
				created = map[string]any{"number": 20, "title": payload["title"], "body": payload["body"], "state": "open", "user": map[string]any{"id": 1, "login": "intake"}, "created_at": "2026-11-01T00:00:00Z"}
				out = created
			} else {
				rows := []any{map[string]any{"number": 1, "title": "元の依頼", "body": "元の本文", "state": "closed", "created_at": "2026-11-01T00:00:00Z"}}
				if created != nil {
					rows = append(rows, created)
				}
				out = rows
			}
		case "/api/v1/repos/owner/repo/issues/1":
			out = map[string]any{"number": 1, "state": "closed"}
		case "/api/v1/repos/owner/repo/issues/20":
			out = created
		case "/api/v1/repos/owner/repo/issues/1/comments", "/api/v1/repos/owner/repo/issues/comments/10":
			p := map[string]any{"version": 3, "kind": "followup", "id": "review-head", "source": "owner/repo#1", "title": "軽微な指摘の修正", "pr": gitURL + "/owner/repo/pulls/2", "items": []any{map[string]any{"problem": "余白がずれる", "expected": "余白を揃える", "test": "画面の余白を検証する"}}, "dependsOn": []any{}, "provider": "codex", "changeScope": map[string]any{"paths": []any{"src/app.go"}}}
			raw, _ := json.Marshal(p)
			comment := map[string]any{"id": 10, "body": "<!-- issue-desk-proposal " + string(raw) + " -->", "user": map[string]any{"login": worker}, "issue_url": gitURL + "/api/v1/repos/owner/repo/issues/1", "created_at": "2026-11-01T00:00:00Z"}
			if strings.HasSuffix(r.URL.Path, "/comments") {
				out = []any{comment}
			} else {
				out = comment
			}
		case "/api/v1/repos/owner/repo/issues/20/comments":
			out = []any{}
		case "/api/v1/repos/owner/repo/pulls/2":
			out = map[string]any{"number": 2, "merged": merged, "state": "closed", "html_url": gitURL + "/owner/repo/pulls/2"}
		case "/api/v1/repos/owner/repo/labels":
			out = []any{map[string]any{"id": 3, "name": "provider/codex"}}
		default:
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer git.Close()
	gitURL = git.URL
	token := filepath.Join(dir, "fixture-intake-token")
	if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.local.json")
	writeConfig(t, file, map[string]any{"workerId": "nas", "giteaUrl": git.URL, "runtimeDir": "runtime", "tokenFile": filepath.Join(dir, "absent-admin-token"), "intakeTokenFile": token, "intakeAccount": "intake", "gitUser": "owner", "projectCreation": map[string]any{"owner": "owner"}, "codex": "/missing-fixture-codex", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"publicUrl": "http://localhost:8787", "port": 8787, "aiConcurrency": 1}})
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"giteaUrl": git.URL, "repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	delete(assignments, "repositoryGroups") // The fixture only allows owner/repo.
	writeConfig(t, filepath.Join(dir, "assignments.json"), assignments)
	stageApplicationAssets(t, dir)
	config, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	useApplicationSchema(t, config)
	app, err := OpenApp(ctx, config, pg, strings.Repeat("d", 40), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err = app.numbering(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err = app.Proposals.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	before := posts
	merged = true
	mu.Unlock()
	if before != 0 {
		t.Fatal("未マージPRを起票しました")
	}
	// OpenApp also starts an immediate scan. Overlapping scans are skipped,
	// so retry until background work is stopped. Continue after filing starts to
	// exercise overlapping scans and verify they do not create duplicate issues.
	scanErrors := make(chan error, 1)
	if !app.startBackgroundTask(func(ctx context.Context) {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := app.Proposals.Scan(ctx); err != nil {
				if ctx.Err() == nil {
					scanErrors <- err
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}) {
		t.Fatal("周期処理を開始できません")
	}
	select {
	case err := <-scanErrors:
		t.Fatalf("後続案の走査に失敗しました: %v", err)
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("起票が開始しません")
	}
	stopped := make(chan struct{})
	go func() { app.StopBackground(); close(stopped) }()
	<-app.backgroundCtx.Done()
	if app.ctx.Err() != nil {
		t.Fatal("業務の寿命が停止しました")
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("周期停止が完了しません")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		drafts, e := app.Records.List(ctx, "drafts")
		if e != nil {
			t.Fatal(e)
		}
		for _, d := range drafts {
			if d["proposal"] != nil && d["status"] == "submitted" {
				issue := d["issue"].(map[string]any)
				job, e := app.Jobs.ByIssue(ctx, "owner/repo", int64(20))
				if e != nil {
					t.Fatal(e)
				}
				if job == nil {
					continue
				}
				mu.Lock()
				count := posts
				body := fmt.Sprint(created["body"])
				mu.Unlock()
				if count != 1 || !strings.Contains(body, "共通番号") || !strings.Contains(body, "余白を揃える") || job.Kind != "followup" {
					t.Fatal("実際の起票・配布が不一致", count, job)
				}
				if issue["project"] != "owner/repo" || filing.DraftID(body) != d["id"] {
					t.Fatal("起票先・マーカーの不一致")
				}
				// The proposal record is marked notified just after the draft is submitted.
				var records []consultations.Record
				for wait := time.Now().Add(10 * time.Second); ; {
					records, e = app.Records.List(ctx, "proposals")
					if e != nil || (len(records) == 1 && records[0]["status"] == "notified") || time.Now().After(wait) {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				if e != nil || len(records) != 1 || records[0]["status"] != "notified" || records[0]["issue"] == nil {
					t.Fatal("案の保存形式", records, e)
				}
				if err = app.Proposals.Scan(ctx); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				count = posts
				mu.Unlock()
				if count != 1 {
					t.Fatalf("再走査で重複起票しました: %d", count)
				}
				// The generic manual policy still rejects proposal provenance.
				if filing.CheckPolicy(ctx, app.Records, consultations.Record(d)) == nil {
					t.Fatal("一般の自動起票制限を解除しました")
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("周期走査が後続案を自動起票しませんでした")
}
