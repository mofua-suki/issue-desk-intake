package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

type fixture struct {
	store   *store.SQLite
	handler http.Handler
	now     time.Time
}

func setup(t *testing.T) fixture {
	t.Helper()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	env := pgtest.NewIsolated(t)
	s, err := store.OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a, err := auth.New(auth.Config{Port: 8787, PublicURL: "https://desk.example", Now: func() time.Time { return now }}, &store.AuthStore{SQLite: s})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := Register(mux, Options{Auth: a, Store: s, PublicURL: "https://desk.example", Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	return fixture{s, mux, now}
}

func call(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:50000"
	if method != "GET" {
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func code(t *testing.T, f fixture) string {
	t.Helper()
	key := "one-use-local-key"
	if err := f.store.AddCLIKey(context.Background(), auth.Hash(key), f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	w := call(f.handler, "POST", "/api/pair-code", "{}", map[string]string{"X-Desk-Key": key})
	if w.Code != 200 {
		t.Fatalf("pair code: %d %s", w.Code, w.Body)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Code
}

func TestPairingCreatesPersistentScreenSessionAndConsumesCode(t *testing.T) {
	f := setup(t)
	c := code(t, f)
	w := call(f.handler, "POST", "/api/pair", `{"code":"`+c+`"}`, nil)
	if w.Code != 200 {
		t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("invalid screen cookie")
	}
	saved, found, err := f.store.Session(context.Background(), auth.Hash(cookies[0].Value))
	if err != nil || !found || saved.CSRF == "" || !saved.ExpiresAt.Equal(f.now.Add(30*24*time.Hour)) {
		t.Fatalf("session was not persisted: %v %v", found, err)
	}
	w = call(f.handler, "GET", "/api/session", "", map[string]string{"Cookie": "desk_session=" + cookies[0].Value})
	var state struct {
		Authenticated bool   `json:"authenticated"`
		CSRF          string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &state)
	if w.Code != 200 || !state.Authenticated || state.CSRF != saved.CSRF {
		t.Fatalf("session lookup: %d %s", w.Code, w.Body)
	}
	if got := call(f.handler, "POST", "/api/pair", `{"code":"`+c+`"}`, nil).Code; got != 403 {
		t.Fatalf("reused pair code: %d", got)
	}
}

func TestDesktopLinkIsConsumedOnceAcrossConcurrentRequests(t *testing.T) {
	f := setup(t)
	if err := f.store.PutSetting(context.Background(), "desktopToken", map[string]any{"hash": auth.Hash("desktop-key"), "expires": f.now.Add(time.Minute).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan int, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- call(f.handler, "POST", "/api/desktop", `{"key":"desktop-key"}`, nil).Code
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for result := range results {
		if result == 200 {
			success++
		} else if result != 403 {
			t.Fatalf("unexpected desktop result: %d", result)
		}
	}
	if success != 1 {
		t.Fatalf("desktop key accepted %d times", success)
	}
}

func TestPairingRequiresOriginAndLocalIssuanceAndLimitsGuesses(t *testing.T) {
	f := setup(t)
	if w := call(f.handler, "POST", "/api/pair", "{}", map[string]string{"Origin": "https://elsewhere.example"}); w.Code != 403 {
		t.Fatalf("cross-origin accepted: %d", w.Code)
	}
	c := code(t, f)
	for i := 0; i < 8; i++ {
		if w := call(f.handler, "POST", "/api/pair", `{"code":"invalid"}`, nil); w.Code != 403 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	if w := call(f.handler, "POST", "/api/pair", `{"code":"`+c+`"}`, nil); w.Code != 429 {
		t.Fatalf("guess limit not enforced: %d", w.Code)
	}
	if err := f.store.AddCLIKey(context.Background(), auth.Hash("local-key"), f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if w := call(f.handler, "POST", "/api/pair-code", "{}", map[string]string{"X-Desk-Key": "local-key", "X-Forwarded-For": "127.0.0.1"}); w.Code != 401 {
		t.Fatalf("forwarded request gained local privilege: %d", w.Code)
	}
}

func TestBearerNeverFallsBackToSessionOrPublicPairing(t *testing.T) {
	f := setup(t)
	if w := call(f.handler, "GET", "/api/session", "", map[string]string{"Authorization": "Bearer invalid"}); w.Code != 401 {
		t.Fatalf("invalid bearer bypassed: %d", w.Code)
	}
	if w := call(f.handler, "POST", "/api/pair", `{"code":"invalid"}`, map[string]string{"Authorization": ""}); w.Code != 401 {
		t.Fatalf("empty authorization bypassed: %d", w.Code)
	}
}
