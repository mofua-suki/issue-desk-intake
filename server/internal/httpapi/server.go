package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

// Health is supplied by the running service and checks its persistent backends.
type Options struct {
	Assets           string
	Revision         string
	API              http.Handler
	Health           func(context.Context) (int, map[string]any)
	DeploymentHealth func(context.Context) (int, map[string]any)
}

type Server struct {
	options Options
	root    *os.Root
	ui      string
}

var uiPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func New(options Options) (*Server, error) {
	root, err := os.OpenRoot(options.Assets)
	if err != nil {
		return nil, errors.New("client assets cannot be opened")
	}
	fail := func(message string) (*Server, error) { root.Close(); return nil, errors.New(message) }
	f, err := root.Open("ui-version.json")
	if err != nil {
		return fail("client version manifest cannot be opened")
	}
	var manifest struct {
		UI string `json:"ui"`
	}
	err = json.NewDecoder(f).Decode(&manifest)
	f.Close()
	if err != nil || !uiPattern.MatchString(manifest.UI) {
		return fail("invalid client version manifest")
	}
	for _, name := range []string{"index.html", "sw.js", "v/" + manifest.UI + "/app.js"} {
		info, err := root.Stat(name)
		if err != nil || !info.Mode().IsRegular() {
			return fail("required client asset is missing")
		}
	}
	return &Server{options: options, root: root, ui: manifest.UI}, nil
}

func (s *Server) Close() error { return s.root.Close() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	if r.URL.Path == "/healthz" || r.URL.Path == "/api/health" {
		s.serveHealth(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if s.options.API != nil {
			s.options.API.ServeHTTP(w, r)
			return
		}
		contract.WriteProblem(w, contract.NewProblem(http.StatusNotImplemented, "この操作はまだ利用できません。", ""))
		return
	}
	s.serveStatic(w, r)
}

// serveHealth は稼働確認に、業務と配置の確認の結果と画面・Serverの版を添えて答える。
func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.method(w)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	status, body := http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": "business_service_unavailable"}
	check := s.options.Health
	if r.URL.Path == "/healthz" && s.options.DeploymentHealth != nil {
		check = s.options.DeploymentHealth
	}
	if check != nil {
		status, body = check(r.Context())
		if body == nil {
			status, body = 503, map[string]any{"ready": false}
		}
	}
	body["ui"], body["revision"] = s.ui, s.options.Revision
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		json.NewEncoder(w).Encode(body)
	}
}

// serveStatic は画面の index.html・sw.js と版付きの静的ファイルだけを配る。
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.method(w)
		return
	}
	if strings.Contains(r.URL.Path, "\\") || strings.ContainsRune(r.URL.Path, 0) || path.Clean(r.URL.Path) != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	immutable := strings.HasPrefix(name, "v/"+s.ui+"/")
	if name == "" {
		name = "index.html"
	} else if name != "sw.js" && !immutable {
		http.NotFound(w, r)
		return
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	f, err := s.root.Open(filepath.FromSlash(name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *Server) method(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, HEAD")
	contract.WriteProblem(w, contract.NewProblem(http.StatusMethodNotAllowed, "このメソッドは利用できません。", ""))
}

// OpenAPISpec serves the checked-in combined contract through the standard authentication policy.
// The file is resolved from applicationDir like the other runtime assets, and a missing or invalid
// file fails startup instead of the first request.
func OpenAPISpec(a *auth.Authenticator, applicationDir string) (http.Handler, error) {
	filename := filepath.Join(applicationDir, "api", "openapi.json")
	raw, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, errors.New("API仕様がJSONではありません: " + filename)
	}
	cache := newSpecCache(filename, raw)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, problem := a.Authenticate(r)
		if problem != nil {
			contract.WriteProblem(w, problem)
			return
		}
		if problem = auth.Authorize(principal, auth.Policy{PC: true, CLI: true, Screen: true}); problem != nil {
			contract.WriteProblem(w, problem)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			contract.WriteProblem(w, contract.NewProblem(405, "このメソッドは利用できません。", ""))
			return
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) > 1 || (len(query) == 1 && (len(query["pathPrefix"]) != 1 || !strings.HasPrefix(query.Get("pathPrefix"), "/") || strings.ContainsAny(query.Get("pathPrefix"), "\\\x00\r\n"))) {
			contract.WriteProblem(w, contract.NewProblem(400, "検索条件が不正です。", ""))
			return
		}
		data, err := cache.load(query.Get("pathPrefix"))
		if err != nil {
			contract.WriteProblem(w, contract.NewProblem(503, "仕様を読み込めません。", ""))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(data)
	}), nil
}
