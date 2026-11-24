package jobs

import (
	"context"
	"database/sql"
	"strings"
	"unicode"

	"issue-desk-intake/server/internal/contract"
)

// Narrow replaces the current scope only with paths already covered by it.
func (s *Store) Narrow(ctx context.Context, id int64, host string, epoch int64, paths []string) (*ScopeResult, error) {
	if len(paths) == 0 || len(paths) > 200 {
		return nil, contract.NewProblem(400, "actualは1〜200件のパスの配列です。", "")
	}
	for _, path := range paths {
		if path == "*" {
			return nil, contract.NewProblem(400, "actualに*は指定できません。", "")
		}
	}
	actual, err := NormalizeScope(paths)
	if err != nil {
		return nil, err
	}
	at := s.options.Now()
	var result *ScopeResult
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		// Narrow only removes coverage, so it cannot introduce a conflict and
		// does not need the repository advisory lock used by Widen and dispatch.
		// The fenced row lock still serializes changes to this job.
		job, err := fenced(ctx, tx, id, host, epoch, at, false)
		if err != nil {
			return err
		}
		for _, path := range actual {
			covered := false
			upper := strings.Map(unicode.ToUpper, path)
			for _, current := range job.Scope {
				prefix := strings.Map(unicode.ToUpper, current)
				if current == "*" || prefix == upper || strings.HasSuffix(current, "/") && strings.HasPrefix(upper, prefix) {
					covered = true
					break
				}
			}
			if !covered {
				return contract.NewProblem(400, "actualは現在の範囲内にしてください。", "")
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET scope=$2::text[],updated_at=$3 WHERE id=$1", id, actual, at); err != nil {
			return err
		}
		if err = addEvent(ctx, tx, id, host, "scope_narrowed", map[string]any{"before": job.Scope, "after": actual, "actual": paths}, at); err != nil {
			return err
		}
		job, err = readJob(ctx, tx, id, false)
		if err != nil {
			return err
		}
		result = &ScopeResult{Result: "granted", Job: job}
		return nil
	})
	return result, err
}
