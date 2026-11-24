package jobs

import (
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// QuestionDataUnconfirmed distinguishes absent or unusable question records
// from an explicitly empty list on an implementation hold.
func QuestionDataUnconfirmed(job *Job) bool {
	if !questionHeld(job) {
		return false
	}
	questions, ok := job.Result["questions"].([]any)
	if !ok {
		return true
	}
	filled := false
	for _, q := range questions {
		text, ok := q.(string)
		if !ok {
			return true
		}
		filled = filled || strings.TrimSpace(text) != ""
	}
	return job.Result["status"] == "question" && !filled
}

// UserWaitKind identifies a saved state awaiting a user's decision.
type UserWaitKind string

const (
	UserWaitPermission    UserWaitKind = "permission"
	UserWaitDeletion      UserWaitKind = "deletion"
	UserWaitFinalApproval UserWaitKind = "final_approval"
	UserWaitRunLimit      UserWaitKind = "run_limit"
	UserWaitUsageLimit    UserWaitKind = "usage_limit"
)

// UserWaits classifies saved holds; it does not validate current approval targets.
func UserWaits(job *Job, runLimit int64, usageLimitHeld bool) ([]UserWaitKind, error) {
	if job == nil || job.Status != "held" || job.View["stopRequested"] == true || PrerequisiteMissingHeld(job) {
		return nil, nil
	}
	var waits []UserWaitKind
	if job.HoldReason != nil && *job.HoldReason == "review_held" && len(fleet.Object(job.View["permissionHold"])) > 0 {
		waits = append(waits, UserWaitPermission)
	}
	if job.HoldReason != nil && *job.HoldReason == "deletions" && job.View["deletionApproval"] != nil {
		waits = append(waits, UserWaitDeletion)
	}
	pending, err := PendingFinalApprovals(job)
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		waits = append(waits, UserWaitFinalApproval)
	}
	if RunLimitHeld(job, runLimit) {
		waits = append(waits, UserWaitRunLimit)
	}
	if questionHeld(job) && usageLimitHeld {
		waits = append(waits, UserWaitUsageLimit)
	}
	return waits, nil
}
