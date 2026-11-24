package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const publicURL = "https://desk.example.test"

type testStore struct {
	pc       map[string]Token
	hostd    map[string]Token
	sessions map[string]Session
	keys     map[string]time.Time
	err      error
	touched  []string
	lookups  int
}

func (s *testStore) PCToken(_ context.Context, hash string) (Token, bool, error) {
	s.lookups++
	t, ok := s.pc[hash]
	return t, ok, s.err
}
func (s *testStore) TouchPCToken(_ context.Context, id string, _ time.Time) error {
	s.touched = append(s.touched, id)
	return s.err
}
func (s *testStore) HostdToken(_ context.Context, hash string) (Token, bool, error) {
	s.lookups++
	t, ok := s.hostd[hash]
	return t, ok, s.err
}
func (s *testStore) Session(_ context.Context, hash string) (Session, bool, error) {
	s.lookups++
	session, ok := s.sessions[hash]
	return session, ok, s.err
}
func (s *testStore) ConsumeCLIKey(_ context.Context, hash string, now time.Time) (bool, error) {
	s.lookups++
	expires, ok := s.keys[hash]
	if s.err != nil {
		return false, s.err
	}
	if !ok || expires.Before(now) {
		return false, nil
	}
	delete(s.keys, hash)
	return true, nil
}

func fixture(t *testing.T) (*Authenticator, *testStore) {
	t.Helper()
	s := &testStore{
		pc: map[string]Token{}, hostd: map[string]Token{},
		sessions: map[string]Session{}, keys: map[string]time.Time{},
	}
	a, err := New(Config{PublicURL: publicURL, Port: 8787, InternalHost: "intake:8787", Now: func() time.Time { return fixedNow }}, s)
	if err != nil {
		t.Fatal(err)
	}
	return a, s
}

func request(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "192.0.2.10:1234"
	return r
}

func TestBearerCredentialIsolationAndNoFallback(t *testing.T) {
	a, store := fixture(t)
	pcToken := "idk_" + strings.Repeat("a", 43)
	hostToken := "idh_" + strings.Repeat("b", 43)
	store.pc[Hash(pcToken)] = Token{ID: "pc-id", Name: "nas", Delegated: true}
	store.hostd[Hash(hostToken)] = Token{Name: "nas"}
	store.sessions[Hash("session-key")] = Session{CSRF: "csrf", ExpiresAt: fixedNow.Add(time.Hour)}
	for _, test := range []struct {
		token string
		kind  Kind
	}{{pcToken, PC}, {hostToken, Hostd}} {
		r := request(http.MethodGet, publicURL+"/api/v1/hosts/nas")
		r.Header.Set("Authorization", "Bearer "+test.token)
		p, problem := a.Authenticate(r)
		if problem != nil || p.Kind != test.kind || p.Name != "nas" {
			t.Fatalf("wrong authenticated identity: %+v %v", p, problem)
		}
		if test.kind == Hostd && p.Delegated {
			t.Fatal("hostd inherited delegation")
		}
	}
	if len(store.touched) != 1 || store.touched[0] != "pc-id" {
		t.Fatal("PC last-used update missing")
	}
	for _, authorization := range []string{"", "Basic abc", "Bearer invalid", "Bearer idk_" + strings.Repeat("z", 43)} {
		r := request(http.MethodGet, publicURL+"/api/issue")
		r.Header.Set("Authorization", authorization)
		r.AddCookie(&http.Cookie{Name: "desk_session", Value: "session-key"})
		_, problem := a.Authenticate(r)
		if problem == nil || problem.Status != 401 || problem.SafeToChange {
			t.Fatalf("credential fallback accepted: %v", problem)
		}
		if strings.Contains(problem.Message, authorization) && authorization != "" {
			t.Fatal("credential leaked in public error")
		}
	}
}

func TestSessionRequiresOriginAndCSRFForMutations(t *testing.T) {
	a, store := fixture(t)
	store.sessions[Hash("good-session")] = Session{CSRF: "csrf", ExpiresAt: fixedNow.Add(time.Hour)}
	for _, test := range []struct {
		method, origin, csrf string
		status               int
	}{
		{http.MethodGet, "", "", 0},
		{http.MethodPost, publicURL, "csrf", 0},
		{http.MethodPost, "", "csrf", 403},
		{http.MethodPost, "https://other.example", "csrf", 403},
		{http.MethodPost, publicURL, "", 403},
		{http.MethodPost, publicURL, "wrong", 403},
	} {
		r := request(test.method, publicURL+"/api/comment")
		r.AddCookie(&http.Cookie{Name: "desk_session", Value: "good-session"})
		if test.origin != "" {
			r.Header.Set("Origin", test.origin)
		}
		if test.csrf != "" {
			r.Header.Set("X-CSRF-Token", test.csrf)
		}
		principal, problem := a.Authenticate(r)
		if test.status == 0 {
			if problem != nil || principal.Kind != Screen {
				t.Fatalf("valid session refused: %v", problem)
			}
		} else if problem == nil || problem.Status != test.status {
			t.Fatalf("unsafe mutation accepted: %v", problem)
		}
	}
	store.sessions[Hash("good-session")] = Session{CSRF: "csrf", ExpiresAt: fixedNow}
	r := request(http.MethodGet, publicURL+"/api/session")
	r.AddCookie(&http.Cookie{Name: "desk_session", Value: "good-session"})
	if _, problem := a.Authenticate(r); problem == nil || problem.Status != 401 {
		t.Fatal("expired session accepted")
	}
}

func TestLocalCLIKeyIsOneUseAndCannotBeForwarded(t *testing.T) {
	a, store := fixture(t)
	for _, forwarding := range []string{"Forwarded", "X-Forwarded-For", "Tailscale-User-Login"} {
		store.keys[Hash("local-key")] = fixedNow.Add(time.Minute)
		r := request(http.MethodGet, "http://localhost:8787/api/todos")
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Desk-Key", "local-key")
		r.Header.Set(forwarding, "")
		if _, problem := a.Authenticate(r); problem == nil {
			t.Fatalf("forwarded key accepted with %s", forwarding)
		}
		if _, ok := store.keys[Hash("local-key")]; !ok {
			t.Fatal("untrusted request consumed a local key")
		}
	}
	r := request(http.MethodPost, "http://localhost:8787/api/todos")
	r.RemoteAddr = "[::1]:1234"
	r.Header.Set("Origin", "http://localhost:8787")
	r.Header.Set("X-Desk-Key", "local-key")
	principal, problem := a.Authenticate(r)
	if problem != nil || principal.Kind != CLI {
		t.Fatalf("local key refused: %v", problem)
	}
	if _, problem := a.Authenticate(r); problem == nil || problem.Status != 403 {
		t.Fatal("local key replay accepted")
	}
}

func TestHostAndInternalRouting(t *testing.T) {
	a, store := fixture(t)
	for _, target := range []string{"https://wrong.example/api/issues", "http://intake:8787/", "http://intake:8787/api/v1/jobs"} {
		r := request(http.MethodGet, target)
		if _, problem := a.Context(r); problem == nil || problem.Status != 403 {
			t.Fatalf("untrusted host accepted: %s", target)
		}
	}
	token := "idk_" + strings.Repeat("c", 43)
	store.pc[Hash(token)] = Token{ID: "pc-id", Name: "nas"}
	r := request(http.MethodPost, "http://intake:8787/api/v1/jobs/next")
	r.Header.Set("Authorization", "Bearer "+token)
	if _, problem := a.Authenticate(r); problem != nil {
		t.Fatal(problem)
	}
	r.Header.Set("Origin", publicURL)
	if _, problem := a.Authenticate(r); problem == nil || problem.Status != 403 {
		t.Fatal("cross-origin bearer accepted")
	}
}

func TestUnavailableStoreFailsClosed(t *testing.T) {
	if _, err := New(Config{}, nil); err == nil {
		t.Fatal("nil auth store accepted")
	}
	a, store := fixture(t)
	store.err = errors.New("backend contains sensitive diagnostics")
	r := request(http.MethodGet, publicURL+"/api/issue")
	r.Header.Set("Authorization", "Bearer idk_"+strings.Repeat("a", 43))
	_, problem := a.Authenticate(r)
	if problem == nil || problem.Status != 503 || strings.Contains(problem.Message, "sensitive") {
		t.Fatalf("unsafe backend failure: %v", problem)
	}
}

func TestPoliciesDenyByDefaultAndPreserveScreenApproval(t *testing.T) {
	for _, principal := range []Principal{{Kind: PC, Name: "nas"}, {Kind: PC, Name: "nas", Delegated: true}, {Kind: Hostd, Name: "nas"}, {Kind: Screen}, {Kind: CLI}} {
		if Authorize(principal, Policy{}) == nil {
			t.Fatal("empty policy allowed request")
		}
		problem := Authorize(principal, Policy{Screen: true})
		if (problem == nil) != (principal.Kind == Screen) {
			t.Fatalf("screen approval widened: %+v", principal)
		}
		problem = Authorize(principal, Policy{PC: true, RequireDelegation: true})
		if (problem == nil) != (principal.Kind == PC && principal.Delegated) {
			t.Fatalf("delegation policy widened: %+v", principal)
		}
	}
	if Authorize(Principal{Kind: Hostd, Name: "nas"}, Policy{Hostd: true, Host: "other-pc"}) == nil {
		t.Fatal("hostd impersonated another host")
	}
	if Authorize(Principal{Kind: Hostd, Name: "nas"}, Policy{Hostd: true, Host: "nas"}) != nil {
		t.Fatal("hostd own-host policy refused")
	}
}

func TestRelayKeyFromOtherColorIsOneUseAndBoundToLoopbackOrigin(t *testing.T) {
	a, store := fixture(t)
	ticket := strings.Repeat("t", 43)
	relayed := func(target string) *http.Request {
		r := request(http.MethodPost, target)
		r.RemoteAddr = "172.18.0.5:40000"
		r.Header.Set("Origin", strings.TrimSuffix(target, "/api/todos"))
		r.Header.Set(RelayKeyHeader, ticket)
		return r
	}
	store.keys[RelayKeyHash(ticket)] = fixedNow.Add(time.Minute)
	if _, problem := a.Authenticate(relayed(publicURL + "/api/todos")); problem == nil || problem.Status != 403 {
		t.Fatal("公開の接続先で転送チケットを受け付けた")
	}
	if _, ok := store.keys[RelayKeyHash(ticket)]; !ok {
		t.Fatal("拒否した要求がチケットを消費した")
	}
	principal, problem := a.Authenticate(relayed("http://127.0.0.1:8787/api/todos"))
	if problem != nil || principal.Kind != CLI {
		t.Fatalf("転送チケットを拒否した: %v", problem)
	}
	if _, problem := a.Authenticate(relayed("http://127.0.0.1:8787/api/todos")); problem == nil || problem.Status != 403 {
		t.Fatal("転送チケットの再利用を受け付けた")
	}
	// 一時キーとチケットは互いの代わりにならない。
	store.keys[Hash(ticket)] = fixedNow.Add(time.Minute)
	if _, problem := a.Authenticate(relayed("http://127.0.0.1:8787/api/todos")); problem == nil || problem.Status != 403 {
		t.Fatal("一時キーを転送チケットとして受け付けた")
	}
	store.keys[RelayKeyHash(ticket)] = fixedNow.Add(time.Minute)
	r := request(http.MethodPost, "http://127.0.0.1:8787/api/todos")
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	r.Header.Set("X-Desk-Key", ticket)
	delete(store.keys, Hash(ticket))
	if _, problem := a.Authenticate(r); problem == nil || problem.Status != 403 {
		t.Fatal("転送チケットを一時キーとして受け付けた")
	}
}

func TestPCTokenUseIsRecordedOncePerIntervalAndRevocationStaysImmediate(t *testing.T) {
	a, store := fixture(t)
	now := fixedNow
	a.config.Now = func() time.Time { return now }
	token := "idk_" + strings.Repeat("d", 43)
	store.pc[Hash(token)] = Token{ID: "pc-id", Name: "nas"}
	use := func() error {
		r := request(http.MethodGet, publicURL+"/api/v1/commands")
		r.Header.Set("Authorization", "Bearer "+token)
		if _, problem := a.Authenticate(r); problem != nil {
			return fmt.Errorf("%d %s", problem.Status, problem.Message)
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		if problem := use(); problem != nil {
			t.Fatal(problem)
		}
		now = now.Add(2 * time.Second)
	}
	if len(store.touched) != 1 {
		t.Fatalf("token use within the interval was written %d times", len(store.touched))
	}
	now = fixedNow.Add(touchInterval)
	if problem := use(); problem != nil {
		t.Fatal(problem)
	}
	if len(store.touched) != 2 {
		t.Fatalf("token use after the interval was not written: %d", len(store.touched))
	}
	delete(store.pc, Hash(token))
	if err := use(); err == nil || !strings.HasPrefix(err.Error(), "401 ") {
		t.Fatalf("revoked token accepted within the interval: %v", err)
	}
}
