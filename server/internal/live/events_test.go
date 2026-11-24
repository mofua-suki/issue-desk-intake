package live

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

type sessionStore struct{}

func (sessionStore) PCToken(context.Context, string) (auth.Token, bool, error) {
	return auth.Token{}, false, nil
}
func (sessionStore) TouchPCToken(context.Context, string, time.Time) error { return nil }
func (sessionStore) HostdToken(context.Context, string) (auth.Token, bool, error) {
	return auth.Token{}, false, nil
}
func (sessionStore) Session(_ context.Context, hash string) (auth.Session, bool, error) {
	return auth.Session{ExpiresAt: time.Now().Add(time.Hour)}, hash == auth.Hash("test-session"), nil
}
func (sessionStore) ConsumeCLIKey(context.Context, string, time.Time) (bool, error) {
	return false, nil
}

func TestEventsContainOnlyIdentifiersAndOverflowCollapsesToList(t *testing.T) {
	e := New(context.Background(), nil)
	defer e.Close()
	c := &client{pending: map[string]map[string]any{}, wake: make(chan struct{}, 1)}
	e.clients[c] = true
	e.Publish(map[string]any{"type": "draft", "id": "11111111-1111-4111-8111-111111111111", "text": "機密の本文", "token": "secret"})
	if len(c.pending) != 1 {
		t.Fatal("missing event")
	}
	for _, row := range c.pending {
		if len(row) != 2 || row["text"] != nil {
			t.Fatal("private fields leaked")
		}
	}
	for n := int64(1); n <= 51; n++ {
		e.Publish(map[string]any{"type": "issue", "project": "owner/app", "number": n})
	}
	if len(c.pending) != 1 || c.pending[`{"type":"list"}`]["type"] != "list" {
		t.Fatal("overflow not bounded")
	}
	e.Publish(map[string]any{"type": "issue", "project": "invalid", "number": json.Number("1")})
	if len(c.pending) != 1 {
		t.Fatal("invalid event accepted")
	}
	e.Close()
	e.Publish(map[string]any{"type": "todo", "id": "11111111-1111-4111-8111-111111111111"})
	if len(c.pending) != 1 {
		t.Fatal("closed publisher changed pending events")
	}
}

func TestSSEAuthenticationAndConnectionLimit(t *testing.T) {
	ctx := context.Background()
	a, err := auth.New(auth.Config{PublicURL: "http://localhost:8787"}, sessionStore{})
	if err != nil {
		t.Fatal(err)
	}
	e := New(ctx, a)
	defer e.Close()
	server := httptest.NewServer(e)
	defer server.Close()
	request := func(session bool) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		req.Host = "localhost:8787"
		if session {
			req.AddCookie(&http.Cookie{Name: "desk_session", Value: "test-session"})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	unauthorized := request(false)
	defer unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status %d", unauthorized.StatusCode)
	}
	for i := 0; i < 4; i++ {
		resp := request(true)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
			t.Fatalf("connection %d: %s %q", i, resp.Status, resp.Header.Get("Content-Type"))
		}
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil || line != "retry: 3000\n" {
			t.Fatalf("retry line %q: %v", line, err)
		}
	}
	limited := request(true)
	defer limited.Body.Close()
	if limited.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("fifth connection status %d", limited.StatusCode)
	}
}
