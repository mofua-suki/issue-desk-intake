package statistics

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
	"time"
)

func (s *Service) localRows(ctx context.Context, table string) ([]map[string]any, error) {
	allowed := map[string]bool{"todos": true, "stall_fixes": true, "actions": true, "proposals": true, "scope_records": true, "usage_review_rounds": true, "usage_review_days": true, "inventory_rounds": true, "priority_review_rounds": true, "priority_review_days": true, "review_notes_rounds": true}
	if !allowed[table] {
		return nil, errors.New("unknown statistics table")
	}
	if s.AppDB == nil {
		return nil, errors.New("PostgreSQL backend missing")
	}
	var exists int
	if err := s.AppDB.QueryRowContext(ctx, tableExistsSQL(), table).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return []map[string]any{}, nil
	}
	rows, err := queryLocalRows(ctx, s.AppDB, table)
	if err != nil {
		return nil, err
	}
	out, _, err := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{SkipInvalid: true})
	return out, err
}
func (s *Service) Scope(ctx context.Context, q Query) (any, error) {
	records, err := s.localRows(ctx, "scope_records")
	if err != nil {
		return nil, err
	}
	logs, _, _, logErr := s.logs(ctx)
	records, skipped, first := validScopeRecords(records)
	period := func(p Period) any {
		filed := scopeFiled(records, p, q.Project, first)
		out := p.Dates()
		out["filed"] = filed
		out["checks"] = scopeChecks(logs, p, q.Project, logErr)
		return out
	}
	return map[string]any{"project": q.Project, "before": period(q.Before), "after": period(q.After), "skipped": map[string]any{"filed": skipped}}, nil
}

// validScopeRecords keeps the scope records with a time, project and known state, and finds the first.
func validScopeRecords(records []map[string]any) ([]map[string]any, int, time.Time) {
	skipped := 0
	var first time.Time
	validRecords := []map[string]any{}
	for _, r := range records {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e != nil || fleet.Text(r["project"]) == "" || !oneOf(r["state"], []string{"file", "directory", "absent", "unknown", "none"}) {
			skipped++
			continue
		}
		validRecords = append(validRecords, r)
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	return validRecords, skipped, first
}

// scopeFiled counts the period's filed scopes by state, route and reason.
func scopeFiled(records []map[string]any, p Period, project string, first time.Time) any {
	states := map[string]any{"file": 0, "directory": 0, "absent": 0, "unknown": 0, "none": 0}
	groups := map[string]map[string]any{}
	total := 0
	for _, r := range records {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e != nil || at.Before(p.From) || !at.Before(p.To) || project != "" && r["project"] != project {
			continue
		}
		state := fleet.Text(r["state"])
		if states[state] == nil {
			continue
		}
		states[state] = states[state].(int) + 1
		total++
		key := strings.Join([]string{state, fleet.Text(r["route"]), fleet.Text(r["reason"])}, "\x00")
		g := groups[key]
		if g == nil {
			g = map[string]any{"state": state, "stateName": scopeStateNames[state], "route": r["route"], "routeName": scopeRouteName(fleet.Text(r["route"])), "reason": r["reason"], "count": 0}
			groups[key] = g
		}
		g["count"] = g["count"].(int) + 1
	}
	list := []any{}
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := fleet.Object(list[i]), fleet.Object(list[j])
		order := map[string]int{"file": 0, "directory": 1, "absent": 2, "unknown": 3, "none": 4}
		if a["state"] != b["state"] {
			return order[fleet.Text(a["state"])] < order[fleet.Text(b["state"])]
		}
		if a["count"] != b["count"] {
			return a["count"].(int) > b["count"].(int)
		}
		if a["route"] != b["route"] {
			return fleet.Text(a["route"]) < fleet.Text(b["route"])
		}
		return fleet.Text(a["reason"]) < fleet.Text(b["reason"])
	})
	var filed any = map[string]any{"reason": "期間内に起票した依頼の記録がありません"}
	if total > 0 {
		var since any
		if first.After(p.From) {
			since = first.In(JST).Format("2006-01-02")
		}
		filed = map[string]any{"reason": nil, "since": since, "total": total, "states": states, "groups": list}
	}
	return filed
}

var scopeStateNames = map[string]string{"file": "ファイル単位", "directory": "ディレクトリを含む", "absent": "宣言なし", "unknown": "不明", "none": "変更なし"}

func scopeRouteName(v string) string {
	names := map[string]string{"consultation": "通常の相談", "split": "分割の案", "legacy": "導入前に作られた下書き", "proposal": "後続Issueの案", "project": "新しいプロジェクトの分割案", "usage_review": "利用量の見直しの固定本文", "stall": "止まりの直す依頼の固定本文"}
	if name := names[v]; name != "" {
		return name
	}
	return v
}
