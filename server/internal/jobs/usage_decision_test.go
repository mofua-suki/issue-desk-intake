package jobs

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestUsageDecisionMissingRows(t *testing.T) {
	missing := func(step string) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "usagePartial": true}
	}
	measured := func(step string, tokens float64) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "provider": "codex", "inputTokens": tokens, "cacheReadTokens": float64(0), "outputTokens": float64(0)}
	}
	type expectedStep struct {
		step    string
		missing int
	}
	tests := []struct {
		name       string
		generation int64
		rows       []map[string]any
		skipped    int
		limit      float64
		reason     string
		missing    int
		steps      []expectedStep
		total      any
	}{
		{"初回の未補完", 0, []map[string]any{missing("実装")}, 0, 100, "", 1, []expectedStep{{"実装", 1}}, nil},
		{"2回目の未補完", 1, []map[string]any{missing("実装")}, 0, 100, "欠測を同じ工程の実測最大値で補完できません", 1, []expectedStep{{"実装", 1}}, nil},
		{"初回の一部補完で超過", 0, []map[string]any{missing("実装"), measured("独立レビュー", 60), missing("独立レビュー")}, 0, 100, "累計使用量（重み付き120）が上限", 2, []expectedStep{{"実装", 1}, {"独立レビュー", 1}}, nil},
		{"初回の実測超過", 0, []map[string]any{missing("実装"), measured("独立レビュー", 101)}, 0, 100, "累計使用量（重み付き101）が上限", 1, []expectedStep{{"実装", 1}}, nil},
		{"初回の実測が上限と同じ", 0, []map[string]any{missing("実装"), measured("独立レビュー", 100)}, 0, 100, "", 1, []expectedStep{{"実装", 1}}, nil},
		{"同じ工程の最大値で補完", 1, []map[string]any{measured("実装", 10), measured("実装", 20), missing("実装")}, 0, 100, "", 1, []expectedStep{{"実装", 1}}, float64(50)},
		{"補完後の上限超過", 0, []map[string]any{measured("実装", 60), missing("実装")}, 0, 100, "累計使用量（重み付き120）が上限", 1, []expectedStep{{"実装", 1}}, float64(120)},
		{"読めない行", 0, []map[string]any{missing("実装")}, 1, 100, "使用量データに読めない行があります", 0, nil, nil},
		{"工程不明も未補完", 0, []map[string]any{missing("その他")}, 0, 100, "", 1, []expectedStep{{"工程不明", 1}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Repo: "o/r", Issue: 1, Generation: tt.generation, View: map[string]any{}}
			reason, estimate := usageDecision(tt.rows, tt.skipped, job, tt.limit, nil)
			if tt.reason == "" && reason != nil || tt.reason != "" && !strings.Contains(reasonString(reason), tt.reason) {
				t.Fatalf("reason = %v, want %q", reason, tt.reason)
			}
			if tt.missing == 0 {
				if estimate != nil {
					t.Fatalf("estimate = %v, want nil", estimate)
				}
				return
			}
			e, ok := estimate.(map[string]any)
			if !ok || e["missingRows"] != tt.missing || e["total"] != tt.total {
				t.Fatalf("estimate = %#v", estimate)
			}
			steps, ok := e["steps"].([]any)
			if !ok || len(steps) != len(tt.steps) {
				t.Fatalf("steps = %#v, want %#v", e["steps"], tt.steps)
			}
			for i, expected := range tt.steps {
				step, ok := steps[i].(map[string]any)
				if !ok || step["step"] != expected.step || step["missingRows"] != expected.missing {
					t.Fatalf("step[%d] = %#v, want %+v", i, steps[i], expected)
				}

			}
		})
	}
}

func reasonString(reason any) string {
	text, _ := reason.(string)
	return text
}

func TestTallyUsageMatchesDecision(t *testing.T) {
	missing := func(step string) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "usagePartial": true}
	}
	measured := func(step string, tokens float64) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "provider": "codex", "inputTokens": tokens, "cacheReadTokens": float64(0), "outputTokens": float64(0)}
	}
	tests := []struct {
		name       string
		generation int64
		rows       []map[string]any
		skipped    int
		limit      float64
		missing    int
		estimated  float64
	}{
		{"初回の未補完", 0, []map[string]any{missing("実装")}, 0, 100, 1, 0},
		{"2回目の未補完", 1, []map[string]any{missing("実装")}, 0, 100, 1, 0},
		{"初回の一部補完で超過", 0, []map[string]any{missing("実装"), measured("独立レビュー", 60), missing("独立レビュー")}, 0, 100, 2, 60},
		{"初回の実測超過", 0, []map[string]any{missing("実装"), measured("独立レビュー", 101)}, 0, 100, 1, 0},
		{"初回の実測が上限と同じ", 0, []map[string]any{missing("実装"), measured("独立レビュー", 100)}, 0, 100, 1, 0},
		{"同じ工程の最大値で補完", 1, []map[string]any{measured("実装", 10), measured("実装", 20), missing("実装")}, 0, 100, 1, 20},
		{"補完後の上限超過", 0, []map[string]any{measured("実装", 60), missing("実装")}, 0, 100, 1, 60},
		{"読めない行", 0, []map[string]any{missing("実装")}, 1, 100, 0, 0},
		{"工程不明も未補完", 0, []map[string]any{missing("その他")}, 0, 100, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Repo: "o/r", Issue: 1, Generation: tt.generation, View: map[string]any{}}
			tally := TallyUsage(tt.rows, tt.skipped, job, nil)
			_, estimate := usageDecision(tt.rows, tt.skipped, job, tt.limit, nil)
			if tally.Estimated != tt.estimated {
				t.Fatalf("estimated = %v, want %v", tally.Estimated, tt.estimated)
			}
			if tally.Unreadable != (tt.skipped != 0) {
				t.Fatalf("unreadable = %v", tally.Unreadable)
			}
			if tt.skipped != 0 {
				if estimate != nil || tally.Rows != 1 || tally.Measured != 0 || tally.MissingRows != 1 || len(tally.Steps) != 1 || tally.Steps[0].Step != "実装" || tally.Steps[0].Missing != 1 || tally.Steps[0].Maximum != nil {
					t.Fatalf("estimate = %v, tally = %+v", estimate, tally)
				}
				return
			}
			if tally.MissingRows != tt.missing {
				t.Fatalf("missing = %d", tally.MissingRows)
			}
			if e, ok := estimate.(map[string]any); ok {
				if total, ok := e["total"].(float64); ok {
					if tally.Total != total || tally.Measured+tally.Estimated != total || len(tally.UnresolvedSteps) != 0 {
						t.Fatalf("tally = %+v, total = %v", tally, total)
					}
				} else if len(tally.UnresolvedSteps) == 0 {
					t.Fatalf("unresolved missing: %+v", tally)
				}
				parts := e["steps"].([]any)
				i := 0
				for _, step := range tally.Steps {
					if step.Missing == 0 {
						continue
					}
					p := parts[i].(map[string]any)
					i++
					if p["step"] != step.Step || p["missingRows"] != step.Missing || !sameMaximum(p["maximum"], step.Maximum) {
						t.Fatalf("step = %+v, estimate = %v", step, p)
					}
				}
			}
		})
	}
	job := &Job{Repo: "o/r", Issue: 1, View: map[string]any{}}
	tally := TallyUsage([]map[string]any{measured("実装", 20), {"repo": "other", "issue": float64(1), "usagePartial": true}}, 0, job, nil)
	if tally.Measured != 20 || tally.Estimated != 0 || tally.MissingRows != 0 || tally.Rows != 1 || len(tally.Steps) != 1 || tally.Steps[0].Maximum == nil || *tally.Steps[0].Maximum != 20 {
		t.Fatalf("complete tally = %+v", tally)
	}
}

// Rows is a count of readable matching rows, even when some input was unreadable.
func TestTallyUsageRowsWithUnreadableInput(t *testing.T) {
	job := &Job{Repo: "o/r", Issue: 1}
	rows := []map[string]any{
		{"repo": "o/r", "issue": float64(1), "step": "実装", "usagePartial": true},
		{"repo": "o/r", "issue": float64(1), "step": "実装", "provider": "codex", "inputTokens": float64(10), "cacheReadTokens": float64(0), "outputTokens": float64(0)},
		{"repo": "other", "issue": float64(1), "usagePartial": true},
	}
	for _, skipped := range []int{0, 2} {
		tally := TallyUsage(rows, skipped, job, nil)
		if tally.Rows != 2 || tally.Unreadable != (skipped > 0) || tally.MissingRows != 1 || tally.Estimated != 10 {
			t.Fatalf("skipped = %d, tally = %+v", skipped, tally)
		}
	}
}

func sameMaximum(value any, maximum *float64) bool {
	other, ok := value.(*float64)
	if !ok {
		return false
	}
	if other == nil || maximum == nil {
		return other == nil && maximum == nil
	}
	return *other == *maximum
}

func TestTallyUsageRoundingAndInfinity(t *testing.T) {
	job := &Job{Repo: "o/r", Issue: 1, Generation: 1, View: map[string]any{}}
	measured := func(step string, value float64) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "provider": "codex", "inputTokens": value, "cacheReadTokens": float64(0), "outputTokens": float64(0)}
	}
	missing := func(step string) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "usagePartial": true}
	}
	rows := []map[string]any{measured("実装", 0.1), missing("実装"), measured("独立レビュー", 0.2), missing("独立レビュー")}
	tally := TallyUsage(rows, 0, job, nil)
	_, estimate := usageDecision(rows, 0, job, 100, nil)
	if tally.Total != estimate.(map[string]any)["total"] {
		t.Fatalf("tally = %+v, estimate = %v", tally, estimate)
	}
	rows = []map[string]any{measured("実装", math.MaxFloat64), missing("実装")}
	tally = TallyUsage(rows, 0, job, nil)
	reason, estimate := usageDecision(rows, 0, job, math.MaxFloat64, nil)
	if !math.IsInf(tally.Total, 1) || reason == nil || estimate.(map[string]any)["total"] != nil {
		t.Fatalf("tally = %+v, reason = %v, estimate = %v", tally, reason, estimate)
	}
	rows = []map[string]any{measured("実装", math.MaxFloat64), missing("実装"), missing("実装")}
	_, estimate = usageDecision(rows, 0, job, math.MaxFloat64, nil)
	if _, err := json.Marshal(estimate); err != nil {
		t.Fatalf("estimate JSON: %v", err)
	}
	step := estimate.(map[string]any)["steps"].([]any)[0].(map[string]any)
	if step["estimated"] != nil || step["fallbackRows"] != 2 {
		t.Fatalf("overflow step = %#v", step)
	}

}

func TestUsageDecisionElapsedEstimates(t *testing.T) {
	job := &Job{Repo: "o/r", Issue: 1, Generation: 1, View: map[string]any{}}
	measured := func(value, elapsed float64) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": "実装", "provider": "codex", "inputTokens": value, "cacheReadTokens": float64(0), "outputTokens": float64(0), "elapsedMs": elapsed}
	}
	missing := func(elapsed any) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": "実装", "usagePartial": true, "elapsedMs": elapsed}
	}
	type expectedStep struct {
		step    string
		missing int
	}
	tests := []struct {
		name         string
		rows         []map[string]any
		weights      map[string]any
		limit        float64
		total        float64
		missingCount int
		steps        []expectedStep
		stop         bool
	}{
		{"実測2件と時間の異なる欠測4件", []map[string]any{measured(100, 100), measured(80, 40), missing(float64(1)), missing(float64(10)), missing(float64(50)), missing(float64(1000))}, nil, 450, 402, 4, []expectedStep{{"実装", 4}}, false},
		{"複数工程", []map[string]any{measured(100, 100), missing(float64(10)), {"repo": "o/r", "issue": float64(1), "step": "独立レビュー", "provider": "codex", "inputTokens": float64(20), "cacheReadTokens": float64(0), "outputTokens": float64(0), "elapsedMs": float64(100)}, {"repo": "o/r", "issue": float64(1), "step": "独立レビュー", "usagePartial": true, "elapsedMs": float64(50)}}, nil, 140, 140, 2, []expectedStep{{"実装", 1}, {"独立レビュー", 1}}, false},
		{"長時間は最大値に制限", []map[string]any{measured(100, 100), missing(float64(9007199254740991))}, nil, 199, 200, 1, []expectedStep{{"実装", 1}}, true},
		{"時間なし", []map[string]any{measured(100, 100), missing(nil)}, nil, 200, 200, 1, []expectedStep{{"実装", 1}}, false},
		{"実測時間なし", []map[string]any{measured(100, 0), missing(float64(1))}, nil, 200, 200, 1, []expectedStep{{"実装", 1}}, false},
		{"時間を持つ実測がゼロ", []map[string]any{measured(1000, 0), measured(0, 100), missing(float64(10000))}, nil, 1999, 2000, 1, []expectedStep{{"実装", 1}}, true},
		{"無効時間", []map[string]any{measured(100, 100), missing(float64(0)), missing(float64(-1)), missing(1.5), missing(math.Inf(1)), missing(float64(9007199254740992))}, nil, 600, 600, 5, []expectedStep{{"実装", 5}}, false},
		{"最大値ゼロ", []map[string]any{measured(0, 100), missing(float64(1))}, nil, 0, 0, 1, []expectedStep{{"実装", 1}}, false},
		{"重みを反映", []map[string]any{measured(100, 100), missing(float64(10))}, map[string]any{"default": map[string]any{"input": float64(2)}}, 220, 220, 1, []expectedStep{{"実装", 1}}, false},
		{"極小の正値", []map[string]any{measured(math.SmallestNonzeroFloat64, 9007199254740991), missing(float64(1))}, nil, 1, 2 * math.SmallestNonzeroFloat64, 1, []expectedStep{{"実装", 1}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, estimate := usageDecision(tt.rows, 0, job, tt.limit, tt.weights)
			if (reason != nil) != tt.stop {
				t.Fatalf("reason = %v", reason)
			}
			e := estimate.(map[string]any)
			if e["total"] != tt.total || e["missingRows"] != tt.missingCount {
				t.Fatalf("estimate = %#v, want total %v", e, tt.total)
			}
			steps := e["steps"].([]any)
			if len(steps) != len(tt.steps) {
				t.Fatalf("steps = %#v, want %#v", steps, tt.steps)
			}
			for i, expected := range tt.steps {
				step, ok := steps[i].(map[string]any)
				if !ok || step["step"] != expected.step || step["missingRows"] != expected.missing {
					t.Fatalf("step[%d] = %#v, want %+v", i, steps[i], expected)
				}
			}
		})
	}
}

func TestUsageDecisionConservativeEstimateBreakdown(t *testing.T) {
	job := &Job{Repo: "o/r", Issue: 1, Generation: 1, View: map[string]any{}}
	measured := func(step string, value, elapsed float64) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "provider": "codex", "inputTokens": value, "cacheReadTokens": float64(0), "outputTokens": float64(0), "elapsedMs": elapsed}
	}
	missing := func(step string, elapsed float64) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "usagePartial": true, "elapsedMs": elapsed}
	}
	rows := []map[string]any{
		measured("実装", 100, 0), measured("実装", 10, 100), missing("実装", 1),
		measured("独立レビュー", 20, 100), missing("独立レビュー", 10), missing("独立レビュー", 0),
		missing("競合解消", 10),
	}
	reason, raw := usageDecision(rows, 0, job, 1000, nil)
	if reason == nil {
		t.Fatal("unresolved step must stop")
	}
	estimate := raw.(map[string]any)
	steps := estimate["steps"].([]any)
	if estimate["total"] != nil || len(steps) != 3 {
		t.Fatalf("estimate = %#v", estimate)
	}
	for i, want := range []struct {
		step      string
		estimated any
		fallback  int
	}{{"実装", float64(100), 1}, {"独立レビュー", float64(22), 1}, {"競合解消", nil, 0}} {
		got := steps[i].(map[string]any)
		if got["step"] != want.step || got["estimated"] != want.estimated || got["fallbackRows"] != want.fallback {
			t.Fatalf("step = %#v, want %+v", got, want)
		}
	}
	// A large untimed measurement must affect the actual limit decision.
	reason, raw = usageDecision(rows[:3], 0, job, 200, nil)
	if reason == nil || raw.(map[string]any)["total"] != float64(210) {
		t.Fatalf("reason = %v estimate = %v", reason, raw)
	}
	// Even smaller untimed measurements trigger fallback when they are the majority.
	rows = []map[string]any{measured("実装", 100, 100), measured("実装", 10, 0), measured("実装", 10, 0), missing("実装", 1)}
	_, raw = usageDecision(rows, 0, job, 1000, nil)
	step := raw.(map[string]any)["steps"].([]any)[0].(map[string]any)
	if step["estimated"] != float64(100) || step["fallbackRows"] != 1 {
		t.Fatalf("majority: %#v", step)
	}
	// Even one smaller untimed measurement disables proportional estimation.
	_, raw = usageDecision([]map[string]any{rows[0], rows[1], rows[3]}, 0, job, 1000, nil)
	step = raw.(map[string]any)["steps"].([]any)[0].(map[string]any)
	if step["estimated"] != float64(100) || step["fallbackRows"] != 1 {
		t.Fatalf("tie: %#v", step)
	}
	_, raw = usageDecision([]map[string]any{rows[0], measured("実装", 50, 100), rows[1], rows[3]}, 0, job, 1000, nil)
	step = raw.(map[string]any)["steps"].([]any)[0].(map[string]any)
	if step["estimated"] != float64(100) || step["fallbackRows"] != 1 {
		t.Fatalf("minority: %#v", step)
	}
}

func TestUsageDecisionKeepsStepEstimatesSeparate(t *testing.T) {
	job := &Job{Repo: "o/r", Issue: 1, Generation: 1, View: map[string]any{}}
	measured := func(step string, value float64) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "provider": "codex", "inputTokens": value, "cacheReadTokens": float64(0), "outputTokens": float64(0), "elapsedMs": float64(100)}
	}
	missing := func(step string, elapsed float64) map[string]any {
		return map[string]any{"repo": "o/r", "issue": float64(1), "step": step, "usagePartial": true, "elapsedMs": elapsed}
	}
	reason, raw := usageDecision([]map[string]any{measured("実装", 100), missing("実装", 10), measured("独立レビュー", 20), missing("独立レビュー", 25)}, 0, job, 1000, nil)
	if reason != nil {
		t.Fatal(reason)
	}
	estimate := raw.(map[string]any)
	if estimate["total"] != float64(135) || estimate["missingRows"] != 2 {
		t.Fatalf("estimate: %v", estimate)
	}
	expected := map[string]struct{ maximum, estimated float64 }{"実装": {100, 10}, "独立レビュー": {20, 5}}
	steps := estimate["steps"].([]any)
	if len(steps) != len(expected) {
		t.Fatalf("steps: %v", steps)
	}
	for _, rawStep := range steps {
		step := rawStep.(map[string]any)
		name := step["step"].(string)
		want, ok := expected[name]
		if !ok || step["missingRows"] != 1 || !sameMaximum(step["maximum"], &want.maximum) || step["estimated"] != want.estimated {
			t.Fatalf("step-specific estimate mixed: %v", step)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		t.Fatalf("steps missing: %v", expected)
	}
}
