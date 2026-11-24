package usage

import (
	"testing"
)

func TestWeightedUsageKeepsMissingAndProviderSemantics(t *testing.T) {
	rows := []map[string]any{{"provider": "codex", "inputTokens": 100.0, "cacheReadTokens": 40.0, "outputTokens": 2.0}, {"provider": "claude", "inputTokens": 100.0, "cacheReadTokens": 40.0, "cacheWriteTokens": 4.0, "outputTokens": 2.0}, {"provider": "codex", "inputTokens": 20.0, "cacheReadTokens": 30.0, "outputTokens": 0.0}}
	totals := Totals(rows, nil)
	if totals["weighted"] != float64(193) || totals["uncachedInputTokens"] != float64(160) || totals["normalizedCacheWriteTokens"] != float64(4) || totals["cacheWriteNotApplicable"] != 2 {
		t.Fatalf("totals %v", totals)
	}
	unknown := totals["unknown"].(map[string]any)
	if unknown["weighted"] != 1 || unknown["costUsd"] != 3 || totals["costUsd"] != nil {
		t.Fatalf("missing measurements %v", totals)
	}
	override := map[string]any{"providers": map[string]any{"codex": map[string]any{"models": map[string]any{"test": map[string]any{"output": 0.0}}}}}
	row := rows[0]
	row["model"] = "test"
	if Weighted(row, override)["weighted"] != 64.0 {
		t.Fatal("model weight ignored")
	}
}
