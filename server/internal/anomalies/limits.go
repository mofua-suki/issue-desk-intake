package anomalies

import (
	"issue-desk-intake/server/internal/fleet"
	"math"
	"sort"
	"time"
)

func LimitTimeline(rows []map[string]any) map[string]any {
	windows := map[string]map[string]any{}
	unknownPercent, unknownResets := 0, 0
	for _, r := range rows {
		percent, known := number(r["percent"])
		if !known {
			unknownPercent++
		}
		reset := fleet.Text(r["resetsAt"])
		if reset == "" {
			unknownResets++
			continue
		}
		provider := text(r["provider"], "claude")
		if provider == "" {
			provider = "claude"
		}
		limit := r["limit"]
		key := provider + "\x00" + text(limit, "") + "\x00" + reset
		w := windows[key]
		if w == nil {
			w = map[string]any{"provider": provider, "limit": limit, "resetsAt": reset, "records": 0, "first": nil, "last": nil}
			windows[key] = w
		}
		w["records"] = w["records"].(int) + 1
		if !known {
			continue
		}
		point := map[string]any{"at": r["at"], "percent": percent}
		first, last := fleet.Object(w["first"]), fleet.Object(w["last"])
		if first == nil || at(r).Before(at(first)) {
			w["first"] = point
		}
		if last == nil || !at(r).Before(at(last)) {
			w["last"] = point
		}
	}
	keys := []string{}
	for key := range windows {
		keys = append(keys, key)
	}
	order := func(provider string) int {
		if provider == "claude" {
			return 0
		}
		if provider == "codex" {
			return 1
		}
		return 2
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := windows[keys[i]], windows[keys[j]]
		ap, bp := order(text(a["provider"], "")), order(text(b["provider"], ""))
		if ap != bp {
			return ap < bp
		}
		if a["provider"] != b["provider"] {
			return text(a["provider"], "") < text(b["provider"], "")
		}
		if (a["limit"] == nil) != (b["limit"] == nil) {
			return a["limit"] != nil
		}
		return keys[i] < keys[j]
	})
	out := []any{}
	for _, key := range keys {
		out = append(out, windows[key])
	}
	return map[string]any{"rows": out, "unknownPercent": unknownPercent, "unknownResets": unknownResets}
}
func LimitForecast(rows []map[string]any, to time.Time, readReason string) []map[string]any {
	recent := []map[string]any{}
	for _, r := range rows {
		if inWindow(r, to.Add(-8*24*time.Hour), to) {
			recent = append(recent, r)
		}
	}
	windows := LimitTimeline(recent)["rows"].([]any)
	out := []map[string]any{}
	for _, provider := range []string{"claude", "codex"} {
		for _, limit := range []string{"seven_day", "five_hour"} {
			window := activeLimitWindow(windows, recent, provider, limit, to)
			first, last := fleet.Object(window["first"]), fleet.Object(window["last"])
			reasons := limitForecastHolds(recent, window, provider, limit, to, readReason)
			rate, measured, threshold, projectedAt, reasons := projectLimit(window, limit, reasons)
			status := "normal"
			if len(reasons) > 0 {
				status = "held"
			} else if measured.(float64) < threshold.(float64) {
				status = "hit"
			}
			out = append(out, map[string]any{"kind": "limit-forecast", "kindName": names["limit-forecast"], "provider": provider, "limit": limit, "resetsAt": window["resetsAt"], "first": nilMap(first), "last": nilMap(last), "ratePerHour": rate, "projectedAt": projectedAt, "measured": measured, "threshold": threshold, "status": status, "reasons": reasons, "causeKey": "limit-forecast:" + provider + ":" + limit, "at": iso(to), "repo": nil, "issue": nil, "step": nil, "unit": "時間", "rule": "同じリセット窓の使用率の増加ペースで100%に達する時刻がリセットより前"})
		}
	}
	return out
}

// activeLimitWindow picks the provider's unexpired window for the limit that
// was observed most recently.
func activeLimitWindow(windows []any, recent []map[string]any, provider, limit string, to time.Time) map[string]any {
	var window map[string]any
	var latestAt time.Time
	for _, raw := range windows {
		w := fleet.Object(raw)
		reset, e := time.Parse(time.RFC3339Nano, fleet.Text(w["resetsAt"]))
		if w["provider"] != provider || w["limit"] != limit || e != nil || !reset.After(to) {
			continue
		}
		var observed time.Time
		for _, r := range recent {
			p := text(r["provider"], "claude")
			if p == "" {
				p = "claude"
			}
			if p == provider && r["limit"] == limit && r["resetsAt"] == w["resetsAt"] && at(r).After(observed) {
				observed = at(r)
			}
		}
		if window == nil || observed.After(latestAt) || observed.Equal(latestAt) && fleet.Text(w["resetsAt"]) > fleet.Text(window["resetsAt"]) {
			window, latestAt = w, observed
		}
	}
	return window
}

// limitForecastHolds lists why a forecast cannot be trusted for the window.
func limitForecastHolds(recent []map[string]any, window map[string]any, provider, limit string, to time.Time, readReason string) []string {
	reasons := []string{}
	first, last := fleet.Object(window["first"]), fleet.Object(window["last"])
	if readReason != "" {
		reasons = append(reasons, readReason)
	} else if window == nil {
		reason := "no_active_window"
		for _, r := range recent {
			p := text(r["provider"], "claude")
			if p == "" {
				p = "claude"
			}
			if p == provider && r["limit"] == limit {
				if _, e := time.Parse(time.RFC3339Nano, fleet.Text(r["resetsAt"])); e != nil {
					reason = "missing_resets_at"
				}
			}
		}
		reasons = append(reasons, reason)
	} else {
		maxAge := 6 * time.Hour
		if limit == "five_hour" {
			maxAge = time.Hour
		}
		if last == nil || to.Sub(at(last)) > maxAge {
			reasons = append(reasons, "stale_record")
		}
		if window["records"].(int) < 2 {
			reasons = append(reasons, "insufficient_records")
		}
		missing := first == nil || last == nil
		for _, r := range recent {
			p := text(r["provider"], "claude")
			if p == "" {
				p = "claude"
			}
			if p == provider && r["limit"] == limit && r["resetsAt"] == window["resetsAt"] {
				if _, ok := number(r["percent"]); !ok {
					missing = true
				}
			}
		}
		if missing {
			reasons = append(reasons, "missing_percent")
		}
	}
	return reasons
}

// projectLimit extrapolates the usage rate of the window to the time it would
// reach 100%, adding the reasons the projection cannot be made.
func projectLimit(window map[string]any, limit string, reasons []string) (rate, measured, threshold, projectedAt any, _ []string) {
	first, last := fleet.Object(window["first"]), fleet.Object(window["last"])
	reset, _ := time.Parse(time.RFC3339Nano, fleet.Text(window["resetsAt"]))
	if last != nil && window != nil {
		threshold = reset.Sub(at(last)).Hours()
	}
	if len(reasons) == 0 && first != nil && last != nil {
		elapsed := at(last).Sub(at(first)).Hours()
		minimum := float64(1)
		if limit == "five_hour" {
			minimum = .25
		}
		if elapsed < minimum {
			reasons = append(reasons, "insufficient_elapsed")
		} else {
			r := (fleet.Number(last["percent"]) - fleet.Number(first["percent"])) / elapsed
			rate = r
			if r <= 0 {
				reasons = append(reasons, "non_positive_rate")
			} else {
				m := (100 - fleet.Number(last["percent"])) / r
				measured = m
				projectedMS := float64(at(last).UnixMilli()) + m*3600000
				if math.IsInf(projectedMS, 0) || math.IsNaN(projectedMS) || math.Abs(projectedMS) > 8640000000000000 {
					reasons = append(reasons, "invalid_projection")
				} else {
					projectedAt = iso(time.UnixMilli(int64(math.Trunc(projectedMS))))
				}
			}
		}
	}
	return rate, measured, threshold, projectedAt, reasons
}
func nilMap(v map[string]any) any {
	if v == nil {
		return nil
	}
	return v
}
