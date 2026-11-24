package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/jobs"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// incidentCauses reads a consistent, key-ordered snapshot of the cause ledger.
func incidentCauses(ctx context.Context, s *Service) ([]IncidentCause, error) {
	var result []IncidentCause
	err := s.withIncidentCauses(ctx, false, func(causes map[string]IncidentCause) error {
		for _, c := range causes {
			result = append(result, c)
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
		return nil
	})
	return result, err
}

func TestIncidentRepeatDetection(t *testing.T) {
	s := &Service{PostgreSQL: postgres(t)}
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	holds := []IncidentHold{{"1", now.Add(-6 * time.Hour)}, {"2", now.Add(-time.Hour)}, {"3", now}}
	c, retry, err := s.EvaluateIncidentHolds(ctx, "repo/1:missing", "ref-a", holds[:2], now)
	if err != nil || retry || c.State != "observing" || len(c.Holds) != 2 {
		t.Fatalf("two holds: %+v %v %v", c, retry, err)
	}
	c, retry, err = s.EvaluateIncidentHolds(ctx, c.Key, "ref-a", holds, now)
	if err != nil || !retry || c.State != "detected" || len(c.Holds) != 3 {
		t.Fatalf("three holds: %+v %v %v", c, retry, err)
	}
	c, retry, err = s.EvaluateIncidentHolds(ctx, c.Key, "ref-a", holds, now)
	if err != nil || retry || len(c.Holds) != 3 {
		t.Fatalf("reread: %+v %v %v", c, retry, err)
	}
	c, retry, err = s.EvaluateIncidentHolds(ctx, c.Key, "ref-b", holds, now)
	if err != nil || !retry {
		t.Fatalf("changed prerequisite: %+v %v %v", c, retry, err)
	}
	loaded, err := incidentCauses(ctx, &Service{PostgreSQL: s.PostgreSQL})
	if err != nil || len(loaded) != 1 || loaded[0].Key != c.Key || loaded[0].Prerequisite != "ref-b" || loaded[0].State != "detected" || len(loaded[0].Holds) != 3 {
		t.Fatalf("reload: %+v %v", loaded, err)
	}
	c, retry, err = s.EvaluateIncidentHolds(ctx, c.Key, "ref-b", nil, now.Add(time.Nanosecond))
	if err != nil || retry || c.State != "observing" || len(c.Holds) != 2 {
		t.Fatalf("window expiry: %+v %v %v", c, retry, err)
	}
}

func TestIncidentCauseLedgerIsolation(t *testing.T) {
	s := &Service{PostgreSQL: postgres(t)}
	ctx := context.Background()
	original := `[{"id":"case","deadline":"2026-11-10T12:15:00Z","answer":"wait","notificationReservation":"n","actionReservation":"a"}]`
	if _, err := s.PostgreSQL.ExecContext(ctx, `INSERT INTO settings(key,value,updated_by) VALUES('incident-deadline',$1::jsonb,'test')`, original); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.EvaluateIncidentHolds(ctx, "same", "ref", []IncidentHold{{fmt.Sprint(i), now}}, now)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	causes, err := incidentCauses(ctx, s)
	if err != nil || len(causes) != 1 || causes[0].State != "detected" || len(causes[0].Holds) != 12 {
		t.Fatalf("causes: %+v %v", causes, err)
	}
	var equal bool
	if err := s.PostgreSQL.QueryRowContext(ctx, `SELECT value=$1::jsonb AND updated_by='test' FROM settings WHERE key='incident-deadline'`, original).Scan(&equal); err != nil || !equal {
		t.Fatalf("incident ledger changed: %v %v", equal, err)
	}
}

// Exercise the report producer and the observation reader against the shipped
// PostgreSQL schema, including the missing historical-data path.
func TestIncidentPrerequisiteReportHistory(t *testing.T) {
	db := postgres(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := now.Add(-time.Hour)
	store, err := jobs.New(db, jobs.Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{PostgreSQL: db}
	_, err = db.Exec(`INSERT INTO jobs(id,repo,issue,title,status,hold_reason,generation,view,result) VALUES(1,'owner/repo',1,'wait','queued',NULL,0,'{}','{}'),(2,'owner/prior',2,'producer','queued',NULL,0,'{}','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal([]byte(`{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"成果物が不足","artifacts":[{"artifact":"API","repo":"owner/prior","issue":2}]}}`), &result); err != nil {
		t.Fatal(err)
	}
	reason := "prerequisite_missing"
	for i := int64(1); i <= 3; i++ {
		clock = now.Add(time.Duration(i-3) * time.Minute)
		_, err = db.Exec(`UPDATE jobs SET status='running',hold_reason=NULL,generation=$1,lease_host='nas',lease_epoch=$1,lease_expires_at=$2 WHERE id=1`, i, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.Report(t.Context(), 1, "nas", jobs.Report{Epoch: i, Status: "held", HoldReason: &reason, Result: result}); err != nil {
			t.Fatal(err)
		}
		cause, fresh, err := s.ObservePrerequisiteHolds(t.Context(), 1, now)
		if err != nil || len(cause.Holds) != int(i) || fresh != (i == 3) {
			t.Fatalf("hold %d: %+v fresh=%v err=%v", i, cause, fresh, err)
		}
	}
	detail, err := store.Detail(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range detail["events"].([]any) {
		event := raw.(map[string]any)
		if event["kind"] == "state" {
			fields := event["data"].(map[string]any)
			if _, exists := fields["prerequisiteCause"]; exists {
				t.Fatal("internal cause leaked through job detail")
			}
			if _, exists := fields["generation"]; exists {
				t.Fatal("internal generation leaked through job detail")
			}
		}
	}
	cause, fresh, err := s.ObservePrerequisiteHolds(t.Context(), 1, now)
	if err != nil || fresh || cause.PendingPrerequisite == "" {
		t.Fatalf("durable pending: %+v %v %v", cause, fresh, err)
	}
	old := cause.PendingPrerequisite
	if _, err = db.Exec(`UPDATE jobs SET status='merged',head_sha=$1 WHERE id=2`, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	cause, fresh, err = s.ObservePrerequisiteHolds(t.Context(), 1, now)
	if err != nil || !fresh || cause.PendingPrerequisite == old {
		t.Fatalf("producer changed: %+v %v %v", cause, fresh, err)
	}
	loaded, err := incidentCauses(t.Context(), s)
	if err != nil || len(loaded) != 1 || loaded[0].PendingPrerequisite != cause.PendingPrerequisite {
		t.Fatalf("pending reload: %+v %v", loaded, err)
	}
	_, err = db.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES(1,'state',$1,'{"to":"held","holdReason":"prerequisite_missing"}')`, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ObservePrerequisiteHolds(t.Context(), 1, now); err == nil {
		t.Fatal("incomplete historical cause accepted")
	}
	afterFailure, err := incidentCauses(t.Context(), s)
	if err != nil || len(afterFailure) != 1 || afterFailure[0].State != loaded[0].State || len(afterFailure[0].Holds) != len(loaded[0].Holds) || afterFailure[0].PendingPrerequisite != loaded[0].PendingPrerequisite {
		t.Fatalf("incomplete history changed detection: %+v %v", afterFailure, err)
	}
	if _, err = db.Exec(`UPDATE job_events SET at=$1 WHERE job_id=1 AND kind='state' AND data->>'holdReason'='prerequisite_missing' AND NOT (data ? 'prerequisiteCause')`, now.Add(-6*time.Hour-time.Second)); err != nil {
		t.Fatal(err)
	}
	resumed, fresh, err := s.ObservePrerequisiteHolds(t.Context(), 1, now)
	if err != nil || fresh || resumed.State != "detected" || len(resumed.Holds) != 3 {
		t.Fatalf("expired incomplete history: %+v fresh=%v err=%v", resumed, fresh, err)
	}
}

func TestPrerequisiteHoldCycleReportsIncompleteHistoryAndRecovers(t *testing.T) {
	db := postgres(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := now
	store, err := jobs.New(db, jobs.Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{PostgreSQL: db, Now: func() time.Time { return now }}
	_, err = db.Exec(`INSERT INTO jobs(id,repo,issue,title,status,hold_reason,generation,view,result) VALUES(1,'owner/repo',1,'first','queued',NULL,0,'{}','{}'),(2,'owner/repo',2,'second','queued',NULL,0,'{}','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"status": "failed", "questions": []any{}, "prerequisiteMissing": map[string]any{"reason": "成果物が不足", "artifacts": []any{map[string]any{"artifact": "API", "repo": "owner/prior", "issue": 3}}}}
	reason := "prerequisite_missing"
	for _, id := range []int64{1, 2} {
		for generation := int64(1); generation <= 3; generation++ {
			clock = now.Add(time.Duration(generation-3) * time.Minute)
			_, err = db.Exec(`UPDATE jobs SET status='running',hold_reason=NULL,generation=$2,lease_host='nas',lease_epoch=$2,lease_expires_at=$3 WHERE id=$1`, id, generation, now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Report(t.Context(), id, "nas", jobs.Report{Epoch: generation, Status: "held", HoldReason: &reason, Result: result}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The question form is another supported representation of a prerequisite hold.
	if _, err = db.Exec(`UPDATE jobs SET hold_reason='question' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES(1,'state',$1,'{"to":"held","holdReason":"prerequisite_missing"}')`, now.Add(-6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = s.ObservePrerequisiteHoldCycle(t.Context()); err == nil || !strings.Contains(err.Error(), "job 1: prerequisite hold history is incomplete") {
		t.Fatalf("cycle did not report incomplete history: %v", err)
	}
	causes, err := incidentCauses(t.Context(), s)
	if err != nil || len(causes) != 1 || !strings.HasPrefix(causes[0].Key, "prerequisite:2:") || causes[0].State != "detected" || causes[0].PendingPrerequisite == "" {
		t.Fatalf("other job was not observed: %+v %v", causes, err)
	}
	if _, err = db.Exec(`UPDATE job_events SET at=$1 WHERE job_id=1 AND kind='state' AND data->>'holdReason'='prerequisite_missing' AND NOT (data ? 'prerequisiteCause')`, now.Add(-6*time.Hour-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = s.ObservePrerequisiteHoldCycle(t.Context()); err != nil {
		t.Fatalf("cycle did not recover: %v", err)
	}
	causes, err = incidentCauses(t.Context(), s)
	if err != nil || len(causes) != 2 || causes[0].State != "detected" || causes[0].PendingPrerequisite == "" || causes[1].State != "detected" || causes[1].PendingPrerequisite == "" {
		t.Fatalf("recovered observations: %+v %v", causes, err)
	}
	now = now.Add(7 * time.Hour)
	if err = s.ObservePrerequisiteHoldCycle(t.Context()); err != nil {
		t.Fatalf("long-lived holds: %v", err)
	}
	causes, err = incidentCauses(t.Context(), s)
	if err != nil || len(causes) != 2 || causes[0].State != "observing" || len(causes[0].Holds) != 0 || causes[1].State != "observing" || len(causes[1].Holds) != 0 {
		t.Fatalf("expired holds: %+v %v", causes, err)
	}
}
