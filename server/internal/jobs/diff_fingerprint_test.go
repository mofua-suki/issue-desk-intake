package jobs

import (
	"strings"
	"testing"
	"time"
)

func TestStateDiffFingerprintBinding(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "one", 1, 1)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	head, branch, fp := strings.Repeat("a", 40), "work", "sha256:"+strings.Repeat("b", 64)
	pr := int64(1)
	report := Report{Epoch: 1, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &head}, Branch: Optional[string]{Present: true, Value: &branch}, PR: Optional[int64]{Present: true, Value: &pr}, DiffFingerprint: Optional[string]{Present: true, Value: &fp}}
	saved, err := s.Report(ctx, id, "one", report)
	if err != nil {
		t.Fatal(err)
	}
	binding := saved.View["diffFingerprint"].(map[string]any)
	if binding["value"] != fp || binding["headSha"] != head || binding["branch"] != branch || binding["pr"] != float64(pr) || binding["generation"] != float64(saved.Generation) || binding["jobId"] != float64(id) {
		t.Fatal(binding)
	}
	for _, key := range []string{"jobId", "generation", "branch", "pr", "headSha"} {
		if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view, '{diffFingerprint}', (view->'diffFingerprint') - $2::text) WHERE id=$1`, id, key); err != nil {
			t.Fatal(err)
		}
		read, err := s.Get(ctx, id)
		if err != nil || read.View["diffFingerprint"] != nil {
			t.Fatal("missing target", key, read, err)
		}
		if _, err = s.Report(ctx, id, "one", report); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.Report(ctx, id, "two", report)
	code(t, err, 409, "lease_lost")
	// Every component of the target must invalidate a missing fingerprint.
	for _, change := range []string{"branch", "pr", "generation"} {
		report.DiffFingerprint = Optional[string]{Present: true, Value: &fp}
		if _, err = s.Report(ctx, id, "one", report); err != nil {
			t.Fatal(change, err)
		}
		report.DiffFingerprint = Optional[string]{}
		switch change {
		case "branch":
			branch = "other"
		case "pr":
			pr = 2
		case "generation":
			if _, err = db.ExecContext(ctx, `UPDATE jobs SET generation=generation+1 WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			// Before another report, every ordinary job reader must reject the stale binding.
			stale, err := s.Get(ctx, id)
			if err != nil || stale.View["diffFingerprint"] != nil {
				t.Fatal("get stale generation", stale, err)
			}
			listed, _, err := s.List(ctx, "owner/repo", nil, 10, 0)
			if err != nil || len(listed) != 1 || listed[0].View["diffFingerprint"] != nil {
				t.Fatal("list stale generation", listed, err)
			}
			batch, err := s.GetBatch(ctx, []int64{id})
			if err != nil || batch[id] == nil || batch[id].View["diffFingerprint"] != nil {
				t.Fatal("batch stale generation", batch, err)
			}
		}
		saved, err = s.Report(ctx, id, "one", report)
		if err != nil || saved.View["diffFingerprint"] != nil {
			t.Fatal(change, saved, err)
		}
	}
	report.DiffFingerprint = Optional[string]{Present: true, Value: &fp}
	if _, err = s.Report(ctx, id, "one", report); err != nil {
		t.Fatal(err)
	}
	report.DiffFingerprint = Optional[string]{}
	report.View = map[string]any{"diffFingerprint": map[string]any{"value": "forged"}, "finalApprovalTransfers": []any{"forged"}}
	saved, err = s.Report(ctx, id, "one", report)
	if err != nil || saved.View["diffFingerprint"].(map[string]any)["value"] != fp || saved.View["finalApprovalTransfers"] != nil {
		t.Fatal(saved, err)
	}
	_, _, err = s.Renew(ctx, id, "one", 1, map[string]any{"diffFingerprint": "forged"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = s.Get(ctx, id)
	if err != nil || saved.View["diffFingerprint"].(map[string]any)["value"] != fp {
		t.Fatal(saved, err)
	}
	head = strings.Repeat("c", 40)
	saved, err = s.Report(ctx, id, "one", report)
	if err != nil || saved.View["diffFingerprint"] != nil {
		t.Fatal(saved, err)
	}
	report.DiffFingerprint = Optional[string]{Present: true, Value: &fp}
	if _, err = s.Report(ctx, id, "one", report); err != nil {
		t.Fatal(err)
	}
	report.DiffFingerprint.Value = nil
	saved, err = s.Report(ctx, id, "one", report)
	if err != nil || saved.View["diffFingerprint"] != nil {
		t.Fatal(saved, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id=$1 AND view->'diffFingerprint' = 'null'::jsonb`, id).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
}
