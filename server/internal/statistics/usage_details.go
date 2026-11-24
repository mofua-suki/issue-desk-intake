package statistics

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"sort"
	"strings"
	"time"
)

func sumAverage(rows []map[string]any, pick func(map[string]any) any) map[string]any {
	var total any
	known, unknown := 0, 0
	for _, r := range rows {
		if n, ok := wholeValue(pick(r)); ok {
			total = fleet.Number(total) + n
			known++
		} else {
			unknown++
		}
	}
	var avg any
	if known > 0 {
		avg = fleet.Number(total) / float64(known)
	}
	return map[string]any{"total": total, "average": avg, "unknown": unknown}
}
func toolsSummary(rows []map[string]any, field, label string, allowed []string) map[string]any {
	count, unknown, total := 0, 0, float64(0)
	counts := map[string]float64{}
	for _, r := range rows {
		m := fleet.Object(r[field])
		if m == nil {
			unknown++
			continue
		}
		count++
		for key, v := range m {
			n, ok := wholeValue(v)
			if !ok || key == "" || allowed != nil && !oneOf(key, allowed) {
				continue
			}
			counts[key] += n
			total += n
		}
	}
	keys := []string{}
	for key := range counts {
		if counts[key] > 0 || field == "tools" {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	items := []any{}
	for _, key := range keys {
		items = append(items, map[string]any{label: key, "count": counts[key]})
	}
	return map[string]any{"count": count, "unknown": unknown, "total": total, "items": items}
}
func fileSummary(rows []map[string]any) map[string]any {
	count, unknown := 0, 0
	files := map[string]map[string]any{}
	for _, r := range rows {
		list, ok := r["files"].([]any)
		if !ok {
			unknown++
			continue
		}
		count++
		seen := map[string]bool{}
		for _, raw := range list {
			f := fleet.Object(raw)
			if fleet.Text(f["path"]) == "" {
				continue
			}
			key := identity(r["repo"], f["path"])
			item := files[key]
			if item == nil {
				item = map[string]any{"repo": r["repo"], "path": f["path"], "count": 0, "bytes": nil, "unknown": 0}
				files[key] = item
			}
			if !seen[key] {
				item["count"] = item["count"].(int) + 1
			}
			seen[key] = true
			if n, ok := wholeValue(f["bytes"]); ok {
				item["bytes"] = fleet.Number(item["bytes"]) + n
			} else {
				item["unknown"] = item["unknown"].(int) + 1
			}
		}
	}
	items := []any{}
	for _, item := range files {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := fleet.Object(items[i]), fleet.Object(items[j])
		if (a["bytes"] == nil) != (b["bytes"] == nil) {
			return a["bytes"] != nil
		}
		if a["bytes"] != b["bytes"] {
			return fleet.Number(a["bytes"]) > fleet.Number(b["bytes"])
		}
		if a["count"] != b["count"] {
			return a["count"].(int) > b["count"].(int)
		}
		if a["repo"] != b["repo"] {
			return fleet.Text(a["repo"]) < fleet.Text(b["repo"])
		}
		return fleet.Text(a["path"]) < fleet.Text(b["path"])
	})
	omitted := 0
	if len(items) > 20 {
		omitted = len(items) - 20
		items = items[:20]
	}
	return map[string]any{"count": count, "unknown": unknown, "items": items, "omitted": omitted}
}
func promptSummary(rows []map[string]any) map[string]any {
	included := []map[string]any{}
	unknown := 0
	for _, r := range rows {
		if usage.CanonicalStep(fleet.Text(r["step"])) != usage.ConsultationStep {
			continue
		}
		if fleet.Object(r["promptBytes"]) == nil {
			unknown++
		} else {
			included = append(included, r)
		}
	}
	parts := []any{}
	for _, name := range usage.PromptPartNames() {
		key := name
		row := sumAverage(included, func(r map[string]any) any { return fleet.Object(r["promptBytes"])[key] })
		row["name"] = name
		parts = append(parts, row)
	}
	out := map[string]any{"count": len(included), "unknown": unknown, "parts": parts}
	for _, name := range []string{"promptTotalBytes", "cacheReadTokens", "cacheWriteTokens"} {
		key := name
		out[name] = sumAverage(included, func(r map[string]any) any { return r[key] })
	}
	return out
}
func rateLimitsSummary(rows []map[string]any) map[string]any {
	claude := []map[string]any{}
	for _, r := range rows {
		if r["provider"] == "claude" {
			claude = append(claude, r)
		}
	}
	sort.SliceStable(claude, func(i, j int) bool { return fleet.Text(claude[i]["at"]) < fleet.Text(claude[j]["at"]) })
	windows, resets, count, unknown := rateLimitWindows(claude)
	out := rateLimitWindowList(claude, windows, resets)
	return map[string]any{"count": count, "unknown": unknown, "windows": out}
}

// rateLimitWindows groups the Claude rows' rate limits by window and reset time.
func rateLimitWindows(claude []map[string]any) (windows map[string]map[string]any, resets map[string]map[string]map[string]any, count, unknown int) {
	windows = map[string]map[string]any{}
	resets = map[string]map[string]map[string]any{}
	for index, r := range claude {
		limit := fleet.Object(r["rateLimit"])
		if limit == nil {
			unknown++
			continue
		}
		count++
		list := map[string]map[string]any{}
		for name, raw := range fleet.Object(limit["windows"]) {
			if name != "" && fleet.Object(raw) != nil {
				list[name] = fleet.Object(raw)
			}
		}
		if len(list) == 0 && fleet.Text(limit["type"]) != "" {
			list[fleet.Text(limit["type"])] = limit
		}
		for name, value := range list {
			win := windows[name]
			if win == nil {
				win = map[string]any{"name": name, "records": 0, "latest": nil, "unknownResets": 0}
				windows[name] = win
				resets[name] = map[string]map[string]any{}
			}
			win["records"] = win["records"].(int) + 1
			var percent, utilization any
			if n, ok := value["utilization"].(float64); ok && n >= 0 {
				utilization = n
				percent = math.Round(n*100*1e6) / 1e6
			}
			mark := map[string]any{"at": r["at"], "utilization": utilization, "percent": percent}
			reset := fleet.Text(value["resetsAt"])
			latest := map[string]any{}
			for k, v := range mark {
				latest[k] = v
			}
			latest["resetsAt"] = nil
			if reset != "" {
				latest["resetsAt"] = reset
			}
			win["latest"] = latest
			if reset == "" {
				win["unknownResets"] = win["unknownResets"].(int) + 1
				continue
			}
			group := resets[name][reset]
			if group == nil {
				group = map[string]any{"resetsAt": reset, "records": 0, "first": mark, "from": index}
				resets[name][reset] = group
			}
			group["records"] = group["records"].(int) + 1
			group["last"], group["to"] = mark, index
		}
	}
	return windows, resets, count, unknown
}

// rateLimitWindowList lists the windows by name with the usage of each reset period.
func rateLimitWindowList(claude []map[string]any, windows map[string]map[string]any, resets map[string]map[string]map[string]any) []any {
	keys := []string{}
	for key := range windows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []any{}
	for _, key := range keys {
		win := windows[key]
		resetKeys := []string{}
		for reset := range resets[key] {
			resetKeys = append(resetKeys, reset)
		}
		sort.Strings(resetKeys)
		groups := []any{}
		for _, reset := range resetKeys {
			g := resets[key][reset]
			from, to := g["from"].(int), g["to"].(int)
			delete(g, "from")
			delete(g, "to")
			subset := claude[from : to+1]
			total := outcomeTotals(subset)
			delete(total, "costUsd")
			delete(fleet.Object(total["unknown"]), "costUsd")
			delete(total, "providers")
			g["claude"] = total
			groups = append(groups, g)
		}
		win["resets"] = groups
		out = append(out, win)
	}
	return out
}
func toolErrorSummary(rows []map[string]any) map[string]any {
	out := toolsSummary(rows, "toolErrors", "category", strings.Fields("sandbox_rejected command_not_found nonzero_exit timeout read_denied other notice retry"))
	commands := toolsSummary(rows, "missingCommands", "name", nil)
	items := commands["items"].([]any)
	omitted := 0
	if len(items) > 20 {
		omitted = len(items) - 20
		items = items[:20]
	}
	out["commands"], out["commandsOmitted"], out["commandsUnknown"] = items, omitted, commands["unknown"]
	return out
}
func serviceTierSummary(rows []map[string]any) map[string]any {
	count, unknown := 0, 0
	groups := map[string][]map[string]any{}
	labels := map[string]string{}
	for _, r := range rows {
		if r["provider"] != "codex" && r["provider"] != "claude" {
			continue
		}
		label := fleet.Text(r["serviceTier"])
		if r["provider"] == "claude" {
			label = fleet.Text(r["fastMode"])
		}
		if strings.TrimSpace(label) == "" || len(label) > 200 {
			unknown++
			continue
		}
		count++
		key := identity(r["provider"], label)
		groups[key] = append(groups[key], r)
		labels[key] = label
	}
	out := []any{}
	for key, group := range groups {
		row := outcomeTotals(group)
		for _, key := range []string{"unknown", "providers", "costUsd"} {
			delete(row, key)
		}
		row["provider"], row["label"] = group[0]["provider"], labels[key]
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := fleet.Object(out[i]), fleet.Object(out[j])
		if a["count"] != b["count"] {
			return a["count"].(int) > b["count"].(int)
		}
		if a["provider"] != b["provider"] {
			return fleet.Text(a["provider"]) < fleet.Text(b["provider"])
		}
		return fleet.Text(a["label"]) < fleet.Text(b["label"])
	})
	return map[string]any{"count": count, "unknown": unknown, "items": out}
}
func usageDetails(mine, inPeriod []map[string]any) map[string]any {
	mine = canonicalUsageRows(mine)
	errors := toolErrorSummary(mine)
	steps := []any{}
	for _, group := range groupRows(mine, "step", "provider") {
		row := toolErrorSummary(group)
		row["step"], row["provider"] = group[0]["step"], group[0]["provider"]
		steps = append(steps, row)
	}
	sort.Slice(steps, func(i, j int) bool {
		a, b := fleet.Object(steps[i]), fleet.Object(steps[j])
		if a["step"] != b["step"] {
			return fleet.Text(a["step"]) < fleet.Text(b["step"])
		}
		return fleet.Text(a["provider"]) < fleet.Text(b["provider"])
	})
	errors["steps"] = steps
	return map[string]any{"tools": toolsSummary(mine, "tools", "name", nil), "files": fileSummary(mine), "promptParts": promptSummary(mine), "rateLimits": rateLimitsSummary(inPeriod), "toolErrors": errors, "serviceTiers": serviceTierSummary(mine)}
}
func intakeCacheSummary(rows []map[string]any, p Period) map[string]any {
	steps := []string{usage.ConsultationStep, usage.FilingScopeStep}
	totals := func(mine []map[string]any) map[string]any {
		var read, write any
		unknown := 0
		for _, r := range mine {
			a, ao := r["cacheReadTokens"].(float64)
			b, bo := r["cacheWriteTokens"].(float64)
			if !ao || !bo {
				unknown++
				continue
			}
			read, write = fleet.Number(read)+a, fleet.Number(write)+b
		}
		var ratio any
		if read != nil && fleet.Number(read)+fleet.Number(write) > 0 {
			ratio = fleet.Number(read) / (fleet.Number(read) + fleet.Number(write))
		}
		return map[string]any{"count": len(mine), "cacheReadTokens": read, "cacheWriteTokens": write, "unknown": unknown, "readRatio": ratio}
	}
	byStep := []any{}
	days := []any{}
	for _, step := range steps {
		mine := []map[string]any{}
		for _, r := range rows {
			if r["source"] == "intake" && r["provider"] == "claude" && usage.CanonicalStep(fleet.Text(r["step"])) == step {
				mine = append(mine, r)
			}
		}
		row := totals(mine)
		row["step"] = step
		byStep = append(byStep, row)
	}
	for at := jstMidnight(p.From); at.Before(p.To); at = at.Add(24 * time.Hour) {
		for _, step := range steps {
			mine := []map[string]any{}
			for _, r := range rows {
				t, _ := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
				if r["source"] == "intake" && r["provider"] == "claude" && usage.CanonicalStep(fleet.Text(r["step"])) == step && !t.Before(at) && t.Before(at.Add(24*time.Hour)) {
					mine = append(mine, r)
				}
			}
			row := totals(mine)
			row["date"], row["step"] = at.In(JST).Format("2006-01-02"), step
			days = append(days, row)
		}
	}
	return map[string]any{"steps": byStep, "days": days, "uncached": uncachedInputSummary(rows)}
}
func uncachedInputSummary(rows []map[string]any) map[string]any {
	totals := func(mine []map[string]any) map[string]any {
		out := map[string]any{"count": len(mine), "unknown": 0, "uncachedInputTokens": nil, "cacheReadTokens": nil, "cacheWriteTokens": nil, "ratio": nil}
		for _, r := range mine {
			input, io := r["inputTokens"].(float64)
			read, ro := r["cacheReadTokens"].(float64)
			write, wo := r["cacheWriteTokens"].(float64)
			if r["provider"] == "codex" {
				input -= read
				write, wo = 0, true
				if input < 0 {
					io = false
				}
			} else if r["provider"] != "claude" {
				io = false
			}
			if !io || !ro || !wo {
				out["unknown"] = out["unknown"].(int) + 1
				continue
			}
			out["uncachedInputTokens"], out["cacheReadTokens"], out["cacheWriteTokens"] = fleet.Number(out["uncachedInputTokens"])+input, fleet.Number(out["cacheReadTokens"])+read, fleet.Number(out["cacheWriteTokens"])+write
		}
		denom := fleet.Number(out["uncachedInputTokens"]) + fleet.Number(out["cacheReadTokens"]) + fleet.Number(out["cacheWriteTokens"])
		if denom > 0 {
			out["ratio"] = fleet.Number(out["uncachedInputTokens"]) / denom
		}
		return out
	}
	mine := []map[string]any{}
	for _, r := range rows {
		if usage.CanonicalStep(fleet.Text(r["step"])) == usage.ConsultationStep {
			mine = append(mine, r)
		}
	}
	out := totals(mine)
	providers := []any{}
	for _, group := range groupRows(mine, "provider") {
		row := totals(group)
		row["provider"] = group[0]["provider"]
		providers = append(providers, row)
	}
	out["providers"] = providers
	return out
}
