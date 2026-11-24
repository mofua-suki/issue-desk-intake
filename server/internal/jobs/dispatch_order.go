package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
)

type dispatchOrder struct {
	priority, successors int
	filedRank            int64
}

// loadDispatchOrder follows only persisted job_deps from dispatch candidates.
// Explicit view.dependsOnIssues references gate dispatch separately; they do not
// inherit priority or contribute to the successor count without a job_deps edge.
func loadDispatchOrder(ctx context.Context, tx *sql.Tx, candidates []*Job) (map[int64]dispatchOrder, error) {
	result := make(map[int64]dispatchOrder, len(candidates))
	if len(candidates) == 0 {
		return result, nil
	}
	ids := make([]int64, len(candidates))
	for i, job := range candidates {
		ids[i] = job.ID
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE roots AS (
 SELECT p.id, row_number() OVER (ORDER BY p.filed_at,p.id) AS filed_rank
 FROM jobs p WHERE p.id IN (SELECT value::bigint FROM jsonb_array_elements_text($1::jsonb)) AND NOT (`+PredecessorReadySQL+`)
 ), reachable(root,id) AS (
 SELECT id,id FROM roots
 UNION
 SELECT r.root,p.id FROM reachable r JOIN job_deps d ON d.depends_on=r.id
 JOIN jobs p ON p.id=d.job_id WHERE NOT (`+PredecessorReadySQL+`)
 )
 SELECT roots.id, max(p.priority), count(*)-1, roots.filed_rank
 FROM roots JOIN reachable r ON r.root=roots.id JOIN jobs p ON p.id=r.id
 GROUP BY roots.id,roots.filed_rank`, string(data))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var order dispatchOrder
		if err = rows.Scan(&id, &order.priority, &order.successors, &order.filedRank); err != nil {
			return nil, err
		}
		result[id] = order
	}
	return result, rows.Err()
}
