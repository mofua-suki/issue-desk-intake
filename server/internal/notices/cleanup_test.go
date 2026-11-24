package notices

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"testing"
)

func TestCleanupKeepsChangedAndUnreadableNoticesAndArchivesAtomically(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	first := map[string]any{"id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "title": "old", "issues": []any{map[string]any{"repo": "owner/repo", "number": 1}}, "updatedAt": "2026-10-31T00:00:00.000Z"}
	expired := map[string]any{"id": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "issues": []any{}, "updatedAt": "2026-10-01T00:00:00.000Z"}
	raw, _ := json.Marshal([]any{first, expired})
	s.DB.Exec("INSERT INTO settings(id,json)VALUES('ops-watch-notices',?)", string(raw))
	cleanup := Cleanup{Service: s, ReadIssue: func(context.Context, string, int64) (map[string]any, error) {
		first["title"] = "new"
		changed, _ := json.Marshal([]any{first, expired})
		if _, e := s.DB.Exec("UPDATE settings SET json=? WHERE id='ops-watch-notices'", string(changed)); e != nil {
			t.Fatal(e)
		}
		return map[string]any{"number": float64(1), "state": "closed"}, nil
	}}
	if e := cleanup.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	alerts, e := s.Alerts(ctx)
	if e != nil || len(alerts) != 1 || fleet.Object(alerts[0])["title"] != "new" {
		t.Fatalf("changed notice lost: %v %v", alerts, e)
	}
	archive, e := s.Archive(ctx)
	if e != nil || len(archive) != 1 {
		t.Fatal(archive, e)
	}
	cleanup.ReadIssue = func(context.Context, string, int64) (map[string]any, error) { return nil, errors.New("offline") }
	if e = cleanup.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	alerts, e = s.Alerts(ctx)
	if e != nil || len(alerts) != 1 {
		t.Fatal("failed read removed notice")
	}
	cleanup.ReadIssue = func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"number": float64(1), "state": "closed"}, nil
	}
	if _, e = s.DB.Exec(`CREATE FUNCTION reject_archive() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'disk failure'; END $$;
CREATE TRIGGER reject_archive BEFORE INSERT OR UPDATE ON settings FOR EACH ROW WHEN (NEW.id='ops-watch-notices-archive') EXECUTE FUNCTION reject_archive()`); e != nil {
		t.Fatal(e)
	}
	if e = cleanup.Reconcile(ctx); e == nil {
		t.Fatal("archive failure ignored")
	}
	alerts, e = s.Alerts(ctx)
	if e != nil || len(alerts) != 1 {
		t.Fatal("notice removed before archival")
	}
}
func TestStageAlertsUseValidatedLatestAndExpiredHeartbeat(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	event := obj(`{"kind":"stage-watch","event":"start","id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","operationId":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","name":"ship","at":"2026-11-01T00:55:00.000Z","startedAt":"2026-11-01T00:55:00.000Z","deadline":null,"target":{"project":"owner/repo","number":1},"observation":"process","lastSuccessAt":null,"holder":"watcher","exitCode":null,"outputs":null,"anomalies":[]}`)
	if _, e := s.Receive(ctx, "nas", event); e != nil {
		t.Fatal(e)
	}
	rows, e := s.StageAlerts(ctx)
	if e != nil || len(rows) != 1 || fleet.Object(rows[0])["stateLabel"] != "heartbeat失効" {
		t.Fatalf("stage %v %v", rows, e)
	}
	var raw string
	s.DB.QueryRow("SELECT json FROM settings WHERE id='stage-watch-operations'").Scan(&raw)
	var saved []map[string]any
	json.Unmarshal([]byte(raw), &saved)
	fleet.Object(saved[0]["latest"])["id"] = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	bytes, _ := json.Marshal(saved)
	s.DB.Exec("UPDATE settings SET json=? WHERE id='stage-watch-operations'", string(bytes))
	rows, e = s.StageAlerts(ctx)
	if e != nil || len(rows) != 0 {
		t.Fatal("inconsistent latest accepted")
	}
}
