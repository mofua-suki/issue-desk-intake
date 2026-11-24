package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

type ReworkRows struct {
	Jobs, Events, Usage []AccountRecord
	Skipped             int
}

func ReworkQuery(values url.Values, now time.Time, repositories []string) (string, Period, error) {
	project := values.Get("project")
	if project != "" && !includes(repositories, project) {
		return "", Period{}, contract.NewProblem(400, "登録されていないprojectです。", "")
	}
	for _, key := range []string{"project", "from", "to"} {
		if len(values[key]) > 1 {
			return "", Period{}, contract.NewProblem(400, "同じ期間・プロジェクトを重ねて指定できません。", "")
		}
	}
	end := values.Get("to")
	if end == "" {
		end = now.In(JST).Format("2006-01-02")
	}
	to, e := time.ParseInLocation("2006-01-02", end, JST)
	if e != nil || to.Format("2006-01-02") != end {
		return "", Period{}, contract.NewProblem(400, "期間は実在するYYYY-MM-DDで指定してください。", "")
	}
	start := values.Get("from")
	if start == "" {
		start = to.Add(-13 * 24 * time.Hour).Format("2006-01-02")
	}
	from, e := time.ParseInLocation("2006-01-02", start, JST)
	to = to.Add(24 * time.Hour)
	if e != nil || from.Format("2006-01-02") != start || !from.Before(to) || to.Sub(from) > 92*24*time.Hour {
		return "", Period{}, contract.NewProblem(400, "期間は開始日から終了日を含め1〜92日です。", "")
	}
	return project, Period{From: from, To: to}, nil
}
func readJSONRows(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]AccountRecord, error) {
	rows, e := tx.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	out, _, e := appsql.JSONObjects[[]byte](rows, appsql.JSONRowsOptions{})
	return out, e
}

// ReadReworkRows fixes the population at each dependency's actual merge event.
func ReadReworkRows(ctx context.Context, tx *sql.Tx, project string, period Period) (ReworkRows, error) {
	data := ReworkRows{Jobs: []AccountRecord{}, Events: []AccountRecord{}, Usage: []AccountRecord{}}
	jobs, e := readJSONRows(ctx, tx, `SELECT row_to_json(selected) FROM (SELECT j.*,m.at AS merged_at FROM jobs j JOIN LATERAL (SELECT at FROM job_events WHERE job_id=j.id AND kind='state' AND data->>'to'='merged' ORDER BY at DESC,seq DESC LIMIT 1)m ON true WHERE j.status='merged' AND j.pr IS NOT NULL AND j.result->>'mergeCommit' IS NOT NULL AND m.at >=$1 AND m.at<$2 AND ($3=''OR j.repo=$3) ORDER BY m.at,j.id)selected`, period.From, period.To, project)
	if e != nil {
		return data, e
	}
	data.Jobs = jobs
	for start := 0; start < len(jobs); start += 100 {
		end := start + 100
		if end > len(jobs) {
			end = len(jobs)
		}
		batch := jobs[start:end]
		ids := []int64{}
		cutoffs := map[int64]time.Time{}
		merged := map[string]time.Time{}
		var latest time.Time
		for _, job := range batch {
			id := int64(fleet.Number(job["id"]))
			ids = append(ids, id)
			cutoffs[id] = accountAt(job["merged_at"])
			merged[accountKey(job)] = accountAt(job["merged_at"])
			if cutoff := accountAt(job["merged_at"]); cutoff.After(latest) {
				latest = cutoff
			}
		}
		events, e := readJSONRows(ctx, tx, `SELECT row_to_json(e) FROM job_events e WHERE job_id=ANY($1::bigint[]) ORDER BY at,seq`, ids)
		if e != nil {
			return data, e
		}
		for _, event := range events {
			if !accountAt(event["at"]).After(cutoffs[int64(fleet.Number(event["job_id"]))]) {
				data.Events = append(data.Events, event)
			}
		}
		stored, e := tx.QueryContext(ctx, `SELECT u.key,u.json FROM usage_rows u JOIN jobs j ON j.repo=u.repo AND j.issue=u.issue WHERE j.id=ANY($1::bigint[]) AND u.at<=$2 ORDER BY u.at,u.key`, ids, latest)
		if e != nil {
			return data, e
		}
		for stored.Next() {
			var key, raw string
			if e = stored.Scan(&key, &raw); e != nil {
				stored.Close()
				return data, e
			}
			var row AccountRecord
			if json.Unmarshal([]byte(raw), &row) != nil || row == nil || accountAt(row["at"]).IsZero() {
				data.Skipped++
				continue
			}
			cutoff, found := merged[accountKey(row)]
			if found && !accountAt(row["at"]).After(cutoff) {
				data.Usage = append(data.Usage, row)
			}
		}
		e = stored.Err()
		stored.Close()
		if e != nil {
			return data, e
		}
	}
	return data, nil
}
func (s *Service) ReworkBreakdown(ctx context.Context, values url.Values, repositories []string) (any, error) {
	return s.reworkBreakdown(ctx, values, repositories, true)
}

// ReworkBreakdownForAlerts omits the implementation report and returns read failures.
func (s *Service) ReworkBreakdownForAlerts(ctx context.Context, values url.Values, repositories []string) (any, error) {
	return s.reworkBreakdown(ctx, values, repositories, false)
}

func (s *Service) reworkBreakdown(ctx context.Context, values url.Values, repositories []string, full bool) (any, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	project, period, e := ReworkQuery(values, now, repositories)
	if e != nil {
		return nil, e
	}
	result := AccountRecord{"project": project, "fromDate": period.From.In(JST).Format("2006-01-02"), "toDate": period.To.Add(-time.Millisecond).In(JST).Format("2006-01-02"), "breakdown": nil, "implementation": nil}
	unavailable := func(reason string) (any, error) {
		if !full {
			return nil, fmt.Errorf("%s", reason)
		}
		result["unavailable"] = []any{reason}
		return result, nil
	}
	if s.DB == nil {
		return unavailable("共有DBがありません。")
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return unavailable("共有DBを読めませんでした。")
	}
	defer tx.Rollback()
	data, e := ReadReworkRows(ctx, tx, project, period)
	if e != nil {
		return unavailable("共有DBを読めませんでした。")
	}
	selected := repositories
	if project != "" {
		selected = []string{project}
	}
	var implementation AccountRecord
	if full {
		implementation, e = readImplementationBreakdown(ctx, tx, selected, period, now)
		if e != nil {
			return unavailable("共有DBを読めませんでした。")
		}
	}
	if e = tx.Commit(); e != nil {
		return unavailable("共有DBを読めませんでした。")
	}
	p := AccountRecord{"fromDate": result["fromDate"], "toDate": result["toDate"], "from": ISO(period.From), "to": ISO(period.To)}
	if e := validateBreakdownWeights(s.Weights); e != nil {
		return nil, e
	}
	result["unavailable"] = []any{}
	result["breakdown"] = BuildReworkBreakdown(data.Jobs, data.Events, data.Usage, s.Weights, p, data.Skipped)
	result["implementation"] = implementation
	return result, nil
}
