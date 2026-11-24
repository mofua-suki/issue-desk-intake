package statistics

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var dailyName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.json$`)

func (s *Service) issueView(value any) any {
	v := fleet.Object(value)
	repo := fleet.Text(v["project"])
	number := fleet.Number(v["number"])
	if !fleet.ValidRepo(repo) || number < 1 || number != float64(int64(number)) {
		return nil
	}
	var desk any
	if s.Desk != nil {
		desk = s.Desk(repo, int64(number))
	}
	return map[string]any{"project": repo, "number": number, "desk": desk}
}
func (s *Service) PriorityReview(ctx context.Context) (any, error) {
	records, e := s.localRows(ctx, "priority_review_rounds")
	if e != nil {
		return nil, e
	}
	rounds := s.priorityRounds(records)
	daily, note, reason, elsewhere := s.priorityDaily()
	return map[string]any{"reason": reason, "daily": daily, "dailyNote": note, "elsewhere": elsewhere, "categoryNames": map[string]string{"stall": "止まりの解消", "manual": "手作業の解消", "misoperation": "誤操作の防止", "monitoring": "監視と復旧", "throughput": "処理量の改善", "future": "将来の備え", "cleanup": "内部整理", "other": "その他"}, "rounds": rounds}, nil
}

// priorityRounds shows the review rounds in order with their issues.
func (s *Service) priorityRounds(records []map[string]any) []any {
	rounds := []any{}
	sort.Slice(records, func(i, j int) bool { return fleet.Number(records[i]["round"]) < fleet.Number(records[j]["round"]) })
	for _, r := range records {
		view := map[string]any{}
		for _, key := range strings.Fields("round status date startedAt autoTurnAt closedAt closeReason") {
			view[key] = r[key]
		}
		view["startReasons"] = textArray(r["startReasons"])
		view["investigationIssue"] = s.issueView(r["investigationIssue"])
		view["noAction"] = r["noAction"] == true
		view["held"] = len(fleet.List(r["held"]))
		issues := []any{}
		for _, item := range fleet.List(r["filedIssues"]) {
			if i := s.issueView(item); i != nil {
				issues = append(issues, i)
			}
		}
		view["filedIssues"] = issues
		rounds = append(rounds, view)
	}
	return rounds
}

// priorityDaily reads the latest daily record with a priority section. When the
// review runs on another PC, its records are not read here.
func (s *Service) priorityDaily() (daily, note, reason, elsewhere any) {
	if s.UsageReviewHost != "" && s.UsageReviewHost != "nas" {
		elsewhere = s.UsageReviewHost
		note = s.UsageReviewHost + " の記録を読めないため対象外（共有DBまで " + s.UsageReviewHost + " の分は集めません）"
	} else {
		dir := filepath.Join(s.RuntimeDir, "state", "usage-review", "daily")
		files, e := os.ReadDir(dir)
		if e != nil && !os.IsNotExist(e) {
			reason = "優先度の見直しの日次記録を読めませんでした"
		}
		names := []string{}
		for _, f := range files {
			if !f.IsDir() && dailyName.MatchString(f.Name()) {
				names = append(names, f.Name())
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(names)))
		if len(names) > 7 {
			names = names[:7]
		}
		for _, name := range names {
			raw, e := os.ReadFile(filepath.Join(dir, name))
			if e != nil {
				reason = "優先度の見直しの日次記録を読めませんでした"
				continue
			}
			var record map[string]any
			if json.Unmarshal(raw, &record) != nil || record["v"] != float64(1) || record["date"] != strings.TrimSuffix(name, ".json") {
				reason = "優先度の見直しの日次記録の形式が不正です"
				continue
			}
			p := fleet.Object(record["priority"])
			if p == nil {
				if record["priority"] != nil {
					reason = "優先度の見直しの日次記録の優先度の段が不正です"
				}
				continue
			}
			daily = fleet.Clone(p)
			daily.(map[string]any)["date"] = record["date"]
			break
		}
		if daily == nil {
			note = "直近7件の日次記録に優先度の段がありません"
		}
	}
	return daily, note, reason, elsewhere
}
func textArray(value any) []any {
	out := []any{}
	for _, v := range fleet.List(value) {
		if s, ok := v.(string); ok {
			out = append(out, usage.Redact(s))
		}
	}
	return out
}
