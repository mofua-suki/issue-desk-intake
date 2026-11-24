package filing

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestContinuationScopeDecision(t *testing.T) {
	draft := draftFixture()
	draft["body"] = "## 目的\n続きの修正\n\n## 受入条件\n- go test\n\n<!-- issue-desk:continuation:1 -->"
	draft["changeScope"] = Record{"known": true, "paths": []any{"original.go"}}
	prepared, err := Prepare(context.Background(), configFixture(), draft)
	if err != nil {
		t.Fatal(err)
	}
	continuationBody := strings.Replace(prepared.Body, "&lt;!-- issue-desk:continuation:1 -->", "<!-- issue-desk:continuation:1 -->", 1)
	if continuationBody == prepared.Body {
		t.Fatal("続きのマーカーがありません")
	}
	for _, tc := range []struct {
		name, body string
		want       []string
		source     string
		calls      int
	}{
		{"declared", continuationBody, []string{"original.go"}, "declared", 0},
		{"undeclared", "## 目的\n続きの修正\n\n## 受入条件\n- go test\n\n<!-- issue-desk:continuation:1 -->", []string{"fix.go"}, "estimated", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api := fixture(t)
			ctx := context.Background()
			calls := 0
			api.issues = []map[string]any{{"number": int64(2), "title": "続き", "body": tc.body, "state": "open", "user": map[string]any{"id": 10}}}
			s.options.EstimateScope = func(_ context.Context, _ string, issue map[string]any) (map[string]any, error) {
				calls++
				if issue["body"] != tc.body {
					t.Fatal("見積もり本文が不一致")
				}
				return map[string]any{"known": true, "paths": []any{"fix.go"}}, nil
			}
			raw, err := json.Marshal(pending{Repo: "owner/app", Number: 2, DraftID: draftID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.options.Records.DB.ExecContext(ctx, bindSQL("INSERT INTO job_filings(key,json) VALUES(?,?)"), "owner/app#2", string(raw)); err != nil {
				t.Fatal(err)
			}
			if err = s.FilePending(ctx); err != nil {
				t.Fatal(err)
			}
			job, err := s.options.Jobs.ByIssue(ctx, "owner/app", 2)
			if err != nil || job == nil || !reflect.DeepEqual(job.Scope, tc.want) || job.ScopeSource != tc.source || calls != tc.calls {
				t.Fatalf("job=%+v calls=%d err=%v", job, calls, err)
			}
		})
	}
}
