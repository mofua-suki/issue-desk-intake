package statistics

import (
	"issue-desk-intake/server/internal/fleet"
	"sort"
)

func followupProvider(rows []AccountRecord) (string, bool) {
	impl := []AccountRecord{}
	for _, r := range rows {
		if r["step"] == "実装" && includes([]string{"antigravity", "claude", "codex"}, fleet.Text(r["provider"])) {
			impl = append(impl, r)
		}
	}
	sort.SliceStable(impl, func(i, j int) bool {
		a, b := fleet.Number(impl[i]["generation"]), fleet.Number(impl[j]["generation"])
		if a != b {
			return a < b
		}
		return accountAt(impl[i]["at"]).Before(accountAt(impl[j]["at"]))
	})
	first := ""
	antigravity, switched := false, false
	for _, r := range impl {
		p := fleet.Text(r["provider"])
		if first == "" {
			first = p
		}
		if p == "antigravity" {
			antigravity = true
		}
		if antigravity && p == "claude" {
			switched = true
		}
	}
	return first, switched
}
func fixedFailures(job AccountRecord) any {
	for _, raw := range []any{job["view"], job["result"]} {
		runs, ok := fleet.Object(raw)["fixedTestRuns"].([]any)
		if !ok {
			continue
		}
		sum := 0.0
		valid := true
		for _, r := range runs {
			value := fleet.Object(r)["failures"]
			if !accountWhole(value) {
				valid = false
				break
			}
			sum += fleet.Number(value)
		}
		if valid {
			return sum
		}
	}
	return nil
}
func breakdownFollowups(jobs, issues []AccountRecord, rowGroups map[string][]AccountRecord, weights AccountRecord) AccountRecord {
	issueMap := map[string]AccountRecord{}
	for _, i := range issues {
		issueMap[fleet.Text(i["key"])] = i
	}
	type item struct {
		issue    AccountRecord
		provider string
		switched bool
		oneShot  any
		usage    AccountRecord
		failures any
	}
	items := []item{}
	other := 0
	for _, job := range jobs {
		if job["kind"] != "followup" {
			continue
		}
		key := accountKey(job)
		issue := issueMap[key]
		rows := rowGroups[key]
		provider, switched := followupProvider(rows)
		var oneShot any
		if fleet.Number(issue["unknownGenerations"]) == 0 {
			oneShot = fleet.Number(issue["retries"])+fleet.Number(issue["reviewReturns"])+fleet.Number(issue["reviewHolds"]) == 0
		}
		if provider == "" {
			other++
		}
		items = append(items, item{issue, provider, switched, oneShot, breakdownUsage(rows, weights), fixedFailures(job)})
	}
	groups := []AccountRecord{}
	for _, provider := range []string{"antigravity", "claude", "codex"} {
		mine := []item{}
		known, passed, switched, failKnown := 0, 0, 0, 0
		failureTotal := 0.0
		keys := []any{}
		for _, i := range items {
			if i.provider != provider {
				continue
			}
			mine = append(mine, i)
			if i.oneShot != nil {
				known++
				if i.oneShot == true {
					passed++
				}
			}
			if i.switched {
				switched++
			}
			if n, ok := accountKnown(i.failures); ok {
				failKnown++
				failureTotal += n
			}
			keys = append(keys, i.issue["key"])
		}
		per := AccountRecord{}
		for _, metric := range breakdownMetrics {
			values := []any{}
			for _, i := range mine {
				value := i.usage[metric]
				if fleet.Number(fleet.Object(i.usage["unknown"])[metric]) > 0 {
					value = nil
				}
				values = append(values, value)
			}
			per[metric] = AccountDistribution(values)
		}
		var rate, failures, reason any
		if known > 0 {
			rate = float64(passed) / float64(known)
		}
		if failKnown > 0 {
			failures = failureTotal
		}
		if failKnown != len(mine) || len(mine) == 0 {
			reason = "共有DBの jobs.view・result に固定テストの記録（fixedTestRuns）がありません（監視の state.sqlite の job.fixedTestRuns にだけあります）。"
		}
		groups = append(groups, AccountRecord{"provider": provider, "count": len(mine), "oneShot": AccountRecord{"passed": passed, "known": known, "unknown": len(mine) - known, "rate": rate}, "switchedToClaude": switched, "perIssue": per, "fixedTestFailures": AccountRecord{"known": failKnown, "unknown": len(mine) - failKnown, "total": failures, "reason": reason}, "issues": keys})
	}
	return AccountRecord{"groups": groups, "excluded": AccountRecord{"other": other}}
}
