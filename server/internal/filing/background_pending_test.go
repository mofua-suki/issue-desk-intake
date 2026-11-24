package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/jobs"
)

type blockingPendingAPI struct {
	*memoryAPI
	started chan struct{}
	release chan struct{}
}

func (a *blockingPendingAPI) Issue(ctx context.Context, repo string, n int64) (map[string]any, error) {
	if n == 1 {
		close(a.started)
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return a.memoryAPI.Issue(ctx, repo, n)
}
func TestFilePendingStopsBetweenAcceptedRecordsPostgres(t *testing.T) {
	s, api := fixture(t)
	api.issues = []map[string]any{{"number": int64(1), "state": "open", "title": "first"}, {"number": int64(2), "state": "open", "title": "second"}}
	block := &blockingPendingAPI{memoryAPI: api, started: make(chan struct{}), release: make(chan struct{})}
	s.options.API = block
	for n := int64(1); n <= 2; n++ {
		raw, _ := json.Marshal(pending{Repo: "owner/app", Number: n, DraftID: draftID, Job: &jobs.NewJob{Repo: "owner/app", Issue: n, Title: "test", Scope: []string{"a.go"}}})
		if _, err := s.options.Records.DB.Exec("INSERT INTO job_filings(key,json) VALUES(?,?)", fmt.Sprintf("owner/app#%d", n), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	periodic, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.FilePendingWithLifetime(periodic, context.Background()) }()
	select {
	case <-block.started:
	case <-time.After(5 * time.Second):
		t.Fatal("配布が開始しません")
	}
	cancel()
	close(block.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("停止しません")
	}
	first, err := s.options.Jobs.ByIssue(context.Background(), "owner/app", 1)
	if err != nil || first == nil {
		t.Fatal("1件目未保存", err)
	}
	second, err := s.options.Jobs.ByIssue(context.Background(), "owner/app", 2)
	if err != nil || second != nil {
		t.Fatal("2件目が開始", err)
	}
	var count int
	if err = s.options.Records.DB.QueryRow("SELECT count(*) FROM job_filings WHERE key=?", "owner/app#2").Scan(&count); err != nil || count != 1 {
		t.Fatal("2件目の保留が失われた", count, err)
	}
}

type failingPendingAPI struct {
	*memoryAPI
	failNumber int64
	calls      int
}

func (a *failingPendingAPI) Issue(ctx context.Context, repo string, n int64) (map[string]any, error) {
	if n == a.failNumber {
		a.calls++
		return nil, fmt.Errorf("issue %d unavailable", n)
	}
	return a.memoryAPI.Issue(ctx, repo, n)
}

func TestPendingFailureDoesNotBlockSubmitOrOtherRecords(t *testing.T) {
	s, api := fixture(t)
	api.issues = []map[string]any{{"number": int64(99), "state": "open", "title": "failed"}}
	broken := &failingPendingAPI{memoryAPI: api, failNumber: 99}
	s.options.API = broken
	raw, _ := json.Marshal(pending{Repo: "owner/app", Number: 99, DraftID: draftID, Job: &jobs.NewJob{Repo: "owner/app", Issue: 99, Title: "failed", Scope: []string{"a.go"}}})
	if _, err := s.options.Records.DB.Exec("INSERT INTO job_filings(key,json) VALUES(?,?)", "owner/app#99", string(raw)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if err != nil || d["status"] != "submitted" {
		t.Fatalf("submit=%v err=%v", d, err)
	}
	issue := row(d["issue"])
	number, _ := integer(issue["number"])
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", number)
	if err != nil || job == nil {
		t.Fatalf("job=%v err=%v", job, err)
	}
	if _, err = s.Submit(ctx, draftID, nil, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	if err = s.FilePending(ctx); err == nil || broken.calls == 0 {
		t.Fatalf("pending err=%v calls=%d", err, broken.calls)
	}
}

func TestEstimateRetriesAfterIssueReadFailure(t *testing.T) {
	s, api := fixture(t)
	api.issues = []map[string]any{{"number": int64(1), "state": "open", "title": "test"}}
	broken := &failingPendingAPI{memoryAPI: api, failNumber: 1}
	s.options.API = broken
	calls := 0
	s.options.EstimateScope = func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return map[string]any{"known": true, "paths": []any{"fix.go"}}, nil
	}
	raw, _ := json.Marshal(pending{Repo: "owner/app", Number: 1, DraftID: draftID, Job: &jobs.NewJob{Repo: "owner/app", Issue: 1, Title: "test"}})
	if _, err := s.options.Records.DB.Exec("INSERT INTO job_filings(key,json) VALUES(?,?)", "owner/app#1", string(raw)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.FilePending(ctx); err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	broken.failNumber = 0
	if err := s.FilePending(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || job == nil || len(job.Scope) != 1 || job.Scope[0] != "fix.go" || calls != 1 {
		t.Fatalf("job=%+v err=%v calls=%d", job, err, calls)
	}
}

type onceIssueAPI struct {
	*memoryAPI
	calls int
}

func (a *onceIssueAPI) Issue(ctx context.Context, repo string, n int64) (map[string]any, error) {
	a.calls++
	if a.calls > 1 {
		return nil, fmt.Errorf("repeated issue read")
	}
	return a.memoryAPI.Issue(ctx, repo, n)
}

func TestEstimateUsesAlreadyReadIssue(t *testing.T) {
	s, api := fixture(t)
	api.issues = []map[string]any{{"number": int64(1), "state": "open", "title": "original", "body": "estimate input"}}
	once := &onceIssueAPI{memoryAPI: api}
	s.options.API = once
	calls := 0
	s.options.EstimateScope = func(_ context.Context, repo string, issue map[string]any) (map[string]any, error) {
		calls++
		if repo != "owner/app" || issue["title"] != "original" || issue["body"] != "estimate input" {
			t.Fatalf("estimate input: %s %v", repo, issue)
		}
		return map[string]any{"known": true, "paths": []any{"fix.go"}}, nil
	}
	raw, err := json.Marshal(pending{Repo: "owner/app", Number: 1, DraftID: draftID, Job: &jobs.NewJob{Repo: "owner/app", Issue: 1, Title: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.options.Records.DB.Exec("INSERT INTO job_filings(key,json) VALUES(?,?)", "owner/app#1", string(raw)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = s.FilePending(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || job == nil || len(job.Scope) != 1 || job.Scope[0] != "fix.go" || calls != 1 || once.calls != 1 {
		t.Fatalf("job=%+v err=%v estimates=%d reads=%d", job, err, calls, once.calls)
	}
}

func TestPendingCycleContinuesAfterBadRecord(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed=%v", malformed), func(t *testing.T) {
			s, api := fixture(t)
			api.issues = []map[string]any{{"number": int64(1), "state": "open", "title": "failed"}, {"number": int64(2), "state": "open", "title": "healthy"}}
			s.options.API = &failingPendingAPI{memoryAPI: api, failNumber: 1}
			for n := int64(1); n <= 2; n++ {
				raw, err := json.Marshal(pending{Repo: "owner/app", Number: n, DraftID: draftID, Job: &jobs.NewJob{Repo: "owner/app", Issue: n, Title: "test", Scope: []string{"a.go"}}})
				if err != nil {
					t.Fatal(err)
				}
				if malformed && n == 1 {
					raw = []byte("{")
				}
				if _, err = s.options.Records.DB.Exec("INSERT INTO job_filings(key,json) VALUES(?,?)", fmt.Sprintf("owner/app#%d", n), string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			if err := s.FilePending(ctx); err == nil {
				t.Fatal("expected pending error")
			}
			job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 2)
			if err != nil || job == nil {
				t.Fatalf("healthy job=%v err=%v", job, err)
			}
			var count int
			if err = s.options.Records.DB.QueryRow("SELECT count(*) FROM job_filings WHERE key=?", "owner/app#1").Scan(&count); err != nil || count != 1 {
				t.Fatalf("failed pending count=%d err=%v", count, err)
			}
			if err = s.options.Records.DB.QueryRow("SELECT count(*) FROM job_filings WHERE key=?", "owner/app#2").Scan(&count); err != nil || count != 0 {
				t.Fatalf("healthy pending count=%d err=%v", count, err)
			}
		})
	}
}

func TestSubmittedIssueWithoutProjectStillFilesItsOwnJob(t *testing.T) {
	s, api := fixture(t)
	ctx := t.Context()
	api.issues = []map[string]any{{"number": int64(1), "state": "open", "title": "legacy"}}
	raw, err := json.Marshal(pending{Repo: "owner/app", Number: 1, DraftID: draftID, Job: &jobs.NewJob{Repo: "owner/app", Issue: 1, Title: "legacy", Scope: []string{"a.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.options.Records.DB.ExecContext(ctx, "INSERT INTO job_filings(key,json) VALUES(?,?)", "owner/app#1", string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["status"] = "submitted"
		d["issue"] = Record{"number": 1}
		return d, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, draftID, nil, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || job == nil {
		t.Fatalf("own job=%v err=%v", job, err)
	}
	var count int
	if err = s.options.Records.DB.QueryRowContext(ctx, "SELECT count(*) FROM job_filings WHERE key=?", "owner/app#1").Scan(&count); err != nil || count != 0 {
		t.Fatalf("pending=%d err=%v", count, err)
	}
}

func TestScopeEstimateDoesNotBlockFilingInAnotherRepository(t *testing.T) {
	t.Run("immediate", func(t *testing.T) { testScopeEstimateAcrossRepositories(t, false) })
	t.Run("periodic", func(t *testing.T) { testScopeEstimateAcrossRepositories(t, true) })
}

func testScopeEstimateAcrossRepositories(t *testing.T, periodic bool) {
	s, api := fixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	s.options.EstimateScope = func(ctx context.Context, _ string, _ map[string]any) (map[string]any, error) {
		close(started)
		select {
		case <-release:
			return Record{"known": true, "paths": []any{"estimated.go"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	api.issues = []map[string]any{{"number": int64(1), "state": "open"}, {"number": int64(2), "state": "open"}}
	for i, repo := range []string{"owner/app", "owner/other"} {
		number := int64(i + 1)
		var scope []string
		if i > 0 {
			scope = []string{"declared.go"}
		}
		raw, err := json.Marshal(pending{Repo: repo, Number: number, Job: &jobs.NewJob{Repo: repo, Issue: number, Title: "fixture", Scope: scope}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.options.Records.DB.ExecContext(ctx, "INSERT INTO job_filings(key,json) VALUES(?,?)", fmt.Sprintf("%s#%d", repo, number), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		if periodic {
			done <- s.FilePending(ctx)
		} else {
			done <- s.filePendingKey(ctx, "owner/app", "owner/app#1")
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("estimate did not start")
	}
	if !periodic {
		if err := s.filePendingKey(ctx, "owner/other", "owner/other#2"); err != nil {
			t.Fatal(err)
		}
	}
	other, err := s.options.Jobs.ByIssue(ctx, "owner/other", 2)
	close(release)
	if err != nil || other == nil {
		t.Fatalf("declared job was blocked: %v %v", other, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i, repo := range []string{"owner/app", "owner/other"} {
		job, err := s.options.Jobs.ByIssue(ctx, repo, int64(i+1))
		if err != nil || job == nil {
			t.Fatalf("repo=%s job=%v err=%v", repo, job, err)
		}
	}
}

func TestSplitFinalizationDoesNotEstimateUnrelatedFiling(t *testing.T) {
	s, api := fixture(t)
	ctx := t.Context()
	changeCorrectionRecord(t, s, draftID, func(d Record) { d["splitIssues"] = []any{Record{}, Record{}} })
	s.options.SplitPlan = func(Record) ([]Record, error) {
		return []Record{
			{"project": "owner/app", "title": "first", "body": "first", "changeScope": Record{"known": true, "paths": []any{"first.go"}}, "_dependsOn": []any{}},
			{"project": "owner/app", "title": "second", "body": "second", "changeScope": Record{"known": true, "paths": []any{"second.go"}}, "_dependsOn": []any{1}},
		}, nil
	}
	api.issues = []map[string]any{{"number": int64(99), "state": "open", "title": "unrelated"}}
	estimateCalls := 0
	s.options.EstimateScope = func(context.Context, string, map[string]any) (map[string]any, error) {
		estimateCalls++
		return nil, fmt.Errorf("unrelated estimate was started")
	}
	raw, err := json.Marshal(pending{Repo: "owner/other", Number: 99, Job: &jobs.NewJob{Title: "unrelated"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.options.Records.DB.ExecContext(ctx, "INSERT INTO job_filings(key,json) VALUES(?,?)", "owner/other#99", string(raw)); err != nil {
		t.Fatal(err)
	}
	result, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if err != nil || result["status"] != "submitted" || api.posts != 2 || estimateCalls != 0 {
		t.Fatalf("parent=%v err=%v posts=%d estimates=%d", result, err, api.posts, estimateCalls)
	}
	for n := int64(1); n <= 2; n++ {
		job, err := s.options.Jobs.ByIssue(ctx, "owner/app", n)
		if err != nil || job == nil {
			t.Fatalf("child %d was not dispatched: %v", n, err)
		}
	}
}
