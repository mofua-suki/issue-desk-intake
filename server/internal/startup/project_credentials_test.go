package startup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/projects"
)

func TestOpenAppMissingProjectAdminKeepsIntakeAvailablePostgres(t *testing.T) {
	ctx := context.Background()
	pg := appPostgres(t)
	dir := t.TempDir()
	var writes atomic.Int32
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
			w.WriteHeader(500)
			return
		}
		if r.Header.Get("Authorization") != "token fixture-intake-token" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "intake", "id": 1})
		case "/api/v1/users/owner":
			json.NewEncoder(w).Encode(map[string]any{"login": "owner", "id": 2})
		default:
			w.WriteHeader(404)
		}
	}))
	defer git.Close()
	if e := os.MkdirAll(filepath.Join(dir, "private"), 0700); e != nil {
		t.Fatal(e)
	}
	token := filepath.Join(dir, "private", "intake-token")
	if e := os.WriteFile(token, []byte("fixture-intake-token"), 0600); e != nil {
		t.Fatal(e)
	}
	admin := filepath.Join(dir, "private", "absent-admin-token")
	file := filepath.Join(dir, "config.local.json")
	values := map[string]any{"workerId": "nas", "giteaUrl": git.URL, "runtimeDir": "runtime", "tokenFile": admin, "intakeTokenFile": token, "intakeAccount": "intake", "gitUser": "owner", "projectCreation": map[string]any{"owner": "owner"}, "codex": "/missing-fixture-codex", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"publicUrl": "http://localhost:8787", "port": 8787, "aiConcurrency": 1}}
	writeConfig(t, file, values)
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"giteaUrl": git.URL, "repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	writeConfig(t, filepath.Join(dir, "assignments.json"), map[string]any{"pollSeconds": 300, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": map[string]any{"owner/repo": map[string]any{"rank": 1}}}}, "repositories": map[string]any{}})
	stageApplicationAssets(t, dir)
	cfg, e := Load(file, dir, "")
	if e != nil {
		t.Fatal(e)
	}
	useApplicationSchema(t, cfg)
	app, e := OpenApp(ctx, cfg, pg, strings.Repeat("a", 40), func(string) {})
	if e != nil {
		t.Fatal(e)
	}
	defer app.Close()
	if app.Projects == nil || app.Filing == nil || app.Jobs == nil {
		t.Fatal("通常受付の接続がありません")
	}
	if app.CurrentConfig().Values["tokenFile"] != admin {
		t.Fatal("管理用設定を変更しました")
	}
	now := time.Now()
	if e = app.SQLite.CreateSession(ctx, auth.Hash("fixture-session"), auth.Session{CSRF: "fixture-csrf", ExpiresAt: now.Add(time.Hour)}, now); e != nil {
		t.Fatal(e)
	}
	id := "11111111-1111-4111-8111-111111111111"
	definition := map[string]any{"name": "new-project"}
	record := map[string]any{"id": id, "status": "create_approved", "definition": definition, "approval": map[string]any{"fingerprint": projects.Fingerprint(definition)}, "revision": 1}
	raw, _ := json.Marshal(record)
	if _, e = app.SQLite.DB().Exec("INSERT INTO projects(id,json)VALUES(?,?)", id, string(raw)); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/todos", 200}, {"GET", "/api/projects/" + id, 200}, {"POST", "/api/projects/" + id + "/create", 503},
	} {
		req := httptest.NewRequest(tc.method, "http://localhost:8787"+tc.path, strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.AddCookie(&http.Cookie{Name: "desk_session", Value: "fixture-session"})
		req.Header.Set("Origin", "http://localhost:8787")
		req.Header.Set("X-CSRF-Token", "fixture-csrf")
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		app.Mux.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatal(tc.path, res.Code, res.Body.String())
		}
		if tc.method == "GET" && strings.HasPrefix(tc.path, "/api/projects/") && !strings.Contains(res.Body.String(), `"creationUnavailable":true`) {
			t.Fatal(res.Body.String())
		}
	}
	if writes.Load() != 0 {
		t.Fatal("専用アカウントで管理操作しました")
	}
	var after string
	if e = app.SQLite.DB().QueryRow("SELECT json FROM projects WHERE id=?", id).Scan(&after); e != nil || after != string(raw) {
		t.Fatal("作成不可の記録が変わりました", e)
	}
	if _, e = os.Stat(admin); !os.IsNotExist(e) {
		t.Fatal("管理用トークンを生成しました", e)
	}
}
