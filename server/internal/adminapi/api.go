// Package adminapi serves screen administration and audited delegated operations.
package adminapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

type Options struct {
	AppDB        *sql.DB
	Postgres     *sql.DB
	RuntimeDir   string
	Auth         *auth.Authenticator
	Now          func() time.Time
	Config       map[string]any
	Publish      func(map[string]any)
	AllowProject func(string) bool
	DirectPRBase func(context.Context, string) (string, error)
	Log          func(string)
	HTTPClient   *http.Client
}
type service struct {
	Options
	mu sync.Mutex
}
type endpoint func(*http.Request, auth.Principal) (any, int, error)

var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var grantPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func Register(mux *http.ServeMux, options *Options) error {
	if mux == nil || options == nil || options.AppDB == nil || options.Auth == nil {
		return errors.New("admin API requires AppDB and authentication")
	}
	s := &service{Options: *options}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Config == nil {
		s.Config = map[string]any{}
	}
	if _, err := s.AppDB.Exec(`CREATE TABLE IF NOT EXISTS decisions(id TEXT PRIMARY KEY,json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS ai_decisions(id TEXT PRIMARY KEY,json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS admin_audit(id INTEGER PRIMARY KEY,at TEXT NOT NULL,actor TEXT NOT NULL,action TEXT NOT NULL,target TEXT NOT NULL);
`); err != nil {
		return err
	}
	for _, path := range []string{"/api/tokens", "/api/tokens/", "/api/decisions", "/api/decisions/", "/api/ai-decisions", "/api/ai-decisions/", "/api/permission-grants", "/api/permission-grants/", "/api/ssh-tickets", "/api/ssh-tickets/", "/api/push/key", "/api/push/subscriptions", "/api/push/test", "/api/diagnostics/db", "/api/diagnostics/db-query", "/api/diagnostics/cpu", "/api/diagnostics/goroutines", "/api/diagnostics/activity"} {
		mux.HandleFunc(path, s.serve)
	}
	if err := s.restoreDecisions(context.Background()); err != nil {
		s.log("決定の記録の写しを照合できません。アプリのDBの記録を保持しています。")
	}
	return nil
}
func (s *service) serve(w http.ResponseWriter, r *http.Request) {
	principal, problem := s.Auth.Authenticate(r)
	if problem != nil {
		s.reply(w, nil, 0, problem)
		return
	}
	policy, handler := s.route(r)
	if denied, err := s.authorize(r, principal, policy); denied {
		s.reply(w, nil, 0, err)
		return
	}
	if err := screenOnly(r, principal); err != nil {
		s.reply(w, nil, 0, err)
		return
	}
	value, status, err := handler(r, principal)
	if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
		s.log("管理APIのリクエストが切断されました: " + r.Method + " " + r.URL.Path)
		return
	}
	s.reply(w, value, status, err)
}

// route selects the authentication policy and the handler for the path.
func (s *service) route(r *http.Request) (policy auth.Policy, handler endpoint) {
	switch {
	case r.URL.Path == "/api/diagnostics/db-query":
		policy = auth.Policy{PC: true, CLI: true, RequireDelegation: true}
		handler = s.diagnosticsDBQuery
	case r.URL.Path == "/api/diagnostics/activity":
		policy = auth.Policy{PC: true, CLI: true, RequireDelegation: true}
		handler = s.diagnosticsActivity
	case r.URL.Path == "/api/diagnostics/cpu":
		policy = auth.Policy{PC: true, CLI: true, RequireDelegation: true}
		handler = s.diagnosticsCPU
	case r.URL.Path == "/api/diagnostics/goroutines":
		policy = auth.Policy{PC: true, CLI: true, RequireDelegation: true}
		handler = s.diagnosticsGoroutines
	case r.URL.Path == "/api/diagnostics/db":
		policy = auth.Policy{PC: true, CLI: true, RequireDelegation: true}
		handler = s.diagnosticsDB
	case strings.HasPrefix(r.URL.Path, "/api/tokens"):
		policy = auth.Policy{Screen: true}
		handler = s.tokens
	case strings.HasPrefix(r.URL.Path, "/api/ai-decisions"):
		policy = auth.Policy{PC: true, RequireDelegation: true}
		handler = s.aiDecisions
	case strings.HasPrefix(r.URL.Path, "/api/decisions"):
		policy = auth.Policy{Screen: true, CLI: r.Method == http.MethodGet && (r.URL.Path == "/api/decisions" || r.URL.Path == "/api/decisions/export")}
		handler = s.decisions
	case strings.HasPrefix(r.URL.Path, "/api/permission-grants"):
		policy = auth.Policy{Screen: true, PC: true, CLI: true, RequireDelegation: true}
		handler = s.grants
	case strings.HasPrefix(r.URL.Path, "/api/ssh-tickets"):
		policy = auth.Policy{Screen: true, PC: true, CLI: true}
		handler = s.ssh
	default:
		policy = auth.Policy{Screen: true}
		handler = s.push
	}
	return policy, handler
}

// authorize checks the principal against the route's policy. A denied DB query
// is still audited by dbQuery, whose error is the reply.
func (s *service) authorize(r *http.Request, principal auth.Principal, policy auth.Policy) (denied bool, err error) {
	problem := auth.Authorize(principal, policy)
	if problem == nil {
		return false, nil
	}
	if r.URL.Path == "/api/diagnostics/db-query" {
		_, _, err := s.dbQuery(r, principal, problem)
		return true, err
	}
	return true, problem
}

// screenOnly rejects screen-only operations sent with a desk key or from
// outside the screen.
func screenOnly(r *http.Request, principal auth.Principal) error {
	if principal.Kind == auth.Screen && strings.HasPrefix(r.URL.Path, "/api/push/") && headerPresent(r, "X-Desk-Key") {
		return bad(403, "この操作は受付画面からだけ使えます。")
	}
	if strings.HasPrefix(r.URL.Path, "/api/ssh-tickets/") && (strings.HasSuffix(r.URL.Path, "/approve") || strings.HasSuffix(r.URL.Path, "/deny")) && (principal.Kind != auth.Screen || headerPresent(r, "X-Desk-Key")) {
		return bad(403, "承認・却下は受付画面からだけ使えます。")
	}
	return nil
}
func (s *service) reply(w http.ResponseWriter, value any, status int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	if err != nil {
		var problem *contract.Problem
		if !errors.As(err, &problem) || problem == nil {
			problem = bad(503, "管理情報を読み書きできません。")
		}
		w.WriteHeader(problem.Status)
		_ = json.NewEncoder(w).Encode(problem)
		return
	}
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func bad(status int, message string) *contract.Problem {
	return contract.NewProblem(status, message, "")
}
func body(r *http.Request) (map[string]any, error) {
	data, p := contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 1024*1024)
	if p != nil {
		return nil, p
	}
	return data, nil
}
func str(v any) string         { s, _ := v.(string); return s }
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, e := n.Int64()
		return i, e == nil
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), n == float64(int64(n))
	}
	return 0, false
}
func boolValue(v any) bool   { b, _ := v.(bool); return b }
func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func moment(v any) (time.Time, bool) {
	t, e := time.Parse(time.RFC3339Nano, str(v))
	return t, e == nil
}
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	x := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", x[:8], x[8:12], x[12:16], x[16:20], x[20:])
}
func actor(p auth.Principal) string {
	switch p.Kind {
	case auth.PC:
		return "token:" + p.Name
	case auth.CLI:
		return "temporary-key"
	case auth.Hostd:
		return "hostd:" + p.Name
	}
	return "screen"
}
func (s *service) log(message string) {
	if s.Log != nil {
		s.Log(message)
	}
}
func (s *service) audit(ctx context.Context, tx *sql.Tx, p auth.Principal, action, target string) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO admin_audit(at,actor,action,target) VALUES(?,?,?,?)", iso(s.Now()), actor(p), action, target)
	return e
}
func (s *service) notify(kind, id string) {
	if s.Publish != nil {
		s.Publish(map[string]any{"type": kind, "id": id})
	}
}
func jsonGet(ctx context.Context, tx *sql.Tx, table, id string) (map[string]any, error) {
	var raw string
	e := tx.QueryRowContext(ctx, "SELECT json FROM "+table+" WHERE id=?", id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var value map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	e = decoder.Decode(&value)
	return value, e
}
func jsonPut(ctx context.Context, tx *sql.Tx, table, id string, value any) error {
	raw, e := json.Marshal(value)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO "+table+"(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json", id, string(raw))
	return e
}
func jsonList(ctx context.Context, tx *sql.Tx, table string) ([]map[string]any, error) {
	rows, e := tx.QueryContext(ctx, "SELECT json FROM "+table+" ORDER BY "+rowOrderSQL(false))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		var v map[string]any
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		if e = d.Decode(&v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func headerPresent(r *http.Request, key string) bool {
	_, ok := r.Header[http.CanonicalHeaderKey(key)]
	return ok
}
