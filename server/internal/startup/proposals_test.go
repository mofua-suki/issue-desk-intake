package startup

import (
	"context"
	"database/sql"
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

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
)

func openFollowupTestApp(ctx context.Context, config *Config, db *sql.DB) (*App, error) {
	app, err := OpenBusinessApp(ctx, config, db, strings.Repeat("d", 40), func(string) {})
	if err != nil {
		return nil, err
	}
	app.proposalsScanInterval = time.Second
	if err = app.StartExclusive(ctx); err != nil {
		app.Close()
		return nil, err
	}
	return app, nil
}

func TestValidateNewPostBeforeProposalsReady(t *testing.T) {
	err := (&App{}).validateFollowupNewPost(context.Background(), nil)
	problem, ok := err.(*contract.Problem)
	if !ok || problem.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while proposals initialize, got %v", err)
	}
}

// OpenApp, its background scanner, the shipped model/account configuration,
// the actual Gitea client, filing outbox, application PostgreSQL and shared DB are all connected.
func TestOpenAppAutomaticallyFilesMergedReviewFollowupPostgres(t *testing.T) {
	testOpenAppFollowup(t, false, false)
}

func TestOpenAppFollowupHeadValidationPostgres(t *testing.T) {
	testOpenAppFollowup(t, true, false)
}

func TestMalformedScanReachesAttentionHTTP(t *testing.T) {
	testOpenAppFollowup(t, false, true)
}

func testOpenAppFollowup(t *testing.T, validateHead, malformed bool) {
	pg := appPostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	assignments := shippedAssignments(t)
	worker := assignments["workerAccounts"].(map[string]any)["nas"]
	var mu sync.Mutex
	posts := 0
	pulls := 0
	merged := false
	staleOnFiling := false
	head := strings.Repeat("a", 40)
	var created map[string]any
	var gitURL string
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			comment := followupComment(10, strings.Repeat("a", 40), "余白を揃える", worker, gitURL)
			if malformed {
				comment["body"] = "<!-- issue-desk-proposal {bad} -->"
				comment["created_at"] = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
			}
			if strings.HasSuffix(r.URL.Path, "/comments") {
				out = []any{comment}
			} else {
				out = comment
			}
		case "/api/v1/repos/owner/repo/issues/20/comments":
			out = []any{}
		case "/api/v1/repos/owner/repo/pulls/2/commits":
			out = fixtureCommits(r, head)
		case "/api/v1/repos/owner/repo/pulls/2":
			pulls++
			if staleOnFiling && pulls == 2 {
				head = strings.Repeat("b", 40)
			}
			out = map[string]any{"number": 2, "merged": merged, "state": "closed", "html_url": gitURL + "/owner/repo/pulls/2", "head": map[string]any{"sha": head}}
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
	app, err := openFollowupTestApp(ctx, config, pg)
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
	if malformed {
		now := time.Now()
		if err = app.SQLite.CreateSession(ctx, auth.Hash("attention-session"), auth.Session{CSRF: "csrf", ExpiresAt: now.Add(time.Hour)}, now); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "http://localhost:8787/api/attention/alerts", nil)
		req.AddCookie(&http.Cookie{Name: "desk_session", Value: "attention-session"})
		response := httptest.NewRecorder()
		app.Mux.ServeHTTP(response, req)
		var rows []map[string]any
		if err = json.Unmarshal(response.Body.Bytes(), &rows); err != nil || response.Code != 200 {
			t.Fatalf("attention HTTP %d %s: %v", response.Code, response.Body.String(), err)
		}
		for _, row := range rows {
			if row["kind"] == "unfiled_proposal" && row["commentId"] == float64(10) {
				if row["proposalId"] != nil && row["proposalId"] != "" || row["reason"] != "案のJSONを読めません。" || row["proposalStatus"] != "rejected" || row["project"] != "owner/repo" || row["number"] != float64(1) {
					t.Fatalf("incorrect alert: %v", row)
				}
				return
			}
		}
		t.Fatalf("missing malformed alert: %v", rows)
	}
	mu.Lock()
	before := posts
	merged = true
	pulls = 0
	staleOnFiling = validateHead
	mu.Unlock()
	if before != 0 {
		t.Fatal("未マージPRを起票しました")
	}
	if validateHead {
		// An initial background scan may already hold the scan lock.
		for deadline := time.Now().Add(5 * time.Second); ; {
			err = app.Proposals.Scan(ctx)
			if err != nil || time.Now().After(deadline) {
				break
			}
			records, e := app.Records.List(ctx, "proposals")
			if e != nil {
				t.Fatal(e)
			}
			if len(records) == 1 && records[0]["headRetryAfter"] != nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil && !strings.Contains(err.Error(), "後続案とPRのheadが一致しません") {
			t.Fatalf("unexpected scan failure: %v", err)
		}
		records, err := app.Records.List(ctx, "proposals")
		if err != nil || len(records) != 1 || records[0]["status"] != "adopted" || records[0]["headRetryAfter"] == nil {
			t.Fatalf("retry was not persisted: %v %v", records, err)
		}
		mu.Lock()
		if posts != 0 || pulls != 3 {
			t.Fatalf("unexpected stale submission: posts=%d pulls=%d", posts, pulls)
		}
		staleOnFiling = false
		head = strings.Repeat("a", 40)
		pulls = 0
		mu.Unlock()
		if err = app.Proposals.Scan(ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		if pulls != 0 || posts != 0 {
			t.Fatalf("retry was not deferred: pulls=%d posts=%d", pulls, posts)
		}
		mu.Unlock()
		drafts, err := app.Records.List(ctx, "drafts")
		if err != nil || len(drafts) != 1 {
			t.Fatalf("draft: %v %v", drafts, err)
		}
		_, err = app.Filing.SubmitAccepted(ctx, fmt.Sprint(drafts[0]["id"]), consultations.Record{"revision": drafts[0]["revision"]}, auth.Principal{Kind: auth.Screen})
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		// Policy reads once, Build validates twice, and each filing boundary reads once.
		if pulls != 5 || posts != 1 {
			t.Fatalf("matching submission: pulls=%d posts=%d", pulls, posts)
		}
		mu.Unlock()
		return
	}
	// The scanner is registered in startBackground. Wait for its next real tick,
	// without calling Scan or Submit after changing the PR to merged.
	deadline := time.Now().Add(20 * time.Second)
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

func TestOpenAppWorkerHeadRestartPostgres(t *testing.T) {
	for _, mode := range []string{"republished", "republished_no_restart", "post_mismatch", "reconcile", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			testOpenAppWorkerHead(t, mode)
		})
	}
}

func testOpenAppWorkerHead(t *testing.T, mode string) {
	pg := appPostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	assignments := shippedAssignments(t)
	worker := assignments["workerAccounts"].(map[string]any)["nas"]
	var mu sync.Mutex
	posts := 0
	merged := false
	head := strings.Repeat("a", 40)
	published := false
	var records *consultations.Records
	commentReads := 0
	reconcileGate := make(chan struct{})
	var releaseGate sync.Once
	var created map[string]any
	var gitURL string
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never hold the fixture mutex while waiting for reconciliation or the DB.
		if r.URL.Path == "/api/v1/repos/owner/repo/issues" && r.Method != "POST" {
			mu.Lock()
			blocked := mode == "reconcile" && created != nil
			mu.Unlock()
			if blocked {
				select {
				case <-reconcileGate:
				default:
					// A retry may already have set submitting. Fail it so filing
					// restores submit_unknown instead of waiting in that state.
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
		}
		if r.URL.Path == "/api/v1/repos/owner/repo/pulls/2" || r.URL.Path == "/api/v1/repos/owner/repo/pulls/2/commits" {
			responseHead, isMerged, e := fixtureHead(&mu, func() (string, bool, *consultations.Records) {
				if mode != "post_mismatch" {
					return head, merged, nil
				}
				return head, merged, records
			}, func(currentRecords *consultations.Records) (bool, error) {
				drafts, err := currentRecords.List(r.Context(), "drafts")
				return len(drafts) > 0, err
			})
			if e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/commits") {
				_ = json.NewEncoder(w).Encode(fixtureCommits(r, responseHead))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 2, "merged": isMerged, "state": "closed", "html_url": gitURL + "/owner/repo/pulls/2", "head": map[string]any{"sha": responseHead}})
			return
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
				if mode == "reconcile" {
					head = strings.Repeat("a", 40)
					conn, _, e := w.(http.Hijacker).Hijack()
					if e != nil {
						t.Error(e)
						return
					}
					conn.Close()
					return
				}
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
		case "/api/v1/repos/owner/repo/issues/1/comments", "/api/v1/repos/owner/repo/issues/comments/10", "/api/v1/repos/owner/repo/issues/comments/11":
			a := followupComment(10, strings.Repeat("a", 40), "余白を揃える", worker, gitURL)
			b := followupComment(11, strings.Repeat("b", 40), "余白の位置を統一する", worker, gitURL)
			if strings.HasSuffix(r.URL.Path, "/comments") {
				commentReads++
				out = []any{a}
				if published {
					out = []any{a, b}
				}
			} else if strings.HasSuffix(r.URL.Path, "/11") {
				out = b
			} else {
				out = a
			}

		case "/api/v1/repos/owner/repo/issues/20/comments":
			out = []any{}
		case "/api/v1/repos/owner/repo/labels":
			out = []any{map[string]any{"id": 3, "name": "provider/codex"}}
		default:
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer git.Close()
	defer releaseGate.Do(func() { close(reconcileGate) })
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
	env := useApplicationSchema(t, config)
	openedRecords, err := consultations.OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	records = openedRecords
	mu.Unlock()
	app, err := openFollowupTestApp(ctx, config, pg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { app.Close() }()
	if err = app.numbering(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Observe the durable pending candidate before stopping the first app.
	wait := func(label string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s を確認できません", label)
	}
	wait("未マージ候補の保存", func() bool {
		rows, e := app.Records.List(ctx, "proposal_scans")
		if e != nil {
			t.Fatal(e)
		}
		for _, row := range rows {
			if pending, ok := row["pending"].([]any); ok && len(pending) == 1 {
				return true
			}
		}
		return false
	})
	mu.Lock()
	before := posts
	mu.Unlock()
	if before != 0 {
		t.Fatal("未マージPRを起票しました")
	}
	if mode != "republished_no_restart" {
		app.Close()
	}
	mu.Lock()
	if mode != "unchanged" {
		head = strings.Repeat("b", 40)
		published = true
	}
	merged = true
	mu.Unlock()
	if mode != "republished_no_restart" {
		app, err = openFollowupTestApp(ctx, config, pg)
		if err != nil {
			t.Fatal(err)
		}
	}
	if mode == "reconcile" {
		var unknownDraftID string
		wait("送信結果不明の保存", func() bool {
			rows, e := app.Records.List(ctx, "drafts")
			if e != nil {
				t.Fatal(e)
			}
			for _, d := range rows {
				if d["status"] == "submit_unknown" && d["attempted"] == true {
					var ok bool
					unknownDraftID, ok = d["id"].(string)
					if !ok || unknownDraftID == "" {
						t.Fatalf("送信結果不明の下書きIDが不正: %v", d["id"])
					}
					return true
				}
			}
			return false
		})
		// A retry may temporarily move the same draft to submitting. Read its
		// durable state while reconciliation is still blocked, before shutdown.
		draft, found, e := app.Records.Get(ctx, "drafts", unknownDraftID)
		if e != nil {
			t.Fatal(e)
		}
		if !found || draft["status"] == "submitted" {
			t.Fatalf("ゲート中の対象下書き: id=%s found=%t draft=%v", unknownDraftID, found, draft)
		}
		app.Close()
		releaseGate.Do(func() { close(reconcileGate) })
		app, err = openFollowupTestApp(ctx, config, pg)
		if err != nil {
			t.Fatal(err)
		}
	}
	if mode == "post_mismatch" {
		wait("新規POST直前の拒否", func() bool {
			rows, e := app.Records.List(ctx, "drafts")
			if e != nil {
				t.Fatal(e)
			}
			for _, d := range rows {
				if d["status"] == "submit_error" && d["error"] == "後続案とPRのheadが一致しません。" {
					mu.Lock()
					count := posts
					mu.Unlock()
					if count != 0 {
						t.Fatalf("拒否後のPOST数=%d", count)
					}
					job, e := app.Jobs.ByIssue(ctx, "owner/repo", 20)
					if e != nil || job != nil {
						t.Fatalf("拒否後の仕事: %v %v", job, e)
					}
					return true
				}
			}
			return false
		})
		return
	}
	wait("Bの起票と仕事", func() bool {
		rows, e := app.Records.List(ctx, "drafts")
		if e != nil {
			t.Fatal(e)
		}
		for _, d := range rows {
			if d["proposal"] == nil || d["status"] != "submitted" {
				continue
			}
			job, e := app.Jobs.ByIssue(ctx, "owner/repo", 20)
			if e != nil {
				t.Fatal(e)
			}
			if job == nil {
				return false
			}
			mu.Lock()
			count, body := posts, fmt.Sprint(created["body"])
			mu.Unlock()
			issue := d["issue"].(map[string]any)
			expected := "余白の位置を統一する"
			if mode == "unchanged" {
				expected = "余白を揃える"
			}
			if count != 1 || !strings.Contains(body, expected) || (mode != "unchanged" && strings.Contains(body, "余白を揃える")) || filing.DraftID(body) != d["id"] || issue["project"] != "owner/repo" || fmt.Sprint(issue["number"]) != "20" || job.Kind != "followup" {
				t.Fatalf("起票・仕事の不一致: %d %v %v", count, issue, job)
			}
			return true
		}
		return false
	})
	if mode == "republished_no_restart" || mode == "unchanged" {
		return
	}
	wait("案の通知記録", func() bool {
		rows, e := app.Records.List(ctx, "proposals")
		if e != nil {
			t.Fatal(e)
		}
		for _, row := range rows {
			if row["status"] == "notified" {
				return true
			}
		}
		return false
	})
	app.Close()
	mu.Lock()
	priorReads := commentReads
	mu.Unlock()
	app, err = openFollowupTestApp(ctx, config, pg)
	if err != nil {
		t.Fatal(err)
	}
	// The initial background scan runs on restart. Observe its persisted completion.
	wait("再走査", func() bool {
		mu.Lock()
		scanned := commentReads >= priorReads+2
		mu.Unlock()
		if !scanned {
			return false
		}
		rows, e := app.Records.List(ctx, "proposal_scans")
		if e != nil {
			t.Fatal(e)
		}
		for _, row := range rows {
			if p, ok := row["pending"].([]any); ok && len(p) == 0 {
				return true
			}
		}
		return false
	})
	mu.Lock()
	count := posts
	mu.Unlock()
	if count != 1 {
		t.Fatalf("再起動後のPOST数=%d", count)
	}
	if mode == "reconcile" {
		drafts, e := app.Records.List(ctx, "drafts")
		if e != nil || len(drafts) != 1 || drafts[0]["status"] != "submitted" || drafts[0]["attempted"] != true {
			t.Fatalf("再走査後の照合済み下書き: %v %v", drafts, e)
		}
	}
	jobs, total, err := app.Jobs.List(ctx, "owner/repo", nil, 100, 0)
	if err != nil || total != 1 || len(jobs) != 1 || jobs[0].Issue != 20 || jobs[0].Kind != "followup" {
		t.Fatalf("再走査後の仕事: %d %v %v", total, jobs, err)
	}
	job, err := app.Jobs.ByIssue(ctx, "owner/repo", 20)
	if err != nil || job == nil || job.Kind != "followup" {
		t.Fatalf("仕事: %v %v", job, err)
	}
}
