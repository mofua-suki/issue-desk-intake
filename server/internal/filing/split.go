package filing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strconv"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
)

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

var splitPlanFields = []string{"executionPlan", "executionPlanParentSHA", "project", "title", "body", "provider", "consultationProvider", "priority", "changeScope", "dependsOnIssues", "_dependsOn", "heavyReason", "workType", "estimate", "imageSourceId", "completionPlanVersion", "completionPlan", "completionApproval", "completionReconfirmation", "completionReconfirmationAfter"}

func splitFingerprint(d Record) string {
	base := Record{}
	for _, key := range splitPlanFields {
		if value, ok := d[key]; ok {
			base[key] = value
		}
	}
	raw, _ := json.Marshal(base)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// sameFiledSplitChild は、起票済みの子が今の計画の同じ子（プロジェクトと題名が一致）かを返す。
func sameFiledSplitChild(stored, planned Record) bool {
	filed := fleet.Text(stored["status"]) == "submitted" || stored["issue"] != nil
	return filed && fleet.Text(stored["project"]) == fleet.Text(planned["project"]) && fleet.Text(stored["title"]) == fleet.Text(planned["title"])
}

func (s *Service) split(ctx context.Context, parent Record) (result Record, failure error) {
	if _, err := prepareCompletionPlan(parent); err != nil {
		return nil, err
	}
	if s.options.SplitPlan == nil {
		return nil, fail(503, "分割計画の検証を利用できません。")
	}
	var groups [][]string
	if s.options.RepositoryGroups != nil {
		var err error
		groups, err = s.options.RepositoryGroups(ctx)
		if err != nil {
			return nil, err
		}
	}
	var plan []Record
	var order []int
	var childConflict error
	originalParent := parent
	now := iso(s.options.Now())
	parent, err := s.options.Records.Change(ctx, "drafts", fleet.Text(parent["id"]), func(tx *sql.Tx, current Record) (Record, error) {
		revision, _ := integer(current["revision"])
		expected, _ := integer(parent["revision"])
		if current == nil || revision != expected || current["status"] != parent["status"] {
			return nil, fail(409, "分割する相談が更新されています。")
		}
		existingIDs := fleet.Items(current["splitChildren"])
		storedChildren, err := loadSplitChildren(ctx, tx, current, existingIDs)
		if err != nil {
			return nil, err
		}
		var ids []any
		parent, err = correctSplitDependencies(current, storedChildren, now)
		if err != nil {
			return nil, err
		}
		plan, err = s.options.SplitPlan(parent)
		if err != nil {
			return nil, err
		}
		if len(plan) == 0 || len(plan) > 25 {
			return nil, fail(400, "分割案の件数を確認してください。")
		}
		if len(existingIDs) > 0 && len(existingIDs) != len(plan) {
			return nil, fail(409, "保存済みの分割計画と件数が一致しません。")
		}
		ids, err = s.prepareSplitChildren(ctx, parent, plan, existingIDs, now)
		if err != nil {
			return nil, err
		}
		order, err = splitOrder(plan)
		if err != nil {
			return nil, err
		}
		if childConflict, err = saveSplitChildren(ctx, tx, plan, storedChildren, now); err != nil {
			return nil, err
		}
		next := clone(parent)
		next["splitChildren"], next["status"], next["updatedAt"] = ids, "submitting", now
		if current["_groupLinkGroups"] == nil {
			next["_groupLinkGroups"] = activeLinkGroups(groups, plan)
		}
		return next, nil
	})
	if err != nil {
		if childConflict != nil {
			if saveErr := s.saveSplitConflict(ctx, originalParent, childConflict); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, err
	}
	defer func() {
		if failure != nil && !errors.Is(failure, ErrFilingReadUnavailable) {
			if stopped, ok := s.recordSplitFailure(ctx, parent, failure); ok {
				result = stopped
			}
		}
	}()
	issues, keepParent, err := s.fileSplitChildren(ctx, parent, plan, order)
	if err != nil {
		if keepParent {
			return parent, err
		}
		return nil, err
	}
	complete, err := s.change(ctx, parent, func(next Record) {
		next["status"], next["issues"], next["_groupLinksComplete"] = "submitted", issues, true
		consultations.SetDraftError(next, nil, nil)
	})
	if err == nil {
		s.emit(complete)
		for _, index := range order {
			issue := row(issues[index])
			repo := fleet.Text(issue["project"])
			if repo == "" {
				repo = fleet.Text(plan[index]["project"])
			}
			number, ok := integer(issue["number"])
			if repo == "" || !ok || number < 1 {
				contract.LogFailure("分割後の配布再開 "+fleet.Text(parent["id"]), errors.New("分割したIssueを確認できません"))
				continue
			}
			key := repo + "#" + strconv.FormatInt(number, 10)
			contract.LogFailure("分割後の配布再開 "+key, s.filePendingKey(ctx, repo, key))
		}
	}
	return complete, err
}

// saveSplitChildren inserts the planned children that are not saved yet and refreshes the
// saved ones that may still change. A saved child that changed after it was sent is
// returned as the conflict, which is also the error.
func saveSplitChildren(ctx context.Context, tx *sql.Tx, plan []Record, storedChildren map[string]splitChildSnapshot, now string) (conflict error, err error) {
	for _, child := range plan {
		id := fleet.Text(child["id"])
		stored := storedChildren[id].record
		if stored != nil {
			if stored["finishedWithoutIssue"] == true {
				return nil, fail(409, "起票せずに終えた子があるため再送できません。残りの子を終えてください。")
			}
			changed := splitChildChanged(stored, child)
			// 本文の生成が変わっただけで起票済みの子の指紋は変わる。同じ子なら止めず、未起票の子の起票へ進む。
			if changed && !refreshableSplitChild(stored) && !sameFiledSplitChild(stored, child) {
				conflict = changedSplitChildConflict(id, stored)
				return conflict, conflict
			}
			if refreshableSplitChild(stored) && (changed || stored["splitBackground"] != child["splitBackground"] || stored["status"] == "error" || stored["status"] == "stalled") {
				if err := refreshSplitChild(ctx, tx, id, stored, storedChildren[id].raw, child, now); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := insertSplitChild(ctx, tx, id, child); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// fileSplitChildren files the children in dependency order and links the issues of a
// repository group. It returns the issues in plan order. keepParent tells that the parent
// is still the current record after the error.
func (s *Service) fileSplitChildren(ctx context.Context, parent Record, plan []Record, order []int) (issues []any, keepParent bool, err error) {
	filed := map[int]Record{}
	for _, index := range order {
		issue, keep, err := s.fileSplitChild(ctx, parent, plan[index], filed)
		if err != nil {
			return nil, keep, err
		}
		filed[index] = issue
	}
	if err := s.linkRepositoryGroupIssues(ctx, parent, plan, filed); err != nil {
		return nil, true, err
	}
	issues = []any{}
	for index := range plan {
		issues = append(issues, filed[index])
	}
	return issues, false, nil
}

// recordSplitFailure saves the failure of filing the split children on the parent. It
// returns the stopped parent when the failure was saved.
func (s *Service) recordSplitFailure(ctx context.Context, parent Record, failure error) (Record, bool) {
	stopped, err := s.change(ctx, parent, func(next Record) {
		failed := consultations.AtStage("分割した依頼の起票", failure)
		next["status"] = "submit_error"
		consultations.SetDraftError(next, consultations.PublicFailure(parent, failed, "分割した依頼の起票を完了できませんでした。各子の結果を確認してください。"), nil)
		next["lastFailure"] = consultations.FailureRecord(iso(s.options.Now()), parent, failed, fleet.Text(next["error"]))
	})
	contract.LogFailure("分割した依頼の起票 "+fleet.Text(parent["id"]), failure)
	contract.LogFailure("分割の失敗の保存 "+fleet.Text(parent["id"]), err)
	if err != nil {
		return nil, false
	}
	s.emit(stopped)
	return stopped, true
}
func splitOrder(plan []Record) ([]int, error) {
	state := make([]int, len(plan))
	order := []int{}
	var visit func(int) error
	visit = func(index int) error {
		if state[index] == 1 {
			return fail(400, "分割案の依存が循環しています。")
		}
		if state[index] == 2 {
			return nil
		}
		state[index] = 1
		for _, raw := range fleet.Items(plan[index]["_dependsOn"]) {
			n, ok := integer(raw)
			if !ok || n < 1 || n > int64(len(plan)) {
				return fail(400, "分割案の依存番号が不正です。")
			}
			if err := visit(int(n) - 1); err != nil {
				return err
			}
		}
		state[index] = 2
		order = append(order, index)
		return nil
	}
	for index := range plan {
		if err := visit(index); err != nil {
			return nil, err
		}
	}
	return order, nil
}
func (s *Service) bindSplitDependencies(ctx context.Context, child Record, filed map[int]Record) (Record, error) {
	dependencies := fleet.Items(child["dependencies"])
	if base, ok := child["_splitBaseDependencies"]; ok {
		dependencies = fleet.Items(base)
	}
	for _, raw := range fleet.Items(child["_dependsOn"]) {
		n, ok := integer(raw)
		if !ok {
			return nil, fail(400, "依存番号が不正です。")
		}
		issue := filed[int(n)-1]
		number, ok := integer(issue["number"])
		if !ok {
			return nil, fail(409, "先行する分割案の起票が終わっていません。")
		}
		if fleet.Text(issue["project"]) == fleet.Text(child["project"]) {
			dependencies = append(dependencies, number)
		} else {
			dependencies = append(dependencies, Record{"repo": issue["project"], "number": number})
		}
	}
	return s.change(ctx, child, func(next Record) { next["dependencies"] = dependencies })
}

// refreshableSplitChild accepts failed, never-sent children without weakening the
// protection used by dependency correction and submission reconciliation.
func refreshableSplitChild(child Record) bool {
	copy := clone(child)
	if copy["status"] == "error" || copy["status"] == "stalled" {
		copy["status"] = "draft"
	}
	return !protectedSplitChild(copy)
}

// prepareSplitChildren gives each planned child its ID and initial draft fields, and checks
// its project and the filing policy.
func (s *Service) prepareSplitChildren(ctx context.Context, parent Record, plan []Record, existingIDs []any, now string) ([]any, error) {
	ids := []any{}
	for index, child := range plan {
		child = clone(child)
		plan[index] = child
		id := ""
		if len(existingIDs) > 0 {
			id = fleet.Text(existingIDs[index])
		} else {
			var err error
			id, err = newID()
			if err != nil {
				return nil, err
			}
		}
		if id == "" {
			return nil, fail(409, "保存済みの分割先IDを読み取れません。")
		}
		child["id"], child["parentSplit"], child["splitIndex"] = id, parent["id"], index+1
		child["status"], child["ready"], child["revision"], child["createdAt"], child["updatedAt"] = "draft", true, 0, now, now
		child["questions"] = []any{}
		if child["messages"] == nil {
			child["messages"] = []any{}
		}
		if parent["owner"] != nil {
			child["owner"] = parent["owner"]
		}
		child["_splitBaseDependencies"] = child["dependencies"]
		child["_splitFingerprint"] = splitFingerprint(child)
		ids = append(ids, id)
		if err := s.options.Project(fleet.Text(child["project"])); err != nil {
			return nil, err
		}
		if err := s.policy(ctx, child); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// splitChildChanged reports whether the saved child was generated from a different plan.
func splitChildChanged(stored, child Record) bool {
	fingerprint := fleet.Text(stored["_splitFingerprint"])
	return fingerprint != "" && fingerprint != fleet.Text(child["_splitFingerprint"])
}

// changedSplitChildConflict explains why a saved child whose plan changed blocks the resend.
func changedSplitChildConflict(id string, stored Record) error {
	action := "子の送信結果を確認してください。"
	switch fleet.Text(stored["status"]) {
	case "submitting", "submit_unknown":
		action = "起票結果を確認してから対応してください。"
	case "submitted":
		action = "起票済みの子は計画を変えられません。"
	}
	return fail(409, fmt.Sprintf("保存済みの分割計画が変わっています。子ID: %s、題名: %s、状態: %s。%s", id, fleet.Text(stored["title"]), fleet.Text(stored["status"]), action))
}

// refreshSplitChild replaces a saved unfiled child with the current plan when it is unchanged in storage.
func refreshSplitChild(ctx context.Context, tx *sql.Tx, id string, stored Record, raw string, child Record, now string) error {
	preserveSplitBaseDependencies(stored, child)
	// Refresh the saved presentation/reply together with its receipt.
	// The pending boundary indexes this history, not the child's old rows.
	for _, key := range append(append([]string{}, splitPlanFields...), "messages", "splitBackground", "dependencies", "_splitBaseDependencies", "_splitFingerprint") {
		if value, ok := child[key]; ok {
			stored[key] = value
		} else {
			delete(stored, key)
		}
	}
	revision, _ := integer(stored["revision"])
	stored["status"], stored["ready"], stored["revision"], stored["updatedAt"] = "draft", true, revision+1, now
	consultations.SetDraftError(stored, nil, nil)
	delete(stored, "lastFailure")
	for _, key := range []string{"stalled", "run", "retryAt", "queued"} {
		delete(stored, key)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, bindSQL("UPDATE drafts SET json=? WHERE id=? AND json=?"), string(encoded), id, raw)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fail(409, "分割先の相談が更新されています。")
	}
	return nil
}

// insertSplitChild saves a new child draft.
func insertSplitChild(ctx context.Context, tx *sql.Tx, id string, child Record) error {
	encoded, err := json.Marshal(child)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), id, string(encoded)); err != nil {
		return err
	}
	return nil
}

// saveSplitConflict stops the parent with the conflict between the saved and the current plan.
func (s *Service) saveSplitConflict(ctx context.Context, originalParent Record, childConflict error) error {
	_, saveErr := s.change(ctx, originalParent, func(next Record) {
		failed := consultations.AtStage("分割した依頼の起票", childConflict)
		next["status"] = "submit_error"
		consultations.SetDraftError(next, consultations.PublicFailure(originalParent, failed, ""), nil)
		next["lastFailure"] = consultations.FailureRecord(iso(s.options.Now()), originalParent, failed, fleet.Text(next["error"]))
	})
	return saveErr
}

// fileSplitChild files one child unless it is already filed and returns its issue.
// keepParent tells the caller to return the parent with the error, as for failures after
// the child's filing began.
func (s *Service) fileSplitChild(ctx context.Context, parent, child Record, filed map[int]Record) (issue Record, keepParent bool, err error) {
	stored, found, err := s.options.Records.Get(ctx, "drafts", fleet.Text(child["id"]))
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, fail(409, "分割先の相談がありません。")
	}
	if fleet.Text(stored["status"]) != "submitted" && stored["issue"] == nil {
		if !protectedSplitChild(stored) {
			stored, err = s.bindSplitDependencies(ctx, stored, filed)
			if err != nil {
				return nil, true, err
			}
		}
		if s.options.BeforeSplitChild != nil {
			issues := []any{}
			for _, entry := range filed {
				issues = append(issues, entry)
			}
			stored, err = s.options.BeforeSplitChild(ctx, parent, stored, issues)
			if err != nil {
				return nil, false, err
			}
		}
		stored, err = s.normal(ctx, stored, fleet.Text(stored["status"]) == "submitting" || fleet.Text(stored["status"]) == "submit_unknown" || stored["attempted"] == true)
		if err != nil {
			return nil, true, err
		}
	}
	issue = row(stored["issue"])
	if issue == nil {
		return nil, true, fail(409, "分割案の起票結果を確認できません。")
	}
	return issue, false, nil
}
