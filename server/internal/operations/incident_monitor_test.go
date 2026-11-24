package operations

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"testing"
	"time"
)

func TestPostgresIncidentMonitorReservesNotificationAndExpiresOnce(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	ctx := context.Background()
	at := s.now()
	s.Now = func() time.Time { return at }
	sources := []IncidentSource{{Key: "case", Kind: "stall", Title: "stop", Target: map[string]any{"project": "owner/repo", "number": float64(1)}}}
	notifications, judgments, sends := 0, 0, 0
	read := func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": float64(1), "status": "failed", "state": "open", "error": "fixed cause", "stopped": false}, nil
	}
	actions := &IncidentActions{Service: s, ReadTarget: read, Judge: func(context.Context, []any) (map[string]any, error) {
		judgments++
		return map[string]any{"actions": []any{map[string]any{"action": "retry", "item": "case", "reason": "cause fixed", "text": "修正済みのため再試行します", "evidence": "修正が実検証済み", "resolved": true, "requiresUser": false}, map[string]any{"action": "retry", "item": "case", "reason": "again", "text": "修正済みのため再試行します", "evidence": "根拠", "resolved": true, "requiresUser": false}}}, nil
	}, Send: func(context.Context, string, int64, string, string, string) error {
		sends++
		return errors.New("response lost")
	}}
	monitor := IncidentMonitor{Service: s, Sources: func(context.Context) ([]IncidentSource, error) { return sources, nil }, Actions: actions, Notify: func(context.Context, map[string]any) (map[string]any, error) {
		notifications++
		return nil, errors.New("offline")
	}}
	if e := monitor.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	if e := monitor.Reconcile(ctx); e != nil || notifications != 1 {
		t.Fatalf("notification retried %d %v", notifications, e)
	}
	at = at.Add(15 * time.Minute)
	if e := monitor.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	restarted := IncidentMonitor{Service: s, Sources: monitor.Sources, Notify: monitor.Notify, Actions: actions}
	if e := restarted.Reconcile(ctx); e != nil || judgments != 1 || sends != 1 {
		t.Fatalf("action retried judge=%d send=%d %v", judgments, sends, e)
	}
	value, e := s.OperationalSetting(ctx, "incident-deadline")
	if e != nil {
		t.Fatal(e)
	}
	row := fleet.Object(fleet.List(value)[0])
	if row["state"] != "acted" || len(fleet.List(row["actions"])) != 1 {
		t.Fatal(row)
	}
	sources = nil
	if e = monitor.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	value, _ = s.OperationalSetting(ctx, "incident-deadline")
	row = fleet.Object(fleet.List(value)[0])
	if row["endedAt"] == nil || row["acknowledgedAt"] == nil {
		t.Fatal("resolved action remains active", row)
	}
}
func TestPostgresIncidentRejectsNewCauseStoppedForeignAndForbiddenActions(t *testing.T) {
	for _, mode := range []string{"cause_changed", "stopped", "foreign", "forbidden", "repair", "requires_user"} {
		t.Run(mode, func(t *testing.T) {
			s := fixture(t)
			s.PostgreSQL = postgres(t)
			ctx := context.Background()
			id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			if e := s.mutateIncidents(ctx, func(rows *[]any) error {
				*rows = []any{map[string]any{"id": id, "key": "case", "state": "expired", "target": map[string]any{"project": "owner/repo", "number": float64(1)}, "history": []any{}}}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			reads, sends := 0, 0
			actions := IncidentActions{Service: s, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
				reads++
				cause := "cause"
				if mode == "cause_changed" && reads > 1 {
					cause = "changed"
				}
				return map[string]any{"project": "owner/repo", "number": float64(1), "state": "open", "status": "failed", "error": cause, "stopped": mode == "stopped"}, nil
			}, Judge: func(context.Context, []any) (map[string]any, error) {
				name, text := "retry", "根拠を確認して再試行"
				if mode == "foreign" {
					text = "owner/other#2 も再試行"
				}
				if mode == "forbidden" {
					name = "close"
				}
				if mode == "repair" {
					name = "repair"
				}
				return map[string]any{"actions": []any{map[string]any{"action": name, "item": "case", "reason": "reason", "text": text, "evidence": "verified", "resolved": true, "requiresUser": mode == "requires_user"}}}, nil
			}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
			if e := actions.Execute(ctx, id); e != nil {
				t.Fatal(e)
			}
			if sends != 0 {
				t.Fatal("unsafe action sent")
			}
		})
	}
}
