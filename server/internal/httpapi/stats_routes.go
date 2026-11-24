package httpapi

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/statistics"
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"
)

func (s *WorkerRoutes) statsRoute(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.identity(w, r, auth.Policy{PC: true, Screen: true, CLI: true, RequireDelegation: true}); !ok {
		return
	}
	if err := workerMethod(r, "GET"); err != nil {
		workerFailure(w, err)
		return
	}
	document, assignments, err := s.fleet.Load()
	if err != nil {
		workerFailure(w, err)
		return
	}
	repos := statsRepos(&document)
	query, details, err := s.parseStatsRequest(r, repos)
	if err != nil {
		workerFailure(w, err)
		return
	}
	key := r.URL.Path + "?" + r.URL.Query().Encode()
	result, ok := s.statsCache.get(r.Context(), key, s.options.Now, func(ctx context.Context) statsResult {
		service := statistics.Service{TokenNames: s.options.TokenNames, Assignments: assignments, UsageReviewHost: fleet.Text(assignments.Output["usageReviewHost"]), RuntimeDir: s.options.RuntimeDir, DB: s.options.DB, Risk: fleet.Object(assignments.Output["intakeSplit"]), AppDB: s.options.AppDB, Now: s.options.Now, Weights: fleet.Object(document.Defaults["usageWeights"]), Desk: s.options.DeskNumber, DraftIssue: s.options.DraftIssue, Drafts: statsDraftSnapshot(s.options.StatisticsDrafts)}
		return s.calculateStats(ctx, r, service, query, repos, details)
	})
	if !ok {
		return
	}
	if result.err != nil {
		workerFailure(w, result.err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.body)
}

// statsRepos は受付に登録されたリポジトリの名前を、重複を除いて並べる。
func statsRepos(document *fleet.Document) []string {
	repos := []string{}
	seen := map[string]bool{}
	for _, raw := range allRepos(document) {
		repo := fleet.Text(fleet.Object(raw)["name"])
		if repo != "" && !seen[repo] {
			repos = append(repos, repo)
			seen[repo] = true
		}
	}
	return repos
}

// parseStatsRequest は集計の期間・詳細・範囲の指定を検査する。
func (s *WorkerRoutes) parseStatsRequest(r *http.Request, repos []string) (statistics.Query, bool, error) {
	query := statistics.Query{}
	var err error
	if r.URL.Path != "/api/stats/rework" {
		query, err = statistics.Parse(r.URL.Query(), s.options.Now(), repos)
	}
	if err != nil {
		return query, false, err
	}
	details := false
	if r.URL.Path == "/api/stats" || r.URL.Path == "/api/stats/rework" {
		values, exists := r.URL.Query()["details"]
		if exists {
			if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
				return query, false, contract.NewProblem(400, "detailsはtrueまたはfalseで指定してください。", "")
			}
			details = values[0] == "true"
		}
	}
	if r.URL.Path == "/api/stats/rework" {
		if _, _, err := statistics.ReworkQuery(r.URL.Query(), s.options.Now(), repos); err != nil {
			return query, false, err
		}
	}
	if r.URL.Path == "/api/stats/overview" {
		name := r.URL.Query().Get("range")
		if name != "" && name != "today" && name != "7d" && name != "30d" {
			return query, false, contract.NewProblem(400, "rangeはtoday・7d・30dです。", "")
		}
	}
	return query, details, nil
}

func (s *WorkerRoutes) calculateStats(ctx context.Context, r *http.Request, service statistics.Service, query statistics.Query, repos []string, details bool) statsResult {
	var value any
	var err error
	partial := false
	service.Summary = !details
	if r.URL.Path == "/api/stats/rework" {
		value, err = service.ReworkBreakdown(ctx, r.URL.Query(), repos)
	} else if r.URL.Path == "/api/stats/overview" {
		value, err = service.Overview(ctx, r.URL.Query(), repos)
	} else {
		started := time.Now()
		value, err = service.Work(ctx, query, repos)
		log.Printf("stats calculation table=work duration_ms=%.3f", float64(time.Since(started))/float64(time.Millisecond))
		if err == nil {
			out := value.(map[string]any)
			known := strings.Fields("stopChecks inputBudget implementation tiers usage usageLimits fixedTestUsage rework reviewNotes followups usageReview priorityReview antigravity scope sizeEstimates workTypes testSelection stalls inventory issueBody waits throughput filingCheck consultationBudget giteaReads")
			var selected map[string]bool
			raw, exists := r.URL.Query()["tables"]
			selected, err = selectedStatsTables(raw, exists, known)
			if err == nil {
				partial = fillStatsTables(ctx, service, query, known, selected, out)
			}
		}
	}
	if err != nil {
		return statsResult{err: err}
	}
	if r.URL.Path == "/api/stats" || r.URL.Path == "/api/stats/rework" {
		applyStatsDetails(value, details)
	}
	partial = partial || statsUnavailable(reflect.ValueOf(value))
	body, err := json.Marshal(value)
	if err != nil {
		return statsResult{}
	}
	return statsResult{body: append(body, '\n'), cacheable: !partial && ctx.Err() == nil}
}

// selectedStatsTables は tables の指定を検査し、集計する表を返す。指定が無ければ全部の表を返す。
func selectedStatsTables(raw []string, exists bool, known []string) (map[string]bool, error) {
	selected := map[string]bool{}
	if !exists {
		for _, key := range known {
			selected[key] = true
		}
		return selected, nil
	}
	if len(raw) != 1 || strings.TrimSpace(raw[0]) == "" {
		return nil, contract.NewProblem(400, "tablesは表の名前を指定してください。", "")
	}
	for _, key := range strings.Split(raw[0], ",") {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		valid := false
		for _, name := range known {
			if key == name {
				valid = true
			}
		}
		if !valid {
			return nil, contract.NewProblem(400, "tablesに知らない表の名前があります。", "")
		}
		selected[key] = true
	}
	return selected, nil
}

// fillStatsTables は選んだ表を集計して結果に入れ、読めなかった表は読めない印を付ける。読めない表があったかを返す。
func fillStatsTables(ctx context.Context, service statistics.Service, query statistics.Query, known []string, selected map[string]bool, out map[string]any) bool {
	partial := false
	for _, name := range known {
		if !selected[name] {
			continue
		}
		var report any
		var failure error
		started := time.Now()
		switch name {
		case "usage":
			report, failure = service.Usage(ctx, query)
		case "rework":
			report, failure = service.Rework(ctx, query)
		case "implementation":
			report, failure = service.Implementation(ctx, query)
		case "scope":
			report, failure = service.Scope(ctx, query)
		case "fixedTestUsage":
			report, failure = service.FixedTests(ctx, query)
		default:
			report, failure = service.Saved(ctx, name, query)
		}
		log.Printf("stats calculation table=%s duration_ms=%.3f", name, float64(time.Since(started))/float64(time.Millisecond))
		if failure != nil {
			partial = true
			out["unavailable"] = append(out["unavailable"].([]any), name+"の集計を読み取れませんでした。")
			out[name] = nil
		} else {
			out[name] = report
		}
	}
	return partial
}
