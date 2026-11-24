package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/claudeapikey"
)

type keyAuthStore struct{}

func (keyAuthStore) PCToken(_ context.Context, hash string) (auth.Token, bool, error) {
	switch hash {
	case auth.Hash("idk_" + strings.Repeat("a", 43)):
		return auth.Token{ID: "a", Name: "nas", Delegated: true}, true, nil
	case auth.Hash("idk_" + strings.Repeat("b", 43)):
		return auth.Token{ID: "b", Name: "other"}, true, nil
	}
	return auth.Token{}, false, nil
}
func (keyAuthStore) TouchPCToken(context.Context, string, time.Time) error { return nil }
func (keyAuthStore) HostdToken(_ context.Context, hash string) (auth.Token, bool, error) {
	return auth.Token{ID: "h", Name: "nas"}, hash == auth.Hash("idh_"+strings.Repeat("h", 43)), nil
}
func (keyAuthStore) Session(_ context.Context, hash string) (auth.Session, bool, error) {
	return auth.Session{ExpiresAt: time.Now().Add(time.Hour)}, hash == auth.Hash("session"), nil
}
func (keyAuthStore) ConsumeCLIKey(_ context.Context, hash string, _ time.Time) (bool, error) {
	return hash == auth.Hash("cli"), nil
}
func keyFixture(t *testing.T) (*WorkerRoutes, *claudeapikey.Store) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	a, err := auth.New(auth.Config{Port: 8787}, keyAuthStore{})
	if err != nil {
		t.Fatal(err)
	}
	return &WorkerRoutes{options: WorkerRouteOptions{Auth: a, ClaudeAPIKeyTokenFile: tokenFile, Now: time.Now}}, claudeapikey.New(tokenFile)
}
func keyRequest(s *WorkerRoutes, method, host, credential string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:8787/api/v1/hosts/"+host+"/claude-api-key", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	switch credential {
	case "session":
		r.AddCookie(&http.Cookie{Name: "desk_session", Value: "session"})
	case "cli":
		r.Header.Set("X-Desk-Key", "cli")
	default:
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
	}
	w := httptest.NewRecorder()
	s.hostRoute(w, r)
	return w
}
func TestClaudeAPIKeyAuthentication(t *testing.T) {
	t.Run("persistent credentials and registered route", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		var appDB *sql.DB
		handler, db, token := workerFixtureOptions(t, true, func(options *WorkerRouteOptions) {
			options.ClaudeAPIKeyTokenFile = filepath.Join(dir, "token")
			appDB = options.AppDB
		})
		other := "idk_" + strings.Repeat("b", 43)
		hostd := "idh_" + strings.Repeat("h", 43)
		if _, err := appDB.Exec("INSERT INTO api_tokens(id,name,hash,issued_at) VALUES(?,?,?,?)", "other", "other", auth.Hash(other), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if _, err := appDB.Exec("INSERT INTO cli_keys(hash,expires) VALUES(?,?)", auth.Hash("cli"), time.Now().Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		sessions, _ := json.Marshal([]any{[]any{auth.Hash("session"), map[string]any{"csrf": "csrf", "expires": time.Now().Add(time.Hour).UnixMilli()}}})
		if _, err := appDB.Exec("INSERT INTO settings(id,json) VALUES('sessions',?) ON CONFLICT(id) DO UPDATE SET json=excluded.json", string(sessions)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO host_image_tokens(host,hash) VALUES($1,$2)", "nas", auth.Hash(hostd)); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			host, credential string
			status           int
		}{{"nas", token, 200}, {"other", token, 403}, {"nas", other, 403}, {"nas", hostd, 403}, {"nas", "cli", 403}, {"nas", "session", 403}, {"nas", "idk_" + strings.Repeat("z", 43), 401}, {"nas", "invalid", 401}, {"nas", "", 401}} {
			r := httptest.NewRequest("GET", "http://localhost:8787/api/v1/hosts/"+tc.host+"/claude-api-key", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			switch tc.credential {
			case "cli":
				r.Header.Set("X-Desk-Key", "cli")
			case "session":
				r.AddCookie(&http.Cookie{Name: "desk_session", Value: "session"})
			default:
				if tc.credential != "" {
					r.Header.Set("Authorization", "Bearer "+tc.credential)
				}
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("host=%s: got %d want %d", tc.host, w.Code, tc.status)
			}
		}
		if _, err := appDB.Exec("DELETE FROM api_tokens WHERE id=?", "test-token"); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "http://localhost:8787/api/v1/hosts/nas/claude-api-key", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("revoked: %d", w.Code)
		}
	})
}
func TestClaudeAPIKeyDistributionLifecycle(t *testing.T) {
	s, store := keyFixture(t)
	other := &WorkerRoutes{options: s.options}
	pc := "idk_" + strings.Repeat("a", 43)
	key := "sk-ant-" + strings.Repeat("x", 25)
	previous := ""
	for _, state := range []string{"unregistered", "registered", "registered", "cleared"} {
		if state == "registered" {
			if err := store.Set(key); err != nil {
				t.Fatal(err)
			}
		}
		if state == "cleared" {
			if err := store.Clear(); err != nil {
				t.Fatal(err)
			}
		}
		for _, instance := range []*WorkerRoutes{s, other} {
			w := keyRequest(instance, "GET", "nas", pc)
			var v claudeapikey.Record
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.State != state {
				t.Fatalf("state %s: %d %s", state, w.Code, w.Body.String())
			}
			if state == "unregistered" && (v.Version != nil || v.Key != nil) || state == "registered" && (v.Version == nil || v.Key == nil || *v.Key != key) || state == "cleared" && (v.Version == nil || v.Key != nil) {
				t.Fatal("invalid state fields")
			}
			if instance == s && v.Version != nil {
				if *v.Version == previous {
					t.Fatal("version unchanged")
				}
				previous = *v.Version
			}
		}
	}
	if w := keyRequest(s, "POST", "nas", pc); w.Code != 405 {
		t.Fatalf("method: %d", w.Code)
	}
	if err := os.Chmod(store.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if w := keyRequest(s, "GET", "nas", pc); w.Code != 503 {
		t.Fatalf("store: %d", w.Code)
	}
	os.Chmod(store.Dir, 0700)
	if err := os.Remove(filepath.Join(store.Dir, claudeKeyReads)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.Dir, claudeKeyReads), 0700); err != nil {
		t.Fatal(err)
	}
	if w := keyRequest(s, "GET", "nas", pc); w.Code != 503 {
		t.Fatalf("journal: %d", w.Code)
	}
	if err := os.Remove(filepath.Join(store.Dir, claudeKeyReads)); err != nil {
		t.Fatal(err)
	}
	if w := keyRequest(s, "GET", "nas", pc); w.Code != 200 {
		t.Fatalf("journal recovery: %d", w.Code)
	}
}
func TestClaudeAPIKeyNoLeak(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	s, store := keyFixture(t)
	key := "sk-ant-" + strings.Repeat("x", 25)
	if err := store.Set(key); err != nil {
		t.Fatal(err)
	}
	pc := "idk_" + strings.Repeat("a", 43)
	w := keyRequest(s, "GET", "nas", pc)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["key"] != key {
		t.Fatal("missing key")
	}
	delete(body, "key")
	remaining, _ := json.Marshal(body)
	if strings.Contains(string(remaining), key) {
		t.Fatal("key outside key field")
	}
	for _, w := range []*httptest.ResponseRecorder{keyRequest(s, "GET", "other", pc), keyRequest(s, "GET", "nas", "invalid"), keyRequest(s, "POST", "nas", pc)} {
		if strings.Contains(w.Body.String(), key) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("leak or cache header")
		}
	}
	b, err := os.ReadFile(filepath.Join(store.Dir, claudeKeyReads))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err = json.Unmarshal(b, &row); err != nil || len(row) != 3 || row["host"] != "nas" || row["version"] != body["version"] || strings.Contains(string(b), key) {
		t.Fatalf("invalid record: %s", b)
	}
	at, err := time.Parse(time.RFC3339Nano, row["at"].(string))
	if err != nil || at.Location() != time.UTC {
		t.Fatal("record time is not UTC")
	}
	info, err := os.Stat(filepath.Join(store.Dir, claudeKeyReads))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("journal permissions")
	}
	if err := os.Chmod(store.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	failure := keyRequest(s, "GET", "nas", pc)
	if failure.Code != 503 || failure.Header().Get("Cache-Control") != "no-store" || strings.Contains(failure.Body.String(), key) || strings.Contains(logs.String(), key) {
		t.Fatal("failure leaked key or missed no-store")
	}
}
func TestClaudeAPIKeyContract(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	handler, _, token := workerFixtureOptions(t, false, func(options *WorkerRouteOptions) { options.ClaudeAPIKeyTokenFile = filepath.Join(dir, "token") })
	data, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	operation := spec["paths"].(map[string]any)["/api/v1/hosts/{host}/claude-api-key"].(map[string]any)["get"].(map[string]any)
	security := operation["security"].([]any)
	if len(security) != 1 || len(security[0].(map[string]any)) != 1 || security[0].(map[string]any)["pcToken"] == nil || operation["x-issue-desk-delegate"] != false {
		t.Fatal("unexpected security contract")
	}
	schema := operation["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:key-response", schema); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("urn:key-response")
	if err != nil {
		t.Fatal(err)
	}
	store := claudeapikey.New(filepath.Join(dir, "token"))
	for _, state := range []string{"unregistered", "registered", "cleared"} {
		if state == "registered" {
			if err := store.Set("sk-ant-" + strings.Repeat("x", 25)); err != nil {
				t.Fatal(err)
			}
		}
		if state == "cleared" {
			if err := store.Clear(); err != nil {
				t.Fatal(err)
			}
		}
		r := httptest.NewRequest("GET", "http://localhost:8787/api/v1/hosts/nas/claude-api-key", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		body, err := jsonschema.UnmarshalJSON(bytes.NewReader(w.Body.Bytes()))
		if w.Code != 200 || err != nil || compiled.Validate(body) != nil || body.(map[string]any)["state"] != state || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response does not match %s contract: status %d", state, w.Code)
		}
	}
}
