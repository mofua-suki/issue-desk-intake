package statistics

import (
	"issue-desk-intake/server/internal/fleet"
	"testing"
	"time"
)

func TestFleetCoverageExcludesUnknownAndRestartGap(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	l := func(second int, body string) logLine {
		return logLine{start.Add(time.Duration(second) * time.Second), body}
	}
	lines := []logLine{l(0, "起動 test"), l(3, "止まりを検知しました: 進まない占有 nas o/r#1（stuck_claim・不具合の疑い）: reason"), l(4, "last"), l(7, "起動 restart"), l(8, "止まりが解消しました: 進まない占有 nas o/r#1（stuck_claim・不具合の疑い）: reason"), l(9, "マージ完了 o/r#1 https://gitea.invalid/pr")}
	src := parseStalls(lines, start.Add(10*time.Second))
	events := []fleetSegment{{start.Add(2 * time.Second), start.Add(6 * time.Second), map[string]int{"nas": 1, "other": 1}}, {start.Add(7 * time.Second), start.Add(10 * time.Second), map[string]int{"nas": 1}}}
	src.Segments = intersectStallSegments(src.Segments, events)
	value := src.measure(Period{start, start.Add(10 * time.Second)}, "o/r")
	if value["recordedMs"] != float64(5000) || fleet.Object(value["parallel"])["parallelMs"] != float64(2000) || fleet.Object(value["stalls"])["stalledMs"] != float64(2000) || fleet.Object(value["stalls"])["total"] != 1 || value["merged"] != 1 {
		t.Fatal(value)
	}
	if v := src.measure(Period{start.Add(4 * time.Second), start.Add(7 * time.Second)}, ""); v != nil {
		t.Fatal(v)
	}
}

func TestStallsLogMetricsRegression(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	l := func(n int, body string) logLine { return logLine{start.Add(time.Duration(n) * time.Minute), body} }
	src := parseStalls([]logLine{
		l(0, "起動 test"),
		l(1, "止まりを検知しました: 進まない占有 nas o/r#1（stuck_claim・不具合の疑い）: reason"),
		l(2, "進まない占有に外す印を付けました o/r#1（nas）: reason"),
		l(3, "外す印の付いた占有が外れました o/r#1（nas）"),
		l(4, "止まりが解消しました: 進まない占有 nas o/r#1（stuck_claim・不具合の疑い）: reason"),
		l(5, "マージ完了 o/r#1 https://gitea.invalid/pr"),
		l(6, "マージ完了 o/r#2 https://gitea.invalid/pr"),
		l(6, "止まりを検知しました: 進まない占有 nas o/r#2（stuck_claim・不具合の疑い）: reason"),
		l(9, "起動 restart"), l(10, "last"),
	}, start.Add(11*time.Minute))
	src.Segments = intersectStallSegments(src.Segments, []fleetSegment{{start, start.Add(11 * time.Minute), map[string]int{"nas": 1}}})
	got := src.measure(Period{start, start.Add(11 * time.Minute)}, "o/r")
	stalls := fleet.Object(got["stalls"])
	if got["recordedMs"] != float64(8*60000) || stalls["total"] != 2 || stalls["suspected"] != 2 || stalls["stalledMs"] != float64(5*60000) || got["marked"] != 1 || got["released"] != 1 || got["merged"] != 2 {
		t.Fatal(got)
	}
}
