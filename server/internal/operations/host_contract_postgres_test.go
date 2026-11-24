package operations

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

func TestPostgresCLISelfcheckAndCycleContract(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	ctx := context.Background()
	stamp := float64(s.now().UnixMilli())
	checks := map[string]any{"at": iso(s.now()), "ok": false, "items": []any{map[string]any{"name": "docker", "label": "Docker", "ok": false, "detail": "daemon unavailable"}}}
	cli := []any{map[string]any{"cli": "codex", "current": "1.0.0", "expected": "2.0.0", "at": stamp}}
	cycle := map[string]any{"result": "failed", "stage": "docker", "exceptionType": nil, "reason": "http_403", "httpStatus": float64(403), "detail": "rejected", "at": iso(s.now()), "receivedAt": stamp, "firstFailureAt": stamp - 31*60000, "lastSuccessAt": nil, "failures": float64(3)}
	raw := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if _, e := s.PostgreSQL.ExecContext(ctx, `INSERT INTO hosts(host,heartbeat_at,ready,checks,cli_versions,hostd_health)VALUES('pc',$1,false,$2,$3,$4)`, s.now(), raw(checks), raw(cli), raw(map[string]any{"cycle": cycle})); e != nil {
		t.Fatal(e)
	}
	alerts, e := s.cliAndChecks(ctx)
	if e != nil || len(alerts) != 2 {
		t.Fatal(alerts, e)
	}
	for _, v := range alerts {
		row := fleet.Object(v)
		if row["id"] == "host-check-pc" && !strings.Contains(fleet.Text(row["detail"]), "Docker: daemon unavailable") {
			t.Fatal(row)
		}
	}
	if _, e = s.PostgreSQL.ExecContext(ctx, `UPDATE hosts SET checks=$1 WHERE host='pc'`, raw(map[string]any{"ok": true, "items": []any{map[string]any{"name": "docker", "ok": true}}})); e != nil {
		t.Fatal(e)
	}
	alerts, e = s.cliAndChecks(ctx)
	if e != nil || len(alerts) != 1 || fleet.Object(alerts[0])["id"] == "host-check-pc" {
		t.Fatal(alerts, e)
	}
	alerts, e = s.hostAlerts(ctx)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range alerts {
		r := fleet.Object(v)
		if r["id"] == "hostd-stalled-pc" {
			found = true
			if !strings.Contains(fleet.Text(r["detail"]), "http_403（HTTP 403）") {
				t.Fatal(r)
			}
		}
	}
	if !found {
		t.Fatal(alerts)
	}
	if _, e = s.PostgreSQL.ExecContext(ctx, "UPDATE hosts SET ready=true,checks='{}',cli_versions='[]',hostd_health='{}',heartbeat_at=$1", s.now().Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	alerts, e = s.cliAndChecks(ctx)
	if e != nil || len(alerts) != 0 {
		t.Fatal(alerts, e)
	}
}
