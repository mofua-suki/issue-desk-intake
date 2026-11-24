package statistics

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/anomalies"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/giteareads"
	"sort"
	"time"
)

func (s *Service) Saved(ctx context.Context, name string, q Query) (any, error) {
	switch name {
	case "stopChecks":
		return s.StopChecks(ctx)
	case "testSelection":
		return s.TestSelection(ctx, q)
	case "reviewNotes":
		return s.ReviewNotes(ctx, q)
	case "antigravity":
		return map[string]any{"reason": "監視の内部状態は読みません。実行先別の比較は手戻りの統計を参照してください。", "groups": nil}, nil
	case "issueBody":
		return map[string]any{"reason": "監視の内部状態を読まないため、起票本文の前後比較は集計できません。", "notes": []string{"仕事の内容（依頼の大きさ・難しさ）の違いも値に表れるので、導入前と導入後の差だけで効果とは断定できません。"}, "groups": nil}, nil
	case "waits":
		return s.Waits(ctx, q)
	case "usageReview":
		return s.UsageReview(ctx)
	case "priorityReview":
		return s.PriorityReview(ctx)
	case "filingCheck":
		return s.FilingCheck(ctx, q)
	case "giteaReads":
		return giteareads.Process.Report(), nil
	case "workTypes":
		return s.WorkTypes(ctx, q)
	case "sizeEstimates":
		return s.Sizes(ctx, q)
	case "usageLimits":
		return s.Limits(ctx)
	case "inventory":
		return s.Inventory(ctx)
	case "tiers":
		return s.Tiers(ctx, q)
	case "followups":
		return s.Followups(ctx, q)
	case "consultationBudget", "inputBudget":
		return s.Budget(ctx, name, q)
	case "throughput":
		return s.Throughput(ctx, q)
	case "stalls":
		return s.Stalls(ctx, q)
	}
	return nil, errors.New("statistics calculation unavailable: " + name)
}
func (s *Service) Limits(ctx context.Context) (any, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT json FROM usage_limit_rows WHERE at>=$1 AND at<$2 ORDER BY at,key", now.Add(-30*24*time.Hour), now.Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	all, skipped, err := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{SkipInvalid: true})
	if err != nil {
		return nil, err
	}
	latest := map[string]map[string]any{}
	for _, row := range all {
		provider := fleet.Text(row["provider"])
		if provider == "" {
			provider = "claude"
		}
		key := identity(provider, row["limit"])
		latest[key] = map[string]any{"provider": provider, "limit": row["limit"], "percent": row["percent"], "windowMinutes": row["windowMinutes"], "resetsAt": row["resetsAt"], "at": row["at"], "host": row["host"]}
	}
	keys := []string{}
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []any{}
	for _, key := range keys {
		out = append(out, latest[key])
	}
	var reason any
	if len(out) == 0 {
		reason = "直近30日に上限の使用率の記録がありません"
	}
	return map[string]any{"reason": reason, "rows": out, "skipped": skipped, "windows": anomalies.LimitTimeline(all)}, nil
}
func (s *Service) Inventory(ctx context.Context) (any, error) {
	records, err := s.localRows(ctx, "inventory_rounds")
	if err != nil {
		return nil, err
	}
	rounds := map[float64]map[string]any{}
	for _, r := range records {
		number := fleet.Number(r["round"])
		round := rounds[number]
		if round == nil {
			round = map[string]any{"round": number, "week": r["week"], "date": r["date"], "projects": []any{}}
			rounds[number] = round
		}
		listed := map[float64]bool{}
		for _, raw := range fleet.List(r["listed"]) {
			listed[fleet.Number(fleet.Object(raw)["number"])] = true
		}
		project := map[string]any{"project": r["project"], "investigationIssue": r["investigationIssue"], "examined": len(listed), "total": r["total"], "rest": r["rest"], "status": "waiting", "closed": nil, "escalated": nil, "kept": nil, "pending": nil, "undetermined": nil}
		p := fleet.Object(r["processing"])
		if p == nil {
			if r["status"] == "closed" {
				project["status"] = "closed"
			}
		} else {
			project["status"] = "processing"
			if p["finishedAt"] != nil {
				project["status"] = "done"
			}
			resolved, duplicate, escalated, kept, pending := 0, 0, 0, 0, 0
			for _, raw := range fleet.List(p["items"]) {
				item := fleet.Object(raw)
				if !listed[fleet.Number(item["target"])] {
					continue
				}
				switch item["status"] {
				case "closed":
					if item["category"] == "resolved" {
						resolved++
					} else if item["category"] == "duplicate" {
						duplicate++
					}
				case "escalated":
					escalated++
				case "kept":
					kept++
				case "pending", "closing":
					pending++
				}
			}
			project["closed"] = map[string]any{"resolved": resolved, "duplicate": duplicate, "total": resolved + duplicate}
			project["escalated"], project["kept"], project["pending"], project["undetermined"] = escalated, kept, pending, len(listed)-resolved-duplicate-escalated-kept-pending
		}
		round["projects"] = append(fleet.List(round["projects"]), project)
	}
	keys := []float64{}
	for key := range rounds {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(keys)))
	out := []any{}
	for _, key := range keys {
		out = append(out, rounds[key])
	}
	return map[string]any{"reason": nil, "rounds": out}, nil
}
