package proposals

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"issue-desk-intake/server/internal/fleet"
)

var errWorkerCommits = errors.New("PRのコミット一覧または投稿案を確認できません。再確認します。")

// workerHead accepts an earlier posted head only when every subsequent PR
// commit is a merge of an external parent and no newer head has a proposal.
func (s *Service) workerHead(ctx context.Context, repo string, issue int64, p, pr Object) (bool, error) {
	id := str(p["id"])
	matched, err := workerHead(id, pr)
	if err != nil || matched {
		return matched, err
	}
	n := num(pr["number"])
	if n < 1 {
		return false, errWorkerCommits
	}
	commits, err := s.pullCommits(ctx, repo, n)
	if err != nil {
		return false, err
	}
	if count, present := pr["commits"]; present && count != nil && (!validNumber(count) || num(count) != int64(len(commits))) {
		return false, errWorkerCommits
	}
	indices, err := commitIndices(commits)
	if err != nil {
		return false, err
	}
	head := str(obj(pr["head"])["sha"])
	if _, ok := indices[head]; !ok {
		return false, errWorkerCommits
	}
	candidate, err := postedCommit(commits, workerID.FindStringSubmatch(id)[3])
	if err != nil || candidate == "" {
		return false, err
	}
	later, external, err := externalMergesSince(commits, indices, head, candidate)
	if err != nil || !external {
		return false, err
	}
	return s.postedHeadIsLatest(ctx, repo, issue, p, later)
}

// pullCommits reads every commit of the pull request and rejects malformed or repeated SHAs.
func (s *Service) pullCommits(ctx context.Context, repo string, n int64) ([]Object, error) {
	commits := []Object{}
	seen := make(map[string]bool)
	for page := 1; ; page++ {
		var batch []Object
		route := fmt.Sprintf("/repos/%s/pulls/%d/commits?limit=50&page=%d", repo, n, page)
		if err := s.o.Gitea.Do(ctx, "GET", route, nil, &batch); err != nil {
			return nil, fmt.Errorf("%w: %v", errWorkerCommits, err)
		}
		if batch == nil || len(batch) > 50 {
			return nil, errWorkerCommits
		}
		if len(batch) == 0 {
			break
		}
		for _, c := range batch {
			sha, ok := c["sha"].(string)
			if !ok || !commitSHA.MatchString(sha) || seen[sha] {
				return nil, errWorkerCommits
			}
			seen[sha] = true
		}
		commits = append(commits, batch...)
	}
	if len(commits) == 0 {
		return nil, errWorkerCommits
	}
	return commits, nil
}

// commitIndices indexes the commits by SHA after checking each commit and parent SHA.
func commitIndices(commits []Object) (map[string]int, error) {
	indices := make(map[string]int, len(commits))
	for i, c := range commits {
		sha, ok := c["sha"].(string)
		parents, valid := c["parents"].([]any)
		if !ok || !commitSHA.MatchString(sha) || !valid || parents == nil {
			return nil, errWorkerCommits
		}
		if _, exists := indices[sha]; exists {
			return nil, errWorkerCommits
		}
		indices[sha] = i
		for _, raw := range parents {
			parent, ok := obj(raw)["sha"].(string)
			if !ok || !commitSHA.MatchString(parent) {
				return nil, errWorkerCommits
			}
		}
	}
	return indices, nil
}

// postedCommit returns the commit whose SHA digest the proposal ID names, or "" when none does.
func postedCommit(commits []Object, digest string) (string, error) {
	candidate := ""
	for _, c := range commits {
		sha := str(c["sha"])
		sum := sha256.Sum256([]byte(sha))
		if fmt.Sprintf("%x", sum[:16]) == digest {
			if candidate != "" {
				return "", errWorkerCommits
			}
			candidate = sha
		}
	}
	return candidate, nil
}

// externalMergesSince walks back from the PR head to the candidate and returns the later
// commits. external is false when one of them is not a merge of an external parent.
func externalMergesSince(commits []Object, indices map[string]int, head, candidate string) ([]string, bool, error) {
	// Walk backwards from the authoritative PR head, independent of API order.
	later := []string{}
	visited := map[string]bool{}
	for current := head; current != candidate; {
		if visited[current] {
			return nil, false, errWorkerCommits
		}
		visited[current] = true
		i, ok := indices[current]
		if !ok {
			return nil, false, errWorkerCommits
		}
		parents := commits[i]["parents"].([]any)
		if len(parents) == 0 {
			return nil, false, errWorkerCommits
		}
		if len(parents) == 1 {
			return nil, false, nil
		}
		for _, raw := range parents[1:] {
			if _, inside := indices[str(obj(raw)["sha"])]; inside {
				return nil, false, nil
			}
		}
		later = append(later, current)
		current = str(obj(parents[0])["sha"])
	}
	return later, true, nil
}

// postedHeadIsLatest confirms from the trusted issue comments that the proposal was posted
// and that no later commit has its own proposal.
func (s *Service) postedHeadIsLatest(ctx context.Context, repo string, issue int64, p Object, later []string) (bool, error) {
	id := str(p["id"])
	if s.o.Comments == nil {
		return false, errWorkerCommits
	}
	comments, err := s.o.Comments(ctx, repo, issue)
	if err != nil || comments == nil {
		return false, fmt.Errorf("%w: コメント取得失敗", errWorkerCommits)
	}
	config, err := s.o.Config()
	if err != nil {
		return false, err
	}
	writers := proposalWriters(config)
	found := false
	for _, comment := range comments {
		trusted := false
		for _, writer := range writers {
			trusted = trusted || writer != "" && writer == str(obj(comment["user"])["login"])
		}
		u, urlErr := url.Parse(str(comment["issue_url"]))
		if !trusted || !validNumber(comment["id"]) || urlErr != nil || !strings.HasSuffix(u.Path, fmt.Sprintf("/%s/issues/%d", repo, issue)) {
			continue
		}
		other, _, parseErr := parse(str(comment["body"]))
		if parseErr != nil || num(other["version"]) != 3 || other["source"] != fmt.Sprintf("%s#%d", repo, issue) || other["pr"] != p["pr"] {
			continue
		}
		otherID := str(other["id"])
		if otherID == id && reflect.DeepEqual(fleet.Clone(other), fleet.Clone(p)) {
			found = true
		}
		otherMatch := workerID.FindStringSubmatch(otherID)
		if otherMatch == nil {
			continue
		}
		for _, sha := range later {
			sum := sha256.Sum256([]byte(sha))
			if fmt.Sprintf("%x", sum[:16]) == otherMatch[3] {
				return false, nil
			}
		}
	}
	if !found {
		return false, errWorkerCommits
	}
	return true, nil
}
