package statistics

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"
)

func normalized(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}
func TestReworkBreakdownKeepsPerIssueAccounting(t *testing.T) {
	raw, e := os.ReadFile("testdata/rework-accounting.json")
	if e != nil {
		t.Fatal(e)
	}
	var fixture struct {
		Input struct {
			Jobs, Events, Usage []AccountRecord
			Period, Weights     AccountRecord
			Skipped             int
		}
		Breakdown, Implementation AccountRecord
	}
	if e = json.Unmarshal(raw, &fixture); e != nil {
		t.Fatal(e)
	}
	input := fixture.Input
	got := BuildReworkBreakdown(input.Jobs, input.Events, input.Usage, input.Weights, input.Period, input.Skipped)
	if !reflect.DeepEqual(normalized(got), normalized(fixture.Breakdown)) {
		t.Fatal(jsonDifference(normalized(got), normalized(fixture.Breakdown), "breakdown"))
	}
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	period := Period{accountAt(input.Period["from"]), accountAt(input.Period["to"])}
	implementation := buildImplementationBreakdown(input.Jobs, input.Events, input.Usage, map[string]bool{}, period, now, input.Skipped)
	if !reflect.DeepEqual(normalized(implementation), normalized(fixture.Implementation)) {
		t.Fatal(jsonDifference(normalized(implementation), normalized(fixture.Implementation), "implementation"))
	}
}
func TestReworkPeriodUsesInclusiveJapanDates(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	_, p, e := ReworkQuery(url.Values{}, now, []string{"owner/repo"})
	if e != nil || p.From.In(JST).Format("2006-01-02") != "2026-10-19" || ISO(p.To) != "2026-11-01T15:00:00.000Z" {
		t.Fatalf("default %v %v", p, e)
	}
	for _, q := range []url.Values{{"from": {"2026-02-30"}}, {"from": {"2026-01-01"}, "to": {"2026-04-03"}}, {"project": {"other/repo"}}, {"from": {"2026-11-02"}, "to": {"2026-11-01"}}} {
		if _, _, e := ReworkQuery(q, now, []string{"owner/repo"}); e == nil {
			t.Fatalf("invalid period accepted %v", q)
		}
	}
}

func jsonDifference(got, want any, path string) string {
	if reflect.DeepEqual(got, want) {
		return ""
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return path + ": type mismatch"
		}
		for k, v := range w {
			if diff := jsonDifference(g[k], v, path+"."+k); diff != "" {
				return diff
			}
		}
		for k := range g {
			if _, has := w[k]; !has {
				return path + ": unexpected " + k
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Sprintf("%s: length got=%v want=%v", path, got, w)
		}
		for i, v := range w {
			if diff := jsonDifference(g[i], v, fmt.Sprintf("%s[%d]", path, i)); diff != "" {
				return diff
			}
		}
	}
	return fmt.Sprintf("%s: got=%v want=%v", path, got, want)
}
