package anomalies

import (
	"fmt"
	"issue-desk-intake/server/internal/usage"
	"net/url"
	"testing"
	"time"
)

var clock = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func row(at time.Time, issue int, step string, amount float64) map[string]any {
	return map[string]any{"at": iso(at), "repo": "o/r", "issue": float64(issue), "source": "worker", "host": "nas", "step": step, "provider": "codex", "model": "test", "inputTokens": amount, "cacheReadTokens": float64(0), "outputTokens": float64(0), "generation": float64(1)}
}
func find(hits []map[string]any, kind string) map[string]any {
	for _, h := range hits {
		if h["kind"] == kind {
			return h
		}
	}
	return nil
}
func TestAnomalyRangeAndPerRowThresholds(t *testing.T) {
	for _, q := range []url.Values{{"from": {"2026-11-01T12:00:00"}}, {"to": {""}}, {"from": {"2026-10-01T00:00:00Z"}}, {"from": {"2026-11-02T00:00:00Z"}}} {
		if _, _, e := Range(q, clock); e == nil {
			t.Fatal(q)
		}
	}
	from, to, e := Range(url.Values{"to": {"2026-11-01T21:00+09:00"}}, clock)
	if e != nil || !to.Equal(clock) || to.Sub(from) != 24*time.Hour {
		t.Fatal(from, to, e)
	}
	history := []map[string]any{}
	for i := 0; i < 3; i++ {
		history = append(history, row(clock.Add(time.Duration(i-3)*time.Hour), 1, "競合解消", 10))
	}
	hits := Basic(history, history, nil)
	if find(hits, "repeat") == nil {
		t.Fatal(hits)
	}
	history = nil
	for i := 0; i < 10; i++ {
		history = append(history, row(clock.Add(time.Duration(-i-1)*time.Hour), i+1, "実装", 10))
	}
	current := row(clock, 50, "実装", 30)
	hits = Basic([]map[string]any{current}, append(history, current), nil)
	if h := find(hits, "outlier"); h == nil || h["ratio"] != float64(3) {
		t.Fatal(hits)
	}
	history = nil
	for i := 0; i < 3; i++ {
		r := row(clock.Add(time.Duration(i-3)*time.Hour), 1, "実装", 10)
		r["outcome"] = "stalled"
		history = append(history, r)
	}
	if find(Basic(history, history, nil), "failure") == nil {
		t.Fatal("stalled streak missing")
	}
	history = []map[string]any{row(clock.Add(-2*time.Hour), 1, "実装", 10), row(clock, 1, "実装", 40)}
	if find(Basic(history[1:], history, nil), "hourly") == nil {
		t.Fatal("hourly boundary missing")
	}
}
func TestAggregateRatiosAndUnitTotals(t *testing.T) {
	history := []map[string]any{}
	for i := 0; i < 20; i++ {
		r := row(clock.Add(-48*time.Hour), i+1, "受付の相談", 10)
		r["source"], r["callTrigger"] = "intake", "initial"
		r["cacheReadTokens"], r["inputTokens"] = float64(90), float64(100)
		history = append(history, r)
	}
	current := []map[string]any{}
	for i := 0; i < 20; i++ {
		r := row(clock.Add(-time.Duration(i)*time.Minute), i+1, "受付の相談", 20)
		r["source"], r["callTrigger"] = "intake", "repair"
		r["cacheReadTokens"], r["inputTokens"] = float64(80), float64(100)
		current = append(current, r)
	}
	history = append(history, current...)
	if len(Cache(current, history)) != 1 || len(CallTriggers(current, history)) != 1 {
		t.Fatal("ratio boundary not detected")
	}
	units := []map[string]any{}
	for i := 0; i < 10; i++ {
		units = append(units, row(clock.Add(-8*24*time.Hour), i+1, "実装", 10))
	}
	current = nil
	for i := 0; i < 30; i++ {
		current = append(current, row(clock.Add(-time.Duration(i)*time.Minute), 100, "実装", 10))
	}
	hits := UnitTotals(current, append(units, current...), nil)
	if len(hits) != 1 || hits[0]["unit"] != "回" {
		t.Fatal(hits)
	}
	before := []map[string]any{}
	for i := 0; i < 20; i++ {
		r := row(clock.Add(-48*time.Hour), i+1, "実装", 1)
		r["provider"] = "claude"
		before = append(before, r)
	}
	if hits := ProviderShifts(current, append(before, current...)); len(hits) < 2 {
		t.Fatal(hits)
	}
}
func TestWeeklyMissingDraftAndCompletedJSTDays(t *testing.T) {
	history := []map[string]any{}
	end := midnight(clock)
	for i := 0; i < 10; i++ {
		r := row(end.Add(-8*24*time.Hour), i+1, "受付の相談", 1)
		r["source"], r["draft"] = "intake", fmt.Sprint(i)
		history = append(history, r)
		for j := 0; j < 3; j++ {
			r = row(end.Add(-24*time.Hour), i+1, "受付の相談", 1)
			r["source"], r["draft"] = "intake", fmt.Sprint(i)
			history = append(history, r)
		}
	}
	results := IntakeRepeats(history, clock, clock)
	if len(results) != 1 || results[0]["status"] != "hit" {
		t.Fatal(results)
	}
	history[0]["draft"] = nil
	results = IntakeRepeats(history, clock, clock)
	if results[0]["status"] != "held" {
		t.Fatal(results)
	}
	if empty := IntakeRepeats(nil, clock, clock); len(empty) != 1 || empty[0]["status"] != "held" {
		t.Fatal(empty)
	}
}
func TestLimitForecastAndFilingDenominators(t *testing.T) {
	reset := iso(clock.Add(6 * time.Hour))
	rows := []map[string]any{{"provider": "claude", "limit": "five_hour", "at": iso(clock.Add(-time.Hour)), "resetsAt": reset, "percent": float64(10)}, {"provider": "claude", "limit": "five_hour", "at": iso(clock), "resetsAt": reset, "percent": float64(60)}}
	forecast := LimitForecast(rows, clock, "")
	hit := map[string]any{}
	for _, r := range forecast {
		if r["provider"] == "claude" && r["limit"] == "five_hour" {
			hit = r
		}
	}
	if hit["status"] != "hit" || hit["ratePerHour"] != float64(50) {
		t.Fatal(forecast)
	}
	rows = append(rows, map[string]any{"at": iso(clock), "resetsAt": reset, "provider": "claude", "limit": "five_hour", "percent": nil})
	for _, r := range LimitForecast(rows, clock, "") {
		if r["provider"] == "claude" && r["limit"] == "five_hour" && r["status"] != "held" {
			t.Fatal(r)
		}
	}
	read := map[string]any{"before": map[string]any{"filing": map[string]any{"comparisonRequests": 1, "newIssuesInPeriod": 1}}, "after": map[string]any{"filing": map[string]any{"comparisonRequests": 2, "newIssuesInPeriod": 1}}}
	if r := FilingCheckResult("o/r", read, false, clock); r["status"] != "hit" {
		t.Fatal(r)
	}
	read["after"] = map[string]any{"filing": map[string]any{"comparisonRequests": 0, "newIssuesInPeriod": 0}}
	if r := FilingCheckResult("o/r", read, false, clock); r["status"] != "held" {
		t.Fatal(r)
	}
	if r := FilingCaches(nil, clock, nil); len(r) != 1 || r[0]["status"] != "held" {
		t.Fatal(r)
	}
}

func TestLimitForecastHoldsStaleRecord(t *testing.T) {
	for _, tc := range []struct {
		limit string
		age   time.Duration
	}{{"five_hour", time.Hour + time.Second}, {"seven_day", 6*time.Hour + time.Second}} {
		rows := []map[string]any{{"provider": "claude", "limit": tc.limit, "at": iso(clock.Add(-tc.age - time.Hour)), "resetsAt": iso(clock.Add(10 * time.Hour)), "percent": float64(10)}, {"provider": "claude", "limit": tc.limit, "at": iso(clock.Add(-tc.age)), "resetsAt": iso(clock.Add(10 * time.Hour)), "percent": float64(80)}}
		for _, row := range LimitForecast(rows, clock, "") {
			if row["provider"] == "claude" && row["limit"] == tc.limit {
				if row["status"] != "held" || row["projectedAt"] != nil {
					t.Fatal(row)
				}
				found := false
				for _, r := range row["reasons"].([]string) {
					if r == "stale_record" {
						found = true
					}
				}
				if !found {
					t.Fatal(row)
				}
			}
		}
	}
}

func TestNewIntakeStepCacheDetections(t *testing.T) {
	var history, current []map[string]any
	for i := 0; i < 20; i++ {
		r := row(clock.Add(-48*time.Hour), i, usage.DraftStep, 100)
		r["cacheReadTokens"] = float64(90)
		history = append(history, r)
		r = row(clock.Add(-time.Duration(i)*time.Minute), i, usage.DraftStep, 100)
		r["cacheReadTokens"] = float64(80)
		current = append(current, r)
	}
	hits := Cache(current, append(history, current...))
	if len(hits) != 1 || hits[0]["step"] != "受付の相談" {
		t.Fatal(hits)
	}
	var filing []map[string]any
	for i := 0; i < 30; i++ {
		for _, after := range []bool{false, true} {
			at := clock.Add(-48 * time.Hour)
			if after {
				at = clock.Add(-24 * time.Hour)
			}
			r := row(at, i, usage.FilingCheckStep, 10)
			r["provider"] = "claude"
			r["cacheWriteTokens"] = float64(0)
			r["promptTotalBytes"] = float64(100)
			r["outcome"] = "completed"
			r["promptCache"] = !after
			filing = append(filing, r)
		}
	}
	result := FilingCaches(filing, clock, nil)
	if result[0]["status"] != "hit" || result[0]["step"] != FilingCacheStep {
		t.Fatal(result)
	}
}

func TestIntakeRepeatCombinesRenamedSteps(t *testing.T) {
	end := midnight(clock)
	var history []map[string]any
	for i := 0; i < 10; i++ {
		old := row(end.Add(-8*24*time.Hour), i+1, "受付の相談", 1)
		old["source"], old["draft"] = "intake", fmt.Sprint(i)
		history = append(history, old)
		for j := 0; j < 3; j++ {
			current := row(end.Add(-24*time.Hour), i+1, usage.DraftStep, 1)
			current["source"], current["draft"] = "intake", fmt.Sprint(i)
			history = append(history, current)
		}
	}
	results := IntakeRepeats(history, clock, clock)
	if len(results) != 1 || results[0]["step"] != "受付の相談" || results[0]["status"] != "hit" {
		t.Fatal(results)
	}
	if p := results[0]["provisional"].(map[string]any); p["status"] != "hit" {
		t.Fatal(p)
	}
}
