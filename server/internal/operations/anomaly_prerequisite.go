package operations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/jobs"
)

// ObservePrerequisiteHoldCycle observes each current prerequisite hold once.
// An incomplete observation fails the cycle without preventing other jobs
// from being observed; the next cycle retries the failed observation.
func (s *Service) ObservePrerequisiteHoldCycle(ctx context.Context) error {
	if s == nil || s.PostgreSQL == nil {
		return fmt.Errorf("prerequisite observation store unavailable")
	}
	rows, err := s.PostgreSQL.QueryContext(ctx, `SELECT id FROM jobs WHERE status='held' AND (hold_reason='prerequisite_missing' OR (hold_reason='question' AND view->'prerequisiteMissingGenerations' @> jsonb_build_array(generation))) ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	now := s.now()
	var failures error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if _, _, err := s.ObservePrerequisiteHolds(ctx, id, now); err != nil {
			failures = errors.Join(failures, fmt.Errorf("prerequisite observation job %d: %w", id, err))
		}
	}
	return failures
}

// ObservePrerequisiteHolds reads saved job reports. It never retries a job.
// Historical events without cause fingerprints make the window incomplete and
// return an error, rather than attributing old holds to the current cause.
func (s *Service) ObservePrerequisiteHolds(ctx context.Context, jobID int64, now time.Time) (IncidentCause, bool, error) {
	if s.PostgreSQL == nil || jobID <= 0 || now.IsZero() {
		return IncidentCause{}, false, fmt.Errorf("invalid prerequisite observation")
	}
	tx, err := s.PostgreSQL.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return IncidentCause{}, false, err
	}
	defer tx.Rollback()
	generation, fingerprint, missingRaw, err := loadPrerequisiteHold(ctx, tx, jobID)
	if err != nil {
		return IncidentCause{}, false, err
	}
	prerequisite, err := prerequisiteStateHash(ctx, tx, missingRaw, fingerprint)
	if err != nil {
		return IncidentCause{}, false, err
	}
	holds, err := prerequisiteHoldHistory(ctx, tx, jobID, generation, fingerprint, now)
	if err != nil {
		return IncidentCause{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return IncidentCause{}, false, err
	}
	return s.EvaluateIncidentHolds(ctx, fmt.Sprintf("prerequisite:%d:%s", jobID, fingerprint), prerequisite, holds, now)
}

// loadPrerequisiteHold は依頼が前提の不足で止まっているかを確かめ、その世代・原因の指紋・不足の記録を返す。
func loadPrerequisiteHold(ctx context.Context, tx *sql.Tx, jobID int64) (int64, string, json.RawMessage, error) {
	var status string
	var hold sql.NullString
	var generation int64
	var resultRaw, viewRaw []byte
	err := tx.QueryRowContext(ctx, `SELECT status,hold_reason,generation,result,view FROM jobs WHERE id=$1`, jobID).Scan(&status, &hold, &generation, &resultRaw, &viewRaw)
	if err != nil {
		return 0, "", nil, err
	}
	var result struct {
		Status  string          `json:"status"`
		Missing json.RawMessage `json:"prerequisiteMissing"`
	}
	var view struct {
		Generations []int64 `json:"prerequisiteMissingGenerations"`
	}
	if err = json.Unmarshal(resultRaw, &result); err != nil {
		return 0, "", nil, err
	}
	if err = json.Unmarshal(viewRaw, &view); err != nil {
		return 0, "", nil, err
	}
	found := hold.Valid && hold.String == "prerequisite_missing"
	if hold.Valid && hold.String == "question" {
		for _, g := range view.Generations {
			if g == generation {
				found = true
			}
		}
	}
	if status != "held" || !found || result.Status != "failed" {
		return 0, "", nil, fmt.Errorf("job is not a prerequisite hold")
	}
	fingerprint, err := jobs.PrerequisiteFingerprint(result.Missing)
	if err != nil {
		return 0, "", nil, err
	}
	return generation, fingerprint, result.Missing, nil
}

// prerequisiteStateHash は不足した成果物を作る依頼の今の状態を、原因の指紋と合わせて1つの値にする。
func prerequisiteStateHash(ctx context.Context, tx *sql.Tx, missingRaw json.RawMessage, fingerprint string) (string, error) {
	var missing struct {
		Artifacts []struct {
			Repo  string `json:"repo"`
			Issue int64  `json:"issue"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(missingRaw, &missing); err != nil {
		return "", err
	}
	states := make([]string, 0, len(missing.Artifacts))
	for _, a := range missing.Artifacts {
		rows, e := tx.QueryContext(ctx, `SELECT id,status,COALESCE(head_sha,'') FROM jobs WHERE lower(repo)=lower($1) AND issue=$2 ORDER BY id`, a.Repo, a.Issue)
		if e != nil {
			return "", e
		}
		matches := 0
		for rows.Next() {
			var id int64
			var state, sha string
			if e = rows.Scan(&id, &state, &sha); e != nil {
				rows.Close()
				return "", e
			}
			b, _ := json.Marshal([]any{strings.ToLower(a.Repo), a.Issue, id, state, sha})
			states = append(states, string(b))
			matches++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return "", e
		}
		if matches == 0 {
			b, _ := json.Marshal([]any{strings.ToLower(a.Repo), a.Issue, "missing"})
			states = append(states, string(b))
		}
	}
	sort.Strings(states)
	b, _ := json.Marshal([]any{fingerprint, states})
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), nil
}

// prerequisiteHoldHistory は直近6時間の同じ原因での停止を集め、今の世代の停止の記録があることを確かめる。
func prerequisiteHoldHistory(ctx context.Context, tx *sql.Tx, jobID, generation int64, fingerprint string, now time.Time) ([]IncidentHold, error) {
	rows, err := tx.QueryContext(ctx, `SELECT seq,at,data FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='held' AND data->>'holdReason'='prerequisite_missing' AND at >= $2 AND at <= $3 ORDER BY seq`, jobID, now.Add(-6*time.Hour), now)
	if err != nil {
		return nil, err
	}
	var holds []IncidentHold
	current := false
	for rows.Next() {
		var seq int64
		var at time.Time
		var raw []byte
		if err = rows.Scan(&seq, &at, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var event struct {
			Cause      string `json:"prerequisiteCause"`
			Generation *int64 `json:"generation"`
		}
		if err = json.Unmarshal(raw, &event); err != nil {
			rows.Close()
			return nil, err
		}
		if event.Cause == "" || event.Generation == nil {
			rows.Close()
			return nil, fmt.Errorf("prerequisite hold history is incomplete")
		}
		if *event.Generation == generation && event.Cause == fingerprint {
			current = true
		}
		if event.Cause == fingerprint {
			holds = append(holds, IncidentHold{ID: fmt.Sprint(seq), At: at})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if !current {
		// A still-held job can have entered its current generation before the
		// window. Verify that event without counting it as a recent hold.
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='held' AND data->>'holdReason'='prerequisite_missing' AND at < $2 AND data->>'generation'=$3 AND data->>'prerequisiteCause'=$4)`, jobID, now.Add(-6*time.Hour), fmt.Sprint(generation), fingerprint).Scan(&current)
		if err != nil {
			return nil, err
		}
		if !current {
			return nil, fmt.Errorf("current prerequisite hold event unavailable")
		}
	}
	return holds, nil
}
