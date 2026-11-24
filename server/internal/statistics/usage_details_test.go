package statistics

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"reflect"
	"testing"
	"time"
)

func TestUsageDetailsKeepZeroUnknownAndUniqueFiles(t *testing.T) {
	rows := []map[string]any{{"source": "intake", "draft": "d", "repo": "o/r", "step": "受付の相談", "at": "2026-11-01T00:00:00.000Z", "provider": "codex", "calls": float64(0), "promptTotalBytes": float64(0), "promptBytes": map[string]any{"skill": float64(0)}, "inputTokens": float64(10), "cacheReadTokens": float64(8), "outputTokens": float64(0), "tools": map[string]any{}, "files": []any{map[string]any{"path": "a.go", "bytes": float64(0)}, map[string]any{"path": "a.go", "bytes": nil}}}, {"source": "worker", "repo": "o/r", "issue": float64(1), "step": "実装", "at": "2026-11-01T00:00:01.000Z"}}
	details := usageDetails(rows, rows)
	files := fleet.Object(details["files"])
	item := fleet.Object(files["items"].([]any)[0])
	if files["count"] != 1 || files["unknown"] != 1 || item["count"] != 1 || item["bytes"] != float64(0) || item["unknown"] != 1 {
		t.Fatal(details)
	}
	totals := detailTotals(rows)
	if totals["calls"] != float64(0) || fleet.Object(totals["detailUnknown"])["calls"] != 1 || totals["promptAverageBytes"] != float64(0) {
		t.Fatal(totals)
	}
	s := Service{DraftIssue: func(string) (string, int64) { return "o/r", 1 }}
	p := Period{time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	out := s.usagePeriod(rows, []string{"", ""}, p, "")
	if len(out["issues"].([]any)) != 1 || out["intakeCache"] == nil || out["drafts"] == nil || out["sessions"] == nil {
		t.Fatal(out)
	}
	uncached := fleet.Object(out["intakeCache"])["uncached"].(map[string]any)
	if uncached["ratio"] != float64(.2) {
		t.Fatal(uncached)
	}
}

func TestNewIntakeStepsMatchLegacyAggregates(t *testing.T) {
	p := Period{time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	makeRows := func(steps []string) []map[string]any {
		rows := []map[string]any{}
		for _, step := range steps {
			rows = append(rows, map[string]any{"source": "intake", "draft": "d", "repo": "o/r", "step": step, "at": "2026-11-01T01:00:00Z", "provider": "claude", "inputTokens": float64(10), "cacheReadTokens": float64(8), "cacheWriteTokens": float64(2), "outputTokens": float64(1), "promptBytes": map[string]any{"skill": float64(10)}, "sessionMode": "new", "callTrigger": "format_repair", "repairKind": "draft_format", "repairField": "title"})
		}
		return rows
	}
	old := makeRows([]string{"受付の相談", "代理の回答", "起票直前の重複と依存の確認"})
	newer := makeRows([]string{usage.DraftStep, usage.ProxyConfirmationStep, usage.FilingCheckStep})
	s := Service{}
	if !reflect.DeepEqual(usageDetails(old, old), usageDetails(newer, newer)) || !reflect.DeepEqual(intakeCacheSummary(old, p), intakeCacheSummary(newer, p)) || !reflect.DeepEqual(s.usageExtras(old), s.usageExtras(newer)) {
		t.Fatal("new steps differ from legacy")
	}
	extras := s.usageExtras(newer)
	if len(extras["drafts"].([]any)) != 3 || len(extras["sessions"].([]any)) != 1 || len(extras["repairs"].([]any)) != 1 || fleet.Object(usageDetails(newer, newer)["promptParts"])["count"] != 1 {
		t.Fatal(extras)
	}
}

func TestIntakePromptPartsValidationAndStatistics(t *testing.T) {
	parts := []string{"references", "notes", "contextEnvelope"}
	for _, part := range parts {
		for _, value := range []any{float64(12), float64(0), nil} {
			row := map[string]any{"host": "nas", "at": "2026-11-01T00:00:00.000Z", "source": "intake", "promptBytes": map[string]any{part: value, "skill": float64(3)}, "measurementMissingReasons": map[string]any{part: "input_unavailable"}}
			if !usage.ValidUsage(row) {
				t.Fatalf("invalid %s=%v", part, value)
			}
		}
		for _, value := range []any{float64(-1), "bad"} {
			row := map[string]any{"host": "nas", "at": "2026-11-01T00:00:00.000Z", "source": "intake", "promptBytes": map[string]any{part: value}}
			if usage.ValidUsage(row) {
				t.Fatalf("accepted %s=%v", part, value)
			}
		}
	}
	rows := []map[string]any{
		{"step": usage.ConsultationStep, "promptBytes": map[string]any{"skill": float64(10), "references": float64(12), "notes": float64(0), "contextEnvelope": nil}, "promptTotalBytes": float64(100)},
		{"step": usage.ConsultationStep, "promptBytes": map[string]any{"skill": float64(0), "references": float64(0), "notes": float64(8), "contextEnvelope": float64(6)}, "promptTotalBytes": float64(0)},
		{"step": usage.ConsultationStep, "promptBytes": map[string]any{"skill": nil, "references": nil, "notes": nil, "contextEnvelope": float64(0)}},
		{"step": usage.ConsultationStep, "promptBytes": map[string]any{"skill": float64(5)}},
		{"step": usage.ConsultationStep},
	}
	out := promptSummary(rows)
	if out["count"] != 4 || out["unknown"] != 1 {
		t.Fatal(out)
	}
	want := map[string]map[string]any{
		"references":      {"total": float64(12), "average": float64(6), "unknown": 2},
		"notes":           {"total": float64(8), "average": float64(4), "unknown": 2},
		"contextEnvelope": {"total": float64(6), "average": float64(3), "unknown": 2},
		"skill":           {"total": float64(15), "average": float64(5), "unknown": 1},
	}
	for _, raw := range out["parts"].([]any) {
		part := fleet.Object(raw)
		name := part["name"].(string)
		if expected, ok := want[name]; ok {
			for k, v := range expected {
				if part[k] != v {
					t.Fatalf("%s: %v", name, part)
				}
			}
			delete(want, name)
		}
	}
	if len(want) != 0 || !reflect.DeepEqual(out["promptTotalBytes"], map[string]any{"total": float64(100), "average": float64(50), "unknown": 2}) {
		t.Fatal(want, out)
	}
}

func TestInputBudgetPromptPartsOldAndNewRecords(t *testing.T) {
	size := func(n any) map[string]any { return map[string]any{"chars": n, "bytes": n} }
	record := func(sequence float64, parts map[string]any, total any) any {
		return map[string]any{"v": float64(1), "at": "2026-11-01T01:00:00Z", "sequence": sequence, "stage": "consultation", "phase": "pre_send", "transport": "codex", "mode": "full", "full": map[string]any{"total": size(total), "parts": parts}}
	}
	drafts := []map[string]any{{"id": "d", "inputBudget": map[string]any{"v": float64(1), "records": []any{
		record(1, map[string]any{"skill": size(float64(10)), "references": size(float64(12)), "notes": size(float64(0)), "contextEnvelope": size(nil)}, float64(100)),
		record(2, map[string]any{"skill": size(float64(0)), "references": size(float64(0)), "notes": size(float64(8)), "contextEnvelope": size(float64(6))}, float64(0)),
		record(3, map[string]any{"skill": size(nil), "references": size(nil), "notes": size(nil), "contextEnvelope": size(float64(0))}, nil),
		record(4, map[string]any{"skill": size(float64(5))}, nil),
	}}}}
	s := Service{Drafts: func(context.Context) ([]map[string]any, error) { return drafts, nil }}
	p := Period{time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	result, err := s.Budget(context.Background(), "inputBudget", Query{After: p})
	if err != nil {
		t.Fatal(err)
	}
	out := fleet.Object(fleet.Object(result)["after"])
	want := map[string]map[string]any{
		"references":      {"known": 2, "missing": 2, "knownTotal": float64(12), "mean": float64(6), "max": float64(12)},
		"notes":           {"known": 2, "missing": 2, "knownTotal": float64(8), "mean": float64(4), "max": float64(8)},
		"contextEnvelope": {"known": 2, "missing": 2, "knownTotal": float64(6), "mean": float64(3), "max": float64(6)},
		"skill":           {"known": 3, "missing": 1, "knownTotal": float64(15), "mean": float64(5), "max": float64(10)},
		"total":           {"known": 2, "missing": 2, "knownTotal": float64(100), "mean": float64(50), "max": float64(100)},
	}
	for _, raw := range out["groups"].([]any) {
		group := fleet.Object(raw)
		name := group["part"].(string)
		if group["input"] != "full" {
			continue
		}
		if expected, ok := want[name]; ok {
			if group["count"] != 4 || !reflect.DeepEqual(group["chars"], expected) || !reflect.DeepEqual(group["bytes"], expected) {
				t.Fatalf("%s: %v", name, group)
			}
			delete(want, name)
		}
	}
	if len(want) != 0 {
		t.Fatal(want)
	}
}

func TestAllPersistedPromptPartsAppearInStatistics(t *testing.T) {
	values := map[string]any{}
	for i, part := range usage.PromptPartNames() {
		values[part] = float64(i + 1)
	}
	row := map[string]any{"step": usage.ConsultationStep, "promptBytes": values}
	parts := promptSummary([]map[string]any{row})["parts"].([]any)
	if len(parts) != len(values) {
		t.Fatalf("statistics omitted prompt parts: %v", parts)
	}
	for _, raw := range parts {
		part := fleet.Object(raw)
		if part["total"] != values[fleet.Text(part["name"])] || part["unknown"] != 0 {
			t.Fatalf("prompt part was lost or counted as unknown: %v", part)
		}
	}
	measured := measurement(map[string]any{"parts": map[string]any{"schema": map[string]any{"bytes": float64(42), "chars": float64(21)}}})["parts"].(map[string]any)
	if len(measured) != len(values) || fleet.Object(measured["schema"])["bytes"] != float64(42) {
		t.Fatalf("input budget omitted prompt parts: %v", measured)
	}
}
