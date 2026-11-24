package operations

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
)

// recoveryIssueClosed reports true only for a confirmed closed issue, so a failed
// or skipped lookup keeps the row visible instead of hiding a pending decision.
func (s *Service) recoveryIssueClosed(ctx context.Context, repo string, issue int64) bool {
	base, err := gitea.RepoPath(repo)
	if s.Gitea == nil || err != nil || issue <= 0 || ctx.Err() != nil {
		return false
	}
	var current map[string]any
	err = s.Gitea.Do(ctx, "GET", fmt.Sprintf("%s/issues/%d", base, issue), nil, &current)
	return err == nil && current["state"] == "closed"
}

// recoveryStateLabel describes the recorded outcome. The ledger does not say
// whether the user must act, so the label must not claim that it is required.
func recoveryStateLabel(state string) string {
	switch state {
	case "abandoned":
		return "自動の再送を打ち切り（原因の確認が必要）"
	case "send_unknown", "send_rejected":
		return "自動復旧の送信結果の確認が必要"
	}
	return "自動復旧の判断の確認が必要"
}

// Project only a still-current decision. Reading never retries or acknowledges it.
func (s *Service) jobRecoveryAttention(ctx context.Context, configured map[string]bool) ([]any, error) {
	out := []any{}
	if s.PostgreSQL == nil {
		return out, nil
	}
	recovery := &JobRecovery{Service: s}
	records, err := recovery.records(ctx)
	if err != nil {
		return nil, err
	}
	// Bound the Gitea lookups across the whole read; on timeout rows stay visible.
	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for _, row := range records {
		if !configured[row.Repo] {
			continue
		}
		cause, ok := row.Causes[row.Cause]
		if !ok || !(cause.Action == "notify" && cause.State == "skipped" || cause.State == "abandoned" || cause.State == "send_unknown" || cause.State == "send_rejected") {
			continue
		}
		job, current, err := s.currentRecoveryJob(ctx, row, cause)
		if err != nil {
			return nil, err
		}
		if !current {
			continue
		}
		// Closed issues are finished work even when the job row is still held.
		if s.recoveryIssueClosed(lookupCtx, row.Repo, row.Issue) {
			continue
		}
		out = append(out, recoveryAttentionView(s.issueRef(row.Repo, row.Issue), row, cause, job))
	}
	return out, nil
}

// currentRecoveryJob は台帳の判断が今のジョブの世代・原因・状態のままかを読み直す。変わっていれば current=false を返す。
func (s *Service) currentRecoveryJob(ctx context.Context, row RecoveryRecord, cause RecoveryCause) (jobs.Job, bool, error) {
	job := jobs.Job{Repo: row.Repo, Issue: row.Issue}
	var raw []byte
	err := s.PostgreSQL.QueryRowContext(ctx, `SELECT id,title,kind,status,hold_reason,generation,result FROM jobs WHERE repo=$1 AND issue=$2`, row.Repo, row.Issue).Scan(&job.ID, &job.Title, &job.Kind, &job.Status, &job.HoldReason, &job.Generation, &raw)
	if err == sql.ErrNoRows {
		return jobs.Job{}, false, nil
	}
	if err != nil {
		return jobs.Job{}, false, err
	}
	job.Result = jobs.StoredResult(job.ID, raw)
	if job.Generation != cause.Generation || recoveryCause(&job) != row.Cause || !(job.Status == "failed" || job.Status == "held" && job.HoldReason != nil && (*job.HoldReason == "question" || *job.HoldReason == "review_held")) {
		return jobs.Job{}, false, nil
	}
	return job, true, nil
}

// recoveryAttentionView は自動復旧の判断待ちを、原因の状態に応じた理由とともに要対応の項目にする。
func recoveryAttentionView(ref any, row RecoveryRecord, cause RecoveryCause, job jobs.Job) map[string]any {
	reason := cause.Reason
	if cause.State == "abandoned" {
		reason = "同じ原因が再発したため、自動の再送を止めました。原因と前回の対処を確認してください。"
		if cause.Handoff && cause.Reason != "" {
			reason = cause.Reason
		}
	} else if cause.State == "send_unknown" || cause.State == "send_rejected" {
		reason = "自動復旧の送信結果を確認できないか、受付が送信を拒否しました。同じ要求を自動で送り直しません。"
	}
	view := fleet.Object(ref)
	for key, value := range map[string]any{"key": fmt.Sprintf("job-recovery:%s#%d", row.Repo, row.Issue), "kind": "job_recovery", "kindName": "自動復旧の判断待ち", "state": "notify", "stateLabel": recoveryStateLabel(cause.State), "title": job.Title, "reason": usage.Redact(reason), "gist": usage.Redact(cause.ActionText), "updatedAt": cause.At, "startedAt": cause.At} {
		view[key] = value
	}
	view["causeKey"] = fmt.Sprintf("job-recovery:%s#%d:%s:%d", row.Repo, row.Issue, row.Cause, cause.Generation)
	view["recoveryState"] = cause.State
	if cause.Handoff {
		view["handoff"] = true
	}
	if cause.RequiresUser != nil {
		view["requiresUser"] = *cause.RequiresUser
	}
	return view
}
