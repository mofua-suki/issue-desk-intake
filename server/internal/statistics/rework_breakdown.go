package statistics

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
)

var breakdownMetrics = []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "costUsd", "weighted"}
var findingKinds = []string{"data", "security", "regression", "acceptance", "blocking", "other"}

func breakdownUsage(rows []AccountRecord, weights AccountRecord) AccountRecord {
	out, unknown := AccountRecord{"rows": len(rows)}, AccountRecord{}
	for _, key := range breakdownMetrics {
		out[key] = nil
		unknown[key] = 0
	}
	for _, row := range rows {
		for _, key := range breakdownMetrics {
			value := row[key]
			if key == "weighted" {
				value = usage.Weighted(row, weights)["weighted"]
			}
			if n, ok := accountKnown(value); ok {
				out[key] = fleet.Number(out[key]) + n
			} else {
				unknown[key] = unknown[key].(int) + 1
			}
		}
	}
	out["unknown"] = unknown
	return out
}
func breakdownWeights(value AccountRecord) AccountRecord {
	defaults := AccountRecord{"input": float64(1), "cacheRead": 0.1, "cacheWrite": 1.25, "output": float64(5)}
	for key, v := range fleet.Object(value["default"]) {
		defaults[key] = v
	}
	providers := fleet.Object(value["providers"])
	if providers == nil {
		providers = AccountRecord{}
	}
	return AccountRecord{"default": defaults, "providers": providers}
}
func breakdownSubtotal(issues, rows []AccountRecord, weights AccountRecord) AccountRecord {
	out := AccountRecord{"issues": len(issues), "retries": 0, "implementationRetries": 0, "nonImplementationRetries": 0, "unknownRetries": 0, "reviewReturns": 0, "reviewHolds": 0, "unknownIssues": 0, "reworkedIssues": 0, "tierRaised": 0}
	reasons := AccountRecord{"implementation": 0, "retest": 0, "unknown": 0}
	judged, usageIssues := 0, 0
	rowGroups := map[string]bool{}
	for _, r := range rows {
		rowGroups[accountKey(r)] = true
	}
	for _, issue := range issues {
		if issue["tierRaised"] == true {
			out["tierRaised"] = out["tierRaised"].(int) + 1
		}
		if rowGroups[fleet.Text(issue["key"])] {
			usageIssues++
		}
		if fleet.Number(issue["unknownGenerations"]) > 0 {
			out["unknownIssues"] = out["unknownIssues"].(int) + 1
			continue
		}
		judged++
		for _, key := range []string{"retries", "implementationRetries", "nonImplementationRetries", "unknownRetries", "reviewReturns", "reviewHolds"} {
			out[key] = out[key].(int) + int(fleet.Number(issue[key]))
		}
		for _, key := range []string{"implementation", "retest", "unknown"} {
			reasons[key] = reasons[key].(int) + int(fleet.Number(fleet.Object(issue["retryReasons"])[key]))
		}
		if fleet.Number(issue["implementationRetries"])+fleet.Number(issue["reviewReturns"])+fleet.Number(issue["reviewHolds"]) > 0 {
			out["reworkedIssues"] = out["reworkedIssues"].(int) + 1
		}
	}
	out["retryReasons"] = reasons
	out["reworkRate"] = nil
	if judged > 0 {
		out["reworkRate"] = float64(out["reworkedIssues"].(int)) / float64(judged)
	}
	totals := breakdownUsage(rows, weights)
	out["usage"] = totals
	out["perIssue"] = nil
	if usageIssues > 0 {
		per := AccountRecord{"unknown": totals["unknown"]}
		for _, key := range breakdownMetrics {
			per[key] = nil
			if n, ok := accountKnown(totals[key]); ok {
				per[key] = n / float64(usageIssues)
			}
		}
		out["perIssue"] = per
	}
	return out
}
func breakdownGenerations(rows []AccountRecord, weights AccountRecord) AccountRecord {
	valid := []AccountRecord{}
	for _, r := range rows {
		if accountWhole(r["generation"]) && fleet.Number(r["generation"]) > 1 {
			valid = append(valid, r)
		}
	}
	keys, groups := accountGroups(valid, func(r AccountRecord) string {
		return fmt.Sprintf("%s#%.0f", accountKey(r), fleet.Number(r["generation"]))
	})
	triggers, kinds := []AccountRecord{}, []AccountRecord{}
	triggerGroups, kindGroups := map[string][]AccountRecord{}, map[string][]AccountRecord{}
	triggerGens, kindGens := map[string]int{}, map[string]int{}
	triggerOrder, kindOrder := []string{}, []string{}
	for _, key := range keys {
		group := groups[key]
		trigger := generationTrigger(group)
		if _, has := triggerGroups[trigger]; !has {
			triggerOrder = append(triggerOrder, trigger)
		}
		triggerGroups[trigger] = append(triggerGroups[trigger], group...)
		triggerGens[trigger]++
		if trigger == "review-return" {
			order := generationFindingKinds(group)
			for _, v := range order {
				if _, has := kindGroups[v]; !has {
					kindOrder = append(kindOrder, v)
				}
				kindGroups[v] = append(kindGroups[v], group...)
				kindGens[v]++
			}
		}
	}
	add := func(key string, group []AccountRecord, count int) AccountRecord {
		issues := map[string]bool{}
		for _, r := range group {
			issues[accountKey(r)] = true
		}
		return AccountRecord{"key": key, "generations": count, "issues": len(issues), "usage": breakdownUsage(group, weights)}
	}
	for _, key := range triggerOrder {
		triggers = append(triggers, add(key, triggerGroups[key], triggerGens[key]))
	}
	for _, key := range kindOrder {
		kinds = append(kinds, add(key, kindGroups[key], kindGens[key]))
	}
	return AccountRecord{"triggers": triggers, "findingKinds": kinds, "findingSeverities": breakdownSeverities(rows)}
}

// generationTrigger is the first known trigger recorded in the generation's rows.
func generationTrigger(group []AccountRecord) string {
	trigger := "unknown"
	for _, r := range group {
		v := fleet.Text(r["generationTrigger"])
		if includes([]string{"review-return", "hold-instruction", "retest", "retry", "answer"}, v) {
			trigger = v
			break
		}
	}
	return trigger
}

// generationFindingKinds lists the finding kinds of a returned generation in
// order; unknown is kept only when no kind was recorded.
func generationFindingKinds(group []AccountRecord) []string {
	seen := map[string]bool{}
	order := []string{}
	for _, r := range group {
		values := fleet.List(r["reviewFindingKinds"])
		if len(values) == 0 {
			values = []any{}
			if !seen["unknown"] {
				order = append(order, "unknown")
				seen["unknown"] = true
			}
		}
		for _, raw := range values {
			v := fleet.Text(raw)
			if !includes(findingKinds, v) {
				v = "other"
			}
			if !seen[v] {
				order = append(order, v)
				seen[v] = true
			}
		}
	}
	if len(order) > 1 {
		clean := []string{}
		for _, v := range order {
			if v != "unknown" {
				clean = append(clean, v)
			}
		}
		order = clean
	}
	return order
}
func breakdownSeverities(rows []AccountRecord) AccountRecord {
	reviews, totalReviews, must, minor := 0, 0, 0, 0
	counts := map[string]AccountRecord{}
	for _, row := range rows {
		if row["step"] != "独立レビュー" {
			continue
		}
		totalReviews++
		pairs, has := row["reviewFindingSeverities"].([]any)
		if !has {
			continue
		}
		valid := []AccountRecord{}
		for _, raw := range pairs {
			p := fleet.Object(raw)
			if includes(findingKinds, fleet.Text(p["kind"])) && (p["severity"] == "must" || p["severity"] == "minor") {
				valid = append(valid, p)
			}
		}
		if len(pairs) > 0 && len(valid) == 0 {
			continue
		}
		reviews++
		for _, p := range valid {
			key := fleet.Text(p["kind"])
			if counts[key] == nil {
				counts[key] = AccountRecord{"kind": key, "must": 0, "minor": 0}
			}
			severity := fleet.Text(p["severity"])
			counts[key][severity] = counts[key][severity].(int) + 1
			if severity == "must" {
				must++
			} else {
				minor++
			}
		}
	}
	out := AccountRecord{"reviews": reviews, "unknown": totalReviews - reviews, "must": nil, "minor": nil}
	if reviews > 0 {
		out["must"], out["minor"] = must, minor
	}
	kinds := []AccountRecord{}
	for _, key := range findingKinds {
		if counts[key] != nil {
			kinds = append(kinds, counts[key])
		}
	}
	out["kinds"] = kinds
	return out
}

// BuildReworkBreakdown is pure: callers provide a consistent DB snapshot.
func BuildReworkBreakdown(jobs, events, rows []AccountRecord, weights, period AccountRecord, skipped int) AccountRecord {
	weights = breakdownWeights(weights)
	rows, _ = usage.CorrectCosts(rows, nil)
	_, eventGroups := accountGroups(events, func(r AccountRecord) string { return fmt.Sprint(fleet.Number(r["job_id"])) })
	_, rowGroups := accountGroups(rows, accountKey)
	issues := []AccountRecord{}
	for _, job := range jobs {
		issues = append(issues, AccountIssue(job, eventGroups[fmt.Sprint(fleet.Number(job["id"]))], rowGroups[accountKey(job)], weights))
	}
	total := breakdownSubtotal(issues, rows, weights)
	judged := len(issues) - total["unknownIssues"].(int)
	out := AccountRecord{"period": period, "weights": weights, "skipped": skipped, "merged": len(issues), "tierRaised": total["tierRaised"], "reworkRate": AccountRecord{"count": total["reworkedIssues"], "denominator": judged, "unknown": total["unknownIssues"], "rate": total["reworkRate"]}}
	projected := []AccountRecord{}
	for _, issue := range issues {
		p := AccountRecord{}
		for _, key := range []string{"key", "tier", "unknownGenerations", "tierRaised"} {
			p[key] = issue[key]
		}
		for _, key := range []string{"retries", "implementationRetries", "nonImplementationRetries", "unknownRetries", "retryReasons", "reviewReturns", "reviewHolds"} {
			p[key] = issue[key]
			if fleet.Number(issue["unknownGenerations"]) > 0 {
				p[key] = nil
			}
		}
		projected = append(projected, p)
	}
	out["issues"] = projected
	for key, value := range breakdownGenerations(rows, weights) {
		out[key] = value
	}
	axis := func(key func(AccountRecord) string, generations bool) []AccountRecord {
		keys, groups := accountGroups(issues, key)
		result := []AccountRecord{}
		for _, k := range keys {
			group := groups[k]
			used := []AccountRecord{}
			for _, issue := range group {
				used = append(used, rowGroups[fleet.Text(issue["key"])]...)
			}
			r := breakdownSubtotal(group, used, weights)
			r["key"] = k
			if generations {
				for key, value := range breakdownGenerations(used, weights) {
					r[key] = value
				}
			}
			result = append(result, r)
		}
		return result
	}
	out["tiers"] = axis(func(i AccountRecord) string { return fleet.Text(i["tier"]) }, true)
	out["sizes"] = axis(func(i AccountRecord) string {
		size := fleet.Object(i["size"])
		return "行:" + fleet.Text(size["lineBand"]) + "/ファイル:" + fleet.Text(size["fileBand"])
	}, false)
	out["followups"] = breakdownFollowups(jobs, issues, rowGroups, weights)
	return out
}
