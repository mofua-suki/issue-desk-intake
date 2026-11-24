package issueapi

import (
	"context"
	"database/sql"
	"time"
)

func (s *Service) dependencyWaits(ctx context.Context, jobID int64) ([]any, error) {
	waits := []any{}
	rows, err := s.o.Postgres.QueryContext(ctx, `SELECT p.repo, p.issue, d.wait_started_at, d.wait_unknown FROM job_deps d JOIN jobs p ON p.id=d.depends_on WHERE d.job_id=$1 AND (d.wait_started_at IS NOT NULL OR d.wait_unknown) ORDER BY p.repo, p.issue, p.id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var number int64
		var started sql.NullTime
		var unknown bool
		if err := rows.Scan(&project, &number, &started, &unknown); err != nil {
			return nil, err
		}
		var at any
		if started.Valid {
			at = started.Time.UTC().Format(time.RFC3339Nano)
		}
		waits = append(waits, Object{"project": project, "number": number, "startedAt": at, "unknown": unknown})
	}
	return waits, rows.Err()
}
