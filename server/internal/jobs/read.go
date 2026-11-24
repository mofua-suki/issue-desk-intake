package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"issue-desk-intake/server/internal/contract"
)

func (s *Store) List(ctx context.Context, repo string, statuses []string, limit, offset int) ([]*Job, int, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	for _, status := range statuses {
		if !oneOf(status, "queued", "running", "held", "failed", "stopped", "merged", "closed") {
			return nil, 0, contract.NewProblem(400, "statusの形が違います。", "")
		}
	}
	var repoValue any
	if repo != "" {
		repoValue = repo
	}
	var statusValue any
	if len(statuses) > 0 {
		statusValue = statuses
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	const filter = " WHERE ($1::text IS NULL OR lower(repo)=lower($1)) AND ($2::text[] IS NULL OR status=ANY($2::text[]))"
	var total int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM jobs"+filter, repoValue, statusValue).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs"+filter+" ORDER BY priority DESC,filed_at,id LIMIT $3 OFFSET $4", repoValue, statusValue, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	list := []*Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		list = append(list, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (s *Store) Detail(ctx context.Context, id int64) (map[string]any, error) {
	job, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT depends_on FROM job_deps WHERE job_id=$1 ORDER BY depends_on", id)
	if err != nil {
		return nil, err
	}
	deps := []int64{}
	for rows.Next() {
		var dep int64
		if err = rows.Scan(&dep); err != nil {
			rows.Close()
			return nil, err
		}
		deps = append(deps, dep)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT seq,job_id,host,kind,at,data FROM(SELECT * FROM job_events WHERE job_id=$1 ORDER BY seq DESC LIMIT 100)e ORDER BY seq", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []any{}
	for rows.Next() {
		var seq, jobID int64
		var host *string
		var kind string
		var at time.Time
		var raw []byte
		if err = rows.Scan(&seq, &jobID, &host, &kind, &at, &raw); err != nil {
			return nil, err
		}
		var data any
		if err = json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		if fields, ok := data.(map[string]any); ok && kind == "state" && fields["holdReason"] == "prerequisite_missing" && fields["prerequisiteCause"] != nil {
			delete(fields, "prerequisiteCause")
			delete(fields, "generation")
		}
		events = append(events, map[string]any{"seq": seq, "jobId": jobID, "host": host, "kind": kind, "at": iso(at), "data": data})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"job": job, "dependsOn": deps, "events": events}, nil
}

func (s *Store) DependencyMerges(ctx context.Context, id int64) ([]any, error) {
	return dependencyMerges(ctx, s.db, id)
}

func dependencyMerges(ctx context.Context, db transferQuery, id int64) ([]any, error) {
	rows, err := db.QueryContext(ctx, "SELECT p.repo,p.issue,p.status,p.pr,p.result,j.view->'dependencyMerges'->(p.id::text) FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id WHERE d.job_id=$1 AND "+DependencyPairReadySQL+" ORDER BY p.repo,p.issue", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []any{}
	for rows.Next() {
		var repo, status string
		var issue int64
		var pr *int64
		var raw, evidenceRaw []byte
		if err = rows.Scan(&repo, &issue, &status, &pr, &raw, &evidenceRaw); err != nil {
			return nil, err
		}
		var result map[string]any
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &result); err != nil {
				return nil, err
			}
		}
		var sha any
		if value, ok := result["mergeCommit"].(string); ok {
			sha = value
		}
		if oneOf(status, "closed", "failed") && len(evidenceRaw) > 0 {
			var evidence struct {
				PR          int64  `json:"pr"`
				MergeCommit string `json:"mergeCommit"`
			}
			if json.Unmarshal(evidenceRaw, &evidence) == nil && evidence.PR > 0 && mergeCommitPattern.MatchString(evidence.MergeCommit) {
				pr = &evidence.PR
				sha = evidence.MergeCommit
				if status == "failed" {
					status = "merged"
				}
			}
		}
		list = append(list, map[string]any{"repo": repo, "number": issue, "status": status, "prNumber": pr, "mergeCommitSha": sha})
	}
	return list, rows.Err()
}

func (s *Store) Sweep(ctx context.Context, exempt func(string) []string) error {
	at := s.options.Now()
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := expireLeases(ctx, tx, at); err != nil {
			return err
		}
		if err := requeueScopeWaits(ctx, tx, exempt, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE merge_turns SET holder_host=NULL,job_id=NULL,request_id=NULL,expires_at=NULL WHERE holder_host IS NOT NULL AND expires_at<=$1", at)
		return err
	})
}

// expireLeases は lease の期限が切れた実行中の依頼を待ちへ戻す。同じ世代で3回続いたら失敗にする。
func expireLeases(ctx context.Context, tx *sql.Tx, at time.Time) error {
	expired, err := dispatchJobs(ctx, tx, "WHERE status='running' AND lease_expires_at<=$1 FOR UPDATE", at)
	if err != nil {
		return err
	}
	for _, job := range expired {
		var failures int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='lease_expired' AND (data->>'generation')::bigint=$2", job.ID, job.Generation).Scan(&failures); err != nil {
			return err
		}
		status := "queued"
		var result any
		if failures+1 >= 3 {
			status = "failed"
			result, err = leaseFailureResult(ctx, tx, job, failures)
			if err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET status=$2,lease_expires_at=NULL,updated_at=$3,result=CASE WHEN $4::jsonb IS NULL THEN result ELSE (CASE WHEN jsonb_typeof(result)='object' THEN result ELSE '{}'::jsonb END - 'lastStage') || $4::jsonb END WHERE id=$1", job.ID, status, at, result); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE merge_turns SET holder_host=NULL,job_id=NULL,expires_at=NULL WHERE job_id=$1", job.ID); err != nil {
			return err
		}
		diagnostic := map[string]any{"generation": job.Generation, "to": status, "leaseHost": job.LeaseHost, "epoch": job.LeaseEpoch, "expiresAt": job.LeaseExpiresAt}
		if phase, ok := job.View["phase"]; ok {
			diagnostic["phase"] = phase
		}
		if err = addEvent(ctx, tx, job.ID, "", "lease_expired", diagnostic, at); err != nil {
			return err
		}
	}
	return nil
}

// leaseFailureResult は lease 切れが続いて失敗にする依頼の結果を、最後のPC・段・延長と報告の断りの記録とともに組み立てる。
func leaseFailureResult(ctx context.Context, tx *sql.Tx, job *Job, failures int) (any, error) {
	host := "不明"
	if job.LeaseHost != nil && *job.LeaseHost != "" {
		host = *job.LeaseHost
	}
	failure := map[string]any{
		"status": "failed", "summary": fmt.Sprintf("lease の期限切れが同じ世代で%d回続いたため失敗にしました。最後のPC: %s", failures+1, host),
		"leaseExpiredCount": failures + 1, "lastHost": host,
	}
	if stage, ok := job.View["phase"].(string); ok && stage != "" {
		failure["lastStage"] = stage
	}
	if executionError, ok := job.View["lastExecutionError"].(map[string]any); ok {
		failure["lastExecutionError"] = executionError
	}
	diagnostics := map[string]any{"lastRenewal": nil, "lastRenewalMissing": "no matching renewal", "lastReportRejection": nil, "lastReportRejectionMissing": "no matching rejection"}
	var renewal []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM job_events WHERE job_id=$1 AND kind='lease_renewed' AND data->>'generation'=$2 AND data->>'epoch'=$3 ORDER BY seq DESC LIMIT 1`, job.ID, jsonNumber(job.Generation), jsonNumber(job.LeaseEpoch)).Scan(&renewal)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		var value map[string]any
		if err = json.Unmarshal(renewal, &value); err != nil {
			return nil, err
		}
		diagnostics["lastRenewal"] = value
		diagnostics["lastRenewalMissing"] = nil
	}
	var rejection []byte
	err = tx.QueryRowContext(ctx, `SELECT data->'diagnostic' FROM job_events WHERE job_id=$1 AND kind='report_rejected' AND data->'diagnostic'->>'generation'=$2 ORDER BY (data->'diagnostic'->>'lastAt')::timestamptz DESC,seq DESC LIMIT 1`, job.ID, jsonNumber(job.Generation)).Scan(&rejection)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		var value map[string]any
		if err = json.Unmarshal(rejection, &value); err != nil {
			return nil, err
		}
		diagnostics["lastReportRejection"] = value
		diagnostics["lastReportRejectionMissing"] = nil
	}
	failure["leaseDiagnostics"] = diagnostics
	return jsonValue(failure)
}

// requeueScopeWaits は範囲の重なる実行中の依頼が無くなった範囲待ちの依頼を、待ちへ戻す。
func requeueScopeWaits(ctx context.Context, tx *sql.Tx, exempt func(string) []string, at time.Time) error {
	waiting, err := dispatchJobs(ctx, tx, "WHERE status='held' AND hold_reason='scope_wait' FOR UPDATE")
	if err != nil {
		return err
	}
	busy, err := dispatchJobs(ctx, tx, "WHERE status='running'")
	if err != nil {
		return err
	}
	for _, job := range waiting {
		blocked := false
		paths := []string{}
		if exempt != nil {
			paths = exempt(job.Repo)
		}
		for _, other := range busy {
			if other.Repo == job.Repo && ScopeOverlap(job.Scope, other.Scope, paths) {
				blocked = true
			}
		}
		if !blocked {
			if _, err = tx.ExecContext(ctx, "UPDATE jobs SET status='queued',hold_reason=NULL,updated_at=$2 WHERE id=$1", job.ID, at); err != nil {
				return err
			}
			if err = addEvent(ctx, tx, job.ID, "", "requeued", map[string]any{"from": "scope_wait"}, at); err != nil {
				return err
			}
		}
	}
	return nil
}
