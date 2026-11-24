package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
)

// Notification coverage (the column comparison lives in migration 34):
//
// Path                              Source of list changes
// Create                            jobs INSERT, job_deps INSERT
// Next                              jobs lease/status/view, dispatch history
// Renew                             view except progress (lease times excluded)
// Report                            status/hold/tier/branch/pr/head/view/result
// Control, PermissionApproval/Receipt status/hold/generation/priority/view
// Widen, Narrow                     scope/source/status/hold/result
// Sweep                             status/result, held jobs returned to queued
// AddDependencies, ResolveFilingReferences job_deps edges, unresolved view
// RefreshTransfers, ReconcileTransferRecords, ResolveTransfer view/job_deps
// ConnectPrerequisiteTx, CheckAutomaticUsage status/generation/view
// Event                             list-visible dispatch history kinds
//
// Merge turns, repo notes and non-list job_events do not change list fields.
// The trigger only collects identifiers when this Store enables collection.
// It uses transaction-local state, including savepoint rollback, without a
// durable queue or a scan of all jobs. Jobs written outside Store.transaction
// must arrange their own post-commit publication.
type issueChange struct {
	Project string `json:"project"`
	Number  int64  `json:"number"`
}

func (s *Store) jobChanges(ctx context.Context, tx *sql.Tx) ([]issueChange, error) {
	if s.options.Publish == nil {
		return nil, nil
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, "SELECT current_setting('issue_desk.job_changes')::jsonb").Scan(&raw); err != nil {
		return nil, err
	}
	var byID map[string]issueChange
	if err := json.Unmarshal(raw, &byID); err != nil {
		return nil, err
	}
	unique := map[issueChange]bool{}
	changes := make([]issueChange, 0, len(byID))
	for _, change := range byID {
		if !unique[change] {
			unique[change] = true
			changes = append(changes, change)
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Project != changes[j].Project {
			return changes[i].Project < changes[j].Project
		}
		return changes[i].Number < changes[j].Number
	})
	return changes, nil
}
