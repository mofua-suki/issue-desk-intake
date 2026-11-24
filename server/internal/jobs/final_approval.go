package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"strings"
	"time"

	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/fleet"
)

// PendingFinalApproval describes an approval required for the current job state.
type PendingFinalApproval struct {
	ID          string   `json:"id"`
	Target      string   `json:"target"`
	Operation   string   `json:"operation"`
	Condition   string   `json:"condition"`
	HeadSHA     string   `json:"headSha"`
	Scope       []string `json:"scope,omitempty"`
	Reason      string   `json:"reason"`
	Fingerprint string   `json:"fingerprint"`
}

// PendingFinalApprovals returns the approvals still required for this job.
// A changed approval invalidates earlier receipts.
func PendingFinalApprovals(job *Job) ([]PendingFinalApproval, error) {
	if job == nil || job.View["completionPlan"] == nil {
		return nil, nil
	}
	plan, err := completionplan.Parse(job.View["completionPlan"])
	if err != nil {
		return nil, err
	}
	records, _ := job.View["finalApprovals"].([]any)
	splitIndex := 0
	if index, ok := count(job.View["splitIndex"]); ok && index > 0 && int64(int(index)) == index {
		splitIndex = int(index)
	}
	head := stringValue(job.HeadSHA)
	result := []PendingFinalApproval{}
	// Nothing but RecordFinalApproval resumes a job that is already held for its
	// final approvals, so such a job keeps the approvals it was held for.
	heldFor := map[any]bool{}
	if FinalApprovalHeld(job) {
		ids, _ := fleet.Object(job.View[FinalApprovalHoldKey])["approvals"].([]any)
		for _, id := range ids {
			heldFor[id] = true
		}
	}
	for _, approval := range plan.Approvals {
		// Parse has verified that a gate only restates an existing after-diff gate.
		if approval.Gate != nil {
			continue
		}
		// Scope and basis items proceed without the user; see completionplan.NeedsUser.
		held := heldFor[approval.ID]
		if !held && !approval.AppliesTo(splitIndex) {
			continue
		}
		if !held && !completionplan.NeedsUser(approval.Category) {
			continue
		}
		scope := held && approval.Category == "scope" && job.ScopeSource == "widened"
		if approval.Phase != "after_diff" && !scope {
			continue
		}
		value := struct {
			Approval completionplan.Approval `json:"approval"`
			Scope    []string                `json:"scope,omitempty"`
		}{Approval: approval}
		value.Approval.Evidence = nil
		if scope {
			value.Scope = job.Scope
		}
		raw, _ := json.Marshal(value)
		hash := sha256.Sum256(raw)
		fingerprint := hex.EncodeToString(hash[:])
		found := false
		for _, item := range records {
			r, ok := item.(map[string]any)
			if ok && head != "" && r["id"] == approval.ID && r["fingerprint"] == fingerprint && (r["headSha"] == head || finalApprovalTransferred(job, r)) {
				found = true
				break
			}
		}
		if found {
			continue
		}
		reason := fmt.Sprintf("ジョブ %s#%d (ID %d) の承認 %s が未了です。head SHA: %s", job.Repo, job.Issue, job.ID, approval.ID, head)
		pending := PendingFinalApproval{ID: approval.ID, Target: approval.Target, Operation: approval.Operation, Condition: approval.Condition, HeadSHA: head, Reason: reason, Fingerprint: fingerprint}
		if scope {
			pending.Scope = append([]string(nil), job.Scope...)
			pending.Reason += "、拡大した範囲: " + strings.Join(job.Scope, ", ")
		}
		result = append(result, pending)
	}
	return result, nil
}

type FinalApprovalRecord struct {
	ID          string `json:"approvalId"`
	HeadSHA     string `json:"headSha"`
	MessageID   string `json:"messageId"`
	Quote       string `json:"quote"`
	Fingerprint string `json:"fingerprint"`
	// Via is "screen" or "token"; PC is the sending PC of a delegated token.
	Via string `json:"via,omitempty"`
	PC  string `json:"pc,omitempty"`
	// InstructionAt is the time the user gave the chat statement a token relays (empty for the screen).
	InstructionAt string `json:"instructionAt,omitempty"`
}

// RecordFinalApproval stores the user's statement (from the screen or relayed by a delegated token) after rechecking the locked job.
// Sending the same message ID for the same approval and head again returns the job without adding a second record.
func (s *Store) RecordFinalApproval(ctx context.Context, id int64, input FinalApprovalRecord) (*Job, error) {
	if !regexpUUID.MatchString(input.MessageID) || !contract.FilledText(input.Quote, 8000) || !shaPattern.MatchString(input.HeadSHA) || strings.TrimSpace(input.ID) == "" {
		return nil, controlProblem(400, "承認ID・対象SHA・発言ID・原文が必要です。")
	}
	var result *Job
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		job, err := readJob(ctx, tx, id, true)
		if err != nil {
			return err
		}
		// 同じ送信IDの送り直しは、記録後にheadが進んでいても記録済みとして返す。
		replayed, err := finalApprovalReplayed(job, input)
		if err != nil {
			return err
		}
		if replayed {
			result = job
			return nil
		}
		if job.HeadSHA == nil || *job.HeadSHA != input.HeadSHA {
			return controlProblem(409, "承認対象のSHAが変わりました。")
		}
		pending, err := PendingFinalApprovals(job)
		if err != nil {
			return controlProblem(409, "完了計画を確認できません。")
		}
		var target *PendingFinalApproval
		for i := range pending {
			if pending[i].ID == input.ID {
				target = &pending[i]
				break
			}
		}
		if target == nil || input.Fingerprint == "" || input.Fingerprint != target.Fingerprint {
			return controlProblem(409, "承認対象が計画にないか、既に記録されています。")
		}
		records, _ := job.View["finalApprovals"].([]any)
		record := s.finalApprovalEntry(job, input, target.Fingerprint)
		records = append(records, record)
		job.View["finalApprovals"] = records
		resume, unavailable, err := s.finalApprovalResume(ctx, tx, job, len(pending))
		if err != nil {
			return err
		}
		raw, err := json.Marshal(job.View)
		if err != nil {
			return err
		}
		if resume {
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb,status='queued',hold_reason=NULL,updated_at=$3 WHERE id=$1`, id, string(raw), s.options.Now())
		} else if unavailable {
			result, marshalErr := json.Marshal(job.Result)
			if marshalErr != nil {
				return marshalErr
			}
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb,result=$3::jsonb,updated_at=$4 WHERE id=$1`, id, string(raw), string(result), s.options.Now())
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb,updated_at=$3 WHERE id=$1`, id, string(raw), s.options.Now())
		}
		if err != nil {
			return err
		}
		if resume {
			if err = addEvent(ctx, tx, id, "", "state", map[string]any{"to": "queued", "from": "held", "finalApproval": true}, s.options.Now()); err != nil {
				return err
			}
		}
		result, err = readJob(ctx, tx, id, false)
		return err
	})
	return result, err
}

// finalApprovalReplayed は同じ送信IDの承認が記録済みなら、同じ承認の送り直しかを照合する。送り直しなら true を返し、別の承認なら断る。
func finalApprovalReplayed(job *Job, input FinalApprovalRecord) (bool, error) {
	existing, _ := job.View["finalApprovals"].([]any)
	for _, item := range existing {
		r, ok := item.(map[string]any)
		if !ok || r["messageId"] != input.MessageID {
			continue
		}
		if r["id"] != input.ID || r["headSha"] != input.HeadSHA || r["quote"] != input.Quote || recordedVia(r) != fmt.Sprint(nonEmpty(input.Via)) || fmt.Sprint(r["pc"]) != fmt.Sprint(nonEmpty(input.PC)) || fmt.Sprint(r["instructionAt"]) != fmt.Sprint(nonEmpty(input.InstructionAt)) {
			return false, controlProblem(409, "送信IDは別の承認で使用済みです。")
		}
		return true, nil
	}
	return false, nil
}

// finalApprovalEntry は記録する最終承認1件を、送信元と差分の指紋とともに組み立てる。
func (s *Store) finalApprovalEntry(job *Job, input FinalApprovalRecord, fingerprint string) map[string]any {
	record := map[string]any{"id": input.ID, "headSha": input.HeadSHA, "messageId": input.MessageID, "quote": input.Quote, "at": s.options.Now().UTC().Format(time.RFC3339Nano), "fingerprint": fingerprint}
	if binding := finalApprovalDiffBinding(job); binding != nil {
		record["diffFingerprint"] = binding
	}
	if input.Via != "" {
		record["via"] = input.Via
	}
	if input.PC != "" {
		record["pc"] = input.PC
	}
	if input.InstructionAt != "" {
		record["instructionAt"] = input.InstructionAt
	}
	return record
}

// finalApprovalResume は最後の最終承認を記録した依頼を待ちへ戻せるかを決める。戻せない最終承認待ちは unavailable を返す。
func (s *Store) finalApprovalResume(ctx context.Context, tx *sql.Tx, job *Job, pendingCount int) (bool, bool, error) {
	var err error
	resume := FinalApprovalHeld(job) && pendingCount == 1
	if resume {
		resume, err = finalApprovalResumeAllowed(ctx, tx, job)
		if err != nil {
			return false, false, err
		}
	}
	unavailable := false
	if !resume && pendingCount == 1 {
		resume, err = resumeReportFinalApproval(ctx, tx, job, s.options.Now())
		if err != nil {
			return false, false, err
		}
	}
	if resume && FinalApprovalHeld(job) {
		// Legacy holds did not snapshot publication before setting releasePending.
		// Do not turn that synthetic flag into evidence for those holds.
		evidenceJob := job
		hold := fleet.Object(job.View[FinalApprovalHoldKey])
		if _, exists := hold["published"]; !exists {
			copyJob := *job
			copyJob.View = make(map[string]any, len(job.View))
			for key, value := range job.View {
				copyJob.View[key] = value
			}
			delete(copyJob.View, "releasePending")
			evidenceJob = &copyJob
		}
		published, err := publishedImplementations(ctx, tx, []*Job{evidenceJob})
		if err != nil {
			return false, false, err
		}
		if evidenceJob != job {
			// The old hold event is not proof that instructions issued during
			// its lease have been implemented and republished.
			var instructed bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND kind='control' AND (data->>'action'='instruction' OR data->>'instruction'='true') AND seq>coalesce((SELECT max(seq) FROM job_events WHERE job_id=$1 AND kind='leased'),0))`, job.ID).Scan(&instructed)
			if err != nil {
				return false, false, err
			}
			if instructed {
				published[job.ID] = false
			}
		}
		resume = resumeFinalApprovalHold(job, s.options.Now(), published[job.ID])
		unavailable = !resume
	}
	return resume, unavailable, nil
}

// nonEmpty returns nil for "" so it compares equal to an omitted record field.
func nonEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// recordedVia treats records saved before the via field existed as screen approvals (the only route then).
func recordedVia(record map[string]any) string {
	if record["via"] == nil {
		return "screen"
	}
	return fmt.Sprint(record["via"])
}

// FinalApprovalHoldKey marks a job that left its lease because the merge turn
// was refused for an unrecorded final approval.
const FinalApprovalHoldKey = "finalApprovalHold"

// FinalApprovalHeld reports whether the job is held by holdForFinalApproval and
// has not been leased, retried or held for another reason since.
func FinalApprovalHeld(job *Job) bool {
	hold, ok := job.View[FinalApprovalHoldKey].(map[string]any)
	if !ok || job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "review_held" {
		return false
	}
	generation, okGeneration := count(hold["generation"])
	epoch, okEpoch := count(hold["epoch"])
	return okGeneration && okEpoch && generation == job.Generation && epoch == job.LeaseEpoch && hold["headSha"] == stringValue(job.HeadSHA)
}

// holdForFinalApproval releases the lease of a running job whose merge waits for
// the user. The worker sees lease_lost on its next renewal and frees its slot.
func (s *Store) holdForFinalApproval(ctx context.Context, tx *sql.Tx, job *Job, host string, pending []PendingFinalApproval, at time.Time) error {
	if job.Status != "running" {
		return nil
	}
	phase, hasPhase := job.View["phase"]
	ids := make([]any, 0, len(pending))
	reasons := make([]string, 0, len(pending))
	for _, item := range pending {
		ids = append(ids, item.ID)
		reasons = append(reasons, item.Reason)
	}
	published, err := publishedImplementations(ctx, tx, []*Job{job})
	if err != nil {
		return err
	}
	if job.View == nil {
		job.View = map[string]any{}
	}
	job.View[FinalApprovalHoldKey] = map[string]any{"headSha": stringValue(job.HeadSHA), "branch": stringValue(job.Branch), "pr": job.PR, "published": published[job.ID], "generation": job.Generation, "epoch": job.LeaseEpoch, "approvals": ids, "at": iso(at)}
	job.View["releasePending"] = true
	raw, err := json.Marshal(job.View)
	if err != nil {
		return err
	}
	if job.Result == nil {
		job.Result = map[string]any{}
	}
	job.Result["summary"] = "マージの最終承認を待っています。" + strings.Join(reasons, "；")
	result, err := json.Marshal(job.Result)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='review_held',view=$2::jsonb,result=$3::jsonb,updated_at=$4 WHERE id=$1 AND status='running'`, job.ID, string(raw), string(result), at); err != nil {
		return err
	}
	event := map[string]any{"to": "held", "holdReason": "review_held", "epoch": job.LeaseEpoch, "finalApproval": true, "leaseHost": job.LeaseHost, "generation": job.Generation}
	if hasPhase {
		event["phase"] = phase
	}
	return addEvent(ctx, tx, job.ID, host, "state", event, at)
}

// FinalApprovalResumeKey keeps a requeued job away from the PC whose worker may
// still be running it until that worker's lease has expired. The worker learns of
// the lost lease only on its next renewal; a new lease to the same PC before then
// would be refused as a duplicate assignment and stop that PC's scheduler.
const FinalApprovalResumeKey = "finalApprovalResume"

// finalApprovalLeaseLive reports whether host may still run the job's previous lease.
func finalApprovalLeaseLive(job *Job, host string, at time.Time) bool {
	resume, ok := job.View[FinalApprovalResumeKey].(map[string]any)
	if !ok || resume["host"] != host {
		return false
	}
	until, err := time.Parse(time.RFC3339Nano, fleet.Text(resume["until"]))
	return err != nil || until.After(at)
}

// resumeFinalApprovalHold queues the held job again once its last approval is
// recorded, continuing from the published PR like an approved permission hold.
func resumeFinalApprovalHold(job *Job, at time.Time, published bool) bool {
	hold := job.View[FinalApprovalHoldKey].(map[string]any)
	artifact, ok := artifactOf(job)
	pr, prOK := count(hold["pr"])
	branch, branchOK := hold["branch"].(string)
	wasPublished, _ := hold["published"].(bool)
	_, hasSnapshot := hold["published"]
	// A legacy final-approval hold has no tuple snapshot. Its row stayed held
	// and fenced since the recorded head and generation; require independent
	// publication evidence and the current intake-owned artifact instead.
	_, hasBranch := hold["branch"]
	_, hasPR := hold["pr"]
	if !hasSnapshot && !hasBranch && !hasPR && publicationTuple(job) {
		pr, prOK, branch, branchOK, wasPublished = *job.PR, true, stringValue(job.Branch), true, published
	}
	if !ok || artifact.Generation != job.Generation || !publicationTuple(job) || !prOK || pr != *job.PR || !branchOK || branch != artifact.Branch || hold["headSha"] != artifact.Head || !wasPublished || !published {
		if job.Result == nil {
			job.Result = map[string]any{}
		}
		job.Result["summary"] = "最終承認は記録されましたが、公開済み成果物のPR・branch・head・世代を確認できないため続行できません。"
		return false
	}
	if job.LeaseHost != nil && job.LeaseExpiresAt != nil {
		if until, err := time.Parse(time.RFC3339Nano, *job.LeaseExpiresAt); err == nil && until.After(at) {
			job.View[FinalApprovalResumeKey] = map[string]any{"host": *job.LeaseHost, "until": *job.LeaseExpiresAt}
		}
	}
	continuation := fleet.Object(job.View["continuation"])
	continuation["publication"] = map[string]any{"generation": artifact.Generation, "branch": artifact.Branch, "headSha": artifact.Head, "pr": *job.PR, "verified": true}
	job.View["continuation"] = continuation
	delete(job.View, FinalApprovalHoldKey)
	job.Status = "queued"
	job.HoldReason = nil
	return true
}
