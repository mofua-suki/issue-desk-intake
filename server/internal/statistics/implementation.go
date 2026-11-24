package statistics

import (
	"context"
	"database/sql"
	"time"
)

func reasonCounts() map[string]any {
	out := map[string]any{}
	for _, key := range endReasons {
		out[key] = 0
	}
	return out
}
func unknownCounts() map[string]any {
	return map[string]any{"generationUnidentified": 0, "inProgress": 0, "reasonNotRecorded": 0, "unclassified": 0}
}
func (s *Service) Implementation(ctx context.Context, q Query) (any, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	repositories := []string{}
	if q.Project != "" {
		repositories = append(repositories, q.Project)
	}
	data, err := readImplementationRows(ctx, tx, repositories, q.Project == "")
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	to := q.Before.To
	if q.After.To.After(to) {
		to = q.After.To
	}
	before, after := data.build(q.Before, now), data.build(q.After, now)
	before["noImplementationRecord"] = data.noRecordBefore(to)
	after["noImplementationRecord"] = before["noImplementationRecord"]
	return map[string]any{"before": implementationPublic(before), "after": implementationPublic(after)}, nil
}

// implementationPublic preserves the comparison endpoint's Go value shapes.
func implementationPublic(result AccountRecord) AccountRecord {
	for _, key := range []string{"days", "issues", "distribution", "uc1Distribution"} {
		rows := result[key].([]AccountRecord)
		values := make([]any, 0, len(rows))
		for _, row := range rows {
			if key == "distribution" || key == "uc1Distribution" {
				row["implementations"] = int64(row["implementations"].(int))
			}
			values = append(values, row)
		}
		result[key] = values
	}
	return result
}
