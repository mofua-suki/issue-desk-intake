package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"issue-desk-intake/server/internal/fleet"
)

// ResolveFilingReferences completes durable dependency edges without treating missing jobs as satisfied.
func (s *Store) ResolveFilingReferences(ctx context.Context) error {
	return s.resolveFilingReferencesWithReader(ctx, readTransferGraph)
}

func (s *Store) resolveFilingReferencesWithReader(ctx context.Context, read func(context.Context, transferQuery) ([]*transferJob, error)) error {
	return s.resolveFilingReferencesWithIndexBuilder(ctx, read, newDependencyIndex)
}

func (s *Store) resolveFilingReferencesWithIndexBuilder(ctx context.Context, read func(context.Context, transferQuery) ([]*transferJob, error), build func([]*transferJob) *dependencyIndex) error {
	all, err := read(ctx, s.db)
	if err != nil {
		return err
	}
	index := build(all)
	for _, item := range all {
		if item.Status == "merged" || item.Status == "closed" {
			continue
		}
		if _, ok := item.View["dependsOnIssues"]; !ok {
			if _, ok = item.View["unresolvedDependencies"]; !ok {
				continue
			}
		}
		job := &Job{ID: item.ID, View: fleet.Object(fleet.Clone(item.View))}
		err = applyFilingReferences(ctx, transferProbe{s.db}, job, index)
		if err == nil {
			continue
		}
		if !errors.Is(err, errTransferWrite) {
			return err
		}
		id := item.ID
		err = s.transaction(ctx, func(tx *sql.Tx) error {
			if err := lockDependencyGraph(ctx, tx); err != nil {
				return err
			}
			job, err := readJob(ctx, tx, id, true)
			if err != nil {
				return err
			}
			if job.Status == "merged" || job.Status == "closed" {
				return nil
			}
			fresh, err := read(ctx, tx)
			if err != nil {
				return err
			}
			return applyFilingReferences(ctx, tx, job, build(fresh))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func applyFilingReferences(ctx context.Context, tx transferWriter, job *Job, index *dependencyIndex) error {
	pending := []any{}
	explicit := []int64{}
	seen := map[string]bool{}
	for _, field := range []string{"dependsOnIssues", "unresolvedDependencies"} {
		raw, err := json.Marshal(job.View[field])
		if err != nil {
			return err
		}
		var refs []map[string]any
		if string(raw) != "null" {
			if err = json.Unmarshal(raw, &refs); err != nil {
				return err
			}
		}
		for _, ref := range refs {
			repo, _ := ref["repo"].(string)
			number, ok := referenceNumber(ref["issue"])
			if repo == "" || !ok {
				pending = append(pending, ref)
				continue
			}
			key := fmt.Sprintf("%s#%d", strings.ToLower(repo), number)
			if seen[key] {
				continue
			}
			seen[key] = true
			dep, exists := index.byKey[transferKey(repo, number)]
			if !exists || dep == job.ID {
				pending = append(pending, ref)
				continue
			}
			explicit = append(explicit, dep)
		}
	}
	if _, err := addExplicitDependenciesFromGraph(ctx, tx, job, explicit, index); err != nil {
		return err
	}
	old, exists := job.View["unresolvedDependencies"]
	oldJSON, err := json.Marshal(old)
	if err != nil {
		return err
	}
	newJSON, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		if !exists {
			return nil
		}
		delete(job.View, "unresolvedDependencies")
	} else {
		if string(oldJSON) == string(newJSON) {
			return nil
		}
		job.View["unresolvedDependencies"] = pending
	}
	raw, err := json.Marshal(job.View)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE jobs SET view=$2::jsonb WHERE id=$1", job.ID, string(raw))
	return err
}

func referenceNumber(value any) (int64, bool) {
	switch n := value.(type) {
	case json.Number:
		i, e := n.Int64()
		return i, e == nil && i > 0 && i <= maxSafeInteger
	case float64:
		return int64(n), n > 0 && n <= float64(maxSafeInteger) && n == float64(int64(n))
	case int:
		return int64(n), n > 0
	case int64:
		return n, n > 0 && n <= maxSafeInteger
	}
	return 0, false
}
