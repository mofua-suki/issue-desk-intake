package jobs

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func reportApprovalFixture(t *testing.T, flag string) (*sql.DB, context.Context, *Store, int64, string) {
	t.Helper()
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
	publishFinalApprovalFixture(t, ctx, s, id, "nas", head)
	reason := "review_held"
	_, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "held", HoldReason: &reason, View: map[string]any{flag: true}})
	if err != nil {
		t.Fatal(err)
	}
	return db, ctx, s, id, head
}

func reportApprovalEvents(t *testing.T, ctx context.Context, db *sql.DB, id int64) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='queued' AND data->>'finalApproval'='true'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFinalApprovalReportHoldResume(t *testing.T) {
	for _, flag := range []string{"releasePending", "publishPending"} {
		t.Run(flag, func(t *testing.T) {
			db, ctx, s, id, head := reportApprovalFixture(t, flag)
			before, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			after := approveFinalResumeFixture(t, ctx, s, id, head)
			if after.Status != "queued" || after.HoldReason != nil || EffectiveRuns(after) != EffectiveRuns(before) || reportApprovalEvents(t, ctx, db, id) != 1 {
				t.Fatalf("before=%+v after=%+v", before, after)
			}
			pending, err := PendingFinalApprovals(after)
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending=%v err=%v", pending, err)
			}
		})
	}
}

func TestFinalApprovalReportHoldBoundaries(t *testing.T) {
	for _, scenario := range []string{"pending", "permission", "deletion", "other hold", "stop", "stop command", "live check", "instruction", "publication", "artifact", "branch", "generation", "pr", "head"} {
		t.Run(scenario, func(t *testing.T) {
			db, ctx, s, id, head := reportApprovalFixture(t, "releasePending")
			var query string
			switch scenario {
			case "pending":
				query = `UPDATE jobs SET view=jsonb_set(view,'{completionPlan,approvals}',(view#>'{completionPlan,approvals}') || '[{"category":"irreversible","id":"second","phase":"after_diff","target":"削除","operation":"削除","condition":"確認"}]'::jsonb) WHERE id=$1`
			case "permission":
				query = `UPDATE jobs SET view=view || '{"permissionHold":{}}'::jsonb WHERE id=$1`
			case "deletion":
				query = `UPDATE jobs SET view=view || '{"deletionApproval":{}}'::jsonb WHERE id=$1`
			case "other hold":
				query = `UPDATE jobs SET hold_reason='deletions' WHERE id=$1`
			case "stop":
				query = `UPDATE jobs SET view=view || '{"stopRequested":true}'::jsonb WHERE id=$1`
			case "live check":
				query = `UPDATE jobs SET view=view || '{"liveCheck":{"status":"pending"}}'::jsonb WHERE id=$1`
			case "publication":
				query = `DELETE FROM job_events WHERE job_id=$1 AND kind IN ('pr_created','running_publication')`
			case "artifact":
				query = `UPDATE jobs SET view=jsonb_set(view,'{continuation,artifact,headSha}', '"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"') WHERE id=$1`
			case "branch":
				query = `UPDATE jobs SET branch='work/other' WHERE id=$1`
			case "generation":
				query = `UPDATE jobs SET generation=generation+1 WHERE id=$1`
			case "pr":
				query = `UPDATE jobs SET pr=pr+1 WHERE id=$1`
			}
			if query != "" {
				if _, err := db.ExecContext(ctx, query, id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "instruction" {
				if _, err := s.Control(ctx, id, ControlInput{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Action: "instruction", By: "issue-screen", Text: "追加の作業"}, ControlOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "stop command" { // Stop commands are consulted even if the view has not yet been updated.
				if _, err := db.ExecContext(ctx, `INSERT INTO job_commands(id,target_host,repo,issue,kind,status,payload) SELECT '00000000-0000-4000-8000-000000000123','nas',repo,issue,'stop','queued','{}' FROM jobs WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			j, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := PendingFinalApprovals(j)
			if err != nil || len(pending) == 0 {
				t.Fatal(pending, err)
			}
			input := FinalApprovalRecord{ID: pending[0].ID, Fingerprint: pending[0].Fingerprint, HeadSHA: head, MessageID: "00000000-0000-4000-8000-000000000099", Quote: "この差分を承認します"}
			if scenario == "head" {
				input.HeadSHA = strings.Repeat("b", 40)
			}
			saved, err := s.RecordFinalApproval(ctx, id, input)
			if scenario == "head" {
				if err == nil {
					t.Fatal("changed head accepted")
				}
				saved, err = s.Get(ctx, id)
			}
			if err != nil || saved.Status != "held" || reportApprovalEvents(t, ctx, db, id) != 0 {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
		})
	}
}

func TestFinalApprovalTurnHoldOtherBlockers(t *testing.T) {
	for _, blocker := range []string{`{"permissionHold":{}}`, `{"deletionApproval":{}}`, `{"stopRequested":true}`, `{"liveCheck":{"status":"pending"}}`} {
		t.Run(blocker, func(t *testing.T) {
			db, ctx, s, _, id, head := finalResumeFixture(t)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=view || $2::jsonb WHERE id=$1`, id, blocker); err != nil {
				t.Fatal(err)
			}
			saved := approveFinalResumeFixture(t, ctx, s, id, head)
			if saved.Status != "held" || reportApprovalEvents(t, ctx, db, id) != 0 {
				t.Fatalf("saved=%+v", saved)
			}
		})
	}
}
