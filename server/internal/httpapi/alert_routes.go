package httpapi

import (
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/statistics"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

func (s *WorkerRoutes) alertRoute(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.identity(w, r, auth.Policy{PC: true, Screen: true, CLI: true, RequireDelegation: true}); !ok {
		return
	}
	if e := workerMethod(r, "GET"); e != nil {
		workerFailure(w, e)
		return
	}
	var value any
	var e error
	switch r.URL.Path {
	case "/api/stalls/alerts":
		service := s.operationService()
		_, a, err := s.fleet.Load()
		e = err
		if e == nil {
			service.Assignments = a
			value, e = service.Stalls(r.Context())
		}
	case "/api/attention/alerts":
		filters, err := parseAttentionFilters(r.URL.RawQuery)
		if err != nil {
			workerFailure(w, err)
			return
		}
		service := s.operationService()
		var rows []any
		timings := []string{}
		rows, e = service.AttentionWithTiming(r.Context(), func(name string, elapsed time.Duration) {
			timings = append(timings, fmt.Sprintf("%s;dur=%.3f", name, float64(elapsed)/float64(time.Millisecond)))
		})
		if len(timings) > 0 {
			w.Header().Set("Server-Timing", strings.Join(timings, ", "))
		}
		if e == nil {
			value = filters.apply(rows)
		}
	case "/api/full-test/alerts":
		value, e = (&operations.Service{DB: s.options.AppDB, RuntimeDir: s.options.RuntimeDir, Now: s.options.Now}).FullTests(r.Context())
	case "/api/usage-review/alerts":
		var doc fleet.Document
		var a *fleet.Assignments
		doc, a, e = s.fleet.Load()
		if e == nil {
			stats := &statistics.Service{DB: s.options.DB, AppDB: s.options.AppDB, RuntimeDir: s.options.RuntimeDir, Now: s.options.Now, Weights: fleet.Object(doc.Defaults["usageWeights"]), Assignments: a, Desk: s.options.DeskNumber}
			value, e = (&operations.Service{Statistics: stats}).UsageReviewAlerts(r.Context())
		}
	}
	if e != nil {
		workerFailure(w, e)
		return
	}
	workerSend(w, value, 200)
}

func (s *WorkerRoutes) operationService() *operations.Service {
	var stats *statistics.Service
	doc, a, e := s.fleet.Load()
	if e == nil {
		stats = &statistics.Service{DB: s.options.DB, AppDB: s.options.AppDB, RuntimeDir: s.options.RuntimeDir, Now: s.options.Now, Weights: fleet.Object(doc.Defaults["usageWeights"]), Assignments: a, Desk: s.options.DeskNumber}
	}
	return &operations.Service{PublicLink: s.options.PublicLink, Assignments: a, AIAlerts: s.options.AIAlerts, SystemAlerts: s.options.SystemAlerts, ContractMismatches: s.options.ContractMismatches, Statistics: stats, Gitea: s.options.Gitea, Fleet: s.fleet.Load, ReadComments: s.options.ReadComments, TrustedWriter: s.options.TrustedWriter, LiveAIAlerts: s.options.LiveAIAlerts, DraftStallAlerts: s.options.DraftStallAlerts, Drift: s.drift, DB: s.options.AppDB, PostgreSQL: s.options.DB, RuntimeDir: s.options.RuntimeDir, Now: s.options.Now, StartedAt: s.startedAt, DraftAlerts: s.options.AttentionDraftAlerts, Desk: s.options.DeskNumber, Repositories: func() ([]string, error) {
		doc, _, e := s.fleet.Load()
		if e != nil {
			return nil, e
		}
		out := []string{}
		for _, raw := range allRepos(&doc) {
			out = append(out, fleet.Text(fleet.Object(raw)["name"]))
		}
		return out, nil
	}}
}

// attentionFilters compares the original API row fields, before presentation transforms.
type attentionFilters map[string]map[string]bool

func parseAttentionFilters(raw string) (attentionFilters, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, contract.NewProblem(400, "絞り込みの指定が不正です。", "")
	}
	filters := attentionFilters{}
	for key, entries := range values {
		if key != "project" && key != "kind" && key != "excludeProject" && key != "excludeKind" {
			return nil, contract.NewProblem(400, "不明な絞り込みです。", "")
		}
		filters[key] = map[string]bool{}
		for _, entry := range entries {
			if entry == "" || strings.TrimSpace(entry) != entry || strings.IndexFunc(entry, unicode.IsControl) >= 0 {
				return nil, contract.NewProblem(400, "絞り込みの値が不正です。", "")
			}
			filters[key][entry] = true
		}
	}
	return filters, nil
}

func (f attentionFilters) apply(rows []any) []any {
	if len(f) == 0 {
		return rows
	}
	out := []any{}
	for _, raw := range rows {
		row := fleet.Object(raw)
		project, kind := fleet.Text(row["project"]), fleet.Text(row["kind"])
		if len(f["project"]) > 0 && !f["project"][project] || len(f["kind"]) > 0 && !f["kind"][kind] || f["excludeProject"][project] || f["excludeKind"][kind] {
			continue
		}
		out = append(out, raw)
	}
	return out
}
