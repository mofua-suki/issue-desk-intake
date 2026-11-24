package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
	"testing"
)

func TestImageWritesRecheckChangedDraftInsideTransaction(t *testing.T) {
	ctx := context.Background()
	previousDialect := appsql.SetActive(appsql.Postgres)
	t.Cleanup(func() { appsql.SetActive(previousDialect) })
	env := pgtest.NewIsolated(t)
	db, e := store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	current := map[string]any{"id": fixtureID, "project": "owner/repo", "status": "draft"}
	save := func(status string) {
		t.Helper()
		record := clone(current)
		record["status"] = status
		raw, _ := json.Marshal(record)
		if _, e = db.DB().Exec(`INSERT INTO drafts(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json`, fixtureID, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	save("draft")
	h := &Handler{Options{AppDB: db.DB(), Project: func(string) error { return nil }, Draft: func(context.Context, string) (map[string]any, error) { return clone(current), nil }, DraftWritable: func(d map[string]any) bool { return d["status"] == "draft" }}}
	data := map[string]any{"project": "owner/repo", "draft": fixtureID, "id": fixtureID, "messageId": messageID, "data": base64.StdEncoding.EncodeToString(pngBytes(t))}
	principal := auth.Principal{Kind: auth.Screen}
	scope, e := h.scope(ctx, data, true, principal)
	if e != nil {
		t.Fatal(e)
	}
	save("thinking")
	if _, e = h.upload(ctx, scope, data, principal); e == nil {
		t.Fatal("相談が応答中になった後に画像を追加しました")
	}
	var count int
	if e = db.DB().QueryRow("SELECT count(*) FROM images").Scan(&count); e != nil || count != 0 {
		t.Fatal("拒否後に画像が残りました", count, e)
	}
	save("draft")
	if _, e = h.upload(ctx, scope, data, principal); e != nil {
		t.Fatal(e)
	}
	save("submitted")
	if _, e = h.remove(ctx, fixtureID, scope, data, principal); e == nil {
		t.Fatal("起票後に画像を削除しました")
	}
	if e = db.DB().QueryRow("SELECT count(*) FROM images").Scan(&count); e != nil || count != 1 {
		t.Fatal("拒否後に画像を失いました", count, e)
	}
}
