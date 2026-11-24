package usage

import (
	"issue-desk-intake/server/internal/fleet"
	"maps"
)

// codexRate is a USD price per one million tokens.
type codexRate struct{ input, cachedInput, output float64 }

// codexRates are OpenAI's Standard (not Fast/Batch/Flex), short-context
// (<=272K input tokens per request) list prices, read from
// https://developers.openai.com/api/docs/pricing (6.2 rates checked 2026-11-18).
// Codex CLI reports tokens but no cost, so the statistics price its rows here.
// A model missing from this table stays cost-unknown; never add a rate without
// a source.
var codexRates = map[string]codexRate{
	"gpt-6.2-astra": {8.00, 0.40, 40.00},
	"gpt-6.2-sol":   {2.00, 0.10, 10.00},
	"gpt-6.1-sol":   {2.00, 0.10, 10.00},
	"gpt-6.1-terra": {1.00, 0.05, 5.00},
	"gpt-6-luna":    {0.10, 0.01, 0.50},
	"gpt-6-astra":   {10.00, 1.00, 50.00},
	"gpt-6-sol":     {2.00, 0.20, 10.00},
}

// codexShortContextLimit is the largest per-request input billed at the
// short-context rate. Larger requests use a long-context rate that a row's
// totals cannot apportion.
const codexShortContextLimit = 272000

// CodexCost prices a Codex usage row from its recorded tokens. It returns false
// unless every input is known and the Standard short-context rate applies: a
// listed model, a complete measurement, no reported cache writes,
// contextMaxTokens (an upper bound of every request's input) within the
// short-context limit, and no non-default serviceTier. An absent serviceTier is
// taken as the CLI default tier: gpt-6.1-terra is run with
// service_tier="default", and the NAS Codex sessions recorded for gpt-6.1-sol
// and gpt-6-astra report service_tier "default". For 6.2, require a recorded
// Standard tier. Codex input_tokens include
// cached_input_tokens, and output_tokens include reasoning tokens.
func CodexCost(r map[string]any) (float64, bool) {
	if r["provider"] != "codex" || r["usagePartial"] == true {
		return 0, false
	}
	model := fleet.Text(r["model"])
	rate, ok := codexRates[model]
	if !ok {
		return 0, false
	}
	rawTier := r["serviceTier"]
	tier := fleet.Text(rawTier)
	if tier != "default" && (rawTier != nil || model == "gpt-6.2-sol" || model == "gpt-6.2-astra") {
		return 0, false
	}
	if write, exists := r["cacheWriteTokens"]; exists && write != nil {
		if n, ok := known(write); !ok || n != 0 {
			return 0, false
		}
	}
	input, cached, output, context := r["inputTokens"], r["cacheReadTokens"], r["outputTokens"], r["contextMaxTokens"]
	if !whole(input) || !whole(cached) || !whole(output) || !whole(context) {
		return 0, false
	}
	in, read, out, peak := fleet.Number(input), fleet.Number(cached), fleet.Number(output), fleet.Number(context)
	if in < 0 || read < 0 || out < 0 || read > in || peak < 0 || peak > codexShortContextLimit {
		return 0, false
	}
	return ((in-read)*rate.input + read*rate.cachedInput + out*rate.output) / 1e6, true
}

// priceCodex fills costUsd on Codex rows that have none and can be priced. The
// priced row is a copy with its own costUsd; its nested values are shared with
// the original and must not be modified.
func priceCodex(rows []map[string]any) {
	for i, r := range rows {
		if r["provider"] != "codex" || r["costUsd"] != nil {
			continue
		}
		if cost, ok := CodexCost(r); ok {
			rows[i] = maps.Clone(r)
			rows[i]["costUsd"] = cost
		}
	}
}
