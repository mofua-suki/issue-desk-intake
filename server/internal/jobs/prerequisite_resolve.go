package jobs

import (
	"context"
	"database/sql"

	"issue-desk-intake/server/internal/contract"
)

// ConnectPrerequisiteTx connects a producer and requeues the still-current prerequisite wait.
// expectedGeneration fences a candidate selected before the transaction began.
// The caller persists resolution metadata in the same transaction.
// The caller owns the transaction and must roll it back on error.
func (s *Store) ConnectPrerequisiteTx(ctx context.Context, tx *sql.Tx, id, expectedGeneration, producerID int64) error {
	if err := lockDependencyGraph(ctx, tx); err != nil {
		return err
	}
	job, err := readJob(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if job.Status != "held" || !PrerequisiteMissingHeld(job) || job.Generation != expectedGeneration {
		return contract.NewProblem(409, "前提待ちの状態または世代が変わりました。", "prerequisite_changed")
	}
	all, err := readTransferGraph(ctx, tx)
	if err != nil {
		return err
	}
	found := false
	for _, item := range all {
		if item.ID == producerID {
			found = true
			break
		}
	}
	if !found {
		return contract.NewProblem(404, "作り手のジョブが見つかりません。", "")
	}
	if _, err = addExplicitDependenciesFromGraph(ctx, tx, job, []int64{producerID}, newDependencyIndex(all)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET status='queued',hold_reason=NULL,generation=generation+1,updated_at=$2 WHERE id=$1`, id, s.options.Now())
	return err
}
