package issueapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/attachments"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Object = map[string]any
type API interface {
	Do(context.Context, string, string, any, any) error
	Pages(context.Context, string) ([]map[string]any, error)
	Issue(context.Context, string, int64) (map[string]any, error)
	Issues(context.Context, string, string) ([]map[string]any, error)
	Comments(context.Context, string, int64) ([]map[string]any, error)
	Comment(context.Context, string, int64, string) (map[string]any, error)
	SetIssueState(context.Context, string, int64, string) error
}
type Options struct {
	Principal       func(context.Context, string) (auth.Principal, error)
	ConfigReader    func() (Object, error)
	DedicatedAuthor func(Object) bool
	Attachments     *attachments.Service
	Drafts          *consultations.Records
	Postgres        *sql.DB
	AppDB           *sql.DB
	Auth            *auth.Authenticator
	Gitea           API
	Numbers         *numbers.Store
	Jobs            *jobs.Store
	Config          map[string]any
	Now             func() time.Time
	Project         func(string) error
	Repositories    func() []string
	Own             func(Object) bool
	PublicLink      func(string, string, int64) string
	Publish         func(Object)
	Answer          func(context.Context, Object) (Object, error)
	UsageLimit      float64
	UsageWeights    map[string]any
	RunLimit        int64
	// ActiveList reads the Issue rows of the screen's active range for
	// GET /api/issues?scope=active. It owns the range decision so the list and
	// the screen never disagree about which closed rows are kept.
	ActiveList func(context.Context) ([]Object, []UnreadProject, error)
}
type Service struct {
	o             Options
	locks         sync.Map
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	closing       sync.Mutex
	closeInFlight sync.Map
}

var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var sha = regexp.MustCompile(`^[a-f0-9]{40}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func fail(status int, message string) *contract.Problem {
	return contract.NewProblem(status, message, "")
}
func number(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), n > 0 && int64(n) <= 9007199254740991
	case int64:
		return n, n > 0 && n <= 9007199254740991
	case float64:
		return int64(n), n > 0 && n <= 9007199254740991 && n == float64(int64(n))
	case json.Number:
		x, e := n.Int64()
		return x, e == nil && x > 0 && x <= 9007199254740991
	case string:
		x, e := strconv.ParseInt(n, 10, 64)
		return x, e == nil && x > 0 && x <= 9007199254740991
	}
	return 0, false
}
func clone(v Object) Object {
	b, _ := json.Marshal(v)
	m := Object{}
	_ = json.Unmarshal(b, &m)
	return m
}
func lock(m *sync.Map, key string) func() {
	v, _ := m.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
func (s *Service) now() string { return contract.ISO(s.o.Now()) }
func (s *Service) emit(repo string, n int64) {
	if s.o.Publish != nil {
		s.o.Publish(Object{"type": "issue", "project": repo, "number": n})
	}
}
func (s *Service) link(repo, kind string, n int64) string {
	if s.o.PublicLink != nil {
		return s.o.PublicLink(repo, kind, n)
	}
	base := strings.TrimRight(fleet.Text(s.o.Config["giteaUrl"]), "/")
	return fmt.Sprintf("%s/%s/%s/%d", base, repo, kind, n)
}
func Register(mux *http.ServeMux, o Options) (*Service, error) {
	if mux == nil || o.AppDB == nil || o.Auth == nil || o.Gitea == nil || o.Numbers == nil || o.Jobs == nil || o.Project == nil || o.Own == nil {
		return nil, errors.New("issue authentication, persistence, Gitea, numbers and jobs are required")
	}
	if o.Drafts == nil {
		var err error
		o.Drafts, err = consultations.OpenRecords(context.Background(), o.AppDB)
		if err != nil {
			return nil, err
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.RunLimit == 0 {
		o.RunLimit = 4
	}
	if _, e := o.AppDB.ExecContext(context.Background(), bindSQL(`CREATE TABLE IF NOT EXISTS actions(id TEXT PRIMARY KEY,json TEXT NOT NULL);CREATE TABLE IF NOT EXISTS close_requests(id TEXT PRIMARY KEY,json TEXT NOT NULL);CREATE TABLE IF NOT EXISTS close_reasons(id TEXT PRIMARY KEY,json TEXT NOT NULL)`)); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{o: o, ctx: ctx, cancel: cancel}
	for _, path := range []string{"/api/issues", "/api/issues/search", "/api/issue", "/api/comment", "/api/handover", "/api/issue/close", "/api/close-reason", "/api/issues/close", "/api/close-requests", "/api/close-requests/"} {
		mux.Handle(path, s)
	}
	return s, nil
}
func (s *Service) Close() { s.closing.Lock(); s.cancel(); s.closing.Unlock(); s.wg.Wait() }
func (s *Service) read(ctx context.Context, table, id string) (Object, error) {
	var raw string
	var row *sql.Row
	switch table {
	case "actions":
		row = s.o.AppDB.QueryRowContext(ctx, bindSQL("SELECT json FROM actions WHERE id=?"), id)
	case "close_reasons":
		row = s.o.AppDB.QueryRowContext(ctx, bindSQL("SELECT json FROM close_reasons WHERE id=?"), id)
	case "close_requests":
		row = s.o.AppDB.QueryRowContext(ctx, bindSQL("SELECT json FROM close_requests WHERE id=?"), id)
	case "drafts":
		row = s.o.AppDB.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=?"), id)
	case "proposals":
		row = s.o.AppDB.QueryRowContext(ctx, bindSQL("SELECT json FROM proposals WHERE id=?"), id)
	default:
		return nil, fmt.Errorf("unknown table %q", table)
	}
	e := row.Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	m := Object{}
	e = json.Unmarshal([]byte(raw), &m)
	return m, e
}
func (s *Service) save(ctx context.Context, table, id string, m Object) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	switch table {
	case "actions":
		_, e = s.o.AppDB.ExecContext(ctx, bindSQL("INSERT INTO actions(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json"), id, string(b))
	case "close_reasons":
		_, e = s.o.AppDB.ExecContext(ctx, bindSQL("INSERT INTO close_reasons(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json"), id, string(b))
	case "close_requests":
		_, e = s.o.AppDB.ExecContext(ctx, bindSQL("INSERT INTO close_requests(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json"), id, string(b))
	case "drafts":
		_, e = s.o.AppDB.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json"), id, string(b))
	case "proposals":
		_, e = s.o.AppDB.ExecContext(ctx, bindSQL("INSERT INTO proposals(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json"), id, string(b))
	default:
		e = fmt.Errorf("unknown table %q", table)
	}
	return e
}
func (s *Service) target(ctx context.Context, data Object) (string, int64, error) {
	if desk, has := data["desk"]; has {
		if data["project"] != nil || data["number"] != nil {
			return "", 0, fail(400, "deskとproject・numberは同時に指定できません。")
		}
		n, ok := number(desk)
		if !ok {
			return "", 0, fail(400, "共通番号が不正です。")
		}
		issue, found, e := s.o.Numbers.Issue(ctx, n)
		if e != nil {
			return "", 0, e
		}
		if !found {
			return "", 0, fail(404, "共通番号が見つかりません。")
		}
		if e = s.o.Project(issue.Project); e != nil {
			return "", 0, e
		}
		return issue.Project, issue.Number, nil
	}
	repo := fleet.Text(data["project"])
	n, ok := number(data["number"])
	if !repoPattern.MatchString(repo) || !ok {
		return "", 0, fail(400, "プロジェクトとIssue番号が不正です。")
	}
	if e := s.o.Project(repo); e != nil {
		return "", 0, e
	}
	return repo, n, nil
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, problem := s.o.Auth.Authenticate(r)
	if problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	if problem = auth.Authorize(p, auth.Policy{Screen: true, CLI: true, PC: true}); problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	data := Object{}
	var e error
	if r.Method == "GET" {
		for k, v := range r.URL.Query() {
			data[k] = v[len(v)-1]
		}
	} else {
		data, problem = contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 100000)
		if problem != nil {
			contract.WriteProblem(w, problem)
			return
		}
	}
	var result any
	status := http.StatusOK
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/issues":
		result, e = s.listScoped(r.Context(), data)
	case r.Method == "GET" && r.URL.Path == "/api/issues/search":
		result, e = s.search(r.Context(), data, p)
	case r.Method == "GET" && r.URL.Path == "/api/issue":
		result, e = s.issueDetail(r.Context(), data, p)
	case r.Method == "GET" && r.URL.Path == "/api/comment":
		result, e = s.replyRequest(r.Context(), fleet.Text(data["id"]), p)
	case r.Method == "POST" && (r.URL.Path == "/api/comment" || r.URL.Path == "/api/handover"):
		if p.Kind == auth.PC && !p.Delegated {
			e = fail(403, "運用の代行を許したトークンが必要です。")
		} else if r.URL.Path == "/api/comment" {
			if data["async"] == true && data["action"] == "reply" {
				result, e = s.acceptReply(r.Context(), data, p)
				status = http.StatusAccepted
			} else {
				result, e = s.comment(r.Context(), data, p)
			}
		} else {
			result, e = s.handover(r.Context(), data, p)
		}
	case strings.HasPrefix(r.URL.Path, "/api/close-") || r.URL.Path == "/api/issue/close" || r.URL.Path == "/api/issues/close":
		result, e = s.closeRoute(r.Context(), r.Method, r.URL.Path, data, p)
	default:
		e = fail(404, "このAPIはありません。")
	}
	if e != nil {
		var problem *contract.Problem
		if !errors.As(e, &problem) || problem == nil {
			contract.LogFailure("IssueのAPI "+r.URL.Path, e)
			problem = fail(503, "Issueの処理を完了できません。")
		}
		contract.WriteProblem(w, problem)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}

// issueDetail は依頼1件の詳細を返す。画面と運用代行のトークンには、残っている最終承認も添える。
func (s *Service) issueDetail(ctx context.Context, data Object, p auth.Principal) (any, error) {
	var result any
	var e error
	if value, has := data["view"]; has && value != "summary" {
		return nil, fail(400, "viewにはsummaryだけ指定できます。")
	}
	var repo string
	var n int64
	repo, n, e = s.target(ctx, data)
	if e == nil {
		result, e = s.Detail(ctx, repo, n, p.Kind != auth.PC, data["view"] == "summary")
		if e == nil && (p.Kind == auth.Screen || p.Kind == auth.CLI && data["verifyMerge"] == "true" || p.Kind == auth.PC && p.Delegated) && data["view"] != "summary" {
			job, jobErr := s.o.Jobs.ByIssue(ctx, repo, n)
			if jobErr != nil {
				e = jobErr
			} else if job != nil {
				pending, pendingErr := jobs.PendingFinalApprovals(job)
				if detail, ok := result.(Object); ok {
					if pendingErr != nil {
						detail["finalApprovalsUnavailable"] = "完了計画を確認できません。"
					} else if len(pending) > 0 {
						detail["pendingFinalApprovals"] = pending
					}
				}
			}
		}
	}
	if detail, ok := result.(Object); ok && e == nil && data["verifyMerge"] == "true" && data["view"] != "summary" {
		if evidence := s.watchMergeEvidence(ctx, repo, n, detail); evidence != nil {
			detail["mergeEvidence"] = evidence
		}
	}
	return result, e
}
func via(p auth.Principal) string {
	if p.Kind == auth.Screen {
		return "issue-screen"
	}
	if p.Kind == auth.CLI {
		return "cli-key"
	}
	return "token:" + p.Name
}
func explicitRejection(e error) bool {
	var g *gitea.Error
	return errors.As(e, &g) && g.Status >= 400 && g.Status < 500 && !g.UnknownOutcome
}
