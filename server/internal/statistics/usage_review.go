package statistics

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/timedcache"
	"issue-desk-intake/server/internal/usage"
	"math"
	"sort"
	"time"
)

func tallyReview(rows []map[string]any, weights map[string]any) map[string]any {
	out := map[string]any{"otherCount": 0, "claude": map[string]any{"count": 0, "unknown": 0, "value": float64(0)}, "codex": map[string]any{"count": 0, "unknown": 0, "value": float64(0)}}
	for _, r := range rows {
		p := fleet.Text(r["provider"])
		if p != "claude" && p != "codex" {
			out["otherCount"] = out["otherCount"].(int) + 1
			continue
		}
		t := fleet.Object(out[p])
		t["count"] = t["count"].(int) + 1
		v := r["costUsd"]
		if p == "codex" {
			v = usage.Weighted(r, weights)["weighted"]
		}
		if n, ok := v.(float64); ok {
			t["value"] = t["value"].(float64) + n
		} else {
			t["unknown"] = t["unknown"].(int) + 1
		}
	}
	for _, p := range []string{"claude", "codex"} {
		t := fleet.Object(out[p])
		if t["count"].(int) > 0 && t["count"] == t["unknown"] {
			t["value"] = nil
		}
	}
	return out
}
func reviewOwn(round, row map[string]any) bool {
	inv := fleet.Object(round["investigationIssue"])
	return row["source"] == "intake" && fleet.Text(round["draftId"]) != "" && row["draft"] == round["draftId"] || inv != nil && row["repo"] == inv["project"] && row["issue"] == inv["number"]
}
func reviewRange(round map[string]any, now time.Time) (string, string, *Period, *Period) {
	treatments := fleet.List(round["treatmentIssues"])
	if len(treatments) == 0 {
		return "cost_only", "対策Issueがありません（見直しの費用だけを示します）", nil, nil
	}
	merges := []time.Time{}
	for _, v := range treatments {
		t := fleet.Object(v)
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(t["mergedAt"]))
		if e == nil {
			merges = append(merges, at)
		} else if t["closedAt"] == nil {
			return "waiting", "PRのマージを確かめていない対策Issueがあります", nil, nil
		}
	}
	if len(merges) == 0 {
		return "cost_only", "対策IssueのPRはマージされずに終わりました（見直しの費用だけを示します）", nil, nil
	}
	sort.Slice(merges, func(i, j int) bool { return merges[i].Before(merges[j]) })
	first, last := merges[0], merges[len(merges)-1]
	end := last.Add(7 * 24 * time.Hour)
	if now.Before(end) {
		end = now
	}
	if end.Before(last) {
		end = last
	}
	before, after := &Period{first.Add(-7 * 24 * time.Hour), first}, &Period{last, end}
	days := end.Sub(last).Hours() / 24
	if days < 3 {
		return "waiting", fmt.Sprintf("最後の対策PRのマージから%.1f日です（比較には3日以上必要）", days), before, after
	}
	return "ready", "", before, after
}
func tallyPeriod(rows []map[string]any, round map[string]any, p Period, weights map[string]any) (map[string]any, int) {
	mine := []map[string]any{}
	issues := map[string]bool{}
	for _, r := range rows {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e != nil || at.Before(p.From) || !at.Before(p.To) || reviewOwn(round, r) {
			continue
		}
		mine = append(mine, r)
		if fleet.Text(r["repo"]) != "" && fleet.Number(r["issue"]) >= 1 {
			issues[identity(r["repo"], r["issue"])] = true
		}
	}
	return tallyReview(mine, weights), len(issues)
}
func reviewJudge(round map[string]any, rows []map[string]any, cost map[string]any, weights map[string]any, now time.Time, earliest time.Time) (any, map[string]any, any) {
	result, reason, b, a := reviewRange(round, now)
	verdict := map[string]any{"result": result, "reason": reason, "providers": map[string]any{}}
	if b == nil {
		return nil, verdict, nil
	}
	bt, bi := tallyPeriod(rows, round, *b, weights)
	at, ai := tallyPeriod(rows, round, *a, weights)
	ad := a.To.Sub(a.From).Hours() / 24
	var afterPer any
	if ad > 0 {
		afterPer = float64(ai) / ad
	}
	comparison := map[string]any{"before": map[string]any{"from": iso(b.From), "to": iso(b.To), "days": 7}, "after": map[string]any{"from": iso(a.From), "to": iso(a.To), "days": ad}, "providers": map[string]any{}, "workload": map[string]any{"before": map[string]any{"issues": bi, "perDay": float64(bi) / 7}, "after": map[string]any{"issues": ai, "perDay": afterPer}}}
	judged := map[string]any{}
	for _, p := range []string{"claude", "codex"} {
		before, after, c := fleet.Object(bt[p]), fleet.Object(at[p]), fleet.Object(cost[p])
		var bp, ap, diff, saved any
		if v, ok := before["value"].(float64); ok {
			bp = v / 7
		}
		if v, ok := after["value"].(float64); ok && ad > 0 {
			ap = v / ad
		}
		if bp != nil && ap != nil {
			diff = ap.(float64) - bp.(float64)
			saved = -diff.(float64) * ad
		}
		before["perDay"], after["perDay"] = bp, ap
		fleet.Object(comparison["providers"])[p] = map[string]any{"before": before, "after": after, "diffPerDay": diff, "saved": saved}
		if result == "waiting" {
			continue
		}
		r, why := "recouped", "減った量が見直しの費用以上です"
		if earliest.IsZero() || jstMidnight(earliest).After(b.From) {
			r, why = "undeterminable", "前の期間の全体に使用量の記録がありません"
		} else if c["value"] == nil || saved == nil || c["unknown"].(int)*2 > c["count"].(int) || before["unknown"].(int)*2 > before["count"].(int) || after["unknown"].(int)*2 > after["count"].(int) {
			r, why = "undeterminable", "不明が件数の半分を超えるか、値がありません"
		} else if saved.(float64) < c["value"].(float64)-1e-9*math.Max(1, math.Abs(c["value"].(float64))) {
			r, why = "not_recouped", "減った量が見直しの費用に届きません"
		}
		judged[p] = map[string]any{"result": r, "reason": why}
	}
	note := fmt.Sprintf("使用量の記録のあるIssue: 前 %d件・後 %d件。仕事量の違いも使用量に表れるため、総量の減少だけで対策の効果とは断定できません。", bi, ai)
	if result == "waiting" {
		return comparison, verdict, note
	}
	overall := "recouped"
	for _, p := range []string{"claude", "codex"} {
		r := fleet.Object(judged[p])["result"]
		if r == "not_recouped" {
			overall = "not_recouped"
		} else if r == "undeterminable" && overall != "not_recouped" {
			overall = "undeterminable"
		}
	}
	return comparison, map[string]any{"result": overall, "reason": map[string]string{"recouped": "元が取れた実行先の記録です", "not_recouped": "元が取れていない実行先があります", "undeterminable": "記録不足のため判定できない実行先があります"}[overall], "providers": judged}, note
}
func iso(t time.Time) string { return contract.ISO(t) }
func (s *Service) UsageReview(ctx context.Context) (any, error) {
	return s.usageReview(ctx, false)
}

// UsageReviewUnacknowledged computes only rounds eligible for notifications.
func (s *Service) UsageReviewUnacknowledged(ctx context.Context) (any, error) {
	return s.usageReview(ctx, true)
}

func (s *Service) usageReview(ctx context.Context, unacknowledgedOnly bool) (any, error) {
	rounds, e := s.localRows(ctx, "usage_review_rounds")
	if e != nil {
		return nil, e
	}
	out := []any{}
	if len(rounds) == 0 {
		return map[string]any{"reason": nil, "rounds": out}, nil
	}
	sort.Slice(rounds, func(i, j int) bool { return fleet.Number(rounds[i]["round"]) < fleet.Number(rounds[j]["round"]) })
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	acks, e := s.readReviewAcks(ctx)
	if e != nil {
		return nil, e
	}
	if unacknowledgedOnly {
		pending := rounds[:0]
		for _, r := range rounds {
			if acks[fleet.Number(r["round"])] == "" {
				pending = append(pending, r)
			}
		}
		rounds = pending
		if len(rounds) == 0 {
			return map[string]any{"reason": nil, "rounds": out}, nil
		}
	}
	// Only the notification path keeps judgments: it is polled with the list,
	// while the full report is read on request and stays current.
	var cache *timedcache.Cache[reviewJudgment]
	if unacknowledgedOnly {
		cache = s.reviewCache
	}
	judged, e := s.reviewJudgments(ctx, cache, rounds, now)
	if e != nil {
		return nil, e
	}
	for i, r := range rounds {
		out = append(out, s.reviewRoundView(r, acks, judged[i]))
	}
	return map[string]any{"reason": nil, "rounds": out}, nil
}

// reviewJudgment is the part of a round's view computed from the usage rows.
type reviewJudgment struct {
	cost       map[string]any
	comparison any
	verdict    map[string]any
	note       any
}

func copyReviewJudgment(j reviewJudgment) reviewJudgment {
	return reviewJudgment{fleet.Object(timedcache.CopyValue(j.cost)), timedcache.CopyValue(j.comparison), fleet.Object(timedcache.CopyValue(j.verdict)), j.note}
}

// usageReviewCacheTTL is how long a round's judgment is kept for the
// notifications. New usage rows change a kept judgment only after this time.
const usageReviewCacheTTL = 5 * time.Minute

// InitUsageReviewCache prepares the shared judgments of the notifications
// before a Service is copied. Without it every call judges from the usage rows.
func (s *Service) InitUsageReviewCache() {
	if s.reviewCache == nil {
		s.reviewCache = timedcache.New(usageReviewCacheTTL, copyReviewJudgment)
	}
}

// reviewCacheKey names the inputs other than the usage rows and the time that
// decide a round's judgment: the round's record and the weights. It is empty,
// and so never kept, when they cannot be written as JSON.
func reviewCacheKey(round, weights map[string]any) string {
	raw, e := json.Marshal([]any{round, weights})
	if e != nil {
		return ""
	}
	return string(raw)
}

// reviewJudgments judges each round. A judgment kept in the cache is used as
// it is, and the usage rows are read only when some round has none.
func (s *Service) reviewJudgments(ctx context.Context, cache *timedcache.Cache[reviewJudgment], rounds []map[string]any, now time.Time) ([]reviewJudgment, error) {
	done, e := cache.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	judged := make([]reviewJudgment, len(rounds))
	keys := make([]string, len(rounds))
	missing := []int{}
	for i, r := range rounds {
		if cache != nil {
			keys[i] = reviewCacheKey(r, s.Weights)
		}
		kept, ok := cache.Get(keys[i], now)
		if !ok {
			missing = append(missing, i)
			continue
		}
		judged[i] = kept
	}
	if len(missing) == 0 {
		return judged, nil
	}
	rows, earliest, e := s.reviewUsageRows(ctx, now)
	if e != nil {
		return nil, e
	}
	for _, i := range missing {
		judged[i] = judgeReviewRound(rounds[i], rows, s.Weights, now, earliest)
		cache.Put(keys[i], judged[i], now)
	}
	return judged, nil
}

// judgeReviewRound computes a round's cost and judgment from the usage rows.
func judgeReviewRound(r map[string]any, rows []map[string]any, weights map[string]any, now, earliest time.Time) reviewJudgment {
	mine := []map[string]any{}
	for _, row := range rows {
		if reviewOwn(r, row) {
			mine = append(mine, row)
		}
	}
	cost := tallyReview(mine, weights)
	comparison, verdict, note := reviewJudge(r, rows, cost, weights, now, earliest)
	return reviewJudgment{cost, comparison, verdict, note}
}

// readReviewAcks reads when each review round was acknowledged.
func (s *Service) readReviewAcks(ctx context.Context) (map[float64]string, error) {
	var e error
	acks := map[float64]string{}
	var exists int
	if e = s.AppDB.QueryRowContext(ctx, tableExistsSQL(), "usage_review_acks").Scan(&exists); e != nil {
		return nil, e
	}
	if exists > 0 {
		records, e := s.AppDB.QueryContext(ctx, bindSQL("SELECT round,at FROM usage_review_acks"))
		if e != nil {
			return nil, e
		}
		for records.Next() {
			var round float64
			var at string
			if e = records.Scan(&round, &at); e != nil {
				records.Close()
				return nil, e
			}
			acks[round] = at
		}
		e = records.Err()
		records.Close()
		if e != nil {
			return nil, e
		}
	}
	return acks, nil
}

// reviewUsageRows reads all usage rows up to the end of today with corrected
// costs, and the time of the earliest one.
func (s *Service) reviewUsageRows(ctx context.Context, now time.Time) ([]map[string]any, time.Time, error) {
	store := usage.Store{DB: s.DB}
	day := now.In(JST)
	end := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, JST)
	rows, _, e := store.Read(ctx, "", time.Unix(0, 0), end)
	if e != nil {
		return nil, time.Time{}, e
	}
	rows = correctedUsageCosts(rows)
	var earliest time.Time
	for _, r := range rows {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e == nil && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	return rows, earliest, nil
}

// reviewRoundView shows one review round with its cost and judgment.
func (s *Service) reviewRoundView(r map[string]any, acks map[float64]string, judged reviewJudgment) map[string]any {
	view := map[string]any{}
	for _, k := range []string{"round", "status", "startedAt", "draftId", "provider"} {
		view[k] = r[k]
	}
	view["startDate"] = nil
	if at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["startedAt"])); e == nil {
		view["startDate"] = at.In(JST).Format("2006-01-02")
	}
	view["noAction"] = r["noAction"] == true
	view["acknowledgedAt"] = nil
	if at := acks[fleet.Number(r["round"])]; at != "" {
		view["acknowledgedAt"] = at
	}
	view["investigationIssue"] = s.issueView(r["investigationIssue"])
	if i := fleet.Object(view["investigationIssue"]); i != nil {
		i["filedAt"] = fleet.Object(r["investigationIssue"])["filedAt"]
	}
	treatments := []any{}
	for _, v := range fleet.List(r["treatmentIssues"]) {
		if i := fleet.Object(s.issueView(v)); i != nil {
			original := fleet.Object(v)
			i["mergedAt"], i["closedAt"] = original["mergedAt"], original["closedAt"]
			treatments = append(treatments, i)
		}
	}
	view["treatmentIssues"] = treatments
	view["cost"], view["comparison"], view["verdict"], view["note"] = judged.cost, judged.comparison, judged.verdict, judged.note
	return view
}

func jstMidnight(at time.Time) time.Time {
	day := at.In(JST)
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, JST)
}
