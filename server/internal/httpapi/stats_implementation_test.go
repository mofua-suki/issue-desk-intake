package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStatsImplementationSelectionDetailsAndFailure(t *testing.T) {
	now := time.Now()
	handler, db, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.Now = func() time.Time { return now }
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { return []map[string]any{}, nil }
	})
	for _, tc := range []struct {
		query             string
		selected, details bool
	}{
		{"tables=implementation", true, false}, {"tables=implementation&details=true", true, true}, {"tables=usage", false, false},
	} {
		status, result := workerRequest(t, handler, token, "GET", "/api/stats?"+tc.query, "")
		if status != 200 {
			t.Fatalf("%s status=%d %v", tc.query, status, result)
		}
		value, exists := result["implementation"]
		if exists != tc.selected {
			t.Fatalf("%s selected=%v: %v", tc.query, exists, result)
		}
		if tc.selected {
			impl, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("%s unavailable: %v", tc.query, result)
			}
			for _, period := range []string{"before", "after"} {
				_, exists = impl[period].(map[string]any)["issues"]
				if exists != tc.details {
					t.Fatalf("%s %s issues=%v", tc.query, period, exists)
				}
			}
		}
	}
	if _, err := db.Exec(`DROP TABLE usage_rows`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(60 * time.Second)
	status, result := workerRequest(t, handler, token, "GET", "/api/stats?tables=implementation&details=true", "")
	if status != 200 || result["implementation"] != nil {
		t.Fatalf("failure: %d %v", status, result)
	}
	unavailable, ok := result["unavailable"].([]any)
	if !ok || len(unavailable) != 1 || !strings.Contains(unavailable[0].(string), "implementation") {
		t.Fatalf("unavailable: %v", result)
	}
}
