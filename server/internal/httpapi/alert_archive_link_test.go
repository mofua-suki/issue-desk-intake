package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestHTTPAttentionArchiveKeepsLegacyPublicReferences(t *testing.T) {
	const original = `{"key":"owner/repo#1","kind":"failed","state":"notify","project":"owner/repo","number":1,"title":"保存済み通知","reason":"判断が必要","gist":"同じ原因","investigation":{"project":"owner/repo","number":2}}`
	var rawArchive string
	var options *WorkerRouteOptions
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		options = o
		o.PublicLink = func(repo, kind string, n int64) string {
			return fmt.Sprintf("https://public.example/%s/%s/%d", repo, kind, n)
		}
		if _, e := o.AppDB.Exec(`INSERT INTO attention_cases(key,json)VALUES('owner/repo#1',?)`, original); e != nil {
			t.Fatal(e)
		}
		// Fixed legacy snapshot: the HTTP constructor must preserve the same links
		// as the screen constructor before the shared archive filter compares it.
		old := map[string]any{"id": "attention:owner/repo#1", "key": "owner/repo#1", "kind": "failed", "kindName": "失敗", "project": "owner/repo", "number": 1, "desk": nil, "url": "https://public.example/owner/repo/issues/1", "state": "notify", "stateLabel": "利用者の判断が必要", "title": "保存済み通知", "reason": "判断が必要", "gist": "同じ原因", "autoCount": 0, "streak": 1, "verdict": nil, "investigation": map[string]any{"project": "owner/repo", "number": 2, "desk": nil, "url": "https://public.example/owner/repo/issues/2"}, "draftId": nil, "sameAs": nil, "startedAt": nil, "updatedAt": nil}
		raw, _ := json.Marshal([]any{map[string]any{"target": "attention:attention:owner/repo#1", "snapshot": old, "fingerprint": "legacy-order-hash"}})
		rawArchive = string(raw)
		if _, e := o.AppDB.Exec(`INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)`, rawArchive); e != nil {
			t.Fatal(e)
		}
	})
	read := func() []map[string]any {
		r := httptest.NewRequest("GET", "http://localhost:8787/api/attention/alerts", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var rows []map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &rows); e != nil || w.Code != 200 || rows == nil {
			t.Fatalf("HTTP%d %s %v", w.Code, w.Body.String(), e)
		}
		return rows
	}
	if rows := read(); len(rows) != 0 {
		t.Fatal("HTTPだけ終了済み通知を再表示", rows)
	}
	var saved string
	if e := options.AppDB.QueryRow(`SELECT json FROM attention_cases WHERE key='owner/repo#1'`).Scan(&saved); e != nil || saved != original {
		t.Fatal("読取で元記録が変化", e)
	}
	const changed = `{"key":"owner/repo#1","kind":"failed","state":"notify","project":"owner/repo","number":1,"title":"保存済み通知","reason":"判断が必要","gist":"実際に変わった原因","investigation":{"project":"owner/repo","number":2}}`
	if _, e := options.AppDB.Exec(`UPDATE attention_cases SET json=? WHERE key='owner/repo#1'`, changed); e != nil {
		t.Fatal(e)
	}
	rows := read()
	if len(rows) != 1 || rows[0]["url"] != "https://public.example/owner/repo/issues/1" || rows[0]["investigation"].(map[string]any)["url"] != "https://public.example/owner/repo/issues/2" {
		t.Fatal("本当の原因変化/公開参照を隠しました", rows)
	}
	if e := options.AppDB.QueryRow(`SELECT json FROM settings WHERE id='desk-cleanup-archive'`).Scan(&saved); e != nil || saved != rawArchive {
		t.Fatal("既存終了記録を変更", e)
	}
}

func TestHTTPStallArchiveFiltersUnchangedRecords(t *testing.T) {
	hidden := map[string]any{"id": "hidden", "kind": "test", "title": "old"}
	visible := map[string]any{"id": "visible", "kind": "test", "title": "visible"}
	var dbOptions *WorkerRouteOptions
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		dbOptions = o
		o.DraftStallAlerts = func(context.Context) ([]any, error) { return []any{hidden, visible}, nil }
		raw, err := json.Marshal([]any{map[string]any{"target": "stall:hidden", "snapshot": hidden}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.AppDB.Exec(`INSERT INTO settings(id,json) VALUES('desk-cleanup-archive',?)`, string(raw)); err != nil {
			t.Fatal(err)
		}
	})
	read := func() []map[string]any {
		r := httptest.NewRequest("GET", "http://localhost:8787/api/stalls/alerts", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var rows []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || w.Code != 200 {
			t.Fatalf("HTTP%d %s: %v", w.Code, w.Body.String(), err)
		}
		return rows
	}
	rows := read()
	for _, row := range rows {
		if row["id"] == "hidden" {
			t.Fatal("終了済み通知が表示されました", rows)
		}
	}
	found := false
	for _, row := range rows {
		if row["id"] == "visible" {
			found = true
		}
	}
	if !found {
		t.Fatal("未終了の通知が消えました", rows)
	}
	hidden["title"] = "changed"
	rows = read()
	found = false
	for _, row := range rows {
		if row["id"] == "hidden" && row["title"] == "changed" {
			found = true
		}
	}
	if !found {
		t.Fatal("変更された通知が表示されません", rows)
	}
	var count int
	if err := dbOptions.AppDB.QueryRow(`SELECT count(*) FROM settings WHERE id='desk-cleanup-archive'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("終了記録が失われました", err)
	}
}
