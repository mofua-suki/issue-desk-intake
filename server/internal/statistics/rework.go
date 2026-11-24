package statistics

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"regexp"
	"sort"
	"time"
)

func reworkPhase(r map[string]any, epochs map[string]float64) string {
	switch r["step"] {
	case "競合解消":
		return "conflict"
	case "固定テストの修正":
		return "repair"
	case "実装":
	default:
		return ""
	}
	generation := fleet.Number(r["generation"])
	if n, ok := r["deskGeneration"].(float64); ok && n >= 0 {
		generation = 1
		if n > 0 {
			return "return"
		}
		if epoch := fleet.Number(r["leaseEpoch"]); epoch >= 1 && epoch > epochs[fmt.Sprint(r["repo"], "#", r["issue"])] {
			return "return"
		}
	}
	if generation < 1 {
		return "unknown"
	}
	if generation >= 2 {
		return "return"
	}
	if turn := fleet.Number(r["turn"]); turn >= 1 {
		if turn == 1 {
			return "first"
		}
		return "fix"
	}
	if r["turn"] == nil && r["provider"] == "codex" {
		return "first"
	}
	return "unknown"
}
func perIssue(t map[string]any, count int) any {
	if count == 0 {
		return nil
	}
	out := map[string]any{"unknown": t["unknown"], "count": float64(t["count"].(int)) / float64(count)}
	for _, key := range []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "costUsd"} {
		out[key] = nil
		if n, ok := t[key].(float64); ok {
			out[key] = n / float64(count)
		}
	}
	return out
}
func (s *Service) Rework(ctx context.Context, q Query) (any, error) {
	to := q.Before.To
	if q.After.To.After(to) {
		to = q.After.To
	}
	store := usage.Store{DB: s.DB}
	all, _, err := store.Read(ctx, "", time.Unix(0, 0), to)
	if err != nil {
		return nil, err
	}
	all, _ = usage.CorrectCosts(all, []map[string]any{})
	epochs := firstDeskEpochs(all)
	before, err := s.reworkPeriod(ctx, all, epochs, q.Before, q.Project)
	if err != nil {
		return nil, err
	}
	after, err := s.reworkPeriod(ctx, all, epochs, q.After, q.Project)
	if err != nil {
		return nil, err
	}
	return map[string]any{"project": q.Project, "unavailable": []any{}, "before": before, "after": after}, nil
}

// firstDeskEpochs finds each issue's first lease epoch of the desk's first generation.
func firstDeskEpochs(all []map[string]any) map[string]float64 {
	epochs := map[string]float64{}
	for _, r := range all {
		if r["step"] == "実装" && r["deskGeneration"] == 0.0 {
			epoch := fleet.Number(r["leaseEpoch"])
			key := fmt.Sprint(r["repo"], "#", r["issue"])
			if old, ok := epochs[key]; epoch >= 1 && (!ok || epoch < old) {
				epochs[key] = epoch
			}
		}
	}
	return epochs
}

// reworkPeriod totals the period's usage by rework phase, return trigger and finding kind.
func (s *Service) reworkPeriod(ctx context.Context, all []map[string]any, epochs map[string]float64, p Period, project string) (any, error) {
	mine, issues, desk, local := reworkRows(all, epochs, p, project)
	total := usage.Totals(mine, s.Weights)
	total["perIssue"] = perIssue(total, len(issues))
	phases := s.reworkPhases(mine, epochs, len(issues))
	triggers, kinds, auto := reworkReturns(mine, epochs)
	returns, e := s.returnRate(ctx, p, project)
	if e != nil {
		return nil, e
	}
	out := p.Dates()
	out["usage"] = map[string]any{"issues": len(issues), "total": total, "phases": phases, "triggers": s.reworkAxis(triggers, "trigger"), "kinds": s.reworkAxis(kinds, "kind"), "autoReturns": auto, "classificationBasis": map[string]any{"deskGeneration": desk, "generation": local}}
	out["returns"] = returns
	return out, nil
}

// reworkRows selects the period's rows with a rework phase and counts how
// implementation rows were classified.
func reworkRows(all []map[string]any, epochs map[string]float64, p Period, project string) (mine []map[string]any, issues map[string]bool, desk, local int) {
	mine = []map[string]any{}
	issues = map[string]bool{}
	for _, r := range all {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e != nil || at.Before(p.From) || !at.Before(p.To) || project != "" && r["repo"] != project || reworkPhase(r, epochs) == "" {
			continue
		}
		mine = append(mine, r)
		if r["repo"] != nil && fleet.Number(r["issue"]) >= 1 {
			issues[fmt.Sprint(r["repo"], "#", r["issue"])] = true
		}
		if r["step"] == "実装" {
			if v, ok := r["deskGeneration"].(float64); ok && v >= 0 {
				desk++
			} else {
				local++
			}
		}
	}
	return mine, issues, desk, local
}

// reworkPhases totals the rows of each rework phase.
func (s *Service) reworkPhases(mine []map[string]any, epochs map[string]float64, issues int) []any {
	phases := []any{}
	for _, phase := range []string{"first", "fix", "return", "conflict", "repair", "unknown"} {
		rows := []map[string]any{}
		for _, r := range mine {
			if reworkPhase(r, epochs) == phase {
				rows = append(rows, r)
			}
		}
		if phase == "unknown" && len(rows) == 0 {
			continue
		}
		t := usage.Totals(rows, s.Weights)
		t["phase"], t["perIssue"] = phase, perIssue(t, issues)
		phases = append(phases, t)
	}
	return phases
}

// reworkReturns groups the returned rows by trigger and, for automatic returns, by finding kind.
func reworkReturns(mine []map[string]any, epochs map[string]float64) (map[string][]map[string]any, map[string][]map[string]any, int) {
	triggers := map[string][]map[string]any{}
	kinds := map[string][]map[string]any{}
	auto := 0
	for _, r := range mine {
		if reworkPhase(r, epochs) != "return" {
			continue
		}
		trigger := fleet.Text(r["generationTrigger"])
		if trigger == "" {
			trigger = "unknown"
		}
		triggers[trigger] = append(triggers[trigger], r)
		if trigger == "review-return" {
			auto++
			names := fleet.Strings(r["reviewFindingKinds"])
			if len(names) == 0 {
				names = []string{"unknown"}
			}
			seen := map[string]bool{}
			for _, kind := range names {
				if !seen[kind] {
					kinds[kind] = append(kinds[kind], r)
					seen[kind] = true
				}
			}
		}
	}
	return triggers, kinds, auto
}

// reworkAxis totals each group, in name order.
func (s *Service) reworkAxis(groups map[string][]map[string]any, key string) []any {
	out := []any{}
	names := []string{}
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := usage.Totals(groups[name], s.Weights)
		t[key] = name
		out = append(out, t)
	}
	return out
}

func (s *Service) returnRate(ctx context.Context, p Period, project string) (any, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT at,line FROM runner_log_lines WHERE at<$1 ORDER BY at,key", p.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	review := regexp.MustCompile(`^工程開始 (\S+)#([0-9]+) レビュー$`)
	returned := regexp.MustCompile(`^(?:レビュー保留|検証) (\S+)#([0-9]+): (?:自動差し戻し|レビューの指摘をこのPCの実装へ自動で差し戻しました)`)
	reviewed, returns := map[string]bool{}, map[string]bool{}
	var first time.Time
	for rows.Next() {
		var at time.Time
		var line string
		if err = rows.Scan(&at, &line); err != nil {
			return nil, err
		}
		if first.IsZero() {
			first = at
		}
		if at.Before(p.From) {
			continue
		}
		m := review.FindStringSubmatch(line)
		isReturn := false
		if m == nil {
			m = returned.FindStringSubmatch(line)
			isReturn = m != nil
		}
		if m != nil && (project == "" || m[1] == project) {
			key := m[1] + "#" + m[2]
			reviewed[key] = true
			if isReturn {
				returns[key] = true
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if first.IsZero() || !first.Before(p.To) {
		return map[string]any{"reason": "この期間に runner.log の記録がありません", "reviewed": nil, "returned": nil, "rate": nil, "since": nil}, nil
	}
	var rate, since any
	if len(reviewed) > 0 {
		rate = float64(len(returns)) / float64(len(reviewed))
	}
	if first.After(p.From) {
		since = first.In(JST).Format("2006-01-02")
	}
	return map[string]any{"reason": nil, "reviewed": len(reviewed), "returned": len(returns), "rate": rate, "since": since}, nil
}
