package statistics

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func filingEvent(id, kind, at string) map[string]any {
	return map[string]any{"v": 1, "eventId": id, "at": at, "repo": "owner/repo", "draft": "private-draft", "parent": nil, "child": nil, "attemptId": "attempt", "comparisonId": "comparison", "revision": nil, "trigger": "automatic_recovery", "kind": kind, "outcome": nil, "issue": nil, "issueAt": nil, "bytes": nil}
}
func TestFilingCheckCompletenessAndPrivacy(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	events := []map[string]any{filingEvent("request", "compare_requested", "2026-11-01T10:00:00.000Z"), filingEvent("input", "compare_input", "2026-11-01T10:01:00.000Z"), filingEvent("request", "compare_requested", "2026-11-01T10:00:00.000Z")}
	bytes := map[string]any{}
	for _, name := range filingByteNames {
		bytes[name] = nil
	}
	bytes["jobs"] = float64(0)
	events[1]["bytes"] = bytes
	content := `{"v":1,"startedAt":"2026-10-31T00:00:00.000Z","completeSince":"2026-10-31T00:00:00.000Z"}` + "\n"
	for _, e := range events {
		raw, _ := json.Marshal(e)
		content += "filing-check-observation " + string(raw) + "\n"
	}
	if e := os.WriteFile(filepath.Join(dir, "filing-check-events.jsonl"), []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
	s := Service{RuntimeDir: dir, Now: func() time.Time { return now }}
	out, e := s.FilingCheck(context.Background(), Query{Before: Period{now.Add(-72 * time.Hour), now.Add(-48 * time.Hour)}, After: Period{now.Add(-12 * time.Hour), now}})
	if e != nil {
		t.Fatal(e)
	}
	report := out.(map[string]any)
	if report["before"] != nil || report["reasons"].(map[string]any)["before"] != "収集開始前の期間を含みます" {
		t.Fatal(report)
	}
	after := report["after"].(map[string]any)
	d := after["diagnostics"].(map[string]int)
	if d["duplicates"] != 1 || d["startsOnly"] != 1 || d["missingInputs"] != 0 {
		t.Fatal(d)
	}
	b := after["bytes"].(map[string]any)
	if b["jobs"].(map[string]any)["total"] != float64(0) || b["promptTotal"].(map[string]any)["total"] != nil || b["promptTotal"].(map[string]any)["unknown"] != 1 {
		t.Fatal(b)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "private-draft") || strings.Contains(string(raw), "comparison") {
		if strings.Contains(string(raw), `"comparisonId"`) || strings.Contains(string(raw), "private-draft") {
			t.Fatal(string(raw))
		}
	}
}
