package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

type stopCheckAuthStore struct{}

func (stopCheckAuthStore) PCToken(_ context.Context, hash string) (auth.Token, bool, error) {
	if hash == auth.Hash("idk_"+strings.Repeat("c", 43)) {
		return auth.Token{ID: "bad-name", Name: "PC 日本語"}, true, nil
	}
	return auth.Token{ID: "pc-token", Name: "pc-1"}, hash == auth.Hash("idk_"+strings.Repeat("a", 43)), nil
}
func (stopCheckAuthStore) TouchPCToken(context.Context, string, time.Time) error { return nil }
func (stopCheckAuthStore) HostdToken(_ context.Context, hash string) (auth.Token, bool, error) {
	return auth.Token{ID: "hostd-token", Name: "pc-1"}, hash == auth.Hash("idh_"+strings.Repeat("b", 43)), nil
}
func (stopCheckAuthStore) Session(_ context.Context, hash string) (auth.Session, bool, error) {
	return auth.Session{CSRF: "csrf", ExpiresAt: time.Now().Add(time.Hour)}, hash == auth.Hash("session"), nil
}
func (stopCheckAuthStore) ConsumeCLIKey(_ context.Context, hash string, _ time.Time) (bool, error) {
	return hash == auth.Hash("key"), nil
}

func TestStopCheckEndpoint(t *testing.T) {
	a, err := auth.New(auth.Config{Port: 8787}, stopCheckAuthStore{})
	if err != nil {
		t.Fatal(err)
	}
	handler, db, _ := workerFixtureOptions(t, false, func(o *WorkerRouteOptions) { o.Auth = a })
	request := func(credential, body string) int {
		r := httptest.NewRequest("POST", "http://localhost:8787/api/v1/stop-check-fires", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Origin", "http://localhost:8787")
		r.Header.Set("Content-Type", "application/json")
		switch credential {
		case "pc":
			r.Header.Set("Authorization", "Bearer idk_"+strings.Repeat("a", 43))
		case "bad-name":
			r.Header.Set("Authorization", "Bearer idk_"+strings.Repeat("c", 43))
		case "hostd":
			r.Header.Set("Authorization", "Bearer idh_"+strings.Repeat("b", 43))
		case "cli":
			r.Header.Set("X-Desk-Key", "key")
		case "screen":
			r.AddCookie(&http.Cookie{Name: "desk_session", Value: "session"})
			r.Header.Set("X-CSRF-Token", "csrf")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("pc", "[]"); got != 200 {
		t.Fatalf("empty: %d", got)
	}
	record := `{"id":"123e4567-e89b-12d3-a456-426614174000","checkId":"unasked-send","result":"block","at":"2026-11-05T12:34:56Z","project":"project-1"}`
	for _, cred := range []string{"screen", "hostd"} {
		if got := request(cred, "["+record+"]"); got != 403 {
			t.Fatalf("%s: %d", cred, got)
		}
	}
	for _, cred := range []string{"pc", "pc", "cli"} {
		if got := request(cred, "["+record+"]"); got != 200 {
			t.Fatalf("%s: %d", cred, got)
		}
	}
	cliRecord := strings.Replace(record, "174000", "174001", 1)
	if got := request("cli", "["+cliRecord+"]"); got != 200 {
		t.Fatal(got)
	}
	var count int
	var host, row string
	if err := db.QueryRow(`SELECT count(*), min(host), min(row_to_json(r)::text) FROM stop_check_records r WHERE id='123e4567-e89b-12d3-a456-426614174000'`).Scan(&count, &host, &row); err != nil || count != 1 || host != "pc-1" {
		t.Fatalf("row count=%d host=%s err=%v", count, host, err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(row), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"body", "reason", "path"} {
		if _, found := fields[key]; found {
			t.Fatalf("stored %s", key)
		}
	}
	if err := db.QueryRow(`SELECT host FROM stop_check_records WHERE id='123e4567-e89b-12d3-a456-426614174001'`).Scan(&host); err != nil || host != "nas" {
		t.Fatalf("cli host=%s err=%v", host, err)
	}
	validNew := strings.Replace(record, "174000", "174002", 1)
	for _, bad := range []string{
		strings.Replace(record, "unasked-send", "unknown", 1), strings.Replace(record, "block", "invalid", 1), strings.Replace(record, "174000", "bad", 1), strings.Replace(record, "2026-11-05T12:34:56Z", "bad", 1),
		strings.TrimSuffix(record, "}") + `,"host":"spoof"}`, strings.TrimSuffix(record, "}") + `,"body":"secret"}`, strings.TrimSuffix(record, "}") + `,"reason":"secret"}`, strings.TrimSuffix(record, "}") + `,"path":"secret"}`, strings.Replace(record, `"project":"project-1"`, `"project":null`, 1),
		strings.Replace(record, `"id":`, `"id":"123e4567-e89b-12d3-a456-426614174000","id":`, 1),
		strings.Replace(record, `"id":`, `"\u0069d":"123e4567-e89b-12d3-a456-426614174000","id":`, 1),
	} {
		if got := request("pc", "["+validNew+","+bad+"]"); got != 400 {
			t.Fatalf("invalid: %d", got)
		}
	}
	for _, body := range []string{`null`, `{}`, `[null]`, "[" + strings.Repeat(record+",", 1000) + record + "]", "[" + strings.Repeat(" ", stopCheckBodyLimit) + "]"} {
		if got := request("pc", body); got != 400 {
			t.Fatalf("boundary: %d", got)
		}
	}
	if err := db.QueryRow(`SELECT count(*) FROM stop_check_records`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("invalid batch persisted: count=%d err=%v", count, err)
	}
	if got := request("pc", "["+validNew+"]"); got != 200 {
		t.Fatalf("isolated valid record: %d", got)
	}
	if err := db.QueryRow(`SELECT count(*) FROM stop_check_records WHERE id='123e4567-e89b-12d3-a456-426614174002'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("isolated valid record count=%d err=%v", count, err)
	}

	badNameRecord := strings.Replace(record, "174000", "174003", 1)
	if got := request("bad-name", "["+badNameRecord+"]"); got != 200 {
		t.Fatalf("invalid PC name: %d", got)
	}
	if err := db.QueryRow(`SELECT host FROM stop_check_records WHERE id='123e4567-e89b-12d3-a456-426614174003'`).Scan(&host); err != nil || host != "other" {
		t.Fatalf("normalized host=%q err=%v", host, err)
	}
	batch := make([]string, 1000)
	for i := range batch {
		batch[i] = strings.Replace(record, "123e4567-e89b-12d3-a456-426614174000", fmt.Sprintf("00000000-0000-0000-0000-%012d", i), 1)
	}
	if got := request("pc", "["+strings.Join(batch, ",")+"]"); got != 200 {
		t.Fatalf("1000 records: %d", got)
	}
	if err := db.QueryRow(`SELECT count(*) FROM stop_check_records`).Scan(&count); err != nil || count != 1004 {
		t.Fatalf("1000 records count=%d err=%v", count, err)
	}
}
