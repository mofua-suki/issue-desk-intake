package jobs

import (
	"strings"
	"testing"
	"time"
)

func TestRunningPublicationReceiptAfterNewWork(t *testing.T) {
	for _, scenario := range []string{"instruction", "implementation phase"} {
		t.Run(scenario, func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('completionPlan',$2::jsonb) WHERE id=$1`, id, finalResumePlan); err != nil {
				t.Fatal(err)
			}
			s, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("a", 40)
			branch, pr := "work/published", int64(12)
			report := Report{Epoch: 7, Status: "running", Branch: Optional[string]{Present: true, Value: &branch}, HeadSHA: Optional[string]{Present: true, Value: &head}, PR: Optional[int64]{Present: true, Value: &pr}, View: map[string]any{"phase": "publishing"}}
			if _, err = s.Report(ctx, id, "nas", report); err != nil {
				t.Fatal(err)
			}
			if scenario == "instruction" {
				if _, err = s.Control(ctx, id, ControlInput{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Action: "instruction", By: "issue-screen", Text: "追加の作業"}, ControlOptions{}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", View: map[string]any{"phase": "running"}}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if _, err = s.Report(ctx, id, "nas", report); err != nil {
					t.Fatal(err)
				}
			}
			var n int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='running_publication'`, id).Scan(&n); err != nil || n != 2 {
				t.Fatalf("receipts=%d err=%v", n, err)
			}
			reason := "review_held"
			if _, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "held", HoldReason: &reason, View: map[string]any{"releasePending": true}}); err != nil {
				t.Fatal(err)
			}
			// This held result is now backed by the later publication, not the pre-instruction receipt.
			job := approveFinalResumeFixture(t, ctx, s, id, head)
			if job.Status != "queued" || reportApprovalEvents(t, ctx, db, id) != 1 {
				t.Fatalf("job=%+v", job)
			}
		})
	}
}
