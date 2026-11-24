package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

// Issue plans are intake-owned; publication and fingerprint binding use the
// ordinary worker report path after dispatch, not hand-built binding JSON.
func finalDiffFixture(t *testing.T, plan string, withDiff, resume bool) (*sql.DB, context.Context, *Store, *Job, Report, FinalApprovalRecord) {
	t.Helper()
	db, ctx, s, assignments, id := legacyArtifactFixture(t, 2, "queued")
	job := takeLegacyArtifact(t, ctx, s, assignments, id, "one")
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET scope_source='widened',scope=ARRAY['src/'],view=view || jsonb_build_object('completionPlan',$2::jsonb) WHERE id=$1`, id, plan); err != nil {
		t.Fatal(err)
	}
	head, branch, fp, pr := strings.Repeat("a", 40), "work/published", "sha256:"+strings.Repeat("b", 64), int64(12)
	report := Report{Epoch: job.LeaseEpoch, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &head}, Branch: Optional[string]{Present: true, Value: &branch}, PR: Optional[int64]{Present: true, Value: &pr}, View: map[string]any{"phase": "publishing"}}
	if withDiff {
		report.DiffFingerprint = Optional[string]{Present: true, Value: &fp}
	}
	job, err := s.Report(ctx, id, "one", report)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingFinalApprovals(job)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending before approval: %v %v", pending, err)
	}
	if resume {
		turn, err := s.AcquireTurn(ctx, job.Repo, TurnRequest{Host: "one", JobID: &id, Epoch: report.Epoch})
		if err != nil || turn.Granted {
			t.Fatalf("approval hold: %+v %v", turn, err)
		}
	}
	input := FinalApprovalRecord{ID: pending[0].ID, Fingerprint: pending[0].Fingerprint, HeadSHA: head, MessageID: "10000000-0000-4000-8000-000000000001", Quote: "この差分の配置を承認します", Via: "token", PC: "one", InstructionAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}
	job, err = s.RecordFinalApproval(ctx, id, input)
	if err != nil {
		t.Fatal(err)
	}
	if resume {
		if job.Status != "queued" {
			t.Fatalf("approval did not requeue: %+v", job)
		}
		// The original PC is unavailable; dispatch resumes the published PR
		// on the other assigned PC using its ordinary eligibility rules.
		if _, err := db.ExecContext(ctx, `UPDATE hosts SET ready=false WHERE host='one'`); err != nil {
			t.Fatal(err)
		}
		var lease *Lease
		job, lease, err = s.Next(ctx, "two", 1, assignments)
		if err != nil || job == nil || job.ID != id || lease == nil || job.View["publicationResume"] != true {
			t.Fatalf("approval resume: %+v %+v %v", job, lease, err)
		}
		report.Epoch = lease.Epoch
	}
	return db, ctx, s, job, report, input
}

func TestFinalApprovalDiffFingerprintTransfer(t *testing.T) {
	_, ctx, s, job, report, _ := finalDiffFixture(t, finalResumePlan, true, true)
	changed := strings.Repeat("c", 40)
	report.HeadSHA.Value = &changed
	job, err := s.Report(ctx, job.ID, "two", report)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(job.View)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		pending, err := PendingFinalApprovals(job)
		if err != nil || len(pending) != 0 {
			t.Fatalf("same diff became pending: %v %v", pending, err)
		}
	}
	after, _ := json.Marshal(job.View)
	if string(before) != string(after) {
		t.Fatal("pending read modified approval history")
	}
	turn, err := s.AcquireTurn(ctx, job.Repo, TurnRequest{Host: "two", JobID: &job.ID, Epoch: report.Epoch})
	if err != nil || !turn.Granted {
		t.Fatalf("same diff turn: %+v %v", turn, err)
	}
}

func TestFinalApprovalDiffFingerprintInvalidation(t *testing.T) {
	for _, change := range []string{"diff", "missing", "clear", "approval", "id", "pr", "branch", "generation-reverse", "generation-missing", "generation-invalid", "job", "invalid-binding", "head-binding", "legacy-no-backfill"} {
		t.Run(change, func(t *testing.T) {
			db, ctx, s, job, report, _ := finalDiffFixture(t, finalResumePlan, change != "legacy-no-backfill", false)
			changed := strings.Repeat("c", 40)
			report.HeadSHA.Value = &changed
			var query string
			switch change {
			case "diff":
				fp := "sha256:" + strings.Repeat("d", 64)
				report.DiffFingerprint.Value = &fp
			case "missing":
				report.DiffFingerprint = Optional[string]{}
			case "clear":
				report.DiffFingerprint = Optional[string]{Present: true}
			case "approval":
				query = `UPDATE jobs SET view=jsonb_set(view,'{completionPlan,approvals,0,condition}','"別の条件"') WHERE id=$1`
			case "id":
				query = `UPDATE jobs SET view=jsonb_set(view,'{completionPlan,approvals,0,id}','"another"') WHERE id=$1`
			case "pr":
				pr := int64(13)
				report.PR.Value = &pr
			case "branch":
				branch := "other-branch"
				report.Branch.Value = &branch
			case "generation-reverse":
				query = `UPDATE jobs SET view=jsonb_set(view,'{finalApprovals,0,diffFingerprint,generation}','1') WHERE id=$1`
			case "generation-missing":
				query = `UPDATE jobs SET view=view #- '{finalApprovals,0,diffFingerprint,generation}' WHERE id=$1`
			case "generation-invalid":
				query = `UPDATE jobs SET view=jsonb_set(view,'{finalApprovals,0,diffFingerprint,generation}','-1') WHERE id=$1`
			case "job":
				query = `UPDATE jobs SET view=jsonb_set(view,'{finalApprovals,0,diffFingerprint,jobId}','0') WHERE id=$1`
			case "invalid-binding":
				query = `UPDATE jobs SET view=jsonb_set(view,'{finalApprovals,0,diffFingerprint,value}','"bad"') WHERE id=$1`
			case "head-binding":
				query = `UPDATE jobs SET view=jsonb_set(view,'{finalApprovals,0,diffFingerprint,headSha}',to_jsonb(repeat('e',40))) WHERE id=$1`
			case "legacy-no-backfill":
				fp := "sha256:" + strings.Repeat("b", 64)
				report.DiffFingerprint = Optional[string]{Present: true, Value: &fp}
			}
			if query != "" {
				if _, err := db.ExecContext(ctx, query, job.ID); err != nil {
					t.Fatal(err)
				}
			}
			job, err := s.Report(ctx, job.ID, "one", report)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := PendingFinalApprovals(job)
			if err != nil || len(pending) != 1 || len(fleet.List(job.View["finalApprovalTransfers"])) != 0 {
				t.Fatalf("invalid transfer: pending=%v history=%v err=%v", pending, job.View["finalApprovalTransfers"], err)
			}
			turn, err := s.AcquireTurn(ctx, job.Repo, TurnRequest{Host: "one", JobID: &job.ID, Epoch: report.Epoch})
			if err != nil || turn.Granted {
				t.Fatalf("invalid transfer granted turn: %+v %v", turn, err)
			}
		})
	}
}

func TestFinalApprovalTransferPersistence(t *testing.T) {
	db, ctx, s, job, report, input := finalDiffFixture(t, finalResumePlan, true, false)
	original := job.View["finalApprovals"]
	changed := strings.Repeat("c", 40)
	report.HeadSHA.Value = &changed
	report.View = map[string]any{"finalApprovalTransfers": []any{"forged"}, "finalApprovals": []any{}, "diffFingerprint": "forged"}
	for i := 0; i < 2; i++ {
		var err error
		job, err = s.Report(ctx, job.ID, "one", report)
		if err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := New(db, Options{Now: func() time.Time { return now.Add(time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	job, err = reloaded.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	history := fleet.List(job.View["finalApprovalTransfers"])
	if len(history) != 1 || !reflect.DeepEqual(original, job.View["finalApprovals"]) {
		t.Fatalf("original approval/history after reload: %v %v", job.View["finalApprovals"], history)
	}
	entry := fleet.Object(history[0])
	if entry["id"] != input.ID || entry["fingerprint"] != input.Fingerprint || entry["messageId"] != input.MessageID || entry["fromHead"] != input.HeadSHA || entry["toHead"] != changed || entry["diffFingerprint"] != *report.DiffFingerprint.Value || entry["at"] != now.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("transfer provenance: %v", entry)
	}
	if pending, err := PendingFinalApprovals(job); err != nil || len(pending) != 0 {
		t.Fatalf("reload lost approval: %v %v", pending, err)
	}
	job, err = reloaded.Report(ctx, job.ID, "one", report)
	if err != nil || !reflect.DeepEqual(history, job.View["finalApprovalTransfers"]) {
		t.Fatalf("report after reload duplicated history: %+v %v", job, err)
	}
	if _, _, err := reloaded.Renew(ctx, job.ID, "one", report.Epoch, report.View); err != nil {
		t.Fatal(err)
	}
	// Replayed chat statements keep the original SHA; a fresh submission for
	// that old SHA is still refused after a transfer.
	job, err = reloaded.RecordFinalApproval(ctx, job.ID, input)
	if err != nil || !reflect.DeepEqual(history, job.View["finalApprovalTransfers"]) || !reflect.DeepEqual(original, job.View["finalApprovals"]) {
		t.Fatalf("replay changed provenance: %+v %v", job, err)
	}
	fresh := input
	fresh.MessageID = "10000000-0000-4000-8000-000000000002"
	_, err = reloaded.RecordFinalApproval(ctx, job.ID, fresh)
	code(t, err, 409, "")
	// A further equivalent head refers back to the original statement and SHA.
	next := strings.Repeat("d", 40)
	report.HeadSHA.Value = &next
	job, err = reloaded.Report(ctx, job.ID, "one", report)
	if err != nil || len(fleet.List(job.View["finalApprovalTransfers"])) != 2 || !reflect.DeepEqual(original, job.View["finalApprovals"]) {
		t.Fatalf("second transfer: %+v %v", job, err)
	}
	if pending, err := PendingFinalApprovals(job); err != nil || len(pending) != 0 {
		t.Fatalf("second transfer pending: %v %v", pending, err)
	}
	// The audit trail is retained, but cannot authorize another head after
	// the current fingerprint has been explicitly cleared.
	report.DiffFingerprint = Optional[string]{Present: true}
	job, err = reloaded.Report(ctx, job.ID, "one", report)
	if err != nil || len(fleet.List(job.View["finalApprovalTransfers"])) != 2 {
		t.Fatalf("clearing fingerprint lost audit history: %+v %v", job, err)
	}
	if pending, err := PendingFinalApprovals(job); err != nil || len(pending) != 1 {
		t.Fatalf("history authorized a cleared fingerprint: %v %v", pending, err)
	}
}

func TestFinalApprovalTransferReportRollback(t *testing.T) {
	_, ctx, s, job, report, _ := finalDiffFixture(t, finalResumePlan, true, false)
	failed := errors.New("merge callback failed")
	s.options.OnMerged = func(context.Context, *sql.Tx, *Job, map[string]any) error { return failed }
	changed := strings.Repeat("c", 40)
	report.HeadSHA.Value, report.Status = &changed, "merged"
	if _, err := s.Report(ctx, job.ID, "one", report); !errors.Is(err, failed) {
		t.Fatalf("merge report: %v", err)
	}
	got, err := s.Get(ctx, job.ID)
	if err != nil || !reflect.DeepEqual(job, got) {
		t.Fatalf("failed transaction changed state or history: %+v %v", got, err)
	}
}

func TestFinalApprovalTransferRequiresPersistedHistory(t *testing.T) {
	// Restore an incomplete or inconsistent saved audit trail. A matching diff
	// alone must not conceal the missing provenance when the job is read again.
	for _, field := range []string{"missing", "id", "fingerprint", "messageId", "fromHead", "toHead", "diffFingerprint", "fromGeneration", "toGeneration", "at"} {
		t.Run(field, func(t *testing.T) {
			db, ctx, s, job, report, _ := finalDiffFixture(t, finalResumePlan, true, false)
			changed := strings.Repeat("c", 40)
			report.HeadSHA.Value = &changed
			job, err := s.Report(ctx, job.ID, "one", report)
			if err != nil {
				t.Fatal(err)
			}
			if field == "missing" {
				_, err = db.ExecContext(ctx, `UPDATE jobs SET view=view-'finalApprovalTransfers' WHERE id=$1`, job.ID)
			} else {
				_, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,ARRAY['finalApprovalTransfers','0',$2::text],'"mismatch"'::jsonb) WHERE id=$1`, job.ID, field)
			}
			if err != nil {
				t.Fatal(err)
			}
			job, err = s.Get(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(job.View)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := PendingFinalApprovals(job)
			if err != nil || len(pending) != 1 || pending[0].HeadSHA != changed {
				t.Fatalf("incomplete history authorized transfer: %v %v", pending, err)
			}
			after, err := json.Marshal(job.View)
			if err != nil || string(before) != string(after) {
				t.Fatalf("approval read repaired history: %v", err)
			}
			turn, err := s.AcquireTurn(ctx, job.Repo, TurnRequest{Host: "one", JobID: &job.ID, Epoch: report.Epoch})
			if err != nil || turn.Granted {
				t.Fatalf("incomplete history granted turn: %+v %v", turn, err)
			}
		})
	}
}

func TestFinalApprovalTransferRetry(t *testing.T) {
	db, ctx, s, job, report, _ := finalDiffFixture(t, finalResumePlan, true, false)
	original := job.View["finalApprovals"]
	report.Status = "failed"
	if _, err := s.Report(ctx, job.ID, "one", report); err != nil {
		t.Fatal(err)
	}
	retried, err := s.Control(ctx, job.ID, ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "issue-screen"}, ControlOptions{})
	if err != nil || retried.Generation != job.Generation+1 {
		t.Fatalf("retry: %+v %v", retried, err)
	}
	// Assign a fresh lease, as dispatch does after retry.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_host='one',lease_epoch=$2,lease_expires_at=$3 WHERE id=$1`, job.ID, report.Epoch+1, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	report.Epoch++
	report.Status = "running"
	changed := strings.Repeat("c", 40)
	report.HeadSHA.Value = &changed
	job, err = s.Report(ctx, job.ID, "one", report)
	if err != nil {
		t.Fatal(err)
	}
	history := fleet.List(job.View["finalApprovalTransfers"])
	if len(history) != 1 || !equivalent(fleet.Object(history[0])["fromGeneration"], retried.Generation-1) || !equivalent(fleet.Object(history[0])["toGeneration"], retried.Generation) || !reflect.DeepEqual(original, job.View["finalApprovals"]) {
		t.Fatalf("retry provenance: %v", history)
	}
	if pending, err := PendingFinalApprovals(job); err != nil || len(pending) != 0 {
		t.Fatalf("retry pending: %v %v", pending, err)
	}
	job, err = s.Report(ctx, job.ID, "one", report)
	if err != nil || len(fleet.List(job.View["finalApprovalTransfers"])) != 1 {
		t.Fatalf("duplicate retry: %+v %v", job, err)
	}
}

func TestFinalApprovalTransferLegacyHistory(t *testing.T) {
	db, ctx, s, job, report, _ := finalDiffFixture(t, finalResumePlan, true, false)
	changed := strings.Repeat("c", 40)
	report.HeadSHA.Value = &changed
	job, err := s.Report(ctx, job.ID, "one", report)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `UPDATE jobs SET view=view #- '{finalApprovalTransfers,0,fromGeneration}' #- '{finalApprovalTransfers,0,toGeneration}' WHERE id=$1`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := PendingFinalApprovals(job); err != nil || len(pending) != 0 {
		t.Fatalf("legacy pending: %v %v", pending, err)
	}
	job, err = s.Report(ctx, job.ID, "one", report)
	if err != nil || len(fleet.List(job.View["finalApprovalTransfers"])) != 1 {
		t.Fatalf("legacy duplicated: %+v %v", job, err)
	}
}
