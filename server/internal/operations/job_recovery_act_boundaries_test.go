package operations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/usage"
)

func recoveryActFixture(t *testing.T) (*JobRecovery, RecoveryCandidate, *RecoveryActions) {
	t.Helper()
	s := &Service{DB: pgtest.New(t), PostgreSQL: postgres(t), Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
	_, err := s.PostgreSQL.Exec(`INSERT INTO jobs(repo,issue,title,status,result,updated_at) VALUES('owner/repo',1,'title','failed','{"error":"first failure"}'::jsonb,now()-interval '1 hour')`)
	if err != nil {
		t.Fatal(err)
	}
	r := &JobRecovery{Service: s, UsageLimit: 100}
	candidates, err := r.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates: %v %v", candidates, err)
	}
	a := &RecoveryActions{Recovery: r}
	a.ReadTarget = func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": 1, "title": "title", "body": "body", "status": "failed", "state": "open", "error": "first failure", "comments": []any{}}, nil
	}
	a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
		return recoveryDecision(items, "retry"), nil
	}
	a.Send = func(context.Context, string, int64, string, string, string) error { return nil }
	return r, candidates[0], a
}
func recoveryDecision(items []any, action string) map[string]any {
	item := items[0].(map[string]any)
	return map[string]any{"actions": []any{map[string]any{"action": action, "item": item["key"], "reason": "reason", "text": "instruction", "evidence": "evidence", "resolved": true, "requiresUser": false}}}
}
func recoveryStored(t *testing.T, r *JobRecovery, c RecoveryCandidate) RecoveryCause {
	t.Helper()
	rows, err := r.records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Repo == c.Job.Repo && row.Issue == c.Job.Issue {
			v, ok := row.Causes[c.Cause]
			if !ok {
				t.Fatalf("cause missing: %+v", row)
			}
			return v
		}
	}
	t.Fatal("record missing")
	return RecoveryCause{}
}

func TestRecoveryActJudgmentValidationPostgres(t *testing.T) {
	cases := []struct {
		name     string
		edit     func(map[string]any)
		response string
	}{
		{"none", func(m map[string]any) { m["action"] = "none" }, ""},
		{"empty", nil, "empty"}, {"missing actions", nil, "missing"}, {"bad actions", nil, "bad"}, {"multiple actions", nil, "multiple"},
		{"extra field", func(m map[string]any) { m["extra"] = true }, ""},
		{"missing field", func(m map[string]any) { delete(m, "resolved") }, ""},
		{"invalid action", func(m map[string]any) { m["action"] = "stop" }, ""},
		{"wrong target", func(m map[string]any) { m["item"] = "elsewhere" }, ""},
		{"invalid target type", func(m map[string]any) { m["item"] = []any{"bad"} }, ""},
		{"unconfirmed reference", func(m map[string]any) { m["text"] = "see #2" }, ""},
		{"other repository", func(m map[string]any) { m["text"] = "see other/repo#1" }, ""},
		{"requires user on retry", func(m map[string]any) { m["requiresUser"] = true }, ""},
		{"empty text", func(m map[string]any) { m["text"] = "" }, ""},
		{"oversized text", func(m map[string]any) { m["text"] = strings.Repeat("x", 20001) }, ""},
		{"invalid reason", func(m map[string]any) { m["reason"] = "" }, ""},
		{"reply while failed", func(m map[string]any) { m["action"] = "reply" }, "retry"},
		{"unresolved", func(m map[string]any) { m["resolved"] = false }, "retry"},
		{"missing evidence", func(m map[string]any) { m["evidence"] = "" }, "retry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c, a := recoveryActFixture(t)
			sends := 0
			a.Send = func(context.Context, string, int64, string, string, string) error { sends++; return nil }
			a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
				switch tc.response {
				case "empty":
					return map[string]any{"actions": []any{}}, nil
				case "error":
					return nil, errors.New("unreadable")
				case "missing":
					return map[string]any{}, nil
				case "bad":
					return map[string]any{"actions": "bad"}, nil
				case "multiple":
					x := recoveryDecision(items, "retry")
					return map[string]any{"actions": append(x["actions"].([]any), x["actions"].([]any)...)}, nil
				}
				result := recoveryDecision(items, "retry")
				tc.edit(result["actions"].([]any)[0].(map[string]any))
				return result, nil
			}
			got, err := a.Act(context.Background(), c)
			if err != nil || got != Skipped || sends != 0 {
				t.Fatalf("%s %d %v", got, sends, err)
			}
			record := recoveryStored(t, r, c)
			if record.State != "skipped" {
				t.Fatal(record)
			}
			if tc.response != "retry" && record.Action != "none" {
				t.Fatalf("not recorded as none: %+v", record)
			}
		})
	}
}

func TestRecoveryActRecheckPostgres(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"status", `UPDATE jobs SET status='running' WHERE id=$1`},
		{"generation", `UPDATE jobs SET generation=generation+1 WHERE id=$1`},
		{"cause", `UPDATE jobs SET result='{"error":"changed"}'::jsonb WHERE id=$1`},
		{"stop requested", `UPDATE jobs SET view='{"stopRequested":true}'::jsonb WHERE id=$1`},
		{"pending stop", `WITH j AS (UPDATE jobs SET lease_host='nas' WHERE id=$1 RETURNING repo,issue) INSERT INTO job_commands(id,repo,issue,kind,payload,target_host) SELECT '00000000-0000-4000-8000-000000000123',repo,issue,'stop','{}','nas' FROM j`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, a := recoveryActFixture(t)
			sends := 0
			a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
				if _, err := r.Service.PostgreSQL.Exec(tc.sql, c.Job.ID); err != nil {
					t.Fatal(err)
				}
				return recoveryDecision(items, "retry"), nil
			}
			a.Send = func(context.Context, string, int64, string, string, string) error { sends++; return nil }
			got, err := a.Act(context.Background(), c)
			if err != nil || got != Skipped || sends != 0 || recoveryStored(t, r, c).State != "skipped" {
				t.Fatal(got, sends, err)
			}
		})
	}
	t.Run("target already stopped", func(t *testing.T) {
		r, c, a := recoveryActFixture(t)
		judges := 0
		a.ReadTarget = func(context.Context, string, int64) (map[string]any, error) {
			return map[string]any{"project": "owner/repo", "number": 1, "stopped": true}, nil
		}
		a.Judge = func(context.Context, []any) (map[string]any, error) { judges++; return nil, nil }
		got, err := a.Act(context.Background(), c)
		if err != nil || got != Skipped || judges != 0 || recoveryStored(t, r, c).State != "skipped" {
			t.Fatal(got, judges, err)
		}
	})
}

func TestRecoveryActSendOutcomesPostgres(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sendErr error
		state   string
		result  RecoveryResult
	}{
		{"conflict", contract.NewProblem(409, "limit", ""), "send_rejected", SendRejected},
		{"forbidden", contract.NewProblem(403, "forbidden", ""), "send_rejected", SendRejected},
		{"unknown", errors.New("response lost"), "send_unknown", Skipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, a := recoveryActFixture(t)
			sends, judges := 0, 0
			oldJudge := a.Judge
			a.Judge = func(ctx context.Context, items []any) (map[string]any, error) { judges++; return oldJudge(ctx, items) }
			a.Send = func(_ context.Context, _ string, _ int64, id, _, _ string) error {
				sends++
				stored := recoveryStored(t, r, c)
				if stored.State != "reserved" || stored.ReservationID != id {
					t.Fatal(stored, id)
				}
				return tc.sendErr
			}
			got, err := a.Act(context.Background(), c)
			if got != tc.result || (tc.name == "unknown") != (err != nil) {
				t.Fatal(got, err)
			}
			if v := recoveryStored(t, r, c); v.State != tc.state || v.SendCount != 1 {
				t.Fatal(v)
			}
			a.Recovery = &JobRecovery{Service: r.Service}
			got, err = a.Act(context.Background(), c)
			if err != nil || got != Skipped || sends != 1 || judges != 1 {
				t.Fatal(got, sends, judges, err)
			}
		})
	}
}

func TestRecoveryActPreviousCauseAndLedgerCompatibilityPostgres(t *testing.T) {
	r, c, a := recoveryActFixture(t)
	ctx := context.Background()
	sends, judges := 0, 0
	a.Send = func(context.Context, string, int64, string, string, string) error { sends++; return nil }
	secret := "token=abcdefghijklmnopqrstuvwxyz123456"
	a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
		judges++
		result := recoveryDecision(items, "retry")
		m := result["actions"].([]any)[0].(map[string]any)
		m["reason"] = "reason " + secret
		m["evidence"] = "evidence " + secret
		return result, nil
	}
	got, err := a.Act(ctx, c)
	if err != nil || got != Acted {
		t.Fatal(got, err)
	}
	saved := recoveryStored(t, &JobRecovery{Service: r.Service}, c)
	if saved.Action != "retry" || saved.ActionText != "instruction" || saved.Reason != usage.Redact("reason "+secret) || saved.Evidence != usage.Redact("evidence "+secret) || strings.Contains(saved.Reason, secret) {
		t.Fatal(saved)
	}
	_, err = r.Service.PostgreSQL.Exec(`UPDATE jobs SET generation=generation+1 WHERE id=$1`, c.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Job.Generation++
	got, err = a.Act(ctx, c)
	if err != nil || got != Abandoned || sends != 1 || judges != 1 || recoveryStored(t, r, c).State != "abandoned" {
		t.Fatal(got, sends, judges, err)
	}
	_, err = r.Service.PostgreSQL.Exec(`UPDATE jobs SET result='{"error":"second failure"}'::jsonb, updated_at=now()-interval '1 hour' WHERE id=$1`, c.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Service.PostgreSQL.Exec(`INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES('recovery-usage',now(),'nas','owner/repo',1,'{"repo":"owner/repo","issue":1,"provider":"codex","inputTokens":1,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}'::jsonb,'test')`)
	if err != nil {
		t.Fatal(err)
	}
	next, err := r.Candidates(ctx)
	if err != nil || len(next) != 1 || next[0].Cause == c.Cause {
		t.Fatal(next, err)
	}
	a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
		judges++
		detail := items[0].(map[string]any)["detail"].(map[string]any)
		if detail["previousCause"] != c.Cause || detail["cause"] != next[0].Cause || detail["previousAction"] != "retry" || detail["previousActionText"] != "instruction" {
			t.Fatalf("detail: %v", detail)
		}
		return recoveryDecision(items, "retry"), nil
	}
	got, err = a.Act(ctx, next[0])
	if err != nil || got != Acted || sends != 2 || judges != 2 {
		t.Fatal(got, sends, judges, err)
	}
	row := recoveryStored(t, &JobRecovery{Service: r.Service}, next[0])
	if row.PreviousCause != c.Cause || row.CauseHash != next[0].Cause {
		t.Fatal(row)
	}
	// Load an old-format settings value without the new optional fields, then
	// rewrite it and reload it using the same production ledger path.
	legacy := `[{"repo":"legacy/repo","issue":7,"cause":"old","causes":{"old":{"causeHash":"old","generation":1,"state":"sent","reservationId":"old-id","at":"2025-01-01T00:00:00Z","sendCount":1}}}]`
	_, err = r.Service.PostgreSQL.Exec(`UPDATE settings SET value=$1::jsonb WHERE key='job-recovery'`, legacy)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.records(ctx)
	if err != nil || len(rows) != 1 || rows[0].Causes["old"].Action != "" {
		t.Fatal(rows, err)
	}
	if err = r.ledger(ctx, func(_ *[]RecoveryRecord) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rows, err = (&JobRecovery{Service: r.Service}).records(ctx)
	if err != nil || len(rows) != 1 || rows[0].Causes["old"].State != "sent" || rows[0].Causes["old"].ReservationID != "old-id" {
		t.Fatal(rows, err)
	}
	legacyCandidate := RecoveryCandidate{Job: jobs.Job{Repo: "legacy/repo", Issue: 7, Generation: 1}, Cause: "old"}
	decision, _, err := (&JobRecovery{Service: r.Service}).Reserve(ctx, legacyCandidate)
	if err != nil || decision != "already_reserved" {
		t.Fatalf("legacy duplicate: %s %v", decision, err)
	}

}

func TestRecoveryActJudgeItemAndReservationPostgres(t *testing.T) {
	r, c, a := recoveryActFixture(t)
	// The actual candidate result, as produced by Candidates, is included with
	// the issue body and comments and is recursively redacted for ops-watch.
	secret := "token=abcdefghijklmnopqrstuvwxyz123456"
	if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET result=jsonb_build_object('error',$1::text) WHERE id=$2`, "failure "+secret, c.Job.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err := r.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	c = candidates[0]
	oldRead := a.ReadTarget
	a.ReadTarget = func(ctx context.Context, repo string, n int64) (map[string]any, error) {
		d, e := oldRead(ctx, repo, n)
		d["body"] = "body " + secret
		d["comments"] = []any{map[string]any{"body": "comment " + secret}}
		return d, e
	}
	a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
		if len(items) != 1 {
			t.Fatal(items)
		}
		item := items[0].(map[string]any)
		detail := item["detail"].(map[string]any)
		if item["kind"] != "job_recovery" || item["project"] != "owner/repo" || item["number"] != int64(1) || item["summary"].(map[string]any)["title"] != "title" {
			t.Fatal(item)
		}
		if detail["status"] != "failed" || detail["cause"] != c.Cause || detail["body"] != usage.Redact("body "+secret) || detail["jobResult"].(map[string]any)["error"] != usage.Redact("failure "+secret) || detail["comments"].([]any)[0].(map[string]any)["body"] != usage.Redact("comment "+secret) {
			t.Fatal(detail)
		}
		result := recoveryDecision(items, "retry")
		result["actions"].([]any)[0].(map[string]any)["text"] = "対象 #1 を再試行"
		return result, nil
	}
	sends := 0
	a.Send = func(_ context.Context, repo string, n int64, id, action, text string) error {
		sends++
		saved := recoveryStored(t, r, c)
		if repo != "owner/repo" || n != 1 || action != "retry" || saved.State != "reserved" || saved.ReservationID != id || text != "対象 #1 を再試行" {
			t.Fatal(saved, repo, n, id, action, text)
		}
		return nil
	}
	got, err := a.Act(context.Background(), c)
	if err != nil || got != Acted || sends != 1 || recoveryStored(t, r, c).State != "sent" {
		t.Fatal(got, sends, err)
	}
}

func TestRecoveryActRetriesTransientFailuresPostgres(t *testing.T) {
	for _, failure := range []string{"judge", "first read", "second read"} {
		t.Run(failure, func(t *testing.T) {
			r, c, a := recoveryActFixture(t)
			judge, read := a.Judge, a.ReadTarget
			reads, judges, sends := 0, 0, 0
			a.ReadTarget = func(ctx context.Context, repo string, n int64) (map[string]any, error) {
				reads++
				if (failure == "first read" && reads == 1) || (failure == "second read" && reads == 2) {
					return nil, errors.New("temporary read")
				}
				return read(ctx, repo, n)
			}
			a.Judge = func(ctx context.Context, items []any) (map[string]any, error) {
				judges++
				if failure == "judge" && judges == 1 {
					return nil, errors.New("temporary judge")
				}
				return judge(ctx, items)
			}
			a.Send = func(context.Context, string, int64, string, string, string) error { sends++; return nil }
			got, err := a.Act(context.Background(), c)
			if got != Skipped || err == nil || sends != 0 || recoveryStored(t, r, c).State != "pre_send_error" {
				t.Fatal(got, err, sends)
			}
			got, err = a.Act(context.Background(), c)
			if got != Acted || err != nil || sends != 1 || recoveryStored(t, r, c).State != "sent" {
				t.Fatal(got, err, sends)
			}
		})
	}
}

func TestRecoveryActExpiresReservationPostgres(t *testing.T) {
	r, c, a := recoveryActFixture(t)
	_, id, err := r.Reserve(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	err = r.ledger(context.Background(), func(rows *[]RecoveryRecord) error {
		v := (*rows)[0].Causes[c.Cause]
		v.At = time.Now().Add(-2 * time.Hour)
		(*rows)[0].Causes[c.Cause] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Act(context.Background(), c)
	if err != nil || got != Skipped || recoveryStored(t, r, c).State != "send_unknown" {
		t.Fatal(got, err)
	}
	if err = r.Finish(context.Background(), c, id, "sent"); err == nil {
		t.Fatal("expired reservation finished")
	}
	other := c
	other.Cause = "different"
	decision, _, err := r.Reserve(context.Background(), other)
	if err != nil || decision != "proceed" {
		t.Fatal(decision, err)
	}
}
