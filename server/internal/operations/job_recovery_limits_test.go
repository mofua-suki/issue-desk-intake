package operations

import (
	"context"
	"errors"
	"testing"
)

func TestRecoveryRechecksLimitsChangedWhileJudgeWasRunningPostgres(t *testing.T) {
	for _, mode := range []string{"unchanged", "tightened", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			r := recoveryFixture(t)
			r.ReviewLimit = 2
			recoveryInsert(t, r, 1, "held", "review_held", "normal", "owner/repo", `{"reviewHolds":1}`, `{"summary":"confirmed review failure"}`, 0, nil, "1 hour")
			candidates, err := r.Candidates(t.Context())
			if err != nil || len(candidates) != 1 {
				t.Fatal(candidates, err)
			}
			judged, refreshed, sends := false, false, 0
			a := &RecoveryActions{Recovery: r,
				ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
					return map[string]any{"project": "owner/repo", "number": 1, "status": "review_held"}, nil
				},
				Judge: func(context.Context, []any) (map[string]any, error) {
					judged = true
					return map[string]any{"actions": []any{map[string]any{"item": "job_recovery:owner/repo#1", "action": "retry", "text": "修正済みの版で再検証してください。", "reason": "原因修正を確認しました。", "evidence": "保存済みの検証結果", "resolved": true, "requiresUser": false}}}, nil
				},
				RefreshLimits: func(context.Context) error {
					if !judged {
						t.Fatal("判断前の値だけを使いました")
					}
					refreshed = true
					if mode == "tightened" {
						r.ReviewLimit = 1
					}
					if mode == "unreadable" {
						return errors.New("現在の上限を読み取れません")
					}
					return nil
				},
				Send: func(context.Context, string, int64, string, string, string) error {
					sends++
					return nil
				},
			}
			result, err := a.Act(t.Context(), candidates[0])
			if !refreshed || mode == "unchanged" && (result != Acted || sends != 1 || err != nil) || mode != "unchanged" && (result != Skipped || sends != 0) || mode == "unreadable" && err == nil {
				t.Fatal(mode, result, refreshed, sends, err)
			}
		})
	}
}
