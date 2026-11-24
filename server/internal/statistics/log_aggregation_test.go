package statistics

import (
	"issue-desk-intake/server/internal/fleet"
	"testing"
	"time"
)

func TestSelectionLinksActualRunAndDetectsOmittedFailure(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	line := func(second int, body string) logLine {
		return logLine{base.Add(time.Duration(second) * time.Second), body}
	}
	lines := []logLine{line(0, "テスト選択 o/r#1: 選択 1/2ファイル（理由）; 省略 1ファイル; 基点 abc; 選択 a.test; 省略 b.test"), line(1, "工程開始 o/r#1 固定テスト"), line(2, "工程終了 o/r#1 固定テスト 1秒"), line(3, "マージ完了 o/r#1 https://gitea.invalid/pr"), line(8, "全部のテスト o/r abc: 失敗 2秒; 2ファイル; 失敗したファイル 1件 b.test")}
	steps := logSteps(lines, base.Add(10*time.Second), false)
	parsed := selectionParse(lines, steps)
	if len(parsed.Choices) != 1 || len(parsed.Fulls) != 1 || parsed.Kinds[steps[0]] != "chosen" || len(parsed.Fulls[0].Leaks["b.test"]) != 1 {
		t.Fatal(parsed)
	}
	lines = []logLine{line(0, "テスト選択 o/r#1: 全部 2ファイル; 理由 dynamic: file"), line(1, "テスト選択 o/r#1: 全部 2ファイル; 理由 configuration"), line(2, "工程開始 o/r#1 固定テスト")}
	parsed = selectionParse(lines, logSteps(lines, base.Add(10*time.Second), true))
	if !parsed.Choices[0].NotRun || parsed.Choices[1].NotRun {
		t.Fatal(parsed.Choices)
	}
}
func TestReviewNotesHandoffAndIncompleteTokens(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	lines := []logLine{{base, "起動 monitor"}, {base.Add(time.Second), "レビュー保留 o/r#1: 自動差し戻し 1/2"}, {base.Add(2 * time.Second), "マージ完了 o/r#1 url"}, {base.Add(3 * time.Second), "引き渡しを受け取りました o/r#2: other"}, {base.Add(4 * time.Second), "マージ完了 o/r#2 url"}, {base.Add(5 * time.Second), "注意点を添付 o/r#1 30 bytes"}}
	events := parseNotes(lines)
	report := notesPeriod(events, base, Period{base, base.Add(time.Minute)}, "o/r", nil, nil)
	if report["reviewed"] != 2 || report["local"] != 1 || report["otherHost"] != 1 || report["returns"] != 1 || fleet.Object(report["notes"])["tokens"] != float64(10) {
		t.Fatal(report)
	}
	avg := unitAverage([][]map[string]any{{{"provider": "claude", "inputTokens": float64(2), "outputTokens": float64(3)}}}, notesTokens)
	if avg["unknown"] != 1 || avg["average"] != nil {
		t.Fatal(avg)
	}
	if value := notesTokens(map[string]any{"provider": "codex", "inputTokens": float64(0), "outputTokens": float64(0)}); value != float64(0) {
		t.Fatal(value)
	}
}
