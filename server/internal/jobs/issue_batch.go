package jobs

import (
	"context"
	"encoding/json"

	"issue-desk-intake/server/internal/contract"
)

// GetBatch reads exactly the requested jobs, using the same decoding and missing-job
// contract as Get. Duplicate IDs do not cause duplicate reads.
func (s *Store) GetBatch(ctx context.Context, ids []int64) (map[int64]*Job, error) {
	const batchSize = 500
	result := make(map[int64]*Job, len(ids))
	seen := make(map[int64]bool, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	for start := 0; start < len(unique); start += batchSize {
		end := start + batchSize
		if end > len(unique) {
			end = len(unique)
		}
		data, err := json.Marshal(unique[start:end])
		if err != nil {
			return nil, err
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id IN (SELECT value::bigint FROM jsonb_array_elements_text($1::jsonb))`, string(data))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			job, scanErr := scanJob(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			result[job.ID] = job
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, id := range unique[start:end] {
			if _, ok := result[id]; !ok {
				return nil, contract.NewProblem(404, "ジョブが見つかりません。", "")
			}
		}
	}
	return result, nil
}
