package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"time"

	"issue-desk-intake/server/internal/contract"
)

// Optional distinguishes a missing field (retain) from JSON null (clear).
type Optional[T any] struct {
	Present bool
	Value   *T
}

func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	o.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		o.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

type Report struct {
	ControllerTest   *ControllerTest   `json:"controllerTest"`
	ControllerReport *ControllerReport `json:"controllerReport"`
	Host             Optional[string]  `json:"host"`
	Epoch            int64             `json:"epoch"`
	Status           string            `json:"status"`
	HoldReason       *string           `json:"holdReason"`
	Branch           Optional[string]  `json:"branch"`
	PR               Optional[int64]   `json:"pr"`
	HeadSHA          Optional[string]  `json:"headSha"`
	DiffFingerprint  Optional[string]  `json:"diffFingerprint"`
	View             map[string]any    `json:"view"`
	Result           map[string]any    `json:"result"`
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validateReport(report Report) error {
	if !oneOf(report.Status, "running", "held", "failed", "merged", "stopped", "queued") {
		return contract.NewProblem(400, "status の形が違います。", "")
	}
	if report.Status == "held" {
		if report.HoldReason == nil || !oneOf(*report.HoldReason, "review_held", "question", "deletions", "scope_wait", "prerequisite_missing") {
			return contract.NewProblem(400, "held には正しい holdReason が必要です。", "")
		}
	} else if report.HoldReason != nil {
		return contract.NewProblem(400, "holdReason は held のときだけ指定できます。", "")
	}
	if report.Branch.Value != nil && (*report.Branch.Value == "" || contract.UTF16Length(*report.Branch.Value) > 200) {
		return contract.NewProblem(400, "branch は200文字以内の文字列にしてください。", "")
	}
	if report.PR.Value != nil && (*report.PR.Value < 1 || *report.PR.Value > maxSafeInteger) {
		return contract.NewProblem(400, "pr は1以上の整数にしてください。", "")
	}
	if report.HeadSHA.Value != nil && !shaPattern.MatchString(*report.HeadSHA.Value) {
		return contract.NewProblem(400, "headSha は40桁の16進数（小文字）にしてください。", "")
	}
	if report.DiffFingerprint.Value != nil && (!regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(*report.DiffFingerprint.Value) || report.HeadSHA.Value == nil) {
		return contract.NewProblem(400, "diffFingerprint には正しい指紋と同じ報告の headSha が必要です。", "")
	}
	if err := checkObject(report.View, "view"); err != nil {
		return err
	}
	return checkObject(report.Result, "result")
}

// recordReportRejection は断った報告を、報告の取引が戻った後に断りの種類ごとの記録へ数える。記録に失敗してもログだけ残す。
func (s *Store) recordReportRejection(ctx context.Context, id int64, host string, report Report, problem *contract.Problem) {
	// Record after the report transaction has rolled back. Lock the job to
	// serialize concurrent identical rejections without a schema change.
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		current, err := readJob(ctx, tx, id, true)
		var readProblem *contract.Problem
		if errors.Is(err, sql.ErrNoRows) || errors.As(err, &readProblem) && readProblem.Status == 404 {
			return nil
		}
		if err != nil {
			return err
		}
		at := s.options.Now()
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT data FROM job_events WHERE job_id=$1 AND kind='report_rejected' AND data->>'code'=$2 ORDER BY seq LIMIT 1`, id, problem.Code).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		data := map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &data); err != nil {
				return err
			}
		}
		count, _ := data["count"].(float64)
		if count == 0 {
			data["firstAt"] = iso(at)
		}
		message := []rune(problem.Message)
		if len(message) > 500 {
			message = message[:500]
		}
		kind := report.Status
		if !oneOf(kind, "running", "held", "failed", "merged", "stopped", "queued") {
			kind = "invalid"
		}
		data["code"], data["status"], data["epoch"] = problem.Code, problem.Status, report.Epoch
		data["message"], data["reportKind"], data["count"], data["lastAt"] = string(message), kind, count+1, iso(at)
		// Only a live lease holder can be attributed as the cause of expiry.
		if CheckFence(current, host, report.Epoch, at, false) == nil && (!report.Host.Present || report.Host.Value != nil && *report.Host.Value == host) {
			diagnostic, _ := data["diagnostic"].(map[string]any)
			if diagnostic == nil || diagnostic["generation"] != float64(current.Generation) {
				diagnostic = map[string]any{"generation": current.Generation, "count": float64(0), "firstAt": iso(at)}
			}
			n, _ := diagnostic["count"].(float64)
			diagnostic["count"], diagnostic["lastAt"], diagnostic["message"], diagnostic["reportKind"], diagnostic["epoch"] = n+1, at.UTC().Format(time.RFC3339Nano), string(message), kind, report.Epoch
			data["diagnostic"] = diagnostic
		}
		body, err := jsonValue(data)
		if err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE job_events SET at=$3, host=$4, data=$5::jsonb WHERE job_id=$1 AND kind='report_rejected' AND data->>'code'=$2`, id, problem.Code, at, host, body)
		if err != nil {
			return err
		}
		changed, err := updated.RowsAffected()
		if err != nil || changed > 0 {
			return err
		}
		return addEvent(ctx, tx, id, host, "report_rejected", data, at)
	})
	if err != nil {
		log.Printf("job %d: failed to record report rejection: %v", id, err)
	}
}

// confirmReportReplay は lease を離れた後の報告を、受領記録と保存済みの内容に照合する。
func confirmReportReplay(ctx context.Context, tx *sql.Tx, id int64, host string, current *Job, report Report, view map[string]any) error {
	if (report.ControllerTest != nil || report.ControllerReport != nil) && current.View["executionProofFingerprint"] != executionProofFingerprint(report) {
		return contract.NewProblem(409, "保存済みの実行証拠と報告が一致しません。", "execution_proof_conflict")
	}
	var receipt bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND host=$2 AND kind='state' AND data->>'epoch'=$3 AND data->>'to'=$4)`, id, host, jsonNumber(report.Epoch), report.Status).Scan(&receipt)
	if err != nil {
		return err
	}
	if !receipt {
		return contract.NewProblem(409, "このジョブの lease はもうありません。", "lease_lost")
	}
	if err := checkStoredTransferReport(current, report.Result); err != nil {
		return err
	}
	if !sameReport(current, report, view) && !sameTransferReplay(current, report, view) {
		return contract.NewProblem(409, "このジョブの lease はもうありません。", "lease_lost")
	}
	return nil
}

// reportStateEventData は lease を離れる報告の状態の記録を、離れる前の状態と止まった原因・実装の終わり方から組み立てる。
func reportStateEventData(current *Job, report Report, leaseHost *string, generation int64, phase any, hasPhase bool) (map[string]any, error) {
	data := map[string]any{"to": report.Status, "holdReason": report.HoldReason, "epoch": report.Epoch, "leaseHost": leaseHost, "generation": generation}
	if hasPhase {
		data["phase"] = phase
	}
	if report.HoldReason != nil && *report.HoldReason == "prerequisite_missing" {
		fingerprint, err := PrerequisiteFingerprint(report.Result["prerequisiteMissing"])
		if err != nil {
			return nil, err
		}
		data["prerequisiteCause"] = fingerprint
	}
	endHold := ""
	if report.HoldReason != nil {
		endHold = *report.HoldReason
	}
	if end := ImplementationEnd(report.Status, endHold, report.Result, report.View, current.Generation); end != nil {
		data["implementationEnd"] = end
	}
	return data, nil
}

func (s *Store) Report(ctx context.Context, id int64, host string, report Report) (_ *Job, reportErr error) {
	defer func() {
		var problem *contract.Problem
		if !errors.As(reportErr, &problem) {
			return
		}
		s.recordReportRejection(ctx, id, host, report, problem)
	}()
	if report.Host.Present && (report.Host.Value == nil || *report.Host.Value != host) {
		return nil, contract.NewProblem(403, "送り手のPCと同じ host だけを送れます。", "")
	}
	if err := validateReport(report); err != nil {
		return nil, err
	}
	at := s.options.Now()
	var saved *Job
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		current, err := fenced(ctx, tx, id, host, report.Epoch, at, report.Status != "running")
		if err != nil {
			return err
		}
		leaseHost, generation := current.LeaseHost, current.Generation
		phase, hasPhase := current.View["phase"]
		view, err := reportView(current, report)
		if err != nil {
			return err
		}
		// A PR belongs to the branch it was opened from. When a lease reports a
		// different branch without naming a PR (for example after a from-base
		// retry), the previous PR must not travel with the new branch and head:
		// the report is treated as naming no PR for the fingerprint, merge proof
		// and the saved tuple.
		detachPR := detachesPR(current, report)
		if detachPR {
			report.PR = Optional[int64]{Present: true}
		}
		bindReportFingerprint(current, report, view)
		if current.Status != "running" {
			if err := confirmReportReplay(ctx, tx, id, host, current, report, view); err != nil {
				return err
			}
			saved = current
			return nil
		}

		if err := s.completeReportView(ctx, tx, current, report, view, host, at); err != nil {
			return err
		}
		body, err := jsonValue(view)
		if err != nil {
			return err
		}
		result, err := jsonValue(report.Result)
		if err != nil {
			return err
		}
		hold := report.HoldReason
		if hold != nil && *hold == "prerequisite_missing" {
			value := "question"
			hold = &value
		}
		leaving := report.Status != "running"
		phaseChanged := false
		if _, present := view["phase"]; !leaving && present {
			err = tx.QueryRowContext(ctx, `SELECT NOT (view ? 'phase') OR (view->'phase') IS DISTINCT FROM ($2::jsonb->'phase') FROM jobs WHERE id=$1`, id, body).Scan(&phaseChanged)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET status=$2,hold_reason=$3,
branch=CASE WHEN $4 THEN $5 ELSE branch END,
pr=CASE WHEN $6 THEN $7::int ELSE pr END,
head_sha=CASE WHEN $8 THEN $9 ELSE head_sha END,
view=view || $10::jsonb,result=coalesce($11::jsonb,result),
lease_expires_at=CASE WHEN $12 THEN NULL ELSE lease_expires_at END,
updated_at=$13 WHERE id=$1`, id, report.Status, hold, report.Branch.Present, report.Branch.Value, report.PR.Present, report.PR.Value, report.HeadSHA.Present, report.HeadSHA.Value, body, result, leaving, at)
		if err != nil {
			return err
		}
		if leaving {
			_, err = tx.ExecContext(ctx, "UPDATE merge_turns SET holder_host=NULL,job_id=NULL,expires_at=NULL WHERE job_id=$1", id)
			if err != nil {
				return err
			}
		}
		saved, err = readJob(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if recordFinalApprovalTransfers(saved, at) {
			body, err := jsonValue(saved.View)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb WHERE id=$1`, id, body); err != nil {
				return err
			}
		}
		if report.Status == "merged" && s.options.OnMerged != nil {
			if err = s.options.OnMerged(ctx, tx, saved, report.Result); err != nil {
				return err
			}
			saved, err = readJob(ctx, tx, id, false)
			if err != nil {
				return err
			}
		}
		return recordReportEvents(ctx, tx, current, saved, report, host, reportEventContext{leaseHost: leaseHost, generation: generation, phase: phase, hasPhase: hasPhase, phaseChanged: phaseChanged, detachPR: detachPR, leaving: leaving}, at)
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func raisesTier(view map[string]any) bool {
	history, _ := view["tierHistory"].([]any)
	for _, value := range history {
		if row, ok := value.(map[string]any); ok && row["from"] == "standard" && row["to"] == "heavy" && row["reason"] == "レビューの自動差し戻し" {
			return true
		}
	}
	return false
}

// detachesPR reports a branch change that names no PR on a job whose saved PR
// belongs to another branch.
func detachesPR(current *Job, report Report) bool {
	return report.Branch.Value != nil && !report.PR.Present && current.PR != nil && current.Branch != nil && *current.Branch != *report.Branch.Value
}

// completeReportView は報告で変わる成果の持ち主・続きの手掛かり・重さ・受入の引き継ぎを view へ足す。
func (s *Store) completeReportView(ctx context.Context, tx *sql.Tx, current *Job, report Report, view map[string]any, host string, at time.Time) error {
	if err := s.bindExecutionReceipts(ctx, tx, current, report, view, host, at); err != nil {
		return err
	}
	legacyOwner, err := legacyArtifactOwner(ctx, tx, current, host)
	if err != nil {
		return err
	}
	continuation, err := updateReportArtifact(current, host, report, legacyOwner)
	if err != nil {
		return err
	}
	if continuation != nil {
		view["continuation"] = continuation
	}
	if current.Tier != nil && *current.Tier == "standard" && current.Kind != "investigation" && raisesTier(view) {
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET tier='heavy' WHERE id=$1", current.ID); err != nil {
			return err
		}
	}
	transfer, err := s.reportTransfers(ctx, current, report.Result)
	if err != nil {
		return err
	}
	if transfer != nil {
		view["acceptanceTransferRecords"] = transfer
	}
	return nil
}

// reportEventContext は報告の保存前に読んだ、出来事の記録に要る値を持つ。
type reportEventContext struct {
	leaseHost              *string
	generation             int64
	phase                  any
	hasPhase, phaseChanged bool
	detachPR, leaving      bool
}

// recordReportEvents は保存した報告から PR・公開・段階・状態の出来事を記録する。
func recordReportEvents(ctx context.Context, tx *sql.Tx, current, saved *Job, report Report, host string, e reportEventContext, at time.Time) error {
	var err error
	if e.detachPR {
		if err = addEvent(ctx, tx, saved.ID, host, "pr_detached", map[string]any{"pr": *current.PR, "from": current.Branch, "to": *report.Branch.Value, "epoch": report.Epoch}, at); err != nil {
			return err
		}
	}
	if report.PR.Value != nil && current.PR == nil {
		if err = addEvent(ctx, tx, saved.ID, host, "pr_created", map[string]any{"pr": *report.PR.Value, "epoch": report.Epoch}, at); err != nil {
			return err
		}
	}
	if err = recordRunningPublication(ctx, tx, current, saved, report, at); err != nil {
		return err
	}
	if e.phaseChanged {
		data := map[string]any{"from": e.phase, "to": saved.View["phase"], "leaseHost": e.leaseHost, "generation": e.generation, "epoch": report.Epoch}
		if err = addEvent(ctx, tx, saved.ID, host, "phase", data, at); err != nil {
			return err
		}
	}
	if e.leaving {
		data, err := reportStateEventData(current, report, e.leaseHost, e.generation, e.phase, e.hasPhase)
		if err != nil {
			return err
		}
		if err = addEvent(ctx, tx, saved.ID, host, "state", data, at); err != nil {
			return err
		}
	}
	return nil
}
