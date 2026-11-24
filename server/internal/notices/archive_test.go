package notices

import (
	"context"
	"encoding/json"
	"testing"
)

func TestArchiveRestoreConflictExpiryAndIdempotency(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	notice := map[string]any{"id": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "reason": "disk", "issues": []any{}, "cause": "disk", "title": "disk", "count": 2}
	records := []any{map[string]any{"id": id, "notice": notice, "removedAt": "2026-10-31T00:00:00.000Z"}, map[string]any{"id": "old", "notice": notice, "removedAt": "2026-09-01T00:00:00.000Z"}}
	raw, _ := json.Marshal(records)
	if _, e := s.DB.Exec("INSERT INTO settings(id,json)VALUES('ops-watch-notices-archive',?)", string(raw)); e != nil {
		t.Fatal(e)
	}
	rows, e := s.Archive(ctx)
	if e != nil || len(rows) != 1 {
		t.Fatalf("archive %v %v", rows, e)
	}
	for range 2 {
		r, e := s.Restore(ctx, id, map[string]any{})
		if e != nil || r.(map[string]any)["restored"] != true {
			t.Fatalf("restore %v %v", r, e)
		}
	}
	alerts, e := s.Alerts(ctx)
	if e != nil || len(alerts) != 1 || alerts[0].(map[string]any)["reason"] != "disk（同じ原因 2件）" {
		t.Fatalf("projection %v %v", alerts, e)
	}
	var current string
	if e = s.DB.QueryRow("SELECT json FROM settings WHERE id='ops-watch-notices'").Scan(&current); e != nil {
		t.Fatal(e)
	}
	var ns []any
	json.Unmarshal([]byte(current), &ns)
	if len(ns) != 1 {
		t.Fatal("restore duplicated")
	}
	records[0].(map[string]any)["restoredAt"] = nil
	raw, _ = json.Marshal(records)
	s.DB.Exec("UPDATE settings SET json=? WHERE id='ops-watch-notices-archive'", string(raw))
	s.DB.Exec("UPDATE settings SET json=? WHERE id='ops-watch-notices'", `[{"id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","reason":"changed"}]`)
	if _, e = s.Restore(ctx, id, map[string]any{}); e == nil {
		t.Fatal("conflicting current overwritten")
	}
}
