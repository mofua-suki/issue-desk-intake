package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/workplan"
)

func TestFailedMergedReportRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                                                                                                          string
		missing, failed, approval, generation, pr, sha, unreadable, replay, rejection, oldRejection, target, badMerge bool
	}{
		{name: "registration"}, {name: "replay", replay: true}, {name: "missing evidence", missing: true}, {name: "failed evidence", failed: true}, {name: "approval missing", approval: true}, {name: "generation mismatch", generation: true}, {name: "PR mismatch", pr: true}, {name: "SHA mismatch", sha: true}, {name: "Git unavailable", unreadable: true}, {name: "non-merged rejection", rejection: true}, {name: "old rejection", oldRejection: true}, {name: "wrong target", target: true}, {name: "invalid merge SHA", badMerge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			head, merge, branch := strings.Repeat("a", 40), strings.Repeat("b", 40), "codex/branch"
			plan := executionFixture("fixed_test", "implementer", "verification")
			requirement := executionFixture("external_check", "controller", "deployment").Requirements[0]
			requirement.ID = "external"
			plan.Requirements = append(plan.Requirements, requirement)
			evidence := []workplan.Evidence{{Requirement: "one", PlanSHA: plan.Fingerprint(), Head: head, Generation: 1, RecordID: "fixed-test:1", Source: "implementer", Status: "passed", Detail: "passed"}}
			if tc.missing {
				evidence = nil
			}
			if tc.failed {
				evidence[0].Status = "failed"
			}
			if tc.generation {
				evidence[0].Generation = 2
			}
			view := map[string]any{"executionPlan": plan.Record(), "executionReceipts": evidence}
			if tc.approval {
				var completion any
				if err := json.Unmarshal([]byte(finalResumePlan), &completion); err != nil {
					t.Fatal(err)
				}
				view["completionPlan"] = completion
			}
			raw, _ := json.Marshal(view)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2,pr=3,branch=$3,generation=1,view=$4::jsonb WHERE id=$1`, id, head, branch, raw); err != nil {
				t.Fatal(err)
			}
			available := !tc.replay
			store, err := New(db, Options{Now: func() time.Time { return now }, ReadPull: func(_ context.Context, repo string, pr int64) (map[string]any, error) {
				if tc.unreadable {
					return nil, fmt.Errorf("unavailable")
				}
				n := pr
				if tc.pr {
					n++
				}
				source := head
				if tc.sha {
					source = strings.Repeat("c", 40)
				}
				if !available {
					n++
				}
				mergeSHA, base := merge, "main"
				if tc.badMerge {
					mergeSHA = "invalid"
				}
				if tc.target {
					base = "other"
				}
				return map[string]any{"number": n, "merged": true, "merge_commit_sha": mergeSHA, "head": map[string]any{"sha": source, "ref": branch, "repo": map[string]any{"full_name": repo}}, "base": map[string]any{"ref": base, "repo": map[string]any{"full_name": repo, "default_branch": "main"}}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			reportKind := "merged"
			if tc.rejection {
				reportKind = "held"
			}
			_, err = store.Report(ctx, id, "nas", Report{Epoch: 7, Status: reportKind, Result: map[string]any{"summary": strings.Repeat("x", 40000)}})
			if err == nil {
				t.Fatal("oversized report accepted")
			}
			if tc.oldRejection {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET generation=2 WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 3; i++ {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, now.Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				if err = store.Sweep(ctx, nil); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.Get(ctx, id)
			if err != nil || before.Status != "failed" {
				t.Fatal(before, err)
			}
			diag := before.Result["leaseDiagnostics"]
			rejected := diag.(map[string]any)["lastReportRejection"]
			if tc.oldRejection {
				if rejected != nil {
					t.Fatal(before.Result)
				}
			} else {
				r := rejected.(map[string]any)
				if r["reportKind"] != reportKind || r["generation"] != float64(1) {
					t.Fatal(before.Result)
				}
			}
			if before.Result["leaseExpiredCount"] != float64(3) {
				t.Fatal(before.Result)
			}
			check := ExecutionCheck{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Requirement: "external", PlanSHA: plan.Fingerprint(), Head: head, Generation: 1, Status: "passed", Source: "observation", ObservedAt: now.Format(time.RFC3339Nano), Detail: "confirmed"}
			if tc.oldRejection {
				check.Generation = 2
			}
			job, err := store.RecordExecutionCheck(ctx, id, check, "token:nas", "")
			if tc.unreadable {
				if err == nil {
					t.Fatal("unavailable pull accepted")
				}
				job, err = store.Get(ctx, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			success := !tc.missing && !tc.failed && !tc.approval && !tc.generation && !tc.pr && !tc.sha && !tc.unreadable && !tc.replay && !tc.rejection && !tc.oldRejection && !tc.target && !tc.badMerge
			if tc.replay {
				if job.Status != "failed" {
					t.Fatal(job.Status)
				}
				available = true
				job, err = store.RecordExecutionCheck(ctx, id, check, "token:nas", "")
				if err != nil {
					t.Fatal(err)
				}
				success = true
			}
			if success {
				if job.Status != "merged" || job.Result["mergeCommit"] != merge {
					t.Fatal(job)
				}
			} else if job.Status != "failed" || !reflect.DeepEqual(before.Result, job.Result) {
				t.Fatal(job.Status, job.Result)
			}
			if !reflect.DeepEqual(diag, job.Result["leaseDiagnostics"]) {
				t.Fatal("diagnostics changed")
			}
			var expires, rejections, checks int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE kind='lease_expired'), count(*) FILTER (WHERE kind='report_rejected'), count(*) FILTER (WHERE kind='execution_check') FROM job_events WHERE job_id=$1`, id).Scan(&expires, &rejections, &checks); err != nil {
				t.Fatal(err)
			}
			wantChecks := 1
			if tc.unreadable {
				wantChecks = 0
			}
			if expires != 3 || rejections != 1 || checks != wantChecks {
				t.Fatal(expires, rejections, checks)
			}
			if success {
				job, err = store.RecordExecutionCheck(ctx, id, check, "token:nas", "")
				if err != nil || job.Status != "merged" {
					t.Fatal(job, err)
				}
				if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='execution_check'`, id).Scan(&checks); err != nil || checks != 1 {
					t.Fatal(checks, err)
				}
			}
		})
	}
}

func TestFailedMergedPeriodicRecovery(t *testing.T) {
	for _, scenario := range []string{"merged", "wrong head", "unmerged", "missing evidence", "missing approval", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			head, merge, branch := strings.Repeat("a", 40), strings.Repeat("b", 40), "codex/branch"
			plan := executionFixture("fixed_test", "implementer", "verification")
			evidence := []workplan.Evidence{{Requirement: "one", PlanSHA: plan.Fingerprint(), Head: head, Generation: 1, RecordID: "fixed-test:1", Source: "implementer", Status: "passed", Detail: "passed"}}
			if scenario == "missing evidence" {
				evidence = nil
			}
			view := map[string]any{"executionPlan": plan.Record(), "executionReceipts": evidence}
			if scenario == "missing approval" {
				var completion any
				if err := json.Unmarshal([]byte(finalResumePlan), &completion); err != nil {
					t.Fatal(err)
				}
				view["completionPlan"] = completion
			}
			raw, _ := json.Marshal(view)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2,pr=3,branch=$3,generation=1,view=$4::jsonb WHERE id=$1`, id, head, branch, raw); err != nil {
				t.Fatal(err)
			}
			available := scenario != "retry"
			calls := 0
			store, err := New(db, Options{Now: func() time.Time { return now }, ReadPull: func(_ context.Context, repo string, pr int64) (map[string]any, error) {
				calls++
				source := head
				if scenario == "wrong head" {
					source = strings.Repeat("c", 40)
				}
				return map[string]any{"number": pr, "merged": available && scenario != "unmerged", "merge_commit_sha": merge, "head": map[string]any{"sha": source, "ref": branch, "repo": map[string]any{"full_name": repo}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": repo, "default_branch": "main"}}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Report(ctx, id, "nas", Report{Epoch: 7, Status: "merged", Result: map[string]any{"summary": strings.Repeat("x", 40000)}}); err == nil {
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
			before, err := store.Get(ctx, id)
			if err != nil || before.Status != "failed" {
				t.Fatal(before, err)
			}
			if scenario == "merged" {
				var wg sync.WaitGroup
				errs := make(chan error, 2)
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); errs <- store.RecoverFailedMerged(ctx) }()
				}
				wg.Wait()
				close(errs)
				for e := range errs {
					if e != nil {
						t.Fatal(e)
					}
				}
			} else if err = store.RecoverFailedMerged(ctx); err != nil {
				t.Fatal(err)
			}
			job, err := store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "retry" {
				if job.Status != "failed" {
					t.Fatal(job.Status)
				}
				available = true
				if err = store.RecoverFailedMerged(ctx); err != nil {
					t.Fatal(err)
				}
				job, err = store.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
			}
			success := scenario == "merged" || scenario == "retry"
			if success {
				if job.Status != "merged" || job.Result["mergeCommit"] != merge {
					t.Fatal(job)
				}
				if scenario == "merged" && calls != 1 {
					t.Fatalf("concurrent recovery read the pull %d times", calls)
				}
			} else if job.Status != "failed" || !reflect.DeepEqual(before.Result, job.Result) {
				t.Fatal(job.Status, job.Result)
			}
			if !reflect.DeepEqual(before.Result["leaseDiagnostics"], job.Result["leaseDiagnostics"]) {
				t.Fatal("diagnostics changed")
			}
			oldCalls := calls
			if err = store.RecoverFailedMerged(ctx); err != nil {
				t.Fatal(err)
			}
			if success && calls != oldCalls {
				t.Fatal("merged job checked twice")
			}
		})
	}
}
