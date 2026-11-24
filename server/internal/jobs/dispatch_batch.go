package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// loadDependencySnapshot reads all edges for the queued jobs in one query.
func loadDependencySnapshot(ctx context.Context, tx *sql.Tx, jobs []*Job) (map[int64]bool, map[int64]bool, error) {
	ids := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	blocked := map[int64]bool{}
	refs := map[int64]map[string]map[int64]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT d.job_id,p.repo,p.issue,`+DependencyPairReadySQL+` FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id WHERE d.job_id=ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, issue int64
		var repo string
		var ready bool
		if err = rows.Scan(&id, &repo, &issue, &ready); err != nil {
			return nil, nil, err
		}
		if !ready {
			blocked[id] = true
		}
		repo = strings.ToLower(repo)
		if refs[id] == nil {
			refs[id] = map[string]map[int64]bool{}
		}
		if refs[id][repo] == nil {
			refs[id][repo] = map[int64]bool{}
		}
		if ready {
			refs[id][repo][issue] = true
		}
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	explicit := map[int64]bool{}
	for _, j := range jobs {
		explicit[j.ID], err = DependencyReferences(j.View, func(repo string, issue int64) (bool, error) { return refs[j.ID][strings.ToLower(repo)][issue], nil })
		if err != nil {
			return nil, nil, err
		}
	}
	return blocked, explicit, nil
}

type skipBatchEntry struct {
	ID   int64          `json:"id"`
	Data map[string]any `json:"data"`
}

// recordFullHostSkips inserts all missing dependency and slot records in one statement.
func recordFullHostSkips(ctx context.Context, tx *sql.Tx, entries []skipBatchEntry, host string, at time.Time) error {
	if len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		entry.Data["host"] = host
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data)
 SELECT x.id,$2,'dispatch_skip',$3,x.data FROM jsonb_to_recordset($1::jsonb) AS x(id bigint,data jsonb)
 WHERE NOT EXISTS (SELECT 1 FROM job_events e WHERE e.job_id=x.id AND e.kind='dispatch_skip' AND e.at>$4 AND e.data->>'reason'=x.data->>'reason' AND e.data->>'host'=$2)`, string(b), host, at, at.Add(-5*time.Minute))
	return err
}
