package jobs

import (
	"context"
	"database/sql"
)

// ForFiling reads the complete active comparison set and its dependency edges
// from one snapshot. Comparison material does not need job events.
func (s *Store) ForFiling(ctx context.Context, repo string) ([]*Job, map[int64][]int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE lower(repo)=lower($1) AND status NOT IN ('merged','closed') ORDER BY priority DESC,filed_at,id", repo)
	if err != nil {
		return nil, nil, err
	}
	out := []*Job{}
	ids := []int64{}
	deps := map[int64][]int64{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		out = append(out, j)
		ids = append(ids, j.ID)
		deps[j.ID] = []int64{}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(ids) > 0 {
		rows, err = tx.QueryContext(ctx, "SELECT job_id,depends_on FROM job_deps WHERE job_id=ANY($1) ORDER BY job_id,depends_on", ids)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id, dep int64
			if err = rows.Scan(&id, &dep); err != nil {
				rows.Close()
				return nil, nil, err
			}
			deps[id] = append(deps[id], dep)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return out, deps, nil
}
