package screenapi

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestLocalCLIReadsScreenListProjection(t *testing.T) {
	f := newFixture(t)
	source := f.s.o.Lists["draft"]
	f.s.o.Lists["draft"] = func(ctx context.Context, origin string) ([]Object, error) {
		rows, err := source(ctx, origin)
		return append(rows, Object{"id": otherID, "project": "outside/repo", "title": "非許可の相談"}), err
	}
	call := func(method, path, key, remote string) (int, Object) {
		t.Helper()
		r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, nil)
		r.RemoteAddr = remote
		if key != "" {
			r.Header.Set("X-Desk-Key", key)
		}
		if method != "GET" {
			r.Header.Set("Origin", "http://127.0.0.1:8787")
		}
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		var result Object
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Code, result
	}
	issueKey := func() string {
		t.Helper()
		key := "list-read-key"
		if err := f.db.AddCLIKey(context.Background(), auth.Hash(key), f.now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		return key
	}
	for _, route := range []string{"/api/list", "/api/list?tabs=active", "/api/list/entry?type=draft&id=" + fixtureID} {
		t.Run(route, func(t *testing.T) {
			screenCode, screenValue := f.call("GET", route, "screen", nil)
			code, value := call("GET", route, issueKey(), "127.0.0.1:1234")
			if code != 200 || screenCode != 200 {
				t.Fatal(code, value, screenCode, screenValue)
			}
			screen := fleet.ObjectOrEmpty(screenValue)
			for _, field := range []string{"rows", "aux", "scope", "item"} {
				if !reflect.DeepEqual(value[field], screen[field]) {
					t.Fatalf("画面と一時キーで%sが不一致: %v / %v", field, value[field], screen[field])
				}
			}
			if route == "/api/list" {
				code, reset := call("GET", "/api/list?since="+fleet.Text(screen["cursor"]), issueKey(), "127.0.0.1:1234")
				if code != 200 || reset["full"] != true || reset["reset"] != true {
					t.Fatal("画面のカーソルを一時キーで共有した", code, reset)
				}
				if len(fleet.Items(value["rows"])) != 1 {
					t.Fatal("許可外の相談を返した", value)
				}
				code, delta := call("GET", "/api/list?since="+fleet.Text(value["cursor"]), issueKey(), "127.0.0.1:1234")
				if code != 200 || delta["full"] != false || len(fleet.Items(delta["added"])) != 0 || len(fleet.Items(delta["updated"])) != 0 || len(fleet.Items(delta["removed"])) != 0 {
					t.Fatal("差分の照合に失敗", code, delta)
				}
			}
			if code, value := call("GET", route, "list-read-key", "127.0.0.1:1234"); code != 403 {
				t.Fatal("使用済みキーを受け付けた", code, value)
			}
			if code, value := call("GET", route, "", "127.0.0.1:1234"); code != 401 {
				t.Fatal("未認証の読取を許可した", code, value)
			}
			if code, value := f.call("GET", route, "token", nil); code != 403 {
				t.Fatal("PCトークンの許可を広げた", code, value)
			}
		})
	}
	if code, value := call("POST", "/api/list/timing", issueKey(), "127.0.0.1:1234"); code != 403 {
		t.Fatal("一時キーで画面の計測を送信した", code, value)
	}
	if code, value := call("GET", "/api/list", issueKey(), "192.0.2.1:1234"); code != 401 {
		t.Fatal("リモートの一時キーを受け付けた", code, value)
	}
}
