package startup

import (
	"context"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
)

func TestPrerequisiteObservationPeriodicFailurePostgres(t *testing.T) {
	a := reviewNotesApp(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store, err := jobs.New(a.DB, jobs.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.DB.Exec(`INSERT INTO jobs(id,repo,issue,title,status,generation,lease_host,lease_epoch,lease_expires_at,view,result) VALUES(95001,'owner/repo',95001,'wait','running',1,'nas',1,$1,'{}','{}')`, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	reason := "prerequisite_missing"
	result := map[string]any{"status": "failed", "questions": []any{}, "prerequisiteMissing": map[string]any{"reason": "成果物が不足", "artifacts": []any{map[string]any{"artifact": "API", "repo": "owner/prior", "issue": 2}}}}
	if _, err = store.Report(t.Context(), 95001, "nas", jobs.Report{Epoch: 1, Status: "held", HoldReason: &reason, Result: result}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES(95001,'state',$1,'{"to":"held","holdReason":"prerequisite_missing"}')`, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	s := &operations.Service{PostgreSQL: a.DB, Now: func() time.Time { return now }}
	a.Operations = func() (*operations.Service, error) { return s, nil }
	a.deferBackground = true
	logs := make(chan string, 1)
	a.Log = func(message string) { logs <- message }
	if err = a.startOperations(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { a.backgroundPending[2](ctx); close(done) }()
	select {
	case message := <-logs:
		if !strings.Contains(message, "周期処理（prerequisite-hold-observation）を完了できません") || !strings.Contains(message, "prerequisite hold history is incomplete") {
			t.Errorf("failure log: %s", message)
		}
	case <-time.After(10 * time.Second):
		t.Error("periodic failure was not logged")
	}
	cancel()
	<-done
	var ledger int
	if err = a.DB.QueryRowContext(t.Context(), `SELECT count(*) FROM settings WHERE key='incident-causes' AND value <> '{}'::jsonb`).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("failed observation changed ledger: %d %v", ledger, err)
	}
}
