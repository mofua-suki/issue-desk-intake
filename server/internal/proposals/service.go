package proposals

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

type Object = map[string]any
type API interface {
	Do(context.Context, string, string, any, any) error
}
type Options struct {
	AppDB, Postgres *sql.DB
	Auth            *auth.Authenticator
	Gitea           API
	Config          func() (Object, error)
	Project         func(string) error
	Now             func() time.Time
	Desk            func(string, int64) any
	Link            func(string, string, int64) string
	Publish         func(Object)
	Repositories    func() []string
	Issues          func(context.Context, string, string) ([]Object, error)
	Comments        func(context.Context, string, int64) ([]Object, error)
	Submit          func(context.Context, string, consultations.Record) (consultations.Record, error)
	AcceptedContext context.Context // lifetime for filing and its durable proposal mark
}
type Service struct {
	o        Options
	records  *consultations.Records
	mu       sync.Mutex
	scanOnce sync.Once
	scanSlot chan struct{} // Scanners serialize; interactive operations do not wait.
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func str(v any) string                      { return fleet.Text(v) }
func obj(v any) Object                      { return fleet.Object(v) }
func arr(v any) []any                       { return fleet.List(v) }
func num(v any) int64                       { return int64(fleet.Number(v)) }
func fail(status int, message string) error { return contract.NewProblem(status, message, "") }
func id() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func (s *Service) now() time.Time { return s.o.Now() }
func (s *Service) at() string     { return contract.ISO(s.now()) }
func (s *Service) read(ctx context.Context, key string) (Object, error) {
	r, _, e := s.records.Get(ctx, "proposals", key)
	return Object(r), e
}
func (s *Service) save(ctx context.Context, key string, value Object) error {
	_, e := s.records.Change(ctx, "proposals", key, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
		return consultations.Record(value), nil
	})
	return e
}
func Register(mux *http.ServeMux, o Options) (*Service, error) {
	if mux == nil || o.AppDB == nil || o.Auth == nil || o.Config == nil || o.Project == nil || o.Gitea == nil {
		return nil, fmt.Errorf("後続案の保存・認証・設定・Gitea接続が必要です")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	records, e := consultations.OpenRecords(context.Background(), o.AppDB)
	if e != nil {
		return nil, e
	}
	s := &Service{o: o, records: records}
	for _, path := range []string{"/api/proposals/repost", "/api/proposals/dismissal", "/api/proposals/dismiss"} {
		mux.Handle(path, s)
	}
	return s, nil
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, problem := s.o.Auth.Authenticate(r)
	if problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	if problem = auth.Authorize(p, auth.Policy{Screen: true, CLI: true, PC: true, RequireDelegation: true}); problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	data := Object{}
	var e error
	if r.Method == "GET" {
		for k, v := range r.URL.Query() {
			if len(v) != 1 {
				e = fail(400, "同じ項目を重ねて指定できません。")
				break
			}
			data[k] = v[0]
		}
		for _, key := range []string{"number", "commentId"} {
			if text, ok := data[key].(string); ok {
				n, err := strconv.ParseInt(text, 10, 64)
				if err != nil {
					e = fail(400, "番号が不正です。")
				}
				data[key] = n
			}
		}
	} else if r.Method == "POST" {
		data, problem = contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 10000)
		if problem != nil {
			e = problem
		}
	} else {
		e = fail(405, "このメソッドは使えません。")
	}
	via := "screen"
	if p.Kind == auth.PC {
		via = "token:" + p.Name
	} else if p.Kind == auth.CLI {
		via = "cli"
	}
	var value any
	if e == nil {
		switch {
		case r.URL.Path == "/api/proposals/dismissal" && r.Method == "GET":
			value, e = s.Candidate(r.Context(), data)
		case r.URL.Path == "/api/proposals/dismiss" && r.Method == "POST":
			value, e = s.Dismiss(r.Context(), data, via, p.Kind == auth.PC)
		case r.URL.Path == "/api/proposals/repost" && r.Method == "POST":
			value, e = s.Repost(r.Context(), data, via)
		default:
			e = fail(405, "このメソッドは使えません。")
		}
	}
	if e != nil {
		if p, ok := e.(*contract.Problem); ok {
			contract.WriteProblem(w, p)
		} else {
			contract.LogFailure("後続案のAPI "+r.URL.Path, e)
			contract.WriteProblem(w, contract.NewProblem(503, "後続案の操作を完了できません。保存済みの状態を保持しています。", ""))
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	_ = json.NewEncoder(w).Encode(value)
	if r.Method == "POST" && s.o.Publish != nil {
		s.o.Publish(Object{"type": "refresh"})
	}
}
func source(t Object) string {
	return fmt.Sprintf("%s#%d:%s", str(t["project"]), num(t["number"]), str(t["proposalId"]))
}
func draftID(repo string, n int64, id string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("issue-desk-proposal\n%s#%d\n%s", repo, n, id)))
	h := fmt.Sprintf("%x", sum)
	variant := ((sum[8] >> 4) & 3) | 8
	return fmt.Sprintf("%s-%s-5%s-%x%s-%s", h[:8], h[8:12], h[13:16], variant, h[17:20], h[20:32])
}
