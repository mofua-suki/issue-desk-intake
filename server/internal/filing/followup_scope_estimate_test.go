package filing

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/jobs"
)

func TestFollowupScopeEstimateFallbackAndBackfill(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		result                        map[string]any
		failure, own, backfillFailure bool
		want                          []string
		backfilled                    bool
	}{
		{name: "estimated and backfilled", result: map[string]any{"known": true, "paths": []any{"fix.go"}}, own: true, want: []string{"fix.go"}, backfilled: true},
		{name: "cannot backfill", result: map[string]any{"known": true, "paths": []any{"fix.go"}}, own: true, backfillFailure: true, want: []string{"fix.go"}, backfilled: true},
		{name: "other author", result: map[string]any{"known": true, "paths": []any{"fix.go"}}, want: []string{"fix.go"}},
		{name: "unknown", result: map[string]any{"known": false}, want: []string{"*"}},
		{name: "failure", failure: true, want: []string{"*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api := fixture(t)
			ctx := context.Background()
			calls := 0
			s.options.Build = func(context.Context, Record) (Prepared, error) {
				return Prepared{Title: "修正", Body: "指摘 fix.go\n期待: 修正\n検証: go test\n\n---\n受付: 開発受付 / owner/app", Job: jobs.NewJob{ScopeSource: "estimated"}}, nil
			}
			s.options.EstimateScope = func(_ context.Context, _ string, issue map[string]any) (map[string]any, error) {
				if !strings.Contains(fleet.Text(issue["body"]), "指摘 fix.go") {
					t.Fatal(issue)
				}
				if tc.failure {
					return nil, errors.New("failure")
				}
				return tc.result, nil
			}
			s.options.Own = func(map[string]any) bool { return tc.own }
			s.options.BackfillScope = func(_ context.Context, _ string, _ map[string]any, body string) error {
				calls++
				if !strings.Contains(body, "fix.go") {
					t.Fatal(body)
				}
				if tc.backfillFailure {
					return errors.New("failure")
				}
				api.mu.Lock()
				api.issues[0]["body"] = body
				api.mu.Unlock()
				return nil
			}
			if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen}); err != nil {
				t.Fatal(err)
			}
			job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
			if err != nil || job == nil || !reflect.DeepEqual(job.Scope, tc.want) || job.ScopeSource != "estimated" {
				t.Fatalf("job=%+v err=%v", job, err)
			}
			issue, err := api.Issue(ctx, "owner/app", 1)
			if err != nil {
				t.Fatal(err)
			}
			parsed := ParseScope(fleet.Text(issue["body"]))
			if (parsed.State == "declared" && parsed.Backfilled) != (tc.backfilled && !tc.backfillFailure) {
				t.Fatalf("補記結果=%+v body=%s", parsed, fleet.Text(issue["body"]))
			}
			if (calls > 0) != tc.backfilled {
				t.Fatalf("backfill calls=%d", calls)
			}
		})
	}
	t.Run("interrupted and resumed", func(t *testing.T) {
		s, api := fixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		s.options.Build = func(context.Context, Record) (Prepared, error) {
			return Prepared{Title: "修正", Body: "指摘 fix.go\n\n---\n受付: 開発受付 / owner/app", Job: jobs.NewJob{ScopeSource: "estimated"}}, nil
		}
		calls := 0
		s.options.EstimateScope = func(context.Context, string, map[string]any) (map[string]any, error) {
			calls++
			cancel()
			return nil, context.Canceled
		}
		_, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("中断 err=%v", err)
		}
		if job, e := s.options.Jobs.ByIssue(context.Background(), "owner/app", 1); e != nil || job != nil {
			t.Fatalf("中断後 job=%v err=%v", job, e)
		}
		if err = s.FilePending(context.Background()); err != nil {
			t.Fatal(err)
		}
		job, err := s.options.Jobs.ByIssue(context.Background(), "owner/app", 1)
		if err != nil || job == nil || !reflect.DeepEqual(job.Scope, []string{"*"}) || job.ScopeSource != "estimated" || calls != 1 || api.posts != 1 {
			t.Fatalf("再開 job=%+v err=%v calls=%d posts=%d", job, err, calls, api.posts)
		}
	})
}
