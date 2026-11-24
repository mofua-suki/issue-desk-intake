package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"strings"
)

// recordMissingCause records an exclusion without disturbing an active reservation.
// loaded is the ledger the caller read for this pass. An exclusion is never
// cleared, so a job already excluded there needs no further read; any other job
// is checked against the saved ledger as before.
func (r *JobRecovery) recordMissingCause(ctx context.Context, loaded []RecoveryRecord, job *jobs.Job) error {
	if missingCauseRecorded(loaded, job) {
		return nil
	}
	records, err := r.records(ctx)
	if err != nil {
		return err
	}
	if missingCauseRecorded(records, job) {
		return nil
	}
	return r.ledger(ctx, func(rows *[]RecoveryRecord) error {
		for i := range *rows {
			row := &(*rows)[i]
			if row.Repo == job.Repo && row.Issue == job.Issue {
				if row.ExclusionReason == "原因なし" {
					return errRecoveryUnchanged
				}
				row.ExclusionReason = "原因なし"
				row.ExclusionAt = r.Service.now()
				return nil
			}
		}
		*rows = append(*rows, RecoveryRecord{Repo: job.Repo, Issue: job.Issue, Causes: map[string]RecoveryCause{}, ExclusionReason: "原因なし", ExclusionAt: r.Service.now()})
		return nil
	})
}

func missingCauseRecorded(records []RecoveryRecord, job *jobs.Job) bool {
	for _, row := range records {
		if row.Repo == job.Repo && row.Issue == job.Issue && row.ExclusionReason == "原因なし" {
			return true
		}
	}
	return false
}

// Unknown stop, transfer, or approval data is not safe to act on.
func recoveryUncertainView(view map[string]any) bool {
	if view == nil {
		return true
	}
	// A merge waiting for the user's final approval resumes only after the approval.
	if view["finalApprovalHold"] != nil {
		return true
	}
	if stop, ok := view["stopRequested"]; ok && stop != nil {
		v, valid := stop.(bool)
		if !valid || v {
			return true
		}
	}
	if raw, ok := view["acceptanceTransferWait"]; ok && raw != nil {
		wait, valid := raw.(map[string]any)
		if !valid {
			return true
		}
		if blocking, ok := wait["blocking"]; ok {
			value, valid := blocking.(bool)
			if !valid || value {
				return true
			}
		} else {
			return true
		}
	}
	return false
}

// Closed records without reservations are evicted oldest first. Open and stopped
// jobs retain their history indefinitely; when full, new records must wait for
// jobs to close so that retry and reservation safety is not lost.
func (r *JobRecovery) recoveryContext(ctx context.Context) (map[string]bool, map[string]bool, map[string]bool, error) {
	if r.Service.Repositories == nil {
		return nil, nil, nil, fmt.Errorf("configured repositories unavailable")
	}
	repos, err := r.Service.Repositories()
	if err != nil {
		return nil, nil, nil, err
	}
	configured := map[string]bool{}
	for _, repo := range repos {
		configured[repo] = true
	}
	if r.Service.DB == nil {
		return nil, nil, nil, fmt.Errorf("attention database unavailable")
	}
	attention, err := r.Service.table(ctx, "attention_cases")
	if err != nil {
		return nil, nil, nil, err
	}
	waits := map[string]bool{}
	for _, a := range attention {
		if a["state"] == "usage_wait" || a["state"] == "retrying" && a["usageLimitRetry"] != nil {
			if fleet.Text(a["project"]) == "" || fleet.Number(a["number"]) < 1 {
				continue
			}
			waits[fmt.Sprintf("%s#%d", fleet.Text(a["project"]), int64(fleet.Number(a["number"])))] = true
		}
	}
	records, err := r.records(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	abandoned := map[string]bool{}
	for _, rec := range records {
		for cause, v := range rec.Causes {
			if v.State == "abandoned" {
				abandoned[fmt.Sprintf("%s#%d:%s", rec.Repo, rec.Issue, cause)] = true
			}
		}
	}
	return configured, waits, abandoned, nil
}

func (r *JobRecovery) recoveryExcluded(ctx context.Context, j *jobs.Job, cause string, configured, waits, abandoned map[string]bool, runLimit int64) (bool, error) {
	if j.Status == "held" && j.HoldReason != nil && *j.HoldReason == "review_held" && recoveryReviewLimitReached(j.View, r.ReviewLimit) {
		return true, nil
	}
	if !configured[j.Repo] || jobs.PrerequisiteMissingHeld(j) || j.Kind == "investigation" || j.View["workType"] == "investigation" || recoveryUncertainView(j.View) || j.View["permissionHold"] != nil || j.View["deletionApproval"] != nil || jobs.EffectiveRuns(j) >= runLimit || jobs.RunLimitHeld(j, runLimit) || waits[fmt.Sprintf("%s#%d", j.Repo, j.Issue)] {
		return true, nil
	}
	reason := strings.ToLower(fleet.Text(j.Result["error"]) + " " + fleet.Text(j.Result["reason"]))
	if recoveryFollowupWithoutQuestion(j) {
		b, err := json.Marshal([]any{j.Result["summary"], j.Result["incomplete"]})
		if err != nil {
			return true, nil
		}
		reason += " " + strings.ToLower(string(b))
	}
	if strings.Contains(reason, "権限の範囲を定めるファイルの変更") || strings.Contains(reason, "利用上限") || strings.Contains(reason, "usage limit") || strings.Contains(reason, "削除の承認") {
		return true, nil
	}
	if j.LeaseHost != nil {
		var stop bool
		err := r.Service.PostgreSQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_commands WHERE target_host=$1 AND repo=$2 AND issue=$3 AND kind='stop' AND status IN ('queued','acked'))`, *j.LeaseHost, j.Repo, j.Issue).Scan(&stop)
		if err != nil || stop {
			return stop, err
		}
	}
	usage, err := jobs.RecoveryUsageReason(ctx, r.Service.PostgreSQL, j, jobs.ControlOptions{UsageLimit: r.UsageLimit, UsageWeights: r.UsageWeights})
	if err != nil {
		return false, err
	}
	return usage != "" || abandoned[fmt.Sprintf("%s#%d:%s", j.Repo, j.Issue, cause)], nil
}
