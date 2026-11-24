package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

const draftID = "11111111-1111-4111-8111-111111111111"

type memoryAPI struct {
	mu         sync.Mutex
	issues     []map[string]any
	posts      int
	uncertain  bool
	lose       bool
	refuse     bool
	historyErr error
}

func (a *memoryAPI) Issues(ctx context.Context, repo, state string) ([]map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.historyErr != nil {
		return nil, a.historyErr
	}
	return append([]map[string]any{}, a.issues...), nil
}
func (a *memoryAPI) Issue(ctx context.Context, repo string, n int64) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, i := range a.issues {
		if value, _ := integer(i["number"]); value == n {
			return i, nil
		}
	}
	return nil, fail(404, "missing")
}
func (a *memoryAPI) CreateIssue(ctx context.Context, repo string, input any) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refuse {
		a.refuse = false
		return nil, &gitea.Error{UnknownOutcome: true, DialRefused: true}
	}
	a.posts++
	body := row(input)
	issue := map[string]any{"number": int64(a.posts), "state": "open", "body": body["body"], "title": body["title"], "user": map[string]any{"id": json.Number("10")}, "created_at": "2026-11-01T00:00:00.000Z"}
	if !a.lose {
		a.issues = append(a.issues, issue)
	}
	if a.uncertain {
		a.uncertain = false
		return nil, &gitea.Error{UnknownOutcome: true}
	}
	return issue, nil
}
func fixture(t *testing.T) (*Service, *memoryAPI) {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("専用PostgreSQL未設定")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("専用DBだけが使用可能")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "filing_" + strings.ReplaceAll(id, "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pg, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Migrate(ctx, pg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pg.Close()
		admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	records, err := consultations.OpenRecords(ctx, pgtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(pg, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	api := &memoryAPI{}
	s, err := New(ctx, Options{Records: records, API: api, Jobs: queue, Project: func(string) error { return nil }, Own: func(user map[string]any) bool { n, _ := integer(user["id"]); return n == 10 }, Build: func(ctx context.Context, d Record) (Prepared, error) {
		return Prepared{Title: "件名", Body: "本文\n<!-- issue-desk:" + fleet.Text(d["id"]) + " -->", Job: jobs.NewJob{Scope: []string{"a.go"}}}, nil
	}, Check: func(context.Context, Record) error { return nil }, Link: func(repo string, n int64) string { return "/fixture" }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = records.Change(ctx, "drafts", draftID, func(*sql.Tx, Record) (Record, error) {
		return Record{"id": draftID, "project": "owner/app", "status": "draft", "ready": true, "revision": 0, "questions": []any{}, "messages": []any{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, api
}
func TestPostgresConcurrentSubmissionCreatesOneIssueAndJob(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if api.posts != 1 {
		t.Fatal("duplicate Gitea submission", api.posts)
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || job == nil {
		t.Fatal("job not filed", err)
	}
	var pending int
	s.options.Records.DB.QueryRow("SELECT count(*) FROM job_filings").Scan(&pending)
	if pending != 0 {
		t.Fatal("completed outbox remains")
	}
}
func TestPostgresDialRefusalCanResume(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	api.refuse = true
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("refusal lost")
	}
	d, _, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || d["attempted"] != false || d["status"] != "submit_error" {
		t.Fatalf("draft=%v err=%v", d, err)
	}
	d, err = s.Submit(ctx, draftID, Record{"revision": d["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil || d["status"] != "submitted" || api.posts != 1 || len(api.issues) != 1 {
		t.Fatalf("draft=%v err=%v posts=%d issues=%d", d, err, api.posts, len(api.issues))
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || job == nil {
		t.Fatalf("job=%v err=%v", job, err)
	}
	_, count, err := s.options.Jobs.List(ctx, "owner/app", nil, 100, 0)
	if err != nil || count != 1 {
		t.Fatalf("jobs=%d err=%v", count, err)
	}
}
func TestPostgresUnknownOutcomeReconcilesWithoutSecondPost(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	api.uncertain = true
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("unknown outcome lost")
	}
	d, _, _ := s.options.Records.Get(ctx, "drafts", draftID)
	if d["status"] != "submit_unknown" || d["attempted"] != true {
		t.Fatal(d)
	}
	d, err := s.Submit(ctx, draftID, Record{}, auth.Principal{Kind: auth.Screen})
	if err != nil || d["status"] != "submitted" || api.posts != 1 {
		t.Fatalf("%+v %v posts=%d", d, err, api.posts)
	}
}
func TestPostgresUnfoundUnknownSubmissionNeverAutomaticallyResends(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	api.uncertain, api.lose = true, true
	s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if _, err := s.Submit(ctx, draftID, Record{}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("missing unknown issue reported successful")
	}
	if api.posts != 1 {
		t.Fatal("ambiguous mutation resent")
	}
}
func TestSplitOrderRejectsCyclesBeforeAnySubmission(t *testing.T) {
	if _, err := splitOrder([]Record{{"_dependsOn": []any{2}}, {"_dependsOn": []any{1}}}); err == nil {
		t.Fatal("cycle accepted")
	}
	order, err := splitOrder([]Record{{"_dependsOn": []any{3}}, {}, {}})
	if err != nil || len(order) != 3 || order[0] != 2 {
		t.Fatal(order, err)
	}
}

func TestPostgresClosedIssueIsNotQueuedFromPendingOutbox(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	api.issues = []map[string]any{{"number": int64(1), "state": "closed", "title": "完了済み", "body": "本文"}}
	entry := pending{Repo: "owner/app", Number: 1, DraftID: draftID, Job: &jobs.NewJob{Repo: "owner/app", Issue: 1, Scope: []string{"a.go"}}}
	raw, _ := json.Marshal(entry)
	if _, err := s.options.Records.DB.Exec("INSERT INTO job_filings(key,json) VALUES('owner/app#1',?)", string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := s.FilePending(ctx); err != nil {
		t.Fatal(err)
	}
	if job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1); err != nil || job != nil {
		t.Fatal("closed issue queued", job, err)
	}
	var pendingCount int
	s.options.Records.DB.QueryRow("SELECT count(*) FROM job_filings").Scan(&pendingCount)
	if pendingCount != 0 {
		t.Fatal("closed receipt not completed")
	}
}

func TestPostgresSplitKeepsImagesAndSiblingDependencyAcrossReconcile(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	photo := "33333333-3333-4333-8333-333333333333"
	messages := []any{Record{"id": draftID, "role": "user", "text": "依頼", "images": []any{Record{"id": photo, "messageId": draftID}}}}
	reviews := []any{Record{"id": photo, "readable": true, "screen": "画面", "location": "位置", "change": "変更", "uncertainty": ""}}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["messages"], d["imageReviews"], d["splitIssues"] = messages, reviews, []any{Record{}, Record{}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.options.SplitPlan = func(d Record) ([]Record, error) {
		return []Record{{"project": "owner/app", "title": "先行", "body": "本文", "messages": d["messages"], "imageReviews": d["imageReviews"], "imageSourceId": d["id"], "changeScope": Record{"known": true, "paths": []any{"a.go"}}, "_dependsOn": []any{}}, {"project": "owner/app", "title": "後続", "body": "本文", "messages": d["messages"], "imageReviews": d["imageReviews"], "imageSourceId": d["id"], "changeScope": Record{"known": true, "paths": []any{"b.go"}}, "_dependsOn": []any{1}}}, nil
	}
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) {
		return Prepare(ctx, BuildConfig{Config: map[string]any{"model": "fixture"}, Records: s.options.Records}, d)
	}
	attachments := 0
	failed := false
	s.options.Attach = func(ctx context.Context, d Record, issue map[string]any) error {
		attachments++
		if !hasImages(d) || fleet.Text(d["imageSourceId"]) != draftID || len(fleet.Items(d["imageReviews"])) != 1 {
			t.Fatal("split image context lost", d)
		}
		number, _ := integer(issue["number"])
		if number == 2 && !failed {
			failed = true
			return fail(503, "fixture attachment interrupted")
		}
		if number == 2 {
			_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, parent Record) (Record, error) {
				parent["error"], parent["errorCode"] = "old", "old_code"
				return parent, nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	}
	if _, err = s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("interruption not observed")
	}
	parent, _, _ := s.options.Records.Get(ctx, "drafts", draftID)
	ids := fleet.Items(parent["splitChildren"])
	if len(ids) != 2 {
		t.Fatal("split IDs not saved")
	}
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["error"], d["errorCode"] = "old", "old_code"
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "submitted" || api.posts != 2 || attachments != 3 {
		t.Fatalf("split not reconciled: %+v posts=%d attachments=%d", result, api.posts, attachments)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || saved["error"] != nil || saved["errorCode"] != nil {
		t.Fatal(saved, err)
	}
	current := fleet.Items(result["splitChildren"])
	if current[0] != ids[0] || current[1] != ids[1] {
		t.Fatal("split IDs changed")
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 2)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	detail, err := s.options.Jobs.Detail(ctx, job.ID)
	if err != nil || len(detail["dependsOn"].([]int64)) != 1 {
		t.Fatal("sibling dependency lost", detail, err)
	}
}

func TestPostgresReconciliationUsesPersistedJobWithoutCurrentAI(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	api.uncertain = true
	calls := 0
	original := s.options.Build
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) { calls++; return original(ctx, d) }
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("不明な送信結果が失われました")
	}
	s.options.Build = func(context.Context, Record) (Prepared, error) {
		t.Fatal("送信済みの照合でAI・現設定を再実行しました")
		return Prepared{}, nil
	}
	s.options.Check = func(context.Context, Record) error {
		t.Fatal("送信済みの照合で新規送信の条件を再確認しました")
		return nil
	}
	d, err := s.Submit(ctx, draftID, Record{}, auth.Principal{Kind: auth.Screen})
	if err != nil || d["status"] != "submitted" || calls != 1 || api.posts != 1 {
		t.Fatalf("結果=%v エラー=%v 構築=%d 投稿=%d", d, err, calls, api.posts)
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || job == nil || len(job.Scope) != 1 || job.Scope[0] != "a.go" {
		t.Fatalf("保存済みjobを復元できません: %v %v", job, err)
	}
}

func TestPostgresLegacyReconciliationReconstructsPublishedIssue(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	api.uncertain = true
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) {
		d["title"], d["body"], d["changeScope"] = "保存済み依頼", "本文", Record{"known": true, "paths": []any{"legacy.go"}}
		return Prepare(ctx, BuildConfig{Config: map[string]any{"model": "fixture"}, Records: s.options.Records}, d)
	}
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("送信結果不明が必要です")
	}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) { delete(d, "preparedJob"); return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	s.options.Build = func(context.Context, Record) (Prepared, error) {
		t.Fatal("旧記録の復旧でAIを再実行しました")
		return Prepared{}, nil
	}
	if _, err := s.Submit(ctx, draftID, Record{}, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || job == nil || len(job.Scope) != 1 || job.Scope[0] != "legacy.go" || job.Provider == nil || *job.Provider != "codex" {
		t.Fatalf("公開済みIssueの設定を復元できません: %v %v", job, err)
	}
}

func TestPostgresImageFilingProjectsThePublishedBody(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["messages"] = []any{Record{"images": []any{Record{"id": "fixture-photo"}}}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.options.Attach = func(context.Context, Record, map[string]any) error {
		api.mu.Lock()
		defer api.mu.Unlock()
		api.issues[0]["body"] = fleet.Text(api.issues[0]["body"]) + "\n公開された画像説明"
		return nil
	}
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if !strings.Contains(fleet.Text(row(job.View["acceptanceTransferInput"])["body"]), "公開された画像説明") {
		t.Fatal("添付前の本文を受入条件へ渡しました")
	}
}

func TestPostgresSubmissionClearsSavedErrorCodeAtCompletion(t *testing.T) {
	s, _ := fixture(t)
	ctx := context.Background()
	s.options.Attach = func(ctx context.Context, d Record, _ map[string]any) error {
		_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
			current["error"], current["errorCode"] = "old", "old_code"
			return current, nil
		})
		return err
	}
	_, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || d["status"] != "submitted" || d["error"] != nil || d["errorCode"] != nil {
		t.Fatal(d, err)
	}
}

func TestPostgresFollowupReconcilePreservesIntentAcrossRestart(t *testing.T) {
	for _, mode := range []string{"found", "missing", "history error"} {
		t.Run(mode, func(t *testing.T) {
			s, api := fixture(t)
			ctx := context.Background()
			_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) { d["proposal"] = Record{"key": "fixture"}; return d, nil })
			if err != nil {
				t.Fatal(err)
			}
			s.options.ValidateFollowup = func(context.Context, Record) error { return nil }
			calls, builds := 0, 0
			s.options.ValidateNewPost = func(context.Context, Record) error { calls++; return nil }
			build := s.options.Build
			s.options.Build = func(ctx context.Context, d Record) (Prepared, error) { builds++; return build(ctx, d) }
			api.uncertain = true
			api.lose = mode == "missing"
			if _, err = s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
				t.Fatal("expected unknown outcome")
			}
			before, _, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "history error" {
				api.historyErr = errors.New("history failed")
			}
			options := s.options
			options.ValidateNewPost = func(context.Context, Record) error { t.Fatal("head checked during recovery"); return nil }
			options.Build = func(context.Context, Record) (Prepared, error) {
				t.Fatal("body regenerated during recovery")
				return Prepared{}, nil
			}
			s, err = New(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Submit(ctx, draftID, Record{}, auth.Principal{Kind: auth.Screen})
			after, _, readErr := s.options.Records.Get(ctx, "drafts", draftID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if api.posts != 1 || calls != 2 || builds != 1 {
				t.Fatalf("posts=%d checks=%d builds=%d", api.posts, calls, builds)
			}
			if mode == "found" {
				if err != nil || after["status"] != "submitted" {
					t.Fatalf("reconcile: %v %v", after, err)
				}
				return
			}
			if err == nil || after["status"] != "submit_unknown" || after["attempted"] != true {
				t.Fatalf("lost uncertainty: %v %v", after, err)
			}
			for _, key := range []string{"id", "postBody", "preparedJob"} {
				a, _ := json.Marshal(after[key])
				b, _ := json.Marshal(before[key])
				if string(a) != string(b) || after[key] == nil {
					t.Fatalf("lost %s", key)
				}
			}
		})
	}
}

func TestPostgresSplitReplansUnfiledChild(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["splitIssues"] = []any{Record{}}
		d["dependsOnIssues"] = []any{5252}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.options.SplitPlan = func(d Record) ([]Record, error) {
		return []Record{{"project": "owner/app", "title": "子", "body": d["body"], "dependsOnIssues": d["dependsOnIssues"], "dependencies": d["dependsOnIssues"], "messages": []any{Record{"text": "保存"}}, "imageReviews": []any{Record{"id": "image"}}}}, nil
	}
	s.options.Check = func(_ context.Context, d Record) error {
		if deps := fleet.Items(d["dependsOnIssues"]); len(deps) > 0 {
			if n, _ := integer(deps[0]); n == 5252 {
				return fail(400, "先行Issueが不正")
			}
		}
		return nil
	}
	var observed Record
	s.options.Build = func(_ context.Context, d Record) (Prepared, error) {
		observed = clone(d)
		return Prepared{Title: "子", Body: "本文 <!-- issue-desk:" + fleet.Text(d["id"]) + " -->", Job: jobs.NewJob{Scope: []string{"a.go"}}}, nil
	}
	if _, err = s.Submit(ctx, draftID, Record{"revision": 0}, p); err == nil {
		t.Fatal("旧依存先が受理された")
	}
	parent, _, _ := s.options.Records.Get(ctx, "drafts", draftID)
	ids := fleet.Items(parent["splitChildren"])
	childID := fleet.Text(ids[0])
	before, _, _ := s.options.Records.Get(ctx, "drafts", childID)
	if before["status"] != "submit_error" || before["attempted"] == true {
		t.Fatal(before)
	}
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["dependsOnIssues"] = []any{5256}
		d["body"] = "新本文"
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ = s.options.Records.Get(ctx, "drafts", draftID)
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, p)
	if err != nil {
		t.Fatal(err)
	}
	deps := fleet.Items(observed["dependencies"])
	var dep int64
	if len(deps) == 1 {
		dep, _ = integer(deps[0])
	}
	if fleet.Text(fleet.Items(result["splitChildren"])[0]) != childID || api.posts != 1 || fleet.Text(observed["body"]) != "新本文" || dep != 5256 {
		t.Fatalf("result=%v observed=%v posts=%d", result, observed, api.posts)
	}
	if observed["createdAt"] != before["createdAt"] || len(fleet.Items(observed["messages"])) != 1 || len(fleet.Items(observed["imageReviews"])) != 1 || observed["error"] != nil || observed["lastFailure"] != nil {
		t.Fatal(observed)
	}
}

func TestPostgresSplitResendsErrorOrStalledChild(t *testing.T) {
	for _, status := range []string{"error", "stalled"} {
		t.Run(status, func(t *testing.T) {
			s, api := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) { d["splitIssues"] = []any{Record{}}; return d, nil })
			if err != nil {
				t.Fatal(err)
			}
			title, body := "旧題名", "旧本文"
			s.options.SplitPlan = func(Record) ([]Record, error) {
				return []Record{{"project": "owner/app", "title": title, "body": body}}, nil
			}
			s.options.Check = func(context.Context, Record) error { return fail(400, "失敗") }
			if _, err = s.Submit(ctx, draftID, Record{"revision": 0}, p); err == nil {
				t.Fatal("expected initial failure")
			}
			parent, _, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			ids := fleet.Items(parent["splitChildren"])
			id := fleet.Text(ids[0])
			_, err = s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, d Record) (Record, error) {
				d["status"], d["stalled"], d["run"], d["retryAt"], d["queued"] = status, true, Record{"old": true}, "old", true
				return d, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			title, body = "新題名", "新本文"
			s.options.Check = func(context.Context, Record) error { return nil }
			s.options.Build = func(_ context.Context, d Record) (Prepared, error) {
				for _, key := range []string{"error", "lastFailure", "stalled", "run", "retryAt", "queued"} {
					if d[key] != nil {
						t.Errorf("%s remains: %v", key, d[key])
					}
				}
				return Prepared{Title: fleet.Text(d["title"]), Body: fleet.Text(d["body"]), Job: jobs.NewJob{Scope: []string{"a.go"}}}, nil
			}
			result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, p)
			if err != nil {
				t.Fatal(err)
			}
			if result["status"] != "submitted" || len(fleet.Items(result["splitChildren"])) != len(ids) || fleet.Text(fleet.Items(result["splitChildren"])[0]) != id || api.posts != len(ids) || len(api.issues) != 1 || api.issues[0]["title"] != title || api.issues[0]["body"] != body {
				t.Fatalf("result=%v posts=%d issues=%v", result, api.posts, api.issues)
			}
		})
	}
}

func TestPostgresSplitRejectsChangedFiledOrUncertainChild(t *testing.T) {
	for _, status := range []string{"submitted", "submitting", "submit_unknown", "attempted", "attempted_error", "attempted_stalled"} {
		t.Run(status, func(t *testing.T) {
			s, api := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			_, _ = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) { d["splitIssues"] = []any{Record{}}; return d, nil })
			title := "旧"
			s.options.SplitPlan = func(Record) ([]Record, error) {
				return []Record{{"project": "owner/app", "title": title}}, nil
			}
			s.options.Check = func(context.Context, Record) error { return fail(400, "失敗") }
			_, _ = s.Submit(ctx, draftID, Record{"revision": 0}, p)
			parent, _, _ := s.options.Records.Get(ctx, "drafts", draftID)
			id := fleet.Text(fleet.Items(parent["splitChildren"])[0])
			_, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, d Record) (Record, error) {
				if status == "attempted_error" || status == "attempted_stalled" {
					d["status"] = strings.TrimPrefix(status, "attempted_")
					d["attempted"] = true
				} else if status == "attempted" {
					d[status] = true
				} else {
					d["status"] = status
				}
				return d, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, _, _ := s.options.Records.Get(ctx, "drafts", id)
			title = "新"
			_, err = s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, p)
			var problem *contract.Problem
			if !errors.As(err, &problem) || problem.Status != 409 {
				t.Fatalf("expected 409: %v", err)
			}
			if api.posts != 0 {
				t.Fatalf("unexpected posts: %d", api.posts)
			}
			action := "子の送信結果を確認してください。"
			if status == "submitted" {
				action = "起票済みの子は計画を変えられません。"
			}
			if status == "submitting" || status == "submit_unknown" {
				action = "起票結果を確認してから対応してください。"
			}
			saved, _, getErr := s.options.Records.Get(ctx, "drafts", draftID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			for _, value := range []string{problem.Message, fleet.Text(saved["error"]), fleet.Text(row(saved["lastFailure"])["message"])} {
				for _, want := range []string{id, fleet.Text(before["title"]), fleet.Text(before["status"]), action} {
					if !strings.Contains(value, want) {
						t.Fatalf("%q missing %q", value, want)
					}
				}
			}
			after, _, _ := s.options.Records.Get(ctx, "drafts", id)
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatalf("child changed: %s %s", a, b)
			}
		})
	}
}

func TestPostgresSplitRejectsFinishedWithoutIssueChild(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) { d["splitIssues"] = []any{Record{}}; return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	title := "旧"
	s.options.SplitPlan = func(Record) ([]Record, error) {
		return []Record{{"project": "owner/app", "title": title}}, nil
	}
	s.options.Check = func(context.Context, Record) error { return fail(400, "失敗") }
	if _, err = s.Submit(ctx, draftID, Record{"revision": 0}, p); err == nil {
		t.Fatal("expected initial failure")
	}
	parent, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("parent: found=%v err=%v", found, err)
	}
	id := fleet.Text(fleet.Items(parent["splitChildren"])[0])
	_, err = s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, d Record) (Record, error) { d["finishedWithoutIssue"] = true; return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	before, found, err := s.options.Records.Get(ctx, "drafts", id)
	if err != nil || !found {
		t.Fatalf("child: found=%v err=%v", found, err)
	}
	title = "新"
	_, err = s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, p)
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem.Status != 409 || problem.Message != "起票せずに終えた子があるため再送できません。残りの子を終えてください。" {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.posts != 0 {
		t.Fatalf("unexpected posts: %d", api.posts)
	}
	after, found, err := s.options.Records.Get(ctx, "drafts", id)
	if err != nil || !found {
		t.Fatalf("child: found=%v err=%v", found, err)
	}
	saved, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("parent: found=%v err=%v", found, err)
	}
	if encoded(before) != encoded(after) {
		t.Fatalf("child changed: %v -> %v", before, after)
	}
	if encoded(parent["error"]) != encoded(saved["error"]) || encoded(parent["lastFailure"]) != encoded(saved["lastFailure"]) {
		t.Fatalf("parent failure changed: %v -> %v", parent, saved)
	}
}

func TestPostgresSplitReplansChangedUserReference(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	principal := auth.Principal{Kind: auth.Screen}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["splitIssues"] = []any{Record{"title": "子", "purpose": "修正", "acceptance": []any{"定義を参照"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"a.go"}}}}
		d["messages"] = []any{Record{"role": "user", "id": "message", "text": "古い定義"}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.options.SplitPlan = PlanSplit
	reject := true
	s.options.Check = func(context.Context, Record) error {
		if reject {
			return fail(400, "一時停止")
		}
		return nil
	}
	var observed Record
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) {
		observed = clone(d)
		c := configFixture()
		c.Project = func(string) error { return nil }
		return Prepare(ctx, c, d)
	}
	if _, err = s.Submit(ctx, draftID, Record{"revision": 0}, principal); err == nil {
		t.Fatal("expected initial failure")
	}
	parent, _, _ := s.options.Records.Get(ctx, "drafts", draftID)
	id := fleet.Text(fleet.Items(parent["splitChildren"])[0])
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["messages"] = []any{Record{"role": "user", "id": "message", "text": "新しい定義"}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ = s.options.Records.Get(ctx, "drafts", draftID)
	reject = false
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, principal)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Text(fleet.Items(result["splitChildren"])[0]) != id || api.posts != 1 || !strings.Contains(fleet.Text(observed["body"]), "新しい定義") || strings.Contains(fleet.Text(observed["body"]), "古い定義") {
		t.Fatalf("result=%v observed=%v posts=%d", result, observed, api.posts)
	}
}

func TestPostgresSplitCorrectsDependenciesBeforeFiling(t *testing.T) {
	s, api := correctionFixture(t, correctionProposals())
	ctx := context.Background()
	result, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	parent := getCorrectionRecord(t, s, draftID)
	assertCorrectionHistory(t, parent, 3, 2, "[1,2]", "[1]")
	if encoded(row(fleet.Items(parent["splitIssues"])[2])["dependsOn"]) != "[1]" || result["status"] != "submitted" || api.posts != 3 {
		t.Fatalf("%v posts=%d", parent, api.posts)
	}
	ids := fleet.Items(parent["splitChildren"])
	for i, id := range ids {
		child := getCorrectionRecord(t, s, fleet.Text(id))
		index, _ := integer(child["splitIndex"])
		number, _ := integer(row(child["issue"])["number"])
		if index != int64(i+1) || number != int64(i+1) {
			t.Fatalf("mapping: %v", child)
		}
		if !strings.Contains(fleet.Text(child["splitBackground"]), "3. 実装B（依存: 1）") || strings.Contains(fleet.Text(child["splitBackground"]), "3. 実装B（依存: 1, 2）") {
			t.Fatal(child["splitBackground"])
		}
	}
	child := getCorrectionRecord(t, s, fleet.Text(ids[2]))
	body := fleet.Text(api.issues[2]["body"])
	if encoded(child["_dependsOn"]) != "[1]" || encoded(child["dependencies"]) != "[1]" || !strings.Contains(body, "この案の依存: 分割案1（契約）") || strings.Contains(body, "この案の依存: 分割案1（契約）、分割案2") || !strings.Contains(body, "依存の自動補正: 分割案2") || !strings.Contains(body, "契約案への依存があるため") {
		t.Fatalf("child=%v body=%s", child, body)
	}
	if err = s.options.Jobs.ResolveFilingReferences(ctx); err != nil {
		t.Fatal(err)
	}
	contractJob, _ := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	job, _ := s.options.Jobs.ByIssue(ctx, "owner/app", 3)
	detail, err := s.options.Jobs.Detail(ctx, job.ID)
	if err != nil || encoded(detail["dependsOn"]) != encoded([]int64{contractJob.ID}) {
		t.Fatalf("job=%v err=%v", detail, err)
	}
	// A repeated interactive request cannot create more issues or correction records.
	repeated, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil || encoded(repeated["issues"]) != encoded(result["issues"]) || api.posts != 3 {
		t.Fatalf("repeat: %v result=%v posts=%d", err, repeated, api.posts)
	}
	assertCorrectionHistory(t, getCorrectionRecord(t, s, draftID), 3, 2, "[1,2]", "[1]")
	all, err := s.options.Jobs.AllForIssues(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("jobs=%v err=%v", all, err)
	}
}

func TestPostgresSplitKeepsExplicitDependencyException(t *testing.T) {
	plans := correctionProposals()
	row(plans[2])["purpose"] = "実装B。依頼者の指定による依存: 分割案2"
	s, api := correctionFixture(t, plans)
	result, err := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	child := getCorrectionRecord(t, s, fleet.Text(fleet.Items(result["splitChildren"])[2]))
	if api.posts != 3 || len(fleet.Items(result["splitDependencyCorrections"])) != 0 || encoded(child["_dependsOn"]) != "[1,2]" || encoded(child["dependencies"]) != "[1,2]" || !strings.Contains(fleet.Text(api.issues[2]["body"]), "分割案2（実装A）") {
		t.Fatalf("result=%v child=%v", result, child)
	}
}

func TestPostgresSplitCorrectsOnlyUnfiledSiblingAndPreservesExternalDependencies(t *testing.T) {
	plans := correctionProposals()[1:]
	row(plans[0])["dependsOn"] = []any{}
	row(plans[1])["changeScope"] = Record{"known": true, "paths": []any{"docs/guide.txt"}}
	row(plans[1])["dependsOn"] = []any{1}
	row(plans[1])["dependsOnIssues"] = []any{Record{"source": "gitea", "number": 80}}
	s, api := correctionFixture(t, plans)
	ctx := context.Background()
	for _, number := range []int64{80, 81} {
		if _, err := s.options.Jobs.Create(ctx, jobs.NewJob{Repo: "owner/app", Issue: number, Title: "分割外の依存", Scope: []string{"external.go"}}); err != nil {
			t.Fatal(err)
		}
	}
	ids := seedCorrectionChildren(t, s)
	first, err := s.normal(ctx, getCorrectionRecord(t, s, ids[0]), false)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON := encoded(first)
	changeCorrectionRecord(t, s, ids[1], func(d Record) { d["_splitBaseDependencies"] = []any{81}; d["dependencies"] = []any{81, 1} })
	parent := getCorrectionRecord(t, s, draftID)
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	child := getCorrectionRecord(t, s, ids[1])
	if api.posts != 2 || encoded(getCorrectionRecord(t, s, ids[0])) != firstJSON || fleet.Text(fleet.Items(result["splitChildren"])[1]) != ids[1] || encoded(child["_dependsOn"]) != "[]" || encoded(child["dependencies"]) != "[81]" || encoded(child["_splitBaseDependencies"]) != "[81]" || !strings.Contains(fleet.Text(child["splitBackground"]), "2. 実装B（依存: なし）") {
		t.Fatalf("result=%v child=%v", result, child)
	}
	body := fleet.Text(api.issues[1]["body"])
	if !strings.Contains(body, "この案の依存: なし") || !strings.Contains(body, "分割案1への依存を外しました") || !strings.Contains(body, "資料案は契約案以外") || !strings.Contains(body, "<!-- issue-desk-dependencies:[81,80] -->") {
		t.Fatal(body)
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 2)
	if err != nil || job == nil || encoded(job.View["dependsOnIssues"]) != `[{"issue":81,"repo":"owner/app"},{"issue":80,"repo":"owner/app"}]` {
		t.Fatalf("job=%v err=%v", job, err)
	}
	assertCorrectionHistory(t, result, 2, 1, "[1]", "[]")
}

func TestPostgresSplitCorrectionRetryReconcilesUnknownWithoutDuplicates(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "found", true: "missing"}[missing], func(t *testing.T) {
			s, api := correctionFixture(t, correctionProposals())
			ctx := context.Background()
			s.options.BeforeSplitChild = func(_ context.Context, _ Record, child Record, _ []any) (Record, error) {
				index, _ := integer(child["splitIndex"])
				if index == 3 {
					api.uncertain = true
					api.lose = missing
				}
				return child, nil
			}
			if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
				t.Fatal("expected unknown")
			}
			parent := getCorrectionRecord(t, s, draftID)
			ids := fleet.Items(parent["splitChildren"])
			unknown := getCorrectionRecord(t, s, fleet.Text(ids[2]))
			if unknown["status"] != "submit_unknown" || api.posts != 3 {
				t.Fatalf("%v posts=%d", unknown, api.posts)
			}
			before := encoded(unknown)
			snapshots := []string{encoded(getCorrectionRecord(t, s, fleet.Text(ids[0]))), encoded(getCorrectionRecord(t, s, fleet.Text(ids[1])))}
			// Reopen the persisted JSON store and resume through a new service instance.
			records, err := consultations.OpenRecords(ctx, s.options.Records.DB)
			if err != nil {
				t.Fatal(err)
			}
			options := s.options
			options.Records = records
			options.BeforeSplitChild = nil
			options.Build = func(context.Context, Record) (Prepared, error) {
				t.Fatal("recovery rebuilt a sent body")
				return Prepared{}, nil
			}
			options.Check = func(context.Context, Record) error { t.Fatal("recovery reran pre-filing check"); return nil }
			s, err = New(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
			if missing && err == nil || !missing && err != nil || api.posts != 3 {
				t.Fatalf("retry err=%v posts=%d", err, api.posts)
			}
			after := getCorrectionRecord(t, s, fleet.Text(ids[2]))
			if missing {
				if after["status"] != "submit_unknown" {
					t.Fatal(after)
				}
			} else if after["status"] != "submitted" {
				t.Fatal(after)
			}
			for _, key := range []string{"postBody", "preparedJob", "_splitFingerprint", "splitBackground", "_dependsOn", "dependencies"} {
				if encoded(after[key]) != encoded(unknown[key]) {
					t.Fatalf("changed %s before=%s after=%v", key, before, after)
				}
			}
			for i := 0; i < 2; i++ {
				if encoded(getCorrectionRecord(t, s, fleet.Text(ids[i]))) != snapshots[i] {
					t.Fatal("filed sibling changed")
				}
			}
			saved := getCorrectionRecord(t, s, draftID)
			assertCorrectionHistory(t, saved, 3, 2, "[1,2]", "[1]")
			all, err := s.options.Jobs.AllForIssues(ctx)
			expected := 3
			if missing {
				expected = 2
			}
			if err != nil || len(all) != expected {
				t.Fatalf("jobs=%v err=%v", all, err)
			}
		})
	}
}

func TestPostgresSplitCorrectionProtectsSavedChildrenInsideTransaction(t *testing.T) {
	for _, state := range []string{"issue", "submitted", "submitting", "submit_unknown", "attempted", "finishedWithoutIssue"} {
		t.Run(state, func(t *testing.T) {
			plans := correctionProposals()[1:]
			row(plans[0])["dependsOn"] = []any{}
			row(plans[1])["dependsOn"] = []any{1}
			row(plans[1])["changeScope"] = Record{"known": true, "paths": []any{"docs/guide.txt"}}
			s, api := correctionFixture(t, plans)
			ids := seedCorrectionChildren(t, s)
			// Capture the parent before the child changes; protection must read saved state.
			parent := getCorrectionRecord(t, s, draftID)
			changeCorrectionRecord(t, s, ids[0], func(d Record) { d["status"] = "submitted"; d["issue"] = Record{"project": "owner/app", "number": 41} })
			changeCorrectionRecord(t, s, ids[1], func(d Record) {
				switch state {
				case "issue":
					d["issue"] = Record{"project": "owner/app", "number": 42}
				case "attempted", "finishedWithoutIssue":
					d[state] = true
				default:
					d["status"] = state
					if state == "submitted" {
						d["issue"] = Record{"project": "owner/app", "number": 42}
					}
				}
			})
			before := encoded(getCorrectionRecord(t, s, ids[1]))
			planned := false
			s.options.SplitPlan = func(d Record) ([]Record, error) {
				planned = true
				if encoded(row(fleet.Items(d["splitIssues"])[1])["dependsOn"]) != "[1]" || len(fleet.Items(d["splitDependencyCorrections"])) != 0 {
					t.Fatal("protected proposal corrected", d)
				}
				return PlanSplit(d)
			}
			// Observe committed plans without advancing the protected child's send state.
			s.options.BeforeSplitChild = func(context.Context, Record, Record, []any) (Record, error) {
				return nil, fail(409, "検証のため送信前で停止")
			}
			_, err := s.split(context.Background(), parent)
			if ((err == nil) != (state == "submitted" || state == "issue")) || !planned || api.posts != 0 || encoded(getCorrectionRecord(t, s, ids[1])) != before {
				t.Fatalf("state=%s planned=%v err=%v posts=%d", state, planned, err, api.posts)
			}
			saved := getCorrectionRecord(t, s, draftID)
			if len(fleet.Items(saved["splitDependencyCorrections"])) != 0 || encoded(row(fleet.Items(saved["splitIssues"])[1])["dependsOn"]) != "[1]" {
				t.Fatal(saved)
			}
		})
	}
}

func TestPostgresSplitCorrectionRollsBackWhenParentChanges(t *testing.T) {
	s, api := correctionFixture(t, correctionProposals())
	parent := getCorrectionRecord(t, s, draftID)
	s.options.SplitPlan = func(d Record) ([]Record, error) {
		changeCorrectionRecord(t, s, draftID, func(saved Record) { saved["revision"] = 1; saved["title"] = "並行更新" })
		return PlanSplit(d)
	}
	_, err := s.split(context.Background(), parent)
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem.Status != 409 {
		t.Fatalf("expected 409: %v", err)
	}
	saved := getCorrectionRecord(t, s, draftID)
	if api.posts != 0 || len(fleet.Items(saved["splitChildren"])) != 0 || len(fleet.Items(saved["splitDependencyCorrections"])) != 0 || encoded(row(fleet.Items(saved["splitIssues"])[2])["dependsOn"]) != "[1,2]" || saved["title"] != "並行更新" {
		t.Fatal(saved)
	}
	var count int
	if err = s.options.Records.DB.QueryRow("SELECT count(*) FROM drafts").Scan(&count); err != nil || count != 1 {
		t.Fatalf("children committed without corrected parent: count=%d err=%v", count, err)
	}
}

func TestPostgresSplitCorrectionRetryUsesPersistedPlanAndRefreshesBackground(t *testing.T) {
	s, api := correctionFixture(t, correctionProposals())
	ctx := context.Background()
	ids := seedCorrectionChildren(t, s)
	before := getCorrectionRecord(t, s, ids[0])
	stop := true
	s.options.BeforeSplitChild = func(_ context.Context, _ Record, child Record, _ []any) (Record, error) {
		index, _ := integer(child["splitIndex"])
		if stop && index == 3 {
			return nil, fail(409, "一時停止")
		}
		return child, nil
	}
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("expected interruption")
	}
	parent := getCorrectionRecord(t, s, draftID)
	assertCorrectionHistory(t, parent, 3, 2, "[1,2]", "[1]")
	refreshed := getCorrectionRecord(t, s, ids[0])
	if api.posts != 2 || refreshed["_splitFingerprint"] != before["_splitFingerprint"] || refreshed["splitBackground"] == before["splitBackground"] || !strings.Contains(fleet.Text(refreshed["splitBackground"]), "3. 実装B（依存: 1）") {
		t.Fatalf("background not refreshed: before=%v after=%v posts=%d", before, refreshed, api.posts)
	}
	childBefore := getCorrectionRecord(t, s, ids[2])
	firstBefore := encoded(refreshed)
	stop = false
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	childAfter := getCorrectionRecord(t, s, ids[2])
	if api.posts != 3 || result["status"] != "submitted" || encoded(getCorrectionRecord(t, s, ids[0])) != firstBefore || childAfter["_splitFingerprint"] != childBefore["_splitFingerprint"] || encoded(result["splitChildren"]) != encoded(parent["splitChildren"]) {
		t.Fatalf("retry changed plan: %v posts=%d", result, api.posts)
	}
	if encoded(result["splitDependencyCorrections"]) != encoded(parent["splitDependencyCorrections"]) {
		t.Fatal("duplicate corrections", result)
	}
	all, err := s.options.Jobs.AllForIssues(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("jobs=%v err=%v", all, err)
	}
}

func TestPostgresSplitCorrectionRetryReflectsRevisedDependencies(t *testing.T) {
	s, api := correctionFixture(t, correctionProposals())
	ctx := context.Background()
	seedCorrectionChildren(t, s)
	s.options.BeforeSplitChild = func(_ context.Context, _ Record, child Record, _ []any) (Record, error) {
		if n, _ := integer(child["splitIndex"]); n == 3 {
			return nil, fail(409, "一時停止")
		}
		return child, nil
	}
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("expected interruption")
	}
	parent := changeCorrectionRecord(t, s, draftID, func(d Record) {
		row(fleet.Items(d["splitIssues"])[2])["dependsOn"] = []any{}
	})
	s.options.BeforeSplitChild = nil
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	body := fleet.Text(api.issues[2]["body"])
	if result["status"] != "submitted" || api.posts != 3 || strings.Contains(body, "依存の自動補正: 分割案2") || !strings.Contains(body, "この案の依存: なし") {
		t.Fatalf("result=%v body=%s posts=%d", result, body, api.posts)
	}
}

func TestPostgresSplitMissingChildReturnsConflict(t *testing.T) {
	s, api := correctionFixture(t, correctionProposals())
	ctx := context.Background()
	ids := seedCorrectionChildren(t, s)
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(tx *sql.Tx, d Record) (Record, error) {
		_, err := tx.ExecContext(ctx, bindSQL("DELETE FROM drafts WHERE id=?"), ids[2])
		return d, err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem.Status != 409 || !strings.Contains(err.Error(), "分割先の相談がありません") || api.posts != 0 {
		t.Fatalf("err=%v posts=%d", err, api.posts)
	}
}

func TestPostgresSplitContinuesPastFiledChildWhenOnlyGeneratedBodyChanged(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	_, _ = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["splitIssues"] = []any{Record{}, Record{}}
		return d, nil
	})
	body := "旧本文"
	s.options.SplitPlan = func(Record) ([]Record, error) {
		return []Record{{"project": "owner/app", "title": "子1", "body": body}, {"project": "owner/app", "title": "子2", "body": body}}, nil
	}
	s.options.Check = func(context.Context, Record) error { return fail(400, "失敗") }
	_, _ = s.Submit(ctx, draftID, Record{"revision": 0}, p)
	parent, _, _ := s.options.Records.Get(ctx, "drafts", draftID)
	filed := fleet.Text(fleet.Items(parent["splitChildren"])[0])
	if _, err := s.options.Records.Change(ctx, "drafts", filed, func(_ *sql.Tx, d Record) (Record, error) {
		d["status"], d["issue"] = "submitted", Record{"project": "owner/app", "number": 1}
		return d, nil
	}); err != nil {
		t.Fatal(err)
	}
	body = "生成の変更で変わった本文"
	s.options.Check = func(context.Context, Record) error { return nil }
	s.options.Build = func(_ context.Context, d Record) (Prepared, error) {
		return Prepared{Title: fleet.Text(d["title"]), Body: fleet.Text(d["body"]), Job: jobs.NewJob{Scope: []string{"a.go"}}}, nil
	}
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, p)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "submitted" || api.posts != 1 {
		t.Fatalf("result=%v posts=%d", result, api.posts)
	}
}

func TestPostgresMarkerLookupReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		users      []int
		historyErr error
		wantError  bool
		wantPosts  int
	}{
		{name: "closed own candidate", users: []int{10}},
		{name: "other author", users: []int{11}, wantPosts: 1},
		{name: "duplicate own candidates", users: []int{10, 10}, wantError: true},
		{name: "history failure", historyErr: errors.New("history unavailable"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api := fixture(t)
			api.historyErr = tc.historyErr
			for i, user := range tc.users {
				api.issues = append(api.issues, map[string]any{"number": int64(i + 10), "state": "closed", "body": "<!-- issue-desk:" + draftID + " -->", "user": map[string]any{"id": user}, "title": "fixture", "created_at": "2026-11-01T00:00:00Z"})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if api.historyErr != nil {
						http.Error(w, "unavailable", http.StatusInternalServerError)
						return
					}
					if strings.Contains(r.URL.Path, "/issues/") {
						n := int64(0)
						fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &n)
						issue, err := api.Issue(r.Context(), "owner/app", n)
						if err != nil {
							http.Error(w, "missing", 404)
							return
						}
						_ = json.NewEncoder(w).Encode(issue)
						return
					}
					_ = json.NewEncoder(w).Encode(api.issues)
					return
				}
				var input map[string]any
				_ = json.NewDecoder(r.Body).Decode(&input)
				issue, err := api.CreateIssue(r.Context(), "owner/app", input)
				if err != nil {
					http.Error(w, "error", 500)
					return
				}
				_ = json.NewEncoder(w).Encode(issue)
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
			s.options.API = client
			_, err = s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
			if (err != nil) != tc.wantError || api.posts != tc.wantPosts {
				t.Fatalf("err=%v posts=%d", err, api.posts)
			}
		})
	}
}
