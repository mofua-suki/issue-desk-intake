package proposals

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"issue-desk-intake/server/internal/pullcheck"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

var errFollowupNumbering = errors.New("元Issueの共通番号を確認できません")
var errStaleWorkerHead = fail(409, "後続案とPRのheadが一致しません。")
var errWorkerHead = errors.New("PRのhead.shaが欠測または不正です。再確認します。")

func (s *Service) reject(ctx context.Context, repo string, n int64, comment, p Object, fingerprint, reason string) error {
	key := fmt.Sprintf("rejection:%s#%d:%d", repo, n, num(comment["id"]))
	prior, err := s.read(ctx, key)
	if err != nil {
		return err
	}
	// Keep the original refusal; rereading a comment never grants permission.
	if prior != nil {
		return nil
	}
	record := Object{"type": "rejection", "key": key, "repo": repo, "number": n,
		"id": p["id"], "commentId": comment["id"], "author": obj(comment["user"])["login"],
		"status": "rejected", "scope": "comment", "fingerprint": fingerprint, "reason": reason,
		"sourceKind": "issues", "postedAt": comment["created_at"], "checkedAt": s.at()}
	if reason == oldHeadReason {
		record["headRuleVersion"] = "1"
	}
	return s.save(ctx, key, record)
}

func (s *Service) permitted(ctx context.Context, repo string, n int64, comment, p Object, fingerprint string) (bool, error) {
	config, err := s.o.Config()
	if err != nil {
		return false, err
	}
	writer := str(obj(comment["user"])["login"])
	trusted := false
	for _, allowed := range proposalWriters(config) {
		trusted = trusted || writer != "" && writer == allowed
	}
	u, errURL := url.Parse(str(comment["issue_url"]))
	if !trusted || !validNumber(comment["id"]) || p["source"] != fmt.Sprintf("%s#%d", repo, n) || errURL != nil || !strings.HasSuffix(u.Path, fmt.Sprintf("/%s/issues/%d", repo, n)) {
		return false, s.reject(ctx, repo, n, comment, p, fingerprint, "投稿者・元Issue・コメントを照合できません。")
	}
	return s.recordsAllowAdoption(ctx, repo, n, comment, p, fingerprint)
}

// pull only resolves canonical links for the configured source repository. The
// proposal's URL is never used as an arbitrary HTTP destination.
func (s *Service) merged(ctx context.Context, repo, link string) (bool, error) {
	pr, err := s.mergedPull(ctx, repo, link)
	return pr != nil && pr["merged"] == true, err
}

func (s *Service) mergedPull(ctx context.Context, repo, link string) (Object, error) {
	u, err := url.Parse(link)
	if err != nil || s.o.Link == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fail(409, "PRのURLを照合できません。")
	}
	suffix := "/" + repo + "/pulls/"
	at := strings.LastIndex(u.Path, suffix)
	if at < 0 {
		return nil, fail(409, "同じリポジトリのPRではありません。")
	}
	n, err := strconv.ParseInt(u.Path[at+len(suffix):], 10, 64)
	if err != nil || n < 1 || s.o.Link(repo, "pulls", n) != link {
		return nil, fail(409, "PRの接続先を照合できません。")
	}
	if cached := pullcheck.Get(ctx, repo, link); cached != nil {
		return Object(cached), nil
	}
	pr := Object{}
	if err := s.o.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
		return nil, err
	}
	if num(pr["number"]) != n || pr["html_url"] != link {
		return nil, fail(409, "PRの応答を照合できません。")
	}
	pullcheck.Put(ctx, repo, link, pr)
	return pr, nil
}

// workerHead matches only the published Worker ID contract. Other IDs retain
// the existing adoption rules. The SHA is hashed as its original text.
var workerID = regexp.MustCompile(`^worker-(.+)-([^-]+)-([0-9a-f]{32})-([^-]+)$`)
var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func workerHead(id string, pr Object) (bool, error) {
	match := workerID.FindStringSubmatch(id)
	if match == nil {
		return true, nil
	}
	head, ok := obj(pr["head"])["sha"].(string)
	if !ok || !commitSHA.MatchString(head) {
		return false, errWorkerHead
	}
	sum := sha256.Sum256([]byte(head))
	return fmt.Sprintf("%x", sum[:16]) == match[3], nil
}

// ValidateNewPost checks the current PR head only on the new POST path.
// ValidateDraft remains responsible for provenance, including during recovery.
func (s *Service) ValidateNewPost(ctx context.Context, d consultations.Record) error {
	mark, err := s.read(ctx, str(obj(d["proposal"])["key"]))
	if err != nil {
		return err
	}
	if mark["type"] != "proposal" || mark["draftId"] != d["id"] {
		return fail(409, "採用済みの後続案を確認できません。")
	}
	p := obj(mark["proposal"])
	pr, err := s.mergedPull(ctx, str(mark["repo"]), str(p["pr"]))
	if err != nil {
		return err
	}
	if pr["merged"] != true {
		return fail(409, "PRのマージを確認できません。")
	}
	matches, err := s.workerHead(ctx, str(mark["repo"]), num(mark["number"]), p, pr)
	if err != nil {
		return err
	}
	if !matches {
		return errStaleWorkerHead
	}
	return nil
}

// proposalState is called with mu held, only around local decisions and writes.
func (s *Service) proposalState(ctx context.Context, repo string, n int64, comment, p Object, fingerprint string) (Object, bool, error) {
	ok, err := s.permitted(ctx, repo, n, comment, p, fingerprint)
	if err != nil || !ok {
		return nil, true, err
	}
	key := fmt.Sprintf("%s#%d:%s", repo, n, str(p["id"]))
	mark, err := s.read(ctx, key)
	if err != nil {
		return nil, true, err
	}
	if mark != nil && (mark["type"] != "proposal" || mark["fingerprint"] != fingerprint || len(arr(mark["conflicts"])) > 0) {
		if mark["type"] == "proposal" && mark["fingerprint"] != fingerprint {
			mark["conflicts"] = []any{Object{"commentId": comment["id"], "fingerprint": fingerprint}}
			return mark, true, s.save(ctx, key, mark)
		}
		return mark, true, nil
	}
	return mark, mark["status"] == "discarded" || mark["status"] == "notified", nil
}

// adopt returns whether the persisted candidate needs another attempt. Pending
// candidates recheck the PR without reloading every Issue's comment collection.
func (s *Service) adopt(ctx context.Context, repo string, n int64, comment Object) (bool, error) {
	// Re-expose a dismissed malformed comment if its body changes on a later scan.
	if readErr := s.refreshMalformedRejection(ctx, repo, n, comment); readErr != nil {
		return true, readErr
	}
	p, fingerprint, err := parse(str(comment["body"]))
	if err != nil {
		reason := "案の形式が不正です。"
		if problem, ok := err.(*contract.Problem); ok {
			reason = problem.Message
		}
		err = s.reject(ctx, repo, n, comment, nil, "", reason)
		return err != nil, err
	}
	if num(p["version"]) != 3 {
		return false, nil
	}
	retry, err := s.reconsiderHead(ctx, repo, n, comment, p, fingerprint)
	if err != nil || retry {
		return retry, err
	}
	s.mu.Lock()
	mark, stop, err := s.proposalState(ctx, repo, n, comment, p, fingerprint)
	s.mu.Unlock()
	if err != nil || stop {
		return err != nil, err
	}
	if mark != nil {
		if next, e := time.Parse(time.RFC3339Nano, str(mark["headRetryAfter"])); e == nil && s.now().Before(next) {
			return true, nil
		}
	}
	pr, err := s.mergedPull(ctx, repo, str(p["pr"]))
	if err != nil {
		// A permanent URL/response mismatch is reported once and remains visible
		// as a comment-scoped refusal. Transport failures remain retryable.
		if problem, ok := err.(*contract.Problem); ok && problem.Status == 409 {
			if saveErr := s.reject(ctx, repo, n, comment, p, fingerprint, problem.Message); saveErr != nil {
				return true, saveErr
			}
			return false, err
		}
		return true, err
	}
	if pr["merged"] != true {
		return true, nil
	}
	// Existing adopted drafts follow their original recovery rules.
	if mark == nil {
		matches, headErr := s.workerHead(ctx, repo, n, p, pr)
		if headErr != nil {
			return true, headErr
		}
		if !matches {
			err := s.reject(ctx, repo, n, comment, p, fingerprint, "PRのhead.shaとWorker案のIDが一致しません。")
			return err != nil, err
		}
	}
	if s.o.Desk == nil || !validNumber(s.o.Desk(repo, n)) || s.o.Link == nil {
		return true, errFollowupNumbering // Retain the candidate while numbering catches up.
	}
	draft, err := s.adoptionDraft(ctx, repo, n, comment, p, fingerprint)
	if err != nil || draft == nil {
		return err != nil, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if s.o.AcceptedContext != nil {
		ctx = s.o.AcceptedContext
	}
	// No proposals mutex is held while waiting for the filing gate or Gitea.
	submitted, sendErr := s.o.Submit(ctx, str(draft["id"]), consultations.Record{"revision": draft["revision"]})
	if submitted != nil && submitted["status"] == "submitted" && submitted["issue"] != nil {
		err := s.markProposalNotified(ctx, repo, n, p, submitted)
		return err != nil, err
	}
	if errors.Is(sendErr, errStaleWorkerHead) {
		if readErr := s.delayStaleHeadRetry(ctx, repo, n, p); readErr != nil {
			return true, readErr
		}
	}
	return true, sendErr
}

func (s *Service) adoptionDraft(ctx context.Context, repo string, n int64, comment, p Object, fingerprint string) (consultations.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Dismiss may have completed during the network request.
	mark, stop, err := s.proposalState(ctx, repo, n, comment, p, fingerprint)
	if err != nil || stop {
		return nil, err
	}
	key := fmt.Sprintf("%s#%d:%s", repo, n, str(p["id"]))
	dID := draftID(repo, n, str(p["id"]))
	body := followupBody(p, s.o.Desk(repo, n), s.o.Link(repo, "issues", n))
	draft, found, err := s.records.Get(ctx, "drafts", dID)
	if err != nil {
		return nil, err
	}
	if mark != nil && !found {
		mark["status"] = "discarded"
		return nil, s.save(ctx, key, mark)
	}
	if mark == nil {
		if found {
			return nil, fail(409, "案の記録がない保存済み下書きです。上書きしません。")
		}
		mark = Object{"type": "proposal", "key": key, "repo": repo, "number": n, "id": p["id"], "fingerprint": fingerprint,
			"commentId": comment["id"], "author": obj(comment["user"])["login"], "proposal": p,
			"draftId": dID, "status": "adopted", "sourceKind": "issues", "postedAt": comment["created_at"], "checkedAt": s.at()}
		draft = consultations.Record{"id": dID, "project": repo, "title": p["title"], "body": body, "provider": p["provider"],
			"owner": "", "proposal": Object{"key": key, "fingerprint": fingerprint}, "revision": 0, "status": "auto_ready", "ready": true,
			"questions": []any{}, "messages": []any{}, "createdAt": s.at(), "updatedAt": s.at()}
		setFollowupPriority(draft, p, s.at())
		tx, err := s.o.AppDB.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		raw, err := json.Marshal(draft)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, bindSQL(`INSERT INTO drafts(id,json) VALUES(?,?)`), dID, string(raw)); err != nil {
			return nil, err
		}
		if err = saveTx(ctx, tx, key, mark); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	if draft["status"] == "submitted" && draft["issue"] != nil {
		mark["issue"], mark["status"] = draft["issue"], "notified"
		return nil, s.save(ctx, key, mark)
	}
	switch draft["status"] {
	case "auto_ready", "draft", "submit_error", "submitting", "submit_unknown":
		return draft, nil
	default:
		return nil, nil // Closed/stopped/withdrawn drafts never resurrect.
	}
}

// ValidateDraft is also called by filing recovery, before history reconciliation
// and on both sides of body generation. It never trusts a draft flag alone.
// The checks run in this order, and each one stops at its first failure:
// the adoption record, the project, the source Issue and comment, the comment
// against the record, the refusals and the merge, and finally the draft content.
func (s *Service) ValidateDraft(ctx context.Context, d consultations.Record) error {
	mark, err := s.adoptedMark(ctx, d)
	if err != nil {
		return err
	}
	repo, n := str(mark["repo"]), num(mark["number"])
	if err := s.o.Project(repo); err != nil {
		return err
	}
	comment := Object{}
	if err := s.fetchSourceComment(ctx, repo, n, mark, &comment); err != nil {
		return err
	}
	p, fingerprint, err := sourceProposal(comment, mark)
	if err != nil {
		return err
	}
	if err := s.proposalStillAdoptable(ctx, repo, n, comment, p, fingerprint); err != nil {
		return err
	}
	return s.draftMatchesProposal(d, mark, p, fingerprint)
}

// adoptedMark reads the adoption record named by the draft's provenance and
// checks that the record and the draft point at each other.
func (s *Service) adoptedMark(ctx context.Context, d consultations.Record) (Object, error) {
	provenance := obj(d["proposal"])
	mark, err := s.read(ctx, str(provenance["key"]))
	if err != nil {
		return nil, err
	}
	repo, n := str(mark["repo"]), num(mark["number"])
	if mark["type"] != "proposal" || mark["status"] == "discarded" || len(arr(mark["conflicts"])) > 0 || mark["draftId"] != d["id"] || mark["fingerprint"] != provenance["fingerprint"] || draftID(repo, n, str(mark["id"])) != d["id"] || d["project"] != repo {
		return nil, fail(409, "採用済みの後続案と下書きを照合できません。")
	}
	return mark, nil
}

// fetchSourceComment reads the source Issue and the adopted comment from Gitea.
// The Issue must be the recorded number and not a PR. The comment is stored in
// out only when both reads succeed.
func (s *Service) fetchSourceComment(ctx context.Context, repo string, n int64, mark Object, out *Object) error {
	issue, comment := Object{}, Object{}
	if err := s.o.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d", repo, n), nil, &issue); err != nil {
		return err
	}
	if num(issue["number"]) != n || issue["pull_request"] != nil {
		return fail(409, "元Issueを確認できません。")
	}
	if err := s.o.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/comments/%d", repo, num(mark["commentId"])), nil, &comment); err != nil {
		return err
	}
	*out = comment
	return nil
}

// sourceProposal parses the comment body and checks that the version, ID,
// fingerprint, content, comment ID and author still match the adoption record.
func sourceProposal(comment, mark Object) (Object, string, error) {
	p, fingerprint, err := parse(str(comment["body"]))
	if err != nil {
		return nil, "", err
	}
	if num(p["version"]) != 3 || p["id"] != mark["id"] || fingerprint != mark["fingerprint"] || !reflect.DeepEqual(fleet.Clone(p), fleet.Clone(mark["proposal"])) || num(comment["id"]) != num(mark["commentId"]) || obj(comment["user"])["login"] != mark["author"] {
		return nil, "", fail(409, "元コメントの版・投稿者・内容が変わっています。")
	}
	return p, fingerprint, nil
}

// proposalStillAdoptable rechecks the writer and saved refusals, then the PR merge.
func (s *Service) proposalStillAdoptable(ctx context.Context, repo string, n int64, comment, p Object, fingerprint string) error {
	allowed, err := s.permitted(ctx, repo, n, comment, p, fingerprint)
	if err != nil {
		return err
	}
	if !allowed {
		return fail(409, "拒否・破棄された後続案です。")
	}
	merged, err := s.merged(ctx, repo, str(p["pr"]))
	if err != nil {
		return err
	}
	if !merged {
		return fail(409, "PRのマージを確認できません。")
	}
	return nil
}

// draftMatchesProposal checks that the draft's body, title, provider and
// provenance are exactly what the adopted proposal produces.
func (s *Service) draftMatchesProposal(d consultations.Record, mark, p Object, fingerprint string) error {
	repo, n := str(mark["repo"]), num(mark["number"])
	if s.o.Desk == nil || !validNumber(s.o.Desk(repo, n)) || d["body"] != followupBody(p, s.o.Desk(repo, n), s.o.Link(repo, "issues", n)) || d["title"] != p["title"] || d["provider"] != p["provider"] || !reflect.DeepEqual(obj(d["proposal"]), Object{"key": mark["key"], "fingerprint": fingerprint}) {
		return fail(409, "後続案から作った下書きの内容が一致しません。")
	}
	if priority := obj(p["priority"]); priority != nil && (d["priority"] != "high" || obj(d["prioritySource"])["reason"] != priority["reason"]) {
		return fail(409, "後続案の優先度または理由が下書きと一致しません。")
	}
	return nil
}

// refreshMalformedRejection updates the body fingerprint of a dismissed malformed comment,
// so a changed body is shown again.
func (s *Service) refreshMalformedRejection(ctx context.Context, repo string, n int64, comment Object) error {
	s.mu.Lock()
	rejectionKey := fmt.Sprintf("rejection:%s#%d:%d", repo, n, num(comment["id"]))
	old, readErr := s.read(ctx, rejectionKey)
	if readErr == nil && old != nil && str(old["id"]) == "" && str(old["fingerprint"]) != "" {
		sum := sha256.Sum256([]byte(str(comment["body"])))
		current := fmt.Sprintf("%x", sum)
		if old["fingerprint"] != current {
			old["fingerprint"] = current
			readErr = s.save(ctx, rejectionKey, old)
		}
	}
	s.mu.Unlock()
	return readErr
}

// markProposalNotified records the issue filed for the adopted proposal.
func (s *Service) markProposalNotified(ctx context.Context, repo string, n int64, p Object, submitted consultations.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s#%d:%s", repo, n, str(p["id"]))
	mark, err := s.read(ctx, key)
	if err != nil {
		return err
	}
	if mark == nil {
		return fail(409, "起票した後続案の保存記録がありません。")
	}
	mark["issue"], mark["status"], mark["error"] = submitted["issue"], "notified", nil
	return s.save(ctx, key, mark)
}

// delayStaleHeadRetry postpones the next adoption attempt by an hour after a stale worker head.
func (s *Service) delayStaleHeadRetry(ctx context.Context, repo string, n int64, p Object) error {
	s.mu.Lock()
	key := fmt.Sprintf("%s#%d:%s", repo, n, str(p["id"]))
	current, readErr := s.read(ctx, key)
	if readErr == nil && current != nil && current["status"] == "adopted" {
		current["headRetryAfter"] = s.now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		readErr = s.save(ctx, key, current)
	}
	s.mu.Unlock()
	return readErr
}

// recordsAllowAdoption reads the rejections and dismissal saved for the proposal and reports
// whether none of them blocks adopting this comment.
func (s *Service) recordsAllowAdoption(ctx context.Context, repo string, n int64, comment, p Object, fingerprint string) (bool, error) {
	// Rejections use one key per comment, while dismissals use one key per
	// proposal. Read them again at each decision so writes made while waiting
	// for Gitea or the filing gate are visible.
	prefix := fmt.Sprintf("rejection:%s#%d:", repo, n)
	dismissal := fmt.Sprintf("dismissal:%s#%d:%s", repo, n, str(p["id"]))
	// The final colon has the byte successor ';'. Pattern operators use byte
	// ordering independently of the database's locale and its LIKE wildcards.
	upper := prefix[:len(prefix)-1] + ";"
	rows, err := s.records.DB.QueryContext(ctx, bindSQL(`SELECT json FROM proposals WHERE (id ~>=~ ? AND id ~<~ ?) OR id = ?`), prefix, upper, dismissal)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	allowed := true
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var r Object
		if err := decoder.Decode(&r); err != nil {
			return false, err
		}
		if r == nil {
			return false, errors.New("saved record is not an object")
		}
		if r["repo"] != repo || num(r["number"]) != n {
			continue
		}
		if r["type"] == "rejection" && (num(r["commentId"]) == num(comment["id"]) || r["scope"] == "proposal" && r["id"] == p["id"] && r["fingerprint"] == fingerprint) && !(r["status"] == "head_reconsidered" && r["headReconsideredAt"] != nil && r["reason"] == oldHeadReason && r["scope"] != "proposal" && r["id"] == p["id"] && r["fingerprint"] == fingerprint && r["author"] == obj(comment["user"])["login"]) {
			allowed = false
		}
		if r["type"] == "dismissal" && r["id"] == p["id"] && r["status"] == "dismissed" && r["fingerprint"] == fingerprint {
			allowed = false
		}
	}
	return allowed, rows.Err()
}
