package usage

import (
	"math"
	"testing"
)

func codexRow() map[string]any {
	return map[string]any{"provider": "codex", "model": "gpt-6.1-sol", "inputTokens": float64(1_000_000), "cacheReadTokens": float64(600_000), "outputTokens": float64(20_000), "contextMaxTokens": float64(90_000), "costUsd": nil}
}

func TestCodexCostUsesListedRatesForCachedAndUncachedInput(t *testing.T) {
	cost, ok := CodexCost(codexRow())
	// 400K uncached x $2 + 600K cached x $0.10 + 20K output x $10, per 1M tokens.
	if want := 0.8 + 0.06 + 0.2; !ok || math.Abs(cost-want) > 1e-9 {
		t.Fatalf("cost = %v %v, want %v", cost, ok, want)
	}
	terra := codexRow()
	terra["model"], terra["serviceTier"], terra["cacheWriteTokens"] = "gpt-6.1-terra", "default", float64(0)
	if cost, ok := CodexCost(terra); !ok || math.Abs(cost-(0.4+0.03+0.1)) > 1e-9 {
		t.Fatalf("terra cost = %v %v", cost, ok)
	}
}

func TestCodexCostUsesStandardRatesForGPT62(t *testing.T) {
	for model, want := range map[string]float64{"gpt-6.2-sol": 1.06, "gpt-6.2-astra": 4.24} {
		row := codexRow()
		row["model"] = model
		if _, ok := CodexCost(row); ok {
			t.Errorf("%s: priced a row without a known speed", model)
		}
		row["serviceTier"] = "default"
		if cost, ok := CodexCost(row); !ok || math.Abs(cost-want) > 1e-9 {
			t.Errorf("%s: cost = %v %v, want %v", model, cost, ok, want)
		}
	}
}

func TestCodexCostStaysUnknownWhenTheRateIsUncertain(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"unlisted model":           func(r map[string]any) { r["model"] = "gpt-unlisted" },
		"claude row":               func(r map[string]any) { r["provider"] = "claude" },
		"long context request":     func(r map[string]any) { r["contextMaxTokens"] = float64(272_001) },
		"request size unknown":     func(r map[string]any) { delete(r, "contextMaxTokens") },
		"partial measurement":      func(r map[string]any) { r["usagePartial"] = true },
		"fast service tier":        func(r map[string]any) { r["serviceTier"] = "priority" },
		"reported cache writes":    func(r map[string]any) { r["cacheWriteTokens"] = float64(5) },
		"output unknown":           func(r map[string]any) { r["outputTokens"] = nil },
		"cached exceeds the input": func(r map[string]any) { r["cacheReadTokens"] = float64(2_000_000) },
	} {
		r := codexRow()
		change(r)
		if cost, ok := CodexCost(r); ok {
			t.Errorf("%s: priced %v", name, cost)
		}
	}
	r := codexRow()
	r["contextMaxTokens"] = float64(codexShortContextLimit)
	if _, ok := CodexCost(r); !ok {
		t.Error("a request at the short-context limit was not priced")
	}
}

func TestCorrectCostsPricesCodexWithoutChangingInputOrRecordedCosts(t *testing.T) {
	priced, unknown, recorded := codexRow(), codexRow(), codexRow()
	unknown["model"] = "gpt-unlisted"
	recorded["costUsd"] = 9.0
	out, status := CorrectCosts([]map[string]any{priced, unknown, recorded}, nil)
	if cost, ok := out[0]["costUsd"].(float64); !ok || math.Abs(cost-1.06) > 1e-9 || priced["costUsd"] != nil {
		t.Fatalf("priced %v, input %v", out[0]["costUsd"], priced["costUsd"])
	}
	if out[1]["costUsd"] != nil || out[2]["costUsd"] != 9.0 {
		t.Fatalf("unknown %v, recorded %v", out[1]["costUsd"], out[2]["costUsd"])
	}
	for i, s := range status {
		if s != "" {
			t.Fatalf("status[%d] = %q; Codex pricing is not a session correction", i, s)
		}
	}
}
