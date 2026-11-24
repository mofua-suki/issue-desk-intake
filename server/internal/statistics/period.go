package statistics

import (
	"issue-desk-intake/server/internal/contract"
	"net/url"
	"time"
)

var JST = time.FixedZone("JST", 9*3600)

type Period struct{ From, To time.Time }

// Contains tells whether t is within the period, From inclusive and To exclusive.
func (p Period) Contains(t time.Time) bool { return !t.Before(p.From) && t.Before(p.To) }

type Query struct {
	Project       string
	Before, After Period
}

func Parse(values url.Values, now time.Time, repos []string) (Query, error) {
	q := Query{Project: values.Get("project")}
	registered := q.Project == ""
	for _, repo := range repos {
		if repo == q.Project {
			registered = true
		}
	}
	if !registered {
		return q, contract.NewProblem(400, "プロジェクトの指定が不正です。", "")
	}
	keys := []string{"beforeFrom", "beforeTo", "afterFrom", "afterTo"}
	given := 0
	for _, key := range keys {
		if values.Get(key) != "" {
			given++
		}
	}
	if given == 0 {
		day := now.In(JST)
		today := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, JST)
		q.After = Period{today.Add(-6 * 24 * time.Hour), today.Add(24 * time.Hour)}
		q.Before = Period{q.After.From.Add(-7 * 24 * time.Hour), q.After.From}
		return q, nil
	}
	if given != 4 {
		return q, contract.NewProblem(400, "前後の期間をすべて指定してください。", "")
	}
	times := []time.Time{}
	for i, key := range keys {
		v := values.Get(key)
		layout := "2006-01-02"
		if len(v) == 16 {
			layout = "2006-01-02T15:04"
		}
		at, err := time.ParseInLocation(layout, v, JST)
		if err != nil || at.Format(layout) != v {
			return q, contract.NewProblem(400, "日付の形式が不正です。", "")
		}
		if i%2 == 1 && len(v) == 10 {
			at = at.Add(24 * time.Hour)
		}
		times = append(times, at)
	}
	q.Before, q.After = Period{times[0], times[1]}, Period{times[2], times[3]}
	for _, p := range []Period{q.Before, q.After} {
		if !p.From.Before(p.To) || p.To.Sub(p.From) > 92*24*time.Hour {
			return q, contract.NewProblem(400, "期間は開始より後、92日以内です。", "")
		}
	}
	return q, nil
}
func ISO(v time.Time) string { return contract.ISO(v) }
func (p Period) Dates() map[string]any {
	return map[string]any{"from": ISO(p.From), "to": ISO(p.To), "fromDate": p.From.In(JST).Format("2006-01-02"), "toDate": p.To.Add(-time.Millisecond).In(JST).Format("2006-01-02"), "fromAt": p.From.In(JST).Format("2006-01-02T15:04"), "toAt": p.To.In(JST).Format("2006-01-02T15:04")}
}
