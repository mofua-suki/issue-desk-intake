package statistics

import (
	"context"
	"database/sql"
)

func (s *Service) Throughput(ctx context.Context, q Query) (any, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var coverage sql.NullTime
	if err = tx.QueryRowContext(ctx, "SELECT min(at) FROM job_events").Scan(&coverage); err != nil {
		return nil, err
	}
	period := func(p Period) (any, error) {
		out := map[string]any{"fromAt": p.From.In(JST).Format("2006-01-02T15:04"), "toAt": p.To.In(JST).Format("2006-01-02T15:04")}
		if !coverage.Valid || p.From.Before(coverage.Time) {
			out["status"] = "missing"
			out["reason"] = "共有DBの記録が期間全体をカバーしていません。"
			for _, key := range []string{"filed", "completed", "closedOther", "openAtStart", "openAtEnd", "rate", "rateWithCarryover"} {
				out[key] = nil
			}
			return out, nil
		}
		var filed, completed, closedOther, openStart, openEnd int64
		err := tx.QueryRowContext(ctx, `WITH terminals AS(SELECT j.filed_at,e.merged,e.closed FROM jobs j CROSS JOIN LATERAL(SELECT min(at)FILTER(WHERE kind='state'AND data->>'to'='merged')AS merged,min(at)FILTER(WHERE kind='closed'OR data->>'to'='closed')AS closed FROM job_events WHERE job_id=j.id)e WHERE($3::text=''OR j.repo=$3))SELECT count(*)FILTER(WHERE filed_at>=$1 AND filed_at<$2),count(*)FILTER(WHERE merged>=$1 AND merged<$2),count(*)FILTER(WHERE closed>=$1 AND closed<$2 AND(merged IS NULL OR merged>closed)),count(*)FILTER(WHERE filed_at<$1 AND(merged IS NULL OR merged>=$1)AND(closed IS NULL OR closed>=$1)),count(*)FILTER(WHERE filed_at<$2 AND(merged IS NULL OR merged>=$2)AND(closed IS NULL OR closed>=$2))FROM terminals`, p.From, p.To, q.Project).Scan(&filed, &completed, &closedOther, &openStart, &openEnd)
		if err != nil {
			return nil, err
		}
		out["status"], out["reason"] = "measured", nil
		out["filed"], out["completed"], out["closedOther"], out["openAtStart"], out["openAtEnd"] = filed, completed, closedOther, openStart, openEnd
		out["rate"], out["rateWithCarryover"] = nil, nil
		if filed > 0 {
			out["rate"] = float64(completed) / float64(filed)
		}
		if openStart+filed > 0 {
			out["rateWithCarryover"] = float64(completed) / float64(openStart+filed)
		}
		return out, nil
	}
	before, err := period(q.Before)
	if err != nil {
		return nil, err
	}
	after, err := period(q.After)
	if err != nil {
		return nil, err
	}
	var at any
	if coverage.Valid {
		at = ISO(coverage.Time)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"project": q.Project, "source": "shared-db", "unavailable": []any{}, "coverage": at, "before": before, "after": after}, nil
}
