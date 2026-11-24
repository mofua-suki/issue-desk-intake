package jobs

import (
	"issue-desk-intake/server/internal/usage"
	"strings"
	"testing"
)

func TestUsageDecisionAllowsFirstRunWithoutRows(t *testing.T) {
	job := &Job{Repo: "o/r", Issue: 1, Generation: 0, View: map[string]any{}}
	if reason, _ := usageDecision(nil, 0, job, 8000000, map[string]any{}); reason != nil {
		t.Fatalf("一度も実装していない依頼は、使用量の行が無くても止めません: %v", reason)
	}
}

func TestUsageDecisionStillStopsLaterRunWithoutRows(t *testing.T) {
	job := &Job{Repo: "o/r", Issue: 1, Generation: 2, View: map[string]any{}}
	reason, _ := usageDecision(nil, 0, job, 8000000, map[string]any{})
	if text, _ := reason.(string); !strings.Contains(text, "行が届いていません") {
		t.Fatalf("2回目以降で行が1件も無いのは、実測の欠落の疑いとして止めます: %v", reason)
	}
}

func TestUsageDecisionCombinesNewAndLegacySteps(t *testing.T) {
	job := &Job{Repo: "o/r", Issue: 1}
	for _, pair := range [][2]string{{"受付の相談", usage.DraftStep}, {"代理の回答", usage.ProxyConfirmationStep}, {"起票直前の重複と依存の確認", usage.FilingCheckStep}} {
		rows := []map[string]any{{"repo": "o/r", "issue": float64(1), "step": pair[0], "provider": "codex", "inputTokens": float64(10), "cacheReadTokens": float64(0), "outputTokens": float64(0)}, {"repo": "o/r", "issue": float64(1), "step": pair[1], "usagePartial": true}}
		reason, estimate := usageDecision(rows, 0, job, 1000, nil)
		if reason != nil {
			t.Fatal(pair, reason, estimate)
		}
	}
}
