package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

func lockDependencyGraph(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, "LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE")
	return err
}
func addExplicitDependencies(ctx context.Context, tx *sql.Tx, job *Job, deps []int64) (int, error) {
	all, err := readTransferGraph(ctx, tx)
	if err != nil {
		return 0, err
	}
	return addExplicitDependenciesFromGraph(ctx, tx, job, deps, newDependencyIndex(all))
}

type dependencyIndex struct {
	byID  map[int64]*transferJob
	byKey map[string]int64
	graph map[int64][]int64
}

func newDependencyIndex(all []*transferJob) *dependencyIndex {
	byID := map[int64]*transferJob{}
	byKey := map[string]int64{}
	graph := map[int64][]int64{}
	for _, j := range all {
		byID[j.ID] = j
		byKey[transferKey(j.Repo, j.Issue)] = j.ID
		graph[j.ID] = append([]int64{}, j.Deps...)
	}
	for _, j := range all {
		for _, field := range []string{"dependsOnIssues", "unresolvedDependencies"} {
			for _, raw := range fleet.List(j.View[field]) {
				ref := fleet.Object(raw)
				if dep, ok := byKey[transferKey(fleet.Text(ref["repo"]), int64(fleet.Number(ref["issue"])))]; ok {
					graph[j.ID] = append(graph[j.ID], dep)
				}
			}
		}
	}
	return &dependencyIndex{byID: byID, byKey: byKey, graph: graph}
}

func addExplicitDependenciesFromGraph(ctx context.Context, tx transferWriter, job *Job, deps []int64, index *dependencyIndex) (int, error) {
	byID, graph := index.byID, index.graph
	targets := []int64{}
	seen := map[int64]bool{}
	for _, dep := range deps {
		if byID[dep] == nil || seen[dep] {
			continue
		}
		targets = append(targets, dep)
		seen[dep] = true
	}
	pending := append([]int64{}, graph[job.ID]...)
	pending = append(pending, targets...)
	visited := map[int64]bool{}
	for len(pending) > 0 {
		dep := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if dep == job.ID {
			return 0, contract.NewProblem(409, "成果物への依存が自己依存または間接循環になるため更新しません。", "acceptance_transfer_cycle")
		}
		if visited[dep] {
			continue
		}
		visited[dep] = true
		pending = append(pending, graph[dep]...)
	}
	wait := fleet.Object(job.View["acceptanceTransferWait"])
	owned := fleet.Object(wait["ownedTargets"])
	changed := false
	if owned != nil {
		for _, dep := range targets {
			key := strconv.FormatInt(dep, 10)
			if _, exists := owned[key]; exists {
				delete(owned, key)
				changed = true
			}
		}
	}
	if changed {
		raw, err := json.Marshal(wait)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferWait}',$2::jsonb) WHERE id=$1", job.ID, string(raw)); err != nil {
			return 0, err
		}
	}
	added := 0
	existing := map[int64]bool{}
	if current := byID[job.ID]; current != nil {
		for _, dep := range current.Deps {
			existing[dep] = true
		}
	}
	for _, dep := range targets {
		if existing[dep] {
			continue
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2) ON CONFLICT DO NOTHING", job.ID, dep)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		added += int(n)
	}
	return added, nil
}
