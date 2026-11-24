package filing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/consultations"
)

func activeLinkGroups(groups [][]string, plan []Record) [][]string {
	var active [][]string
	for _, group := range groups {
		present := map[string]bool{}
		for _, child := range plan {
			for _, repo := range group {
				if strings.EqualFold(fleet.Text(child["project"]), repo) {
					present[strings.ToLower(repo)] = true
				}
			}
		}
		if len(present) > 1 {
			active = append(active, group)
		}
	}
	return active
}
func (s *Service) groupLinkWaiting(ctx context.Context, id string) (bool, error) {
	return groupLinkWaiting(ctx, s.options.Records, id)
}

func groupLinkWaiting(ctx context.Context, records interface {
	Get(context.Context, string, string) (consultations.Record, bool, error)
}, id string) (bool, error) {
	seen := map[string]bool{}
	waiting := false
	for id != "" {
		if seen[id] {
			return false, fail(409, "分割元の相談が循環しています。")
		}
		seen[id] = true
		child, found, err := records.Get(ctx, "drafts", id)
		if err != nil {
			return false, err
		}
		if !found {
			// Older pending entries may not have an associated draft.
			// Once a parent is declared, its absence is an error below.
			if len(seen) == 1 {
				return false, nil
			}
			return false, fail(409, "分割元の相談がありません。")
		}
		if len(seen) > 1 {
			waiting = waiting || len(fleet.Items(child["_groupLinkGroups"])) > 0 && child["_groupLinksComplete"] != true
		}
		id = fleet.Text(child["parentSplit"])
	}
	return waiting, nil
}
func groupPeers(groups any, plan []Record, filed map[int]Record, index int) []string {
	repo := fleet.Text(plan[index]["project"])
	var peers []string
	for _, raw := range fleet.Items(groups) {
		member := false
		for _, v := range fleet.Items(raw) {
			if strings.EqualFold(fleet.Text(v), repo) {
				member = true
			}
		}
		if !member {
			continue
		}
		for i, child := range plan {
			other := fleet.Text(child["project"])
			if strings.EqualFold(repo, other) {
				continue
			}
			matches := false
			for _, v := range fleet.Items(raw) {
				if strings.EqualFold(fleet.Text(v), other) {
					matches = true
				}
			}
			if matches {
				if n, ok := integer(filed[i]["number"]); ok && n > 0 {
					peers = append(peers, fmt.Sprintf("%s#%d", other, n))
				}
			}
		}
	}
	return peers
}
func (s *Service) linkRepositoryGroupIssues(ctx context.Context, parent Record, plan []Record, filed map[int]Record) error {
	if len(fleet.Items(parent["_groupLinkGroups"])) == 0 {
		return nil
	}
	if s.options.GroupIssueLinks == nil {
		return fail(503, "組のIssue本文更新を利用できません。")
	}
	for i, planned := range plan {
		peers := groupPeers(parent["_groupLinkGroups"], plan, filed, i)
		if len(peers) == 0 {
			continue
		}
		child, found, err := s.options.Records.Get(ctx, "drafts", fleet.Text(planned["id"]))
		if err != nil {
			return err
		}
		if !found {
			return fail(409, "分割先の相談がありません。")
		}
		repo := fleet.Text(planned["project"])
		number, ok := integer(filed[i]["number"])
		if !ok || number < 1 {
			return fail(409, "Issue番号がありません。")
		}
		entry, err := s.savedJobFiling(ctx, repo, number)
		if err != nil {
			return err
		}
		issue, err := s.options.GroupIssueLinks.Get(ctx, repo, number)
		if err != nil {
			return err
		}
		if DraftID(fleet.Text(issue["body"])) != fleet.Text(child["id"]) || !s.options.Own(row(issue["user"])) {
			return fail(409, "Issueの投稿者またはマーカーが一致しません。")
		}
		intent := row(child["_groupLinkIntent"])
		if intent == nil {
			if child, err = s.saveGroupLinkIntent(ctx, child, entry, issue, peers); err != nil {
				return err
			}
			intent = row(child["_groupLinkIntent"])
		}
		if issue, err = s.applyGroupLink(ctx, repo, number, issue, intent); err != nil {
			return err
		}
		if err = s.saveGroupLinkResult(ctx, child, repo, number, issue); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) saveGroupLinkResult(ctx context.Context, child Record, repo string, number int64, issue map[string]any) error {
	_, err := s.options.Records.Change(ctx, "drafts", fleet.Text(child["id"]), func(tx *sql.Tx, current Record) (Record, error) {
		if current == nil || fleet.Text(current["status"]) != "submitted" || fleet.Text(row(current["_groupLinkIntent"])["after"]) != fleet.Text(issue["body"]) {
			return nil, fail(409, "分割先が更新されています。")
		}
		key := fmt.Sprintf("%s#%d", repo, number)
		var raw string
		if err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM job_filings WHERE key=?"), key).Scan(&raw); err != nil {
			return nil, err
		}
		var entry pending
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return nil, err
		}
		if entry.Job == nil || entry.Job.View == nil {
			return nil, fail(409, "保存済みの実装入力がありません。")
		}
		entry.Job.View["acceptanceTransferInput"] = transferInput(issue)
		sum := sha256.Sum256([]byte(fleet.Text(issue["body"])))
		if row(entry.Job.View["completionPlanEvidence"]) != nil {
			evidence := clone(row(entry.Job.View["completionPlanEvidence"]))
			evidence["bodySHA256"] = hex.EncodeToString(sum[:])
			entry.Job.View["completionPlanEvidence"] = evidence
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, bindSQL("UPDATE job_filings SET json=? WHERE key=?"), string(encoded), key); err != nil {
			return nil, err
		}
		next := clone(current)
		next["postBody"] = issue["body"]
		next["preparedJob"] = entry.Job
		next["_groupLinkDone"] = true
		return next, nil
	})
	return err
}

// savedJobFiling reads the implementation input saved when the issue was filed.
func (s *Service) savedJobFiling(ctx context.Context, repo string, number int64) (pending, error) {
	var pendingRaw string
	if err := s.options.Records.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM job_filings WHERE key=?"), fmt.Sprintf("%s#%d", repo, number)).Scan(&pendingRaw); err != nil {
		return pending{}, fail(409, "相互参照を更新する前の実装入力がありません。")
	}
	var entry pending
	if err := json.Unmarshal([]byte(pendingRaw), &entry); err != nil {
		return pending{}, err
	}
	if entry.Job == nil || entry.Job.View == nil {
		return pending{}, fail(409, "保存済みの実装入力がありません。")
	}
	return entry, nil
}

// saveGroupLinkIntent records the issue body before and after adding the peer links, so a
// retry applies the same change. The issue must be unchanged since filing.
func (s *Service) saveGroupLinkIntent(ctx context.Context, child Record, entry pending, issue map[string]any, peers []string) (Record, error) {
	expected := fleet.Text(row(entry.Job.View["acceptanceTransferInput"])["revision"])
	if expected == "" || expected != fleet.Text(transferInput(issue)["revision"]) {
		return nil, fail(409, "起票後にIssue本文が変更されました。")
	}
	original := fleet.Text(issue["body"])
	target := original + "\n\n## 同じ組のIssue\n"
	for _, peer := range peers {
		target += "- " + peer + "\n"
	}
	return s.change(ctx, child, func(next Record) { next["_groupLinkIntent"] = Record{"before": original, "after": target} })
}

// applyGroupLink updates the issue body to the recorded target unless it is already there.
func (s *Service) applyGroupLink(ctx context.Context, repo string, number int64, issue map[string]any, intent Record) (map[string]any, error) {
	before, after := fleet.Text(intent["before"]), fleet.Text(intent["after"])
	current := fleet.Text(issue["body"])
	if current != after {
		if current != before {
			return nil, fail(409, "Issue本文が変更されたため相互参照を更新できません。")
		}
		if issue["content_version"] == nil {
			return nil, fail(409, "Issue本文の版を確認できません。")
		}
		if err := s.options.GroupIssueLinks.Update(ctx, repo, number, after, issue["content_version"]); err != nil {
			return nil, err
		}
		issue, err := s.options.GroupIssueLinks.Get(ctx, repo, number)
		if err != nil {
			return nil, err
		}
		if fleet.Text(issue["body"]) != after {
			return nil, fail(409, "更新後のIssue本文を確認できません。")
		}
		return issue, nil
	}
	return issue, nil
}
