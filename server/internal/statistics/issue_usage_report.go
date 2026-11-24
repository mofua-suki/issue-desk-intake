package statistics

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
)

func issueUsageSummary(issues []AccountRecord) AccountRecord {
	metrics, durations, rework := AccountRecord{}, AccountRecord{}, AccountRecord{}
	collect := func(key string, read func(AccountRecord) any) AccountRecord {
		values := []any{}
		for _, issue := range issues {
			values = append(values, read(issue))
		}
		return AccountDistribution(values)
	}
	for _, key := range accountMetrics {
		metrics[key] = collect(key, func(i AccountRecord) any {
			total := fleet.Object(i["total"])
			if fleet.Number(fleet.Object(total["unknown"])[key]) > 0 || key != "elapsedMs" && fleet.Number(total["partialRows"]) > 0 {
				return nil
			}
			return total[key]
		})
	}
	for _, key := range []string{"filedToStartMs", "startToPrMs", "prToMergeMs", "filedToMergeMs"} {
		durations[key] = collect(key, func(i AccountRecord) any { return fleet.Object(i["durations"])[key] })
	}
	for _, key := range []string{"retries", "reviewReturns", "reviewHolds", "implementationRetries", "nonImplementationRetries", "unknownRetries"} {
		rework[key] = collect(key, func(i AccountRecord) any {
			if fleet.Number(i["unknownGenerations"]) > 0 {
				return nil
			}
			return float64(fleet.Number(i[key]))
		})
	}
	count, unknown, raised := 0, 0, 0
	stepNames := []string{}
	seen := map[string]bool{}
	for _, i := range issues {
		if i["tierRaised"] == true {
			raised++
		}
		if fleet.Number(i["unknownGenerations"]) > 0 {
			unknown++
		} else if fleet.Number(i["implementationRetries"])+fleet.Number(i["reviewReturns"])+fleet.Number(i["reviewHolds"]) > 0 {
			count++
		}
		for _, v := range fleet.List(i["steps"]) {
			name := fleet.Text(fleet.Object(v)["step"])
			if !seen[name] {
				seen[name] = true
				stepNames = append(stepNames, name)
			}
		}
	}
	var rate any
	if len(issues) > unknown {
		rate = float64(count) / float64(len(issues)-unknown)
	}
	steps := []any{}
	for _, step := range stepNames {
		m := AccountRecord{}
		for _, key := range accountMetrics {
			m[key] = collect(key, func(i AccountRecord) any {
				for _, v := range fleet.List(i["steps"]) {
					s := fleet.Object(v)
					if s["step"] == step {
						if fleet.Number(fleet.Object(s["unknown"])[key]) > 0 || key != "elapsedMs" && fleet.Number(s["partialRows"]) > 0 {
							return nil
						}
						return s[key]
					}
				}
				if fleet.Number(fleet.Object(i["total"])["count"]) > 0 {
					return float64(0)
				}
				return nil
			})
		}
		steps = append(steps, AccountRecord{"step": step, "metrics": m})
	}
	return AccountRecord{"count": len(issues), "metrics": metrics, "durations": durations, "rework": rework, "reworkRate": AccountRecord{"count": count, "denominator": len(issues) - unknown, "unknown": unknown, "rate": rate}, "steps": steps, "tierRaised": raised}
}

// BuildIssueUsageReport preserves the NAS report's per-merged-issue population,
// unknown denominators and retry subtotals. Consultation attribution is supplied
// by its consistent read-only database reader.
func BuildIssueUsageReport(data ReworkRows, weights, period, attribution AccountRecord) AccountRecord {
	issues := []AccountRecord{}
	for _, job := range data.Jobs {
		events, rows := []AccountRecord{}, []AccountRecord{}
		for _, event := range data.Events {
			if event["job_id"] == job["id"] {
				events = append(events, event)
			}
		}
		for _, row := range data.Usage {
			if accountKey(row) == accountKey(job) {
				rows = append(rows, row)
			}
		}
		issues = append(issues, AccountIssue(job, events, rows, weights))
	}
	axis := func(fn func(AccountRecord) string) []any {
		keys, groups := accountGroups(issues, fn)
		out := []any{}
		for _, key := range keys {
			v := issueUsageSummary(groups[key])
			v["key"] = key
			out = append(out, v)
		}
		return out
	}
	size := func(i AccountRecord) string {
		s := fleet.Object(i["size"])
		return fmt.Sprintf("行:%s/ファイル:%s", s["lineBand"], s["fileBand"])
	}
	values := []any{}
	for _, i := range issues {
		values = append(values, i)
	}
	return AccountRecord{"period": period, "weights": breakdownWeights(weights), "skipped": data.Skipped, "attribution": attribution, "total": issueUsageSummary(issues), "issues": values, "repositories": axis(func(i AccountRecord) string { return fleet.Text(i["repo"]) }), "tiers": axis(func(i AccountRecord) string { return fleet.Text(i["tier"]) }), "sizes": axis(size), "combinations": axis(func(i AccountRecord) string {
		return fleet.Text(i["repo"]) + "/" + fleet.Text(i["tier"]) + "/" + size(i)
	})}
}
