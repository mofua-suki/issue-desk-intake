package issueapi

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/contract"
)

func usageHoldFixture(t *testing.T, tokens int, generation int, limit float64) *fixture {
	t.Helper()
	f := newFixture(t)
	f.options.RunLimit = 10
	f.options.UsageLimit = limit
	f.restart()
	if _, err := f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='question',generation=$2 WHERE id=$1`, f.job, generation); err != nil {
		t.Fatal(err)
	}
	if tokens >= 0 {
		row := fmt.Sprintf(`{"repo":"owner/repo","issue":1,"step":"実装","provider":"codex","inputTokens":%d,"cacheReadTokens":0,"outputTokens":0}`, tokens)
		if _, err := f.pg.Exec(`INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES($1,$2,$3,$4,$5,$6,$7)`, "usage-1", time.Now(), "worker", "owner/repo", 1, row, "worker"); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestUsageHoldDetailBoundariesPostgres(t *testing.T) {
	for _, tc := range []struct {
		name               string
		tokens, generation int
		limit              float64
		want               bool
	}{
		{"over", 101, 0, 100, true}, {"below", 10, 0, 100, false}, {"unknown", -1, 1, 100, false}, {"disabled", 101, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := usageHoldFixture(t, tc.tokens, tc.generation, tc.limit)
			detail, err := f.s.Detail(f.ctx, "owner/repo", 1, true, false)
			if err != nil {
				t.Fatal(err)
			}
			hasRetry := false
			for _, action := range detail["actions"].([]string) {
				if action == "retry" {
					hasRetry = true
				}
			}
			if detail["status"] != "waiting" || detail["retryable"] != tc.want || hasRetry != tc.want {
				t.Fatalf("detail: %#v", detail)
			}
		})
	}
}

func TestUsageHoldApprovalPostgres(t *testing.T) {
	for _, action := range []string{"retry", "reply"} {
		t.Run(action, func(t *testing.T) {
			f := usageHoldFixture(t, 101, 0, 100)
			input := Object{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "number": 1, "action": action, "text": "原因を確認して続けてください", "overLimitStatement": "共通番号1の使用量超過を確認して再試行を承認します", "instructionAt": "2026-11-01T08:58:00+09:00"}
			code, value := f.call("POST", "/api/comment", "delegate", input)
			if code != 200 {
				t.Fatalf("%d %#v", code, value)
			}
			job, err := f.options.Jobs.Get(f.ctx, f.job)
			if err != nil || job.Status != "queued" || len(fleet.Items(job.View["overLimitApprovals"])) != 1 || fleet.ObjectOrEmpty(fleet.Items(job.View["overLimitApprovals"])[0])["instructionAt"] != input["instructionAt"] || fleet.ObjectOrEmpty(job.View["reimplementExpected"])["usageAdmission"] != true {
				t.Fatalf("job: %#v %v", job, err)
			}
		})
	}
}

func TestUsageHoldRejectBeforeSideEffectsPostgres(t *testing.T) {
	f := usageHoldFixture(t, 101, 0, 100)
	input := Object{"id": "33333333-1111-4111-8111-111111111111", "project": "owner/repo", "number": 1, "action": "reply", "text": "続けてください"}
	code, value := f.call("POST", "/api/comment", "delegate", input)
	if code != 409 || !strings.Contains(fmt.Sprint(value), "使用量") {
		t.Fatalf("reply: %d %#v", code, value)
	}
	_, err := f.s.SendRecoveryAction(f.ctx, "owner/repo", 1, "44444444-1111-4111-8111-111111111111", "reply", "続けてください")
	p, ok := err.(*contract.Problem)
	if !ok || p.Status != 409 || !strings.Contains(err.Error(), "使用量") {
		t.Fatalf("recovery: %v", err)
	}
	if f.api.commentCalls != 0 {
		t.Fatalf("posted %d comments", f.api.commentCalls)
	}
}

func TestUsageHoldRetryActorsPostgres(t *testing.T) {
	for _, tc := range []struct {
		actor, status string
		code          int
	}{
		{"delegate", "held", 409}, {"screen", "held", 200}, {"screen", "failed", 200},
	} {
		t.Run(tc.actor+"_"+tc.status, func(t *testing.T) {
			f := usageHoldFixture(t, 101, 0, 100)
			if tc.status == "failed" {
				if _, err := f.pg.Exec(`UPDATE jobs SET status='failed',hold_reason=NULL WHERE id=$1`, f.job); err != nil {
					t.Fatal(err)
				}
			}
			input := Object{"id": "55555555-1111-4111-8111-111111111111", "project": "owner/repo", "number": 1, "action": "retry", "text": "続けてください"}
			code, value := f.call("POST", "/api/comment", tc.actor, input)
			if code != tc.code {
				t.Fatalf("%d %#v", code, value)
			}
			if code == 409 {
				if !strings.Contains(fmt.Sprint(value), "使用量") {
					t.Fatal(value)
				}
				return
			}
			job, err := f.options.Jobs.Get(f.ctx, f.job)
			if err != nil || job.Status != "queued" || fleet.ObjectOrEmpty(job.View["reimplementExpected"])["usageAdmission"] != true {
				t.Fatalf("job: %#v %v", job, err)
			}
		})
	}
}

func TestUsageHoldDetailSharedResultPostgres(t *testing.T) {
	f := usageHoldFixture(t, 101, 0, 100)
	// Count actual rows read by the production query in this isolated schema.
	_, err := f.pg.Exec(`ALTER TABLE usage_rows RENAME TO usage_rows_data;
		CREATE SEQUENCE usage_read_count;
		CREATE FUNCTION count_usage_read(value text) RETURNS text LANGUAGE plpgsql VOLATILE AS $$
		BEGIN PERFORM nextval('usage_read_count'); RETURN value; END $$;
		CREATE VIEW usage_rows AS SELECT key, at, repo, issue, count_usage_read(json) AS json FROM usage_rows_data`)
	if err != nil {
		t.Fatal(err)
	}
	input := Object{"id": "77777777-1111-4111-8111-111111111111", "project": "owner/repo", "number": 1, "action": "reply", "text": "続けてください"}
	code, value := f.call("POST", "/api/comment", "delegate", input)
	if code != 409 || !strings.Contains(fmt.Sprint(value), "使用量") {
		t.Fatalf("response: %d %v", code, value)
	}
	var reads int
	var called bool
	if err := f.pg.QueryRow(`SELECT last_value, is_called FROM usage_read_count`).Scan(&reads, &called); err != nil || !called || reads != 1 {
		t.Fatalf("usage row reads=%d called=%v err=%v", reads, called, err)
	}
}
