package statistics

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"time"
)

type fleetSegment struct {
	From, To time.Time
	Hosts    map[string]int
}
type stallInterval struct {
	Start, End          time.Time
	Repo, Kind, Key     string
	Detected, Suspected bool
}
type stallMark struct {
	At   time.Time
	Repo string
}
type stallSource struct {
	Segments                []fleetSegment
	Intervals               []*stallInterval
	Marks, Releases, Merges []stallMark
	First, End              time.Time
	Alive                   bool
	Fixes                   []map[string]any
}

var stallEvent = regexp.MustCompile(`^止まり(を検知しました|が解消しました): ((\S+) ([a-z0-9][a-z0-9-]*) ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:#\d+)?（[a-z_]+(・不具合の疑い)?）: .*)$`)
var stallMarkPattern = regexp.MustCompile(`^進まない占有に外す印を付けました ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#\d+（[a-z0-9][a-z0-9-]*）: `)
var stallReleasePattern = regexp.MustCompile(`^外す印の付いた占有が外れました ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#\d+（[a-z0-9][a-z0-9-]*）$`)
var stallMergePattern = regexp.MustCompile(`^マージ完了 ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#\d+ [^\s（]+$`)
var stallNames = map[string]string{"実装しない実装PC": "idle_host", "進まない占有": "stuck_claim", "同じ失敗の繰り返し": "repeated_failure", "範囲の重なりで取れない実行待ち": "scope_blocked"}

func parseStalls(lines []logLine, end time.Time) stallSource {
	src := stallSource{End: end}
	if len(lines) == 0 {
		return src
	}
	src.First = lines[0].At
	cursor := src.First
	previous := src.First
	open := map[string][]*stallInterval{}
	close := func(at time.Time) {
		if at.After(cursor) {
			src.Segments = append(src.Segments, fleetSegment{cursor, at, nil})
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line.Body, "起動 ") {
			close(previous)
			cursor = line.At
		}
		if m := stallEvent.FindStringSubmatch(line.Body); m != nil {
			if kind := stallNames[m[3]]; kind != "" {
				key := m[2]
				if m[1] == "を検知しました" {
					item := &stallInterval{Start: line.At, Repo: m[5], Kind: kind, Key: key, Detected: true, Suspected: m[6] != ""}
					src.Intervals = append(src.Intervals, item)
					open[key] = append(open[key], item)
				} else if len(open[key]) > 0 {
					open[key][0].End = line.At
					open[key] = open[key][1:]
				} else {
					src.Intervals = append(src.Intervals, &stallInterval{Start: src.First, End: line.At, Repo: m[5], Kind: kind})
				}
			}
		}
		for _, target := range []struct {
			Pattern *regexp.Regexp
			Rows    *[]stallMark
		}{{stallMarkPattern, &src.Marks}, {stallReleasePattern, &src.Releases}, {stallMergePattern, &src.Merges}} {
			if m := target.Pattern.FindStringSubmatch(line.Body); m != nil {
				*target.Rows = append(*target.Rows, stallMark{line.At, m[1]})
			}
		}
		previous = line.At
	}
	close(end)
	return src
}
func clipDuration(from, to, start, end time.Time) float64 {
	if start.After(from) {
		from = start
	}
	if end.Before(to) {
		to = end
	}
	if !to.After(from) {
		return 0
	}
	return float64(to.Sub(from)) / float64(time.Millisecond)
}
func (src stallSource) measure(p Period, repo string) map[string]any {
	c := src.coverage(p)
	if c.recorded == 0 {
		return nil
	}
	inside := func(at time.Time) bool {
		if at.Before(p.From) || !at.Before(p.To) {
			return false
		}
		for _, seg := range src.Segments {
			if !at.Before(seg.From) && !at.After(seg.To) {
				return true
			}
		}
		return false
	}
	mine := func(r string) bool { return repo == "" || repo == r }
	stalls := src.stallCounts(p, inside, mine)
	count := func(rows []stallMark) int {
		n := 0
		for _, r := range rows {
			if mine(r.Repo) && inside(r.At) {
				n++
			}
		}
		return n
	}
	merged := count(src.Merges)
	days := c.recorded / (24 * 60 * 60 * 1000)
	var ratio any
	if c.active > 0 {
		ratio = c.parallel / c.active
	}
	fixes := src.fixCount(p, mine)
	return map[string]any{"measured": map[string]any{"start": iso(c.first), "end": iso(c.last)}, "recordedMs": c.recorded, "unrecordedMs": float64(c.last.Sub(c.first))/float64(time.Millisecond) - c.recorded, "stalls": stalls, "marked": count(src.Marks), "released": count(src.Releases), "fixes": fixes, "fixesReason": nil, "parallel": map[string]any{"measuredMs": c.known, "activeMs": c.active, "parallelMs": c.parallel, "ratio": ratio, "maxHosts": c.maxHosts, "hosts": c.hostMs}, "merged": merged, "days": days, "mergedPerDay": float64(merged) / days}
}

// stallCoverage is how much of the period the segments record, and how many hosts worked in them.
type stallCoverage struct {
	recorded, known, active, parallel float64
	maxHosts                          int
	hostMs                            map[string]float64
	first, last                       time.Time
}

// coverage measures the recorded segments within the period.
func (src stallSource) coverage(p Period) stallCoverage {
	recorded, known, active, parallel, maxHosts := float64(0), float64(0), float64(0), float64(0), 0
	hostMs := map[string]float64{}
	var first, last time.Time
	for _, seg := range src.Segments {
		ms := clipDuration(seg.From, seg.To, p.From, p.To)
		if ms <= 0 {
			continue
		}
		start, end := seg.From, seg.To
		if p.From.After(start) {
			start = p.From
		}
		if p.To.Before(end) {
			end = p.To
		}
		if first.IsZero() {
			first = start
		}
		last = end
		recorded += ms
		known += ms
		working := 0
		for host, count := range seg.Hosts {
			if count > 0 {
				working++
				hostMs[host] += ms
			}
		}
		if working > 0 {
			active += ms
		}
		if working > 1 {
			parallel += ms
		}
		if working > maxHosts {
			maxHosts = working
		}
	}
	return stallCoverage{recorded: recorded, known: known, active: active, parallel: parallel, maxHosts: maxHosts, hostMs: hostMs, first: first, last: last}
}

// stallCounts counts the stalls detected in the period by kind, and how long they lasted within the segments.
func (src stallSource) stallCounts(p Period, inside func(time.Time) bool, mine func(string) bool) map[string]any {
	kinds := map[string]int{"idle_host": 0, "stuck_claim": 0, "repeated_failure": 0, "scope_blocked": 0}
	total, suspected, unresolved := 0, 0, 0
	stalled := float64(0)
	for _, item := range src.Intervals {
		if !mine(item.Repo) {
			continue
		}
		if item.Detected && inside(item.Start) {
			total++
			kinds[item.Kind]++
			if item.Suspected {
				suspected++
			}
		}
		end := item.End
		if end.IsZero() {
			end = src.End
		}
		for _, seg := range src.Segments {
			a, b := seg.From, seg.To
			if p.From.After(a) {
				a = p.From
			}
			if p.To.Before(b) {
				b = p.To
			}
			if b.After(a) {
				stalled += clipDuration(a, b, item.Start, end)
			}
		}
		if item.Start.Before(p.To) && (item.End.IsZero() || !item.End.Before(p.To)) {
			unresolved++
		}
	}
	return map[string]any{"total": total, "kinds": kinds, "suspected": suspected, "unresolved": unresolved, "stalledMs": stalled}
}

// fixCount counts the stall fixes requested in the period.
func (src stallSource) fixCount(p Period, mine func(string) bool) int {
	fixes := 0
	for _, fix := range src.Fixes {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(fix["at"]))
		if e == nil && mine(fleet.Text(fix["project"])) && !at.Before(p.From) && at.Before(p.To) && !at.Before(src.First) && oneOf(fix["status"], []string{"submitted", "investigation_waiting", "investigation_reported", "investigation_ended"}) {
			fixes++
		}
	}
	return fixes
}
func (s *Service) Stalls(ctx context.Context, q Query) (any, error) {
	lines, alive, end, e := s.logs(ctx)
	if e != nil {
		return nil, e
	}
	src := parseStalls(lines, end)
	src.Alive = alive
	to := q.Before.To
	if q.After.To.After(to) {
		to = q.After.To
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if to.After(now) {
		to = now
	}
	events, e := s.stallEvents(ctx, to)
	if e != nil {
		return nil, e
	}
	src.Segments = intersectStallSegments(src.Segments, events)
	fixes, e := s.localRows(ctx, "stall_fixes")
	if e != nil {
		return nil, e
	}
	src.Fixes = fixes
	var coverage any
	var reason any = "job_eventsから実装中の区間を復元できないか、監視の記録と重なりません"
	if len(src.Segments) > 0 {
		coverage = map[string]any{"start": iso(src.Segments[0].From), "end": iso(src.Segments[len(src.Segments)-1].To)}
		reason = nil
	} else if len(lines) == 0 {
		reason = "監視の記録がありません"
	}
	period := func(p Period) any {
		out := p.Dates()
		value := src.measure(p, q.Project)
		if value == nil {
			out["measured"] = nil
			out["reason"] = reason
			if reason == nil {
				out["reason"] = "この期間は記録がありません（監視の停止や、記録を確認できなかった間です）"
			}
			return out
		}
		for k, v := range value {
			out[k] = v
		}
		out["reason"] = nil
		daily := []any{}
		for at := jstMidnight(p.From); at.Before(p.To); at = at.Add(24 * time.Hour) {
			from, to := at, at.Add(24*time.Hour)
			if p.From.After(from) {
				from = p.From
			}
			if p.To.Before(to) {
				to = p.To
			}
			value := src.measure(Period{from, to}, q.Project)
			row := map[string]any{"date": at.In(JST).Format("2006-01-02"), "measured": nil}
			if value != nil {
				row["measured"], row["stalls"], row["merged"], row["ratio"] = value["measured"], fleet.Object(value["stalls"])["total"], value["merged"], fleet.Object(value["parallel"])["ratio"]
			} else {
				row["reason"] = "この期間は記録がありません"
			}
			daily = append(daily, row)
		}
		out["daily"] = daily
		return out
	}
	return map[string]any{"project": q.Project, "worker": alive, "coverage": coverage, "reason": reason, "unavailable": []any{}, "before": period(q.Before), "after": period(q.After)}, nil
}
