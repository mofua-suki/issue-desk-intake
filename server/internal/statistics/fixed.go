package statistics

import (
	"context"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"sort"
	"time"
)

func percentile(values []float64, p float64) any {
	if len(values) == 0 {
		return nil
	}
	sort.Float64s(values)
	if p == 0.5 {
		mid := len(values) / 2
		if len(values)%2 == 1 {
			return values[mid]
		}
		return (values[mid-1] + values[mid]) / 2
	}
	return values[int(math.Ceil(float64(len(values))*p))-1]
}
func (s *Service) FixedTests(ctx context.Context, q Query) (any, error) {
	to := q.Before.To
	if q.After.To.After(to) {
		to = q.After.To
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT json FROM fixed_test_usage WHERE at<$1 ORDER BY at,key", to)
	if err != nil {
		return nil, err
	}
	all, skipped, err := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{SkipInvalid: true})
	if err != nil {
		return nil, err
	}
	return map[string]any{"project": q.Project, "reason": nil, "skipped": skipped, "before": fixedTestPeriod(all, q.Project, q.Before), "after": fixedTestPeriod(all, q.Project, q.After)}, nil
}

// fixedTestPeriod summarizes the period's fixed test runs by host, repository and scope.
func fixedTestPeriod(all []map[string]any, project string, p Period) any {
	mine := []map[string]any{}
	for _, row := range all {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(row["startedAt"]))
		if e == nil && !at.Before(p.From) && at.Before(p.To) && (project == "" || row["repo"] == project) && (row["scope"] == "full" || row["scope"] == "selected" || row["scope"] == "early") {
			mine = append(mine, row)
		}
	}
	out := p.Dates()
	summaries := []any{}
	for _, list := range groupRows(mine, "host", "repo", "scope") {
		summaries = append(summaries, fixedTestSummary(list))
	}
	out["count"], out["rows"] = len(mine), summaries
	return out
}

// fixedTestSummary describes one host, repository and scope by how many
// implementations ran when each test started.
func fixedTestSummary(list []map[string]any) map[string]any {
	base := list[0]
	unknown, zero, unknownLoad := 0, 0, 0
	for _, row := range list {
		if row["implementingAtStart"] == nil {
			unknown++
		} else if row["implementingAtStart"] == 0.0 {
			zero++
		}
		if row["loadavg1AtStart"] == nil {
			unknownLoad++
		}
	}
	groups := []any{}
	for _, name := range []string{"1", "2-3", "4+"} {
		groups = append(groups, fixedTestLoadGroup(list, name))
	}
	return map[string]any{"host": base["host"], "repo": base["repo"], "scope": base["scope"], "unknownImplementing": unknown, "zeroImplementing": zero, "unknownLoad": unknownLoad, "groups": groups}
}

// fixedTestLoadGroup measures the runs started while the named number of implementations ran.
func fixedTestLoadGroup(list []map[string]any, name string) map[string]any {
	elapsed, percent, load := []float64{}, []float64{}, []float64{}
	cut := 0
	for _, row := range list {
		n := fleet.Number(row["implementingAtStart"])
		bucket := ""
		if n == 1 {
			bucket = "1"
		} else if n >= 2 && n < 4 {
			bucket = "2-3"
		} else if n >= 4 {
			bucket = "4+"
		}
		if bucket != name {
			continue
		}
		elapsed = append(elapsed, fleet.Number(row["elapsedMs"]))
		percent = append(percent, fleet.Number(row["percent"]))
		if n, ok := row["loadavg1AtStart"].(float64); ok {
			load = append(load, n)
		}
		if row["status"] == "cut" {
			cut++
		}
	}
	var count, cutCount any
	if len(elapsed) > 0 {
		count = len(elapsed)
		cutCount = cut
	}
	return map[string]any{"group": name, "count": count, "elapsedMsMedian": percentile(elapsed, 0.5), "elapsedMsP90": percentile(elapsed, 0.9), "percentMedian": percentile(percent, 0.5), "cutCount": cutCount, "loadavg1Median": percentile(load, 0.5)}
}
