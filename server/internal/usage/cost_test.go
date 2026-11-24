package usage

import "testing"

func TestCostCorrectionNeedsMatchingTurnAndPreservesResets(t *testing.T) {
	row := func(turn, cost float64) map[string]any {
		return map[string]any{"provider": "claude", "step": "実装", "repo": "owner/repo", "issue": 1.0, "generation": 1.0, "turn": turn, "costUsd": cost}
	}
	first, second, reset, missing := row(1, 4), row(2, 7), row(3, 1), row(4, 0)
	reset["attempt"] = 2.0
	missing["costUsd"] = nil
	out, status := CorrectCosts([]map[string]any{first, second, reset, missing}, nil)
	if out[1]["costUsd"] != float64(3) || status[1] != "corrected" || out[2]["costUsd"] != float64(1) || status[2] != "" || status[3] != "uncorrectable" || second["costUsd"] != float64(7) {
		t.Fatalf("costs %v %v", out, status)
	}
	start := row(1, 0)
	start["costUsd"] = nil
	start["sessionCostUsd"] = 2.0
	out, status = CorrectCosts([]map[string]any{start}, []map[string]any{})
	if out[0]["costUsd"] != float64(2) || status[0] != "filled" {
		t.Fatal("first session not filled")
	}
	out, status = CorrectCosts([]map[string]any{start}, []map[string]any{first})
	if out[0]["costUsd"] != nil || status[0] != "" {
		t.Fatal("double counted resumed session")
	}
}
