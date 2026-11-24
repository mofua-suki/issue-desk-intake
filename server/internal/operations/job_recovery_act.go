package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
)

func recoveryFollowup(j *jobs.Job) bool {
	return j.Kind == "followup" && j.Status == "held" && j.HoldReason != nil && *j.HoldReason == "question"
}

func recoveryFollowupHandoffReport(j *jobs.Job) string {
	if recoveryFollowupWithoutQuestion(j) {
		return recoveryFollowupReport(j)
	}
	b, _ := json.Marshal(map[string]any{"questions": j.Result["questions"], "summary": j.Result["summary"], "incomplete": j.Result["incomplete"]})
	return usage.Redact(string(b))
}

// RecoveryActions is wired by the caller to the existing ops-watch judge and issue API.
type RecoveryActions struct {
	Recovery      *JobRecovery
	Judge         func(context.Context, []any) (map[string]any, error)
	ReadTarget    func(context.Context, string, int64) (map[string]any, error)
	Send          func(context.Context, string, int64, string, string, string) error
	RefreshLimits func(context.Context) error
	records       func(context.Context) ([]RecoveryRecord, error)
	recheck       func(context.Context, RecoveryCandidate) (bool, error)
}

type RecoveryResult string

const (
	Acted        RecoveryResult = "acted"
	Skipped      RecoveryResult = "skipped"
	NeedsUser    RecoveryResult = "needs_user"
	Abandoned    RecoveryResult = "abandoned"
	SendRejected RecoveryResult = "send_rejected"
)

var errRecoveryTargetMismatch = errors.New("target mismatch")

// Act reserves before calling external services, so uncertain outcomes are never resent.
func (a *RecoveryActions) Act(ctx context.Context, c RecoveryCandidate) (RecoveryResult, error) {
	if a.Recovery == nil {
		return Skipped, fmt.Errorf("recovery unavailable")
	}
	decision, id, err := a.Recovery.Reserve(ctx, c)
	if err != nil {
		return Skipped, err
	}
	if decision == "abandon" {
		return Abandoned, nil
	}
	if decision != "proceed" {
		return Skipped, nil
	}
	finish := func(state string, result RecoveryResult, cause error) (RecoveryResult, error) {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if e := a.Recovery.Finish(saveCtx, c, id, state); e != nil {
			return result, e
		}
		return result, cause
	}
	followup := recoveryFollowup(&c.Job)
	handoffFollowup := func(reason string) (RecoveryResult, error) {
		no := false
		reason = "受付担当が元の指摘、依頼本文・コメントと実装結果の不足資料を照合してください。" + reason
		report := recoveryFollowupHandoffReport(&c.Job)
		if e := a.recordRecoveryJudgment(ctx, c, id, recoveryJudgment{requiresUser: &no, handoff: true, action: "notify", reason: reason, evidence: report, text: report}); e != nil {
			return finish("skipped", Skipped, e)
		}
		return finish("skipped", NeedsUser, nil)
	}
	detail, err := a.readRecoveryTarget(ctx, c)
	if err != nil {
		if errors.Is(err, errRecoveryTargetMismatch) {
			return finish("skipped", Skipped, err)
		}
		if followup {
			return handoffFollowup("依頼資料を読み取れませんでした。")
		}
		return finish("pre_send_error", Skipped, err)
	}
	if detail["stopped"] == true || detail["state"] == "closed" {
		return finish("skipped", Skipped, nil)
	}
	snapshot, err := recoveryTargetSnapshot(detail)
	if err != nil {
		return finish("pre_send_error", Skipped, err)
	}
	key, item, err := a.recoveryJudgeItem(ctx, c, detail)
	if err != nil {
		return finish("pre_send_error", Skipped, err)
	}
	// Store a judgment before returning or sending.
	var requiresUser *bool
	handoff := false
	record := func(name, reason, evidence, text string) error {
		return a.recordRecoveryJudgment(ctx, c, id, recoveryJudgment{requiresUser: requiresUser, handoff: handoff, action: name, reason: reason, evidence: evidence, text: text})
	}
	skipUndecidable := func() (RecoveryResult, error) {
		if followup {
			return handoffFollowup("復旧の根拠を確認できませんでした。")
		}
		if e := record("none", "判断結果を確認できないため見送り", "", ""); e != nil {
			return finish("skipped", Skipped, e)
		}
		return finish("skipped", Skipped, nil)
	}
	if a.Judge == nil {
		return skipUndecidable()
	}
	result, err := a.Judge(ctx, []any{redactIncidentData(item)})
	if err != nil {
		if followup {
			return handoffFollowup("復旧判断を取得できませんでした。")
		}
		return finish("pre_send_error", Skipped, err)
	}
	action, ok := recoveryJudgedAction(result, key, c)
	if !ok {
		return skipUndecidable()
	}
	name := fleet.Text(action["action"])
	requires, _ := action["requiresUser"].(bool)
	resolved, _ := action["resolved"].(bool)
	requiresUser = &requires
	if followup && name != "reply" && !(name == "notify" && requires) {
		return handoffFollowup(fleet.Text(action["reason"]))
	}
	if name == "reply" {
		if detail["status"] != "waiting" {
			return finish("skipped", Skipped, nil)
		}
		if ok, reason := recoveryQuestionAnswer(detail, action); !ok {
			if followup {
				return handoffFollowup(reason)
			}
			handoff = true
			if err = record("notify", reason, fleet.Text(action["evidence"]), ""); err != nil {
				return finish("skipped", Skipped, err)
			}
			return finish("skipped", NeedsUser, nil)
		}
	}
	sendName, sendText := name, fleet.Text(action["text"])
	if name == "repair" {
		var reason string
		sendText, reason = recoveryRepairInstruction(c, action)
		if reason != "" {
			handoff = true
			if err = record("notify", reason, fleet.Text(action["evidence"]), ""); err != nil {
				return finish("skipped", Skipped, err)
			}
			return finish("skipped", NeedsUser, nil)
		}
		sendName = "retry"
	}
	if err = record(name, fleet.Text(action["reason"]), fleet.Text(action["evidence"]), sendText); err != nil {
		return finish("skipped", Skipped, err)
	}
	if name == "none" {
		return finish("skipped", Skipped, nil)
	}
	if name == "notify" {
		return finish("skipped", NeedsUser, nil)
	}
	fresh, err := a.readRecoveryTarget(ctx, c)
	if err != nil {
		if errors.Is(err, errRecoveryTargetMismatch) {
			return finish("skipped", Skipped, err)
		}
		if followup {
			return handoffFollowup("送信前に依頼資料を再読取できませんでした。")
		}
		return finish("pre_send_error", Skipped, err)
	}
	if fresh["stopped"] == true || fresh["state"] == "closed" {
		return finish("skipped", Skipped, nil)
	}
	if !recoveryTargetUnchanged(snapshot, fresh) {
		if recoveryTargetDecisionUnchanged(snapshot, fresh) {
			return finish("pre_send_error", Skipped, nil)
		}
		if followup {
			return handoffFollowup("判断中に依頼本文・受入条件・事前承認の前提が変わりました。")
		}
		handoff = true
		if err = record("notify", "判断中に依頼本文・受入条件・事前承認の前提が変わりました。再照合が必要です。", "", ""); err != nil {
			return finish("skipped", Skipped, err)
		}
		return finish("skipped", NeedsUser, nil)
	}
	if !recoveryStillSendable(name, action, detail, fresh, resolved) {
		return finish("skipped", Skipped, nil)
	}
	return finish(a.sendRecovery(ctx, c, id, sendName, sendText))
}

// recoveryJudgeItem は前回の原因への判断を依頼の状態に足し、判断AIへ渡す項目とその鍵を作る。
func (a *RecoveryActions) recoveryJudgeItem(ctx context.Context, c RecoveryCandidate, detail map[string]any) (string, map[string]any, error) {
	records := a.records
	if records == nil {
		records = a.Recovery.records
	}
	rows, err := records(ctx)
	if err != nil {
		return "", nil, err
	}
	var previous RecoveryCause
	var previousHash string
	for _, row := range rows {
		if row.Repo == c.Job.Repo && row.Issue == c.Job.Issue {
			current := row.Causes[c.Cause]
			previousHash = current.PreviousCause
			previous = row.Causes[previousHash]
		}
	}
	detail["cause"] = c.Cause
	// The hash identifies a cause; the job result supplies the actual failure,
	// review findings, or questions needed by the judge.
	detail["jobResult"] = c.Job.Result
	detail["followupWithoutQuestion"] = recoveryFollowupWithoutQuestion(&c.Job)
	detail["followupQuestion"] = recoveryFollowup(&c.Job) && !recoveryFollowupWithoutQuestion(&c.Job)
	detail["previousCause"] = previousHash
	detail["previousAction"] = previous.Action
	detail["previousActionText"] = previous.ActionText
	detail["previousReason"] = previous.Reason
	detail["previousEvidence"] = previous.Evidence
	key := fmt.Sprintf("job_recovery:%s#%d", c.Job.Repo, c.Job.Issue)
	item := map[string]any{"key": key, "kind": "job_recovery", "project": c.Job.Repo, "number": c.Job.Issue, "summary": map[string]any{"title": c.Job.Title}, "detail": detail}
	return key, item, nil
}

// recoveryJudgedAction は判断AIの結果が対象の依頼だけを指す決まった形の操作1件かを検査し、その操作を返す。
func recoveryJudgedAction(result map[string]any, key string, c RecoveryCandidate) (map[string]any, bool) {
	actions, ok := result["actions"].([]any)
	if len(result) != 1 || !ok || len(actions) != 1 {
		return nil, false
	}
	action := fleet.Object(actions[0])
	name := fleet.Text(action["action"])
	itemKey, itemOK := action["item"].(string)
	_, textOK := action["text"].(string)
	_, evidenceOK := action["evidence"].(string)
	requires, uok := action["requiresUser"].(bool)
	_, rok := action["resolved"].(bool)
	if len(action) != 7 || contract.OnlyKeys(action, "action", "item", "reason", "text", "evidence", "resolved", "requiresUser") != nil || (name != "retry" && name != "reply" && name != "repair" && name != "notify" && name != "none") || !itemOK || itemKey != key || !incidentText(action["reason"], 1000) || !textOK || !evidenceOK || !uok || !rok || (requires && name != "notify") || (name != "none" && !incidentText(action["text"], 20000)) {
		return nil, false
	}
	for _, m := range incidentRefs.FindAllStringSubmatch(fleet.Text(action["text"]), -1) {
		repo := m[1]
		if repo == "" {
			repo = c.Job.Repo
		}
		n, e := strconv.ParseInt(m[2], 10, 64)
		if e != nil || repo != c.Job.Repo || n != c.Job.Issue {
			return nil, false
		}
	}
	return action, true
}

// recoveryStillSendable は送る直前の依頼の状態が、判断した操作をまだ受け付けるかを判定する。
func recoveryStillSendable(name string, action, detail, fresh map[string]any, resolved bool) bool {
	if name == "repair" && fresh["status"] != "failed" && fresh["status"] != "review_held" {
		return false
	}
	if name == "retry" && ((fresh["status"] != "failed" && fresh["status"] != "review_held") || !resolved || !incidentText(action["evidence"], 4000)) {
		return false
	}
	if name == "reply" {
		before, _ := json.Marshal(detail["comments"])
		after, _ := json.Marshal(fresh["comments"])
		if fresh["status"] != "waiting" || !incidentText(action["evidence"], 4000) || string(before) != string(after) {
			return false
		}
	}
	return true
}

// readRecoveryTarget は対象の依頼を読み、別の依頼を指していれば errRecoveryTargetMismatch を返す。
func (a *RecoveryActions) readRecoveryTarget(ctx context.Context, c RecoveryCandidate) (map[string]any, error) {
	if a.ReadTarget == nil {
		return nil, fmt.Errorf("target unavailable")
	}
	d, e := a.ReadTarget(ctx, c.Job.Repo, c.Job.Issue)
	if e != nil {
		return nil, e
	}
	if d == nil || d["project"] != c.Job.Repo || int64(fleet.Number(d["number"])) != c.Job.Issue {
		return nil, errRecoveryTargetMismatch
	}
	return d, nil
}

// recoveryJudgment は予約へ残す判断の内容を持つ。
type recoveryJudgment struct {
	requiresUser                   *bool
	handoff                        bool
	action, reason, evidence, text string
}

// recordRecoveryJudgment は返す・送る前に、今回の予約へ判断を記録する。
// Older ledger entries may omit these optional fields, so only the current
// reservation is changed.
func (a *RecoveryActions) recordRecoveryJudgment(ctx context.Context, c RecoveryCandidate, id string, j recoveryJudgment) error {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return a.Recovery.ledger(saveCtx, func(rows *[]RecoveryRecord) error {
		for i := range *rows {
			row := &(*rows)[i]
			if row.Repo == c.Job.Repo && row.Issue == c.Job.Issue {
				v, ok := row.Causes[c.Cause]
				if !ok || v.ReservationID != id || v.State != "reserved" {
					return fmt.Errorf("reservation mismatch")
				}
				v.RequiresUser = j.requiresUser
				v.Handoff = j.handoff
				v.Action, v.Reason, v.Evidence, v.ActionText = j.action, usage.Redact(j.reason), usage.Redact(j.evidence), usage.Redact(j.text)
				row.Causes[c.Cause] = v
				return nil
			}
		}
		return fmt.Errorf("reservation missing")
	})
}

// sendRecovery は送る直前に上限と対象を確かめ直してから操作を送り、予約の終わりの状態と結果を返す。
func (a *RecoveryActions) sendRecovery(ctx context.Context, c RecoveryCandidate, id, sendName, sendText string) (string, RecoveryResult, error) {
	if a.RefreshLimits != nil {
		if err := a.RefreshLimits(ctx); err != nil {
			return "pre_send_error", Skipped, err
		}
	}
	recheck := a.recheck
	if recheck == nil {
		recheck = a.Recovery.Recheck
	}
	valid, err := recheck(ctx, c)
	if err != nil {
		return "pre_send_error", Skipped, err
	}
	if !valid {
		return "skipped", Skipped, nil
	}
	if a.Send == nil {
		return "send_unknown", Skipped, fmt.Errorf("send unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "pre_send_error", Skipped, err
	}
	err = a.Send(ctx, c.Job.Repo, c.Job.Issue, id, sendName, sendText)
	if err != nil {
		var p *contract.Problem
		if errors.As(err, &p) && (p.Status == 403 || p.Status == 409) {
			return "send_rejected", SendRejected, nil
		}
		return "send_unknown", Skipped, err
	}
	return "sent", Acted, nil
}
