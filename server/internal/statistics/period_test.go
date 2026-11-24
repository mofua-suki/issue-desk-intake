package statistics

import (
	"net/url"
	"testing"
	"time"
)

func TestPeriodsUseJapanDatesAndRejectAmbiguousRanges(t *testing.T) {
	now := time.Date(2026, 11, 1, 22, 0, 0, 0, time.UTC)
	q, err := Parse(url.Values{}, now, nil)
	if err != nil || ISO(q.After.To) != "2026-11-02T15:00:00.000Z" || q.After.To.Sub(q.After.From) != 7*24*time.Hour {
		t.Fatalf("default %v %v", q, err)
	}
	for _, v := range []url.Values{{"beforeFrom": {"2026-01-01"}}, {"project": {"missing"}}, {"beforeFrom": {"2026-02-30"}, "beforeTo": {"2026-03-01"}, "afterFrom": {"2026-03-01"}, "afterTo": {"2026-03-02"}}, {"beforeFrom": {"2026-01-01"}, "beforeTo": {"2026-08-01"}, "afterFrom": {"2026-08-01"}, "afterTo": {"2026-08-02"}}} {
		if _, err := Parse(v, now, nil); err == nil {
			t.Fatalf("invalid range accepted %v", v)
		}
	}
}
func TestParallelWorkUsesIntervalsAndDoesNotMeasureBeforeRecords(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r := records{Jobs: map[int64]job{1: {ID: 1, Filed: start}, 2: {ID: 2, Filed: start}}, Oldest: start, End: start.Add(time.Hour), Events: []event{{Job: 1, Kind: "leased", At: start}, {Job: 2, Kind: "leased", At: start.Add(10 * time.Minute)}, {Job: 1, Kind: "state", At: start.Add(20 * time.Minute), Data: map[string]any{"to": "merged"}}, {Job: 2, Kind: "state", At: start.Add(30 * time.Minute), Data: map[string]any{"to": "failed"}}}}
	p := workPeriod(r, Period{start.Add(-time.Hour), start.Add(time.Hour)})
	if p["activeMs"] != int64(30*time.Minute/time.Millisecond) || p["parallelMs"] != int64(10*time.Minute/time.Millisecond) || p["completed"] != 1 || p["maxConcurrent"] != 2 || p["measuredMs"] != int64(time.Hour/time.Millisecond) {
		t.Fatalf("intervals %v", p)
	}
	if value := workPeriod(r, Period{start.Add(-2 * time.Hour), start.Add(-time.Hour)}); value["measured"] != nil {
		t.Fatal("unobserved period reported measured")
	}
}
