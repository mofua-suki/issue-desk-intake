package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// Observations are accepted only through the delegated controller operation.
// Source is the cited artifact; Actor is supplied by the authenticated route.
type ExecutionCheck struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
	PlanSHA     string `json:"planSHA"`
	Head        string `json:"head"`
	Generation  int64  `json:"generation"`
	Status      string `json:"status"`
	Source      string `json:"source"`
	ObservedAt  string `json:"observedAt"`
	Detail      string `json:"detail"`
}

// RecoverFailedMerged retries saved failed reports without creating new evidence.
func (s *Store) RecoverFailedMerged(ctx context.Context) error {
	if s.options.ReadPull == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE status='failed' AND pr IS NOT NULL AND head_sha IS NOT NULL AND result->'leaseDiagnostics'->'lastReportRejection'->>'reportKind'='merged' ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		err = s.transaction(ctx, func(tx *sql.Tx) error {
			job, e := readJob(ctx, tx, id, true)
			if e != nil {
				return e
			}
			candidate, e := failedMergedReportCandidate(ctx, tx, job)
			if e != nil || !candidate {
				return e
			}
			plan, e := workplan.Parse(job.View["executionPlan"])
			if e != nil || plan == nil {
				return e
			}
			receipts, e := workplan.DecodeEvidence(job.View["executionReceipts"])
			if e != nil {
				return e
			}
			complete, e := executionEvidenceComplete(job, plan, *job.HeadSHA, receipts, true)
			if e != nil || !complete {
				return e
			}
			merge, e := s.confirmFailedMergedPull(ctx, job, *job.HeadSHA)
			if e != nil || merge == "" {
				return e
			}
			job.View["executionWait"] = nil
			job.View["executionReportError"] = nil
			view, e := jsonValue(job.View)
			if e != nil {
				return e
			}
			result, e := jsonValue(map[string]any{"status": "merged", "summary": "外部確認と報告の条件を対象版で確認しました", "mergeCommit": merge})
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, `UPDATE jobs SET status='merged',hold_reason=NULL,lease_expires_at=NULL,view=$2::jsonb,updated_at=$3,result=(CASE WHEN jsonb_typeof(result)='object' THEN result ELSE '{}'::jsonb END) || $4::jsonb WHERE id=$1`, id, view, s.options.Now(), result)
			return e
		})
		if err != nil {
			failures = append(failures, err)
			continue
		}
	}
	return errors.Join(failures...)
}

func (s *Store) RecordExecutionCheck(ctx context.Context, id int64, check ExecutionCheck, actor, reportRecord string) (*Job, error) {
	if actor == "" {
		return nil, contract.NewProblem(403, "確認記録の送信元がありません。", "")
	}
	if err := ValidateExecutionCheck(check, s.options.Now()); err != nil {
		return nil, err
	}

	var saved *Job
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		job, e := readJob(ctx, tx, id, true)
		if e != nil {
			return e
		}
		payload := map[string]any{"check": check, "actor": actor, "reportRecord": reportRecord}
		raw, _ := json.Marshal(payload)
		replayed, e := executionCheckReplayed(ctx, tx, id, check.ID, raw)
		if e != nil {
			return e
		}
		failedCandidate, e := failedMergedReportCandidate(ctx, tx, job)
		if e != nil {
			return e
		}
		if replayed && !(job.Status == "held" && mergedReportWait(job)) && !failedCandidate {
			saved = job
			return nil
		}
		plan, e := workplan.Parse(job.View["executionPlan"])
		if e != nil || plan == nil || plan.Fingerprint() != check.PlanSHA || job.HeadSHA == nil || *job.HeadSHA != check.Head || job.Generation != check.Generation {
			if replayed {
				saved = job
				return nil
			}
			return contract.NewProblem(409, "確認対象の計画・世代・SHAが変わりました。", "execution_check_stale")
		}
		var completed bool
		if replayed {
			receipts, err := workplan.DecodeEvidence(job.View["executionReceipts"])
			if err != nil {
				return err
			}
			completed, e = executionEvidenceComplete(job, plan, check.Head, receipts, failedCandidate)
		} else {
			completed, e = recordExecutionEvidence(job, plan, check, actor, reportRecord, failedCandidate)
		}
		if e != nil {
			return e
		}
		needsMergeProof := completed && (mergedReportWait(job) || failedCandidate)
		mergeCommit := check.Head
		if needsMergeProof {
			if failedCandidate {
				mergeCommit, e = s.confirmFailedMergedPull(ctx, job, check.Head)
				completed = mergeCommit != ""
			} else {
				completed, e = s.confirmReportWaitMerge(ctx, job, check.Head)
			}
			if e != nil {
				return e
			}
		}
		if replayed && !completed {
			saved = job
			return nil
		}
		if completed {
			job.View["executionWait"] = nil
			job.View["executionReportError"] = nil
		}
		body, e := jsonValue(job.View)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb,updated_at=$3 WHERE id=$1`, id, body, s.options.Now()); e != nil {
			return e
		}
		if completed {
			result := map[string]any{"status": "merged", "summary": "外部確認と報告の条件を対象版で確認しました"}
			if needsMergeProof {
				result["mergeCommit"] = mergeCommit
			}
			rawResult, e := jsonValue(result)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `UPDATE jobs SET status='merged',hold_reason=NULL,lease_expires_at=NULL,result=(CASE WHEN jsonb_typeof(result)='object' THEN result ELSE '{}'::jsonb END) || $2::jsonb WHERE id=$1`, id, rawResult); e != nil {
				return e
			}
		}
		if !replayed {
			if e = addEvent(ctx, tx, id, actor, "execution_check", payload, s.options.Now()); e != nil {
				return e
			}
		}
		saved, e = readJob(ctx, tx, id, false)
		return e
	})
	if err == nil && s.options.Publish != nil {
		s.options.Publish(map[string]any{"repo": saved.Repo, "issue": saved.Issue})
	}
	return saved, err
}

// executionCheckReplayed は同じ送信IDの確認記録があれば、同じ内容の送り直しかを照合する。送り直しなら true を返し、別の記録なら断る。
func executionCheckReplayed(ctx context.Context, tx *sql.Tx, id int64, checkID string, raw []byte) (bool, error) {
	var old []byte
	e := tx.QueryRowContext(ctx, `SELECT data FROM job_events WHERE job_id=$1 AND kind='execution_check' AND data->'check'->>'id'=$2 LIMIT 1`, id, checkID).Scan(&old)
	if e == nil {
		var a, b any
		_ = json.Unmarshal(raw, &a)
		_ = json.Unmarshal(old, &b)
		if !reflect.DeepEqual(a, b) {
			return false, contract.NewProblem(409, "送信IDは別の確認記録で使用済みです。", "")
		}
		return true, nil
	}
	if e != sql.ErrNoRows {
		return false, e
	}
	return false, nil
}

// recordExecutionEvidence はコントローラーの外部確認・訂正報告の条件に確認結果を記録し、計画のすべての条件が対象版で通ってレビュー保留を終えられるかを返す。
func recordExecutionEvidence(job *Job, plan *workplan.Plan, check ExecutionCheck, actor, reportRecord string, failedCandidate bool) (bool, error) {
	var requirement *workplan.Requirement
	for i := range plan.Requirements {
		if plan.Requirements[i].ID == check.Requirement {
			requirement = &plan.Requirements[i]
		}
	}
	if requirement == nil || !(requirement.EvidenceKind == "external_check" && requirement.Owner == "controller" || requirement.EvidenceKind == "report_comment" && reportRecord != "") {
		return false, contract.NewProblem(409, "この条件はコントローラーの外部確認・訂正報告ではありません。", "")
	}
	evidence, e := workplan.DecodeEvidence(job.View["executionReceipts"])
	if e != nil {
		return false, e
	}
	kept := evidence[:0]
	for _, v := range evidence {
		if v.Requirement != check.Requirement {
			kept = append(kept, v)
		}
	}
	record := "controller:" + actor + ":" + check.ID
	if reportRecord != "" {
		record = reportRecord
	}
	next := workplan.Evidence{Requirement: check.Requirement, PlanSHA: check.PlanSHA, Head: check.Head, Generation: check.Generation, RecordID: record, Source: requirement.Owner, Status: check.Status, Detail: check.Source + "\n" + check.ObservedAt + "\n" + check.Detail}
	job.View["executionReceipts"] = append(kept, next)
	return executionEvidenceComplete(job, plan, check.Head, append(kept, next), failedCandidate)
}

func executionEvidenceComplete(job *Job, plan *workplan.Plan, head string, evidence []workplan.Evidence, failedCandidate bool) (bool, error) {
	allDone, noCode := true, true
	for _, r := range plan.Requirements {
		noCode = noCode && r.EvidenceKind != "fixed_test"
	}
	for _, c := range plan.Checks(head, evidence, job.Generation) {
		allDone = allDone && c.Status == "passed"
	}
	completed := allDone && (failedCandidate || (noCode || mergedReportWait(job)) && job.Status == "held" && job.HoldReason != nil && *job.HoldReason == "review_held")
	if completed {
		pending, err := PendingFinalApprovals(job)
		if err != nil {
			return false, err
		}
		completed = len(pending) == 0
	}
	return completed, nil
}

func mergedReportWait(job *Job) bool {
	return job.View["codeMerged"] == true && job.View["executionWait"] == "reporter" &&
		job.PR != nil && *job.PR > 0 && job.Branch != nil && *job.Branch != ""
}

func (s *Store) confirmReportWaitMerge(ctx context.Context, job *Job, head string) (bool, error) {
	if s.options.ReadPull == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	pull, err := s.options.ReadPull(ctx, job.Repo, *job.PR)
	if err != nil {
		return false, err
	}
	source, target := fleet.Object(pull["head"]), fleet.Object(pull["base"])
	sourceRepo, targetRepo := fleet.Object(source["repo"]), fleet.Object(target["repo"])
	branch := fleet.Text(targetRepo["default_branch"])
	return fleet.WholeNumber(pull["number"]) == *job.PR && pull["merged"] == true &&
		pull["merge_commit_sha"] == head && source["sha"] == head && source["ref"] == *job.Branch &&
		sourceRepo["full_name"] == job.Repo && targetRepo["full_name"] == job.Repo &&
		branch != "" && target["ref"] == branch, nil
}

func ValidateExecutionCheck(check ExecutionCheck, now time.Time) error {
	if !regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`).MatchString(check.ID) || !shaPattern.MatchString(check.Head) || !oneOf(check.Status, "passed", "failed") || len(check.Source) > 2000 || strings.TrimSpace(check.Source) == "" || strings.TrimSpace(check.Detail) == "" || len(check.Detail) > 8000 {
		return contract.NewProblem(400, "確認記録の対象・根拠が不正です。", "")
	}
	at, err := time.Parse(time.RFC3339Nano, check.ObservedAt)
	if err != nil || at.After(now.Add(time.Minute)) || at.Before(now.Add(-7*24*time.Hour)) {
		return contract.NewProblem(400, "観測時刻は直近7日以内で指定してください。", "")
	}
	return nil
}

// failedMergedReportCandidate requires both the persisted failure diagnostic and
// the actual same-generation expiration events, not a worker's merge claim.
func failedMergedReportCandidate(ctx context.Context, tx *sql.Tx, job *Job) (bool, error) {
	if job.Status != "failed" || job.PR == nil || *job.PR <= 0 || job.Branch == nil || *job.Branch == "" || job.HeadSHA == nil {
		return false, nil
	}
	diag := fleet.Object(fleet.Object(job.Result["leaseDiagnostics"])["lastReportRejection"])
	gen, ok := count(diag["generation"])
	expired, valid := count(job.Result["leaseExpiredCount"])
	if !ok || gen != job.Generation || !valid || expired < 3 || diag["reportKind"] != "merged" {
		return false, nil
	}
	var n int
	var lastStatus string
	err := tx.QueryRowContext(ctx, `SELECT count(*), COALESCE((array_agg(data->>'to' ORDER BY seq DESC))[1], '') FROM job_events WHERE job_id=$1 AND kind='lease_expired' AND data->>'generation'=$2`, job.ID, jsonNumber(job.Generation)).Scan(&n, &lastStatus)
	return n >= 3 && lastStatus == "failed", err
}

func (s *Store) confirmFailedMergedPull(ctx context.Context, job *Job, head string) (string, error) {
	if s.options.ReadPull == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	pull, err := s.options.ReadPull(ctx, job.Repo, *job.PR)
	if err != nil {
		return "", err
	}
	source, target := fleet.Object(pull["head"]), fleet.Object(pull["base"])
	sourceRepo, targetRepo := fleet.Object(source["repo"]), fleet.Object(target["repo"])
	merge := fleet.Text(pull["merge_commit_sha"])
	branch := fleet.Text(targetRepo["default_branch"])
	if fleet.WholeNumber(pull["number"]) != *job.PR || pull["merged"] != true || !mergeCommitPattern.MatchString(merge) || source["sha"] != head || source["ref"] != *job.Branch || sourceRepo["full_name"] != job.Repo || targetRepo["full_name"] != job.Repo || branch == "" || target["ref"] != branch {
		return "", nil
	}
	return merge, nil
}
