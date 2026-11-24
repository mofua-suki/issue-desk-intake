package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

type ControlInput struct {
	Automatic                                                      bool `json:",omitempty"`
	ID, Action, By, Text, Context, Reason, Head, Value             string
	FromBase, Retest, AutoRetest, ImplementationLimitRetry, IfIdle bool
	OverLimitStatement                                             string
	// OverLimitInstructionAt is the time the user gave OverLimitStatement in the chat.
	// omitempty keeps the receipt fingerprint of inputs without it unchanged.
	OverLimitInstructionAt string `json:",omitempty"`
	DeskNumber             int64
	Approval               map[string]any
}
type ControlOptions struct {
	RunLimit     int64
	UsageLimit   float64
	UsageWeights map[string]any
}

func controlProblem(status int, message string) error {
	return contract.NewProblem(status, message, "")
}

// Control applies a server-authenticated operation under the job row lock. The
// durable event receipt makes an HTTP replay independent of later job changes.
func (s *Store) Control(ctx context.Context, id int64, input ControlInput, options ControlOptions) (*Job, error) {
	var result *Job
	if !regexpUUID.MatchString(input.ID) {
		return nil, controlProblem(400, "操作IDが不正です。")
	}
	payload, _ := json.Marshal(input)
	sum := sha256.Sum256(payload)
	fingerprint := hex.EncodeToString(sum[:])
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "issue-control:"+input.ID); e != nil {
			return e
		}
		job, e := readJob(ctx, tx, id, true)
		if e != nil {
			return e
		}
		replayed, e := controlReceiptReplayed(ctx, tx, id, input.ID, fingerprint)
		if e != nil {
			return e
		}
		if replayed {
			result = job
			return nil
		}
		if job.View == nil {
			job.View = map[string]any{}
		}
		if fleet.Object(job.View["liveCheck"])["status"] == "pending" {
			return controlProblem(409, "実地確認待ちの操作はできません。")
		}
		if input.Automatic {
			if e = refuseAutomaticControl(ctx, tx, job); e != nil {
				return e
			}
		}
		from := job.Status
		leaseHost, epoch := job.LeaseHost, job.LeaseEpoch
		phase, hasPhase := job.View["phase"]
		sendPayload := map[string]any{"jobId": job.ID, "epoch": job.LeaseEpoch, "by": input.By}
		generation := job.Generation
		sendKind, e := s.applyControl(ctx, tx, job, input, options, generation, sendPayload)
		if e != nil {
			return e
		}
		if sendKind != "" {
			if job.LeaseHost == nil || job.LeaseExpiresAt == nil {
				return controlProblem(409, "有効な持ち主を確認できません。")
			}
			raw, _ := json.Marshal(sendPayload)
			if _, e = tx.ExecContext(ctx, `INSERT INTO job_commands(id,repo,issue,kind,payload,target_host,created_at) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7)`, input.ID, job.Repo, job.Issue, sendKind, string(raw), *job.LeaseHost, s.options.Now()); e != nil {
				return e
			}
		}
		raw, e := json.Marshal(job.View)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE jobs SET status=$2,hold_reason=$3,generation=$4,priority=$5,view=$6::jsonb,lease_expires_at=$7,updated_at=$8 WHERE id=$1`, job.ID, job.Status, job.HoldReason, job.Generation, job.Priority, string(raw), job.LeaseExpiresAt, s.options.Now()); e != nil {
			return e
		}
		event := map[string]any{"action": input.Action, "from": from, "to": job.Status, "leaseHost": leaseHost, "epoch": epoch, "by": input.By, "id": input.ID, "generation": job.Generation, "instruction": strings.TrimSpace(input.Text+input.Context) != ""}
		if hasPhase {
			event["phase"] = phase
		}
		if e = addEvent(ctx, tx, id, input.By, "control", event, s.options.Now()); e != nil {
			return e
		}
		if e = addEvent(ctx, tx, id, input.By, "control_receipt", map[string]any{"id": input.ID, "fingerprint": fingerprint}, s.options.Now()); e != nil {
			return e
		}
		result, e = readJob(ctx, tx, id, false)
		return e
	})
	return result, e
}

// controlReceiptReplayed は同じ送信IDの受領記録があれば、同じ依頼・同じ内容の再送かを照合する。再送なら true を返し、別の操作なら断る。
func controlReceiptReplayed(ctx context.Context, tx *sql.Tx, id int64, actionID, fingerprint string) (bool, error) {
	var previous string
	var previousID int64
	e := tx.QueryRowContext(ctx, `SELECT job_id,data->>'fingerprint' FROM job_events WHERE kind='control_receipt' AND data->>'id'=$1 ORDER BY seq LIMIT 1`, actionID).Scan(&previousID, &previous)
	if e == nil {
		if previousID != id || previous != fingerprint {
			return false, controlProblem(409, "送信IDは別の操作で使用済みです。")
		}
		return true, nil
	}
	if e != sql.ErrNoRows {
		return false, e
	}
	return false, nil
}

// refuseAutomaticControl は明示の停止や停止の指示がある依頼への自動の操作を断る。
func refuseAutomaticControl(ctx context.Context, tx *sql.Tx, job *Job) error {
	if job.Status == "stopped" || job.View["stopRequested"] == true {
		return controlProblem(409, "明示された停止は自動で解除できません。")
	}
	if QuestionDataUnconfirmed(job) {
		return controlProblem(409, "質問の記録が欠けているため自動で操作できません。")
	}
	if job.LeaseHost != nil {
		var stopped bool
		if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_commands WHERE target_host=$1 AND repo=$2 AND issue=$3 AND kind='stop' AND status IN('queued','acked'))`, *job.LeaseHost, job.Repo, job.Issue).Scan(&stopped); e != nil {
			return e
		}
		if stopped {
			return controlProblem(409, "停止の指示がある依頼は自動で操作できません。")
		}
	}
	return nil
}

// applyControl は操作ごとに依頼の状態を遷移させ、実行中のPCへ送る指示の種類を返す（送らないときは空）。
func (s *Store) applyControl(ctx context.Context, tx *sql.Tx, job *Job, input ControlInput, options ControlOptions, generation int64, sendPayload map[string]any) (string, error) {
	switch input.Action {
	case "stop":
		return controlStop(job)
	case "resume":
		return "", s.controlResume(ctx, tx, job, input, generation)
	case "retry":
		return "", s.controlRetry(ctx, tx, job, input, options)
	case "instruction":
		return s.controlInstruction(ctx, tx, job, input, options, generation, sendPayload)
	case "priority":
		return "", s.controlPriority(job, input)
	case "close":
		return controlClose(ctx, tx, job, input, sendPayload)
	case "approve-deletions":
		return "", s.controlApproveDeletions(job, input)
	default:
		return "", controlProblem(400, "ジョブの操作が不正です。")
	}
}

// controlStop は実行中なら実行中のPCへ停止を送り、待ち・保留・失敗なら停止にする。
func controlStop(job *Job) (string, error) {
	if job.Status == "running" {
		return "stop", nil
	}
	if oneOf(job.Status, "queued", "held", "failed") {
		job.Status = "stopped"
		job.HoldReason = nil
		return "", nil
	}
	return "", controlProblem(409, "この状態のジョブは停止できません。")
}

// controlResume は停止・保留の依頼を待ちへ戻し、指示があれば足す。
func (s *Store) controlResume(ctx context.Context, tx *sql.Tx, job *Job, input ControlInput, generation int64) error {
	if job.Kind == "investigation" || !oneOf(job.Status, "stopped", "held") {
		return controlProblem(409, "再開できるジョブではありません。")
	}
	job.Status = "queued"
	job.HoldReason = nil
	markControlContinuation(ctx, tx, job, generation, s.options.Now())
	if strings.TrimSpace(input.Text+input.Context) != "" {
		appendControlInstruction(job, input, s.options.Now())
	}
	return nil
}

// controlInstruction は指示を検査して記録し、実行中なら実行中のPCへ送る内容を作り、質問で止まっていれば待ちへ戻す。
func (s *Store) controlInstruction(ctx context.Context, tx *sql.Tx, job *Job, input ControlInput, options ControlOptions, generation int64, sendPayload map[string]any) (string, error) {
	if e := validateControlInstruction(job, input); e != nil {
		return "", e
	}
	if e := s.refuseInstructionOverUsage(ctx, tx, job, options); e != nil {
		return "", e
	}
	appendControlInstruction(job, input, s.options.Now())
	if job.Status == "running" {
		sendPayload["text"] = input.Text
		if input.Context != "" {
			sendPayload["context"] = input.Context
		}
		return "instruction", nil
	}
	if job.Status == "held" && job.HoldReason != nil && *job.HoldReason == "question" {
		job.Status = "queued"
		job.HoldReason = nil
		markControlContinuation(ctx, tx, job, generation, s.options.Now())
	}
	return "", nil
}

// validateControlInstruction は指示の長さと、指示できる状態かを検査する。
func validateControlInstruction(job *Job, input ControlInput) error {
	if strings.TrimSpace(input.Text) == "" && strings.TrimSpace(input.Context) == "" || contract.UTF16Length(input.Text) > 8000 {
		return controlProblem(400, "指示は8000文字以内で入力してください。")
	}
	if oneOf(job.Status, "merged", "closed") {
		return controlProblem(409, "終了したジョブには指示できません。")
	}
	return nil
}

// refuseInstructionOverUsage は質問で止まった依頼が累計使用量の上限を超えていれば、指示ではなく承認付き再試行を求める。
func (s *Store) refuseInstructionOverUsage(ctx context.Context, tx *sql.Tx, job *Job, options ControlOptions) error {
	if options.UsageLimit > 0 && job.Status == "held" && job.HoldReason != nil && *job.HoldReason == "question" {
		reason := s.controlUsage(ctx, tx, job, options)
		if usageLimitHeld(job, reason) {
			return controlProblem(409, reason+" 承認付き再試行を使ってください。")
		}
	}
	return nil
}

// controlClose は依頼を閉じ、実行中なら停止を送り、マージの番を手放す。
func controlClose(ctx context.Context, tx *sql.Tx, job *Job, input ControlInput, sendPayload map[string]any) (string, error) {
	if job.Status == "running" && input.IfIdle {
		return "", controlProblem(409, "実行中のジョブは閉じられません。")
	}
	sendKind := ""
	if !oneOf(job.Status, "merged", "closed") {
		if job.Status == "running" {
			sendKind = "stop"
			sendPayload["reason"] = "issue_closed"
		}
		job.Status = "closed"
		job.HoldReason = nil
		job.LeaseExpiresAt = nil
		if _, e := tx.ExecContext(ctx, `UPDATE merge_turns SET holder_host=NULL,job_id=NULL,expires_at=NULL WHERE job_id=$1`, job.ID); e != nil {
			return "", e
		}
	}
	return sendKind, nil
}

// controlApproveDeletions は削除の保留対象と同じSHAの承認を記録し、依頼を待ちへ戻す。承認済みの送り直しは何もしない。
func (s *Store) controlApproveDeletions(job *Job, input ControlInput) error {
	if !shaPattern.MatchString(input.Head) {
		return controlProblem(400, "対象SHAが不正です。")
	}
	if fleet.Object(job.View["deletionApproved"])["head"] != input.Head || job.Status == "held" {
		pending := fleet.Object(job.View["deletionApproval"])
		if job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "deletions" || pending["head"] != input.Head {
			return controlProblem(409, "削除の保留対象が変わりました。")
		}
		job.View["deletionApproved"] = map[string]any{"head": input.Head, "by": input.By, "actionId": input.ID, "at": iso(s.options.Now())}
		job.Status = "queued"
		job.HoldReason = nil
	}
	return nil
}

// controlPriority は実行待ちの依頼の優先度を変え、利用者の指定かAIの判断かを記録する。
func (s *Store) controlPriority(job *Job, input ControlInput) error {
	if job.Status != "queued" {
		return controlProblem(409, "優先度は実行待ちの間だけ変更できます。")
	}
	kept := fleet.Object(job.View["priority"])
	filed := job.Priority
	if kept["source"] == "user" {
		if n, ok := count(kept["filed"]); ok && n <= 1 {
			filed = int(n)
		}
	}
	switch input.Value {
	case "high":
		if input.Reason == "" {
			return controlProblem(400, "割り込みには理由が必要です。")
		}
		job.Priority = 1
	case "normal":
		job.Priority = 0
	case "auto":
		job.Priority = filed
	default:
		return controlProblem(400, "優先度が不正です。")
	}
	if input.Value == "auto" {
		job.View["priority"] = map[string]any{"source": "ai"}
	} else {
		job.View["priority"] = map[string]any{"source": "user", "filed": filed, "reason": input.Reason, "by": input.By, "at": iso(s.options.Now())}
	}
	return nil
}
