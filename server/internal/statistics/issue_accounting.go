package statistics

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"sort"
	"time"
)

type AccountRecord = map[string]any

var accountMetrics = []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "uncachedInputTokens", "normalizedCacheWriteTokens", "weighted", "elapsedMs"}

func accountKnown(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0
}
func accountAt(v any) time.Time {
	if at, ok := v.(time.Time); ok {
		return at
	}
	at, _ := time.Parse(time.RFC3339Nano, fleet.Text(v))
	return at
}
func accountWhole(v any) bool {
	n, ok := accountKnown(v)
	return ok && n == math.Trunc(n) && n <= 9007199254740991
}
func accountKey(r AccountRecord) string {
	return fmt.Sprintf("%s#%.0f", fleet.Text(r["repo"]), fleet.Number(r["issue"]))
}
func accountGroups(records []AccountRecord, key func(AccountRecord) string) ([]string, map[string][]AccountRecord) {
	keys := []string{}
	groups := map[string][]AccountRecord{}
	for _, r := range records {
		k := key(r)
		if _, has := groups[k]; !has {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	return keys, groups
}
func accountTotals(rows []AccountRecord, weights AccountRecord) AccountRecord {
	unknown := AccountRecord{}
	result := AccountRecord{"count": len(rows), "unknown": unknown, "partialRows": 0}
	for _, key := range accountMetrics {
		result[key] = nil
		unknown[key] = 0
	}
	for _, row := range rows {
		normalized := usage.Weighted(row, weights)
		for _, key := range accountMetrics {
			v := row[key]
			if n, has := normalized[key]; has {
				v = n
			}
			if n, ok := accountKnown(v); ok {
				result[key] = fleet.Number(result[key]) + n
			} else {
				unknown[key] = unknown[key].(int) + 1
			}
		}
		if row["usagePartial"] == true {
			result["partialRows"] = result["partialRows"].(int) + 1
		}
	}
	return result
}

// FiledTier uses only the first recorded tier, never the current jobs.tier.
func FiledTier(job AccountRecord, rows []AccountRecord) (string, bool) {
	levels := map[string]int{"light": 0, "standard": 1, "heavy": 2}
	history, has := fleet.Object(job["view"])["tierHistory"].([]any)
	initial := ""
	raised := false
	if has && len(history) > 0 {
		initial = fleet.Text(fleet.Object(history[0])["from"])
	} else {
		first := []AccountRecord{}
		for _, row := range rows {
			if fleet.Number(row["generation"]) == 1 && row["step"] == "実装" {
				first = append(first, row)
			}
		}
		sort.SliceStable(first, func(i, j int) bool { return accountAt(first[i]["at"]).Before(accountAt(first[j]["at"])) })
		if len(first) > 0 {
			initial = fleet.Text(first[0]["tier"])
		}
	}
	for _, raw := range history {
		h := fleet.Object(raw)
		from, ok := levels[fleet.Text(h["from"])]
		to, valid := levels[fleet.Text(h["to"])]
		raised = raised || ok && valid && to > from
	}
	if _, ok := levels[initial]; !ok {
		initial = "unknown"
	}
	return initial, raised
}

// IssueRework counts manual controls and recorded generation triggers once.
func IssueRework(events, rows []AccountRecord) AccountRecord {
	controls, holds := reworkControls(events)
	valid := []AccountRecord{}
	unknown := 0
	for _, row := range rows {
		if accountWhole(row["generation"]) && fleet.Number(row["generation"]) > 0 {
			valid = append(valid, row)
		}
		if includes([]string{"実装", "競合解消", "独立レビュー", "固定テスト"}, fleet.Text(row["step"])) && (!accountWhole(row["generation"]) || fleet.Number(row["generation"]) < 1) {
			unknown = 1
		}
	}
	keys, groups := accountGroups(valid, func(r AccountRecord) string { return fmt.Sprint(fleet.Number(r["generation"])) })
	if len(keys) == 0 {
		unknown = 1
	}
	sort.Slice(keys, func(i, j int) bool {
		return fleet.Number(groups[keys[i]][0]["generation"]) < fleet.Number(groups[keys[j]][0]["generation"])
	})
	used := map[int]bool{}
	previous := time.Time{}
	implementation, retest, unknownRetries, returns := 0, 0, 0, 0
	for _, key := range keys {
		group := groups[key]
		first, last, trigger := generationSpan(group)
		pick := func(retestOnly bool) int {
			for index, control := range controls {
				at := accountAt(control["at"])
				d := fleet.Object(control["data"])
				if !used[index] && !at.IsZero() && (previous.IsZero() || at.After(previous)) && !first.IsZero() && !at.After(first) && (!retestOnly || d["retest"] == true || d["autoRetest"] == true) {
					return index
				}
			}
			return -1
		}
		if fleet.Number(group[0]["generation"]) > 1 {
			if trigger == "" {
				if index := pick(true); index >= 0 {
					used[index] = true
					retest++
				} else {
					unknown++
				}
			}
			if trigger == "review-return" {
				returns++
			}
			if trigger == "retest" {
				if index := pick(false); index >= 0 {
					used[index] = true
					impl := false
					for _, row := range group {
						impl = impl || row["step"] == "実装" || row["step"] == "競合解消"
					}
					if impl {
						implementation++
					} else {
						retest++
					}
				}
			}
			if trigger == "retry" {
				if index := pick(false); index >= 0 {
					used[index] = true
					d := fleet.Object(controls[index]["data"])
					if d["retest"] == true || d["autoRetest"] == true {
						retest++
					} else {
						implementation++
					}
				} else {
					implementation++
				}
			}
		}
		if !last.IsZero() {
			previous = last
		}
	}
	for index, control := range controls {
		if used[index] {
			continue
		}
		d := fleet.Object(control["data"])
		if d["retest"] == true || d["autoRetest"] == true {
			retest++
		} else if d["fromBase"] == true || d["instruction"] == true {
			implementation++
		} else {
			unknownRetries++
		}
	}
	return AccountRecord{"retries": implementation, "implementationRetries": implementation, "nonImplementationRetries": retest, "unknownRetries": unknownRetries, "retryReasons": AccountRecord{"implementation": implementation, "retest": retest, "unknown": unknownRetries}, "reviewReturns": returns, "reviewHolds": holds, "unknownGenerations": unknown}
}

// reworkControls picks the manual retry controls and counts the review holds.
func reworkControls(events []AccountRecord) ([]AccountRecord, int) {
	controls := []AccountRecord{}
	holds := 0
	for _, event := range events {
		data := fleet.Object(event["data"])
		if event["kind"] == "control" && data["action"] == "retry" {
			controls = append(controls, event)
		}
		if event["kind"] == "state" && data["to"] == "held" && data["holdReason"] == "review_held" {
			holds++
		}
	}
	return controls, holds
}

// generationSpan finds when a generation's rows began and ended, and its first recorded trigger.
func generationSpan(group []AccountRecord) (first, last time.Time, trigger string) {
	for _, row := range group {
		at := accountAt(row["at"])
		if !at.IsZero() {
			if first.IsZero() || at.Before(first) {
				first = at
			}
			if last.IsZero() || at.After(last) {
				last = at
			}
		}
		if trigger == "" && fleet.Text(row["generationTrigger"]) != "" && row["generationTrigger"] != "unknown" {
			trigger = fleet.Text(row["generationTrigger"])
		}
	}
	return first, last, trigger
}

func accountISO(value any) any {
	at := accountAt(value)
	if at.IsZero() {
		return nil
	}
	return ISO(at)
}
func accountDuration(a, b any) any {
	from, to := accountAt(a), accountAt(b)
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil
	}
	return float64(to.Sub(from).Milliseconds())
}

// AccountIssue exposes the shared per-issue accounting used by read-only reports.
func AccountIssue(job AccountRecord, events, rows []AccountRecord, weights AccountRecord) AccountRecord {
	tier, raised := FiledTier(job, rows)
	result := IssueRework(events, rows)
	started, pr := issueMilestones(events)
	size := reviewedSize(rows)
	tiers, retryRows := rowTiersAndRetries(rows)
	for key, value := range (AccountRecord{"key": accountKey(job), "repo": job["repo"], "issue": job["issue"], "pr": job["pr"], "tier": tier, "tierRaised": raised, "tiers": tiers, "size": size, "filedAt": accountISO(job["filed_at"]), "startedAt": started, "prAt": pr, "mergedAt": accountISO(job["merged_at"]), "total": accountTotals(rows, weights), "steps": accountSteps(rows, weights), "retryUsage": accountTotals(retryRows, weights)}) {
		result[key] = value
	}
	estimate := fleet.Object(fleet.Object(job["view"])["estimate"])
	if accountWhole(estimate["lines"]) && accountWhole(estimate["files"]) {
		result["estimate"] = AccountRecord{"lines": estimate["lines"], "files": estimate["files"]}
	}
	result["durations"] = AccountRecord{"filedToStartMs": accountDuration(result["filedAt"], started), "startToPrMs": accountDuration(started, pr), "prToMergeMs": accountDuration(pr, result["mergedAt"]), "filedToMergeMs": accountDuration(result["filedAt"], result["mergedAt"])}
	return result
}

// issueMilestones finds when the issue was first leased and its PR created.
func issueMilestones(events []AccountRecord) (started, pr any) {
	for _, event := range events {
		if started == nil && event["kind"] == "leased" {
			started = accountISO(event["at"])
		}
		if pr == nil && event["kind"] == "pr_created" {
			pr = accountISO(event["at"])
		}
	}
	return started, pr
}

// reviewedSize is the diff size the last independent review recorded, with its bands.
func reviewedSize(rows []AccountRecord) AccountRecord {
	var lines, files any
	for _, row := range rows {
		if row["step"] == "独立レビュー" {
			lines, files = nil, nil
			if n, ok := accountKnown(row["reviewDiffLines"]); ok {
				lines = n
			}
			if n, ok := accountKnown(row["reviewDiffFiles"]); ok {
				files = n
			}
		}
	}
	lineBand, fileBand := "不明", "不明"
	if lines != nil {
		lineBand = "501以上"
		if fleet.Number(lines) <= 100 {
			lineBand = "0〜100"
		} else if fleet.Number(lines) <= 500 {
			lineBand = "101〜500"
		}
	}
	if files != nil {
		fileBand = "16以上"
		if fleet.Number(files) <= 5 {
			fileBand = "0〜5"
		} else if fleet.Number(files) <= 15 {
			fileBand = "6〜15"
		}
	}
	return AccountRecord{"lines": lines, "files": files, "lineBand": lineBand, "fileBand": fileBand}
}

// rowTiersAndRetries lists the tiers the rows ran at and the rows of retried generations or attempts.
func rowTiersAndRetries(rows []AccountRecord) ([]any, []AccountRecord) {
	tiers := []any{}
	seen := map[string]bool{}
	retryGens := map[float64]bool{}
	for _, row := range rows {
		t := fleet.Text(row["tier"])
		if t != "" && !seen[t] {
			seen[t] = true
			tiers = append(tiers, t)
		}
		if row["generationTrigger"] == "retry" && accountWhole(row["generation"]) && fleet.Number(row["generation"]) > 1 {
			retryGens[fleet.Number(row["generation"])] = true
		}
	}
	retryRows := []AccountRecord{}
	for _, row := range rows {
		if retryGens[fleet.Number(row["generation"])] || fleet.Number(row["attempt"]) > 1 {
			retryRows = append(retryRows, row)
		}
	}
	return tiers, retryRows
}

// accountSteps totals the rows by step.
func accountSteps(rows []AccountRecord, weights AccountRecord) []any {
	steps := []any{}
	keys, groups := accountGroups(rows, func(r AccountRecord) string {
		step := fleet.Text(r["step"])
		if step == "" {
			step = "不明"
		}
		return step
	})
	for _, step := range keys {
		value := accountTotals(groups[step], weights)
		value["step"] = step
		steps = append(steps, value)
	}
	return steps
}
func includes(items []string, target string) bool {
	for _, v := range items {
		if v == target {
			return true
		}
	}
	return false
}
func AccountDistribution(values []any) AccountRecord {
	numbers := []float64{}
	for _, value := range values {
		if n, ok := accountKnown(value); ok {
			numbers = append(numbers, n)
		}
	}
	sort.Float64s(numbers)
	out := AccountRecord{"known": len(numbers), "unknown": len(values) - len(numbers), "sum": nil, "mean": nil, "median": nil}
	if len(numbers) > 0 {
		sum := 0.0
		for _, n := range numbers {
			sum += n
		}
		out["sum"], out["mean"], out["median"] = sum, sum/float64(len(numbers)), (numbers[(len(numbers)-1)/2]+numbers[len(numbers)/2])/2
	}
	return out
}
