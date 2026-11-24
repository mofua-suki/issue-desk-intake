package proposals

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

func followupFilingFixture(t *testing.T, explicit bool) {
	t.Helper()
	ctx := context.Background()
	s, api, _ := adoptFixture(t)
	p := v3()
	p["items"] = []any{Object{"problem": "fix/target.go is broken", "expected": "fix/target.go works", "test": "go test ./fix"}}
	if !explicit {
		p["changeScope"] = nil
	} else {
		p["changeScope"] = Object{"paths": []any{"explicit.go"}}
	}
	api.comments[0] = proposalComment(p, 10)
	api.files = map[string][]Object{"/repos/owner/repo/pulls/2/files?limit=50&page=1": {{"filename": "original/pr.go"}}}
	api.before = func(path string) {
		if strings.Contains(path, "/files?") {
			t.Errorf("元PRファイルを取得: %s", path)
		}
	}
	env := pgtest.NewIsolated(t)
	schema := env.Schema + "_jobs"
	if _, err := env.DB.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = env.DB.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	u, err := url.Parse(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	jobDB, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobDB.Close() })
	if _, err := store.Migrate(ctx, jobDB); err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobDB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	estimates := 0
	filer, err := filing.New(ctx, filing.Options{Records: s.records, API: api, Jobs: queue, Project: s.o.Project, Own: func(Object) bool { return false }, Check: s.ValidateDraft, ValidateFollowup: s.ValidateDraft, ValidateNewPost: s.ValidateNewPost,
		Build: func(c context.Context, d consultations.Record) (filing.Prepared, error) {
			return s.PrepareFollowup(c, d, Object{"provider": "codex", "model": "fixture", "effort": "high"}, nil)
		},
		Link: func(repo string, n int64) string { return s.o.Link(repo, "issues", n) },
		EstimateScope: func(_ context.Context, _ string, issue map[string]any) (map[string]any, error) {
			estimates++
			body := str(issue["body"])
			for _, part := range []string{"fix/target.go is broken", "fix/target.go works", "go test ./fix"} {
				if !strings.Contains(body, part) {
					t.Errorf("見積もり本文に %q がない: %s", part, body)
				}
			}
			return map[string]any{"known": true, "paths": []any{"fix/target.go"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.o.Submit = func(c context.Context, id string, d consultations.Record) (consultations.Record, error) {
		return filer.SubmitAccepted(c, id, d, auth.Principal{Kind: auth.Screen})
	}
	mustScan(t, s)
	if api.posts != 1 {
		t.Fatalf("posts=%d draft=%v", api.posts, savedDraft(t, s))
	}
	job, err := queue.ByIssue(ctx, "owner/repo", 20)
	if err != nil || job == nil {
		t.Fatalf("job=%v err=%v", job, err)
	}
	want, source, calls := []string{"fix/target.go"}, "estimated", 1
	if explicit {
		want, source, calls = []string{"explicit.go"}, "declared", 0
	}
	if !reflect.DeepEqual(job.Scope, want) || job.ScopeSource != source || estimates != calls {
		t.Fatalf("scope=%v source=%s estimateCalls=%d", job.Scope, job.ScopeSource, estimates)
	}
}
func TestFollowupScopeEstimatedFromFindings(t *testing.T) { followupFilingFixture(t, false) }
func TestFollowupExplicitScopePreserved(t *testing.T)     { followupFilingFixture(t, true) }
