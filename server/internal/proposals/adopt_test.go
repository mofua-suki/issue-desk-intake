package proposals

import (
	"context"
	"crypto/sha256"
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
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
)

type adoptionAPI struct {
	files         map[string][]Object
	commits       map[string][]Object
	commitErr     error
	comments      []Object
	pull          Object
	issue         Object
	err           error
	before        func(string)
	posts         int
	history       []Object
	unknown, lose bool
}

func (a *adoptionAPI) Do(_ context.Context, method, route string, _ any, out any) error {
	if a.before != nil {
		a.before(route)
	}
	if a.err != nil {
		return a.err
	}
	var value any
	switch {
	case method == "GET" && strings.HasPrefix(route, "/repos/owner/repo/pulls/2/commits?"):
		if a.commitErr != nil {
			return a.commitErr
		}
		if a.commits != nil {
			if batch, ok := a.commits[route]; ok {
				value = batch
			}
		} else if strings.HasSuffix(route, "page=1") {
			value = []Object{{"sha": obj(a.pull["head"])["sha"], "parents": []any{}}}
		}
		if value == nil {
			value = []Object{}
		}
	case method == "GET" && strings.HasPrefix(route, "/repos/owner/repo/pulls/2/files?") && a.files != nil:
		if files := a.files[route]; files != nil {
			value = files
		} else {
			value = []Object{}
		}
	case method == "GET" && route == "/repos/owner/repo/pulls/2":
		if a.pull != nil {
			value = a.pull
		} else {
			value = Object{}
		}
	case method == "GET" && route == "/repos/owner/repo/issues/1":
		value = a.issue
	case method == "GET" && strings.HasPrefix(route, "/repos/owner/repo/issues/comments/"):
		for _, c := range a.comments {
			if route == fmt.Sprintf("/repos/owner/repo/issues/comments/%d", num(c["id"])) {
				value = c
			}
		}
	default:
		return fmt.Errorf("予期しないAPI呼出し: %s %s", method, route)
	}
	if value == nil {
		return fail(404, "見つかりません")
	}
	raw, _ := json.Marshal(value)
	return json.Unmarshal(raw, out)
}
func (a *adoptionAPI) Issues(context.Context, string, string) ([]Object, error) {
	return a.history, nil
}
func (a *adoptionAPI) Issue(_ context.Context, _ string, n int64) (Object, error) {
	for _, issue := range a.history {
		if num(issue["number"]) == n {
			return issue, nil
		}
	}
	return nil, fail(404, "見つかりません")
}
func (a *adoptionAPI) CreateIssue(_ context.Context, _ string, input any) (Object, error) {
	a.posts++
	payload := obj(input)
	issue := Object{"number": int64(20), "title": payload["title"], "body": payload["body"], "user": Object{"login": "intake"}, "state": "open"}
	if !a.lose {
		a.history = append(a.history, issue)
	}
	if a.unknown {
		return nil, &gitea.Error{UnknownOutcome: true}
	}
	return issue, nil
}
func v3() Object {
	return Object{"version": 3, "kind": "followup", "id": "review-head-1", "source": "owner/repo#1", "title": "レビュー指摘を直す",
		"pr": "https://git.example/owner/repo/pulls/2", "items": []any{Object{"problem": "指摘その1", "expected": "修正その1", "test": "検証その1"}, Object{"problem": "指摘その2", "expected": "修正その2", "test": "検証その2"}},
		"dependsOn": []any{}, "provider": "codex", "changeScope": Object{"paths": []any{"src/app.go"}}}
}
func proposalComment(p Object, n int64) Object {
	raw, _ := json.Marshal(p)
	return Object{"id": n, "body": "レビューの表示文\n<!-- issue-desk-proposal " + string(raw) + " -->", "issue_url": "https://git.example/api/v1/repos/owner/repo/issues/1", "user": Object{"login": "worker"}, "created_at": "2026-11-01T00:00:00Z"}
}
func adoptFixture(t *testing.T) (*Service, *adoptionAPI, func()) {
	t.Helper()
	s, _, restart := fixture(t)
	if _, err := s.o.AppDB.Exec("DELETE FROM proposals"); err != nil {
		t.Fatal(err)
	}
	api := &adoptionAPI{comments: []Object{proposalComment(v3(), 10)}, pull: Object{"number": 2, "html_url": "https://git.example/owner/repo/pulls/2", "merged": true, "state": "closed"}, issue: Object{"number": 1, "state": "closed"}}
	s.o.Gitea = api
	s.o.Desk = func(string, int64) any { return int64(4553) }
	s.o.Link = func(repo, kind string, n int64) string {
		return fmt.Sprintf("https://git.example/%s/%s/%d", repo, kind, n)
	}
	s.o.Repositories = func() []string { return []string{"owner/repo"} }
	s.o.Issues = func(_ context.Context, _ string, state string) ([]Object, error) {
		if state != "all" {
			t.Fatal("閉じた元Issueが走査されません")
		}
		return []Object{api.issue}, api.err
	}
	s.o.Comments = func(context.Context, string, int64) ([]Object, error) { return api.comments, api.err }
	s.o.Submit = func(ctx context.Context, id string, _ consultations.Record) (consultations.Record, error) {
		d, _, err := s.records.Get(ctx, "drafts", id)
		if err != nil {
			return nil, err
		}
		prepared, err := s.PrepareFollowup(ctx, d, Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
		if err != nil {
			return nil, err
		}
		issue, err := api.CreateIssue(ctx, "owner/repo", Object{"title": prepared.Title, "body": prepared.Body})
		if err != nil {
			return nil, err
		}
		return s.records.Change(ctx, "drafts", id, func(_ *sql.Tx, current consultations.Record) (consultations.Record, error) {
			current["status"], current["issue"] = "submitted", Object{"project": "owner/repo", "number": issue["number"], "url": s.o.Link("owner/repo", "issues", 20)}
			return current, nil
		})
	}
	return s, api, restart
}
func mustScan(t *testing.T, s *Service) {
	t.Helper()
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func savedDraft(t *testing.T, s *Service) consultations.Record {
	t.Helper()
	d, _, err := s.records.Get(context.Background(), "drafts", draftID("owner/repo", 1, "review-head-1"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAdoptPriorityPersistsAndRevalidates(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(fmt.Sprintf("marked=%v", marked), func(t *testing.T) {
			s, api, restart := adoptFixture(t)
			p := v3()
			if marked {
				p["priority"] = Object{"level": "high", "reason": "  早急に修正  "}
			}
			api.comments[0] = proposalComment(p, 10)
			s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
			mustScan(t, s)
			restart()
			ctx := context.Background()
			d := savedDraft(t, s)
			if d == nil {
				t.Fatal("採用済み下書きがありません")
			}
			if marked {
				source := obj(d["prioritySource"])
				if d["priority"] != "high" || source["via"] != "proposal" || source["reason"] != "早急に修正" || source["at"] == nil {
					t.Fatalf("保存された優先度: %v", d)
				}
			} else if d["priority"] != nil || d["prioritySource"] != nil {
				t.Fatalf("印なし案に優先度が付きました: %v", d)
			}
			if err := s.ValidateDraft(ctx, d); err != nil {
				t.Fatalf("保存済み下書きの再照合: %v", err)
			}
			if !marked {
				return
			}
			for _, change := range []string{"priority", "reason", "missing source"} {
				_, err := s.records.Change(ctx, "drafts", str(d["id"]), func(_ *sql.Tx, current consultations.Record) (consultations.Record, error) {
					current["priority"] = "high"
					current["prioritySource"] = Object{"via": "proposal", "reason": "早急に修正"}
					switch change {
					case "priority":
						current["priority"] = "normal"
					case "reason":
						current["prioritySource"] = Object{"reason": "別の理由"}
					case "missing source":
						delete(current, "prioritySource")
					}
					return current, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.ValidateDraft(ctx, savedDraft(t, s)); status(err) != 409 {
					t.Fatalf("%s の不一致を拒否しません: %v", change, err)
				}
			}
		})
	}
}

func TestAdoptMergedV3PersistsBodyAndOneIssueAcrossRestart(t *testing.T) {
	s, api, restart := adoptFixture(t)
	mustScan(t, s)
	body := str(api.history[0]["body"])
	for _, text := range []string{"指摘その1", "修正その1", "検証その1", "指摘その2", "修正その2", "検証その2", "共通番号4553", "https://git.example/owner/repo/issues/1", "https://git.example/owner/repo/pulls/2"} {
		if !strings.Contains(body, text) {
			t.Fatalf("本文の欠落: %s", text)
		}
	}
	mark, err := s.read(context.Background(), "owner/repo#1:review-head-1")
	if err != nil || mark["type"] != "proposal" || mark["status"] != "notified" || mark["draftId"] != savedDraft(t, s)["id"] || num(obj(mark["issue"])["number"]) != 20 || !hashPattern.MatchString(str(mark["fingerprint"])) {
		t.Fatalf("保存形式: %v %v", mark, err)
	}
	job, err := filing.JobFromIssue("owner/repo", api.history[0], "")
	if err != nil || job.Kind != "followup" || job.Provider == nil || *job.Provider != "codex" || len(job.Scope) != 1 || job.Scope[0] != "src/app.go" {
		t.Fatalf("実出力の再読込: %+v %v", job, err)
	}
	restart()
	// Recreate the controller, not just its transient scanning state.
	s = &Service{o: s.o, records: s.records}
	api.comments = append(api.comments, proposalComment(v3(), 11))
	mustScan(t, s)
	mustScan(t, s)
	if api.posts != 1 {
		t.Fatal("重複起票", api.posts)
	}
}

func TestAdoptWaitsForMergeIncludingManuallyMergedClosedIssue(t *testing.T) {
	s, api, restart := adoptFixture(t)
	for _, state := range []string{"open", "closed"} {
		api.pull["state"], api.pull["merged"] = state, false
		mustScan(t, s)
		if api.posts != 0 || savedDraft(t, s) != nil {
			t.Fatal("未マージで採用・起票されました", state)
		}
	}
	api.pull["merged"] = true
	restart()
	mustScan(t, s)
	if api.posts != 1 {
		t.Fatal("後の手動マージが取り込まれません")
	}
}

func TestAdoptRejectsMalformedUntrustedAndMismatchedProposals(t *testing.T) {
	cases := []struct {
		name   string
		change func(*adoptionAPI)
	}{
		{"missing-test", func(a *adoptionAPI) {
			p := v3()
			delete(obj(arr(p["items"])[0]), "test")
			a.comments[0] = proposalComment(p, 10)
		}},
		{"empty-items", func(a *adoptionAPI) { p := v3(); p["items"] = []any{}; a.comments[0] = proposalComment(p, 10) }},
		{"six-items", func(a *adoptionAPI) {
			p := v3()
			for len(arr(p["items"])) < 6 {
				p["items"] = append(arr(p["items"]), arr(p["items"])[0])
			}
			a.comments[0] = proposalComment(p, 10)
		}},
		{"duplicate-marker", func(a *adoptionAPI) { a.comments[0]["body"] = str(a.comments[0]["body"]) + str(a.comments[0]["body"]) }},
		{"unknown-provider", func(a *adoptionAPI) { p := v3(); p["provider"] = "other"; a.comments[0] = proposalComment(p, 10) }},
		{"untrusted-writer", func(a *adoptionAPI) { a.comments[0]["user"] = Object{"login": "stranger"} }},
		{"wrong-source", func(a *adoptionAPI) { p := v3(); p["source"] = "owner/repo#9"; a.comments[0] = proposalComment(p, 10) }},
		{"wrong-comment-issue", func(a *adoptionAPI) {
			a.comments[0]["issue_url"] = "https://git.example/api/v1/repos/owner/repo/issues/9"
		}},
		{"legacy-version", func(a *adoptionAPI) { p := v3(); p["version"] = 2; a.comments[0] = proposalComment(p, 10) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			tc.change(api)
			mustScan(t, s)
			if api.posts != 0 || savedDraft(t, s) != nil {
				t.Fatal("不正な案が起票されました")
			}
		})
	}
}

func TestAdoptRejectsForeignAndUnverifiablePullLinks(t *testing.T) {
	for _, link := range []string{"https://evil.example/owner/repo/pulls/2", "https://git.example/other/repo/pulls/2", "https://git.example/owner/repo/pulls/2?x=1", "https://git.example/owner/repo/pulls/2#fragment", "https://git.example/owner/repo/pulls/02"} {
		t.Run(link, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			p := v3()
			p["pr"] = link
			api.comments[0] = proposalComment(p, 10)
			if err := s.Scan(context.Background()); err == nil {
				t.Fatal("照合不能を成功扱いしました")
			}
			if api.posts != 0 {
				t.Fatal("別のPRで起票されました")
			}
		})
	}
	s, api, _ := adoptFixture(t)
	delete(api.pull, "merged")
	mustScan(t, s)
	if api.posts != 0 {
		t.Fatal("マージ欄欠測で起票されました")
	}
	api.pull["merged"] = true
	api.pull["html_url"] = "https://git.example/owner/repo/pulls/3"
	if err := s.Scan(context.Background()); err == nil || api.posts != 0 {
		t.Fatal("別のPR応答を採用しました")
	}
}

func TestAdoptHonorsDismissalRejectionAndWithdrawnDrafts(t *testing.T) {
	for _, kind := range []string{"dismissal", "rejection", "discarded", "withdrawn", "missing-draft"} {
		t.Run(kind, func(t *testing.T) {
			s, api, restart := adoptFixture(t)
			ctx := context.Background()
			p, fp, _ := parse(str(api.comments[0]["body"]))
			key := "owner/repo#1:review-head-1"
			switch kind {
			case "dismissal":
				_ = s.save(ctx, "dismissal:"+key, Object{"type": "dismissal", "repo": "owner/repo", "number": 1, "id": p["id"], "status": "dismissed", "fingerprint": fp})
			case "rejection":
				_ = s.save(ctx, "rejection:owner/repo#1:10", Object{"type": "rejection", "repo": "owner/repo", "number": 1, "id": p["id"], "commentId": 10, "status": "rejected", "fingerprint": fp})
			default:
				submit := s.o.Submit
				s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
				mustScan(t, s)
				s.o.Submit = submit
				if kind == "discarded" {
					r, _ := s.read(ctx, key)
					r["status"] = "discarded"
					_ = s.save(ctx, key, r)
				}
				if kind == "withdrawn" {
					_, _ = s.records.Change(ctx, "drafts", str(savedDraft(t, s)["id"]), func(_ *sql.Tx, d consultations.Record) (consultations.Record, error) {
						d["status"] = "closed"
						return d, nil
					})
				}
				if kind == "missing-draft" {
					_, err := s.o.AppDB.Exec("DELETE FROM drafts")
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			restart()
			mustScan(t, s)
			if api.posts != 0 {
				t.Fatal("終了した案を起票しました", kind)
			}
		})
	}
}

func TestAdoptConflictAndRecoveryRevalidateCommentAndMerge(t *testing.T) {
	s, api, _ := adoptFixture(t)
	ctx := context.Background()
	s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
	mustScan(t, s)
	d := savedDraft(t, s)
	if err := s.ValidateDraft(ctx, d); err != nil {
		t.Fatal(err)
	}
	api.issue["pull_request"] = Object{}
	if err := s.ValidateDraft(ctx, d); err == nil {
		t.Fatal("元IssueではないPRを採用しました")
	}
	delete(api.issue, "pull_request")
	for _, field := range []string{"body", "title", "provider"} {
		old := d[field]
		d[field] = "変更"
		if err := s.ValidateDraft(ctx, d); err == nil {
			t.Fatal("更新された下書きを採用", field)
		}
		d[field] = old
	}
	api.pull["merged"] = false
	if err := s.ValidateDraft(ctx, d); err == nil {
		t.Fatal("待機後に未マージを見逃しました")
	}
	api.pull["merged"] = true
	original := api.comments[0]
	p := v3()
	p["title"] = "本文の後更新"
	api.comments[0] = proposalComment(p, 10)
	if err := s.ValidateDraft(ctx, d); err == nil {
		t.Fatal("元コメントの更新を見逃しました")
	}
	mustScan(t, s)
	api.comments[0] = original
	if err := s.ValidateDraft(ctx, d); err == nil {
		t.Fatal("競合記録が無視されました")
	}
}

func TestAdoptConcurrentScansAndDraftRecordRecovery(t *testing.T) {
	s, api, restart := adoptFixture(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Scan(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if api.posts != 1 {
		t.Fatal("並列走査で二重起票", api.posts)
	}
	r, _ := s.read(context.Background(), "owner/repo#1:review-head-1")
	delete(r, "issue")
	r["status"] = "adopted"
	_ = s.save(context.Background(), "owner/repo#1:review-head-1", r)
	restart()
	mustScan(t, s)
	r, _ = s.read(context.Background(), "owner/repo#1:review-head-1")
	if r["status"] != "notified" || r["issue"] == nil || api.posts != 1 {
		t.Fatal("案の起票記録を復元できません", r)
	}
}

func TestAdoptRealFilingOutboxUnknownOutcomeNeverReposts(t *testing.T) {
	for _, lose := range []bool{false, true} {
		t.Run(fmt.Sprint(lose), func(t *testing.T) {
			s, api, restart := adoptFixture(t)
			ctx := context.Background()
			api.unknown, api.lose = true, lose
			connect := func() {
				queue, err := jobs.New(s.o.AppDB, jobs.Options{})
				if err != nil {
					t.Fatal(err)
				}
				filer, err := filing.New(ctx, filing.Options{Records: s.records, API: api, Jobs: queue, Project: s.o.Project, Own: func(u Object) bool { return u["login"] == "intake" },
					ValidateFollowup: s.ValidateDraft, ValidateNewPost: s.ValidateNewPost, Check: func(ctx context.Context, d consultations.Record) error { return s.ValidateDraft(ctx, d) },
					Build: func(ctx context.Context, d consultations.Record) (filing.Prepared, error) {
						return s.PrepareFollowup(ctx, d, Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
					}, Link: func(repo string, n int64) string { return s.o.Link(repo, "issues", n) }})
				if err != nil {
					t.Fatal(err)
				}
				s.o.Submit = func(ctx context.Context, id string, data consultations.Record) (consultations.Record, error) {
					return filer.SubmitAccepted(ctx, id, data, auth.Principal{Kind: auth.Screen})
				}
			}
			connect()
			if err := s.Scan(ctx); err == nil {
				t.Fatal("応答不明が成功扱いです")
			}
			if savedDraft(t, s)["status"] != "submit_unknown" {
				t.Fatal("送信意図の永続化がありません")
			}
			restart()
			connect()
			// Reconcile the found Issue after reopening the PostgreSQL-backed store.
			// The original submission must not be posted again.
			_ = s.Scan(ctx)
			if api.posts != 1 {
				t.Fatal("応答不明を再POSTしました", api.posts)
			}
			d := savedDraft(t, s)
			if !lose && (d["status"] != "submitted" || d["issue"] == nil) {
				t.Fatal("既存Issueを照合できません", d)
			}
			if lose && d["status"] != "submit_unknown" {
				t.Fatal("欠測した送信結果を確定しました", d)
			}
		})
	}
}

func TestAdoptMissingDeskAndReadFailuresDoNotBecomeSuccess(t *testing.T) {
	s, api, _ := adoptFixture(t)
	s.o.Desk = func(string, int64) any { return nil }
	if err := s.Scan(context.Background()); err == nil || api.posts != 0 {
		t.Fatal("共通番号欠測を成功扱いしました")
	}
	api.err = fmt.Errorf("読取失敗")
	if err := s.Scan(context.Background()); err == nil {
		t.Fatal("読取失敗を空一覧扱いしました")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Scan(ctx); err == nil {
		t.Fatal("取消後も走査しました")
	}
}

func TestAdoptRealFilingRechecksMergeAfterWaitAndBodyGeneration(t *testing.T) {
	for _, boundary := range []string{"after-acquire", "after-build"} {
		t.Run(boundary, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			ctx := context.Background()
			queue, err := jobs.New(s.o.AppDB, jobs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			filer, err := filing.New(ctx, filing.Options{Records: s.records, API: api, Jobs: queue, Project: s.o.Project, Own: func(u Object) bool { return u["login"] == "intake" },
				ValidateFollowup: s.ValidateDraft,
				Acquire: func(ctx context.Context, _ string) (context.Context, func(), error) {
					if boundary == "after-acquire" {
						api.pull["merged"] = false
					}
					return ctx, func() {}, nil
				},
				Check: func(ctx context.Context, d consultations.Record) error { return s.ValidateDraft(ctx, d) },
				Build: func(ctx context.Context, d consultations.Record) (filing.Prepared, error) {
					prepared, err := s.PrepareFollowup(ctx, d, Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
					if boundary == "after-build" {
						api.pull["merged"] = false
					}
					return prepared, err
				},
				Link: func(repo string, n int64) string { return s.o.Link(repo, "issues", n) },
			})
			if err != nil {
				t.Fatal(err)
			}
			s.o.Submit = func(ctx context.Context, id string, data consultations.Record) (consultations.Record, error) {
				return filer.SubmitAccepted(ctx, id, data, auth.Principal{Kind: auth.Screen})
			}
			if err := s.Scan(ctx); err == nil || api.posts != 0 {
				t.Fatal("待機・生成後の再確認がありません", err, api.posts)
			}
		})
	}
}

func workerFixtureID(head, batch string) string {
	sum := sha256.Sum256([]byte(head))
	return fmt.Sprintf("worker-job-1-%x-%s", sum[:16], batch)
}

func TestWorkerHeadAdoptionAndRecovery(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name           string
		value          any
		retry, adopted bool
	}{
		{"match", head, false, true}, {"mismatch", strings.Repeat("b", 40), false, false},
		{"missing", nil, true, false}, {"type", 42, true, false},
		{"length", "abc", true, false}, {"sha256", strings.Repeat("a", 64), true, false}, {"nonhex", strings.Repeat("z", 40), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			p := v3()
			p["id"] = workerFixtureID(head, "1")
			c := proposalComment(p, 10)
			api.comments = []Object{c}
			if tc.value != nil {
				api.pull["head"] = Object{"sha": tc.value}
			}
			retry, err := s.adopt(context.Background(), "owner/repo", 1, c)
			if retry != tc.retry || (err != nil) != tc.retry || (api.posts == 1) != tc.adopted {
				t.Fatalf("retry=%v err=%v posts=%d", retry, err, api.posts)
			}
			if tc.retry {
				api.pull["head"] = Object{"sha": head}
				_, err = s.adopt(context.Background(), "owner/repo", 1, c)
				if err != nil || api.posts != 1 {
					t.Fatalf("recovery: %v posts=%d", err, api.posts)
				}
			}
		})
	}
}

func TestWorkerHeadExistingDraftValidationIgnoresHead(t *testing.T) {
	s, api, _ := adoptFixture(t)
	head := strings.Repeat("a", 40)
	p := v3()
	p["id"] = workerFixtureID(head, "1")
	c := proposalComment(p, 10)
	api.comments = []Object{c}
	api.pull["head"] = Object{"sha": head}
	_, err := s.adopt(context.Background(), "owner/repo", 1, c)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.records.Get(context.Background(), "drafts", draftID("owner/repo", 1, str(p["id"])))
	if err != nil {
		t.Fatal(err)
	}
	delete(api.pull, "head")
	if err := s.ValidateDraft(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerIDUppercaseHexUsesLegacyAdoption(t *testing.T) {
	s, api, _ := adoptFixture(t)
	p := v3()
	p["id"] = "worker-job-1-ABCDEF0123456789ABCDEF0123456789-1"
	c := proposalComment(p, 10)
	api.comments = []Object{c}
	retry, err := s.adopt(context.Background(), "owner/repo", 1, c)
	if retry || err != nil || api.posts != 1 {
		t.Fatalf("retry=%v err=%v posts=%d", retry, err, api.posts)
	}
}

func TestAdoptStaleHeadRetriesAfterDelay(t *testing.T) {
	s, api, restart := adoptFixture(t)
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	s.o.Now = func() time.Time { return now }
	head := strings.Repeat("a", 40)
	p := v3()
	p["id"] = workerFixtureID(head, "1")
	comment := proposalComment(p, 10)
	api.comments = []Object{comment}
	api.pull["head"] = Object{"sha": head}
	parsed, fingerprint, err := parse(str(comment["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.adoptionDraft(context.Background(), "owner/repo", 1, comment, parsed, fingerprint); err != nil {
		t.Fatal(err)
	}
	api.pull["head"] = Object{"sha": strings.Repeat("b", 40)}
	pulls, submits := 0, 0
	api.before = func(route string) {
		if route == "/repos/owner/repo/pulls/2" {
			pulls++
		}
	}
	s.o.Submit = func(ctx context.Context, id string, _ consultations.Record) (consultations.Record, error) {
		submits++
		d, _, e := s.records.Get(ctx, "drafts", id)
		if e != nil {
			return nil, e
		}
		return nil, s.ValidateNewPost(ctx, d)
	}
	retry, err := s.adopt(context.Background(), "owner/repo", 1, comment)
	if !retry || !errors.Is(err, errStaleWorkerHead) {
		t.Fatalf("retry=%v err=%v", retry, err)
	}
	// Reopen the actual store to verify that the delay survives a restart.
	restart()
	mark, err := s.read(context.Background(), fmt.Sprintf("owner/repo#1:%s", p["id"]))
	if err != nil || mark["status"] != "adopted" || mark["headRetryAfter"] != now.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("saved retry: mark=%v err=%v", mark, err)
	}
	first := pulls
	retry, err = s.adopt(context.Background(), "owner/repo", 1, comment)
	if !retry || err != nil || pulls != first || submits != 1 {
		t.Fatalf("retry=%v err=%v pulls=%d submits=%d", retry, err, pulls, submits)
	}
	now = now.Add(time.Hour)
	api.pull["head"] = Object{"sha": head}
	retry, err = s.adopt(context.Background(), "owner/repo", 1, comment)
	if !retry || err != nil || pulls <= first || submits != 2 {
		t.Fatalf("retry=%v err=%v pulls=%d submits=%d", retry, err, pulls, submits)
	}
}

// The first merge is also a posted head; a later merge brings in trunk again.
func trunkMergeFixture(api *adoptionAPI) (string, string, string) {
	old, posted, final := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	trunk1, trunk2 := strings.Repeat("d", 40), strings.Repeat("e", 40)
	api.pull["head"] = Object{"sha": final}
	api.commits = map[string][]Object{
		"/repos/owner/repo/pulls/2/commits?limit=50&page=1": {
			{"sha": old, "parents": []any{Object{"sha": strings.Repeat("f", 40)}}},
			{"sha": posted, "parents": []any{Object{"sha": old}, Object{"sha": trunk1}}},
			{"sha": final, "parents": []any{Object{"sha": posted}, Object{"sha": trunk2}}},
		},
	}
	return old, posted, final
}

func TestAdoptTrunkMergePostedHeads(t *testing.T) {
	for _, tc := range []struct {
		name                                            string
		old, ownChange, absent, failure, internalParent bool
		wantPost, wantRetry                             bool
	}{
		{name: "latest posted head", wantPost: true},
		{name: "earlier posted head", old: true},
		{name: "own change after posted head", ownChange: true},
		{name: "not in pull", absent: true},
		{name: "commit read failure", failure: true, wantRetry: true},
		{name: "external parent inside pull", internalParent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			old, posted, _ := trunkMergeFixture(api)
			p := v3()
			head := posted
			if tc.old {
				head = old
			}
			if tc.absent {
				head = strings.Repeat("9", 40)
			}
			p["id"] = workerFixtureID(head, "1")
			c := proposalComment(p, 10)
			latest := v3()
			latest["id"] = workerFixtureID(posted, "2")
			api.comments = []Object{c, proposalComment(latest, 11)}
			if tc.ownChange {
				api.commits["/repos/owner/repo/pulls/2/commits?limit=50&page=1"][2]["parents"] = []any{Object{"sha": posted}}
			}
			if tc.internalParent {
				list := api.commits["/repos/owner/repo/pulls/2/commits?limit=50&page=1"]
				list[2]["parents"] = []any{Object{"sha": posted}, Object{"sha": list[0]["sha"]}}
			}
			if tc.failure {
				api.commitErr = errors.New("unavailable")
			}
			retry, err := s.adopt(context.Background(), "owner/repo", 1, c)
			if retry != tc.wantRetry || (err != nil) != tc.wantRetry || (api.posts == 1) != tc.wantPost {
				t.Fatalf("retry=%v err=%v posts=%d", retry, err, api.posts)
			}
			if tc.failure {
				rejection, e := s.read(context.Background(), "rejection:owner/repo#1:10")
				if e != nil || rejection != nil {
					t.Fatalf("transient failure rejected: %v %v", rejection, e)
				}
				api.commitErr = nil
				if _, e = s.adopt(context.Background(), "owner/repo", 1, c); e != nil || api.posts != 1 {
					t.Fatalf("retry failed: %v posts=%d", e, api.posts)
				}
			}
		})
	}
}

func TestAdoptIncompleteCommitListRetries(t *testing.T) {
	for _, tc := range []string{"empty", "missing parents", "empty parents", "bad sha", "missing head", "wrong count", "duplicate pages"} {
		t.Run(tc, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			_, posted, _ := trunkMergeFixture(api)
			p := v3()
			p["id"] = workerFixtureID(posted, "1")
			c := proposalComment(p, 10)
			api.comments = []Object{c}
			route := "/repos/owner/repo/pulls/2/commits?limit=50&page=1"
			list := api.commits[route]
			switch tc {
			case "empty":
				api.commits[route] = []Object{}
			case "missing parents":
				delete(list[1], "parents")
			case "empty parents":
				list[2]["parents"] = []any{}
			case "bad sha":
				list[0]["sha"] = "bad"
			case "missing head":
				api.commits[route] = list[:2]
			case "wrong count":
				api.pull["commits"] = 4
			case "duplicate pages":
				for page := 1; page <= 2; page++ {
					batch := make([]Object, 50)
					for i := range batch {
						batch[i] = Object{"sha": fmt.Sprintf("%040x", 100+i), "parents": []any{}}
					}
					api.commits[fmt.Sprintf("/repos/owner/repo/pulls/2/commits?limit=50&page=%d", page)] = batch
				}
			}
			retry, err := s.adopt(context.Background(), "owner/repo", 1, c)
			if !retry || err == nil {
				t.Fatalf("expected retry: %v %v", retry, err)
			}
			if api.posts != 0 {
				t.Fatal("unexpected post")
			}
		})
	}
}

func TestAdoptTrunkMergeThroughGiteaClient(t *testing.T) {
	s, api, _ := adoptFixture(t)
	_, posted, _ := trunkMergeFixture(api)
	api.pull["commits"] = 3
	p := v3()
	p["id"] = workerFixtureID(posted, "1")
	comment := proposalComment(p, 10)
	api.comments = []Object{comment}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value any
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/pulls/2":
			value = api.pull
		case "/api/v1/repos/owner/repo/pulls/2/commits":
			if r.URL.Query().Get("limit") != "50" {
				t.Errorf("unexpected query: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("page") == "1" {
				value = api.commits["/repos/owner/repo/pulls/2/commits?limit=50&page=1"]
			} else {
				value = []Object{}
			}
		case "/api/v1/repos/owner/repo/issues/1/comments":
			value = api.comments
		case "/api/v1/repos/owner/repo/issues/1":
			value = api.issue
		case "/api/v1/repos/owner/repo/issues/comments/10":
			value = comment
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "fixture-token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New(server.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.o.Gitea, s.o.Comments = client, client.Comments
	retry, err := s.adopt(context.Background(), "owner/repo", 1, comment)
	if retry || err != nil || api.posts != 1 {
		t.Fatalf("retry=%v err=%v posts=%d", retry, err, api.posts)
	}
}
