package usage

import (
	"issue-desk-intake/server/internal/fleet"
	"math"
	"sort"
)

var metrics = []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "costUsd", "uncachedInputTokens", "normalizedCacheWriteTokens", "weighted"}

func known(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func Weighted(row, table map[string]any) map[string]any {
	out := map[string]any{"uncachedInputTokens": nil, "normalizedCacheWriteTokens": nil, "weighted": nil}
	provider := fleet.Text(row["provider"])
	if provider != "codex" && provider != "claude" {
		return out
	}
	input, inputOK := known(row["inputTokens"])
	read, readOK := known(row["cacheReadTokens"])
	write, writeOK := known(row["cacheWriteTokens"])
	output, outputOK := known(row["outputTokens"])
	if provider == "codex" {
		write, writeOK = 0, true
		if !readOK {
			inputOK = false
		} else {
			input -= read
			if input < 0 {
				inputOK = false
			}
		}
	}
	if inputOK {
		out["uncachedInputTokens"] = input
	}
	if writeOK {
		out["normalizedCacheWriteTokens"] = write
	}
	if !inputOK || !readOK || !writeOK || !outputOK {
		return out
	}
	weights := map[string]float64{"input": 1, "cacheRead": 0.1, "cacheWrite": 1.25, "output": 5}
	p := fleet.Object(fleet.Object(table["providers"])[provider])
	m := fleet.Object(fleet.Object(p["models"])[fleet.Text(row["model"])])
	for _, override := range []map[string]any{fleet.Object(table["default"]), fleet.Object(p["default"]), m} {
		for key, value := range override {
			if n, ok := known(value); ok && n >= 0 {
				weights[key] = n
			}
		}
	}
	out["weighted"] = input*weights["input"] + read*weights["cacheRead"] + write*weights["cacheWrite"] + output*weights["output"]
	return out
}
func Totals(rows []map[string]any, weights map[string]any) map[string]any {
	unknown := map[string]any{}
	out := map[string]any{"count": len(rows), "cacheWriteNotApplicable": 0, "unknown": unknown}
	for _, key := range metrics {
		out[key] = nil
		unknown[key] = 0
	}
	for _, row := range rows {
		normalized := Weighted(row, weights)
		if row["provider"] == "codex" {
			out["cacheWriteNotApplicable"] = out["cacheWriteNotApplicable"].(int) + 1
		}
		for _, key := range metrics {
			value := row[key]
			if _, exists := normalized[key]; exists {
				value = normalized[key]
			}
			if n, ok := known(value); ok {
				sum, _ := known(out[key])
				out[key] = sum + n
			} else {
				unknown[key] = unknown[key].(int) + 1
			}
		}
	}
	return out
}
func Groups(rows []map[string]any, axis string, weights map[string]any) []map[string]any {
	buckets := map[string][]map[string]any{}
	for _, row := range rows {
		key := fleet.Text(row[axis])
		buckets[key] = append(buckets[key], row)
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []map[string]any{}
	for _, key := range keys {
		totals := Totals(buckets[key], weights)
		var value any
		if key != "" {
			value = key
		}
		totals[axis] = value
		out = append(out, totals)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, aOK := known(out[i]["weighted"])
		b, bOK := known(out[j]["weighted"])
		if aOK != bOK {
			return aOK
		}
		if a != b {
			return a > b
		}
		return out[i]["count"].(int) > out[j]["count"].(int)
	})
	return out
}
