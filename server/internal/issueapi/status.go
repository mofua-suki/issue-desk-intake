package issueapi

// Issueとジョブの状態から、画面とAPIに出す状態を導く。

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

func shownStatus(issue Object, job *jobs.Job, waiting bool) string {
	status := "untracked"
	if waiting {
		status = "pending"
	}
	if job != nil {
		switch job.Status {
		case "queued":
			status = "queued"
			if fleet.ObjectOrEmpty(job.View["acceptanceTransferWait"])["blocking"] == true {
				status = "dependency_waiting"
			}
		case "running":
			phase := fleet.Text(job.View["phase"])
			if phase == "publishing" || phase == "releasing" || phase == "deploying" {
				status = phase
			} else {
				status = "running"
			}
		case "held":
			reason := ""
			if job.HoldReason != nil {
				reason = *job.HoldReason
			}
			status = map[string]string{"question": "waiting", "scope_wait": "scope_waiting", "prerequisite_missing": "prerequisite_waiting"}[reason]
			if status == "" {
				status = "review_held"
			}
			if fleet.ObjectOrEmpty(job.View["liveCheck"])["status"] == "pending" {
				status = "deploying"
			}
		case "merged":
			status = "completed"
		default:
			status = job.Status
		}
	}
	if job != nil && job.View["liveCheck"] != nil && job.Status != "merged" && job.Status != "closed" {
		return status
	}
	if job == nil && issue["state"] == "closed" {
		if legacy := legacyStatus(issue); legacy != "" {
			status = legacy
		}
	}
	if issue["state"] == "closed" && status != "completed" && !unfinishedExecutionJob(job) && !oneOf(status, "running", "publishing", "scope_waiting", "releasing", "deploying") {
		status = "closed"
	}
	return status
}
