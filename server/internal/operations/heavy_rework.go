package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/timedcache"
	"math"
	"net/url"
	"time"
)

func HeavyRework(tier, period map[string]any, unavailable []any) []any {
	out := []any{}
	issues := fleet.Number(tier["issues"])
	count := issues - fleet.Number(tier["unknownIssues"])
	rate, ok := tier["reworkRate"].(float64)
	if len(unavailable) > 0 || tier["key"] != "heavy" || tier["issues"] == nil || math.Trunc(issues) != issues || count < 3 || !ok || math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0.5 || rate > 1 {
		return out
	}
	return append(out, map[string]any{"id": "heavy-rework", "key": "heavy-rework", "kind": "heavy_rework", "kindName": "高の手戻り率", "title": "起票時の難易度「高」の手戻り率がしきい値を超えています", "stateLabel": "見直しが必要", "period": period, "days": 14, "count": count, "reworkRate": rate, "threshold": 0.5, "reason": fmt.Sprintf("直近14日の起票時「高」の判定可能なマージ%.0f件、実装再試行を分子とする手戻り率%.1f%%（しきい値50%%超）。率が下がると自動で消えます。", count, rate*100)})
}

// heavyReworkTTL is how long the heavy rework alert is kept. The alert is read
// with every list poll, and its 14 days of merges change slowly.
const heavyReworkTTL = 5 * time.Minute

func copyAlerts(rows []any) []any {
	out, _ := timedcache.CopyValue(rows).([]any)
	return out
}

// InitHeavyReworkCache prepares shared state before a Service is copied.
// Without it every call reads the rework statistics.
func (s *Service) InitHeavyReworkCache() {
	if s.reworkCache == nil {
		s.reworkCache = timedcache.New(heavyReworkTTL, copyAlerts)
	}
}

// heavyReworkAlerts returns the alert of the last 14 days. A result kept for
// the same period, repositories and weights is returned without reading again.
func (s *Service) heavyReworkAlerts(ctx context.Context) ([]any, error) {
	if s.Statistics == nil {
		return nil, fmt.Errorf("手戻り統計サービスがありません。")
	}
	at := s.now()
	now := at.In(time.FixedZone("JST", 9*3600))
	values := url.Values{"from": {now.AddDate(0, 0, -13).Format("2006-01-02")}, "to": {now.Format("2006-01-02")}}
	repos := []string{}
	if s.Repositories != nil {
		var e error
		repos, e = s.Repositories()
		if e != nil {
			return nil, e
		}
	}
	key := ""
	if raw, e := json.Marshal([]any{values, repos, s.Statistics.Weights}); e == nil {
		key = string(raw)
	}
	done, e := s.reworkCache.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	if kept, ok := s.reworkCache.Get(key, at); ok {
		return kept, nil
	}
	alerts, e := s.readHeavyReworkAlerts(ctx, values, repos)
	if e != nil {
		return nil, e
	}
	s.reworkCache.Put(key, alerts, at)
	return alerts, nil
}

// readHeavyReworkAlerts computes the alert from the rework statistics.
func (s *Service) readHeavyReworkAlerts(ctx context.Context, values url.Values, repos []string) ([]any, error) {
	report, e := s.Statistics.ReworkBreakdownForAlerts(ctx, values, repos)
	if e != nil {
		return nil, e
	}
	r := fleet.Object(report)
	breakdown := fleet.Object(r["breakdown"])
	for _, raw := range fleet.List(breakdown["tiers"]) {
		tier := fleet.Object(raw)
		if tier["key"] == "heavy" {
			return HeavyRework(tier, fleet.Object(breakdown["period"]), fleet.List(r["unavailable"])), nil
		}
	}
	return []any{}, nil
}
