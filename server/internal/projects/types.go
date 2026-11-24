package projects

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"regexp"
	"sync"
	"time"
	"unicode/utf16"
)

//go:embed schema.json prompt.md all:templates
var assets embed.FS

type Object = map[string]any
type Gitea interface {
	Do(context.Context, string, string, any, any) error
}
type Options struct {
	Acquire                                 func(context.Context, string) (context.Context, func(), error)
	Collaborators                           func(string) []string
	Webhook                                 func(context.Context, string) (Object, error)
	AppDB                                   *sql.DB
	Auth                                    *auth.Authenticator
	Owner, ConfigFile, RuntimeDir, Provider string
	Gitea                                   Gitea
	Now                                     func() time.Time
	Repositories                            func() []string
	Answer                                  func(context.Context, Object) (Object, error)
	// Registration publishes and reconciles one checked placement-table PR.
	// false means the persisted operation is waiting for merge or reload.
	Registration func(context.Context, string, Object, Object) (bool, error)
	Preflight    func(context.Context, Object, Object) error
	Registered   func(context.Context, Object) error
	Home         func(string) string
	Submit       func(context.Context, Object) ([]Object, error)
	DeskNumber   func(string, int64) any
	Publish      func(Object)
}
type Service struct {
	o       Options
	mu      sync.Mutex
	running map[string]bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func problem(status int, message string) *contract.Problem {
	return contract.NewProblem(status, message, "")
}
func size(s string) int { return len(utf16.Encode([]rune(s))) }
func uuid() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func clone(v Object) Object {
	b, _ := json.Marshal(v)
	out := Object{}
	_ = json.Unmarshal(b, &out)
	return out
}
func jsonBytes(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.ReplaceAll(bytes.ReplaceAll(bytes.TrimSuffix(b.Bytes(), []byte("\n")), []byte(`\u2028`), []byte(" ")), []byte(`\u2029`), []byte(" "))
}
func (s *Service) now() string { return contract.ISO(s.o.Now()) }
func hash(v []byte) string     { sum := sha256.Sum256(v); return hex.EncodeToString(sum[:]) }
func revision(p Object)        { p["revision"] = fleet.Int64(p["revision"]) + 1 }
func locked(p Object) bool {
	return p["status"] == "creating" || p["status"] == "registered" || fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])["requested"] != nil
}
func approved(p Object) bool {
	return p["definition"] != nil && fleet.ObjectOrEmpty(p["approval"])["fingerprint"] == Fingerprint(fleet.ObjectOrEmpty(p["definition"]))
}
func (s *Service) creatable() bool {
	return s.o.ConfigFile != "" && s.o.Gitea != nil && s.o.Preflight != nil && s.o.Registration != nil && s.o.Registered != nil
}
func New(o Options) (*Service, error) {
	if o.AppDB == nil || o.Auth == nil {
		return nil, fmt.Errorf("project persistence and authentication are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Provider == "" {
		o.Provider = "codex"
	}
	if _, e := o.AppDB.Exec(bindSQL(`CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY,json TEXT NOT NULL)`)); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{o: o, running: map[string]bool{}, ctx: ctx, cancel: cancel}, nil
}
func Register(mux *http.ServeMux, o Options) (*Service, error) {
	s, e := New(o)
	if e != nil {
		return nil, e
	}
	mux.Handle("/api/projects", s)
	mux.Handle("/api/projects/", s)
	return s, nil
}
func (s *Service) Close() { s.cancel(); s.wg.Wait() }
func (s *Service) load(ctx context.Context, id string) (Object, error) {
	if !uuidPattern.MatchString(id) {
		return nil, problem(404, "プロジェクトの相談が見つかりません。")
	}
	var raw string
	if e := s.o.AppDB.QueryRowContext(ctx, bindSQL(`SELECT json FROM projects WHERE id=?`), id).Scan(&raw); e != nil {
		if e == sql.ErrNoRows {
			return nil, problem(404, "プロジェクトの相談が見つかりません。")
		}
		return nil, e
	}
	out := Object{}
	if e := json.Unmarshal([]byte(raw), &out); e != nil {
		return nil, e
	}
	return out, nil
}
func (s *Service) save(ctx context.Context, p Object) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	_, e = s.o.AppDB.ExecContext(ctx, bindSQL(`INSERT INTO projects(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json`), fleet.Text(p["id"]), string(b))
	if e == nil && s.o.Publish != nil {
		s.o.Publish(Object{"type": "project", "id": p["id"]})
	}
	return e
}
func (s *Service) all(ctx context.Context) ([]Object, error) {
	rows, e := s.o.AppDB.QueryContext(ctx, bindSQL(`SELECT json FROM projects ORDER BY `+rowOrder(true)))
	if e != nil {
		return nil, e
	}
	out, _, e := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{})
	return out, e
}
