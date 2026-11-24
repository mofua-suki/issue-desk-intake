package statistics

import (
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
	"time"
)

var outcomeMetrics = []string{"costUsd", "inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens"}

func outcomeTotals(rows []map[string]any) map[string]any {
	out := map[string]any{"count": len(rows)}
	unknown := map[string]any{}
	providers := map[string]bool{}
	missingProvider := false
	for _, key := range outcomeMetrics {
		out[key] = nil
		unknown[key] = 0
	}
	for _, r := range rows {
		p := fleet.Text(r["provider"])
		if p == "" {
			missingProvider = true
		} else {
			providers[p] = true
		}
		for _, key := range outcomeMetrics {
			if n, ok := r[key].(float64); ok {
				out[key] = fleet.Number(out[key]) + n
			} else {
				unknown[key] = unknown[key].(int) + 1
			}
		}
	}
	keys := []string{}
	for key := range providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	list := []any{}
	for _, key := range keys {
		list = append(list, key)
	}
	if missingProvider {
		list = append(list, nil)
	}
	out["providers"], out["unknown"] = list, unknown
	return out
}
func outcomeIssue(rows []map[string]any) any {
	if len(rows) == 0 {
		return nil
	}
	out := map[string]any{"total": outcomeTotals(rows), "implementation": nil}
	groups := map[string][]map[string]any{}
	unknown := []map[string]any{}
	for _, r := range rows {
		step := fleet.Text(r["step"])
		if step == "" {
			unknown = append(unknown, r)
		} else {
			groups[step] = append(groups[step], r)
		}
		if step == "実装" {
			out["implementation"] = map[string]any{"provider": r["provider"], "model": r["model"]}
		}
	}
	keys := []string{}
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	steps := []any{}
	for _, key := range keys {
		row := outcomeTotals(groups[key])
		row["step"] = key
		steps = append(steps, row)
	}
	if len(unknown) > 0 {
		row := outcomeTotals(unknown)
		row["step"] = nil
		steps = append(steps, row)
	}
	out["steps"] = steps
	return out
}
func combinedOutcome(draftRows, issueRows []map[string]any, shared int) any {
	if len(draftRows)+len(issueRows) == 0 {
		return nil
	}
	all := append(append([]map[string]any{}, draftRows...), issueRows...)
	out := outcomeTotals(all)
	out["includesConsultation"], out["consultationShared"] = len(draftRows) > 0, shared
	return out
}
func tierJudged(row map[string]any) (string, any) {
	c := fleet.Object(row["combined"])
	if fleet.Number(c["count"]) == 0 {
		return "", nil
	}
	ps := fleet.List(c["providers"])
	claude, codex := len(ps) > 0, false
	for _, p := range ps {
		claude = claude && p == "claude"
		codex = codex || p == "codex"
	}
	unknown := fleet.Object(c["unknown"])
	if claude {
		if c["costUsd"] != nil && fleet.Number(unknown["costUsd"]) == 0 {
			return "cost", c["costUsd"]
		}
		return "cost", nil
	}
	if codex {
		if c["inputTokens"] != nil && c["outputTokens"] != nil && fleet.Number(unknown["inputTokens"]) == 0 && fleet.Number(unknown["outputTokens"]) == 0 {
			return "tokens", fleet.Number(c["inputTokens"]) + fleet.Number(c["outputTokens"])
		}
		return "tokens", nil
	}
	return "", nil
}
func tierRatePeriod(rows []any, p Period) map[string]any {
	mine := []map[string]any{}
	judged := map[string][]float64{"cost": {}, "tokens": {}}
	for _, raw := range rows {
		r := fleet.Object(raw)
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["createdAt"]))
		if e != nil || at.Before(p.From) || !at.Before(p.To) {
			continue
		}
		mine = append(mine, r)
		basis, value := tierJudged(r)
		if n, ok := value.(float64); ok {
			judged[basis] = append(judged[basis], n)
		}
	}
	medians := map[string]any{"cost": percentile(judged["cost"], .5), "tokens": percentile(judged["tokens"], .5)}
	out := p.Dates()
	out["medians"], out["total"], out["levels"], out["consultModels"], out["implementations"] = medians, tierRateTally(mine, medians), tierRateGroups(mine, "level", medians), tierRateGroups(mine, "model", medians), tierRateGroups(mine, "implementation", medians)
	return out
}

// tierRateTally counts the passes and the passes no costlier than the period median.
func tierRateTally(list []map[string]any, medians map[string]any) map[string]any {
	passed, outcomeUnknown, known, cheap, unknown := 0, 0, 0, 0, 0
	for _, r := range list {
		if r["outcome"] == "passed" {
			passed++
		}
		if r["outcome"] == nil {
			outcomeUnknown++
		}
		basis, value := tierJudged(r)
		if value == nil {
			unknown++
			continue
		}
		known++
		if r["outcome"] == "passed" && value.(float64) <= medians[basis].(float64) {
			cheap++
		}
	}
	var passRate, cheapRate any
	if len(list) > 0 {
		passRate = float64(passed) / float64(len(list))
	}
	if known > 0 {
		cheapRate = float64(cheap) / float64(known)
	}
	return map[string]any{"count": len(list), "passed": passed, "outcomeUnknown": outcomeUnknown, "judged": known, "cheapPassed": cheap, "unknown": unknown, "passedRate": passRate, "cheapRate": cheapRate}
}

// tierRateGroups tallies the rows by starting level, consultation model or implementation provider and model.
func tierRateGroups(mine []map[string]any, axis string, medians map[string]any) []any {
	groups := map[string][]map[string]any{}
	projection := map[string]map[string]any{}
	for _, r := range mine {
		key := ""
		fields := map[string]any{}
		switch axis {
		case "level":
			fields["level"] = fleet.Object(r["start"])["level"]
			key = textKey(fields["level"])
		case "model":
			fields["model"] = r["model"]
			key = textKey(fields["model"])
		default:
			impl := fleet.Object(fleet.Object(r["issueUsage"])["implementation"])
			fields["provider"], fields["model"] = impl["provider"], impl["model"]
			key = textKey(fields["provider"]) + "\x00" + textKey(fields["model"])
		}
		groups[key] = append(groups[key], r)
		projection[key] = fields
	}
	keys := []string{}
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if axis == "level" {
			rank := map[string]int{"light": 0, "standard": 1, "heavy": 2, "fixed": 3}
			ai, bi := fleet.Text(projection[keys[i]]["level"]), fleet.Text(projection[keys[j]]["level"])
			a, ok := rank[ai]
			if !ok {
				a = 50
				if ai == "" {
					a = 99
				}
			}
			b, ok := rank[bi]
			if !ok {
				b = 50
				if bi == "" {
					b = 99
				}
			}
			if a != b {
				return a < b
			}
		}
		return keys[i] < keys[j]
	})
	out := []any{}
	for _, key := range keys {
		r := tierRateTally(groups[key], medians)
		for k, v := range projection[key] {
			r[k] = v
		}
		out = append(out, r)
	}
	return out
}
func textKey(v any) string {
	s := fleet.Text(v)
	if strings.TrimSpace(s) == "" {
		return "\uffff"
	}
	return s
}
func tierImplementation(settings any) any {
	c := fleet.Object(settings)
	tier := fleet.Object(c["tier"])
	if strings.TrimSpace(fleet.Text(tier["level"])) == "" {
		return nil
	}
	out := map[string]any{"level": tier["level"], "source": tier["source"], "reason": tier["reason"], "model": c["model"], "effort": c["effort"], "history": []any{}}
	history := []any{}
	for _, raw := range fleet.List(tier["history"]) {
		r := fleet.Object(raw)
		if r != nil {
			history = append(history, map[string]any{"from": r["from"], "to": r["to"], "reason": r["reason"], "at": r["at"]})
		}
	}
	out["history"] = history
	return out
}
func tierDecision(value any) any {
	tier := fleet.Object(value)
	if fleet.Text(tier["level"]) == "" {
		return nil
	}
	return map[string]any{"level": tier["level"], "source": tier["source"], "reason": tier["reason"], "decidedAt": tier["decidedAt"]}
}
