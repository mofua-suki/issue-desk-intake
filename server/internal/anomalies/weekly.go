package anomalies

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"sort"
	"strings"
	"time"
)

type repeatPeriod struct{ From, Split, To time.Time }

func midnight(at time.Time) time.Time {
	jst := at.In(time.FixedZone("JST", 9*3600))
	return time.Date(jst.Year(), jst.Month(), jst.Day(), 0, 0, 0, 0, jst.Location())
}
func repeatSide(rows []map[string]any, from, to time.Time) map[string]any {
	drafts := map[string]bool{}
	count, missing := 0, 0
	for _, r := range rows {
		t := at(r)
		if t.Before(from) || !t.Before(to) {
			continue
		}
		count++
		draft := fleet.Text(r["draft"])
		if strings.TrimSpace(draft) == "" {
			missing++
		} else {
			drafts[draft] = true
		}
	}
	var d, per any
	if missing == 0 {
		d = len(drafts)
		if len(drafts) > 0 {
			per = float64(count) / float64(len(drafts))
		}
	}
	return map[string]any{"D": d, "R": count, "rowsPerDraft": per, "missingDraftRows": missing}
}
func repeatJudge(rows []map[string]any, groups map[string][]map[string]any, step, provider string, p repeatPeriod, incomplete bool) map[string]any {
	before, current := repeatSide(rows, p.From, p.Split), repeatSide(rows, p.Split, p.To)
	reasons := []string{}
	if incomplete {
		reasons = append(reasons, "period_incomplete")
	}
	if len(rows) == 0 {
		reasons = append(reasons, "missing_data")
	}
	unassigned := false
	for key, other := range groups {
		parts := strings.Split(key, "\x00")
		if len(other) > 0 && (parts[0] == "" || parts[1] == "") && (parts[0] == "" || parts[0] == step) && (parts[1] == "" || parts[1] == provider) {
			unassigned = true
		}
	}
	if len(rows) > 0 && (unassigned || step == "" || provider == "" || fleet.Number(before["missingDraftRows"]) > 0 || fleet.Number(current["missingDraftRows"]) > 0) {
		reasons = append(reasons, "missing_required_data")
	}
	if before["D"] != nil && fleet.Number(before["D"]) < 10 || current["D"] != nil && fleet.Number(current["D"]) < 10 {
		reasons = append(reasons, "insufficient_drafts")
	}
	baseline, measured := before["rowsPerDraft"], current["rowsPerDraft"]
	var threshold any
	if baseline != nil {
		n := baseline.(float64)
		threshold = math.Max(n*1.5, n+2)
	}
	status := "normal"
	if len(reasons) > 0 {
		status = "held"
	} else if measured != nil && threshold != nil && measured.(float64)+2.220446049250313e-16*threshold.(float64) >= threshold.(float64) {
		status = "hit"
	}
	return map[string]any{"windows": map[string]any{"previous": map[string]any{"from": iso(p.From), "to": iso(p.Split)}, "current": map[string]any{"from": iso(p.Split), "to": iso(p.To)}, "timezone": "Asia/Tokyo"}, "previous": before, "current": current, "baseline": baseline, "measured": measured, "threshold": threshold, "status": status, "reasons": reasons}
}
func IntakeRepeats(history []map[string]any, to, now time.Time) []map[string]any {
	end := midnight(to)
	confirmed := repeatPeriod{end.Add(-14 * 24 * time.Hour), end.Add(-7 * 24 * time.Hour), end}
	provisional := repeatPeriod{to.Add(-14*24*time.Hour + time.Millisecond), to.Add(-7*24*time.Hour + time.Millisecond), to.Add(time.Millisecond)}
	groups, all := map[string][]map[string]any{}, map[string][]map[string]any{}
	keys := map[string]bool{}
	for _, r := range history {
		if r["source"] != "intake" || at(r).IsZero() {
			continue
		}
		step, provider := usage.CanonicalStep(fleet.Text(r["step"])), fleet.Text(r["provider"])
		if strings.TrimSpace(step) == "" {
			step = ""
		}
		if strings.TrimSpace(provider) == "" {
			provider = ""
		}
		key := step + "\x00" + provider
		t := at(r)
		if !t.Before(confirmed.From) && t.Before(confirmed.To) {
			groups[key] = append(groups[key], r)
			keys[key] = true
		}
		if !t.Before(provisional.From) && t.Before(provisional.To) {
			all[key] = append(all[key], r)
			keys[key] = true
		}
	}
	if len(keys) == 0 {
		keys["\x00"] = true
	}
	ordered := []string{}
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	out := []map[string]any{}
	for _, key := range ordered {
		parts := strings.Split(key, "\x00")
		step, provider := parts[0], parts[1]
		h := repeatJudge(groups[key], groups, step, provider, confirmed, confirmed.To.After(midnight(now)))
		p := repeatJudge(all[key], all, step, provider, provisional, false)
		pair := []any{nil, nil}
		if step != "" {
			pair[0] = step
		}
		if provider != "" {
			pair[1] = provider
		}
		raw, _ := json.Marshal(pair)
		h["kind"], h["kindName"], h["step"], h["provider"], h["at"], h["period"], h["thresholds"], h["unit"], h["repo"], h["issue"], h["rule"], h["causeKey"] = "intake-repeat", names["intake-repeat"], pair[0], pair[1], iso(end), "JSTの完了済み直近7日と直前7日", map[string]any{"minDrafts": 10, "ratio": 1.5, "increase": 2}, "行/下書き", nil, nil, "両期間D>=10、R/Dが1.5倍以上かつ2以上増加", "intake-repeat:"+string(raw)
		p["period"], p["causeKey"] = "toまでの直近7日と直前7日（暫定）", "intake-repeat-provisional:"+string(raw)
		h["provisional"] = p
		out = append(out, h)
	}
	return out
}
