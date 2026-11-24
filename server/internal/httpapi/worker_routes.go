package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"issue-desk-intake/server/internal/hostdrift"
	"issue-desk-intake/server/internal/hosts"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
)

type WorkerRouteOptions struct {
	Publish            func(map[string]any)
	PublicLink         func(string, string, int64) string
	AIAlerts           func(context.Context) ([]any, error)
	SystemAlerts       func(context.Context) ([]any, error)
	ContractMismatches *operations.ContractMismatches
	Gitea              interface {
		Do(context.Context, string, string, any, any) error
	}
	ReadComments     func(context.Context, string, int64) ([]map[string]any, error)
	ReportAuthor     func(string) string
	TrustedWriter    func(map[string]any) bool
	LiveAIAlerts     func(context.Context) ([]any, error)
	DraftStallAlerts func(context.Context) ([]any, error)

	HostDrift                  *hostdrift.Service
	HostDriftCompatReleases    *int
	AttentionDraftAlerts       func(context.Context) ([]any, error)
	RuntimeDir                 string
	ModelConfig                func() (map[string]any, error)
	ModelProviders             func(context.Context) ([]map[string]any, error)
	WebhookVerifier            func(context.Context, string, map[string]any) (string, error)
	ReadIssue                  func(context.Context, string, int64) (map[string]any, error)
	AppDB                      *sql.DB
	StatisticsDrafts           func(context.Context) ([]map[string]any, error)
	DeskNumber                 func(string, int64) any
	DraftIssue                 func(string) (string, int64)
	TokenNames                 func(context.Context) ([]string, error)
	DB                         *sql.DB
	ClaudeAPIKeyTokenFile      string
	Auth                       *auth.Authenticator
	FleetPath, AssignmentsPath string
	Now                        func() time.Time
	OnMerged                   func(context.Context, *sql.Tx, *jobs.Job, map[string]any) error
	ReleasePolicy              func(string, map[string]any) map[string]any
	Notice                     func(context.Context, string, map[string]any) (any, error)
	IssueState                 func(context.Context, string, int64, string) error
}
type WorkerRoutes struct {
	statsCache statsCache
	drift      *hostdrift.Service
	startedAt  time.Time
	options    WorkerRouteOptions
	fleet      *fleet.Service
	hosts      *hosts.Service
	jobs       *jobs.Store
}

func RegisterWorkerRoutes(mux *http.ServeMux, options WorkerRouteOptions) error {
	if _, err := hosts.DrainDefault(); err != nil {
		return err
	}
	if options.DB == nil || options.Auth == nil {
		return errors.New("worker API requires persistent backends and authentication")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	config := &fleet.Service{DB: options.DB, FleetPath: options.FleetPath, AssignmentsPath: options.AssignmentsPath, Now: options.Now, ReleasePolicy: options.ReleasePolicy, WebhookVerifier: options.WebhookVerifier}
	_, assignments, err := config.Load()
	if err != nil {
		return err
	}
	if _, _, err := config.Targets(); err != nil {
		return err
	}
	afterMerged := func(ctx context.Context, tx *sql.Tx, job *jobs.Job, result map[string]any) error {
		if options.OnMerged != nil {
			if err := options.OnMerged(ctx, tx, job, result); err != nil {
				return err
			}
		}
		return (&hostd.Service{DB: options.DB, Now: options.Now, Targets: config.Targets}).Merged(ctx, tx, job, result)
	}
	store, err := jobs.New(options.DB, jobs.Options{Now: options.Now, LeaseDuration: time.Duration(fleet.Number(fleet.Object(assignments.Output["dispatch"])["leaseMinutes"])) * time.Minute, AppDB: options.AppDB, ReadIssue: options.ReadIssue, ReadComments: options.ReadComments, ReportAuthor: options.ReportAuthor, OnMerged: afterMerged, Publish: options.Publish})
	if err != nil {
		return err
	}
	s := &WorkerRoutes{startedAt: options.Now(), options: options, fleet: config, hosts: &hosts.Service{DB: options.DB, Now: options.Now, StartedAt: options.Now()}, jobs: store}
	compat := 3
	if options.HostDriftCompatReleases != nil {
		compat = *options.HostDriftCompatReleases
	}
	if compat < 0 {
		return errors.New("hostDriftCompatReleasesは0以上の整数です")
	}
	s.drift = &hostdrift.Service{DB: options.DB, Now: options.Now, CompatLimit: compat}
	if options.HostDrift != nil {
		s.drift = options.HostDrift
	}
	mux.HandleFunc("/api/v1/client-config", s.configRoute)
	mux.HandleFunc("/api/v1/settings", s.settingsRoute)
	mux.HandleFunc("/api/v1/ops-watch/lease", s.operationsRoute)
	for _, path := range []string{"/api/ops-watch/archive", "/api/ops-watch/archive/", "/api/ops-watch/notices/", "/api/incidents/", "/api/usage-review/rounds/"} {
		mux.HandleFunc(path, s.noticeControlRoute)
	}
	mux.HandleFunc("/api/intake/force-handoff", s.operationsRoute)
	mux.HandleFunc("/api/full-test/alerts", s.alertRoute)
	mux.HandleFunc("/api/stalls/alerts", s.alertRoute)
	mux.HandleFunc("/api/attention/alerts", s.alertRoute)
	mux.HandleFunc("/api/usage-review/alerts", s.alertRoute)
	mux.HandleFunc("/api/v1/ops/usage-anomalies", s.anomaliesRoute)
	mux.HandleFunc("/api/v1/usage", s.usageRoute)
	mux.HandleFunc("/api/v1/stop-check-fires", s.stopCheckRoute)
	mux.HandleFunc("/api/v1/usage/summary", s.usageSummary)
	mux.HandleFunc("/api/v1/hosts/", s.hostRoute)
	mux.HandleFunc("/api/model-overrides", s.modelRoute)
	mux.HandleFunc("/api/model-overrides/", s.modelRoute)
	mux.HandleFunc("/api/provider-disables", s.modelRoute)
	mux.HandleFunc("/api/provider-disables/", s.modelRoute)
	mux.HandleFunc("/api/stats", s.statsRoute)
	mux.HandleFunc("/api/stats/overview", s.statsRoute)
	mux.HandleFunc("/api/stats/rework", s.statsRoute)
	mux.HandleFunc("/api/hosts", s.hostsViewRoute)
	mux.HandleFunc("/api/hosts/", s.hostsViewRoute)
	mux.HandleFunc("/api/v1/notices", s.noticeRoute)
	mux.HandleFunc("/api/v1/jobs", s.jobsRoute)
	mux.HandleFunc("/api/v1/jobs/", s.jobsRoute)
	mux.HandleFunc("/api/v1/merge-turns/", s.turnRoute)
	mux.HandleFunc("/api/v1/commands", s.commandRoute)
	mux.HandleFunc("/api/v1/commands/", s.commandRoute)
	mux.HandleFunc("/api/v1/requests", s.requestRoute)
	mux.HandleFunc("/api/v1/requests/", s.requestRoute)
	return nil
}

func workerSend(w http.ResponseWriter, value any, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	w.WriteHeader(status)
	if status != 204 {
		_ = json.NewEncoder(w).Encode(value)
	}
}
func workerFailure(w http.ResponseWriter, err error) {
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem == nil {
		contract.LogFailure("監視のAPI", err)
		problem = contract.NewProblem(503, "受付の記録を読み書きできません。少し待って同じ要求を再照合してください。", "")
	}
	contract.WriteProblem(w, problem)
}
func workerMethod(r *http.Request, method string) error {
	if r.Method != method {
		return contract.NewProblem(405, "この経路のメソッドが違います。", "")
	}
	return nil
}
func workerBody(r *http.Request, keys ...string) (map[string]any, error) {
	return workerBodyWithLimit(r, contract.JobsBodyLimit, keys...)
}
func workerBodyWithLimit(r *http.Request, limit int64, keys ...string) (map[string]any, error) {
	body, problem := contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), limit)
	if problem != nil {
		return nil, problem
	}
	if keys != nil {
		if problem = contract.OnlyKeys(body, keys...); problem != nil {
			return nil, problem
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var normalized map[string]any
	err = json.Unmarshal(raw, &normalized)
	return normalized, err
}
func workerInteger(value any, min, max float64) (int64, error) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < min || number > max || number != math.Trunc(number) {
		return 0, contract.NewProblem(400, "整数の形が違います。", "")
	}
	return int64(number), nil
}

func (s *WorkerRoutes) identity(w http.ResponseWriter, r *http.Request, policy auth.Policy) (auth.Principal, bool) {
	principal, problem := s.options.Auth.Authenticate(r)
	if problem == nil {
		problem = auth.Authorize(principal, policy)
	}
	if problem != nil {
		contract.WriteProblem(w, problem)
		return principal, false
	}
	return principal, true
}

func (s *WorkerRoutes) configRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true})
	if !ok {
		return
	}
	if err := workerMethod(r, "GET"); err != nil {
		workerFailure(w, err)
		return
	}
	host := r.URL.Query().Get("host")
	if values, exists := r.URL.Query()["host"]; exists && (len(values) != 1 || host != principal.Name) {
		workerFailure(w, contract.NewProblem(403, "トークンのPCと同じ設定だけを読めます。", ""))
		return
	}
	value, err := s.fleet.ClientConfig(r.Context(), principal.Name, r.URL.Query().Get("since"))
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}

func (s *WorkerRoutes) settingsRoute(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.identity(w, r, auth.Policy{PC: true}); !ok {
		return
	}
	if err := workerMethod(r, "GET"); err != nil {
		workerFailure(w, err)
		return
	}
	settings, version, err := s.fleet.Settings(r.Context())
	if err != nil {
		workerFailure(w, err)
		return
	}
	if r.URL.Query().Get("since") == version {
		workerSend(w, map[string]any{"version": version, "unchanged": true}, 200)
		return
	}
	workerSend(w, map[string]any{"version": version, "settings": settings}, 200)
}

func (s *WorkerRoutes) hostRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/hosts/"), "/")
	if len(parts) == 2 && parts[1] == "claude-api-key" {
		s.claudeAPIKeyRoute(w, r, parts[0])
		return
	}
	if len(parts) < 1 || !fleet.ValidHost(parts[0]) {
		workerFailure(w, contract.NewProblem(404, "PCが見つかりません。", ""))
		return
	}
	host := parts[0]
	if len(parts) == 2 && (parts[1] != "drain" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Authorization"))), "bearer idh_")) {
		s.hostdRoute(w, r, host, parts[1])
		return
	}
	policy := auth.Policy{PC: true, Host: host}
	if len(parts) == 2 && parts[1] == "drain" {
		policy.Host = ""
	}
	principal, ok := s.identity(w, r, policy)
	if !ok {
		return
	}
	var value any
	var err error
	if len(parts) == 1 {
		err = workerMethod(r, "PUT")
		if err == nil {
			var body map[string]any
			body, err = workerBody(r)
			if err == nil {
				value, err = s.hosts.Put(r.Context(), host, body)
				if err == nil {
					_, version, e := s.fleet.Settings(r.Context())
					if e == nil {
						value.(map[string]any)["settingsVersion"] = version
					}
				}
			}
		}
	} else if len(parts) == 2 && parts[1] == "drain" {
		if problem := auth.Authorize(principal, auth.Policy{PC: true, RequireDelegation: true}); problem != nil {
			err = problem
		} else if r.Method != "PUT" && r.Method != "DELETE" {
			err = contract.NewProblem(405, "停止印はPUTまたはDELETEです。", "")
		} else {
			body := map[string]any{}
			if r.Method == "PUT" {
				body, err = workerBody(r, "until", "reason")
			}
			if err == nil {
				value, err = s.hosts.Drain(r.Context(), host, principal.Name, body, r.Method == "PUT")
			}
		}
	} else {
		err = contract.NewProblem(404, "経路が見つかりません。", "")
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}

func (s *WorkerRoutes) noticeRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true})
	if !ok {
		return
	}
	if err := workerMethod(r, "POST"); err != nil {
		workerFailure(w, err)
		return
	}
	body, err := workerBody(r)
	if err != nil {
		workerFailure(w, err)
		return
	}
	var value any
	if oneHostd(fleet.Text(body["kind"]), "ops-watch", "format-version", "remote-control", "boot-check", "stage-watch", "desk-checkout-lock", "rc-update", "app-health") {
		if body["kind"] == "ops-watch" {
			if p := auth.Authorize(principal, auth.Policy{PC: true, RequireDelegation: true}); p != nil {
				workerFailure(w, p)
				return
			}
		}
		if s.options.Notice == nil {
			err = contract.NewProblem(503, "お知らせの保存先が設定されていません。", "")
		} else {
			value, err = s.options.Notice(r.Context(), principal.Name, body)
		}
	} else {
		value, err = s.hosts.Notice(r.Context(), principal.Name, body)
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}
