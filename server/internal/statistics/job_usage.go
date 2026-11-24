package statistics

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"time"
)

type jobUsage struct {
	Repo  string
	Issue int64
	Filed time.Time
	Scope []string
	View  map[string]any
	Rows  []map[string]any
}

func (s *Service) jobUsage(ctx context.Context, q Query) ([]jobUsage, int, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT repo,issue,filed_at,array_to_json(scope),jsonb_build_object('estimate',view->'estimate','workType',view->'workType')FROM jobs WHERE($1=''OR repo=$1)ORDER BY id", q.Project)
	if err != nil {
		return nil, 0, err
	}
	jobs := []jobUsage{}
	for rows.Next() {
		j := jobUsage{}
		var scope, view []byte
		if err = rows.Scan(&j.Repo, &j.Issue, &j.Filed, &scope, &view); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if json.Unmarshal(scope, &j.Scope) != nil || json.Unmarshal(view, &j.View) != nil {
			continue
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	to := now.Add(time.Second)
	if q.Before.To.After(to) {
		to = q.Before.To
	}
	if q.After.To.After(to) {
		to = q.After.To
	}
	all, skipped, err := (&usage.Store{DB: s.DB}).Read(ctx, q.Project, time.Unix(0, 0), to)
	if err != nil {
		return nil, 0, err
	}
	worker := []map[string]any{}
	for _, r := range all {
		if r["source"] == "worker" {
			worker = append(worker, r)
		}
	}
	worker, _ = usage.CorrectCosts(worker, []map[string]any{})
	byIssue := map[string][]map[string]any{}
	for _, r := range worker {
		key := fmt.Sprint(r["repo"], "#", r["issue"])
		byIssue[key] = append(byIssue[key], r)
	}
	for i := range jobs {
		j := &jobs[i]
		j.Rows = byIssue[fmt.Sprintf("%s#%d", j.Repo, j.Issue)]
	}
	return jobs, skipped, nil
}

var workTypeNames = map[string]string{"bugfix": "不具合の修正", "feature": "機能追加", "refactor": "作り替え", "docs": "資料", "test": "テスト", "operations": "運用", "investigation": "調査", "unknown": "不明"}
var workSizeNames = map[string]string{"1-2": "1〜2", "3-4": "3〜4", "5-7": "5〜7", "8+": "8以上", "all": "全体（*）", "none": "変更なし（調査など）", "unknown": "不明（宣言なし）"}

func workSize(j jobUsage) string {
	for _, p := range j.Scope {
		if p == "*" {
			return "all"
		}
		if p == "" {
			return "unknown"
		}
	}
	seen := map[string]bool{}
	for _, p := range j.Scope {
		seen[p] = true
	}
	switch n := len(seen); {
	case n == 0:
		return "none"
	case n <= 2:
		return "1-2"
	case n <= 4:
		return "3-4"
	case n <= 7:
		return "5-7"
	default:
		return "8+"
	}
}
func (s *Service) WorkTypes(ctx context.Context, q Query) (any, error) {
	all, skipped, err := s.jobUsage(ctx, q)
	if err != nil {
		return nil, err
	}
	return map[string]any{"project": q.Project, "unavailable": []any{}, "skipped": map[string]any{"jobs": 0, "usage": skipped}, "before": s.workTypePeriod(all, q.Before), "after": s.workTypePeriod(all, q.After)}, nil
}

// workTypePeriod summarizes the issues filed in the period by work type and size.
func (s *Service) workTypePeriod(all []jobUsage, p Period) any {
	mine := []jobUsage{}
	for _, j := range all {
		if !j.Filed.Before(p.From) && j.Filed.Before(p.To) {
			mine = append(mine, j)
		}
	}
	types, sizes, groups := []any{}, []any{}, []any{}
	for _, kind := range []string{"bugfix", "feature", "refactor", "docs", "test", "operations", "investigation", "unknown"} {
		g := s.workGroup(mine, kind, "")
		if g["count"].(int) > 0 {
			types = append(types, g)
		}
		for _, size := range []string{"1-2", "3-4", "5-7", "8+", "all", "none", "unknown"} {
			g := s.workGroup(mine, kind, size)
			if g["count"].(int) > 0 {
				groups = append(groups, g)
			}
		}
	}
	for _, size := range []string{"1-2", "3-4", "5-7", "8+", "all", "none", "unknown"} {
		g := s.workGroup(mine, "", size)
		if g["count"].(int) > 0 {
			sizes = append(sizes, g)
		}
	}
	out := p.Dates()
	total := s.workSummary(mine)
	out["reason"], out["total"], out["groups"], out["types"], out["sizes"] = total["reason"], total, groups, types, sizes
	return out
}

// workSummary summarizes the usage and the generation triggers of the issues.
func (s *Service) workSummary(jobs []jobUsage) map[string]any {
	recorded := 0
	rows := []map[string]any{}
	unknownTriggers := 0
	for _, j := range jobs {
		if len(j.Rows) > 0 {
			recorded++
			rows = append(rows, j.Rows...)
			known := true
			found := false
			for _, r := range j.Rows {
				if r["step"] != "実装" && r["step"] != "競合解消" {
					continue
				}
				found = true
				v, exists := r["generationTrigger"]
				if !exists || v == "unknown" || v == nil && r["generation"] != 1.0 {
					known = false
				}
			}
			if !known || !found {
				unknownTriggers++
			}
		}
	}
	totals := usage.Totals(rows, s.Weights)
	var reason any
	if len(jobs) == 0 {
		reason = "この期間に起票した依頼がありません。"
	} else if recorded == 0 {
		reason = "使用量の行のある依頼がありません。"
	}
	return map[string]any{"count": len(jobs), "usageIssues": recorded, "missingUsage": len(jobs) - recorded, "unknownTriggers": unknownTriggers, "reviewReturns": workTriggerRate(jobs, "review-return"), "retries": workTriggerRate(jobs, "retry"), "retests": workTriggerRate(jobs, "retest"), "perIssue": perIssue(totals, recorded), "reason": reason}
}

// workTriggerRate is the share of the issues whose implementation was started again by the trigger.
func workTriggerRate(jobs []jobUsage, trigger string) any {
	count, denominator, unknown := 0, 0, 0
	for _, j := range jobs {
		if len(j.Rows) == 0 {
			continue
		}
		found, known, hasRows := false, true, false
		for _, r := range j.Rows {
			if r["step"] != "実装" && r["step"] != "競合解消" {
				continue
			}
			hasRows = true
			v, exists := r["generationTrigger"]
			if v == trigger {
				found = true
			}
			if !exists || v == "unknown" || v == nil && r["generation"] != 1.0 {
				known = false
			}
		}
		if found {
			count++
			denominator++
		} else if hasRows && known {
			denominator++
		} else {
			unknown++
		}
	}
	var rate, why any
	if denominator > 0 {
		rate = float64(count) / float64(denominator)
	} else {
		why = "きっかけを判定できる使用量の記録がありません。"
	}
	return map[string]any{"count": count, "denominator": denominator, "unknown": unknown, "rate": rate, "reason": why}
}

// workGroup summarizes the issues of the work type and size; an empty value matches any.
func (s *Service) workGroup(mine []jobUsage, kind, size string) map[string]any {
	list := []jobUsage{}
	for _, j := range mine {
		t := fleet.Text(j.View["workType"])
		if workTypeNames[t] == "" {
			t = "unknown"
		}
		if (kind == "" || kind == t) && (size == "" || size == workSize(j)) {
			list = append(list, j)
		}
	}
	out := s.workSummary(list)
	var typ, siz, typName, sizName any
	if kind != "" {
		typ, typName = kind, workTypeNames[kind]
	}
	if size != "" {
		siz, sizName = size, workSizeNames[size]
	}
	out["type"], out["typeName"], out["size"], out["sizeName"] = typ, typName, siz, sizName
	return out
}

func validEstimate(value any) map[string]any {
	v := fleet.Object(value)
	for _, name := range []string{"lines", "files"} {
		if numeric(v[name]) == nil {
			return nil
		}
	}
	return v
}
func (s *Service) Sizes(ctx context.Context, q Query) (any, error) {
	jobs, skipped, err := s.jobUsage(ctx, q)
	if err != nil {
		return nil, err
	}
	risk := map[string]any{"riskLines": 500.0, "riskFiles": 16.0}
	for _, key := range []string{"riskLines", "riskFiles"} {
		if n, ok := s.Risk[key].(float64); ok {
			risk[key] = n
		}
	}
	return map[string]any{"project": q.Project, "thresholds": risk, "unavailable": []any{}, "skipped": map[string]any{"jobs": 0, "usage": skipped}, "before": sizePeriod(jobs, risk, q.Before), "after": sizePeriod(jobs, risk, q.After)}, nil
}

// sizePeriod compares the estimated and reviewed sizes of the issues filed in the period.
func sizePeriod(jobs []jobUsage, risk map[string]any, p Period) any {
	entries := []jobUsage{}
	actual := map[string]map[string]any{}
	for _, j := range jobs {
		if j.Filed.Before(p.From) || !j.Filed.Before(p.To) {
			continue
		}
		entries = append(entries, j)
		for _, row := range j.Rows {
			if row["step"] == "独立レビュー" {
				actual[fmt.Sprintf("%s#%d", j.Repo, j.Issue)] = row
			}
		}
	}
	lines, files := fleet.Object(sizeMetric(entries, actual, risk, "lines")), fleet.Object(sizeMetric(entries, actual, risk, "files"))
	paired, missingEstimate, missingActual := 0, 0, 0
	for _, j := range entries {
		estimate := validEstimate(j.View["estimate"])
		row := actual[fmt.Sprintf("%s#%d", j.Repo, j.Issue)]
		_, lOK := row["reviewDiffLines"].(float64)
		_, fOK := row["reviewDiffFiles"].(float64)
		if estimate == nil {
			missingEstimate++
		}
		if !lOK || !fOK {
			missingActual++
		}
		if estimate != nil && lOK && fOK {
			paired++
		}
	}
	out := p.Dates()
	var reason any
	if len(entries) == 0 {
		reason = "対象の依頼がありません。"
	}
	out["count"], out["paired"], out["missingEstimate"], out["missingActual"], out["lines"], out["files"], out["reason"] = len(entries), paired, missingEstimate, missingActual, lines, files, reason
	return out
}

// sizeMetric compares the estimate of one size metric with the reviewed diff.
func sizeMetric(entries []jobUsage, actual map[string]map[string]any, risk map[string]any, name string) any {
	paired, missingEstimate, missingActual, double, under := 0, 0, 0, 0, 0
	ratios := []float64{}
	for _, j := range entries {
		estimate := validEstimate(j.View["estimate"])
		row := actual[fmt.Sprintf("%s#%d", j.Repo, j.Issue)]
		key := "reviewDiffLines"
		if name == "files" {
			key = "reviewDiffFiles"
		}
		value, ok := row[key].(float64)
		if estimate == nil {
			missingEstimate++
		}
		if !ok {
			missingActual++
		}
		if estimate == nil || !ok {
			continue
		}
		paired++
		expected := fleet.Number(estimate[name])
		if expected > 0 {
			ratios = append(ratios, value/expected)
			if value >= 2*expected {
				double++
			}
		}
		threshold := fleet.Number(risk["riskLines"])
		if name == "files" {
			threshold = fleet.Number(risk["riskFiles"])
			if expected < threshold && value >= threshold {
				under++
			}
		} else if expected <= threshold && value > threshold {
			under++
		}
	}
	var mean any
	if len(ratios) > 0 {
		sum := 0.0
		for _, r := range ratios {
			sum += r
		}
		mean = sum / float64(len(ratios))
	}
	return map[string]any{"paired": paired, "missingEstimate": missingEstimate, "missingActual": missingActual, "ratioCount": len(ratios), "medianRatio": percentile(ratios, 0.5), "meanRatio": mean, "atLeastDouble": double, "underestimatedRisk": under}
}
