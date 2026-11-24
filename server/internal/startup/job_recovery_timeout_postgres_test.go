package startup

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"issue-desk-intake/server/internal/operations"
)

// A stale actor must not send after another cause has taken over an expired reservation.
func TestRecoveryCycleExpiredActorDoesNotSendPostgres(t *testing.T) {
	ctx := context.Background()
	db := appPostgres(t)
	appDB := newAppStore(t).DB()
	now := time.Now().UTC()
	s := &operations.Service{DB: appDB, PostgreSQL: db, Now: func() time.Time { return now }, Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
	_, err := db.Exec(`INSERT INTO jobs(repo,issue,title,status,result,updated_at) VALUES('owner/repo',1,'title','failed','{"error":"failure"}',now()-interval '1 hour')`)
	if err != nil {
		t.Fatal(err)
	}
	r := &operations.JobRecovery{Service: s, UsageLimit: 100, Limit: 3, ReservationTimeout: time.Hour}
	var first, second operations.RecoveryCandidate
	sends := 0
	actions := &operations.RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": 1, "title": "title", "body": "body", "status": "failed", "state": "open", "comments": []any{}}, nil
	}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
	actions.Judge = func(ctx context.Context, items []any) (map[string]any, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Before(time.Now().Add(r.ReservationTTL())) {
			t.Fatal("Act deadline is not shorter than reservation", deadline, ok)
		}
		now = now.Add(r.ReservationTTL())
		second = first
		second.Cause = "different-cause"
		decision, _, err := r.Reserve(context.Background(), second)
		if err != nil || decision != "proceed" {
			t.Fatal(decision, err)
		}
		item := items[0].(map[string]any)
		return map[string]any{"actions": []any{map[string]any{"action": "retry", "item": item["key"], "reason": "reason", "text": "instruction", "evidence": "evidence", "resolved": true, "requiresUser": false}}}, nil
	}
	err = runRecoveryCycle(ctx, func(context.Context) error { return nil }, func(ctx context.Context) ([]operations.RecoveryCandidate, error) {
		candidates, e := r.Candidates(ctx)
		if e != nil || len(candidates) != 1 {
			t.Fatalf("candidates %v: %v", candidates, e)
		}
		first = candidates[0]
		return candidates, nil
	}, boundedRecoveryAct(r, actions.Act))
	if err == nil || sends != 0 {
		t.Fatalf("stale actor sent: %d, err=%v", sends, err)
	}
	raw, err := s.OperationalSetting(ctx, "job-recovery")
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var rows []operations.RecoveryRecord
	if err = json.Unmarshal(bytes, &rows); err != nil || len(rows) != 1 {
		t.Fatal(err, string(bytes))
	}
	if rows[0].Causes[first.Cause].State != "send_unknown" || rows[0].Causes[second.Cause].State != "reserved" {
		t.Fatal(rows)
	}
}

func TestRecoveryCycleCanceledJudgeDoesNotSendPostgres(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := appPostgres(t)
	appDB := newAppStore(t).DB()
	s := &operations.Service{DB: appDB, PostgreSQL: db, Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
	_, err := db.Exec(`INSERT INTO jobs(repo,issue,title,status,result,updated_at) VALUES('owner/repo',1,'title','failed','{"error":"failure"}',now()-interval '1 hour')`)
	if err != nil {
		t.Fatal(err)
	}
	r := &operations.JobRecovery{Service: s, UsageLimit: 100, Limit: 3, ReservationTimeout: time.Hour}
	sends := 0
	a := &operations.RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": 1, "title": "title", "body": "body", "status": "failed", "state": "open", "comments": []any{}}, nil
	}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
	a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
		cancel()
		item := items[0].(map[string]any)
		return map[string]any{"actions": []any{map[string]any{"action": "retry", "item": item["key"], "reason": "reason", "text": "instruction", "evidence": "evidence", "resolved": true, "requiresUser": false}}}, nil
	}
	err = runRecoveryCycle(ctx, func(context.Context) error { return nil }, r.Candidates, boundedRecoveryAct(r, a.Act))
	if !errors.Is(err, context.Canceled) || sends != 0 {
		t.Fatalf("canceled actor sent: %d, err=%v", sends, err)
	}
}
