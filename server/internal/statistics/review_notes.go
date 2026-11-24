package statistics

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"maps"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type notesEvent struct {
	At              time.Time
	Kind, Key, Repo string
	Bytes           float64
}

var notePatterns = []struct {
	Kind    string
	Pattern *regexp.Regexp
}{{"return", regexp.MustCompile(`^レビュー保留 ((\S+)#\d+): 自動差し戻し \d+/\d+$`)}, {"return", regexp.MustCompile(`^検証 ((\S+)#\d+): レビューの指摘をこのPCの実装へ自動で差し戻しました`)}, {"held", regexp.MustCompile(`^レビュー保留 ((\S+)#\d+): (?:人の確認待ち|削除の承認待ち（\S+）)$`)}, {"merged", regexp.MustCompile(`^マージ完了 ((\S+)#\d+)(?: |$)`)}, {"handoff", regexp.MustCompile(`^引き渡しを受け取りました ((\S+)#\d+): `)}}
var noteLine = regexp.MustCompile(`^注意点を添付 ((\S+)#\d+) (\d+) bytes$`)
var looseNote = regexp.MustCompile(`^注意点を添付(?: ((\S+)#\d+))?`)

func parseNotes(lines []logLine) []notesEvent {
	out := []notesEvent{}
	for _, line := range lines {
		if m := noteLine.FindStringSubmatch(line.Body); m != nil {
			n, _ := strconv.ParseFloat(m[3], 64)
			out = append(out, notesEvent{line.At, "note", m[1], m[2], n})
			continue
		}
		if m := looseNote.FindStringSubmatch(line.Body); m != nil {
			out = append(out, notesEvent{At: line.At, Kind: "noteUnreadable", Key: m[1], Repo: m[2]})
			continue
		}
		for _, p := range notePatterns {
			if m := p.Pattern.FindStringSubmatch(line.Body); m != nil {
				out = append(out, notesEvent{At: line.At, Kind: p.Kind, Key: m[1], Repo: m[2]})
				break
			}
		}
	}
	return out
}
func notesTokens(row map[string]any) any {
	keys := []string{"inputTokens", "outputTokens"}
	if row["provider"] == "claude" {
		keys = append(keys, "cacheReadTokens", "cacheWriteTokens")
	}
	total := float64(0)
	for _, key := range keys {
		n, ok := row[key].(float64)
		if !ok {
			return nil
		}
		total += n
	}
	return total
}
func unitAverage(units [][]map[string]any, metric func(map[string]any) any) map[string]any {
	known, unknown, sum := 0, 0, float64(0)
	for _, unit := range units {
		value := float64(0)
		missing := false
		for _, r := range unit {
			n, ok := metric(r).(float64)
			if !ok {
				missing = true
			} else {
				value += n
			}
		}
		if missing {
			unknown++
		} else {
			known++
			sum += value
		}
	}
	var avg any
	if known > 0 {
		avg = sum / float64(known)
	}
	return map[string]any{"average": avg, "count": known, "unknown": unknown}
}
func notesCounted(r map[string]any) bool {
	return oneOf(r["step"], []string{"実装", "競合解消", "独立レビュー"})
}
func notesUsage(rows []map[string]any, keys map[string]bool, p Period) map[string]any {
	byKey := map[string][]map[string]any{}
	for _, r := range rows {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		key := identityIssue(r)
		if e == nil && !at.Before(p.From) && at.Before(p.To) && notesCounted(r) && keys[key] {
			byKey[key] = append(byKey[key], r)
		}
	}
	units := [][]map[string]any{}
	for _, mine := range byKey {
		for _, r := range mine {
			if r["step"] == "実装" {
				units = append(units, mine)
				break
			}
		}
	}
	return map[string]any{"noRecord": len(keys) - len(byKey), "noImplementation": len(byKey) - len(units), "uncorrectable": 0, "costUsd": unitAverage(units, func(r map[string]any) any { return r["costUsd"] }), "tokens": unitAverage(units, notesTokens)}
}
func identityIssue(r map[string]any) string {
	return fleet.Text(r["repo"]) + "#" + strconv.FormatInt(int64(fleet.Number(r["issue"])), 10)
}
func notesTime(steps []*loggedStep, keys map[string]bool, p Period) map[string]any {
	type entry struct {
		ms                      float64
		missing, implementation bool
	}
	per := map[string]*entry{}
	for _, span := range steps {
		placed := stepPlaced(span)
		if !keys[span.Key] || !oneOf(span.Label, []string{"実装", "追従", "レビュー"}) || stepNested(span, []string{"実装", "追従", "レビュー"}) || placed.Before(p.From) || !placed.Before(p.To) {
			continue
		}
		x := per[span.Key]
		if x == nil {
			x = &entry{}
			per[span.Key] = x
		}
		if span.Label == "実装" {
			x.implementation = true
		}
		if stepMeasured(span) {
			x.ms += float64(span.End.Sub(span.Start)) / float64(time.Millisecond)
		} else {
			x.missing = true
		}
	}
	count, missing, implemented, sum := 0, 0, 0, float64(0)
	for _, x := range per {
		if !x.implementation {
			continue
		}
		implemented++
		if x.missing {
			missing++
		} else {
			count++
			sum += x.ms
		}
	}
	var avg any
	if count > 0 {
		avg = sum / float64(count)
	}
	return map[string]any{"averageMs": avg, "count": count, "missing": missing, "noImplementation": len(per) - implemented, "noRecord": len(keys) - len(per)}
}
func notesPeriod(events []notesEvent, first time.Time, p Period, repo string, steps []*loggedStep, rows []map[string]any) map[string]any {
	out := p.Dates()
	out["reason"], out["since"] = nil, nil
	if first.IsZero() || !first.Before(p.To) {
		out["reason"] = "この期間の監視の記録がありません"
		for _, k := range strings.Fields("reviewed otherHost local returnedPrs rate returns usage time notes") {
			out[k] = nil
		}
		return out
	}
	if first.After(p.From) {
		out["since"] = first.In(JST).Format("2006-01-02")
	}
	elsewhere := map[string]bool{}
	for _, e := range events {
		if e.Kind == "handoff" && e.At.Before(p.To) {
			elsewhere[e.Key] = true
		}
	}
	reviewed, returned := map[string]bool{}, map[string]bool{}
	returns, noteCount, unreadable, bytes := 0, 0, 0, float64(0)
	for _, e := range events {
		if e.At.Before(p.From) || !e.At.Before(p.To) || repo != "" && e.Repo != repo && !(e.Kind == "noteUnreadable" && e.Repo == "") || e.Kind == "handoff" {
			continue
		}
		switch e.Kind {
		case "note":
			noteCount++
			bytes += e.Bytes
		case "noteUnreadable":
			unreadable++
		default:
			reviewed[e.Key] = true
			if e.Kind == "return" && !elsewhere[e.Key] {
				returns++
				returned[e.Key] = true
			}
		}
	}
	local := map[string]bool{}
	for key := range reviewed {
		if !elsewhere[key] {
			local[key] = true
		}
	}
	var rate any
	if len(local) > 0 {
		rate = float64(len(returned)) / float64(len(local))
	}
	out["reviewed"], out["otherHost"], out["local"], out["returnedPrs"], out["rate"], out["returns"] = len(reviewed), len(reviewed)-len(local), len(local), len(returned), rate, returns
	out["usage"], out["time"] = notesUsage(rows, local, p), notesTime(steps, local, p)
	out["notes"] = map[string]any{"lines": noteCount, "bytes": bytes, "unreadable": unreadable, "tokens": math.Round(bytes / 3)}
	return out
}
func notesReturnUsage(rows []map[string]any, q Query) map[string]any {
	runs := map[string][]map[string]any{}
	returned := map[string]bool{}
	for _, r := range rows {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if !notesCounted(r) || q.Project != "" && r["repo"] != q.Project || fleet.Text(r["repo"]) == "" || fleet.Number(r["issue"]) < 1 || r["generation"] == nil || e != nil || !(!at.Before(q.Before.From) && at.Before(q.Before.To) || !at.Before(q.After.From) && at.Before(q.After.To)) {
			continue
		}
		key := identity(r["repo"], r["issue"], r["generation"])
		runs[key] = append(runs[key], r)
		if r["step"] != "独立レビュー" && r["generationTrigger"] == "review-return" {
			returned[key] = true
		}
	}
	units := [][]map[string]any{}
	for key := range returned {
		units = append(units, runs[key])
	}
	return map[string]any{"returns": len(units), "costUsd": unitAverage(units, func(r map[string]any) any { return r["costUsd"] }), "tokens": unitAverage(units, notesTokens)}
}
func notesNet(before, after, perReturn map[string]any, skipped int) map[string]any {
	out := map[string]any{"tokens": nil, "reason": nil, "perReturnTokens": nil, "expectedReturns": nil, "reducedReturns": nil, "savedTokens": nil, "noteTokens": nil}
	per := fleet.Object(perReturn["tokens"])
	if per["unknown"] == 0 {
		out["perReturnTokens"] = per["average"]
	}
	reason := ""
	if before["reason"] != nil || after["reason"] != nil {
		reason = "前後の期間の監視の記録がありません"
	} else if before["local"] == 0 || after["local"] == 0 {
		reason = "前後の期間にこのPCで実装してレビューしたPRがありません"
	} else if fleet.Number(fleet.Object(before["notes"])["lines"]) > 0 || fleet.Number(fleet.Object(before["notes"])["unreadable"]) > 0 {
		reason = "前の期間にも注意点を添付した実装があります"
	} else if fleet.Number(fleet.Object(after["notes"])["unreadable"]) > 0 {
		reason = "後の期間に読めない注意点の添付記録があります"
	}
	if reason != "" {
		out["reason"] = reason
		return out
	}
	expected := fleet.Number(before["returns"]) / fleet.Number(before["local"]) * fleet.Number(after["local"])
	reduced := expected - fleet.Number(after["returns"])
	saved := float64(0)
	if reduced != 0 {
		if skipped > 0 {
			reason = "使用量の記録に読めなかった行があります"
		} else if per["average"] == nil || per["unknown"] != 0 {
			reason = "差し戻しの実行のトークン数を確定できません"
		} else {
			saved = fleet.Number(per["average"]) * reduced
		}
	}
	if reason != "" {
		out["reason"] = reason
		return out
	}
	notes := fleet.Number(fleet.Object(after["notes"])["tokens"])
	out["tokens"], out["expectedReturns"], out["reducedReturns"], out["savedTokens"], out["noteTokens"] = saved-notes, expected, reduced, saved, notes
	return out
}
func (s *Service) ReviewNotes(ctx context.Context, q Query) (any, error) {
	lines, alive, end, e := s.logs(ctx)
	if e != nil {
		return nil, e
	}
	store := usage.Store{DB: s.DB}
	to := q.Before.To
	if q.After.To.After(to) {
		to = q.After.To
	}
	rows, skipped, e := store.Read(ctx, "", time.Unix(0, 0), to)
	if e != nil {
		return nil, e
	}
	rows, status := usage.CorrectCosts(rows, []map[string]any{})
	mine := []map[string]any{}
	for i, r := range rows {
		if r["host"] != "nas" {
			continue
		}
		if status[i] == "uncorrectable" {
			r = maps.Clone(r)
			r["costUsd"] = nil
		}
		mine = append(mine, r)
	}
	steps := logSteps(lines, end, alive)
	events := parseNotes(lines)
	var first time.Time
	if len(lines) > 0 {
		first = lines[0].At
	}
	before, after := notesPeriod(events, first, q.Before, q.Project, steps, mine), notesPeriod(events, first, q.After, q.Project, steps, mine)
	per := notesReturnUsage(mine, q)
	return map[string]any{"project": q.Project, "unavailable": []any{}, "bytesPerToken": 3, "skipped": skipped, "before": before, "after": after, "perReturn": per, "net": notesNet(before, after, per, skipped)}, nil
}
