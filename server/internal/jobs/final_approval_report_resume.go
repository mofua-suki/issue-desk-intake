package jobs

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func finalApprovalResumeAllowed(ctx context.Context, tx *sql.Tx, job *Job) (bool, error) {
	if job.View["stopRequested"] == true || fleet.Object(job.View["liveCheck"])["status"] == "pending" || job.View["permissionHold"] != nil || job.View["deletionApproval"] != nil {
		return false, nil
	}
	var stopped bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_commands WHERE target_host=$1 AND repo=$2 AND issue=$3 AND kind='stop' AND status IN ('queued','acked'))`, stringValue(job.LeaseHost), job.Repo, job.Issue).Scan(&stopped)
	return !stopped, err
}

// resumeReportFinalApproval only transfers an intake-owned, independently published artifact.
// The caller holds the job row lock and commits the approval and transition together.
func resumeReportFinalApproval(ctx context.Context, tx *sql.Tx, job *Job, at time.Time) (bool, error) {
	artifact, resumable, err := reportFinalApprovalResumable(ctx, tx, job)
	if err != nil || !resumable {
		return false, err
	}
	applyReportFinalApprovalResume(job, artifact, at)
	return true, nil
}

// reportFinalApprovalResumable は受付が別に公開を確かめた成果物で、レビュー保留から最終承認だけで再開できるかを照合する。
func reportFinalApprovalResumable(ctx context.Context, tx *sql.Tx, job *Job) (artifactIdentity, bool, error) {
	if job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "review_held" || job.View[FinalApprovalHoldKey] != nil || (job.View["releasePending"] != true && job.View["publishPending"] != true) {
		return artifactIdentity{}, false, nil
	}
	allowed, err := finalApprovalResumeAllowed(ctx, tx, job)
	if err != nil || !allowed {
		return artifactIdentity{}, false, err
	}
	artifact, ok := artifactOf(job)
	if !ok || artifact.Generation != job.Generation || !publicationTuple(job) {
		return artifactIdentity{}, false, nil
	}
	var held, instructed, receipt bool
	err = tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='held' AND data->>'holdReason'='review_held' AND data->>'epoch'=$2),
 EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND kind='control' AND (data->>'action'='instruction' OR data->>'instruction'='true') AND seq>coalesce((SELECT max(seq) FROM job_events WHERE job_id=$1 AND (kind='running_publication' AND data->>'generation'=$3 AND data->>'branch'=$4 AND data->>'headSha'=$5 AND data->>'pr'=$6)),0)),
 EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND (kind='running_publication' AND data->>'generation'=$3 AND data->>'branch'=$4 AND data->>'headSha'=$5 AND data->>'pr'=$6))`, job.ID, strconv.FormatInt(job.LeaseEpoch, 10), strconv.FormatInt(job.Generation, 10), artifact.Branch, artifact.Head, strconv.FormatInt(*job.PR, 10)).Scan(&held, &instructed, &receipt)
	if err != nil {
		return artifactIdentity{}, false, err
	}
	publication := fleet.Object(fleet.Object(job.View["continuation"])["publication"])
	publicationPR, prOK := count(publication["pr"])
	verified := prOK && publicationPR == *job.PR && artifactPublished(job, artifact)
	if !held || instructed || (!receipt && !verified) {
		return artifactIdentity{}, false, nil
	}
	published, err := publishedImplementations(ctx, tx, []*Job{job})
	if err != nil || !published[job.ID] {
		return artifactIdentity{}, false, err
	}
	return artifact, true, nil
}

// applyReportFinalApprovalResume は確かめた公開を引き継ぎとして記録し、依頼を待ちへ戻す。lease が残っていればそのPCを優先する。
func applyReportFinalApprovalResume(job *Job, artifact artifactIdentity, at time.Time) {
	if job.LeaseHost != nil && job.LeaseExpiresAt != nil {
		if until, err := time.Parse(time.RFC3339Nano, *job.LeaseExpiresAt); err == nil && until.After(at) {
			job.View[FinalApprovalResumeKey] = map[string]any{"host": *job.LeaseHost, "until": *job.LeaseExpiresAt}
		}
	}
	c := fleet.Object(job.View["continuation"])
	c["publication"] = map[string]any{"generation": artifact.Generation, "branch": artifact.Branch, "headSha": artifact.Head, "pr": *job.PR, "verified": true}
	job.View["continuation"] = c
	job.Status = "queued"
	job.HoldReason = nil
}
