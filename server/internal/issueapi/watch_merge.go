package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
)

// watchMergeEvidence reads the exact PR linked by the completed job or the
// recorded direct close. The issue state alone is not a merge receipt.
func (s *Service) watchMergeEvidence(ctx context.Context, repo string, issue int64, detail Object) Object {
	if detail["state"] != "closed" {
		return nil
	}
	var n int64
	var wantHead, wantBranch string
	closeReason := fleet.ObjectOrEmpty(detail["closeReason"])
	if closeReason["reason"] == "other_route" {
		route := fleet.ObjectOrEmpty(closeReason["route"])
		if route["repo"] != repo {
			return nil
		}
		n, _ = number(route["pr"])
	} else {
		job, err := s.o.Jobs.ByIssue(ctx, repo, issue)
		if err != nil || job == nil || job.PR == nil || job.HeadSHA == nil || job.Branch == nil {
			return nil
		}
		n, wantHead, wantBranch = *job.PR, *job.HeadSHA, *job.Branch
		if !sha.MatchString(wantHead) || wantBranch == "" {
			return nil
		}
	}
	if n <= 0 {
		return nil
	}
	pr := Object{}
	if err := s.o.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return nil
	}
	number, _ := number(pr["number"])
	commit := fleet.Text(pr["merge_commit_sha"])
	head, base := fleet.ObjectOrEmpty(pr["head"]), fleet.ObjectOrEmpty(pr["base"])
	baseRepo := fleet.ObjectOrEmpty(base["repo"])
	baseBranch := fleet.Text(baseRepo["default_branch"])
	if number != n || pr["merged"] != true || !sha.MatchString(commit) ||
		fleet.ObjectOrEmpty(head["repo"])["full_name"] != repo || baseRepo["full_name"] != repo ||
		baseBranch == "" || base["ref"] != baseBranch ||
		(wantHead != "" && (head["sha"] != wantHead || head["ref"] != wantBranch)) {
		return nil
	}
	return Object{"repo": repo, "pr": n, "mergeCommitSha": commit}
}
