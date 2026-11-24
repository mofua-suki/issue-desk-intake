package statistics

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/usage"
)

func TestUsagePeriodBilling(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := Period{From: from, To: from.Add(24 * time.Hour)}
	rows := []map[string]any{
		{"at": "2026-11-01T00:00:00Z", "repo": "a", "step": "実装", "provider": "claude", "issue": float64(1), "generation": float64(1), "turn": float64(1), "billing": "subscription", "inputTokens": float64(10), "outputTokens": float64(2), "cacheReadTokens": float64(0), "cacheWriteTokens": float64(0), "costUsd": float64(4)},
		{"at": "2026-11-01T01:00:00Z", "repo": "a", "step": "実装", "provider": "claude", "issue": float64(1), "generation": float64(1), "turn": float64(2), "billing": "api_credit", "inputTokens": float64(0), "outputTokens": float64(0), "cacheReadTokens": float64(0), "cacheWriteTokens": float64(0), "costUsd": float64(7)},
		{"at": "2026-11-01T02:00:00Z", "repo": "a", "step": "実装", "provider": "claude", "inputTokens": float64(0)},
		{"at": "2026-11-01T03:00:00Z", "repo": "a", "step": "実装", "provider": "claude", "billing": nil},
		{"at": "2026-11-01T04:00:00Z", "repo": "b", "billing": "api_credit", "inputTokens": float64(999)},
		{"at": "2026-11-02T00:00:00Z", "repo": "a", "billing": "api_credit", "inputTokens": float64(999)},
		{"at": "2026-10-31T23:59:59Z", "repo": "a", "billing": "api_credit", "inputTokens": float64(999)},
	}
	corrected, status := usage.CorrectCosts(rows, nil)
	raw, err := json.Marshal(corrected)
	if err != nil {
		t.Fatal(err)
	}
	var before []map[string]any
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	s := Service{Summary: true, Weights: map[string]any{"default": map[string]any{"input": float64(2), "output": float64(3)}}}
	out := s.usagePeriod(corrected, status, p, "a")
	if !reflect.DeepEqual(corrected, before) {
		t.Fatalf("input changed: %#v", corrected)
	}
	groups := out["byBilling"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups: %v", groups)
	}
	sub := groups[0].(map[string]any)
	credit := groups[1].(map[string]any)
	check := func(label string, got map[string]any, expected map[string]any) {
		t.Helper()
		for k, v := range expected {
			if !reflect.DeepEqual(got[k], v) {
				t.Errorf("%s %s: got %v want %v", label, k, got[k], v)
			}
		}
	}
	check("subscription", sub, map[string]any{"billing": "subscription", "count": 3, "inputTokens": float64(10), "outputTokens": float64(2), "costUsd": float64(4), "weighted": float64(26)})
	check("subscription unknown", sub["unknown"].(map[string]any), map[string]any{"inputTokens": 1, "outputTokens": 2, "costUsd": 2, "weighted": 2})
	check("credit", credit, map[string]any{"billing": "api_credit", "count": 1, "inputTokens": float64(0), "weighted": float64(0), "costUsd": float64(3)})
	check("total", out["total"].(map[string]any), map[string]any{"count": 4, "inputTokens": float64(10), "outputTokens": float64(2), "costUsd": float64(7), "weighted": float64(26)})
	check("total unknown", out["total"].(map[string]any)["unknown"].(map[string]any), map[string]any{"inputTokens": 1, "outputTokens": 2, "costUsd": 2, "weighted": 2})
	for _, axis := range []string{"steps", "models"} {
		values := out[axis].([]any)
		if len(values) != 1 {
			t.Fatalf("%s: %v", axis, values)
		}
		check(axis, values[0].(map[string]any), map[string]any{"count": 4, "inputTokens": float64(10), "weighted": float64(26)})
	}
	empty := s.usagePeriod(corrected, status, Period{From: from.Add(48 * time.Hour), To: from.Add(72 * time.Hour)}, "a")
	if got := empty["byBilling"].([]any); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
	// A group containing only missing values must remain unknown, unlike known zero.
	missing := s.usagePeriod(corrected[3:4], status[3:4], p, "a")["byBilling"].([]any)[0].(map[string]any)
	check("missing", missing, map[string]any{"inputTokens": nil, "weighted": nil, "costUsd": nil})
	check("missing unknown", missing["unknown"].(map[string]any), map[string]any{"inputTokens": 1, "weighted": 1, "costUsd": 1})
}
