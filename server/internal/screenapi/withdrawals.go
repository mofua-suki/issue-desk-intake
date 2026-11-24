package screenapi

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"net/url"
	"time"
)

func (s *Service) withdrawals(ctx context.Context, q url.Values) (Object, error) {
	jst := time.FixedZone("JST", 9*60*60)
	now := s.o.Now().In(jst)
	parse := func(value, fallback string) (time.Time, error) {
		if value == "" {
			value = fallback
		}
		t, e := time.ParseInLocation("2006-01-02", value, jst)
		if e != nil || t.Format("2006-01-02") != value {
			return time.Time{}, fail(400, "期間はYYYY-MM-DDで指定してください。")
		}
		return t, nil
	}
	to, e := parse(q.Get("to"), now.Format("2006-01-02"))
	if e != nil {
		return nil, e
	}
	to = to.AddDate(0, 0, 1)
	from, e := parse(q.Get("from"), to.AddDate(0, 0, -7).Format("2006-01-02"))
	if e != nil {
		return nil, e
	}
	if !from.Before(to) || to.Sub(from) > 92*24*time.Hour {
		return nil, fail(400, "期間は終了日を含む1〜92日で指定してください。")
	}
	project := q.Get("project")
	if q.Has("project") && !repositoryPattern.MatchString(project) {
		return nil, fail(400, "projectはowner/repoで指定してください。")
	}
	rows, e := s.o.AppDB.QueryContext(ctx, bindSQL(`SELECT draft,project,title,operation,at,created_at,via,token_name,reason,parent FROM draft_withdrawals WHERE at>=? AND at<? AND (?='' OR project=?) ORDER BY at,sequence`), contract.ISO(from), contract.ISO(to), project, project)
	if e != nil {
		return nil, e
	}
	records := []Object{}
	for rows.Next() {
		var draft, operation, at, via string
		var project, title, created, token, reason, parent sql.NullString
		if e = rows.Scan(&draft, &project, &title, &operation, &at, &created, &via, &token, &reason, &parent); e != nil {
			rows.Close()
			return nil, e
		}
		nullable := func(v sql.NullString) any {
			if !v.Valid {
				return nil
			}
			return v.String
		}
		records = append(records, Object{"draft": draft, "project": nullable(project), "title": nullable(title), "operation": operation, "at": at, "createdAt": nullable(created), "via": via, "tokenName": nullable(token), "reason": nullable(reason), "parent": nullable(parent), "used": nil})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if s.o.Usage == nil {
		return Object{"records": records, "reason": "相談の使用量を取得する経路が設定されていません。"}, nil
	}
	usageRows, e := s.o.Usage(ctx, time.Unix(0, 0), to)
	if e != nil {
		return Object{"records": records, "reason": "相談の使用量を取得できません。"}, nil
	}
	if e = applyWithdrawalUsage(records, usageRows, s.o.UsageWeights, nil); e != nil {
		return nil, e
	}
	return Object{"records": records, "reason": nil}, nil
}

type withdrawalUsage struct {
	at       time.Time
	weighted float64
	known    bool
}

// Keep each draft's rows in source order: changing addition order changes float results.
// scanned, when non-nil, counts visits to the complete input for regression tests.
func applyWithdrawalUsage(records, usageRows []Object, weights map[string]any, scanned *int) error {
	drafts := make(map[string][]withdrawalUsage, len(records))
	for _, r := range records {
		drafts[fleet.Text(r["draft"])] = nil
	}
	for _, u := range usageRows {
		if scanned != nil {
			*scanned++
		}
		if u["source"] != "intake" {
			continue
		}
		draft, ok := u["draft"].(string)
		if !ok {
			continue
		}
		if _, ok = drafts[draft]; !ok {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(u["at"]))
		if e != nil {
			continue
		}
		value, known := usage.Weighted(u, weights)["weighted"].(float64)
		drafts[draft] = append(drafts[draft], withdrawalUsage{at, value, known})
	}
	for _, r := range records {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e != nil {
			return fail(503, "操作時刻を確認できません。")
		}
		used := Object{"rows": 0, "weighted": nil, "missing": 0}
		for _, u := range drafts[fleet.Text(r["draft"])] {
			if u.at.After(at) {
				continue
			}
			used["rows"] = intValue(used["rows"]) + 1
			if u.known {
				previous, _ := used["weighted"].(float64)
				used["weighted"] = previous + u.weighted
			} else {
				used["missing"] = intValue(used["missing"]) + 1
			}
		}
		r["used"] = used
	}
	return nil
}
