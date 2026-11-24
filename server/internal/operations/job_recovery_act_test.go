package operations

import (
	"context"
	"issue-desk-intake/server/internal/pgtest"
	"testing"
)

func TestRecoveryActPostgres(t *testing.T) {
	for _, tc := range []struct {
		name, status, hold, action string
		changed                    bool
		want                       RecoveryResult
		sends                      int
	}{
		{"failed", "failed", "", "retry", false, Acted, 1},
		{"review", "held", "review_held", "retry", false, Acted, 1},
		{"question", "held", "question", "reply", false, Acted, 1},
		{"new comment", "held", "question", "reply", true, Skipped, 0},
		{"notify", "failed", "", "notify", false, NeedsUser, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{DB: pgtest.New(t), PostgreSQL: postgres(t), Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
			var hold any
			if tc.hold != "" {
				hold = tc.hold
			}
			_, err := s.PostgreSQL.Exec(`INSERT INTO jobs(repo,issue,status,hold_reason,view,result,updated_at) VALUES('owner/repo',1,$1,$2,'{"reviewHolds":0}'::jsonb,'{"error":"failure","questions":["why"],"summary":"review"}'::jsonb,now()-interval '1 hour')`, tc.status, hold)
			if err != nil {
				t.Fatal(err)
			}
			r := &JobRecovery{Service: s, UsageLimit: 100}
			candidates, err := r.Candidates(context.Background())
			if err != nil || len(candidates) != 1 {
				t.Fatal(candidates, err)
			}
			calls, sends := 0, 0
			a := &RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
				calls++
				status := tc.status
				if tc.hold == "question" {
					status = "waiting"
				} else if tc.hold != "" {
					status = tc.hold
				}
				comments := []any{}
				if tc.changed && calls > 1 {
					comments = append(comments, "new")
				}
				return map[string]any{"project": "owner/repo", "number": 1, "status": status, "body": "既存のJSON契約を維持する。", "comments": comments}, nil
			}, Judge: func(_ context.Context, items []any) (map[string]any, error) {
				item := items[0].(map[string]any)
				evidence := "evidence"
				if tc.action == "reply" {
					evidence = questionEvidence("fact", "body", "既存のJSON契約を維持する。")
				}
				return map[string]any{"actions": []any{map[string]any{"item": item["key"], "action": tc.action, "reason": "reason", "text": "instruction", "evidence": evidence, "resolved": true, "requiresUser": tc.action == "notify"}}}, nil
			}}
			a.Send = func(_ context.Context, repo string, n int64, id, action, text string) error { sends++; return nil }
			got, err := a.Act(context.Background(), candidates[0])
			if err != nil || got != tc.want || sends != tc.sends {
				t.Fatal(got, sends, err)
			}
			a.Recovery = &JobRecovery{Service: s, UsageLimit: 100}
			_, err = a.Act(context.Background(), candidates[0])
			wantSends := tc.sends
			if tc.changed {
				wantSends++ // Stable new observations are judged on the next cycle.
			}
			if err != nil || sends != wantSends {
				t.Fatal(sends, err)
			}
		})
	}
}
