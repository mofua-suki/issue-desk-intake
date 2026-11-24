package anomalies

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"time"
)

const FilingCacheStep = usage.FilingStep

func filingCacheSide(rows []map[string]any, weights map[string]any) map[string]any {
	known, sized := 0, 0
	sums := map[string]float64{}
	sizedBytes, sizedWeight := float64(0), float64(0)
	for _, r := range rows {
		weight, ok := weighted(r, weights)
		if !ok {
			continue
		}
		known++
		sums["weight"] += weight
		parts := usage.Weighted(r, weights)
		sums["uncachedInput"] += fleet.Number(parts["uncachedInputTokens"])
		sums["cacheRead"] += fleet.Number(r["cacheReadTokens"])
		sums["cacheWrite"] += fleet.Number(parts["normalizedCacheWriteTokens"])
		sums["output"] += fleet.Number(r["outputTokens"])
		if integer(r["promptTotalBytes"]) && fleet.Number(r["promptTotalBytes"]) > 0 {
			sized++
			sizedBytes += fleet.Number(r["promptTotalBytes"])
			sizedWeight += weight
		}
	}
	per := map[string]any{}
	for _, key := range []string{"weight", "uncachedInput", "cacheRead", "cacheWrite", "output"} {
		per[key] = nil
		if known > 0 {
			per[key] = sums[key] / float64(known)
		}
	}
	per["promptBytes"] = nil
	if sized > 0 {
		per["promptBytes"] = sizedBytes / float64(sized)
	}
	var perKB any
	if sizedBytes > 0 {
		perKB = sizedWeight / (sizedBytes / 1000)
	}
	return map[string]any{"rows": known, "sizedRows": sized, "perCall": per, "weightPerKB": perKB}
}
func FilingCaches(rows []map[string]any, to time.Time, weights map[string]any) []map[string]any {
	before, after, deployed := filingCacheWindows(rows, to)
	previous, current := filingCacheSide(before, weights), filingCacheSide(after, weights)
	baseline, measured := previous["weightPerKB"], current["weightPerKB"]
	reasons := filingCacheHolds(deployed, to, previous, current, baseline, measured)
	status := "normal"
	if len(reasons) > 0 {
		status = "held"
	} else if measured.(float64) >= baseline.(float64) {
		status = "hit"
	}
	var date, prevWindow, currentWindow, ratio any
	if !deployed.IsZero() {
		date = iso(deployed)
		prevWindow = map[string]any{"from": iso(deployed.Add(-7 * 24 * time.Hour)), "to": date}
		currentWindow = map[string]any{"from": date, "to": iso(to)}
	}
	if baseline != nil && baseline.(float64) != 0 && measured != nil {
		ratio = measured.(float64) / baseline.(float64)
	}
	return []map[string]any{{"kind": "filing-cache", "kindName": names["filing-cache"], "repo": nil, "issue": nil, "step": FilingCacheStep, "provider": "claude", "at": iso(to), "status": status, "reasons": reasons, "deployedAt": date, "measured": measured, "baseline": baseline, "threshold": baseline, "ratio": ratio, "unit": "重み/1,000 bytes", "rule": "前後に大きさの分かる完了行が各30件以上で、反映後の1,000 bytesあたりの重みが直前7日以上", "period": "反映前7日と反映後", "windows": map[string]any{"previous": prevWindow, "current": currentWindow}, "previous": previous, "current": current, "causeKey": "filing-cache:" + FilingCacheStep}}
}

// filingCacheWindows finds when the cache fix was deployed and the completed
// filing checks of the week before and of the time since.
func filingCacheWindows(rows []map[string]any, to time.Time) (before, after []map[string]any, deployed time.Time) {
	own := []map[string]any{}
	for _, r := range rows {
		t := at(r)
		if usage.CanonicalStep(fleet.Text(r["step"])) != FilingCacheStep || r["provider"] != "claude" || t.IsZero() || t.After(to) || t.Before(to.Add(-21*24*time.Hour)) {
			continue
		}
		own = append(own, r)
		if r["promptCache"] == false && (deployed.IsZero() || t.Before(deployed)) {
			deployed = t
		}
	}
	before, after = []map[string]any{}, []map[string]any{}
	if !deployed.IsZero() {
		for _, r := range own {
			if r["outcome"] != "completed" {
				continue
			}
			if r["promptCache"] == false {
				after = append(after, r)
			} else if !at(r).Before(deployed.Add(-7*24*time.Hour)) && at(r).Before(deployed) {
				before = append(before, r)
			}
		}
	}
	return before, after, deployed
}

// filingCacheHolds lists why the comparison cannot be judged.
func filingCacheHolds(deployed, to time.Time, previous, current map[string]any, baseline, measured any) []string {
	reasons := []string{}
	if deployed.IsZero() {
		reasons = append(reasons, "not_deployed")
	} else {
		if deployed.Before(to.Add(-14 * 24 * time.Hour)) {
			reasons = append(reasons, "deployment_out_of_range")
		}
		if previous["sizedRows"].(int) < 30 {
			reasons = append(reasons, "previous_insufficient_rows")
		}
		if current["sizedRows"].(int) < 30 {
			reasons = append(reasons, "current_insufficient_rows")
		}
	}
	if len(reasons) == 0 && (baseline == nil || measured == nil) {
		reasons = append(reasons, "missing_data")
	}
	return reasons
}
func numeric(v any) (float64, bool) {
	switch v.(type) {
	case int, int64, float64:
		return fleet.Number(v), true
	}
	return 0, false
}
func filingSide(value any) map[string]any {
	filing := fleet.Object(fleet.Object(value)["filing"])
	requests, ro := numeric(filing["comparisonRequests"])
	issues, io := numeric(filing["newIssuesInPeriod"])
	var r, i, per any
	if ro {
		r = requests
	}
	if io {
		i = issues
	}
	if ro && io && issues != 0 {
		per = requests / issues
	}
	return map[string]any{"requests": r, "newIssues": i, "requestsPerNewIssue": per}
}
func FilingCheckResult(repo string, read map[string]any, failed bool, to time.Time) map[string]any {
	previous, current := filingSide(read["before"]), filingSide(read["after"])
	reasons, details := []string{}, []string{}
	if failed || read == nil {
		reasons = append(reasons, "read_failed")
	} else {
		for _, side := range []struct{ Name, Key string }{{"previous", "before"}, {"current", "after"}} {
			if read[side.Key] == nil {
				reasons = append(reasons, side.Name+"_unavailable")
				detail := fleet.Text(fleet.Object(read["reasons"])[side.Key])
				if detail == "" {
					unavailable := fleet.List(read["unavailable"])
					if len(unavailable) > 0 {
						detail = fleet.Text(unavailable[0])
					}
				}
				if detail != "" {
					details = append(details, side.Name+": "+detail)
				}
			}
		}
		if read["before"] != nil && fleet.Number(previous["newIssues"]) == 0 {
			reasons = append(reasons, "previous_no_issues")
		}
		if read["after"] != nil && fleet.Number(current["newIssues"]) == 0 {
			reasons = append(reasons, "current_no_issues")
		}
	}
	baseline, measured := previous["requestsPerNewIssue"], current["requestsPerNewIssue"]
	var threshold, ratio any
	if baseline != nil {
		threshold = math.Max(1, baseline.(float64)*2)
		if baseline.(float64) != 0 && measured != nil {
			ratio = measured.(float64) / baseline.(float64)
		}
	}
	status := "normal"
	if len(reasons) > 0 {
		status = "held"
	} else if measured != nil && threshold != nil && measured.(float64) >= threshold.(float64) {
		status = "hit"
	}
	return map[string]any{"kind": "filing-check", "kindName": names["filing-check"], "repo": repo, "issue": nil, "step": nil, "at": iso(to), "status": status, "reasons": reasons, "details": details, "measured": measured, "baseline": baseline, "threshold": threshold, "ratio": ratio, "unit": "回/起票", "rule": "直近7日の起票あたり確認回数が max(1.0, 直前7日の2倍) 以上", "period": "直近7日と直前7日", "windows": map[string]any{"previous": map[string]any{"from": iso(to.Add(-14 * 24 * time.Hour)), "to": iso(to.Add(-7 * 24 * time.Hour))}, "current": map[string]any{"from": iso(to.Add(-7 * 24 * time.Hour)), "to": iso(to)}}, "previous": previous, "current": current, "causeKey": "filing-check:" + repo}
}
