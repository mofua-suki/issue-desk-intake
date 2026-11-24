package proposals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/consultations"
)

// Scan includes closed Issues because manual merges may happen after closure.
// The durable snapshot skips unchanged comment collections across restarts and
// retains only candidates that still need merge/numbering/filing reconciliation.
func (s *Service) Scan(ctx context.Context) error {
	if s.o.Repositories == nil || s.o.Issues == nil || s.o.Comments == nil || s.o.Submit == nil {
		return fmt.Errorf("後続案の走査・起票の接続がありません")
	}
	s.scanOnce.Do(func() { s.scanSlot = make(chan struct{}, 1) })
	// Concurrent scans wait for the active scan, unless their context is canceled.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.scanSlot <- struct{}{}:
	}
	defer func() { <-s.scanSlot }()
	if err := ctx.Err(); err != nil {
		return err
	}
	saved, err := s.records.List(ctx, "proposal_scans")
	if err != nil {
		return err
	}
	snapshots := map[string]consultations.Record{}
	for _, row := range saved {
		if row["type"] == "review_followup_scan" {
			snapshots[str(row["id"])] = row
		}
	}
	failures := []error{}
	for _, repo := range s.o.Repositories() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.o.Project(repo); err != nil {
			failures = append(failures, err)
			continue
		}
		issues, err := s.o.Issues(ctx, repo, "all")
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, issue := range issues {
			if err := ctx.Err(); err != nil {
				return err
			}
			if issue["pull_request"] != nil || !validNumber(issue["number"]) {
				continue
			}
			n := num(issue["number"])
			key := fmt.Sprintf("review-followup:%s#%d", repo, n)
			if err := s.scanIssue(ctx, repo, n, issue, key, snapshots[key]); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (s *Service) saveScan(ctx context.Context, key string, value consultations.Record) error {
	_, err := s.records.Change(ctx, "proposal_scans", key, func(_ *sql.Tx, _ consultations.Record) (consultations.Record, error) {
		return value, nil
	})
	return err
}

// discoverProposals rereads the comments when the issue changed since the saved scan and
// saves the proposal comments still to process.
func (s *Service) discoverProposals(ctx context.Context, repo string, n int64, issue Object, key string, snapshot consultations.Record) (consultations.Record, error) {
	stamp := str(issue["updated_at"])
	commentCount := fmt.Sprint(issue["comments"])
	// A missing update timestamp is not evidence that comments are unchanged.
	if snapshot == nil || snapshot["headReconsiderationVersion"] != "1" || stamp == "" || snapshot["updatedAt"] != stamp || snapshot["commentCount"] != commentCount {
		comments, err := s.o.Comments(ctx, repo, n)
		if err != nil {
			return nil, err // Do not advance the cursor on a failed read.
		}
		pending := []any{}
		for _, comment := range comments {
			if strings.Contains(str(comment["body"]), "<!-- issue-desk-proposal") {
				pending = append(pending, comment)
			}
		}
		previous := snapshot
		snapshot = consultations.Record{"type": "review_followup_scan", "id": key, "repo": repo, "number": n,
			"updatedAt": stamp, "commentCount": commentCount, "pending": pending, "headReconsiderationVersion": "1"}
		if previous != nil {
			snapshot["headReportedComments"] = previous["headReportedComments"]
			snapshot["numberingReported"] = previous["numberingReported"]
		}
		// Persist discovery before sending anything or advancing the cursor.
		if err := s.saveScan(ctx, key, snapshot); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}
func (s *Service) scanIssue(ctx context.Context, repo string, n int64, issue Object, key string, snapshot consultations.Record) error {
	snapshot, err := s.discoverProposals(ctx, repo, n, issue, key, snapshot)
	if err != nil {
		return err
	}
	pending := arr(snapshot["pending"])
	remaining := []any{}
	failures := []error{}
	numberingPending := false
	reportedHeads := map[string]bool{}
	for _, id := range arr(snapshot["headReportedComments"]) {
		reportedHeads[str(id)] = true
	}
	remainingHeads := []any{}
	for _, raw := range pending {
		if err := ctx.Err(); err != nil {
			return err // Original pending collection remains durable.
		}
		retry, err := s.adopt(ctx, repo, n, obj(raw))
		if retry {
			remaining = append(remaining, raw)
		}
		if errors.Is(err, errFollowupNumbering) {
			numberingPending = true
			if snapshot["numberingReported"] == true {
				continue // Already reported and durable; retry without a log storm.
			}
		}
		if errors.Is(err, errWorkerHead) {
			id := strconv.FormatInt(num(obj(raw)["id"]), 10)
			remainingHeads = append(remainingHeads, id)
			if reportedHeads[id] {
				continue
			}
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	previousHeads := arr(snapshot["headReportedComments"])
	headsChanged := len(previousHeads) != len(remainingHeads) || (len(previousHeads) > 0 && !reflect.DeepEqual(previousHeads, remainingHeads))
	if len(remaining) != len(pending) || (snapshot["numberingReported"] == true) != numberingPending || headsChanged || snapshot["headReported"] != nil {
		snapshot["pending"] = remaining
		snapshot["numberingReported"] = numberingPending
		snapshot["headReportedComments"] = remainingHeads
		delete(snapshot, "headReported")
		if err := s.saveScan(ctx, key, snapshot); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
