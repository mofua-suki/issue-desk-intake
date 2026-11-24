package startup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/workplan"
)

func TestFailedMergedRecoveryBackgroundPostgres(t *testing.T) {
	db := appPostgres(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	now := time.Now().UTC()
	head, merge, branch := strings.Repeat("a", 40), strings.Repeat("b", 40), "codex/branch"
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "one", Criterion: "条件", Owner: "implementer", Phase: "verification", Method: "照合", Expected: "一致", EvidenceKind: "fixed_test", Capabilities: []string{}, Inputs: []string{}, Children: []int{}}}}
	evidence := []workplan.Evidence{{Requirement: "one", PlanSHA: plan.Fingerprint(), Head: head, Generation: 1, RecordID: "fixed-test:1", Source: "implementer", Status: "passed", Detail: "passed"}}
	view, _ := json.Marshal(map[string]any{"executionPlan": plan.Record(), "executionReceipts": evidence})
	var id int64
	if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,head_sha,pr,branch,generation,view) VALUES('owner/repo',1,'running','nas',7,$1,$2,3,$3,1,$4::jsonb) RETURNING id`, now.Add(time.Hour), head, branch, view).Scan(&id); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	store, err := jobs.New(db, jobs.Options{Now: func() time.Time { return now }, ReadPull: func(_ context.Context, repo string, pr int64) (map[string]any, error) {
		return map[string]any{"number": pr, "merged": true, "merge_commit_sha": merge, "head": map[string]any{"sha": head, "ref": branch, "repo": map[string]any{"full_name": repo}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": repo, "default_branch": "main"}}}, nil
	}, Publish: func(map[string]any) {
		select {
		case done <- struct{}{}:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Report(ctx, id, "nas", jobs.Report{Epoch: 7, Status: "merged", Result: map[string]any{"summary": strings.Repeat("x", 40000)}}); err == nil {
		t.Fatal("report accepted")
	}
	for i := 0; i < 3; i++ {
		if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err = store.Sweep(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	default:
	}
	app := &App{ctx: ctx, cancel: cancel, Jobs: store, Log: func(string) {}}
	app.startFailedMergedRecovery()
	defer app.StopBackground()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("周期処理が完了しない")
	}
	job, err := store.Get(ctx, id)
	if err != nil || job.Status != "merged" || job.Result["mergeCommit"] != merge {
		t.Fatal(job, err)
	}
}
