package statistics

import (
	"testing"
	"time"
)

func TestIntakeTimingPeriods(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	row := func(day int, kind string, elapsed, first any) map[string]any {
		r := map[string]any{"at": at.Add(time.Duration(day) * 24 * time.Hour).Format(time.RFC3339), "source": "intake", "step": "consultation", "provider": "codex", "model": "m", "effort": "high", "tier": "heavy", "elapsedMs": elapsed, "firstOutputMs": first}
		if kind != "" {
			r["diagnosticKind"] = kind
		}
		return r
	}
	rows := []map[string]any{row(0, "completed", float64(0), float64(0)), row(0, "completed", float64(10), float64(20)), row(0, "completed", float64(20), nil), row(0, "timeout_idle", float64(40), nil), row(0, "execution", nil, nil), row(0, "", nil, nil), row(1, "completed", float64(2), float64(4))}
	different := row(0, "completed", float64(8), float64(0))
	different["provider"], different["model"], different["effort"], different["tier"], different["step"] = "claude", "other", "low", "light", "other"
	rows = append(rows, different)
	rows = append(rows, map[string]any{"at": at.Format(time.RFC3339), "source": "worker"})
	statuses := make([]string, len(rows))
	s := &Service{Summary: true}
	before := s.usagePeriod(rows, statuses, Period{From: at, To: at.Add(24 * time.Hour)}, "")
	after := s.usagePeriod(rows, statuses, Period{From: at.Add(24 * time.Hour), To: at.Add(48 * time.Hour)}, "")
	groups := before["intakeTiming"].([]any)
	if len(groups) != 5 {
		t.Fatalf("groups: %#v", groups)
	}
	found := map[string]map[string]any{}
	for _, raw := range groups {
		g := raw.(map[string]any)
		k, _ := g["diagnosticKind"].(string)
		if g["provider"] == "codex" {
			found[k] = g
		}
	}
	completed := found["completed"]
	if completed["count"] != 3 || completed["provider"] != "codex" || completed["tier"] != "heavy" || completed["effort"] != "high" || completed["model"] != "m" {
		t.Fatal(completed)
	}
	elapsed := completed["elapsedMs"].(map[string]any)
	first := completed["firstOutputMs"].(map[string]any)
	if elapsed["median"] != float64(10) || elapsed["p90"] != float64(20) || first["median"] != float64(10) || first["p90"] != float64(20) || first["missing"] != 1 {
		t.Fatal(elapsed, first)
	}
	for _, k := range []string{"timeout_idle", "execution", ""} {
		if found[k] == nil || found[k]["count"] != 1 || found[k]["firstOutputMs"].(map[string]any)["median"] != nil {
			t.Fatal(k, found[k])
		}
	}
	if len(after["intakeTiming"].([]any)) != 1 || after["intakeTiming"].([]any)[0].(map[string]any)["count"] != 1 {
		t.Fatal(after)
	}
	if before["total"].(map[string]any)["count"] != 8 {
		t.Fatal(before["total"])
	}
}
