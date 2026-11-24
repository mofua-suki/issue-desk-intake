package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func publicationTuple(job *Job) bool {
	return job.PR != nil && *job.PR > 0 && strings.TrimSpace(stringValue(job.Branch)) != "" && shaPattern.MatchString(stringValue(job.HeadSHA))
}

func publicationPhase(view map[string]any) bool {
	return fleet.PublicationPhase(view)
}

func reimplementationExpected(job *Job) bool {
	for _, key := range []string{"reimplementExpected", "retryRetest", "retryFromBase"} {
		mark := fleet.Object(job.View[key])
		generation, ok := count(mark["generation"])
		if ok && generation == job.Generation && (key != "reimplementExpected" || mark["reimplements"] == true) {
			return true
		}
	}
	return false
}

// A running report that explicitly confirms the PR is the publication boundary.
// Keep the receipt in the existing event store: controlRetry deliberately retains
// the old PR tuple, phase and reimplementExpected even after a new lease.
func recordRunningPublication(ctx context.Context, tx *sql.Tx, previous, job *Job, report Report, at time.Time) error {
	if job.Status != "running" || !publicationTuple(job) || !report.PR.Present || report.PR.Value == nil || !publicationPhase(report.View) {
		return nil
	}
	if publicationPhase(previous.View) {
		var recorded bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM job_events p WHERE p.job_id=$1 AND p.kind='running_publication'
			AND p.data->>'generation'=$2 AND p.data->>'epoch'=$3 AND p.data->>'pr'=$4
			AND p.data->>'branch'=$5 AND p.data->>'headSha'=$6
			AND NOT EXISTS(SELECT 1 FROM job_events c WHERE c.job_id=p.job_id AND c.kind='control' AND c.seq>p.seq
			AND (c.data->>'action' IN ('instruction','retry','resume') OR c.data->>'instruction'='true'))
		)`, job.ID, jsonNumber(job.Generation), jsonNumber(job.LeaseEpoch), jsonNumber(*job.PR), *job.Branch, *job.HeadSHA).Scan(&recorded)
		if err != nil {
			return err
		}
		if recorded {
			return nil
		}
	}
	return addEvent(ctx, tx, job.ID, stringValue(job.LeaseHost), "running_publication", map[string]any{
		"generation": job.Generation, "epoch": job.LeaseEpoch, "pr": *job.PR, "branch": *job.Branch, "headSha": *job.HeadSHA,
	}, at)
}

// publishedImplementations reads complete intake history, including instructions sent
// during the current lease. publicationResume only describes queued publication
// recovery and is intentionally not used here.
func publishedImplementations(ctx context.Context, tx *sql.Tx, running []*Job) (map[int64]bool, error) {
	result := map[int64]bool{}
	ids := make([]int64, 0, len(running))
	byID := map[int64]*Job{}
	protected := map[int64]bool{}
	for _, job := range running {
		if !publicationTuple(job) {
			continue
		}
		ids = append(ids, job.ID)
		byID[job.ID] = job
		result[job.ID] = publicationPhase(job.View)
		protected[job.ID] = reimplementationExpected(job)
	}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT job_id,kind,data FROM job_events WHERE job_id=ANY($1::bigint[]) AND kind IN('control','state','running_publication') ORDER BY seq`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind string
		var raw []byte
		if err = rows.Scan(&id, &kind, &raw); err != nil {
			return nil, err
		}
		var data map[string]any
		if err = json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		replayPublicationEvent(byID[id], kind, data, protected, result)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, job := range running {
		// An implementation report always restores protection, including an
		// automatic repair within the same generation and lease.
		if protected[job.ID] || fleet.Text(job.View["phase"]) != "" && !publicationPhase(job.View) {
			result[job.ID] = false
		}
	}
	return result, nil
}

// replayPublicationEvent は出来事1件を seq の順に当て、再実装の保護と公開済みの印を更新する。
func replayPublicationEvent(job *Job, kind string, data map[string]any, protected, result map[int64]bool) {
	if kind == "control" {
		action := fleet.Text(data["action"])
		if action == "instruction" || data["instruction"] == true && oneOf(action, "retry", "resume") || action == "retry" && fleet.Object(job.View["reimplementExpected"])["reimplements"] != false {
			protected[job.ID] = true
		}
		return
	}
	if kind == "state" {
		// Review hold is a completed publication, unlike failure, retest or
		// scope wait. A later retry/instruction restores protection in seq order.
		epoch, ok := count(data["epoch"])
		if data["to"] == "held" && data["holdReason"] == "review_held" && (!reimplementationExpected(job) || ok && epoch == job.LeaseEpoch) {
			protected[job.ID] = false
		}
		return
	}
	generation, ok := count(data["generation"])
	if ok && generation <= job.Generation && (generation == job.Generation || !reimplementationExpected(job)) && data["pr"] == float64(*job.PR) && data["branch"] == *job.Branch && data["headSha"] == *job.HeadSHA {
		protected[job.ID] = false
		result[job.ID] = true
	}
}
