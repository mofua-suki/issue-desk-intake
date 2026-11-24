package jobs

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPostgresReportRejectionRecordedOnce(t *testing.T) {
	db, ctx, s, a, id := legacyArtifactFixture(t, 2, "queued")
	job := takeLegacyArtifact(t, ctx, s, a, id, "one")
	for i := 0; i < 3; i++ {
		_, err := s.Report(ctx, id, "two", Report{Epoch: job.LeaseEpoch, Status: "running"})
		code(t, err, 409, "lease_lost")
	}
	var count int
	var status, epoch int64
	var host string
	err := db.QueryRowContext(ctx, `SELECT count(*), min(host), min((data->>'status')::int), min((data->>'epoch')::bigint) FROM job_events WHERE job_id=$1 AND kind='report_rejected' AND data->>'code'='lease_lost'`, id).Scan(&count, &host, &status, &epoch)
	if err != nil || count != 1 || host != "two" || status != 409 || epoch != job.LeaseEpoch {
		t.Fatalf("rejection count=%d host=%s status=%d epoch=%d err=%v", count, host, status, epoch, err)
	}
}

func TestPostgresReportRejectionConcurrentAndLatest(t *testing.T) {
	db, ctx, s, a, id := legacyArtifactFixture(t, 2, "queued")
	job := takeLegacyArtifact(t, ctx, s, a, id, "one")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Report(ctx, id, "two", Report{Epoch: job.LeaseEpoch, Status: "running"})
			if err == nil {
				t.Error("expected rejection")
			}
		}()
	}
	wg.Wait()
	var originalSeq int64
	if err := db.QueryRowContext(ctx, `SELECT seq FROM job_events WHERE job_id=$1 AND kind='report_rejected' AND data->>'code'='lease_lost'`, id).Scan(&originalSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,data) SELECT $1,'leased','{}' FROM generate_series(1,25)`, id); err != nil {
		t.Fatal(err)
	}
	latest := now.Add(time.Minute)
	s.options.Now = func() time.Time { return latest }
	_, err := s.Report(ctx, id, "three", Report{Epoch: job.LeaseEpoch + 1, Status: "running"})
	code(t, err, 409, "lease_lost")
	var count int
	var seq, epoch int64
	var host string
	var at time.Time
	if err = db.QueryRowContext(ctx, `SELECT count(*), min(seq), min(host), min(at), min((data->>'epoch')::bigint) FROM job_events WHERE job_id=$1 AND kind='report_rejected' AND data->>'code'='lease_lost'`, id).Scan(&count, &seq, &host, &at, &epoch); err != nil || count != 1 || seq != originalSeq || host != "three" || !at.Equal(latest) || epoch != job.LeaseEpoch+1 {
		t.Fatalf("count=%d seq=%d host=%s at=%s epoch=%d err=%v", count, seq, host, at, epoch, err)
	}
}

func TestPostgresReportRejectionValidationCodes(t *testing.T) {
	db, ctx, s, a, id := legacyArtifactFixture(t, 2, "queued")
	job := takeLegacyArtifact(t, ctx, s, a, id, "one")
	other := "two"
	_, err := s.Report(ctx, id, "one", Report{Epoch: job.LeaseEpoch, Status: "invalid"})
	code(t, err, 400, "REQUEST_REJECTED")
	_, err = s.Report(ctx, id, "one", Report{Epoch: job.LeaseEpoch, Status: "running", Host: Optional[string]{Present: true, Value: &other}})
	code(t, err, 403, "REQUEST_REJECTED")
	_, err = s.Report(ctx, id, "two", Report{Epoch: job.LeaseEpoch, Status: "running"})
	code(t, err, 409, "lease_lost")
	var count int
	var status int
	if err = db.QueryRowContext(ctx, `SELECT count(*), min((data->>'status')::int) FROM job_events WHERE job_id=$1 AND kind='report_rejected' AND data->>'code'='REQUEST_REJECTED'`, id).Scan(&count, &status); err != nil || count != 1 || status != 403 {
		t.Fatalf("latest validation rejection: count=%d status=%d err=%v", count, status, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='report_rejected'`, id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("distinct codes: count=%d err=%v", count, err)
	}
}

// This is a synthetic release failure, not the unknown payload from issue 5584.
func TestPostgresReleaseFailureRejectionRecorded(t *testing.T) {
	db, ctx, s, _, id := legacyArtifactFixture(t, 2, "running")
	_, err := db.ExecContext(ctx, `UPDATE jobs SET lease_host='two', lease_expires_at=$2, pr=1257, view='{"releasePending":true,"lastStage":"releasing","continuation":{"artifact":null}}' WHERE id=$1`, id, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	report := Report{Epoch: 2, Status: "failed", HeadSHA: Optional[string]{Present: true, Value: &head}, View: map[string]any{"lastExecutionError": map[string]any{"kind": "execution_error", "stage": "merge", "reason": "レビュー対象とPRの先頭コミットが一致しません"}}, Result: map[string]any{"status": "failed"}}
	for i := 0; i < 3; i++ {
		_, err = s.Report(ctx, id, "two", report)
		code(t, err, 409, "artifact_unavailable")
	}
	var count int
	var status int
	var epoch int64
	err = db.QueryRowContext(ctx, `SELECT count(*), min((data->>'status')::int), min((data->>'epoch')::bigint) FROM job_events WHERE job_id=$1 AND kind='report_rejected' AND data->>'code'='artifact_unavailable'`, id).Scan(&count, &status, &epoch)
	if err != nil || count != 1 || status != 409 || epoch != 2 {
		t.Fatalf("count=%d status=%d epoch=%d err=%v", count, status, epoch, err)
	}
	saved, err := s.Get(ctx, id)
	if err != nil || saved.Status != "running" {
		t.Fatalf("rejected report persisted: %+v %v", saved, err)
	}
}

func TestPostgresReportStateTransitionMetadata(t *testing.T) {
	cases := []struct {
		name, view            string
		phase                 any
		present, prerequisite bool
	}{
		{"missing", `{}`, nil, false, false},
		{"null", `{"phase":null}`, nil, true, false},
		{"empty", `{"phase":""}`, "", true, false},
		{"normal", `{"phase":"implementation"}`, "implementation", true, false},
		{"prerequisite", `{"phase":"review"}`, "review", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			s, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			id := seedJob(t, ctx, db, "owner/repo", "one", 1, 2)
			_, err = db.ExecContext(ctx, `UPDATE jobs SET generation=7,view=$2::jsonb WHERE id=$1`, id, tc.view)
			if err != nil {
				t.Fatal(err)
			}
			report := Report{Epoch: 2, Status: "failed", View: map[string]any{"phase": "after"}, Result: map[string]any{"status": "failed"}}
			var wantHold any
			if tc.prerequisite {
				reason := "prerequisite_missing"
				report.Status = "held"
				report.HoldReason = &reason
				wantHold = reason
				report.Result["questions"] = []any{}
				report.Result["prerequisiteMissing"] = map[string]any{"reason": "needed", "artifacts": []any{map[string]any{"artifact": "API", "repo": "owner/repo", "issue": float64(1)}}}
			}
			saved, err := s.Report(ctx, id, "one", report)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantJobHold, endReason := "failed", "", "unknown"
			if tc.prerequisite {
				wantStatus, wantJobHold, endReason = "held", "prerequisite_missing", "prerequisite"
			}
			if saved.Status != wantStatus || (wantJobHold != "" && (saved.HoldReason == nil || *saved.HoldReason != wantJobHold)) || saved.View["phase"] != "after" {
				t.Fatalf("saved job: %+v", saved)
			}
			var raw []byte
			var host string
			var count int
			err = db.QueryRowContext(ctx, `SELECT data,host FROM job_events WHERE job_id=$1 AND kind='state'`, id).Scan(&raw, &host)
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err = json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			phase, present := event["phase"]
			end, ok := event["implementationEnd"].(map[string]any)
			if host != "one" || event["leaseHost"] != "one" || event["generation"] != float64(7) || event["epoch"] != float64(2) || event["to"] != report.Status || event["holdReason"] != wantHold || present != tc.present || phase != tc.phase || !ok || end["generation"] != float64(7) || end["reason"] != endReason {
				t.Fatalf("event=%v host=%s", event, host)
			}
			if tc.prerequisite {
				fingerprint, err := PrerequisiteFingerprint(report.Result["prerequisiteMissing"])
				if err != nil {
					t.Fatal(err)
				}
				if event["prerequisiteCause"] != fingerprint {
					t.Fatalf("cause: %v want %v", event["prerequisiteCause"], fingerprint)
				}
			} else if _, ok := event["prerequisiteCause"]; ok {
				t.Fatalf("unexpected cause: %v", event)
			}
			if _, err = s.Report(ctx, id, "one", report); err != nil {
				t.Fatalf("resend: %v", err)
			}
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'epoch'=$2 AND data->>'to'=$3`, id, "2", report.Status).Scan(&count); err != nil || count != 1 {
				t.Fatalf("receipt count=%d err=%v", count, err)
			}
		})
	}
}

func TestPostgresReportPhaseTransitionMetadata(t *testing.T) {
	db, ctx := testPostgres(t)
	at := now
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	id := seedJob(t, ctx, db, "owner/repo", "one", 1, 2)
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET generation=7,view='{}'::jsonb WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name     string
		view     map[string]any
		from, to any
		change   bool
	}{
		{"initial", map[string]any{"phase": "A"}, nil, "A", true},
		{"change", map[string]any{"phase": "B"}, "A", "B", true},
		{"same", map[string]any{"phase": "B"}, "B", "B", false},
		{"omitted", map[string]any{"other": true}, "B", "B", false},
		{"null", map[string]any{"phase": nil}, "B", nil, true},
		{"same null", map[string]any{"phase": nil}, nil, nil, false},
		{"empty", map[string]any{"phase": ""}, nil, "", true},
		{"same empty", map[string]any{"phase": ""}, "", "", false},
		{"back to null", map[string]any{"phase": nil}, "", nil, true},
	}
	count := 0
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			at = at.Add(time.Second)
			saved, err := s.Report(ctx, id, "one", Report{Epoch: 2, Status: "running", View: step.view})
			if err != nil {
				t.Fatal(err)
			}
			value, present := saved.View["phase"]
			if !present || value != step.to {
				t.Fatalf("saved phase=%v present=%v, want %v", value, present, step.to)
			}
			if step.change {
				count++
			}
			var actual int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='phase'`, id).Scan(&actual); err != nil || actual != count {
				t.Fatalf("phase count=%d want=%d err=%v", actual, count, err)
			}
			if !step.change {
				return
			}
			var raw []byte
			var host string
			var eventAt, updatedAt time.Time
			if err := db.QueryRowContext(ctx, `SELECT e.data,e.host,e.at,j.updated_at FROM job_events e JOIN jobs j ON j.id=e.job_id WHERE e.job_id=$1 AND e.kind='phase' ORDER BY e.seq DESC LIMIT 1`, id).Scan(&raw, &host, &eventAt, &updatedAt); err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			from, fromPresent := data["from"]
			to, toPresent := data["to"]
			if !fromPresent || !toPresent || from != step.from || to != step.to || data["leaseHost"] != "one" || data["generation"] != float64(7) || data["epoch"] != float64(2) || host != "one" || !eventAt.Equal(at) || !eventAt.Equal(updatedAt) || len(data) != 5 {
				t.Fatalf("data=%v host=%s at=%v updated=%v", data, host, eventAt, updatedAt)
			}
		})
	}
	leave := Report{Epoch: 2, Status: "failed", View: map[string]any{"phase": "after"}, Result: map[string]any{"status": "failed"}}
	for i := 0; i < 2; i++ {
		if _, err := s.Report(ctx, id, "one", leave); err != nil {
			t.Fatalf("leave %d: %v", i, err)
		}
	}
	var phases, states int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE kind='phase'),count(*) FILTER (WHERE kind='state') FROM job_events WHERE job_id=$1`, id).Scan(&phases, &states); err != nil || phases != count || states != 1 {
		t.Fatalf("after leave phases=%d want=%d states=%d err=%v", phases, count, states, err)
	}
}

func TestReportPreservesServerDependencyMerges(t *testing.T) {
	db, ctx, s, a, id := legacyArtifactFixture(t, 2, "queued")
	job := takeLegacyArtifact(t, ctx, s, a, id, "one")
	const evidence = `{"1":{"pr":12,"mergeCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{dependencyMerges}',$2::jsonb) WHERE id=$1`, id, evidence); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Report(ctx, id, "one", Report{Epoch: job.LeaseEpoch, Status: "running", View: map[string]any{"dependencyMerges": map[string]any{}, "phase": "implementation"}})
	if err != nil {
		t.Fatal(err)
	}
	merges := saved.View["dependencyMerges"].(map[string]any)
	if merges["1"] == nil || saved.View["phase"] != "implementation" {
		t.Fatalf("view=%v", saved.View)
	}
	var actual string
	if err := db.QueryRowContext(ctx, `SELECT view->'dependencyMerges'->'1'->>'mergeCommit' FROM jobs WHERE id=$1`, id).Scan(&actual); err != nil || actual != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("commit=%q err=%v", actual, err)
	}
}
