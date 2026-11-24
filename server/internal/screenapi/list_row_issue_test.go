package screenapi

import (
	"context"
	"net/url"
	"testing"

	"issue-desk-intake/server/internal/fleet"
)

// 画面の一覧が読まない Issue の項目は、一覧の行・1件の取得のどちらにも出さず、その変化だけでは差分にしない。
// 一覧が読む項目と、読取元の行（Issue の一覧・詳細が返す内容）は変えない。
func TestIssueListRowOmitsFieldsTheListDoesNotRead(t *testing.T) {
	omitted := []string{"dispatchRecord", "executionSettings", "dispatchSkip", "workType", "publishPending", "releasePending", "executionWait"}
	source := func() Object {
		return Object{
			"project": "owner/repo", "number": 1, "desk": 10, "title": "題名", "status": "queued", "updatedAt": "2026-11-01T00:00:00Z",
			"workTypeLabel": "不具合修正", "scopeWait": Object{"note": "先行のマージ待ち"}, "executionProgress": Object{"pending": 1}, "priority": Object{"level": "high"},
			"dispatchRecord":    Object{"events": []any{Object{"at": "2026-11-01T00:00:00Z", "host": "nas"}}},
			"executionSettings": Object{"implementation": Object{"provider": "codex"}},
			"dispatchSkip":      Object{"reason": "capability"},
			"workType":          "bugfix",
			"publishPending":    true,
			"releasePending":    true,
			"executionWait":     "reporter",
		}
	}
	current := source()
	empty := func(context.Context, string) ([]Object, error) { return []Object{}, nil }
	s := &Service{o: Options{
		Config:  func() (Object, error) { return Object{"repositories": []any{Object{"name": "owner/repo"}}}, nil },
		Lists:   map[string]ListReader{"issue": func(context.Context, string) ([]Object, error) { return []Object{current}, nil }, "draft": empty, "todo": empty, "closeRequest": empty},
		Entries: map[string]EntryReader{"issue": func(context.Context, Object, string) (Object, error) { return current, nil }},
		Aux:     map[string]func(context.Context) (any, error){},
	}}
	ctx := context.Background()
	check := func(name string, item Object) {
		t.Helper()
		for _, key := range omitted {
			if _, has := item[key]; has {
				t.Fatalf("%s: %s が一覧の行に残っている: %v", name, key, item)
			}
		}
		if item["workTypeLabel"] != "不具合修正" || item["status"] != "queued" || item["title"] != "題名" || fleet.Number(item["desk"]) != 10 ||
			fleet.ObjectOrEmpty(item["scopeWait"])["note"] != "先行のマージ待ち" || fleet.Number(fleet.ObjectOrEmpty(item["executionProgress"])["pending"]) != 1 || fleet.ObjectOrEmpty(item["priority"])["level"] != "high" {
			t.Fatalf("%s: 一覧が読む項目が欠けた: %v", name, item)
		}
	}
	read := func(context.Context) (Snapshot, error) { return s.snapshot(ctx, "", newListTiming()) }
	d := NewDelta(DeltaOptions{})
	full, e := d.Respond(ctx, "scope", nil, read)
	if e != nil || full["full"] != true {
		t.Fatal(full, e)
	}
	rows, ok := full["rows"].([]Row)
	if !ok || len(rows) != 1 || rows[0].Key != "issue:owner/repo#1" || rows[0].Type != "issue" {
		t.Fatal(full["rows"])
	}
	check("全体", rows[0].Item)
	entry, e := s.entry(ctx, url.Values{"type": {"issue"}, "project": {"owner/repo"}, "number": {"1"}}, "")
	if e != nil || entry["key"] != "issue:owner/repo#1" {
		t.Fatal(entry, e)
	}
	check("1件", fleet.ObjectOrEmpty(entry["item"]))
	for _, key := range omitted {
		if _, has := current[key]; !has {
			t.Fatalf("読取元の行から %s が消えた", key)
		}
	}

	// 外した項目だけが変わっても、行の更新として返さない。
	cursor := fleet.Text(full["cursor"])
	current = source()
	current["dispatchRecord"] = Object{"events": []any{Object{"at": "2026-11-01T00:01:00Z", "host": "gt12-pro"}}}
	current["executionSettings"] = Object{"implementation": Object{"provider": "claude"}}
	current["dispatchSkip"] = Object{"reason": "execution_plan"}
	current["workType"] = "feature"
	current["executionWait"] = nil
	delete(current, "publishPending")
	current["releasePending"] = false
	change, e := d.Respond(ctx, "scope", &cursor, read)
	if e != nil || change["full"] != false || len(fleet.Items(change["added"])) != 0 || len(fleet.Items(change["updated"])) != 0 || len(fleet.Items(change["removed"])) != 0 {
		t.Fatal("一覧が読まない項目の変化が差分になった", change, e)
	}
	// 一覧が読む項目の変化は、外した項目を含まない行として返す。
	cursor = fleet.Text(change["cursor"])
	current["status"] = "running"
	change, e = d.Respond(ctx, "scope", &cursor, read)
	updated, ok := change["updated"].([]Row)
	if e != nil || !ok || len(updated) != 1 || updated[0].Item["status"] != "running" {
		t.Fatal(change, e)
	}
	for _, key := range omitted {
		if _, has := updated[0].Item[key]; has {
			t.Fatalf("差分: %s が一覧の行に残っている: %v", key, updated[0].Item)
		}
	}
}
