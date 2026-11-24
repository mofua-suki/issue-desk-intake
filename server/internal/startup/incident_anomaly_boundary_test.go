package startup

import (
	"context"
	"testing"
	"time"

	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
)

// Exercise the recovery cycle and deadline monitor with the same target, using
// the production stores and isolated databases.
func TestIncidentRecoveryCycleBoundaryPostgres(t *testing.T) {
	ctx := context.Background()
	db := appPostgres(t)
	appDB := newAppStore(t).DB()
	now := time.Now().UTC()
	s := &operations.Service{DB: appDB, PostgreSQL: db, Now: func() time.Time { return now }, Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,title,status,result,updated_at) VALUES('owner/repo',1,'title','failed','{"error":"failure"}',now()-interval '1 hour')`)
	if err != nil {
		t.Fatal(err)
	}
	store, err := jobs.New(db, jobs.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	r := &operations.JobRecovery{Service: s, UsageLimit: 100, Limit: 3}
	resolver := &operations.PrerequisiteResolver{Service: s, Store: store}
	sends := 0
	read := func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": 1, "title": "title", "body": "body", "status": "failed", "state": "open", "error": "failure", "comments": []any{}}, nil
	}
	actions := &operations.RecoveryActions{Recovery: r, ReadTarget: read, Judge: func(_ context.Context, items []any) (map[string]any, error) {
		row := items[0].(map[string]any)
		return map[string]any{"actions": []any{map[string]any{"action": "retry", "item": row["key"], "reason": "reason", "text": "instruction", "evidence": "evidence", "resolved": true, "requiresUser": false}}}, nil
	}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
	source := []operations.IncidentSource{{Key: "stall-job-dispatch-stall-1", Kind: "stall", Title: "stalled", Target: map[string]any{"project": "owner/repo", "number": float64(1)}}}
	deadlineJudges := 0
	monitor := operations.IncidentMonitor{Service: s, Sources: func(context.Context) ([]operations.IncidentSource, error) { return source, nil }, Actions: &operations.IncidentActions{Service: s, ReadTarget: read, Judge: func(context.Context, []any) (map[string]any, error) {
		deadlineJudges++
		return map[string]any{"actions": []any{}}, nil
	}, Send: func(context.Context, string, int64, string, string, string) error {
		t.Error("unauthorized deadline action")
		return nil
	}}}
	for i := 0; i < 2; i++ {
		if err = monitor.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if err = runRecoveryCycle(ctx, resolver.Resolve, r.Candidates, actions.Act); err != nil {
			t.Fatal(err)
		}
		now = now.Add(15 * time.Minute)
	}
	if sends != 1 || deadlineJudges != 1 {
		t.Fatalf("duplicate sends=%d deadline judgments=%d", sends, deadlineJudges)
	}
}
