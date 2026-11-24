package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"time"
)

func (s *Service) usageStaleAlerts(ctx context.Context) ([]any, error) {
	out := []any{}
	if s.PostgreSQL == nil {
		return out, nil
	}
	now := s.now()
	store := usage.Store{DB: s.PostgreSQL}
	calls, _, e := store.Read(ctx, "", now.Add(-6*time.Hour), now.Add(time.Millisecond))
	if e != nil {
		return nil, e
	}
	rows, e := s.PostgreSQL.QueryContext(ctx, "SELECT json FROM usage_limit_rows WHERE at >= $1 AND at < $2 ORDER BY at", now.Add(-30*24*time.Hour), now.Add(time.Millisecond))
	if e != nil {
		return nil, e
	}
	limits, _, e := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{SkipInvalid: true})
	if e != nil {
		return nil, e
	}
	providerOf := func(r map[string]any) string {
		p := fleet.Text(r["provider"])
		if p == "" {
			return "claude"
		}
		return p
	}
	for _, provider := range []string{"claude", "codex"} {
		count := 0
		for _, r := range calls {
			if providerOf(r) == provider {
				count++
			}
		}
		if count == 0 {
			continue
		}
		last := time.Time{}
		for _, r := range limits {
			_, numeric := r["percent"].(float64)
			at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
			if e == nil && numeric && r["limit"] == "seven_day" && providerOf(r) == provider && at.After(last) {
				last = at
			}
		}
		if !last.IsZero() && !last.Before(now.Add(-6*time.Hour)) {
			continue
		}
		lastText := "直近の記録なし"
		if !last.IsZero() {
			lastText = last.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04:05") + "（日本時間）"
		}
		name := map[string]string{"claude": "Claude", "codex": "Codex"}[provider]
		out = append(out, map[string]any{"id": "usage-limit-stale-" + provider, "kind": "usage_limit_stale", "status": "stall_alert", "project": fleet.SystemProject, "detectedAt": iso(now), "title": name + " の上限の使用率の記録が止まっています", "detail": fmt.Sprintf("実行先: %s\n最後に記録できた時刻: %s\n直近6時間の使用量の行: %d件\n直近6時間に、percent のある seven_day の上限の行が記録されていません。新しい行が記録されると、この要対応は消えます。", provider, lastText, count)})
	}
	return out, nil
}
