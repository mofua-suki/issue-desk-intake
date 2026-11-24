package screenapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/listscope"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Object = map[string]any
type ListReader func(context.Context, string) ([]Object, error)

// ListScope returns the requested list range for downstream readers.
func ListScope(ctx context.Context) string {
	if listscope.Active(ctx) {
		return "active"
	}
	return "full"
}

type EntryReader func(context.Context, Object, string) (Object, error)
type Options struct {
	Auth                 *auth.Authenticator
	Postgres             *sql.DB
	AppDB                *sql.DB
	RuntimeDir, RepoRoot string
	Now                  func() time.Time
	Config               func() (Object, error)
	RuntimeMeasurements  func() map[string]any
	Meta                 func(context.Context) (Object, error)
	Providers            func(context.Context) (any, error)
	Lists                map[string]ListReader
	Entries              map[string]EntryReader
	Aux                  map[string]func(context.Context) (any, error)
	Usage                func(context.Context, time.Time, time.Time) ([]Object, error)
	UsageWeights         map[string]any
	Outcome              func(context.Context, string) (Object, error)
	Delta                DeltaOptions
}
type Service struct {
	o        Options
	delta    *Delta
	profiles *CPUProfiles
	listUse  ListMeasurements
	joins    listJoins
}

var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func fail(status int, message string) *contract.Problem {
	return contract.NewProblem(status, message, "")
}
func clone(v Object) Object {
	b, _ := json.Marshal(v)
	m := Object{}
	_ = json.Unmarshal(b, &m)
	return m
}
func Register(mux *http.ServeMux, o Options) (*Service, error) {
	if mux == nil || o.Auth == nil || o.AppDB == nil || o.Config == nil {
		return nil, errors.New("screen authentication, persistence and configuration are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.Delta.Now = o.Now
	s := &Service{o: o, delta: NewDelta(o.Delta), profiles: NewCPUProfiles(o.RuntimeDir, o.RepoRoot)}
	for _, path := range []string{"/api/meta", "/api/list", "/api/list/entry", "/api/list/timing", "/api/cpu-profiles", "/api/cpu-profiles/", "/api/topic-links", "/api/draft-withdrawals"} {
		mux.Handle(path, s)
	}
	mux.HandleFunc("POST /api/drafts/{id}/topic-links/remove", s.ServeHTTP)
	return s, nil
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var timing *listTiming
	if r.Method == "GET" && r.URL.Path == "/api/list" {
		timing = newListTiming()
		counted := &listResponseWriter{ResponseWriter: w, status: http.StatusOK}
		w = counted
		defer func() {
			// Counts are neither a full-list nor a delta response.
			if !oneOf(r.URL.Query().Get("view"), "counts", "work") {
				s.listUse.server(s.o.Now(), timing, counted)
			}
		}()
	}
	principal, p := s.o.Auth.Authenticate(r)
	if p != nil {
		contract.WriteProblem(w, p)
		return
	}
	if p = auth.Authorize(principal, screenRoutePolicy(r)); p != nil {
		contract.WriteProblem(w, p)
		return
	}
	ctx, p := s.o.Auth.Context(r)
	if p != nil {
		contract.WriteProblem(w, p)
		return
	}
	var value any
	var e error
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/meta":
		var meta Object
		if meta, e = s.meta(r.Context(), ctx.Origin); e == nil {
			meta["listMeasurements"] = s.listUse.snapshot()
			value = meta
		}
	case r.Method == "GET" && r.URL.Path == "/api/list":
		query, rangeName, countsOnly, parseErr := parseListQuery(r.URL.RawQuery)
		e = parseErr
		if e == nil {
			timing.scope = rangeName
			var cursor *string
			if values, has := query["since"]; has {
				cursor = &values[0]
			}
			config, err := s.o.Config()
			if err != nil {
				e = err
				break
			}
			if countsOnly {
				value, e = s.listCounts(r.Context(), ctx.Origin, rangeName, query.Get("view"), config, timing)
				break
			}
			value, e = s.listDelta(r, principal, ctx.Origin, rangeName, cursor, config, timing)
		}
	case r.Method == "POST" && r.URL.Path == "/api/list/timing":
		var data Object
		data, p = contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 2000)
		if p == nil {
			p = s.listUse.browser(s.o.Now(), data)
		}
		if p != nil {
			e = p
		} else {
			value = Object{"ok": true}
		}
	case r.Method == "GET" && r.URL.Path == "/api/list/entry":
		value, e = s.entry(r.Context(), r.URL.Query(), ctx.Origin)
	case r.Method == "GET" && r.URL.Path == "/api/cpu-profiles":
		value, e = s.profiles.List(r.Context())
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/cpu-profiles/"):
		value, e = s.profiles.Detail(r.Context(), strings.TrimPrefix(r.URL.Path, "/api/cpu-profiles/"))
	case r.Method == "GET" && r.URL.Path == "/api/topic-links":
		value, e = s.topicLinks(r.Context(), r.URL.Query())
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/topic-links/remove"):
		var data Object
		data, p = contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 10000)
		if p != nil {
			e = p
		} else {
			value, e = s.removeTopicLink(r.Context(), r.PathValue("id"), data, principal.Name)
		}
	case r.Method == "GET" && r.URL.Path == "/api/draft-withdrawals":
		value, e = s.withdrawals(r.Context(), r.URL.Query())
	default:
		e = fail(405, "このメソッドは使えません。")
	}
	if e != nil {
		var p *contract.Problem
		if !errors.As(e, &p) || p == nil {
			contract.LogFailure("画面のAPI "+r.URL.Path, e)
			p = fail(503, "画面の情報を取得できません。")
		}
		contract.WriteProblem(w, p)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	if timing == nil {
		_ = json.NewEncoder(w).Encode(value)
		return
	}
	var body bytes.Buffer
	startedJSON := time.Now()
	_ = json.NewEncoder(&body).Encode(value)
	timing.stages["json"] = time.Since(startedJSON)
	w.Header().Set("Server-Timing", timing.header())
	_, _ = w.Write(body.Bytes())
}

// screenRoutePolicy は経路ごとに受け付ける送信元を決める。
func screenRoutePolicy(r *http.Request) auth.Policy {
	policy := auth.Policy{Screen: true, CLI: true}
	// Local one-time keys may read the same projection as the screen. Browser
	// measurements remain screen-only; delegated PC tokens may read only counts.
	if r.Method == "GET" && r.URL.Path == "/api/list" && r.URL.Query().Get("view") == "counts" {
		policy = auth.Policy{Screen: true, CLI: true, PC: true, RequireDelegation: true}
	} else if r.URL.Path == "/api/list/timing" {
		policy = auth.Policy{Screen: true}
	} else if strings.HasPrefix(r.URL.Path, "/api/cpu-profiles") {
		policy = auth.Policy{PC: true}
	} else if r.URL.Path == "/api/topic-links" || r.URL.Path == "/api/draft-withdrawals" || strings.HasSuffix(r.URL.Path, "/topic-links/remove") {
		policy = auth.Policy{PC: true, RequireDelegation: true}
	}
	return policy
}

// parseListQuery は一覧のクエリを検査し、読む範囲と件数だけの応答（view=counts・work）かを返す。
func parseListQuery(raw string) (url.Values, string, bool, error) {
	var e error
	rangeName := "full"
	query, parseErr := url.ParseQuery(raw)
	if parseErr != nil {
		e = fail(400, "一覧のクエリを解析できません。")
	}
	for key, values := range query {
		if len(values) != 1 || (key != "since" && key != "tabs" && key != "view") || (key == "tabs" && values[0] != "active") || (key == "view" && !oneOf(values[0], "counts", "work")) {
			e = fail(400, "一覧のクエリはsince、tabs=active、view=countsまたはworkをそれぞれ1つまで指定してください。")
		} else if key == "tabs" {
			rangeName = "active"
		}
	}
	countsOnly := oneOf(query.Get("view"), "counts", "work")
	if countsOnly {
		rangeName = "active"
		if _, has := query["since"]; has {
			e = fail(400, "view=countsまたはworkとsinceは併用できません。")
		}
	}
	return query, rangeName, countsOnly, e
}

// listCounts は一覧の件数（view=work では作業の組と系列も）を、読めなかった補助の情報とともに返す。
func (s *Service) listCounts(ctx context.Context, origin, rangeName, view string, config Object, timing *listTiming) (any, error) {
	loadContext := listscope.WithScope(ctx, rangeName)
	snapshot, err := s.joinSnapshot(loadContext, origin, rangeName, config, timing)
	if err != nil {
		return nil, err
	}
	missing := []string{}
	for _, name := range auxiliary {
		if _, ok := snapshot.Aux[name]; !ok {
			missing = append(missing, name)
		}
	}
	groups := listWorkGroups(snapshot)
	response := Object{"counts": countSnapshotGroups(groups, snapshot), "checkedAt": s.o.Now().UTC().Format(time.RFC3339Nano), "scope": rangeName, "partial": len(missing) > 0 || len(snapshot.UnreadProjects) > 0, "unavailable": missing}
	if len(snapshot.UnreadProjects) > 0 {
		// Counts omit the unread candidates' Issues.
		response["unreadProjects"] = snapshot.UnreadProjects
	}
	if view == "work" {
		response["groups"], response["families"] = groups, listFamilies(snapshot)
	}
	return response, nil
}

// listDelta は送信元ごとの続きの位置から一覧の全件または差分を返し、所要時間を計る。
func (s *Service) listDelta(r *http.Request, principal auth.Principal, origin, rangeName string, cursor *string, config Object, timing *listTiming) (any, error) {
	// CLI keys are single-use, so their cursor identity must stay stable
	// across keys while remaining separate from every screen session.
	identity := string(principal.Kind) + ":"
	if principal.Kind == auth.Screen {
		cookie, err := r.Cookie("desk_session")
		if err != nil {
			return nil, fail(401, "画面のセッションが必要です。")
		}
		identity += cookie.Value
	}
	scope := sessionScope(identity, config, origin) + ":" + rangeName
	startedDelta := time.Now()
	loadContext := listscope.WithScope(r.Context(), rangeName)
	body, e := s.delta.Respond(loadContext, scope, cursor, func(loadContext context.Context) (Snapshot, error) {
		return s.joinSnapshot(loadContext, origin, rangeName, config, timing)
	})
	if e == nil && body != nil {
		body["scope"] = rangeName
	}
	timing.stages["delta"] = time.Since(startedDelta) - timing.snapshot
	timing.observe(body)
	return body, e
}
