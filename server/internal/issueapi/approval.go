package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"regexp"
	"strings"
)

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

func (s *Service) approveDeletions(ctx context.Context, repo string, n int64, data Object, p auth.Principal) (Object, error) {
	id, head := fleet.Text(data["id"]), fleet.Text(data["sha"])
	statement, route, pc, instructionAt := "", "screen", "", ""
	if p.Kind == auth.PC {
		// トークンの承認は権限変更の承認と同じ形で、利用者の発言そのままと指示の日時を必須にする。
		head, statement, route, pc = fleet.Text(data["head"]), strings.TrimSpace(fleet.Text(data["statement"])), "token", p.Name
		if !jobs.MeaningfulApproval(statement) {
			return nil, fail(400, "承認のSHAと具体的な利用者発言を確認してください。")
		}
		at, ok := contract.InstructionAt(data["instructionAt"], s.o.Now())
		if !ok {
			return nil, fail(400, contract.InstructionAtProblem)
		}
		instructionAt = at
	}
	if !uuid.MatchString(id) || !sha.MatchString(head) || len(fleet.Items(data["images"])) > 0 {
		return nil, fail(400, "承認IDと対象SHAが不正です。")
	}
	unlock := lock(&s.locks, "operation:"+id)
	defer unlock()
	issueUnlock := lock(&s.locks, fmt.Sprintf("issue:%s#%d", repo, n))
	defer issueUnlock()
	payload := Object{"repo": repo, "number": n, "action": "approve-deletions", "text": "", "value": "approve", "sha": head}
	if route == "token" {
		payload["statement"], payload["via"], payload["pc"], payload["instructionAt"] = statement, route, pc, instructionAt
	}
	saved, e := s.read(ctx, "actions", id)
	if e != nil {
		return nil, e
	}
	if saved != nil && !samePayload(fleet.ObjectOrEmpty(saved["payload"]), payload) {
		return nil, fail(409, "送信IDは別の操作で使用済みです。")
	}
	if saved == nil {
		saved = Object{"payload": payload}
	}
	save := func() error { return s.save(ctx, "actions", id, saved) }
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if job == nil {
		return nil, fail(409, "承認対象のジョブがありません。")
	}
	if saved["approval"] == nil {
		pending := fleet.ObjectOrEmpty(job.View["deletionApproval"])
		if job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "deletions" || pending["head"] != head || job.PR == nil {
			return nil, fail(409, "削除の承認対象が変わりました。")
		}
		account := "受付画面の利用者"
		if route == "token" {
			account = "運用代行トークン " + pc
		}
		saved["approval"] = Object{"sha": head, "items": pending["items"], "total": pending["total"], "pr": *job.PR, "account": account, "at": s.now()}
		if e = save(); e != nil {
			return nil, e
		}
	}
	if saved["applied"] != true {
		if _, e = s.o.Jobs.Control(ctx, job.ID, jobs.ControlInput{ID: id, Action: "approve-deletions", By: via(p), Head: head}, jobs.ControlOptions{}); e != nil {
			return nil, e
		}
		saved["applied"] = true
		if e = save(); e != nil {
			return nil, e
		}
	}
	approval := fleet.ObjectOrEmpty(saved["approval"])
	body := deletionApprovalAudit(approval, id, head, route, pc, statement, instructionAt)
	if e = s.postDeletionAudit(ctx, repo, n, body, approval, saved, save); e != nil {
		return nil, e
	}
	s.emit(repo, n)
	return Object{"id": fleet.ObjectOrEmpty(saved["comment"])["id"], "pr": fleet.ObjectOrEmpty(saved["prComment"])["id"]}, nil
}

// deletionApprovalAudit は削除の承認を監査する投稿の本文を、承認の経路ごとの書式で作る。
// データ削除SQLの保留では Worker が PR の変更ファイル全部をパスの文字列で送るため、見出しと項目をそれに合わせる。
func deletionApprovalAudit(approval Object, id, head, route, pc, statement, instructionAt string) string {
	sql := false
	for _, v := range fleet.Items(approval["items"]) {
		if _, ok := v.(string); ok {
			sql = true
		}
	}
	title, list := "削除・改名・種別変更の承認の記録", "対象ファイル:"
	if sql {
		title, list = "データ削除SQLを含むPRの承認の記録", "対象（データ削除SQLを含むPRの変更ファイル）:"
	}
	lines := []string{title + "（受付画面での操作）", "承認: " + fleet.Text(approval["account"]), "時刻: " + fleet.Text(approval["at"]), "対象コミット: " + head, list}
	if route == "token" {
		lines = []string{title + "（チャットの利用者の承認）", "承認経路: token", "送信元PC: " + pc, "利用者の発言: " + strings.ReplaceAll(statement, "<!--", "&lt;!--"), "指示の日時: " + instructionAt, "時刻: " + fleet.Text(approval["at"]), "対象コミット: " + head, list}
	}
	for _, v := range fleet.Items(approval["items"]) {
		item := fleet.ObjectOrEmpty(v)
		if text, ok := v.(string); ok {
			item = Object{"kind": "changed", "path": text}
		}
		path := auditPath(item["path"])
		if from := auditPath(item["from"]); from != "" {
			path = from + " → " + path
		}
		kind := fleet.Text(item["kind"])
		if kind == "changed" {
			kind = "変更ファイル"
		}
		lines = append(lines, "- "+kind+": "+path)
	}
	return strings.Join(lines, "\n") + "\n\n<!-- issue-desk-action:" + id + " -->"
}

// auditPath は監査の本文に埋め込むパスを、長さを切り、改行とコメントの開始を無害化して返す。
func auditPath(value any) string {
	path := strings.Join(strings.FieldsFunc(clipped(value, 500), func(r rune) bool { return r == '\n' || r == '\r' }), " ")
	return strings.ReplaceAll(path, "<!--", "&lt;!--")
}

// postDeletionAudit は監査の本文を Issue と PR へ1回ずつ投稿する。既に同じ本文があればそれを記録し、結果を照合できない再投稿は断る。
func (s *Service) postDeletionAudit(ctx context.Context, repo string, n int64, body string, approval, saved Object, save func() error) error {
	for _, destination := range []string{"comment", "prComment"} {
		if saved[destination] != nil {
			continue
		}
		index := n
		if destination == "prComment" {
			index, _ = number(approval["pr"])
		}
		comments, e := s.o.Gitea.Comments(ctx, repo, index)
		if e != nil {
			return e
		}
		var found any
		for _, c := range comments {
			if s.o.Own(fleet.ObjectOrEmpty(c["user"])) && c["body"] == body {
				found = c["id"]
				break
			}
		}
		attempt := "attempted"
		if destination == "prComment" {
			attempt = "prAttempted"
		}
		if found == nil {
			if saved[attempt] == true {
				return fail(503, "承認の投稿結果を照合できません。同じIDで確認を続けてください。")
			}
			saved[attempt] = true
			if e := save(); e != nil {
				return e
			}
			c, e := s.o.Gitea.Comment(ctx, repo, index, body)
			if e != nil {
				if explicitRejection(e) {
					saved[attempt] = false
					_ = save()
				}
				return e
			}
			found = c["id"]
		}
		saved[destination] = Object{"id": found}
		if e := save(); e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) handover(ctx context.Context, data Object, p auth.Principal) (Object, error) {
	repo, n, e := s.target(ctx, data)
	if e != nil {
		return nil, e
	}
	config, e := s.configuration()
	if e != nil {
		return nil, e
	}
	id, host := fleet.Text(data["id"]), fleet.Text(data["host"])
	knownHost := assignmentHasHost(fleet.ObjectOrEmpty(config["assignments"])["hosts"], host)
	if !uuid.MatchString(id) || !hostPattern.MatchString(host) || !knownHost {
		return nil, fail(400, "送信IDと担当表にあるホストを指定してください。")
	}
	unlock := lock(&s.locks, "operation:"+id)
	defer unlock()
	issueUnlock := lock(&s.locks, fmt.Sprintf("issue:%s#%d", repo, n))
	defer issueUnlock()
	payload := Object{"repo": repo, "number": n, "action": "handover", "text": "", "host": host, "via": via(p)}
	saved, e := s.read(ctx, "actions", id)
	if e != nil {
		return nil, e
	}
	if saved != nil && !samePayload(fleet.ObjectOrEmpty(saved["payload"]), payload) {
		return nil, fail(409, "送信IDは別の操作で使用済みです。")
	}
	if saved == nil {
		saved = Object{"payload": payload}
	}
	save := func() error { return s.save(ctx, "actions", id, saved) }
	if saved["comment"] != nil {
		return Object{"id": fleet.ObjectOrEmpty(saved["comment"])["id"], "host": host}, nil
	}
	issue, e := s.o.Gitea.Issue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if job != nil || issue["state"] != "open" || issue["pull_request"] != nil || !s.o.Own(fleet.ObjectOrEmpty(issue["user"])) || !uuid.MatchString(filing.DraftID(fleet.Text(issue["body"]))) {
		return nil, fail(409, "担当を引き継げるのは受付から登録した未着手の依頼だけです。")
	}
	body := fmt.Sprintf("担当を %s へ引き継ぎました（%s。操作ID %s）。\n\n<!-- issue-desk:handover %s -->\n<!-- issue-desk-action:%s -->", host, via(p), id, host, id)
	comments, e := s.o.Gitea.Comments(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	for _, c := range comments {
		if s.o.Own(fleet.ObjectOrEmpty(c["user"])) && c["body"] == body {
			saved["comment"] = Object{"id": c["id"]}
			if e = save(); e != nil {
				return nil, e
			}
			return Object{"id": c["id"], "host": host}, nil
		}
	}
	if saved["attempted"] == true {
		return nil, fail(409, "前回の引継ぎ結果を照合できません。再投稿しません。")
	}
	if saved["labels"] != true {
		if e = s.replaceHostLabel(ctx, repo, n, host, issue); e != nil {
			return nil, e
		}
		saved["labels"] = true
		if e = save(); e != nil {
			return nil, e
		}
	}
	saved["attempted"] = true
	if e = save(); e != nil {
		return nil, e
	}
	c, e := s.o.Gitea.Comment(ctx, repo, n, body)
	if e != nil {
		if explicitRejection(e) {
			saved["attempted"] = false
			_ = save()
		}
		return nil, e
	}
	saved["comment"] = Object{"id": c["id"]}
	saved["at"] = s.now()
	if e = save(); e != nil {
		return nil, e
	}
	s.emit(repo, n)
	return Object{"id": c["id"], "host": host}, nil
}

// replaceHostLabel は Issue の担当PCのラベルを引き継ぎ先のものに付け替える。ラベルが無ければ作る。
func (s *Service) replaceHostLabel(ctx context.Context, repo string, n int64, host string, issue Object) error {
	labelName := "host/" + host
	labels, e := s.o.Gitea.Pages(ctx, "/repos/"+repo+"/labels")
	if e != nil {
		return e
	}
	var labelID any
	for _, label := range labels {
		if label["name"] == labelName {
			labelID = label["id"]
		}
	}
	if labelID == nil {
		label := Object{}
		if e = s.o.Gitea.Do(ctx, "POST", "/repos/"+repo+"/labels", Object{"name": labelName, "color": "607d8b", "description": "このIssueの実装を担当するPC"}, &label); e != nil {
			return e
		}
		labelID = label["id"]
	}
	ids := []any{}
	for _, v := range fleet.Items(issue["labels"]) {
		label := fleet.ObjectOrEmpty(v)
		if !strings.HasPrefix(fleet.Text(label["name"]), "host/") {
			ids = append(ids, label["id"])
		}
	}
	ids = append(ids, labelID)
	if e := s.o.Gitea.Do(ctx, "PUT", fmt.Sprintf("/repos/%s/issues/%d/labels", repo, n), Object{"labels": ids}, nil); e != nil {
		return e
	}
	return nil
}

// assignmentHasHost accepts both normalized []string and JSON-decoded []any.
func assignmentHasHost(value any, host string) bool {
	switch hosts := value.(type) {
	case []string:
		for _, name := range hosts {
			if name == host {
				return true
			}
		}
	case []any:
		for _, candidate := range hosts {
			if name, ok := candidate.(string); ok && name == host {
				return true
			}
		}
	}
	return false
}
