package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Scope widening has its own fallback, independent of the shared intake defaults.
const scopeFileLimit = 8

// Retain concrete requests when NormalizeScope collapses a request to a wildcard.
func scopeRequestedPaths(paths []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range paths {
		values, err := NormalizeScope([]string{raw})
		if err != nil || len(values) == 0 {
			continue
		}
		key := strings.Map(unicode.ToUpper, values[0])
		if !seen[key] {
			out = append(out, values[0])
			seen[key] = true
		}
	}
	return out
}

func scopeFileCount(paths []string) (int, []string) {
	seen := map[string]bool{}
	uncounted := []string{}
	count := 0
	for _, raw := range paths {
		normalized, err := NormalizeScope([]string{raw})
		if err != nil || len(normalized) == 0 {
			continue
		}
		p := normalized[0]
		key := strings.Map(unicode.ToUpper, p)
		if seen[key] {
			continue
		}
		seen[key] = true
		if p == "*" || strings.HasSuffix(p, "/") {
			uncounted = append(uncounted, p)
			continue
		}
		if IsTestScopePath(p) {
			continue
		}
		count++
	}
	return count, uncounted
}

func scopeLimitHistory(ctx context.Context, tx *sql.Tx, job *Job) (int, bool, error) {
	baseline := 0
	var saved sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT (data->>'baseline')::integer FROM job_events WHERE job_id=$1 AND kind IN ('scope_widened','scope_wait','scope_over_limit') AND data ? 'baseline' ORDER BY seq LIMIT 1`, job.ID).Scan(&saved)
	if err != nil && err != sql.ErrNoRows {
		return 0, false, err
	}
	if saved.Valid {
		baseline = int(saved.Int64)
	} else if job.ScopeSource == "declared" {
		var original []string
		var before []byte
		err := tx.QueryRowContext(ctx, `SELECT data->'before' FROM job_events WHERE job_id=$1 AND kind='scope_narrowed' ORDER BY seq LIMIT 1`, job.ID).Scan(&before)
		if err != nil && err != sql.ErrNoRows {
			return 0, false, err
		}
		if err == sql.ErrNoRows {
			original = job.Scope
		} else {
			// The first narrowing preserves the declaration before any additions.
			// Incomplete legacy history must not invent a filing-time count.
			if json.Unmarshal(before, &original) != nil || original == nil {
				original = nil
			}
		}
		baseline, _ = scopeFileCount(original)
	}
	var approved bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND kind='scope_over_limit_approved') OR EXISTS(SELECT 1 FROM job_events c WHERE c.job_id=$1 AND c.kind='control' AND c.data->>'action'='resume' AND c.data->>'instruction'='true' AND c.seq > COALESCE((SELECT max(seq) FROM job_events WHERE job_id=$1 AND kind='scope_over_limit'), 9223372036854775807))`, job.ID).Scan(&approved)
	return baseline, approved, err
}

func stopScopeOverLimit(ctx context.Context, tx *sql.Tx, job *Job, host string, extra []string, count, limit, baseline int, uncounted []string, at time.Time) error {
	shown := extra
	if len(shown) > 10 {
		shown = shown[:10]
	}
	suffix := ""
	if len(extra) > len(shown) {
		suffix = fmt.Sprintf("、他%d件", len(extra)-len(shown))
	}
	message := fmt.Sprintf("変更範囲が目安を超えました（%d件、上限%d件、起票時%d件）。理由を付けた再開が必要です。理由を付けて再開するか、閉じて分けてください。追加しようとしたファイル: %s%s。件数に含めない指定: %s。", count, limit, baseline, strings.Join(shown, "、"), suffix, strings.Join(uncounted, "、"))
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET status='stopped',hold_reason=NULL,lease_expires_at=NULL,result=(CASE WHEN jsonb_typeof(result)='object' THEN result ELSE '{}'::jsonb END) || jsonb_build_object('error',$2::text),updated_at=$3 WHERE id=$1`, job.ID, message, at)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE merge_turns SET holder_host=NULL,job_id=NULL,expires_at=NULL WHERE job_id=$1`, job.ID); err != nil {
		return err
	}
	data := map[string]any{"add": extra, "count": count, "limit": limit, "baseline": baseline, "uncounted": uncounted, "to": "stopped", "leaseHost": job.LeaseHost, "generation": job.Generation, "epoch": job.LeaseEpoch}
	if phase, ok := job.View["phase"]; ok {
		data["phase"] = phase
	}
	return addEvent(ctx, tx, job.ID, host, "scope_over_limit", data, at)
}
