package jobs

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPostgresDependencyReviewProvenance(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"previous": map[string]any{"relation": "required", "evidence": "受付の根拠", "consultationId": "consult-1", "at": "2026-11-06T00:00:00Z"}}
	for _, existing := range []bool{false, true} {
		name := "absent"
		if existing {
			name = "saved"
		}
		t.Run(name, func(t *testing.T) {
			id := seedJob(t, ctx, db, "owner/repo", "nas", 10+int64(idForBool(existing)), 7)
			if existing {
				raw, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view = view || jsonb_build_object('dependencyReviews', $2::jsonb) WHERE id=$1`, id, string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			check := func(phase string) {
				t.Helper()
				job, err := s.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				got, present := job.View["dependencyReviews"]
				if present != existing || existing && !equivalent(got, record) {
					t.Fatalf("%s: dependencyReviews=%#v present=%v", phase, got, present)
				}
				if job.View["instructions"] != "server" || job.View["phase"] != phase {
					t.Fatalf("%s: view=%#v", phase, job.View)
				}
			}
			payloads := []struct {
				name    string
				value   any
				include bool
			}{
				{"legacy", nil, false},
				{"unrelated", map[string]any{"previous": map[string]any{"relation": "unrelated", "evidence": "fake", "consultationId": "consult-2", "at": "2026-11-06T00:00:00Z"}}, true},
				{"required", map[string]any{"previous": map[string]any{"relation": "required", "evidence": "fake", "consultationId": "consult-2", "at": "2026-11-06T00:00:00Z"}}, true},
				{"null", nil, true}, {"omitted", nil, false},
			}
			for _, p := range payloads {
				view := map[string]any{"phase": p.name, "instructions": "worker", "reviewHolds": float64(2)}
				if p.include {
					view["dependencyReviews"] = p.value
				}
				if _, err := s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", View: view}); err != nil {
					t.Fatalf("%s: %v", p.name, err)
				}
				check(p.name)
			}
			bad := Report{Epoch: 6, Status: "running", View: map[string]any{"dependencyReviews": nil}}
			_, err := s.Report(ctx, id, "nas", bad)
			code(t, err, 409, "lease_lost")
			terminal := Report{Epoch: 7, Status: "failed", View: map[string]any{"phase": "terminal", "dependencyReviews": nil, "reviewHolds": float64(2)}}
			if _, err := s.Report(ctx, id, "nas", terminal); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Report(ctx, id, "nas", terminal); err != nil {
				t.Fatalf("resend: %v", err)
			}
			check("terminal")
		})
	}
}

func idForBool(value bool) int {
	if value {
		return 1
	}
	return 0
}
