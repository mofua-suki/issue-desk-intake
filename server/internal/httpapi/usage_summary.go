package httpapi

import (
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (s *WorkerRoutes) usageSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.identity(w, r, auth.Policy{PC: true, RequireDelegation: true}); !ok {
		return
	}
	if err := workerMethod(r, "GET"); err != nil {
		workerFailure(w, err)
		return
	}
	document, a, err := s.fleet.Load()
	if err != nil {
		workerFailure(w, err)
		return
	}
	project := r.URL.Query().Get("project")
	registered := project == ""
	for _, raw := range allRepos(&document) {
		if fleet.Object(raw)["name"] == project {
			registered = true
		}
	}
	if !registered {
		workerFailure(w, contract.NewProblem(400, "登録済みリポジトリを指定してください。", ""))
		return
	}
	now := s.options.Now()
	jst := time.FixedZone("JST", 9*3600)
	day := now.In(jst)
	to := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, jst)
	from := to.Add(-30 * 24 * time.Hour)
	db := usage.Store{DB: s.options.DB}
	rows, skipped, err := db.Read(r.Context(), project, from, to)
	if err != nil {
		workerFailure(w, err)
		return
	}
	lookback, _, err := db.Read(r.Context(), project, from.Add(-30*24*time.Hour), from)
	if err != nil {
		workerFailure(w, err)
		return
	}
	rows, statuses := usage.CorrectCosts(rows, lookback)
	correction := map[string]int{"corrected": 0, "uncorrectable": 0, "filled": 0}
	for _, status := range statuses {
		if status != "" {
			correction[status]++
		}
	}
	weights := fleet.Object(document.Defaults["usageWeights"])
	hosts := make([]string, 0, len(a.Hosts))
	for host := range a.Hosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	var senders []string
	if s.options.TokenNames != nil {
		senders, err = s.options.TokenNames(r.Context())
		if err != nil {
			workerFailure(w, err)
			return
		}
	}
	coverage, err := db.Coverage(r.Context(), from, to, now, hosts, senders)
	if err != nil {
		workerFailure(w, err)
		return
	}
	summary := []string{fmt.Sprintf("使用量 %s〜%s（受付DB）", from.Format("2006-01-02"), to.Format("2006-01-02")), "入力・出力・キャッシュは記録された値の合計。費用は同一セッションの累積値を補正。不明は0へ補完しません。", summaryTotals(usage.Totals(rows, weights)), fmt.Sprintf("費用補正%v件・補完%v件・補正不能%v件", correction["corrected"], correction["filled"], correction["uncorrectable"])}
	details := []string{}
	for _, axis := range []string{"step", "provider", "model", "repo"} {
		for _, group := range usage.Groups(rows, axis, weights) {
			line := fmt.Sprintf("%s=%v %s", axis, group[axis], summaryTotals(group))
			if len(usage.Redact(strings.Join(details, "\n")))+len(line) > 2500 {
				break
			}
			details = append(details, line)
		}
	}
	workerSend(w, map[string]any{"source": "usage-db", "project": project, "from": contract.ISO(from), "to": contract.ISO(to), "generatedAt": contract.ISO(now), "ok": true, "reason": nil, "skipped": skipped, "summaries": []any{map[string]any{"kind": "usage", "text": usage.Redact(strings.Join(summary, "\n"))}, map[string]any{"kind": "detail", "text": usage.Redact(strings.Join(details, "\n"))}, map[string]any{"kind": "coverage", "text": usage.CoverageText(coverage)}}}, 200)
}
func allRepos(document *fleet.Document) []any {
	out := append([]any{}, fleet.List(document.Defaults["repositories"])...)
	for _, config := range document.Hosts {
		out = append(out, fleet.List(config["repositories"])...)
	}
	return out
}
func summaryTotals(total map[string]any) string {
	parts := []string{fmt.Sprintf("%v件", total["count"])}
	unknown := fleet.Object(total["unknown"])
	for _, key := range []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "costUsd", "weighted"} {
		value := "不明"
		if total[key] != nil {
			value = fmt.Sprint(total[key])
		}
		parts = append(parts, fmt.Sprintf("%s=%s（不明%v件）", key, value, unknown[key]))
	}
	return strings.Join(parts, " ")
}
