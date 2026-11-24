package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/store"
)

// binaryAppFixture opens the application behind a real TLS listener with fixture Gitea and tokens.
func binaryAppFixture(t *testing.T, pg *sql.DB) (*App, *httptest.Server, string, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token fixture-intake-token" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "intake", "id": 1})
		case "/api/v1/users/owner":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "owner", "id": 2})
		case "/api/v1/repos/owner/repo/issues":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "fixture unavailable"})
		}
	}))
	t.Cleanup(git.Close)
	server := httptest.NewUnstartedServer(nil)
	// Authは実際に待受けるTLS originだけを許可する。
	public := "https://" + server.Listener.Addr().String()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	intakeToken := write("intake-token", "fixture-intake-token")
	adminToken := write("admin-token", "fixture-admin-token")
	file := filepath.Join(dir, "config.local.json")
	writeConfig(t, file, map[string]any{"workerId": "nas", "giteaUrl": git.URL, "runtimeDir": "runtime", "tokenFile": adminToken, "intakeTokenFile": intakeToken, "intakeAccount": "intake", "gitUser": "owner", "codex": "/missing-fixture-codex", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"publicUrl": public, "port": 8787, "aiConcurrency": 1}})
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"giteaUrl": git.URL, "repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	writeConfig(t, filepath.Join(dir, "assignments.json"), map[string]any{"pollSeconds": 300, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": map[string]any{"owner/repo": map[string]any{"rank": 1}}}}, "repositories": map[string]any{}})
	config, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	sqlite := openAppStore(t, useApplicationSchema(t, config))
	pc, hostd := "idk_"+strings.Repeat("a", 43), "idh_"+strings.Repeat("b", 43)
	if _, err = sqlite.AddPCToken(ctx, store.PCTokenRecord{ID: "fixture-worker", Name: "nas", Hash: auth.Hash(pc), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.Exec(`INSERT INTO host_image_tokens(host,hash)VALUES('nas',$1)`, auth.Hash(hostd)); err != nil {
		t.Fatal(err)
	}
	stageApplicationAssets(t, dir)
	app, err := OpenApp(ctx, config, pg, strings.Repeat("c", 40), func(string) {})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = app.Mux
	server.StartTLS()
	t.Cleanup(func() { server.Close(); app.Close() })
	return app, server, dir, pc, hostd
}
