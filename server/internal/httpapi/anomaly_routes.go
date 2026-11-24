package httpapi

import (
	"context"
	"issue-desk-intake/server/internal/anomalies"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/statistics"
	"net/http"
	"time"
)

func (s *WorkerRoutes) anomaliesRoute(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.identity(w, r, auth.Policy{PC: true, RequireDelegation: true}); !ok {
		return
	}
	if e := workerMethod(r, "GET"); e != nil {
		workerFailure(w, e)
		return
	}
	from, to, e := anomalies.Range(r.URL.Query(), s.options.Now())
	if e != nil {
		workerFailure(w, e)
		return
	}
	document, _, e := s.fleet.Load()
	if e != nil {
		workerFailure(w, e)
		return
	}
	repos := []string{}
	seen := map[string]bool{}
	for _, raw := range allRepos(&document) {
		repo := fleet.Text(fleet.Object(raw)["name"])
		if repo != "" && !seen[repo] {
			repos = append(repos, repo)
			seen[repo] = true
		}
	}
	stats := statistics.Service{DB: s.options.DB, AppDB: s.options.AppDB, RuntimeDir: s.options.RuntimeDir, Now: s.options.Now}
	service := anomalies.Service{DB: s.options.DB, Now: s.options.Now, Weights: fleet.Object(document.Defaults["usageWeights"]), Repositories: repos, FilingCheck: func(ctx context.Context, repo string, to time.Time) (map[string]any, error) {
		value, e := stats.FilingCheck(ctx, statistics.Query{Project: repo, Before: statistics.Period{From: to.Add(-14 * 24 * time.Hour), To: to.Add(-7 * 24 * time.Hour)}, After: statistics.Period{From: to.Add(-7 * 24 * time.Hour), To: to}})
		if e != nil {
			return nil, e
		}
		return value.(map[string]any), nil
	}}
	value, e := service.Detect(r.Context(), from, to)
	if e != nil {
		workerFailure(w, e)
		return
	}
	workerSend(w, value, 200)
}
