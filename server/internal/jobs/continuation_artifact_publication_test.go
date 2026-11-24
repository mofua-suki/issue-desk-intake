package jobs

import (
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func TestPermissionApprovalArtifactPublicationPostgres(t *testing.T) {
	for _, scenario := range []string{"matching", "different-head", "different-generation", "missing-artifact"} {
		t.Run(scenario, func(t *testing.T) {
			db, ctx := testPostgres(t)
			store, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			id := seedJob(t, ctx, db, "owner/repo", "worker", 42, 7)
			head, branch := strings.Repeat("a", 40), "work"
			_, err = store.Report(ctx, id, "worker", Report{Epoch: 7, Status: "running", Branch: Optional[string]{Present: true, Value: &branch}, HeadSHA: Optional[string]{Present: true, Value: &head}})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "different-head":
				_, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{continuation,artifact,headSha}',to_jsonb($2::text)) WHERE id=$1`, id, strings.Repeat("b", 40))
			case "different-generation":
				_, err = db.ExecContext(ctx, `UPDATE jobs SET generation=generation+1 WHERE id=$1`, id)
			case "missing-artifact":
				_, err = db.ExecContext(ctx, `UPDATE jobs SET view=view-'continuation' WHERE id=$1`, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='review_held',pr=12 WHERE id=$1`, id)
			if err != nil {
				t.Fatal(err)
			}
			job, err := store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			operation := "44444444-4444-4444-8444-444444444444"
			identity := map[string]any{"id": operation, "identity": map[string]any{"head": head}, "approvedBy": "screen"}
			hold := map[string]any{"project": "owner/repo", "head": head, "pr": int64(12), "generation": job.Generation, "epoch": int64(7), "files": []any{"host-ssh.json"}}
			job, err = store.PermissionApproval(ctx, id, identity, hold, false)
			if err != nil {
				t.Fatal(err)
			}
			assertNoPublication := func(job *Job) {
				t.Helper()
				if _, exists := fleet.Object(job.View["continuation"])["publication"]; exists {
					t.Fatal("premature publication", job.View)
				}
			}
			assertNoPublication(job)
			if _, err = store.PermissionApproval(ctx, id, identity, hold, true); err == nil {
				t.Fatal("approval without audit succeeded")
			}
			job, err = store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			assertNoPublication(job)
			if _, err = store.PermissionReceipt(ctx, id, operation, map[string]any{"issuePosted": true}); err != nil {
				t.Fatal(err)
			}
			if _, err = store.PermissionApproval(ctx, id, identity, hold, true); err == nil {
				t.Fatal("approval with incomplete audit succeeded")
			}
			job, err = store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			assertNoPublication(job)
			if _, err = store.PermissionReceipt(ctx, id, operation, map[string]any{"prPosted": true}); err != nil {
				t.Fatal(err)
			}
			if _, err = store.PermissionApproval(ctx, id, identity, hold, true); err != nil {
				t.Fatal(err)
			}
			job, err = store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			publication := fleet.Object(fleet.Object(job.View["continuation"])["publication"])
			if scenario != "matching" {
				if len(publication) != 0 {
					t.Fatal("unexpected publication", publication)
				}
				return
			}
			artifact, ok := artifactOf(job)
			if !ok || !artifactPublished(job, artifact) || publication["branch"] != branch || publication["headSha"] != head || job.Status != "queued" {
				t.Fatal("publication not persisted", job.View)
			}
			other := artifact
			other.Head = strings.Repeat("b", 40)
			if artifactPublished(job, other) {
				t.Fatal("publication reused for another head")
			}
			other = artifact
			other.Generation++
			if artifactPublished(job, other) {
				t.Fatal("publication reused for another generation")
			}
		})
	}
}
