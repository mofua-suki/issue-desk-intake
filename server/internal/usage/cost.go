package usage

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"maps"
	"math"
)

func sessionRow(r map[string]any) bool {
	return r["provider"] == "claude" && (r["step"] == "実装" || r["step"] == "競合解消")
}
func whole(v any) bool {
	n, ok := known(v)
	return ok && n == math.Trunc(n) && math.Abs(n) <= 9007199254740991
}
func identity(v ...any) string { b, _ := json.Marshal(v); return string(b) }

// CorrectCosts converts cumulative session costs into per-call costs where the
// recorded identity and preceding turn prove that subtraction is valid, and
// prices Codex rows, which report tokens but no cost (see CodexCost). A changed
// row is a copy with its own costUsd; its nested values are shared with the
// original and must not be modified.
func CorrectCosts(rows, lookback []map[string]any) ([]map[string]any, []string) {
	fill := firstTurnFills(rows, lookback)
	previous, first := sessionTurns(rows)
	out := make([]map[string]any, len(rows))
	status := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r
		if fill[i] {
			out[i] = maps.Clone(r)
			out[i]["costUsd"] = r["sessionCostUsd"]
			status[i] = "filled"
			continue
		}
		if _, exists := r["sessionCostUsd"]; exists || !sessionRow(r) || !whole(r["turn"]) {
			continue
		}
		attempt := 1.0
		if v, ok := known(r["attempt"]); ok {
			attempt = v
		}
		if attempt >= 2 {
			continue
		}
		turn := fleet.Number(r["turn"])
		if turn == 1 {
			firstGeneration := first[identity(r["repo"], r["issue"])]
			if whole(r["generation"]) && fleet.Number(r["generation"]) >= 2 && firstGeneration < fleet.Number(r["generation"]) {
				status[i] = "uncorrectable"
			}
			continue
		}
		var before map[string]any
		if fleet.Text(r["repo"]) != "" && whole(r["issue"]) && whole(r["generation"]) {
			before = previous[identity(r["repo"], r["issue"], r["generation"], r["step"], turn-1)]
		}
		cost, ok := known(r["costUsd"])
		prior, priorOK := known(before["costUsd"])
		if before == nil || !ok || !priorOK || cost < prior {
			status[i] = "uncorrectable"
			continue
		}
		out[i] = maps.Clone(r)
		out[i]["costUsd"] = cost - prior
		status[i] = "corrected"
	}
	priceCodex(out)
	return out, status
}

// firstTurnFills names the rows whose missing first-turn cost is the session
// cost, because no earlier cost of the issue is recorded.
func firstTurnFills(rows, lookback []map[string]any) map[int]bool {
	fill := map[int]bool{}
	seen := map[string]bool{}
	all := append(append([]map[string]any{}, lookback...), rows...)
	for i, r := range all {
		if !sessionRow(r) {
			continue
		}
		key := identity(r["repo"], r["issue"])
		session, hasSession := r["sessionCostUsd"]
		_, costOK := known(r["costUsd"])
		_, sessionOK := known(session)
		if lookback != nil && i >= len(lookback) && hasSession && r["turn"] == 1.0 && r["costUsd"] == nil && sessionOK && fleet.Text(r["repo"]) != "" && whole(r["issue"]) && !seen[key] {
			fill[i-len(lookback)] = true
		}
		if costOK || sessionOK {
			seen[key] = true
		}
	}
	return fill
}

// sessionTurns indexes the latest attempt of each session turn and the first
// generation of each issue.
func sessionTurns(rows []map[string]any) (map[string]map[string]any, map[string]float64) {
	previous := map[string]map[string]any{}
	first := map[string]float64{}
	for _, r := range rows {
		if !sessionRow(r) {
			continue
		}
		if whole(r["turn"]) {
			key := identity(r["repo"], r["issue"], r["generation"], r["step"], r["turn"])
			had := previous[key]
			if had == nil || fleet.Number(r["attempt"]) >= fleet.Number(had["attempt"]) {
				previous[key] = r
			}
		}
		if whole(r["generation"]) {
			key := identity(r["repo"], r["issue"])
			generation := fleet.Number(r["generation"])
			if old, ok := first[key]; !ok || generation < old {
				first[key] = generation
			}
		}
	}
	return previous, first
}
