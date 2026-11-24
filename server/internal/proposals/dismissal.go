package proposals

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"issue-desk-intake/server/internal/usage"
)

func validNumber(v any) bool {
	n := float64(num(v))
	return n >= 1 && n <= 9007199254740991 && n == fleetNumber(v)
}
func fleetNumber(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		n, _ := x.Float64()
		return n
	}
	return math.NaN()
}
func dismissalTarget(data Object) error {
	if !repoPattern.MatchString(str(data["project"])) || !validNumber(data["number"]) || (data["proposalId"] != nil && data["proposalId"] != "" && !contract.FilledText(data["proposalId"], 200)) {
		return fail(400, "project・number・proposalIdで案を指定してください。")
	}
	return nil
}
func (s *Service) rejected(ctx context.Context, data Object) (Object, error) {
	if e := dismissalTarget(data); e != nil {
		return nil, e
	}
	if e := s.o.Project(str(data["project"])); e != nil {
		return nil, e
	}
	if !validNumber(data["commentId"]) {
		return nil, fail(400, "commentIdを指定してください。")
	}
	key := fmt.Sprintf("rejection:%s#%d:%d", str(data["project"]), num(data["number"]), num(data["commentId"]))
	r, e := s.read(ctx, key)
	if e != nil {
		return nil, e
	}
	if r == nil || r["type"] != "rejection" || str(r["id"]) != str(data["proposalId"]) || r["repo"] != data["project"] || num(r["number"]) != num(data["number"]) || num(r["commentId"]) != num(data["commentId"]) {
		return nil, fail(404, "指定した未採用案が見つかりません。")
	}
	if r["status"] != "rejected" && r["status"] != "scan_excluded" {
		return nil, fail(409, "未採用・走査除外の案だけを終了できます。")
	}
	if str(data["proposalId"]) == "" {
		all, err := s.records.List(ctx, "proposals")
		if err != nil {
			return nil, err
		}
		for _, item := range all {
			if item["type"] == "proposal" && item["repo"] == r["repo"] && num(item["number"]) == num(r["number"]) && num(item["commentId"]) == num(r["commentId"]) {
				return nil, fail(409, "採用済みの案です。")
			}
		}
	}
	adopted, e := s.read(ctx, source(data))
	if e != nil {
		return nil, e
	}
	_, found, e := s.records.Get(ctx, "drafts", draftID(str(data["project"]), num(data["number"]), str(data["proposalId"])))
	if e != nil {
		return nil, e
	}
	if adopted["type"] == "proposal" || found {
		return nil, fail(409, "採用済みの案です。保存済みの相談を確認してください。")
	}
	return r, nil
}
func (s *Service) Candidate(ctx context.Context, data Object) (Object, error) {
	r, e := s.rejected(ctx, data)
	if e != nil {
		return nil, e
	}
	comment := Object{}
	route := fmt.Sprintf("/repos/%s/issues/comments/%d", str(r["repo"]), num(r["commentId"]))
	if e = s.o.Gitea.Do(ctx, "GET", route, nil, &comment); e != nil {
		return nil, e
	}
	p, fingerprint, parseErr := parse(str(comment["body"]))
	malformed := str(r["id"]) == ""
	if malformed && parseErr == nil {
		return nil, fail(409, "元コメントは現在解析可能です。")
	}
	if !malformed && parseErr != nil {
		return nil, parseErr
	}
	if malformed {
		sum := sha256.Sum256([]byte(str(comment["body"])))
		fingerprint = fmt.Sprintf("%x", sum)
	}
	u, ue := url.Parse(str(comment["issue_url"]))
	want := fmt.Sprintf("/%s/issues/%d", str(r["repo"]), num(r["number"]))
	if !contract.FilledText(r["author"], 200) || num(comment["id"]) != num(r["commentId"]) || (!malformed && (p["id"] != r["id"] || p["source"] != fmt.Sprintf("%s#%d", str(r["repo"]), num(r["number"])))) || ue != nil || !strings.HasSuffix(u.Path, want) || obj(comment["user"])["login"] != r["author"] {
		return nil, fail(409, "元コメントと案・元Issueを照合できません。")
	}
	if _, e = s.rejected(ctx, data); e != nil {
		return nil, e
	}
	updated := comment["updated_at"]
	if updated == nil {
		updated = comment["created_at"]
	}
	if malformed {
		return Object{"project": r["repo"], "number": r["number"], "proposalId": "", "commentId": r["commentId"], "fingerprint": fingerprint, "fingerprintKind": "comment-body-sha256", "body": str(comment["body"]), "parseError": parseErr.Error(), "recordedReason": recordedReason(r), "postedAt": comment["created_at"], "updatedAt": updated}, nil
	}
	purpose, acceptance := p["purpose"], p["acceptance"]
	if num(p["version"]) == 3 {
		raw, _ := json.Marshal(p["items"])
		items := []Object{}
		_ = json.Unmarshal(raw, &items)
		purposes := []string{}
		conditions := []string{}
		for _, item := range items {
			purposes = append(purposes, str(item["problem"]))
			conditions = append(conditions, str(item["expected"])+"\n"+str(item["test"]))
		}
		purpose = strings.Join(purposes, "\n")
		acceptance = conditions
	}
	return Object{"project": r["repo"], "number": r["number"], "proposalId": r["id"], "commentId": r["commentId"], "fingerprint": fingerprint, "fingerprintKind": "proposal-json-sha256", "title": p["title"], "postedAt": comment["created_at"], "updatedAt": updated, "purpose": purpose, "acceptance": acceptance}, nil
}

var instructionPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?(?:Z|[+-]\d{2}:\d{2})$`)

func (s *Service) instruction(value, created any) error {
	if !instructionPattern.MatchString(str(value)) {
		return fail(400, "instructionAtはUTCオフセット付きISO 8601日時で指定してください。")
	}
	raw := str(value)
	if !strings.HasSuffix(raw, "Z") {
		offset := raw[len(raw)-6:]
		hour, _ := strconv.Atoi(offset[1:3])
		minute, _ := strconv.Atoi(offset[4:6])
		if hour > 23 || minute > 59 {
			return fail(400, "instructionAtのUTCオフセットが不正です。")
		}
	}
	at, e := time.Parse(time.RFC3339Nano, str(value))
	if e != nil || at.After(s.now()) {
		return fail(400, "instructionAtの日時が不正または未来です。")
	}
	if created != nil {
		c, e := time.Parse(time.RFC3339Nano, str(created))
		if e != nil || c.After(at) {
			return fail(409, "指示時刻より後に作成・更新された対象は操作できません。")
		}
	}
	return nil
}
func (s *Service) Dismiss(ctx context.Context, data Object, via string, token bool) (Object, error) {
	if e := dismissalTarget(data); e != nil {
		return nil, e
	}
	if token {
		if e := s.instruction(data["instructionAt"], nil); e != nil {
			return nil, e
		}
	}
	if !uuidPattern.MatchString(str(data["id"])) || !hashPattern.MatchString(str(data["fingerprint"])) || !contract.FilledText(data["reason"], 1000) {
		return nil, fail(400, "送信ID・fingerprint・理由を指定してください。")
	}
	if e := s.o.Project(str(data["project"])); e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "dismissal:" + source(data)
	if str(data["proposalId"]) == "" {
		key = fmt.Sprintf("dismissal-comment:%s#%d:%d", str(data["project"]), num(data["number"]), num(data["commentId"]))
	}
	prior, e := s.read(ctx, key)
	if e != nil {
		return nil, e
	}
	reason := usage.Redact(strings.TrimSpace(str(data["reason"])))
	all, e := s.records.List(ctx, "proposals")
	if e != nil {
		return nil, e
	}
	if e := dismissalIDUnused(all, key, data); e != nil {
		return nil, e
	}
	if repeated, e := repeatedDismissal(prior, data, reason); e != nil || repeated {
		if e != nil {
			return nil, e
		}
		return prior, nil
	}
	candidate, e := s.Candidate(ctx, data)
	if e != nil {
		return nil, e
	}
	if candidate["fingerprint"] != data["fingerprint"] {
		return nil, fail(409, "案の内容が変わりました。")
	}
	if token {
		if e = s.instruction(data["instructionAt"], candidate["updatedAt"]); e != nil {
			return nil, e
		}
	}
	rejection, e := s.rejected(ctx, data)
	if e != nil {
		return nil, e
	}
	tx, e := s.o.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	// Reading inside the write transaction detects a concurrent adoption.
	if e = notAdoptedTx(ctx, tx, data); e != nil {
		return nil, e
	}
	history := arr(prior["history"])
	if prior != nil {
		delete(prior, "history")
		history = append(history, prior)
	}
	if history == nil {
		history = []any{}
	}
	mark := Object{"type": "dismissal", "key": key, "repo": data["project"], "number": data["number"], "id": str(data["proposalId"]), "operationId": data["id"], "commentId": data["commentId"], "fingerprint": candidate["fingerprint"], "title": str(candidate["title"]), "status": "dismissed", "reason": reason, "dismissedAt": s.at(), "via": via, "history": history}
	if token {
		mark["instructionAt"] = data["instructionAt"]
	}
	rejection["fingerprint"] = candidate["fingerprint"]
	rejectionKey := fmt.Sprintf("rejection:%s#%d:%d", str(data["project"]), num(data["number"]), num(data["commentId"]))
	if e = saveTx(ctx, tx, rejectionKey, rejection); e != nil {
		return nil, e
	}
	if e = saveTx(ctx, tx, key, mark); e != nil {
		return nil, e
	}
	return mark, tx.Commit()
}

// dismissalIDUnused refuses a send ID already used to dismiss another target.
func dismissalIDUnused(all []consultations.Record, key string, data Object) error {
	for _, item := range all {
		sameTarget := item["key"] == key || (item["key"] == nil && item["repo"] == data["project"] && num(item["number"]) == num(data["number"]) && str(item["id"]) == str(data["proposalId"]) && (str(data["proposalId"]) != "" || num(item["commentId"]) == num(data["commentId"])))
		if item["type"] == "dismissal" && !sameTarget {
			if item["operationId"] == data["id"] {
				return fail(409, "同じ送信IDが別の内容で使われています。")
			}
			for _, old := range arr(item["history"]) {
				if obj(old)["operationId"] == data["id"] {
					return fail(409, "同じ送信IDが別の内容で使われています。")
				}
			}
		}
	}
	return nil
}

// repeatedDismissal reports whether the request repeats the saved dismissal, and refuses a
// send ID reused with other content or superseded by a later operation.
func repeatedDismissal(prior, data Object, reason string) (bool, error) {
	if prior["operationId"] == data["id"] {
		if prior["fingerprint"] != data["fingerprint"] || num(prior["commentId"]) != num(data["commentId"]) || prior["reason"] != reason || (prior["instructionAt"] != nil && prior["instructionAt"] != data["instructionAt"]) {
			return false, fail(409, "同じ送信IDが別の内容で使われています。")
		}
		return true, nil
	}
	for _, old := range arr(prior["history"]) {
		if obj(old)["operationId"] == data["id"] {
			return false, fail(409, "この送信IDの後に別の操作があります。")
		}
	}
	return false, nil
}

// notAdoptedTx refuses the dismissal when the proposal or its draft was already adopted.
func notAdoptedTx(ctx context.Context, tx *sql.Tx, data Object) error {
	var raw string
	e := tx.QueryRowContext(ctx, bindSQL(`SELECT json FROM proposals WHERE id=?`), source(data)).Scan(&raw)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if e == nil {
		m := Object{}
		if json.Unmarshal([]byte(raw), &m) != nil {
			return fail(503, "案の保存記録を読めません。")
		}
		if m["type"] == "proposal" {
			return fail(409, "採用済みの案です。")
		}
	}
	var one int
	e = tx.QueryRowContext(ctx, bindSQL(`SELECT 1 FROM drafts WHERE id=?`), draftID(str(data["project"]), num(data["number"]), str(data["proposalId"]))).Scan(&one)
	if e == nil {
		return fail(409, "採用済みの相談です。")
	}
	if e != sql.ErrNoRows {
		return e
	}
	return nil
}

func saveTx(ctx context.Context, tx *sql.Tx, key string, value Object) error {
	raw, e := json.Marshal(value)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, bindSQL(`INSERT INTO proposals(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json`), key, string(raw))
	return e
}

func recordedReason(r Object) string {
	reason := str(r["reason"])
	if reason == "" || reason == "案の形式が不正です。" {
		return "案の形式が不正です（当時の詳細未保存）。"
	}
	return reason
}
