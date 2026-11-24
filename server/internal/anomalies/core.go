package anomalies

import (
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

var names = map[string]string{"repeat": "競合解消・独立レビューの繰り返し", "outlier": "1件の量の外れ", "failure": "失敗・世代の繰り返し", "hourly": "1時間の量の急増", "cache": "相談のキャッシュに乗らない入力の増加", "intake-repeat": "受付の活動下書き当たりの呼び出し増加", "call-trigger": "受付の呼び出しのきっかけの比率の悪化", "unit-total": "相談・Issue 単位の合計の外れ", "provider-shift": "実行先の急変", "limit-forecast": "上限の枠が切れる見込み", "filing-check": "起票1件あたりの確認回数の増加", "filing-cache": "起票直前の確認のキャッシュの是正の効果なし"}

func at(row map[string]any) time.Time {
	v, _ := time.Parse(time.RFC3339Nano, fleet.Text(row["at"]))
	return v
}
func iso(t time.Time) string { return contract.ISO(t) }
func number(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func integer(v any) bool {
	n, ok := number(v)
	return ok && n == math.Trunc(n) && math.Abs(n) <= 9007199254740991
}
func issueKey(row map[string]any) string {
	if fleet.Text(row["repo"]) == "" || !integer(row["issue"]) {
		return ""
	}
	return fleet.Text(row["repo"]) + "#" + strconv.FormatInt(int64(fleet.Number(row["issue"])), 10)
}
func weighted(row, weights map[string]any) (float64, bool) {
	return number(usage.Weighted(row, weights)["weighted"])
}
func median(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sort.Float64s(values)
	return (values[(len(values)-1)/2] + values[len(values)/2]) / 2, true
}
func copyRow(row map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range strings.Fields("at step provider model inputTokens cacheReadTokens cacheWriteTokens outputTokens generation turn attempt generationTrigger outcome elapsedMs") {
		out[key] = row[key]
	}
	return out
}
func cause(kind string, row map[string]any) string {
	if kind == "hourly" {
		return kind + ":" + text(row["step"], "-")
	}
	repo := text(row["repo"], "-")
	issue := "-"
	if integer(row["issue"]) {
		issue = strconv.FormatInt(int64(fleet.Number(row["issue"])), 10)
	}
	return kind + ":" + repo + "#" + issue
}
func text(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}
func hit(kind string, row map[string]any, measured, baseline, threshold any, rule, period, unit string) map[string]any {
	return map[string]any{"kind": kind, "kindName": names[kind], "repo": row["repo"], "issue": row["issue"], "step": row["step"], "at": row["at"], "measured": measured, "baseline": baseline, "threshold": threshold, "rule": rule, "period": period, "unit": unit, "row": copyRow(row), "causeKey": cause(kind, row)}
}
func inWindow(row map[string]any, from, to time.Time) bool {
	t := at(row)
	return !t.IsZero() && t.After(from) && !t.After(to)
}
func matchesBucket(a, b map[string]any, kept int) bool {
	for i, key := range []string{"provider", "model", "turn", "generation"} {
		if i >= kept {
			break
		}
		if key == "turn" || key == "generation" {
			if (integer(a[key]) && fleet.Number(a[key]) >= 2) != (integer(b[key]) && fleet.Number(b[key]) >= 2) {
				return false
			}
		} else if a[key] != b[key] {
			return false
		}
	}
	return true
}
func Basic(rows, history []map[string]any, weights map[string]any) []map[string]any {
	byIssue, byStep := indexHistory(history)
	seen := map[string]bool{}
	out := []map[string]any{}
	out = append(out, repeatedSteps(rows, byIssue, seen)...)
	out = append(out, stepOutliers(rows, byStep, weights)...)
	out = append(out, consecutiveFailures(rows, byIssue, seen)...)
	out = append(out, generationIncreases(rows, byIssue, seen)...)
	out = append(out, hourlySurges(rows, byStep, weights, seen)...)
	return out
}

// indexHistory groups dated history rows by issue (ordered by time) and by step.
func indexHistory(history []map[string]any) (map[string][]map[string]any, map[string][]map[string]any) {
	byIssue := map[string][]map[string]any{}
	byStep := map[string][]map[string]any{}
	for _, r := range history {
		if at(r).IsZero() {
			continue
		}
		if key := issueKey(r); key != "" {
			byIssue[key] = append(byIssue[key], r)
		}
		if step, ok := r["step"].(string); ok {
			byStep[step] = append(byStep[step], r)
		}
	}
	for _, group := range byIssue {
		sort.SliceStable(group, func(i, j int) bool { return at(group[i]).Before(at(group[j])) })
	}
	return byIssue, byStep
}

// repeatedSteps detects conflict resolution and independent review rows repeated within a day.
func repeatedSteps(rows []map[string]any, byIssue map[string][]map[string]any, seen map[string]bool) []map[string]any {
	out := []map[string]any{}
	// Repetition retains the first threshold crossing for each issue.
	for _, r := range rows {
		key := issueKey(r)
		if key == "" || seen[cause("repeat", r)] || (r["step"] != "競合解消" && r["step"] != "独立レビュー") {
			continue
		}
		end := at(r)
		for _, check := range []struct {
			Step  string
			Limit int
		}{{"競合解消", 3}, {"独立レビュー", 4}} {
			mine := []map[string]any{}
			for _, h := range byIssue[key] {
				if h["step"] == check.Step && inWindow(h, end.Add(-24*time.Hour), end) {
					mine = append(mine, h)
				}
			}
			if len(mine) >= check.Limit {
				h := hit("repeat", mine[len(mine)-1], len(mine), nil, check.Limit, fmt.Sprintf("%sの行が%d回以上", check.Step, check.Limit), "直近24時間", "回")
				h["at"] = r["at"]
				out = append(out, h)
				seen[cause("repeat", r)] = true
				break
			}
		}
	}
	return out
}

// stepOutliers detects rows far above the median of their step population.
func stepOutliers(rows []map[string]any, byStep map[string][]map[string]any, weights map[string]any) []map[string]any {
	out := []map[string]any{}
	// Outliers choose the largest ratio while relaxing generation, turn, model, provider.
	best := map[string]map[string]any{}
	order := []string{}
	for _, r := range rows {
		value, ok := weighted(r, weights)
		end := at(r)
		if !ok || end.IsZero() {
			continue
		}
		step, ok := r["step"].(string)
		if !ok {
			continue
		}
		compared := []float64{}
		kept := 4
		for ; kept >= 0; kept-- {
			compared = nil
			for _, h := range byStep[step] {
				t := at(h)
				if t.Before(end.Add(-14*24*time.Hour)) || !t.Before(end) || !matchesBucket(r, h, kept) {
					continue
				}
				if v, ok := weighted(h, weights); ok {
					compared = append(compared, v)
				}
			}
			if len(compared) >= 10 {
				break
			}
		}
		m, ok := median(compared)
		if kept < 0 || !ok || m <= 0 || value < m*3 {
			continue
		}
		h := hit("outlier", r, value, m, m*3, fmt.Sprintf("同じ母集団の中央値の3倍以上（比べた行 %d件）", len(compared)), "直近14日", "量")
		h["ratio"] = value / m
		population := map[string]any{"step": step}
		for i, key := range []string{"provider", "model", "turn", "generation"} {
			if i >= kept {
				break
			}
			v := r[key]
			if key == "turn" || key == "generation" {
				v = "first"
				if integer(r[key]) && fleet.Number(r[key]) >= 2 {
					v = "later"
				}
			}
			population[key] = v
		}
		h["population"] = population
		key := cause("outlier", r)
		if old := best[key]; old == nil {
			order = append(order, key)
			best[key] = h
		} else if value/m > fleet.Number(old["ratio"]) {
			best[key] = h
		}
	}
	for _, key := range order {
		out = append(out, best[key])
	}
	return out
}

// consecutiveFailures detects three failed, timed out or stalled rows in a row for one issue and step.
func consecutiveFailures(rows []map[string]any, byIssue map[string][]map[string]any, seen map[string]bool) []map[string]any {
	out := []map[string]any{}
	for _, r := range rows {
		key := issueKey(r)
		if key == "" || seen[cause("failure", r)] {
			continue
		}
		same := []map[string]any{}
		for _, h := range byIssue[key] {
			if h["step"] == r["step"] && !at(h).After(at(r)) {
				same = append(same, h)
			}
		}
		if len(same) >= 3 {
			last := same[len(same)-3:]
			failed := true
			for _, h := range last {
				if h["outcome"] != "failed" && h["outcome"] != "timeout" && h["outcome"] != "stalled" {
					failed = false
				}
			}
			if failed {
				out = append(out, hit("failure", r, 3, nil, 3, "失敗・時間切れ・停止の行が3回続く", "同じIssue・工程の最新の行", "回"))
				seen[cause("failure", r)] = true
			}
		}
	}
	return out
}

// generationIncreases detects an issue whose generation increased three times within a day.
func generationIncreases(rows []map[string]any, byIssue map[string][]map[string]any, seen map[string]bool) []map[string]any {
	out := []map[string]any{}
	for _, r := range rows {
		key := issueKey(r)
		if key == "" || seen[cause("failure", r)] || !integer(r["generation"]) {
			continue
		}
		end := at(r)
		from := end.Add(-24 * time.Hour)
		base := math.Inf(-1)
		inside := []float64{}
		for _, h := range byIssue[key] {
			if !integer(h["generation"]) {
				continue
			}
			t := at(h)
			g := fleet.Number(h["generation"])
			if !t.After(from) {
				base = math.Max(base, g)
			} else if !t.After(end) {
				inside = append(inside, g)
			}
		}
		if len(inside) == 0 {
			continue
		}
		if math.IsInf(base, -1) {
			base = inside[0]
			for _, g := range inside {
				base = math.Min(base, g)
			}
		}
		increases := map[float64]bool{}
		for _, g := range inside {
			if g > base {
				increases[g] = true
			}
		}
		if len(increases) >= 3 {
			h := hit("failure", r, len(increases), nil, 3, "世代が3回以上増える", "直近24時間", "回")
			h["step"], h["generationBase"] = nil, base
			out = append(out, h)
			seen[cause("failure", r)] = true
		}
	}
	return out
}

// hourlySurges detects a step whose last hour exceeds four times the hourly median of the past week.
func hourlySurges(rows []map[string]any, byStep map[string][]map[string]any, weights map[string]any, seen map[string]bool) []map[string]any {
	out := []map[string]any{}
	for _, r := range rows {
		step, ok := r["step"].(string)
		if !ok || seen[cause("hourly", r)] {
			continue
		}
		end := at(r)
		sums := map[int]float64{}
		current := float64(0)
		for _, h := range byStep[step] {
			age := end.Sub(at(h))
			v, ok := weighted(h, weights)
			if !ok || age < 0 || age > 8*24*time.Hour {
				continue
			}
			if age <= time.Hour {
				current += v
			} else if age <= time.Hour+7*24*time.Hour {
				bucket := int(math.Ceil(float64(age-time.Hour) / float64(time.Hour)))
				sums[bucket] += v
			}
		}
		used := []float64{}
		for _, n := range sums {
			if n > 0 {
				used = append(used, n)
			}
		}
		m, ok := median(used)
		if !ok || m <= 0 || current < m*4 {
			continue
		}
		h := hit("hourly", r, current, m, m*4, "直近7日の使用のあった1時間の中央値の4倍以上", "直近1時間", "量")
		h["repo"], h["issue"], h["ratio"] = nil, nil, current/m
		out = append(out, h)
		seen[cause("hourly", r)] = true
	}
	return out
}
