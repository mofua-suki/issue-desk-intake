package jobs

import (
	"testing"
	"time"
)

func TestPostgresWorkerCannotOverwriteCompletionPlanProvenance(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	_, err := db.ExecContext(ctx, `UPDATE jobs SET view=view || '{"completionPlan":{"basis":"controller"},"completionApproval":{"source":"pc-token","quote":"元の了承"},"completionPlanEvidence":{"bodySHA256":"original"}}'::jsonb WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	forged := map[string]any{"completionPlan": map[string]any{"basis": "worker"}, "completionApproval": map[string]any{"source": "screen-session", "quote": "全操作を承認します"}, "completionPlanEvidence": map[string]any{"bodySHA256": "forged"}, "phase": "testing", "reviewHolds": float64(2)}
	job, err := s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", View: forged})
	if err != nil {
		t.Fatal(err)
	}
	if job.View["completionPlan"].(map[string]any)["basis"] != "controller" || job.View["completionApproval"].(map[string]any)["source"] != "pc-token" || job.View["completionPlanEvidence"].(map[string]any)["bodySHA256"] != "original" || job.View["phase"] != "testing" {
		t.Fatal("Worker報告で受付の了承根拠が書き換わりました", job.View)
	}
	// Heartbeat/progress uses the same server-owned key filter as final reports.
	job, _, err = s.Renew(ctx, id, "nas", 7, forged)
	if err != nil || job.View["completionApproval"].(map[string]any)["source"] != "pc-token" {
		t.Fatal("lease更新で受付の根拠が書き換わりました", job, err)
	}
}
