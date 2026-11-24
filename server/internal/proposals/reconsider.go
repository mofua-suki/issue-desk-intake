package proposals

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

const oldHeadReason = "PRのhead.shaとWorker案のIDが一致しません。"

// reconsiderHead preserves the refusal and only releases its exact original
// comment after the current merged-PR rule accepts it.
func (s *Service) reconsiderHead(ctx context.Context, repo string, n int64, comment, p Object, fingerprint string) (bool, error) {
	key := fmt.Sprintf("rejection:%s#%d:%d", repo, n, num(comment["id"]))
	s.mu.Lock()
	r, err := s.read(ctx, key)
	eligible := err == nil && oldHeadRefusal(r, repo, n, comment, p, fingerprint)
	s.mu.Unlock()
	if err != nil || !eligible {
		return false, err
	}
	// Pending candidates may have been saved before a comment was edited.
	current := Object{}
	if err := s.o.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/comments/%d", repo, num(comment["id"])), nil, &current); err != nil {
		return true, err
	}
	latest, fp, parseErr := parse(str(current["body"]))
	if parseErr != nil || fp != fingerprint || latest["id"] != p["id"] || num(current["id"]) != num(comment["id"]) || obj(current["user"])["login"] != obj(comment["user"])["login"] {
		return false, nil
	}
	u, urlErr := url.Parse(str(current["issue_url"]))
	if urlErr != nil || !strings.HasSuffix(u.Path, fmt.Sprintf("/%s/issues/%d", repo, n)) || p["source"] != fmt.Sprintf("%s#%d", repo, n) {
		return false, nil
	}
	// Check the current writer before releasing the refusal.
	config, err := s.o.Config()
	if err != nil {
		return true, err
	}
	trusted := false
	for _, writer := range proposalWriters(config) {
		trusted = trusted || writer != "" && writer == str(obj(comment["user"])["login"])
	}
	if !trusted {
		return false, nil
	}
	pr, err := s.mergedPull(ctx, repo, str(p["pr"]))
	if err != nil {
		return true, err
	}
	if pr["merged"] != true {
		return true, nil
	}
	matches, err := s.workerHead(ctx, repo, n, p, pr)
	if err != nil {
		return true, err
	}
	return s.saveHeadReconsideration(ctx, key, repo, n, comment, p, fingerprint, matches)
}

func oldHeadRefusal(r Object, repo string, n int64, comment, p Object, fingerprint string) bool {
	return workerID.MatchString(str(p["id"])) && r != nil && r["type"] == "rejection" && r["status"] == "rejected" && r["reason"] == oldHeadReason && (r["scope"] == nil || r["scope"] == "comment") && r["headReconsideredAt"] == nil && r["headRuleVersion"] == nil && r["repo"] == repo && num(r["number"]) == n && num(r["commentId"]) == num(comment["id"]) && r["id"] == p["id"] && r["fingerprint"] == fingerprint && fingerprint != "" && r["author"] == obj(comment["user"])["login"]
}

// A different head of the same batch may already have an in-flight or
// completed submission, including one whose POST result is still unknown.
// Called with mu held so adoption cannot create a sibling between the check
// and recording this decision.
func (s *Service) reconsiderationSiblingExists(ctx context.Context, repo string, n int64, comment, p Object) (bool, error) {
	family, ok := proposalAlertFamily(Object{"repo": repo, "number": n, "commentId": comment["id"], "id": p["id"]})
	if !ok {
		return false, nil
	}
	rows, err := s.records.List(ctx, "proposals")
	if err != nil {
		return false, err
	}
	for _, raw := range rows {
		r := Object(raw)
		if r["type"] != "proposal" || r["id"] == p["id"] || (r["status"] != "adopted" && r["status"] != "notified") || obj(r["proposal"])["pr"] != p["pr"] {
			continue
		}
		other, valid := proposalAlertFamily(r)
		if valid && other == family {
			return true, nil
		}
	}
	return false, nil
}

// saveHeadReconsideration records the head-rule decision on the refusal when it is still an
// old-head refusal, releasing it when the head matches and no sibling blocks it.
func (s *Service) saveHeadReconsideration(ctx context.Context, key, repo string, n int64, comment, p Object, fingerprint string, matches bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.read(ctx, key)
	if err != nil {
		return true, err
	}
	if oldHeadRefusal(r, repo, n, comment, p, fingerprint) {
		if matches {
			blocked, err := s.reconsiderationSiblingExists(ctx, repo, n, comment, p)
			if err != nil {
				return true, err
			}
			matches = !blocked
		}
		r["headRuleVersion"] = "1"
		r["headReconsideredAt"] = s.at()
		if matches {
			r["status"] = "head_reconsidered"
		}
		if err = s.save(ctx, key, r); err != nil {
			return true, err
		}
	}
	return false, nil
}
