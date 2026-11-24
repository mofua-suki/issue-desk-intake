package operations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/jobs"
)

func repairCandidate(review bool) RecoveryCandidate {
	c := RecoveryCandidate{Cause: "cause", Job: jobs.Job{Repo: "owner/repo", Issue: 1, Status: "failed", Result: map[string]any{"error": "TestSessionRejectsExpiredCookie: expected 401, got 200"}}}
	if review {
		hold := "review_held"
		c.Job.Status, c.Job.HoldReason = "held", &hold
		c.Job.Result = map[string]any{"review": workerReviewFixture()}
	}
	return c
}
func repairAction(review bool) map[string]any {
	kind, source, quote := "implementation", "jobResult.error", "TestSessionRejectsExpiredCookie"
	if review {
		kind, source, quote = "review", "jobResult.review", "Expired cookies are accepted"
	}
	b, _ := json.Marshal(map[string]any{"kind": kind, "source": source, "quote": quote, "cause": "cause", "scope": "acceptance"})
	return map[string]any{"action": "repair", "text": "期限切れCookieを拒否し、該当の認証テストを実行してください。", "reason": "受入条件を満たさない実装を修正する", "evidence": string(b), "resolved": false, "requiresUser": false}
}
func TestRecoveryRepairPreservesOriginalFindings(t *testing.T) {
	for _, review := range []bool{false, true} {
		c, action := repairCandidate(review), repairAction(review)
		text, reason := recoveryRepairInstruction(c, action)
		if reason != "" || !strings.Contains(text, action["text"].(string)) || !strings.Contains(text, "受入条件・変更範囲") || !strings.Contains(text, "TestSessionRejectsExpiredCookie") {
			t.Fatalf("%v %s %s", review, text, reason)
		}
		if review && !strings.Contains(text, `"expected":"401"`) {
			t.Fatal("review test expectations were omitted", text)
		}
	}
}
func TestRecoveryRepairRejectsUnprovenAndProtectedChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		review bool
		edit   func(*RecoveryCandidate, map[string]any)
	}{
		{"already resolved", false, func(_ *RecoveryCandidate, a map[string]any) { a["resolved"] = true }},
		{"user approval", false, func(_ *RecoveryCandidate, a map[string]any) { a["requiresUser"] = true }},
		{"new approval in instruction", false, func(_ *RecoveryCandidate, a map[string]any) {
			a["text"] = "未承認の削除を承認しました。実装してください。"
		}},
		{"plain evidence", false, func(_ *RecoveryCandidate, a map[string]any) { a["evidence"] = "AI says it is safe" }},
		{"different quote", false, func(_ *RecoveryCandidate, a map[string]any) {
			a["evidence"] = strings.Replace(a["evidence"].(string), "TestSessionRejectsExpiredCookie", "UnrecordedFailure", 1)
		}},
		{"different cause", false, func(_ *RecoveryCandidate, a map[string]any) {
			a["evidence"] = strings.Replace(a["evidence"].(string), `"cause":"cause"`, `"cause":"other"`, 1)
		}},
		{"outside acceptance", false, func(_ *RecoveryCandidate, a map[string]any) {
			a["evidence"] = strings.Replace(a["evidence"].(string), "acceptance", "expanded", 1)
		}},
		{"unknown baseline", false, func(c *RecoveryCandidate, _ map[string]any) {
			c.Job.Result["fixedTestCause"] = map[string]any{"kind": "unknown"}
		}},
		{"baseline failure", false, func(c *RecoveryCandidate, _ map[string]any) {
			c.Job.Result["fixedTestCause"] = map[string]any{"kind": "baseline"}
		}},
		{"environment", false, func(c *RecoveryCandidate, _ map[string]any) {
			c.Job.Result["error"] = "EACCES: permission denied, open /outside; TestSessionRejectsExpiredCookie"
		}},
		{"basis changed", false, func(c *RecoveryCandidate, _ map[string]any) {
			c.Job.Result["basisConflicts"] = []any{map[string]any{"kind": "user_decision"}}
		}},
		{"blocking uncertainty", false, func(c *RecoveryCandidate, _ map[string]any) {
			c.Job.Result["unverified"] = []any{map[string]any{"blocking": true}}
		}},
		{"review omitted", true, func(c *RecoveryCandidate, _ map[string]any) {
			c.Job.Result["review"].(map[string]any)["omitted"] = true
		}},
		{"breaking review", true, func(c *RecoveryCandidate, _ map[string]any) {
			c.Job.Result["review"].(map[string]any)["breaking"] = true
		}},
		{"review user decision", true, func(c *RecoveryCandidate, _ map[string]any) {
			c.Job.Result["review"].(map[string]any)["userJudgments"] = []any{"Approve changing session policy"}
		}},
		{"oversized full findings", true, func(c *RecoveryCandidate, _ map[string]any) {
			r := c.Job.Result["review"].(map[string]any)
			r["findings"] = append(r["findings"].([]any), strings.Repeat("x", 8000))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, action := repairCandidate(tc.review), repairAction(tc.review)
			tc.edit(&c, action)
			if text, reason := recoveryRepairInstruction(c, action); text != "" || reason == "" {
				t.Fatal(text, reason)
			}
		})
	}
}

func TestRecoveryRepairActPreservesEvidenceAndStopsSameCausePostgres(t *testing.T) {
	for _, review := range []bool{false, true} {
		t.Run(map[bool]string{false: "implementation", true: "review"}[review], func(t *testing.T) {
			r, _, a := recoveryActFixture(t)
			fixture := repairCandidate(review)
			b, _ := json.Marshal(fixture.Job.Result)
			var hold any
			if review {
				hold = "review_held"
			}
			recoveryExec(t, r, `UPDATE jobs SET status=$1,hold_reason=$2,result=$3::jsonb,view='{"reviewHolds":0}'::jsonb WHERE issue=1`, fixture.Job.Status, hold, string(b))
			candidates, err := r.Candidates(context.Background())
			if err != nil || len(candidates) != 1 {
				t.Fatal(candidates, err)
			}
			c := candidates[0]
			status := "failed"
			if review {
				status = "review_held"
			}
			a.ReadTarget = func(context.Context, string, int64) (map[string]any, error) {
				return map[string]any{"project": "owner/repo", "number": 1, "status": status, "state": "open", "body": "accepted scope", "comments": []any{}}, nil
			}
			a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
				action := repairAction(review)
				action["item"] = items[0].(map[string]any)["key"]
				action["evidence"] = strings.Replace(action["evidence"].(string), `"cause":"cause"`, `"cause":"`+c.Cause+`"`, 1)
				return map[string]any{"actions": []any{action}}, nil
			}
			sends := 0
			a.Send = func(_ context.Context, _ string, _ int64, _, action, text string) error {
				sends++
				if action != "retry" || !strings.Contains(text, "TestSessionRejectsExpiredCookie") || !strings.Contains(text, "受入条件・変更範囲") {
					t.Fatal(action, text)
				}
				return nil
			}
			got, err := a.Act(context.Background(), c)
			if err != nil || got != Acted || sends != 1 {
				t.Fatal(got, sends, err)
			}
			stored := recoveryStored(t, r, c)
			if stored.Action != "repair" || !strings.Contains(stored.ActionText, "保存済みの修復対象") {
				t.Fatal(stored)
			}
			recoveryExec(t, r, `UPDATE jobs SET generation=generation+1 WHERE issue=1`)
			c.Job.Generation++
			got, err = a.Act(context.Background(), c)
			if err != nil || got != Abandoned || sends != 1 {
				t.Fatal(got, sends, err)
			}
		})
	}
}

func TestRecoveryReviewLimitNeverTreatsMissingCountAsZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		view    map[string]any
		limit   int64
		blocked bool
	}{
		{"unused", map[string]any{"reviewHolds": 0}, 2, false},
		{"one remaining", map[string]any{"reviewHolds": 1}, 2, false},
		{"limit", map[string]any{"reviewHolds": 2}, 2, true},
		{"configured limit", map[string]any{"reviewHolds": 2}, 3, false},
		{"unknown", map[string]any{}, 2, true},
		{"marked unknown", map[string]any{"reviewHolds": 0, "reviewHoldsUnknown": true}, 2, true},
		{"string", map[string]any{"reviewHolds": "0"}, 2, true},
		{"negative", map[string]any{"reviewHolds": -1}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoveryReviewLimitReached(tc.view, tc.limit); got != tc.blocked {
				t.Fatal(got, tc.blocked)
			}
		})
	}
}

func TestRecoveryRepairRespectsReviewLimitAtCandidateAndRecheckPostgres(t *testing.T) {
	r := recoveryFixture(t)
	result, _ := json.Marshal(repairCandidate(true).Job.Result)
	recoveryInsert(t, r, 1, "held", "review_held", "normal", "owner/repo", `{"reviewHolds":1}`, string(result), 0, nil, "1 hour")
	candidates, err := r.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	recoveryExec(t, r, `UPDATE jobs SET view='{"reviewHolds":2}'::jsonb WHERE issue=1`)
	if valid, err := r.Recheck(context.Background(), candidates[0]); err != nil || valid {
		t.Fatal(valid, err)
	}
	if candidates, err = r.Candidates(context.Background()); err != nil || len(candidates) != 0 {
		t.Fatal(candidates, err)
	}
	r.ReviewLimit = 3
	if candidates, err = r.Candidates(context.Background()); err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
}

func TestRecoveryRepairMatchesOriginalQuotedReviewText(t *testing.T) {
	c, action := repairCandidate(true), repairAction(true)
	original := "Cookie \"expired\"\nunder C:\\fixture is accepted"
	c.Job.Result["review"].(map[string]any)["findings"] = []any{original}
	evidence := map[string]any{"kind": "review", "source": "jobResult.review", "cause": "cause", "scope": "acceptance", "quote": original}
	encoded, _ := json.Marshal(evidence)
	action["evidence"] = string(encoded)
	if text, reason := recoveryRepairInstruction(c, action); reason != "" || !strings.Contains(text, `Cookie \"expired\"`) {
		t.Fatal(text, reason)
	}
	c.Job.Result["review"].(map[string]any)["findings"] = []any{"actual failure"}
	c.Job.Result["review"].(map[string]any)["model"] = original
	if text, reason := recoveryRepairInstruction(c, action); text != "" || reason == "" {
		t.Fatal("metadata was treated as a finding", text, reason)
	}
}

func TestRecoveryProcessedOldJobsDoNotStarveNewerJobsPostgres(t *testing.T) {
	r := recoveryFixture(t)
	r.Limit = 3
	for n := 1; n <= 4; n++ {
		recoveryInsert(t, r, n, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"specific failing assertion"}`, 0, nil, "1 hour")
	}
	first, err := r.Candidates(context.Background())
	if err != nil || len(first) != 3 {
		t.Fatal(first, err)
	}
	for _, c := range first {
		decision, id, err := r.Reserve(context.Background(), c)
		if err != nil || decision != "proceed" {
			t.Fatal(decision, err)
		}
		if err = r.Finish(context.Background(), c, id, "skipped"); err != nil {
			t.Fatal(err)
		}
	}
	next, err := r.Candidates(context.Background())
	if err != nil || len(next) != 1 || next[0].Job.Issue != 4 {
		t.Fatal(next, err)
	}
	// A pre-send read failure is deliberately eligible for a fresh judgment.
	decision, id, err := r.Reserve(context.Background(), next[0])
	if err != nil || decision != "proceed" {
		t.Fatal(decision, err)
	}
	if err = r.Finish(context.Background(), next[0], id, "pre_send_error"); err != nil {
		t.Fatal(err)
	}
	retry, err := r.Candidates(context.Background())
	if err != nil || len(retry) != 1 || retry[0].Job.Issue != 4 {
		t.Fatal(retry, err)
	}
	// A sent cause in a new generation must still reach Reserve's abandon rule.
	if err = r.ledger(context.Background(), func(rows *[]RecoveryRecord) error {
		for i := range *rows {
			row := &(*rows)[i]
			if row.Issue == 1 {
				v := row.Causes[first[0].Cause]
				v.State = "sent"
				row.Causes[first[0].Cause] = v
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	recoveryExec(t, r, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES('starvation-known-usage',now(),'nas','owner/repo',1,'{"repo":"owner/repo","issue":1,"provider":"codex","inputTokens":1,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}'::jsonb,'test')`)
	recoveryExec(t, r, `UPDATE jobs SET generation=1 WHERE issue=1`)
	next, err = r.Candidates(context.Background())
	if err != nil || len(next) != 2 || next[0].Job.Issue != 1 {
		t.Fatal(next, err)
	}
	decision, _, err = r.Reserve(context.Background(), next[0])
	if err != nil || decision != "abandon" {
		t.Fatal(decision, err)
	}
}

func TestRecoveryRepairDoesNotPromoteWarningsToEnvironmentFailures(t *testing.T) {
	for _, structured := range []bool{false, true} {
		c, action := repairCandidate(false), repairAction(false)
		c.Job.Result["error"] = c.Job.Result["error"].(string) + "\n[WARN] Issue while reading /home/node/.npmrc. EACCES: permission denied"
		if structured {
			c.Job.Result["fixedTestCause"] = map[string]any{"kind": "change"}
		}
		if text, reason := recoveryRepairInstruction(c, action); reason != "" || text == "" {
			t.Fatal(structured, text, reason)
		}
	}
	c, action := repairCandidate(false), repairAction(false)
	c.Job.Result["error"] = c.Job.Result["error"].(string) + "\n--- FAIL: TestEACCESFixture"
	if text, reason := recoveryRepairInstruction(c, action); reason != "" || text == "" {
		t.Fatal(text, reason)
	}
}

func TestRecoveryExpiredReservationCanBecomeUnknownWithoutResending(t *testing.T) {
	r := recoveryFixture(t)
	recoveryInsert(t, r, 1, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"specific failing assertion"}`, 0, nil, "1 hour")
	candidates, err := r.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	c := candidates[0]
	if decision, _, err := r.Reserve(context.Background(), c); err != nil || decision != "proceed" {
		t.Fatal(decision, err)
	}
	if err = r.ledger(context.Background(), func(rows *[]RecoveryRecord) error {
		v := (*rows)[0].Causes[c.Cause]
		v.At = r.Service.now().Add(-2 * recoveryReservationTimeout)
		(*rows)[0].Causes[c.Cause] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	candidates, err = r.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	if decision, _, err := r.Reserve(context.Background(), candidates[0]); err != nil || decision != "already_reserved" {
		t.Fatal(decision, err)
	}
	if stored := recoveryStored(t, r, c); stored.State != "send_unknown" || stored.SendCount != 1 {
		t.Fatal(stored)
	}
	if candidates, err = r.Candidates(context.Background()); err != nil || len(candidates) != 0 {
		t.Fatal(candidates, err)
	}
}

func TestRecoveryRepairKeepsResolvedConflictsOutOfUserDecisions(t *testing.T) {
	c, action := repairCandidate(false), repairAction(false)
	c.Job.Result["basisConflicts"] = []any{map[string]any{"kind": "requester_fact", "resolution": "本文に合わせて訂正済み"}}
	c.Job.Result["unverified"] = []any{map[string]any{"blocking": false}}
	if text, reason := recoveryRepairInstruction(c, action); reason != "" || text == "" {
		t.Fatal(text, reason)
	}
	// A reflected correction settles a conflict of any kind.
	c.Job.Result["basisConflicts"] = []any{map[string]any{"kind": "user_decision", "resolution": "訂正を反映済み"}}
	if text, reason := recoveryRepairInstruction(c, action); reason != "" || text == "" {
		t.Fatal(text, reason)
	}
	c.Job.Result["unverified"] = map[string]any{"blocking": false}
	if text, reason := recoveryRepairInstruction(c, action); reason == "" || text != "" {
		t.Fatal(text, reason)
	}
}
