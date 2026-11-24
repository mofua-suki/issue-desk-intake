package screenapi

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"testing"
	"time"
)

func deltaSnapshot() Snapshot {
	return Snapshot{Rows: []Row{{"draft:a", "draft", Object{"id": "a", "title": "元"}}}, Aux: map[string]any{"reviews": []any{}, "fullTests": []any{}, "stalls": []any{}, "attentionAlerts": []any{}}, Hosts: Object{"checkedAt": "1", "hosts": []any{Object{"host": "nas"}}}}
}
func TestDeltaPreservesUnavailableKindsAndIgnoresHostCheckedAt(t *testing.T) {
	ctx := context.Background()
	d := NewDelta(DeltaOptions{})
	s := deltaSnapshot()
	read := func(context.Context) (Snapshot, error) { return s, nil }
	full, e := d.Respond(ctx, "scope", nil, read)
	if e != nil || full["full"] != true {
		t.Fatal(full, e)
	}
	cursor := fleet.Text(full["cursor"])
	delete(s.Aux, "reviews")
	s.Hosts = Object{"checkedAt": "2", "hosts": []any{Object{"host": "nas"}}}
	change, e := d.Respond(ctx, "scope", &cursor, read)
	if e != nil || change["full"] != false || len(fleet.ObjectOrEmpty(change["aux"])) != 0 || change["hosts"] != nil || len(fleet.Items(change["removed"])) != 0 {
		t.Fatal("unavailable data became removal", change, e)
	}
	cursor = fleet.Text(change["cursor"])
	s.Aux["reviews"] = []any{}
	s.Hosts = nil
	change, e = d.Respond(ctx, "scope", &cursor, read)
	if e != nil || len(fleet.ObjectOrEmpty(change["aux"])) != 0 {
		t.Fatal("recovered unchanged aux became changed", change, e)
	}
	cursor = fleet.Text(change["cursor"])
	s.Hosts = Object{"checkedAt": "3", "hosts": []any{Object{"host": "nas"}}}
	s.Rows[0].Item["title"] = "新"
	change, e = d.Respond(ctx, "scope", &cursor, read)
	if e != nil || len(fleet.Items(change["updated"])) != 1 || change["hosts"] != nil {
		t.Fatal(change, e)
	}
}
func TestDeltaLoadFailureRetainsCursorAndScopeExpiryBounds(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	d := NewDelta(DeltaOptions{Now: func() time.Time { return now }, TTL: time.Minute})
	s := deltaSnapshot()
	load := func(context.Context) (Snapshot, error) { return s, nil }
	full, _ := d.Respond(ctx, "a", nil, load)
	cursor := fleet.Text(full["cursor"])
	if _, e := d.Respond(ctx, "a", &cursor, func(context.Context) (Snapshot, error) { return Snapshot{}, errors.New("core read failed") }); e == nil {
		t.Fatal("core failure became empty success")
	}
	change, e := d.Respond(ctx, "a", &cursor, load)
	if e != nil || change["full"] != false {
		t.Fatal("failed load consumed cursor", change, e)
	}
	cursor = fleet.Text(change["cursor"])
	reset, e := d.Respond(ctx, "b", &cursor, load)
	if e != nil || reset["reset"] != true {
		t.Fatal("cross-session cursor reused", reset, e)
	}
	cursor = fleet.Text(reset["cursor"])
	now = now.Add(2 * time.Minute)
	reset, e = d.Respond(ctx, "b", &cursor, load)
	if e != nil || reset["reset"] != true {
		t.Fatal("expired cursor reused", reset, e)
	}
	called := false
	bad := "../invalid"
	if _, e = d.Respond(ctx, "b", &bad, func(context.Context) (Snapshot, error) { called = true; return s, nil }); e == nil || called {
		t.Fatal("invalid cursor reached data source")
	}
	tiny := NewDelta(DeltaOptions{Bytes: 1})
	full, e = tiny.Respond(ctx, "a", nil, load)
	if e != nil || full["cursor"] != nil || full["full"] != true {
		t.Fatal("memory bound did not fall back to full", full, e)
	}
}
