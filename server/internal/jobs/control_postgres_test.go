package jobs

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"testing"
	"time"
)

func TestControlReceiptsCommandsLimitsAndFencingPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	store, e := New(db, Options{Now: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	id := seedJob(t, ctx, db, "owner/repo", "worker", 1, 7)
	if _, e = db.ExecContext(ctx, `UPDATE jobs SET view=view || '{"phase":"implementation"}'::jsonb WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	stop := ControlInput{ID: "11111111-1111-4111-8111-111111111111", Action: "stop", By: "issue-screen"}
	if _, e = store.Control(ctx, id, stop, ControlOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e = store.Control(ctx, id, stop, ControlOptions{}); e != nil {
		t.Fatal(e)
	}
	var count int
	var payload string
	if e = db.QueryRowContext(ctx, `SELECT count(*) FROM job_commands WHERE repo='owner/repo' AND issue=1`).Scan(&count); e != nil || count != 1 {
		t.Fatal("duplicate stop command", count, e)
	}
	if e = db.QueryRowContext(ctx, `SELECT payload FROM job_commands WHERE id=$1`, stop.ID).Scan(&payload); e != nil {
		t.Fatal(e)
	}
	var order map[string]any
	_ = json.Unmarshal([]byte(payload), &order)
	if order["epoch"] != float64(7) || order["jobId"] != float64(id) {
		t.Fatal("command not fenced", order)
	}
	var eventRaw []byte
	var eventHost string
	if e = db.QueryRowContext(ctx, `SELECT data,host FROM job_events WHERE job_id=$1 AND kind='control'`, id).Scan(&eventRaw, &eventHost); e != nil {
		t.Fatal(e)
	}
	var event map[string]any
	if e = json.Unmarshal(eventRaw, &event); e != nil {
		t.Fatal(e)
	}
	if eventHost != "issue-screen" || event["from"] != "running" || event["to"] != "running" || event["leaseHost"] != "worker" || event["epoch"] != float64(7) || event["phase"] != "implementation" || event["action"] != "stop" || event["id"] != stop.ID || event["by"] != stop.By || event["generation"] != float64(0) || event["instruction"] != false {
		t.Fatal(eventHost, event)
	}
	if e = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='control'`, id).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	stop.By = "token:different"
	if _, e = store.Control(ctx, id, stop, ControlOptions{}); e == nil {
		t.Fatal("same ID changed actor")
	}
	if _, e = db.ExecContext(ctx, `UPDATE jobs SET status='failed',hold_reason=NULL WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	retry := ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "token:worker", Text: "原因と修正方針を確認した"}
	if _, e = store.Control(ctx, id, retry, ControlOptions{RunLimit: 1, UsageLimit: 8000000}); e == nil {
		t.Fatal("run limit bypassed")
	}
	retry.OverLimitStatement = "共通番号1の原因を確認し、この再試行を承認します"
	job, e := store.Control(ctx, id, retry, ControlOptions{RunLimit: 1, UsageLimit: 8000000})
	if e != nil {
		t.Fatal(e)
	}
	if job.Generation != 1 || job.Status != "queued" || len(fleet.List(job.View["overLimitApprovals"])) != 1 || fleet.Object(job.View["reimplementExpected"])["usageAdmission"] != true {
		t.Fatal("retry state", job)
	}
	again, e := store.Control(ctx, id, retry, ControlOptions{RunLimit: 1, UsageLimit: 8000000})
	if e != nil || again.Generation != 1 {
		t.Fatal("retry replay incremented generation", again, e)
	}
	close := ControlInput{ID: "33333333-3333-4333-8333-333333333333", Action: "close", By: "issue-screen", IfIdle: true}
	job, e = store.Control(ctx, id, close, ControlOptions{})
	if e != nil || job.Status != "closed" || job.LeaseExpiresAt != nil {
		t.Fatal(job, e)
	}
}
func TestPermissionApprovalRequiresAuditAndUnchangedHeadPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	store, _ := New(db, Options{Now: func() time.Time { return now }})
	id := seedJob(t, ctx, db, "owner/repo", "worker", 2, 3)
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, e := db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='review_held',pr=12,head_sha=$2,branch='work' WHERE id=$1`, id, head); e != nil {
		t.Fatal(e)
	}
	operation := "44444444-4444-4444-8444-444444444444"
	identity := map[string]any{"id": operation, "identity": map[string]any{"project": "owner/repo", "number": int64(2), "head": head}, "approvedBy": "screen", "approvedFrom": "screen", "approvalStatement": "対象の権限ファイルの変更を承認します"}
	hold := map[string]any{"project": "owner/repo", "head": head, "pr": int64(12), "generation": int64(0), "epoch": int64(3), "files": []any{"host-ssh.json"}}
	if _, e := store.PermissionApproval(ctx, id, identity, hold, false); e != nil {
		t.Fatal(e)
	}
	if _, e := store.PermissionApproval(ctx, id, identity, hold, true); e == nil {
		t.Fatal("resumed without audit comments")
	}
	if _, e := store.PermissionReceipt(ctx, id, operation, map[string]any{"issuePosted": true, "prPosted": true}); e != nil {
		t.Fatal(e)
	}
	if _, e := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2 WHERE id=$1`, id, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); e != nil {
		t.Fatal(e)
	}
	if _, e := store.PermissionApproval(ctx, id, identity, hold, true); e == nil {
		t.Fatal("changed head accepted")
	}
	if _, e := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2 WHERE id=$1`, id, head); e != nil {
		t.Fatal(e)
	}
	job, e := store.PermissionApproval(ctx, id, identity, hold, true)
	if e != nil || job.Status != "queued" || fleet.Object(job.View["permissionApproval"])["resumePublication"] != true {
		t.Fatal("approval not finalized", job, e)
	}
}

func TestRunningInstructionsSurviveControllerRestartAndOwnerChangePostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	first, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	id := seedJob(t, ctx, db, "owner/repo", "worker", 1, 7)
	input := ControlInput{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Action: "instruction", By: "issue-screen", Text: "対象を確認して修正する", Context: "保存する判断の根拠"}
	if _, err = first.Control(ctx, id, input, ControlOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE jobs SET lease_host='another-worker',lease_epoch=8 WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Control(ctx, id, input, ControlOptions{}); err != nil {
		t.Fatal(err)
	}
	job, err := reopened.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	entries := fleet.List(job.View["instructions"])
	if len(entries) != 1 {
		t.Fatal(entries)
	}
	instruction := fleet.Object(entries[0])
	if instruction["id"] != "command:"+input.ID || instruction["text"] != input.Text || instruction["context"] != input.Context {
		t.Fatal(instruction)
	}
	var commands int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM job_commands WHERE id=$1", input.ID).Scan(&commands); err != nil || commands != 1 {
		t.Fatal(commands, err)
	}
}

func TestControlEventTransitionMetadataPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	store, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, view, action, status, want string
		phase                            any
		present                          bool
	}{
		{"missing", `{}`, "close", "queued", "closed", nil, false},
		{"null", `{"phase":null}`, "close", "queued", "closed", nil, true},
		{"empty", `{"phase":""}`, "close", "queued", "closed", "", true},
		{"retry", `{"phase":"review"}`, "retry", "failed", "queued", "review", true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := seedJob(t, ctx, db, "owner/repo", "worker", int64(i+20), 7)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET status=$2,lease_host=NULL,view=$3::jsonb WHERE id=$1`, id, tc.status, tc.view); err != nil {
				t.Fatal(err)
			}
			input := ControlInput{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), Action: tc.action, By: "tester"}
			if tc.action == "retry" {
				input.By = "token:worker"
				input.OverLimitStatement = fmt.Sprintf("共通番号%dの原因を確認し、この再試行を承認します", i+20)
			}
			job, err := store.Control(ctx, id, input, ControlOptions{RunLimit: 1, UsageLimit: 8000000})
			if err != nil {
				t.Fatal(err)
			}
			var raw []byte
			var host string
			if err := db.QueryRowContext(ctx, `SELECT data,host FROM job_events WHERE job_id=$1 AND kind='control'`, id).Scan(&raw, &host); err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			phase, present := event["phase"]
			if job.Status != tc.want || event["from"] != tc.status || event["to"] != tc.want || event["leaseHost"] != nil || event["epoch"] != float64(7) || present != tc.present || phase != tc.phase || event["generation"] != float64(job.Generation) || host != input.By {
				t.Fatalf("job=%+v event=%v host=%s", job, event, host)
			}
			if tc.action == "retry" && job.Generation != 1 {
				t.Fatal(job.Generation)
			}
		})
	}
}
