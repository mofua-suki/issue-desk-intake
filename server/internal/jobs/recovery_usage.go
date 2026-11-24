package jobs

import (
	"context"
	"database/sql"
)

// RecoveryUsageReason applies exactly the retry admission usage check.
func RecoveryUsageReason(ctx context.Context, db *sql.DB, job *Job, options ControlOptions) (string, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	reason := (&Store{}).controlUsage(ctx, tx, job, options)
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return reason, nil
}
