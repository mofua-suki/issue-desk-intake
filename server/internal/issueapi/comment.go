package issueapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"regexp"
	"strings"
	"unicode"
)

func (s *Service) comment(ctx context.Context, data Object, p auth.Principal) (Object, error) {
	return s.commentWithVia(ctx, data, p, via(p))
}
func (s *Service) commentWithVia(ctx context.Context, data Object, p auth.Principal, actor string) (Object, error) {
	repo, n, e := s.target(ctx, data)
	if e != nil {
		return nil, e
	}
	id, action := fleet.Text(data["id"]), fleet.Text(data["action"])
	if !uuid.MatchString(id) || !oneOf(action, "reply", "stop", "retry", "resume", "priority", "approve-deletions", "approve-permissions", "approve-final", "run", "record-check") {
		return nil, fail(400, "送信IDまたは操作が不正です。")
	}
	if action == "record-check" {
		unlock := lock(&s.locks, "operation:"+id)
		defer unlock()
		issueUnlock := lock(&s.locks, fmt.Sprintf("issue:%s#%d", repo, n))
		defer issueUnlock()
		return s.recordExecutionCheck(ctx, repo, n, id, data, p)
	}
	if action == "run" {
		return nil, fail(409, "この画面からは実行しません。依頼は相談から登録してください。")
	}
	if action == "approve-final" {
		if p.Kind != auth.Screen && (p.Kind != auth.PC || !p.Delegated) {
			return nil, fail(403, "最終承認は画面か運用代行トークンから行ってください。")
		}
		return s.approveFinal(ctx, repo, n, data, p)
	}
	if action == "approve-permissions" {
		if p.Kind != auth.Screen && (p.Kind != auth.PC || !p.Delegated) {
			return nil, fail(403, "権限変更の承認は画面か運用代行トークンから行ってください。")
		}
		return s.approvePermissions(ctx, repo, n, data, p)
	}
	if action == "approve-deletions" {
		if p.Kind != auth.Screen && (p.Kind != auth.PC || !p.Delegated) {
			return nil, fail(403, "削除の承認は画面か運用代行トークンから行ってください。")
		}
		return s.approveDeletions(ctx, repo, n, data, p)
	}
	text, reason, e := s.validateCommentRequest(data, action, p)
	if e != nil {
		return nil, e
	}
	if data["images"] != nil && len(fleet.Items(data["images"])) > 0 {
		return s.imageComment(ctx, repo, n, data, p)
	}
	unlock := lock(&s.locks, "operation:"+id)
	defer unlock()
	issueUnlock := lock(&s.locks, fmt.Sprintf("issue:%s#%d", repo, n))
	defer issueUnlock()
	payload := commentPayload(repo, n, action, text, actor, reason, data)
	saved, e := s.claimCommentAction(ctx, id, repo, n, payload)
	if e != nil {
		return nil, e
	}
	save := func() error { return s.save(ctx, "actions", id, saved) }
	if saved["applied"] == true {
		return Object{"id": fleet.ObjectOrEmpty(saved["comment"])["id"], "job": saved["job"]}, nil
	}
	var usageHeld bool
	detail, e := s.detail(ctx, repo, n, false, false, &usageHeld)
	if e != nil {
		return nil, e
	}
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	limitHeld, e := s.commentAllowed(detail, job, action, data, usageHeld, saved["attempted"] == true)
	if e != nil {
		return nil, e
	}
	instruction, contextText, e := s.commentInstruction(ctx, saved, text)
	if e != nil {
		return nil, e
	}
	if action == "reply" && saved["comment"] == nil {
		if e = s.postReplyComment(ctx, repo, n, id, instruction+contextText, saved, save); e != nil {
			return nil, e
		}
	}
	if e = s.controlCommentJob(ctx, repo, n, job, commentControl{id: id, action: action, actor: actor, instruction: instruction, contextText: contextText, reason: reason, value: fleet.Text(data["value"]), overLimitStatement: fleet.Text(data["overLimitStatement"]), instructionAt: fleet.Text(data["instructionAt"]), fromBase: data["fromBase"] == true, retest: data["retest"] == true, autoRetest: data["autoRetest"] == true, implementationLimit: data["implementationLimitRetry"] == true, limitHeld: limitHeld}, saved, save); e != nil {
		return nil, e
	}
	saved["applied"] = true
	saved["at"] = s.now()
	if e = save(); e != nil {
		return nil, e
	}
	s.emit(repo, n)
	return Object{"id": fleet.ObjectOrEmpty(saved["comment"])["id"], "job": saved["job"]}, nil
}

// validateCommentRequest は返信・操作の入力の形と送信元の組み合わせを検査し、送る本文と割り込みの理由を返す。
func (s *Service) validateCommentRequest(data Object, action string, p auth.Principal) (string, string, error) {
	if e := validateRetryOptions(data, action); e != nil {
		return "", "", e
	}
	text, e := commentRequestText(data, action, p)
	if e != nil {
		return "", "", e
	}
	reason, e := commentRequestReason(data, action, p)
	if e != nil {
		return "", "", e
	}
	if e := s.validateOverLimitApproval(data, action, p, text); e != nil {
		return "", "", e
	}
	if data["images"] != nil {
		if _, ok := data["images"].([]any); !ok {
			return "", "", fail(400, "画像は配列で指定してください。")
		}
	}
	return text, reason, nil
}

// validateRetryOptions は再試行の追加指定（fromBase・retest・autoRetest など）の型と、retry だけに使い併用しないことを検査する。
func validateRetryOptions(data Object, action string) error {
	for _, key := range []string{"fromBase", "retest", "autoRetest", "implementationLimitRetry"} {
		if value, has := data[key]; has {
			if _, ok := value.(bool); !ok {
				return fail(400, key+"は真偽値で指定してください。")
			}
		}
	}
	if data["fromBase"] == true && action != "retry" || data["retest"] == true && action != "retry" || data["autoRetest"] == true && action != "retry" {
		return fail(400, "再試行の追加指定はretryだけに使えます。")
	}
	if data["fromBase"] == true && (data["retest"] == true || data["autoRetest"] == true) || data["retest"] == true && data["autoRetest"] == true {
		return fail(400, "再試行の指定を併用できません。")
	}
	return nil
}

// commentRequestText は送る本文の長さと、専用の操作でしか送れない印・コマンドが含まれていないことを検査して返す。
func commentRequestText(data Object, action string, p auth.Principal) (string, error) {
	text, ok := data["text"].(string)
	if action == "priority" {
		text = ""
		ok = true
	}
	if !ok || action == "reply" && strings.TrimSpace(text) == "" {
		return "", fail(400, "返信を入力してください。")
	}
	if contract.UTF16Length(text) > 20000 || oneOf(action, "reply", "retry", "resume") && contract.UTF16Length(text) > 8000 {
		return "", fail(400, "追加の指示は8000文字以内で入力してください。")
	}
	if strings.Contains(text, "<!-- issue-desk:handover") {
		return "", fail(400, "担当の引き継ぎは専用の操作から行ってください。")
	}
	if action == "reply" && regexp.MustCompile(`(?i)^\s*/codex\s+(retry|approve-deletions)\b`).MatchString(text) {
		return "", fail(400, "再試行と削除承認は専用の操作から行ってください。")
	}
	if action == "reply" && p.Kind != auth.Screen {
		if command := regexp.MustCompile(`(?i)^\s*/codex\s+(\S+)`).FindStringSubmatch(text); len(command) > 0 && !oneOf(strings.ToLower(command[1]), "message", "ask", "status") {
			return "", fail(403, "返信から停止・再開・優先度などの操作は送れません。")
		}
	}
	return text, nil
}

// commentRequestReason は優先度の値と割り込みの理由を検査し、記録する理由を返す。理由を取らない操作に理由があれば断る。
func commentRequestReason(data Object, action string, p auth.Principal) (string, error) {
	if action != "priority" {
		if p.Kind != auth.Screen && oneOf(action, "stop", "resume") && data["reason"] != nil {
			return "", fail(400, "この操作には理由を指定しません。")
		}
		return "", nil
	}
	if !oneOf(fleet.Text(data["value"]), "high", "normal", "auto") {
		return "", fail(400, "優先度が不正です。")
	}
	if data["value"] != "high" {
		if p.Kind != auth.Screen && data["reason"] != nil {
			return "", fail(400, "理由は割り込みだけに指定してください。")
		}
		return "", nil
	}
	if p.Kind == auth.Screen {
		return "受付画面の操作", nil
	}
	reason := strings.TrimSpace(fleet.Text(data["reason"]))
	if len([]rune(reason)) < 1 || len([]rune(reason)) > 500 {
		return "", fail(400, "割り込みには1〜500文字の理由が必要です。")
	}
	return reason, nil
}

// validateOverLimitApproval は上限超過の承認発言とその日時、期限付き上限許可の指定が運用代行トークンからの正しい組み合わせかを検査する。
func (s *Service) validateOverLimitApproval(data Object, action string, p auth.Principal, text string) error {
	if data["overLimitStatement"] != nil && (!oneOf(action, "retry", "reply") || p.Kind != auth.PC || !p.Delegated || !jobs.MeaningfulApproval(fleet.Text(data["overLimitStatement"]))) {
		return fail(400, "利用者の具体的な承認発言を運用代行トークンから送ってください。")
	}
	// 上限超過の承認発言には、利用者がチャットで指示した日時を必須にする（発言の無い指定は断る）。
	if data["overLimitStatement"] != nil {
		if _, ok := contract.InstructionAt(data["instructionAt"], s.o.Now()); !ok {
			return fail(400, contract.InstructionAtProblem)
		}
	} else if data["instructionAt"] != nil {
		return fail(400, "instructionAtは利用者の承認発言（overLimitStatement）と一緒に指定してください。")
	}
	if data["implementationLimitRetry"] == true && (p.Kind != auth.PC || !p.Delegated || !oneOf(action, "retry", "reply") || strings.TrimSpace(text) == "" || data["overLimitStatement"] != nil) {
		return fail(400, "期限付き上限許可の指定が不正です。")
	}
	return nil
}

// commentPayload は送信IDの使い回しを照合するための操作内容を組み立てる。
func commentPayload(repo string, n int64, action, text, actor, reason string, data Object) Object {
	payload := Object{"repo": repo, "number": n, "action": action, "text": text, "value": data["value"], "via": actor, "reason": reason}
	for _, k := range []string{"fromBase", "retest", "autoRetest", "overLimitStatement", "instructionAt", "implementationLimitRetry"} {
		if data[k] != nil {
			payload[k] = data[k]
		}
	}
	return payload
}

// claimCommentAction は送信IDの保存済みの記録を読み、同じ操作の続きか未使用なら記録を返す。別の操作で使用済みなら断る。
func (s *Service) claimCommentAction(ctx context.Context, id, repo string, n int64, payload Object) (Object, error) {
	saved, e := s.read(ctx, "actions", id)
	if e != nil {
		return nil, e
	}
	if saved != nil && !samePayload(fleet.ObjectOrEmpty(saved["payload"]), payload) {
		if saved["attempted"] == true || saved["comment"] != nil || fleet.ObjectOrEmpty(saved["payload"])["repo"] != repo || fleet.ObjectOrEmpty(saved["payload"])["number"] != float64(n) {
			return nil, fail(409, "送信IDは別の操作で使用済みです。")
		}
		saved = nil
	}
	if saved == nil {
		saved = Object{"payload": payload}
	}
	return saved, nil
}

// commentAllowed は依頼の状態から操作を受け付けるかを判定し、上限で止まった依頼への承認付き操作かを返す。
func (s *Service) commentAllowed(detail Object, job *jobs.Job, action string, data Object, usageHeld, attempted bool) (bool, error) {
	if !attempted {
		refusalAction := action
		if action == "retry" && detail["status"] == "review_held" {
			refusalAction = "review"
		}
		if reason := refusal(detail, refusalAction); reason != "" {
			return false, fail(409, reason)
		}
	}
	if action == "reply" && usageHeld && fleet.Text(data["overLimitStatement"]) == "" {
		return false, fail(409, "累計使用量の上限を超えています。承認付き再試行を使ってください。")
	}
	limitHeld := fleet.Text(data["overLimitStatement"]) != "" && (jobs.RunLimitHeld(job, s.o.RunLimit) || usageHeld)
	if fleet.Text(data["overLimitStatement"]) != "" && action == "reply" && !limitHeld && (job == nil || job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "review_held") {
		return false, fail(409, "承認付き返信はレビュー保留、実装回数または使用量の上限で止まった依頼の指示だけに使えます。")
	}
	return limitHeld, nil
}

// commentInstruction は本文に参照した依頼の解決結果を足した指示と、収まらない分の補足を返す。一度決めた結果は記録から再利用する。
func (s *Service) commentInstruction(ctx context.Context, saved Object, text string) (string, string, error) {
	instruction, contextText := text, ""
	if saved["instruction"] != nil {
		instruction = fleet.Text(saved["instruction"])
		contextText = fleet.Text(saved["context"])
	} else {
		refs, e := s.references(ctx, text)
		if e != nil {
			return "", "", e
		}
		room := 8000 - contract.UTF16Length(text)
		if contract.UTF16Length(refs) <= room {
			instruction += refs
		} else {
			contextText = refs
		}
		saved["instruction"] = instruction
		saved["context"] = contextText
	}
	return instruction, contextText, nil
}

// postReplyComment は返信をGiteaのコメントとして1回だけ投稿する。既に同じ本文があればそれを記録し、結果を照合できない再投稿は断る。
func (s *Service) postReplyComment(ctx context.Context, repo string, n int64, id, text string, saved Object, save func() error) error {
	body := text + "\n\n<!-- issue-desk-action:" + id + " -->"
	saved["body"] = body
	comments, e := s.o.Gitea.Comments(ctx, repo, n)
	if e != nil {
		return e
	}
	for _, comment := range comments {
		if s.o.Own(fleet.ObjectOrEmpty(comment["user"])) && comment["body"] == body {
			saved["comment"] = Object{"id": comment["id"]}
			break
		}
	}
	if saved["comment"] == nil {
		if saved["attempted"] == true {
			return fail(409, "前回の返信結果を照合できません。重複防止のため再投稿しません。")
		}
		saved["attempted"] = true
		if e = save(); e != nil {
			return e
		}
		comment, e := s.o.Gitea.Comment(ctx, repo, n, body)
		if e != nil {
			if explicitRejection(e) {
				saved["attempted"] = false
				_ = save()
			}
			return e
		}
		saved["comment"] = Object{"id": comment["id"]}
		saved["at"] = s.now()
		if e = save(); e != nil {
			return e
		}
	}
	return nil
}

// commentControl はジョブへ渡す操作の内容。
type commentControl struct {
	id, action, actor, instruction, contextText, reason string
	value, overLimitStatement, instructionAt            string
	fromBase, retest, autoRetest, implementationLimit   bool
	limitHeld                                           bool
}

// controlCommentJob は操作をジョブの状態遷移として送り、変わったジョブを記録する。返信以外で操作できるジョブが無ければ断る。
func (s *Service) controlCommentJob(ctx context.Context, repo string, n int64, job *jobs.Job, c commentControl, saved Object, save func() error) error {
	action := c.action
	if job != nil && !oneOf(job.Status, "merged", "closed") {
		controlAction := action
		if action == "reply" {
			controlAction = "instruction"
			if c.limitHeld || job.Status == "held" && job.HoldReason != nil && *job.HoldReason == "review_held" {
				controlAction = "retry"
			}
		}
		desk, _, _ := s.o.Numbers.Number(ctx, repo, n)
		input := jobs.ControlInput{ID: c.id, Action: controlAction, By: c.actor, Automatic: c.actor == "有事の期限切れの対処" || c.actor == "止まった依頼の自動復旧", Text: c.instruction, Context: c.contextText, Value: c.value, Reason: c.reason, FromBase: c.fromBase, Retest: c.retest, AutoRetest: c.autoRetest, OverLimitStatement: c.overLimitStatement, OverLimitInstructionAt: c.instructionAt, ImplementationLimitRetry: c.implementationLimit, DeskNumber: desk}
		if action != "reply" {
			saved["attempted"] = true
			if e := save(); e != nil {
				return e
			}
		}
		changed, e := s.o.Jobs.Control(ctx, job.ID, input, jobs.ControlOptions{RunLimit: s.o.RunLimit, UsageLimit: s.o.UsageLimit, UsageWeights: s.o.UsageWeights})
		if e != nil {
			if action != "reply" {
				var problem *contract.Problem
				if errors.As(e, &problem) && problem.Status >= 400 && problem.Status < 500 {
					saved["attempted"] = false
					_ = save()
				}
			}
			return e
		}
		saved["job"] = changed.ID
	} else if action != "reply" {
		return fail(409, "この依頼に操作できるジョブがありません。")
	}
	return nil
}
func samePayload(a, b Object) bool {
	aa, bb := clone(a), clone(b)
	if _, has := a["via"]; !has {
		delete(bb, "via")
		delete(bb, "reason")
	}
	x, _ := json.Marshal(aa)
	y, _ := json.Marshal(bb)
	return string(x) == string(y)
}
func (s *Service) references(ctx context.Context, text string) (string, error) {
	ids := []int64{}
	seen := map[int64]bool{}
	pattern := regexp.MustCompile(`(?:共通番号\s*#?|#)([1-9][0-9]{0,8})`)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") || regexp.MustCompile(`^\s*- 共通番号[0-9、]+: `).MatchString(line) {
			continue
		}
		for _, m := range pattern.FindAllStringSubmatchIndex(line, -1) {
			if m[1] < len(line) {
				next := line[m[1]]
				if next >= '0' && next <= '9' || next >= 'A' && next <= 'Z' || next >= 'a' && next <= 'z' || next == '_' {
					continue
				}
			}
			before := line[:m[0]]
			if strings.HasPrefix(line[m[0]:], "#") && len(before) > 0 {
				last := []rune(before)[len([]rune(before))-1]
				if unicode.IsLetter(last) || unicode.IsNumber(last) || strings.ContainsRune("_/#&.-", last) || regexp.MustCompile(`(?i)(?:PR|Gitea(?:番号)?)\s*$`).MatchString(before) {
					continue
				}
			}
			n, _ := number(line[m[2]:m[3]])
			if !seen[n] {
				ids = append(ids, n)
				seen[n] = true
			}
		}
	}
	if len(ids) == 0 {
		return "", nil
	}
	lines := []string{"\n\n## 参照した依頼（受付が解決）"}
	for _, id := range ids {
		issue, found, e := s.o.Numbers.Issue(ctx, id)
		if e != nil {
			return "", e
		}
		if !found {
			lines = append(lines, fmt.Sprintf("- 共通番号%d: 未確認（対応する依頼がありません）", id))
			continue
		}
		if e = s.o.Project(issue.Project); e != nil {
			return "", e
		}
		detail, e := s.Detail(ctx, issue.Project, issue.Number, false, true)
		if e != nil {
			return "", e
		}
		lines = append(lines, fmt.Sprintf("- 共通番号%d: %s / %s / %s", id, fleet.Text(detail["title"]), fleet.Text(detail["status"]), s.link(issue.Project, "issues", issue.Number)))
	}
	return strings.Join(lines, "\n"), nil
}
