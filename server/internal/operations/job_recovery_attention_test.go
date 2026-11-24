package operations

import (
	"context"
	"testing"

	"issue-desk-intake/server/internal/fleet"
)

func TestRecoveryAttentionReflectsOnlyCurrentDecisionPostgres(t *testing.T) {
	for _, scenario := range []string{"reply validation", "repair instruction", "changed premise"} {
		t.Run(scenario, func(t *testing.T) {
			r := recoveryFixture(t)
			ctx := context.Background()
			status, action, shown := "failed", "repair", "failed"
			var hold any
			if scenario == "reply validation" {
				status, hold, action, shown = "held", "question", "reply", "waiting"
			} else if scenario == "changed premise" {
				action = "retry"
			}
			recoveryInsert(t, r, 1, status, hold, "normal", "owner/repo", `{}`, `{"error":"implementation broken","questions":["確認してください"]}`, 0, nil, "1 hour")
			candidates, err := r.Candidates(ctx)
			if err != nil || len(candidates) != 1 {
				t.Fatal(candidates, err)
			}
			reads, sends := 0, 0
			a := &RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
				reads++
				body := "現在の受入条件"
				if scenario == "changed premise" && reads > 1 {
					body = "変更された受入条件"
				}
				return map[string]any{"project": "owner/repo", "number": 1, "status": shown, "body": body}, nil
			}, Judge: func(context.Context, []any) (map[string]any, error) {
				return map[string]any{"actions": []any{map[string]any{"item": "job_recovery:owner/repo#1", "action": action, "reason": "原因を調べました", "text": "対処する", "evidence": "検証不能な根拠", "requiresUser": false, "resolved": true}}}, nil
			}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
			if result, err := a.Act(ctx, candidates[0]); err != nil || result != NeedsUser || sends != 0 {
				t.Fatal(result, sends, err)
			}
			restarted := &JobRecovery{Service: r.Service}
			saved, err := restarted.records(ctx)
			if err != nil || len(saved) != 1 {
				t.Fatal(saved, err)
			}
			cause := saved[0].Causes[candidates[0].Cause]
			if !cause.Handoff || cause.RequiresUser == nil || *cause.RequiresUser || cause.Action != "notify" || cause.Reason == "" {
				t.Fatal(cause)
			}
			rows, err := r.Service.jobRecoveryAttention(ctx, map[string]bool{"owner/repo": true})
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
			row := fleet.Object(rows[0])
			if row["handoff"] != true || row["requiresUser"] != false || row["reason"] != cause.Reason || row["recoveryState"] != "skipped" || row["causeKey"] == "" {
				t.Fatal(row)
			}
		})
	}
	r := recoveryFixture(t)
	ctx := context.Background()
	recoveryInsert(t, r, 1, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"implementation broken"}`, 0, nil, "1 hour")
	candidates, err := r.Candidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	c := candidates[0]
	a := &RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": float64(1), "status": "failed"}, nil
	}, Judge: func(context.Context, []any) (map[string]any, error) {
		return map[string]any{"actions": []any{map[string]any{"item": "job_recovery:owner/repo#1", "action": "notify", "reason": "元の条件に無い削除が必要です。", "text": "対象データの削除を判断してください。", "evidence": "失敗の記録", "requiresUser": true, "resolved": false}}}, nil
	}}
	if result, err := a.Act(ctx, c); err != nil || result != NeedsUser {
		t.Fatal(result, err)
	}
	read := func() []any {
		t.Helper()
		rows, err := r.Service.jobRecoveryAttention(ctx, map[string]bool{"owner/repo": true})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rows := read()
	if len(rows) != 1 || fleet.Object(rows[0])["reason"] != "元の条件に無い削除が必要です。" || fleet.Object(rows[0])["gist"] != "対象データの削除を判断してください。" || fleet.Object(rows[0])["requiresUser"] != true || fleet.Text(fleet.Object(rows[0])["causeKey"]) == "" {
		t.Fatal(rows)
	}
	// Reload through a new coordinator: the optional judgment is stored, not inferred from notify.
	restarted := &JobRecovery{Service: r.Service}
	saved, err := restarted.records(ctx)
	if err != nil || saved[0].Causes[c.Cause].RequiresUser == nil || !*saved[0].Causes[c.Cause].RequiresUser {
		t.Fatal(saved, err)
	}
	for _, value := range []*bool{new(bool), nil} {
		if err := restarted.ledger(ctx, func(records *[]RecoveryRecord) error {
			v := (*records)[0].Causes[c.Cause]
			v.RequiresUser = value
			(*records)[0].Causes[c.Cause] = v
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		row := fleet.Object(read()[0])
		if value == nil {
			if _, exists := row["requiresUser"]; exists {
				t.Fatal(row)
			}
		} else if row["requiresUser"] != false {
			t.Fatal(row)
		}
	}
	// A GET is a projection and must not create another reservation or send.
	before, err := r.records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = read()
	after, err := r.records(ctx)
	if err != nil || before[0].Causes[c.Cause].State != after[0].Causes[c.Cause].State || after[0].Causes[c.Cause].SendCount != 0 {
		t.Fatal(after, err)
	}
	recoveryExec(t, r, `UPDATE jobs SET result='{"error":"different cause"}'::jsonb WHERE issue=1`)
	if rows := read(); len(rows) != 0 {
		t.Fatal("古い判断を表示した", rows)
	}
	recoveryExec(t, r, `UPDATE jobs SET result='{"error":"implementation broken"}'::jsonb,generation=1 WHERE issue=1`)
	if rows := read(); len(rows) != 0 {
		t.Fatal("別世代の判断を表示した", rows)
	}
	recoveryExec(t, r, `UPDATE jobs SET generation=0,status='merged' WHERE issue=1`)
	if rows := read(); len(rows) != 0 {
		t.Fatal("完了した依頼の判断を表示した", rows)
	}
}
