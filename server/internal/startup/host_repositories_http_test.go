package startup

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
)

// Exercise the registered application, including screen authentication and its real snapshot loader.
func checkHostRepositoriesListHTTP(t *testing.T, dir string, db *sql.DB, call func(string, string, string, string) *httptest.ResponseRecorder) {
	t.Helper()
	write := func(repos map[string]any) {
		writeConfig(t, filepath.Join(dir, "assignments.json"), map[string]any{"pollSeconds": 300, "hosts": map[string]any{
			"nas":   map[string]any{"maxImplementations": 2, "repositories": repos},
			"empty": map[string]any{"maxImplementations": 1},
		}, "repositories": map[string]any{}})
	}
	write(map[string]any{"owner/z": map[string]any{"rank": 2}, "owner/b": map[string]any{"rank": 1}, "owner/a": map[string]any{"rank": 1}})
	if _, err := db.Exec(`INSERT INTO hosts(host,heartbeat_at) VALUES ('record-only',now())`); err != nil {
		t.Fatal(err)
	}
	check := func(path string, want []any, full bool) string {
		t.Helper()
		response := call("GET", path, "screen", "")
		var body map[string]any
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatalf("list: %d %s", response.Code, response.Body.String())
		}
		if body["full"] != full {
			t.Fatalf("full=%v response=%s", body["full"], response.Body.String())
		}
		hostView, ok := body["hosts"].(map[string]any)
		if !ok {
			t.Fatalf("missing hosts: %s", response.Body.String())
		}
		rows, ok := hostView["hosts"].([]any)
		if !ok {
			t.Fatalf("missing rows: %s", response.Body.String())
		}
		found := map[string]any{}
		for _, raw := range rows {
			row := raw.(map[string]any)
			if value, exists := row["repositories"]; exists {
				found[row["name"].(string)] = value
			}
		}
		for name, expected := range map[string]any{"nas": want, "empty": []any{}, "record-only": nil} {
			value, exists := found[name]
			if !exists || !reflect.DeepEqual(value, expected) {
				t.Fatalf("%s repositories=%#v, want %#v", name, value, expected)
			}
		}
		cursor, ok := body["cursor"].(string)
		if !ok || cursor == "" {
			t.Fatalf("missing cursor: %s", response.Body.String())
		}
		return cursor
	}
	cursor := check("/api/list", []any{"owner/a", "owner/b", "owner/z"}, true)
	write(map[string]any{"owner/new": map[string]any{"rank": 1}})
	check("/api/list?since="+url.QueryEscape(cursor), []any{"owner/new"}, false)
	write(map[string]any{"owner/repo": map[string]any{"rank": 1}})
}
