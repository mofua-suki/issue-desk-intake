package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

// MergedPredecessorResolver records externally verified merges on dependent jobs.
type MergedPredecessorResolver struct{ Service *Service }
type mergedCandidate struct {
	predecessor, successor, issue, generation int64
	repo, status                              string
	pr                                        sql.NullInt64
	head, branch, artifactGeneration          sql.NullString
}

func (r *MergedPredecessorResolver) Resolve(ctx context.Context) error {
	if r == nil || r.Service == nil || r.Service.PostgreSQL == nil {
		return errors.New("merged predecessor resolver is not configured")
	}
	s := r.Service
	rows, err := s.PostgreSQL.QueryContext(ctx, `SELECT p.id,j.id,p.repo,p.issue,p.pr,p.status,p.generation,p.view#>>'{continuation,artifact,headSha}',p.view#>>'{continuation,artifact,branch}',p.view#>>'{continuation,artifact,generation}' FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id WHERE `+jobs.MergedPredecessorCandidateSQL+` ORDER BY j.id,p.id`)
	if err != nil {
		return err
	}
	var candidates []mergedCandidate
	for rows.Next() {
		var c mergedCandidate
		if err = rows.Scan(&c.predecessor, &c.successor, &c.repo, &c.issue, &c.pr, &c.status, &c.generation, &c.head, &c.branch, &c.artifactGeneration); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var all error
	for _, c := range candidates {
		if err = r.resolveOne(ctx, c); err != nil {
			all = errors.Join(all, fmt.Errorf("dependency %d -> %d: %w", c.predecessor, c.successor, err))
		}
	}
	return all
}

func (r *MergedPredecessorResolver) resolveOne(ctx context.Context, c mergedCandidate) error {
	s := r.Service
	if s.Gitea == nil || !fleet.ValidRepo(c.repo) {
		//lint:ignore ST1005 Gitea is a proper product name, not a capitalized sentence.
		return errors.New("Gitea unavailable or invalid repository")
	}
	pr, source, commit := c.pr.Int64, "own_pr", ""
	var ownErr error
	if c.status == "failed" {
		source = "rejected_report"
		commit, ownErr = r.rejectedReportMerge(ctx, c)
		if ownErr != nil || commit == "" {
			return ownErr
		}
	} else if c.pr.Valid && pr > 0 &&
		(c.artifactGeneration.String == strconv.FormatInt(c.generation, 10) || !c.artifactGeneration.Valid && c.generation == 0) {
		commit, ownErr = r.mergeCommit(ctx, c.repo, pr)
	}
	if commit == "" {
		directPR, err := s.DirectMergedPredecessorPR(ctx, c.repo, c.issue)
		if err != nil {
			return errors.Join(ownErr, err)
		}
		if directPR == 0 {
			return ownErr
		}
		pr, source = directPR, "merge_manual"
		commit, err = r.mergeCommit(ctx, c.repo, pr)
		if err != nil {
			return errors.Join(ownErr, err)
		}
		if commit == "" {
			return ownErr
		}
	}
	evidence, err := json.Marshal(map[string]any{"repo": c.repo, "pr": pr, "mergeCommit": commit, "source": source, "checkedAt": iso(s.now()), "generation": c.generation, "head": c.head.String, "branch": c.branch.String})
	if err != nil {
		return err
	}
	// Update only the still-unresolved edge, and preserve concurrent view updates.
	_, err = s.PostgreSQL.ExecContext(ctx, `UPDATE jobs j SET view=jsonb_set(j.view,'{dependencyMerges}',(CASE WHEN jsonb_typeof(j.view->'dependencyMerges')='object' THEN j.view->'dependencyMerges' ELSE '{}'::jsonb END) || jsonb_build_object(p.id::text,$3::jsonb),true) FROM jobs p,job_deps d WHERE j.id=$1 AND p.id=$2 AND d.job_id=j.id AND d.depends_on=p.id AND p.repo=$4 AND p.issue=$5 AND p.pr IS NOT DISTINCT FROM $6 AND p.status=$7 AND p.generation=$8 AND (p.view#>>'{continuation,artifact,headSha}') IS NOT DISTINCT FROM $9 AND (p.view#>>'{continuation,artifact,branch}') IS NOT DISTINCT FROM $10 AND (p.view#>>'{continuation,artifact,generation}') IS NOT DISTINCT FROM $11 AND `+jobs.MergedPredecessorCandidateSQL, c.successor, c.predecessor, string(evidence), c.repo, c.issue, c.pr, c.status, c.generation, c.head, c.branch, c.artifactGeneration)
	return err
}

// DirectMergedPredecessorPR returns the PR identified by a structured close and a completed manual merge request.
// Callers must still verify the PR itself before treating the dependency as satisfied.
func (s *Service) DirectMergedPredecessorPR(ctx context.Context, repo string, issue int64) (int64, error) {
	if s.DB == nil {
		return 0, errors.New("close reason database unavailable")
	}
	var raw string
	err := s.DB.QueryRowContext(ctx, bindSQL(`SELECT json FROM close_reasons WHERE id=?`), fmt.Sprintf("%s#%d", repo, issue)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var reason struct {
		Reason string `json:"reason"`
		Route  struct {
			Repo string `json:"repo"`
			PR   int64  `json:"pr"`
		} `json:"route"`
	}
	if err = json.Unmarshal([]byte(raw), &reason); err != nil {
		return 0, err
	}
	if reason.Reason != "other_route" || !strings.EqualFold(reason.Route.Repo, repo) || reason.Route.PR < 1 {
		return 0, nil
	}
	var found bool
	err = s.PostgreSQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE kind='merge_manual' AND lower(repo)=lower($1) AND payload->>'number'=$2 AND result->>'status'='merged' AND status='done')`, repo, strconv.FormatInt(reason.Route.PR, 10)).Scan(&found)
	if err != nil || !found {
		return 0, err
	}
	return reason.Route.PR, nil
}

// rejectedReportMerge ties the actual merged PR to the stored publication
// before using its merge commit as evidence for a failed predecessor.
func (r *MergedPredecessorResolver) rejectedReportMerge(ctx context.Context, c mergedCandidate) (string, error) {
	var pull map[string]any
	if err := r.Service.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", c.repo, c.pr.Int64), nil, &pull); err != nil {
		return "", err
	}
	head, base := fleet.Object(pull["head"]), fleet.Object(pull["base"])
	headRepo, baseRepo := fleet.Object(head["repo"]), fleet.Object(base["repo"])
	defaultBranch := fleet.Text(baseRepo["default_branch"])
	commit := fleet.Text(pull["merge_commit_sha"])
	if fleet.WholeNumber(pull["number"]) != c.pr.Int64 || pull["merged"] != true ||
		!revision.MatchString(commit) || head["sha"] != c.head.String || head["ref"] != c.branch.String ||
		headRepo["full_name"] != c.repo || baseRepo["full_name"] != c.repo || defaultBranch == "" || base["ref"] != defaultBranch {
		return "", nil
	}
	return commit, nil
}

func (r *MergedPredecessorResolver) mergeCommit(ctx context.Context, repo string, pr int64) (string, error) {
	var pull struct {
		Merged      bool   `json:"merged"`
		MergeCommit string `json:"merge_commit_sha"`
	}
	if err := r.Service.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, pr), nil, &pull); err != nil {
		return "", err
	}
	if !pull.Merged || !revision.MatchString(pull.MergeCommit) {
		return "", nil
	}
	return pull.MergeCommit, nil
}
