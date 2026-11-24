package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
)

// ReconcileSplitParent rechecks one saved family. The caller supplies the filing
// activity check and the normal draft event publisher; periodic scheduling is
// deliberately independent of this operation.
func (s *Service) ReconcileSplitParent(ctx context.Context, id string, filingBusy func(string) bool, publish func(Record)) (bool, error) {
	if filingBusy == nil || publish == nil {
		return false, errors.New("split reconciliation requires filing activity and event callbacks")
	}
	parent, found, err := s.options.Records.Get(ctx, "drafts", id)
	if err != nil || !found {
		return false, err
	}
	ids := fleet.Items(parent["splitChildren"])
	if len(ids) == 0 {
		return false, nil
	}
	keys, distinct := splitFamilyKeys(id, ids)
	if !distinct {
		return false, nil
	}
	tx, err := s.options.Records.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	members, err := lockFamilyDraftsTx(ctx, tx, keys)
	if err != nil || members == nil {
		return false, err
	}
	parent = members[id]
	children, settled := settledSplitChildren(id, parent, ids, members)
	if !settled {
		return false, nil
	}
	// Inspect activity after obtaining the database locks as well as before writing.
	for _, key := range keys {
		if s.busy(members[key]) || filingBusy(key) {
			return false, nil
		}
	}
	next := familyIssues(parent, children)
	entries, _ := next["issues"].([]Record)
	unique, valid := uniqueFamilyIssues(entries)
	if !valid {
		return false, nil
	}
	next["issues"] = unique
	if len(unique) > 0 {
		next["issue"] = unique[0]
	} else {
		delete(next, "issue")
	}
	revision, _ := integer(parent["revision"])
	next["status"], next["revision"], next["updatedAt"] = "submitted", revision+1, timestamp(s.options.Now())
	SetDraftError(next, nil, nil)
	if err := updateSplitParentTx(ctx, tx, id, next); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	publish(Record{"type": "draft", "id": id, "revision": revision + 1})
	return true, nil
}

// splitFamilyKeys returns the sorted IDs of the parent and its children, and false when a
// child ID is empty or repeated.
func splitFamilyKeys(id string, ids []any) ([]string, bool) {
	keys := []string{id}
	seen := map[string]bool{id: true}
	for _, raw := range ids {
		childID := text(raw)
		if childID == "" || seen[childID] {
			return nil, false
		}
		seen[childID] = true
		keys = append(keys, childID)
	}
	sort.Strings(keys)
	return keys, true
}

// lockFamilyDraftsTx locks and reads every family draft in key order. It returns nil
// members without an error when one of them no longer exists.
func lockFamilyDraftsTx(ctx context.Context, tx *sql.Tx, keys []string) (map[string]Record, error) {
	members := make(map[string]Record, len(keys))
	for _, key := range keys {
		var raw string
		err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=? FOR UPDATE"), key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		members[key], err = decode(raw)
		if err != nil {
			return nil, err
		}
	}
	return members, nil
}

// settledSplitChildren returns the children in plan order when the failed parent's family
// is unchanged and every child has finished filing.
func settledSplitChildren(id string, parent Record, ids []any, members map[string]Record) ([]Record, bool) {
	currentIDs := fleet.Items(parent["splitChildren"])
	plans := fleet.Items(parent["splitIssues"])
	if text(parent["id"]) != id || parent["status"] != "submit_error" || len(currentIDs) != len(ids) || len(plans) != len(ids) || linksPending(parent) {
		return nil, false
	}
	children := make([]Record, len(ids))
	for i, raw := range ids {
		childID := text(raw)
		if text(currentIDs[i]) != childID || record(plans[i]) == nil {
			return nil, false
		}
		c := members[childID]
		if c == nil || text(c["id"]) != childID || text(c["parentSplit"]) != id || text(c["owner"]) != text(parent["owner"]) || linksPending(c) || is(text(c["status"]), "thinking", "submitting", "auto_ready", "submit_unknown", "closed") {
			return nil, false
		}
		if !(c["status"] == "submitted" && c["finishedWithoutIssue"] == true) {
			childIssues, _ := familyIssues(Record{}, []Record{c})["issues"].([]Record)
			if !filedChild(c) || len(childIssues) == 0 {
				return nil, false
			}
		}
		children[i] = c
	}
	return children, true
}

// uniqueFamilyIssues drops repeated issues, and returns false when an issue lacks its project or number.
func uniqueFamilyIssues(entries []Record) ([]Record, bool) {
	unique := []Record{}
	encountered := map[string]map[int64]bool{}
	for _, issue := range entries {
		project := strings.ToLower(text(issue["project"]))
		number, ok := integer(issue["number"])
		if !ok || number <= 0 || project == "" {
			return nil, false
		}
		if encountered[project] == nil {
			encountered[project] = map[int64]bool{}
		}
		if !encountered[project][number] {
			unique = append(unique, issue)
			encountered[project][number] = true
		}
	}
	return unique, true
}

// updateSplitParentTx saves the reconciled parent in the locking transaction.
func updateSplitParentTx(ctx context.Context, tx *sql.Tx, id string, next Record) error {
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, bindSQL("UPDATE drafts SET json=? WHERE id=?"), string(encoded), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("split parent update lost")
	}
	return nil
}

func linksPending(d Record) bool {
	return len(fleet.Items(d["_groupLinkGroups"])) > 0 && d["_groupLinksComplete"] != true
}
