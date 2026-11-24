package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/splitplan"
)

type splitChildSnapshot struct {
	record Record
	raw    string
}

func protectedSplitChild(child Record) bool {
	status := fleet.Text(child["status"])
	return status != "draft" && status != "submit_error" || child["issue"] != nil || child["attempted"] == true || child["finishedWithoutIssue"] == true || child["issues"] != nil || child["investigations"] != nil || child["preparedJob"] != nil
}

// Lock the saved children until the corrected parent and its children commit.
func loadSplitChildren(ctx context.Context, tx *sql.Tx, parent Record, ids []any) (map[string]splitChildSnapshot, error) {
	children := map[string]splitChildSnapshot{}
	for _, value := range ids {
		id := fleet.Text(value)
		if id == "" || children[id].record != nil {
			return nil, fail(409, "保存済みの分割先IDを読み取れません。")
		}
		var raw string
		err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=? FOR UPDATE"), id).Scan(&raw)
		if err == sql.ErrNoRows {
			return nil, fail(409, "保存済みの分割先の相談がありません。子の状態を確認してください。")
		}
		if err != nil {
			return nil, err
		}
		var child Record
		if json.Unmarshal([]byte(raw), &child) != nil || child == nil {
			return nil, fail(503, "保存済みの分割計画を読み取れません。")
		}
		if fleet.Text(child["parentSplit"]) != fleet.Text(parent["id"]) {
			return nil, fail(409, "分割先は別の相談へ結び付いています。")
		}
		children[id] = splitChildSnapshot{child, raw}
	}
	return children, nil
}

func correctSplitDependencies(parent Record, children map[string]splitChildSnapshot, now string) (Record, error) {
	proposals := fleet.Items(parent["splitIssues"])
	converted := make([]splitplan.Proposal, len(proposals))
	protected := map[int]bool{}
	for i, id := range fleet.Items(parent["splitChildren"]) {
		if protectedSplitChild(children[fleet.Text(id)].record) {
			protected[i+1] = true
		}
	}
	for i, raw := range proposals {
		p := row(raw)
		scope := row(p["changeScope"])
		acceptance := []string{}
		for _, value := range fleet.Items(p["acceptance"]) {
			acceptance = append(acceptance, fleet.Text(value))
		}
		converted[i] = splitplan.Proposal{Purpose: fleet.Text(p["purpose"]), Acceptance: acceptance, WorkType: fleet.Text(p["workType"]), ChangeScope: splitplan.Scope{Known: scope["known"] == true, Paths: fleet.Items(scope["paths"])}, DependsOn: fleet.Items(p["dependsOn"])}
	}
	deps, corrections := splitplan.CorrectDependencies(converted, protected)
	next := clone(parent)
	if len(corrections) == 0 {
		return next, nil
	}
	// Invalid forward/duplicate dependencies must not become valid by removal.
	if err := validateProposals(parent); err != nil {
		return nil, err
	}
	plans := make([]any, len(proposals))
	for i, raw := range proposals {
		p := clone(row(raw))
		p["dependsOn"] = deps[i]
		plans[i] = p
	}
	history := append([]any{}, fleet.Items(parent["splitDependencyCorrections"])...)
	for _, c := range corrections {
		history = append(history, Record{"proposalNumber": c.ProposalNumber, "removedDependency": c.RemovedDependency, "reason": string(c.Reason), "before": c.Before, "after": c.After, "at": now})
	}
	next["splitIssues"], next["splitDependencyCorrections"] = plans, history
	return next, nil
}

func splitCorrectionNotes(parent Record, index int) string {
	notes := []string{}
	plans := fleet.Items(parent["splitIssues"])
	if index < 0 || index >= len(plans) {
		return ""
	}
	current := fleet.Elements(row(plans[index])["dependsOn"])
	if current == nil {
		return ""
	}
	seen := map[int64]bool{}
	for _, raw := range fleet.Items(parent["splitDependencyCorrections"]) {
		c := row(raw)
		n, validN := integer(c["proposalNumber"])
		removed, validRemoved := integer(c["removedDependency"])
		after := fleet.Elements(c["after"])
		if !validN || !validRemoved || removed < 1 || n != int64(index+1) || after == nil || seen[removed] || len(after) != len(current) {
			continue
		}
		matches := true
		for i, value := range current {
			dep, ok := integer(value)
			corrected, correctedOK := integer(after[i])
			if !ok || !correctedOK || dep != corrected || dep == removed {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		seen[removed] = true
		reason := "資料案は契約案以外に依存する必要がないため"
		if fleet.Text(c["reason"]) == string(splitplan.ImplementationDependency) {
			reason = "契約案への依存があるため実装案どうしの依存は不要"
		}
		notes = append(notes, fmt.Sprintf("依存の自動補正: 分割案%dへの依存を外しました（%s）。", removed, reason))
	}
	if len(notes) == 0 {
		return ""
	}
	return "\n" + strings.Join(notes, "\n")
}

func preserveSplitBaseDependencies(stored, child Record) {
	// A correction changes sibling edges, not existing dependencies outside this split.
	// Explicitly replanned preceding Issues continue to use the new resolved base.
	if encoded(stored["dependsOnIssues"]) != encoded(child["dependsOnIssues"]) {
		return
	}
	base := append([]any{}, fleet.Items(child["_splitBaseDependencies"])...)
	seen := map[string]bool{}
	for _, dep := range base {
		seen[encoded(dep)] = true
	}
	for _, dep := range fleet.Items(stored["_splitBaseDependencies"]) {
		if !seen[encoded(dep)] {
			base = append(base, dep)
			seen[encoded(dep)] = true
		}
	}
	child["_splitBaseDependencies"], child["dependencies"] = base, base
}
