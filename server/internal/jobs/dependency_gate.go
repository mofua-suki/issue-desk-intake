package jobs

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strconv"
	"strings"
)

var mergeCommitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// Explicit references must have a persisted edge and a terminal predecessor.
// A missing predecessor or a malformed reference cannot authorize dispatch.
type DependencyReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func explicitDependenciesReady(ctx context.Context, tx *sql.Tx, job *Job) (bool, error) {
	return DependenciesReady(ctx, tx, job.ID, job.View)
}

// PredecessorReadySQL excludes terminal jobs from priority inheritance.
// Eligibility for each dependency edge uses DependencyPairReadySQL.
const PredecessorReadySQL = "p.status IN('merged','closed')"

// MergedPredecessorCandidateSQL identifies unresolved edges for reconciliation.
const MergedPredecessorCandidateSQL = `(p.status='closed' OR (
	p.status='failed' AND p.pr>0
	AND p.view#>>'{workerExecution,stage}'='merge'
	AND p.view#>>'{workerExecution,generation}'=p.generation::text
	AND p.view#>>'{continuation,artifact,generation}'=p.generation::text
	AND nullif(p.view#>>'{continuation,artifact,branch}','') IS NOT NULL
	AND p.view#>>'{continuation,artifact,headSha}' ~ '^[a-f0-9]{40}$'
	AND p.result#>>'{leaseDiagnostics,lastReportRejection,reportKind}'='merged'
	AND p.result#>>'{leaseDiagnostics,lastReportRejection,generation}'=p.generation::text
)) AND NOT ` + DependencyPairReadySQL + ` AND j.status IN ('queued','running','held')`

// DependencyPairReadySQL evaluates an edge with predecessor p and successor j.
// The database function also governs dependency wait recording.
const DependencyPairReadySQL = `dependency_pair_ready(p,j)`

// DependencyReferences checks each distinct explicit reference. Invalid references
// are not allowed to authorize dispatch.
func DependencyReferences(view map[string]any, check func(string, int64) (bool, error)) (bool, error) {
	seen := map[string]bool{}
	for _, field := range []string{"dependsOnIssues", "unresolvedDependencies"} {
		raw, exists := view[field]
		if !exists {
			continue
		}
		refs, ok := raw.([]any)
		if !ok {
			return false, nil
		}
		for _, raw := range refs {
			ref := fleet.Object(raw)
			if ref == nil {
				return false, nil
			}
			repo := fleet.Text(ref["repo"])
			issue, ok := count(ref["issue"])
			if !ok || issue < 1 || !fleet.ValidRepo(repo) {
				return false, nil
			}
			key := strings.ToLower(repo) + "#" + strconv.FormatInt(issue, 10)
			if seen[key] {
				continue
			}
			seen[key] = true
			ready, e := check(repo, issue)
			if e != nil {
				return false, e
			}
			if !ready {
				return false, nil
			}
		}
	}
	return true, nil
}

// DependenciesReady requires an edge to a released predecessor for each reference.
func DependenciesReady(ctx context.Context, q DependencyReader, id int64, view map[string]any) (bool, error) {
	return DependencyReferences(view, func(repo string, issue int64) (bool, error) {
		var ready bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs p JOIN job_deps edge ON edge.depends_on=p.id AND edge.job_id=$1 JOIN jobs j ON j.id=edge.job_id WHERE lower(p.repo)=lower($2) AND p.issue=$3 AND `+DependencyPairReadySQL+`)`, id, repo, issue).Scan(&ready)
		return ready, err
	})
}
