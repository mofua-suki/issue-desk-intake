package adminapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

type authStore struct{ *store.SQLite }

func (a authStore) HostdToken(context.Context, string) (auth.Token, bool, error) {
	return auth.Token{Name: "test-host"}, true, nil
}

type fixture struct {
	t                 *testing.T
	db                *store.SQLite
	pg                *sql.DB
	mux               *http.ServeMux
	now               time.Time
	directory, policy string
	options           Options
	token             map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	directory := t.TempDir()
	env := pgtest.NewIsolated(t)
	db, e := store.OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	previousDialect := appsql.SetActive(appsql.Postgres)
	t.Cleanup(func() { appsql.SetActive(previousDialect) })
	f := &fixture{t: t, db: db, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), directory: directory, policy: filepath.Join(directory, "ssh-policy.json"), token: map[string]string{}}
	for index, name := range []string{"worker", "delegate", "signer"} {
		token := "idk_" + strings.Repeat(string(rune('a'+index)), 43)
		f.token[name] = token
		_, e = db.AddPCToken(context.Background(), store.PCTokenRecord{ID: []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}[index], Name: name, Hash: auth.Hash(token), IssuedAt: f.now})
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, e = db.SetDelegated(context.Background(), "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", true, f.now); e != nil {
		t.Fatal(e)
	}
	if e = db.CreateSession(context.Background(), auth.Hash("screen-secret"), auth.Session{CSRF: "screen-csrf", ExpiresAt: f.now.Add(24 * time.Hour)}, f.now); e != nil {
		t.Fatal(e)
	}
	a, e := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787", Now: func() time.Time { return f.now }}, authStore{db})
	if e != nil {
		t.Fatal(e)
	}
	policy := map[string]any{"ttlMinutes": 5, "signer": map[string]any{"token": "signer"}, "hosts": map[string]any{"test-host": map[string]any{"principal": "issue-desk-repair-test-host", "user": "test", "machine": "test-machine.example.ts.net"}}, "operations": map[string]any{"status": "/bin/sh -c 'hostname'", "repair": "/bin/sh -c 'echo repair'"}}
	raw, _ := json.Marshal(policy)
	if e = os.WriteFile(f.policy, raw, 0600); e != nil {
		t.Fatal(e)
	}
	f.options = Options{AppDB: db.DB(), Auth: a, Now: func() time.Time { return f.now }, RuntimeDir: directory, Config: map[string]any{"hostSshPolicyFile": f.policy}, AllowProject: func(p string) bool { return p == "owner/repo" }}
	f.restart()
	return f
}
func (f *fixture) restart() {
	f.t.Helper()
	f.options.Postgres = f.pg
	f.mux = http.NewServeMux()
	if e := Register(f.mux, &f.options); e != nil {
		f.t.Fatal(e)
	}
}
func (f *fixture) call(method, path, credential string, data any, headers ...map[string]string) (int, any) {
	f.t.Helper()
	code, value, err := f.callResult(method, path, credential, data, headers...)
	if err != nil {
		f.t.Fatal(err)
	}
	return code, value
}

// Concurrent callers return errors to the test goroutine instead of calling FailNow.
func (f *fixture) callResult(method, path, credential string, data any, headers ...map[string]string) (int, any, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, nil, err
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/json")
	if credential == "screen" {
		r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen-secret"})
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		r.Header.Set("X-CSRF-Token", "screen-csrf")
	} else if credential == "cli" {
		key := randomID()
		if e := f.db.AddCLIKey(context.Background(), auth.Hash(key), f.now.Add(time.Minute)); e != nil {
			return 0, nil, e
		}
		r.Header.Set("X-Desk-Key", key)
		r.Header.Set("Origin", "http://127.0.0.1:8787")
	} else if credential == "hostd" {
		r.Header.Set("Authorization", "Bearer idh_"+strings.Repeat("x", 43))
	} else if credential != "" {
		r.Header.Set("Authorization", "Bearer "+f.token[credential])
	}
	for _, overrides := range headers {
		for k, v := range overrides {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	var result any
	if e := decodeJSON(w.Body.Bytes(), &result); e != nil {
		return w.Code, nil, fmt.Errorf("応答を読めません: status=%d: %w", w.Code, e)
	}
	return w.Code, result, nil
}
func status(t *testing.T, actual, want int, value any) {
	t.Helper()
	if actual != want {
		t.Fatalf("status=%d want=%d value=%v", actual, want, value)
	}
}
func TestDecisionsConcurrencyReceiptsMirrorAndRestart(t *testing.T) {
	f := newFixture(t)
	id := "11111111-1111-4111-8111-111111111111"
	input := map[string]any{"id": id, "text": "方針を保持する", "project": "owner/repo", "enabled": true}
	code, value := f.call("POST", "/api/decisions", "screen", input)
	status(t, code, 201, value)
	code, value = f.call("POST", "/api/decisions", "screen", input)
	status(t, code, 200, value)
	input["text"] = "違う内容"
	code, value = f.call("POST", "/api/decisions", "screen", input)
	status(t, code, 409, value)
	requestID := "22222222-2222-4222-8222-222222222222"
	save := map[string]any{"id": requestID, "revision": 0, "text": "更新する"}
	code, value = f.call("POST", "/api/decisions/"+id+"/save", "screen", save)
	status(t, code, 200, value)
	code, value = f.call("POST", "/api/decisions/"+id+"/save", "screen", save)
	status(t, code, 200, value)
	revision, _ := integer(obj(value)["revision"])
	if revision != 1 {
		t.Fatal("再送で版が進みました")
	}
	save["text"] = "書き換え"
	code, value = f.call("POST", "/api/decisions/"+id+"/save", "screen", save)
	status(t, code, 409, value)
	f.restart()
	code, value = f.call("GET", "/api/decisions/"+id, "screen", nil)
	status(t, code, 200, value)
	if obj(value)["text"] != "更新する" {
		t.Fatal(value)
	}
	code, value = f.call("POST", "/api/decisions/"+id+"/remove", "screen", map[string]any{"revision": 1})
	status(t, code, 200, value)
	code, value = f.call("POST", "/api/decisions/"+id+"/remove", "screen", map[string]any{"revision": 0})
	status(t, code, 200, value)
	code, value = f.call("GET", "/api/decisions/"+id, "screen", nil)
	status(t, code, 404, value)
	code, value = f.call("GET", "/api/decisions/export", "cli", nil)
	status(t, code, 200, value)
	if len(obj(value)["decisions"].([]any)) != 6 {
		t.Fatal("削除済みまたは方針が書き出しから欠けています")
	}
	mirror := filepath.Join(f.directory, "state", "intake-decisions.json")
	raw, e := os.ReadFile(mirror)
	if e != nil || !bytes.Contains(raw, []byte("removedAt")) {
		t.Fatalf("鏡写し: %v", e)
	}
	code, value = f.call("GET", "/api/decisions/"+id, "cli", nil)
	status(t, code, 403, value)
}
func TestCredentialAndCSRFLimits(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/tokens", "/api/decisions", "/api/push/key"} {
		for _, credential := range []string{"worker", "delegate", "hostd"} {
			code, value := f.call("GET", path, credential, nil)
			status(t, code, 403, value)
		}
	}
	for _, credential := range []string{"screen", "worker", "cli", "hostd"} {
		code, value := f.call("GET", "/api/ai-decisions", credential, nil)
		status(t, code, 403, value)
	}
	for _, headers := range []map[string]string{{"X-CSRF-Token": "wrong"}, {"Origin": "https://other.invalid"}} {
		code, value := f.call("POST", "/api/decisions", "screen", map[string]any{}, headers)
		status(t, code, 403, value)
	}
	code, value := f.call("POST", "/api/ssh-tickets/11111111-1111-4111-8111-111111111111/approve", "cli", map[string]any{})
	status(t, code, 403, value)
	code, value = f.call("GET", "/api/push/key", "screen", nil, map[string]string{"X-Desk-Key": ""})
	status(t, code, 403, value)
}
func TestAIDecisionsAndTokenDelegation(t *testing.T) {
	f := newFixture(t)
	input := map[string]any{"id": "decision-1", "at": iso(f.now), "who": "implementation", "pc": "test-host", "project": "owner/repo", "kind": "test-choice", "context": "状況", "options": []any{"一", "二"}, "chosen": "一", "reason": "根拠", "expected": "想定", "check": "検証"}
	code, value := f.call("POST", "/api/ai-decisions", "delegate", input)
	status(t, code, 200, value)
	if obj(value)["created"] != true {
		t.Fatal(value)
	}
	code, value = f.call("POST", "/api/ai-decisions", "delegate", input)
	status(t, code, 200, value)
	if obj(value)["created"] != false {
		t.Fatal(value)
	}
	code, value = f.call("POST", "/api/ai-decisions/decision-1/score", "delegate", map[string]any{"score": "hit", "outcome": "結果", "lesson": "教訓"})
	status(t, code, 200, value)
	f.restart()
	code, value = f.call("GET", "/api/ai-decisions/stats", "delegate", nil)
	status(t, code, 200, value)
	if obj(value.([]any)[0])["rate"] != json.Number("1") {
		t.Fatal(value)
	}
	code, value = f.call("POST", "/api/tokens/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/delegate", "worker", map[string]any{"delegate": true})
	status(t, code, 403, value)
	code, value = f.call("POST", "/api/tokens/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/delegate", "screen", map[string]any{"delegate": true})
	status(t, code, 200, value)
	if obj(value)["delegate"] != true {
		t.Fatal(value)
	}
	if _, e := f.db.RevokePCToken(context.Background(), "worker"); e != nil {
		t.Fatal(e)
	}
	code, value = f.call("POST", "/api/tokens/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/delegate", "screen", map[string]any{"delegate": true})
	status(t, code, 404, value)
}

func TestDecisionListsPagination(t *testing.T) {
	f := newFixture(t)
	for _, input := range []map[string]any{
		{"id": "b", "at": "2026-11-01T00:00:00Z", "who": "chat", "kind": "choice"},
		{"id": "a", "at": "2026-11-01T00:00:00Z", "who": "chat", "kind": "choice"},
		{"id": "c", "at": "2026-11-02T00:00:00Z", "who": "review", "kind": "other"},
	} {
		for k, v := range map[string]any{"pc": "test-host", "project": "owner/repo", "context": "context", "options": []any{"one"}, "chosen": "one", "reason": "reason", "expected": "expected", "check": "check"} {
			input[k] = v
		}
		code, value := f.call("POST", "/api/ai-decisions", "delegate", input)
		status(t, code, 200, value)
	}
	for i, expected := range []string{"a", "b", ""} {
		code, value := f.call("GET", "/api/ai-decisions?kind=choice&who=chat&limit=1&page="+string(rune('1'+i)), "delegate", nil)
		status(t, code, 200, value)
		page := obj(value)
		items := page["items"].([]any)
		if page["total"] != json.Number("2") || page["hasMore"] != (i == 0) || len(items) != map[bool]int{true: 1, false: 0}[i < 2] {
			t.Fatal(page)
		}
		if expected != "" && obj(items[0])["id"] != expected {
			t.Fatal(page)
		}
	}
	ids := []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}
	for _, id := range ids {
		code, value := f.call("POST", "/api/decisions", "screen", map[string]any{"id": id, "text": "test", "project": "owner/repo"})
		status(t, code, 201, value)
	}
	code, value := f.call("POST", "/api/decisions/"+ids[1]+"/remove", "screen", map[string]any{})
	status(t, code, 200, value)
	code, value = f.call("GET", "/api/decisions?project=owner%2Frepo&limit=1", "cli", nil)
	status(t, code, 200, value)
	first := obj(value)
	total := first["total"].(json.Number)
	if total != json.Number("7") || obj(first["items"].([]any)[0])["id"] != ids[2] || first["hasMore"] != true {
		t.Fatal(first)
	}
	code, value = f.call("GET", "/api/decisions?project=owner%2Frepo&limit=1&page=2", "cli", nil)
	status(t, code, 200, value)
	if obj(value)["total"] != total || obj(obj(value)["items"].([]any)[0])["id"] != ids[0] {
		t.Fatal(value)
	}
	// The shipped seed records are included in project-filtered results; deletion must reduce the total by one.
	code, value = f.call("GET", "/api/decisions?project=owner%2Frepo&limit=200", "screen", nil)
	status(t, code, 200, value)
	for _, item := range obj(value)["items"].([]any) {
		if obj(item)["id"] == ids[1] {
			t.Fatal(value)
		}
	}
	code, value = f.call("GET", "/api/decisions?project=owner%2Frepo&limit=1&page=7", "cli", nil)
	status(t, code, 200, value)
	if len(obj(value)["items"].([]any)) != 1 || obj(value)["hasMore"] != false {
		t.Fatal(value)
	}
	code, value = f.call("GET", "/api/decisions?page=999999999999999999&limit=1", "cli", nil)
	status(t, code, 200, value)
	if len(obj(value)["items"].([]any)) != 0 || obj(value)["hasMore"] != false {
		t.Fatal(value)
	}
	for _, path := range []string{"/api/ai-decisions", "/api/decisions"} {
		credential := "delegate"
		if path == "/api/decisions" {
			credential = "cli"
		}
		for _, q := range []string{"limit=0", "limit=201", "limit=abc", "limit=", "page=0", "page=-1", "page=1&page=2", "page=999999999999999999999999"} {
			code, value := f.call("GET", path+"?"+q, credential, nil)
			status(t, code, 400, value)
		}
	}
	for _, path := range []string{"/api/ai-decisions?kind=BAD", "/api/ai-decisions?who=bad", "/api/decisions?project=unknown"} {
		credential := "delegate"
		if strings.HasPrefix(path, "/api/decisions?") {
			credential = "cli"
		}
		code, value := f.call("GET", path, credential, nil)
		status(t, code, 400, value)
	}
}
