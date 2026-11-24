package jobs

import (
	"strings"
	"testing"
)

// A from-base generation (explicit or dispatch:artifact_unknown) reports a new
// branch and head without a PR. The previous PR belongs to the old branch, so
// it must not be saved with the new tuple or handed to the next worker.
func TestPostgresRetryFromBaseDoesNotCarryPreviousPR(t *testing.T) {
	for _, mode := range []string{"fromBase", "artifactUnknown"} {
		t.Run(mode, func(t *testing.T) {
			db, ctx, s, a, id := legacyArtifactFixture(t, 2, "failed")
			continuation := `{"artifact":null}`
			if mode == "fromBase" {
				continuation = `{"artifact":{"generation":0,"owner":"one","headSha":"` + strings.Repeat("a", 40) + `","branch":"old-branch"}}`
			}
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET pr=7,view=jsonb_build_object('continuation',$2::jsonb) WHERE id=$1`, id, continuation); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Control(ctx, id, ControlInput{ID: "33333333-3333-4333-8333-333333333333", Action: "retry", By: "issue-screen", FromBase: mode == "fromBase"}, ControlOptions{}); err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Next(ctx, "one", 1, a)
			if err != nil || job == nil || !artifactFromBase(job) {
				t.Fatalf("dispatch did not start from base: %+v %v", job, err)
			}
			r := legacyArtifactReport(job.LeaseEpoch, "failed")
			fingerprint := "sha256:" + strings.Repeat("c", 64)
			r.DiffFingerprint = Optional[string]{Present: true, Value: &fingerprint}
			if _, err := s.Report(ctx, id, "one", r); err != nil {
				t.Fatal(err)
			}
			saved, err := s.Get(ctx, id)
			if err != nil || saved.PR != nil || *saved.Branch != "new-branch" {
				t.Fatalf("previous PR stayed with the new branch: %+v %v", saved, err)
			}
			// The fingerprint is bound to the new tuple without the previous PR.
			if bound, _ := saved.View["diffFingerprint"].(map[string]any); bound == nil || bound["value"] != fingerprint || bound["pr"] != nil || bound["branch"] != "new-branch" {
				t.Fatalf("fingerprint lost or bound to the previous PR: %#v", saved.View["diffFingerprint"])
			}
			var detached, created int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE kind='pr_detached' AND data->>'pr'='7' AND data->>'from'='old-branch' AND data->>'to'='new-branch'),
count(*) FILTER (WHERE kind='pr_created') FROM job_events WHERE job_id=$1`, id).Scan(&detached, &created); err != nil || detached != 1 || created != 0 {
				t.Fatalf("history: detached=%d created=%d err=%v", detached, created, err)
			}
			// Resending the same report after the lease ended stays idempotent.
			if _, err := s.Report(ctx, id, "one", r); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Control(ctx, id, ControlInput{ID: "44444444-4444-4444-8444-444444444444", Action: "retry", By: "issue-screen"}, ControlOptions{}); err != nil {
				t.Fatal(err)
			}
			job, _, err = s.Next(ctx, "one", 1, a)
			if err != nil || job == nil || job.PR != nil || *job.Branch != "new-branch" {
				t.Fatalf("next worker received the previous PR: %+v %v", job, err)
			}
		})
	}
}

// Reports that keep the branch, name a PR for the new branch, or set the
// first branch of a job, keep the saved PR.
func TestPostgresRetryPreservesValidPRTuple(t *testing.T) {
	for _, mode := range []string{"sameBranch", "newPR", "noSavedBranch"} {
		t.Run(mode, func(t *testing.T) {
			db, ctx, s, a, id := legacyArtifactFixture(t, 2, "queued")
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET pr=7 WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			// A PR saved before any branch has no branch to contradict.
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET branch=NULL WHERE id=$1 AND $2`, id, mode == "noSavedBranch"); err != nil {
				t.Fatal(err)
			}
			job := takeLegacyArtifact(t, ctx, s, a, id, "one")
			r := legacyArtifactReport(job.LeaseEpoch, "running")
			want := int64(7)
			if mode == "sameBranch" {
				branch := "old-branch"
				r.Branch.Value = &branch
				r.View = nil
			} else if mode == "newPR" {
				want = 9
				r.PR = Optional[int64]{Present: true, Value: &want}
			}
			if _, err := s.Report(ctx, id, "one", r); err != nil {
				t.Fatal(err)
			}
			saved, err := s.Get(ctx, id)
			if err != nil || saved.PR == nil || *saved.PR != want || *saved.Branch != *r.Branch.Value {
				t.Fatalf("valid tuple changed: %+v %v", saved, err)
			}
			var detached int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='pr_detached'`, id).Scan(&detached); err != nil || detached != 0 {
				t.Fatalf("detached=%d err=%v", detached, err)
			}
		})
	}
}
