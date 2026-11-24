package images

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

const fixtureID = "11111111-1111-4111-8111-111111111111"
const messageID = "22222222-2222-4222-8222-222222222222"

func TestImagesScopeDedupBindingLinkAndRestart(t *testing.T) {
	ctx := context.Background()
	previousDialect := appsql.SetActive(appsql.Postgres)
	t.Cleanup(func() { appsql.SetActive(previousDialect) })
	env := pgtest.NewIsolated(t)
	db, e := store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	save := func(record map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(record)
		if _, err := db.DB().ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json`, fixtureID, string(raw)); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if e = db.CreateSession(ctx, auth.Hash("screen"), auth.Session{CSRF: "csrf", ExpiresAt: now.Add(time.Hour)}, now); e != nil {
		t.Fatal(e)
	}
	a, e := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787", Now: func() time.Time { return now }}, &store.AuthStore{SQLite: db})
	if e != nil {
		t.Fatal(e)
	}
	draft := map[string]any{"id": fixtureID, "project": "owner/repo", "status": "draft"}
	saveDraft := func() { save(draft) }
	saveDraft()
	options := Options{AppDB: db.DB(), Auth: a, Project: func(project string) error {
		if project != "owner/repo" {
			return fail(403, "対象外")
		}
		return nil
	}, Issue: func(context.Context, string, int64) error { return nil }, Draft: func(context.Context, string) (map[string]any, error) { return draft, nil }, Todo: func(context.Context, string) (map[string]any, error) {
		return map[string]any{"id": fixtureID, "project": "owner/repo"}, nil
	}, DraftWritable: func(d map[string]any) bool { return d["status"] == "draft" }}
	mux := http.NewServeMux()
	if e = Register(mux, options); e != nil {
		t.Fatal(e)
	}
	call := func(method, path string, data any, headers ...map[string]string) (int, []byte) {
		raw, _ := json.Marshal(data)
		r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		r.Header.Set("X-CSRF-Token", "csrf")
		r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
		for _, values := range headers {
			for k, v := range values {
				r.Header.Set(k, v)
			}
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code, w.Body.Bytes()
	}
	input := map[string]any{"project": "owner/repo", "draft": fixtureID, "id": fixtureID, "messageId": messageID, "data": base64.StdEncoding.EncodeToString(pngBytes(t))}
	code, raw := call("POST", "/api/images", input)
	if code != 201 {
		t.Fatalf("upload %d %s", code, raw)
	}
	code, raw = call("POST", "/api/images", input)
	if code != 201 {
		t.Fatalf("dedup %d %s", code, raw)
	}
	input["messageId"] = "33333333-3333-4333-8333-333333333333"
	code, _ = call("POST", "/api/images", input)
	if code != 409 {
		t.Fatal("異なるメッセージで画像IDを再使用しました")
	}
	code, raw = call("GET", "/api/images/"+fixtureID+"?project=owner%2Frepo&draft="+fixtureID, nil)
	if code != 200 || !bytes.Equal(raw, pngBytes(t)) {
		t.Fatalf("read %d", code)
	}
	code, _ = call("GET", "/api/images/"+fixtureID+"?project=other%2Frepo&draft="+fixtureID, nil)
	if code != 403 {
		t.Fatal("別projectから読めました")
	}
	if _, e = db.DB().ExecContext(ctx, "UPDATE images SET bound=1 WHERE id=?", fixtureID); e != nil {
		t.Fatal(e)
	}
	code, _ = call("POST", "/api/images/"+fixtureID+"/remove", map[string]any{"project": "owner/repo", "draft": fixtureID})
	if code != 409 {
		t.Fatal("使用中の画像を削除しました")
	}
	save(map[string]any{"id": fixtureID, "project": "owner/repo", "issue": map[string]any{"number": 1}})

	mux = http.NewServeMux()
	if e = Register(mux, options); e != nil {
		t.Fatal(e)
	}
	code, raw = call("GET", "/api/images/"+fixtureID+"?project=owner%2Frepo&number=1", nil)
	if code != 200 || !bytes.Equal(raw, pngBytes(t)) {
		t.Fatal("起票後のリンク画像を読めません")
	}
	draft["status"] = "submitted"
	code, _ = call("POST", "/api/images", input)
	if code != 409 {
		t.Fatal("送信不能な相談の画像を変更しました")
	}
	draft["status"] = "draft"
	draft["unassigned"] = true
	draft["project"] = nil
	saveDraft()
	input["project"] = nil
	input["id"] = "44444444-4444-4444-8444-444444444444"
	code, raw = call("POST", "/api/images", input)
	if code != 201 {
		t.Fatalf("未定 %d %s", code, raw)
	}
	code, _ = call("GET", "/api/images/44444444-4444-4444-8444-444444444444?project=null&draft="+fixtureID, nil)
	if code != 200 {
		t.Fatal("プロジェクト未定の画像を読めません")
	}
	for index := 20; index < 23; index++ {
		input["id"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		code, raw = call("POST", "/api/images", input)
		if code != 201 {
			t.Fatalf("4枚まで許可: %d %s", code, raw)
		}
	}
	input["id"] = "00000000-0000-4000-8000-000000000023"
	code, _ = call("POST", "/api/images", input)
	if code != 400 {
		t.Fatal("1メッセージの5枚目を許しました")
	}
	for index := 23; index < 30; index++ {
		input["id"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		input["messageId"] = fmt.Sprintf("99999999-9999-4999-8999-%012d", (index-23)/4)
		code, raw = call("POST", "/api/images", input)
		if code != 201 {
			t.Fatalf("12枚まで許可: %d %s", code, raw)
		}
	}
	input["id"] = "00000000-0000-4000-8000-000000000030"
	input["messageId"] = "88888888-8888-4888-8888-888888888888"
	code, _ = call("POST", "/api/images", input)
	if code != 400 {
		t.Fatal("1相談の13枚目を許しました")
	}
	code, _ = call("POST", "/api/images", input, map[string]string{"X-CSRF-Token": "bad"})
	if code != 403 {
		t.Fatal("CSRF不正で書き込めました")
	}
	view := Present(map[string]any{"id": fixtureID, "project": "owner/repo", "images": []any{map[string]any{"id": fixtureID}}}, "https://desk.example")
	if !strings.Contains(fleet.Text(view["images"].([]any)[0].(map[string]any)["url"]), "todo="+fixtureID) {
		t.Fatal(view)
	}
}

func TestRegisterApplicationDBRequirements(t *testing.T) {
	options := Options{AppDB: &sql.DB{}, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, Issue: func(context.Context, string, int64) error { return nil }, Draft: func(context.Context, string) (map[string]any, error) { return nil, nil }, Todo: func(context.Context, string) (map[string]any, error) { return nil, nil }, DraftWritable: func(map[string]any) bool { return true }}
	for _, tc := range []struct {
		name      string
		change    func(*Options)
		nilMux    bool
		wantError bool
	}{
		{"complete", func(*Options) {}, false, false},
		{"no database", func(o *Options) { o.AppDB = nil }, false, true},
		{"no mux", func(*Options) {}, true, true},
		{"no auth", func(o *Options) { o.Auth = nil }, false, true},
		{"no project", func(o *Options) { o.Project = nil }, false, true},
		{"no issue", func(o *Options) { o.Issue = nil }, false, true},
		{"no draft", func(o *Options) { o.Draft = nil }, false, true},
		{"no todo", func(o *Options) { o.Todo = nil }, false, true},
		{"no draft writable", func(o *Options) { o.DraftWritable = nil }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := options
			tc.change(&o)
			mux := http.NewServeMux()
			if tc.nilMux {
				mux = nil
			}
			if err := Register(mux, o); (err != nil) != tc.wantError {
				t.Fatalf("Register error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

// Exercise the upload, read and remove transactions through the application DB.
func TestApplicationDBImageTransactions(t *testing.T) {
	ctx := context.Background()
	previous := appsql.SetActive(appsql.Postgres)
	t.Cleanup(func() { appsql.SetActive(previous) })
	env := pgtest.NewIsolated(t)
	db, err := store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &Handler{options: Options{AppDB: db.DB()}}
	data := map[string]any{"id": fixtureID, "messageId": messageID, "data": base64.StdEncoding.EncodeToString(pngBytes(t))}
	scope := "issue:owner/repo#1"
	if _, err := h.upload(ctx, scope, data, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	_, raw, err := h.read(ctx, fixtureID, scope, data)
	if err != nil || !bytes.Equal(raw, pngBytes(t)) {
		t.Fatalf("read: %v", err)
	}
	if _, err := h.remove(ctx, fixtureID, scope, data, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.read(ctx, fixtureID, scope, data); err == nil {
		t.Fatal("removed image remains readable")
	}
}
