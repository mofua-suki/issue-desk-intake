package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Issue identifies a job's repository and issue number.
type Issue struct {
	Repo   string `json:"repo"`
	Number int64  `json:"number"`
}

// ForIssues loads all jobs matching the requested issues, ignoring repo case.
func (s *Store) ForIssues(ctx context.Context, issues []Issue) ([]*Job, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(issues)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE EXISTS (SELECT 1 FROM jsonb_to_recordset($1::jsonb) AS wanted(repo text, number bigint) WHERE lower(jobs.repo)=lower(wanted.repo) AND jobs.issue=wanted.number)`, string(data))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

// FamilyDependencies returns the predecessors that block dispatch, in bulk.
func (s *Store) FamilyDependencies(ctx context.Context, all []*Job) (map[int64][]string, error) {
	if len(all) == 0 {
		return map[int64][]string{}, nil
	}
	ids := make([]int64, 0, len(all))
	for _, job := range all {
		ids = append(ids, job.ID)
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.job_id,p.repo,p.issue,(`+DependencyPairReadySQL+`) FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id WHERE d.job_id IN (SELECT value::bigint FROM jsonb_array_elements_text($1::jsonb)) ORDER BY p.repo,p.issue`, string(data))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	waiting := map[int64][]string{}
	edges := map[int64]map[string]bool{}
	for rows.Next() {
		var id, n int64
		var repo string
		var ready bool
		if err = rows.Scan(&id, &repo, &n, &ready); err != nil {
			return nil, err
		}
		key := strings.ToLower(fmt.Sprintf("%s#%d", repo, n))
		if edges[id] == nil {
			edges[id] = map[string]bool{}
		}
		edges[id][key] = ready
		if !ready {
			waiting[id] = append(waiting[id], fmt.Sprintf("%s#%d", repo, n))
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, job := range all {
		valid, err := DependencyReferences(job.View, func(repo string, n int64) (bool, error) {
			key := strings.ToLower(fmt.Sprintf("%s#%d", repo, n))
			if _, ok := edges[job.ID][key]; !ok {
				waiting[job.ID] = append(waiting[job.ID], fmt.Sprintf("%s#%d（依存の記録なし）", repo, n))
			}
			// Collect every missing reference; existing blocked edges are already listed.
			return true, nil
		})
		if err != nil {
			return nil, err
		}
		if !valid {
			waiting[job.ID] = append(waiting[job.ID], "依存の記録が不正です")
		}
	}
	return waiting, nil
}
