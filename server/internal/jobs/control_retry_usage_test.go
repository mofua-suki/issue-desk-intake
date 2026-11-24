package jobs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func TestControlRetryUsageHeldPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	opts := ControlOptions{RunLimit: 10, UsageLimit: 100}
	id := seedJob(t, ctx, db, "owner/repo", "worker", 743, 7)
	row := `{"repo":"owner/repo","issue":743,"step":"実装","provider":"codex","inputTokens":101,"cacheReadTokens":0,"outputTokens":0}`
	if _, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES($1,$2,$3,$4,$5,$6,$7)`, "usage-743", now, "worker", "owner/repo", 743, row, "worker"); err != nil {
		t.Fatal(err)
	}
	result, err := s.CheckAutomaticUsage(ctx, id, "worker", 7, map[string]any{"rows": []any{}, "skipped": float64(0)}, 100, nil)
	reason := fmt.Sprint(fleet.Object(result)["usageLimitReason"])
	if err != nil || !strings.Contains(reason, "上限") || strings.HasPrefix(reason, "累計使用量が未知です") {
		t.Fatalf("initial usage: %v %v", result, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='question' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	job, err := readJob(ctx, db, id, false)
	if err != nil || !s.UsageLimitHeld(ctx, job, opts) || RunLimitHeld(job, opts.RunLimit) {
		t.Fatalf("held: %+v %v", job, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if s.UsageLimitHeld(canceled, job, opts) {
		t.Fatal("unreadable usage reported as over limit")
	}
	instruction := ControlInput{ID: "11111111-1111-4111-8111-111111111111", Action: "instruction", By: "token:worker", Text: "続けてください"}
	_, err = s.Control(ctx, id, instruction, opts)
	code(t, err, 409, "")
	if !strings.Contains(err.Error(), "承認付き再試行") || !strings.Contains(err.Error(), "上限") {
		t.Fatal(err)
	}
	retry := ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "token:worker"}
	_, err = s.Control(ctx, id, retry, opts)
	code(t, err, 409, "")
	retry.OverLimitStatement = "共通番号743の使用量超過を確認して再試行を承認します"
	bad := retry
	bad.By = "issue-screen"
	_, err = s.Control(ctx, id, bad, opts)
	code(t, err, 400, "")
	bad = retry
	bad.OverLimitStatement = "承認します"
	_, err = s.Control(ctx, id, bad, opts)
	code(t, err, 400, "")
	bad = retry
	bad.OverLimitStatement = "共通番号999の使用量超過を確認して再試行を承認します"
	_, err = s.Control(ctx, id, bad, opts)
	code(t, err, 400, "")
	job, err = s.Control(ctx, id, retry, opts)
	if err != nil || job.Status != "queued" || len(fleet.List(job.View["overLimitApprovals"])) != 1 || fleet.Object(job.View["reimplementExpected"])["usageAdmission"] != true {
		t.Fatalf("retry: %+v %v", job, err)
	}
	job, err = s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	approval := fleet.Object(fleet.List(job.View["overLimitApprovals"])[0])
	if approval["kind"] != "usage" || approval["usageLimit"] != float64(100) || approval["usageReason"] != reason || approval["limit"] != float64(10) {
		t.Fatalf("persisted approval: %#v; reason: %q", approval, reason)
	}
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='running' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	result, err = s.CheckAutomaticUsage(ctx, id, "worker", 7, map[string]any{"admissionId": "33333333-3333-4333-8333-333333333333"}, 100, nil)
	if err != nil || fleet.Object(result)["usageLimitReason"] != nil {
		t.Fatalf("admission: %v %v", result, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='question' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	retry.ID = "44444444-4444-4444-8444-444444444444"
	_, err = s.Control(ctx, id, retry, opts)
	code(t, err, 409, "")
	if !strings.Contains(err.Error(), "使用済み") {
		t.Fatal(err)
	}
}

func TestLimitHeldQuestionState(t *testing.T) {
	question, other := "question", "review_held"
	for _, tc := range []struct {
		name string
		job  *Job
		want bool
	}{
		{"nil", nil, false}, {"no reason", &Job{Status: "held"}, false},
		{"other reason", &Job{Status: "held", HoldReason: &other}, false},
		{"other status", &Job{Status: "failed", HoldReason: &question}, false},
		{"question", &Job{Status: "held", HoldReason: &question}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if questionHeld(tc.job) != tc.want || usageLimitHeld(tc.job, "上限超過") != tc.want {
				t.Fatal("question hold mismatch")
			}
			if usageLimitHeld(tc.job, "累計使用量が未知です（上限不明）") {
				t.Fatal("unknown usage is not an overage")
			}
		})
	}
}

func TestControlUsageQuestionBoundariesPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	s, _ := New(db, Options{Now: func() time.Time { return now }})
	for i, tc := range []struct {
		name, row  string
		limit      float64
		generation int
	}{
		{"below", `{"repo":"owner/repo","issue":1,"step":"実装","provider":"codex","inputTokens":10,"cacheReadTokens":0,"outputTokens":0}`, 100, 0},
		{"unknown", "", 100, 1},
		{"disabled", `{"repo":"owner/repo","issue":1,"step":"実装","provider":"codex","inputTokens":101,"cacheReadTokens":0,"outputTokens":0}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "owner/" + tc.name
			id := seedJob(t, ctx, db, repo, "worker", 1, 7)
			if tc.row != "" {
				row := strings.Replace(tc.row, "owner/repo", repo, 1)
				if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES($1,$2,$3,$4,$5,$6,$7)`, tc.name, now, "worker", repo, 1, row, "worker"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='question',generation=$2 WHERE id=$1`, id, tc.generation); err != nil {
				t.Fatal(err)
			}
			opts := ControlOptions{RunLimit: 10, UsageLimit: tc.limit}
			job, err := readJob(ctx, db, id, false)
			if err != nil || s.UsageLimitHeld(ctx, job, opts) {
				t.Fatalf("unexpected hold: %v %v", job, err)
			}
			retry := ControlInput{ID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", i+10), Action: "retry", By: "token:worker", OverLimitStatement: "共通番号1の使用量超過を確認して再試行を承認します"}
			job, err = s.Control(ctx, id, retry, opts)
			if err == nil && len(fleet.List(job.View["overLimitApprovals"])) != 0 {
				t.Fatal("unexpected approval", job.View)
			}
			if err != nil {
				code(t, err, 409, "")
			}
			instruction := ControlInput{ID: fmt.Sprintf("%08d-2222-4222-8222-222222222222", i+10), Action: "instruction", By: "token:worker", Text: "続けてください"}
			if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='question' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Control(ctx, id, instruction, opts); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControlRetryBothLimitsPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name, status, view string
		generation         int
	}{
		{"generation", "failed", `{"instructions":[]}`, 3},
		{"worker count", "held", `{"instructions":[],"workerExecution":{"implementationRuns":4}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "owner/" + strings.ReplaceAll(tc.name, " ", "-")
			id := seedJob(t, ctx, db, repo, "worker", 743, 7)
			row := fmt.Sprintf(`{"repo":%q,"issue":743,"step":"実装","provider":"codex","inputTokens":101,"cacheReadTokens":0,"outputTokens":0}`, repo)
			if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES($1,$2,$3,$4,$5,$6,$7)`, tc.name, now, "worker", repo, 743, row, "worker"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET status=$2,hold_reason=CASE WHEN $2='held' THEN 'question' ELSE NULL END,generation=$3,view=$4 WHERE id=$1`, id, tc.status, tc.generation, tc.view); err != nil {
				t.Fatal(err)
			}
			input := ControlInput{ID: fmt.Sprintf("%08d-5555-4555-8555-555555555555", i+50), Action: "retry", By: "token:worker", OverLimitStatement: "共通番号743の両方の上限を確認して再試行を承認します"}
			if _, err := s.Control(ctx, id, input, ControlOptions{RunLimit: 4, UsageLimit: 100}); err != nil {
				t.Fatal(err)
			}
			job, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			approvals := fleet.List(job.View["overLimitApprovals"])
			if len(approvals) != 1 {
				t.Fatalf("approvals: %#v", approvals)
			}
			approval := fleet.Object(approvals[0])
			if approval["kind"] != "run_and_usage" || approval["implementationRuns"] != float64(4) || approval["limit"] != float64(4) || approval["usageLimit"] != float64(100) || !strings.Contains(fleet.Text(approval["usageReason"]), "上限") {
				t.Fatalf("approval: %#v", approval)
			}
		})
	}
}
