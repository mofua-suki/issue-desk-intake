package httpapi

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func assets(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ui := strings.Repeat("a", 64)
	for name, body := range map[string]string{
		"index.html": "<!doctype html><title>Intake</title>", "sw.js": "// worker",
		"ui-version.json": `{"ui":"` + ui + `"}`, "v/" + ui + "/app.js": "console.log('ok')",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestStaticAssetsAndCache(t *testing.T) {
	dir := assets(t)
	s, err := New(Options{Assets: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ui := strings.Repeat("a", 64)
	for _, tc := range []struct {
		path   string
		status int
		cache  string
	}{
		{"/", 200, "no-store"}, {"/sw.js", 200, "no-store"},
		{"/v/" + ui + "/app.js", 200, "public, max-age=31536000, immutable"},
		{"/v/" + strings.Repeat("b", 64) + "/app.js", 404, "no-store"},
		{"/ui-version.json", 404, "no-store"}, {"/server/go.mod", 404, "no-store"},
		{"/v/" + ui + "/../app.js", 404, "no-store"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRecorder()
			s.ServeHTTP(r, httptest.NewRequest("GET", tc.path, nil))
			if r.Code != tc.status || r.Header().Get("Cache-Control") != tc.cache {
				t.Fatalf("status=%d cache=%q", r.Code, r.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestSymlinkCannotEscapeAssets(t *testing.T) {
	dir := assets(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "v", strings.Repeat("a", 64), "secret")); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Assets: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := httptest.NewRecorder()
	s.ServeHTTP(r, httptest.NewRequest("GET", "/v/"+strings.Repeat("a", 64)+"/secret", nil))
	if r.Code != 404 || strings.Contains(r.Body.String(), "private") {
		t.Fatal("asset escaped root")
	}
}

func TestIncompleteServiceCannotBecomeReady(t *testing.T) {
	s, err := New(Options{Assets: assets(t), Revision: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []string{"/healthz", "/api/health"} {
		r := httptest.NewRecorder()
		s.ServeHTTP(r, httptest.NewRequest("GET", p, nil))
		var v struct {
			Ready bool   `json:"ready"`
			UI    string `json:"ui"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if r.Code != 503 || v.Ready || len(v.UI) != 64 {
			t.Fatalf("invalid readiness response: %s", r.Body)
		}
	}
	r := httptest.NewRecorder()
	s.ServeHTTP(r, httptest.NewRequest("POST", "/api/issues", strings.NewReader(`{}`)))
	if r.Code != http.StatusNotImplemented {
		t.Fatalf("unimplemented write status=%d", r.Code)
	}
}

func TestMissingAssetsFailStartup(t *testing.T) {
	dir := assets(t)
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	if s, err := New(Options{Assets: dir}); err == nil {
		s.Close()
		t.Fatal("accepted incomplete assets")
	}
}

type specAuthStore struct{}

func (specAuthStore) PCToken(_ context.Context, hash string) (auth.Token, bool, error) {
	return auth.Token{ID: "id", Name: "pc"}, hash == auth.Hash("idk_"+strings.Repeat("a", 43)), nil
}
func (specAuthStore) TouchPCToken(context.Context, string, time.Time) error { return nil }
func (specAuthStore) HostdToken(context.Context, string) (auth.Token, bool, error) {
	return auth.Token{}, false, nil
}
func (specAuthStore) Session(_ context.Context, hash string) (auth.Session, bool, error) {
	return auth.Session{ExpiresAt: time.Now().Add(time.Hour)}, hash == auth.Hash("session"), nil
}
func (specAuthStore) ConsumeCLIKey(_ context.Context, hash string, _ time.Time) (bool, error) {
	return hash == auth.Hash("key"), nil
}

func TestOpenAPISpecRejectsMissingOrInvalidFileAtStartup(t *testing.T) {
	a, err := auth.New(auth.Config{PublicURL: "https://desk.example.test", Port: 8787}, specAuthStore{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := OpenAPISpec(a, dir); err == nil {
		t.Fatal("accepted a missing spec")
	}
	if err := os.MkdirAll(filepath.Join(dir, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api", "openapi.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAPISpec(a, dir); err == nil {
		t.Fatal("accepted an invalid spec")
	}
}

func TestOpenAPISpecAuthenticationAndFiltering(t *testing.T) {
	applicationDir, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(auth.Config{PublicURL: "https://desk.example.test", Port: 8787}, specAuthStore{})
	if err != nil {
		t.Fatal(err)
	}
	// README の手順（server/ で --application-dir ..）と同じく、作業ディレクトリに依らず読む。
	t.Chdir(t.TempDir())
	handler, err := OpenAPISpec(a, applicationDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query, credential string
		status                  int
	}{
		{"anonymous", "", "", 401}, {"pc", "", "pc", 200}, {"screen", "", "screen", 200}, {"cli", "", "cli", 200},
		{"prefix", "?pathPrefix=%2Fapi%2Fissues", "pc", 200}, {"empty", "?pathPrefix=", "pc", 400}, {"unknown", "?tag=x", "pc", 400}, {"duplicate", "?pathPrefix=%2Fa&pathPrefix=%2Fb", "pc", 400}, {"relative", "?pathPrefix=api", "pc", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://desk.example.test/api/openapi.json"+tc.query, nil)
			switch tc.credential {
			case "pc":
				r.Header.Set("Authorization", "Bearer idk_"+strings.Repeat("a", 43))
			case "screen":
				r.AddCookie(&http.Cookie{Name: "desk_session", Value: "session"})
			case "cli":
				r.Host = "localhost:8787"
				r.RemoteAddr = "127.0.0.1:1234"
				r.Header.Set("X-Desk-Key", "key")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.status == 200 {
				var spec struct {
					Paths      map[string]json.RawMessage `json:"paths"`
					Components json.RawMessage            `json:"components"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
					t.Fatal(err)
				}
				if len(spec.Paths) == 0 || len(spec.Components) == 0 {
					t.Fatal("missing paths or components")
				}
				if tc.name == "prefix" {
					for p := range spec.Paths {
						if !strings.HasPrefix(p, "/api/issues") {
							t.Fatalf("unexpected path %s", p)
						}
					}
				}
			}
		})
	}
}

func TestOpenAPISpecConcurrentRequests(t *testing.T) {
	raw, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var original struct {
		Paths      map[string]json.RawMessage `json:"paths"`
		Components json.RawMessage            `json:"components"`
	}
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	compact := func(raw json.RawMessage) string {
		out, _ := json.Marshal(raw)
		return string(out)
	}
	a, err := auth.New(auth.Config{PublicURL: "https://desk.example.test", Port: 8787}, specAuthStore{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := OpenAPISpec(a, "../../..")
	if err != nil {
		t.Fatal(err)
	}
	prefixes := []string{"", "/api/issues", "/api/", "/missing"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		prefix := prefixes[i%len(prefixes)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			url := "https://desk.example.test/api/openapi.json"
			if prefix != "" {
				url += "?pathPrefix=" + prefix
			}
			r := httptest.NewRequest("GET", url, nil)
			r.Header.Set("Authorization", "Bearer idk_"+strings.Repeat("a", 43))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("prefix %q: status %d", prefix, w.Code)
				return
			}
			var got struct {
				Paths      map[string]json.RawMessage `json:"paths"`
				Components json.RawMessage            `json:"components"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Errorf("prefix %q: %v", prefix, err)
				return
			}
			if compact(got.Components) != compact(original.Components) {
				t.Errorf("prefix %q: components changed", prefix)
			}
			for path, value := range original.Paths {
				want := strings.HasPrefix(path, prefix)
				actual, ok := got.Paths[path]
				if ok != want || (ok && compact(actual) != compact(value)) {
					t.Errorf("prefix %q: incorrect path %q", prefix, path)
				}
			}
			for path := range got.Paths {
				if _, ok := original.Paths[path]; !ok {
					t.Errorf("prefix %q: unexpected path %q", prefix, path)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestOpenAPISpecConcurrentAuthorization(t *testing.T) {
	a, err := auth.New(auth.Config{PublicURL: "https://desk.example.test", Port: 8787}, specAuthStore{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := OpenAPISpec(a, "../../..")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		authorized := i%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := httptest.NewRequest("GET", "https://desk.example.test/api/openapi.json?pathPrefix=/api/issues", nil)
			if authorized {
				r.Header.Set("Authorization", "Bearer idk_"+strings.Repeat("a", 43))
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if authorized && w.Code != http.StatusOK {
				t.Errorf("authorized request: status %d", w.Code)
			} else if !authorized && (w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), `"paths"`)) {
				t.Errorf("anonymous request: status %d, body %s", w.Code, w.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestOpenAPISpecReloadAndReadFailure(t *testing.T) {
	raw, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "api"), 0755); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "api", "openapi.json")
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(auth.Config{PublicURL: "https://desk.example.test", Port: 8787}, specAuthStore{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := OpenAPISpec(a, dir)
	if err != nil {
		t.Fatal(err)
	}
	request := func(query string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://desk.example.test/api/openapi.json"+query, nil)
		if authenticated {
			r.Header.Set("Authorization", "Bearer idk_"+strings.Repeat("a", 43))
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, content := range []string{`{"paths":{"/new":{}}}`, "{", `{"paths":null}`, `{"paths":[]}`} {
		if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		unfiltered := request("", true)
		if unfiltered.Code != 200 || unfiltered.Body.String() != content {
			t.Fatalf("unfiltered: %d %s", unfiltered.Code, unfiltered.Body.String())
		}
		filtered := request("?pathPrefix=/new", true)
		want := 503
		if strings.Contains(content, "/new") {
			want = 200
		}
		if filtered.Code != want {
			t.Fatalf("filtered: %d %s", filtered.Code, filtered.Body.String())
		}
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query  string
		auth   bool
		status int
	}{{"", true, 503}, {"?pathPrefix=/new", true, 503}, {"", false, 401}, {"?bad=x", true, 400}} {
		if got := request(tc.query, tc.auth).Code; got != tc.status {
			t.Errorf("%+v: got %d", tc, got)
		}
	}
}
