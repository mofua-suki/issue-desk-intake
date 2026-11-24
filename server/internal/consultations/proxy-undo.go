package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
)

func (s *Service) UndoProxy(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	approval, denied := auth.UserApproval(p, data["statement"], data["instructionAt"], s.options.Now(), "代理回答の取消")
	if denied != nil {
		return nil, denied
	}
	messageID := text(data["messageId"])
	if messageID == "" || len(messageID) > 200 {
		return nil, refuse(400, "取り消す代理の発言IDを指定してください。")
	}
	d, err := s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if d == nil {
			return nil, refuse(404, "相談が見つかりません。")
		}
		if err := s.draftAccess(d, p); err != nil {
			return nil, err
		}
		if reason := draftRefusal(d, "proxy_undo", false); reason != "" {
			return nil, refuse(409, reason)
		}
		messages := fleet.Items(d["messages"])
		index := -1
		for i, item := range messages {
			m := record(item)
			if m["role"] == "proxy" && m["id"] == messageID {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, refuse(409, "この代理の回答は取り消し済みか、今の会話にありません。")
		}
		active, snapshot, err := activeProxyAnswersTx(ctx, tx, id, messageID)
		if err != nil {
			return nil, err
		}
		count, valid := integer(snapshot["messageCount"])
		if !valid || count != int64(index) || fleet.Items(snapshot["questions"]) == nil {
			return nil, refuse(409, "この代理回答の時点の記録が見つからないため取り消せません。")
		}
		next := clone(d)
		kept := messages[:index]
		removed := messages[index:]
		proxyIDs := map[string]bool{}
		userIDs := map[string]bool{}
		for _, item := range removed {
			m := record(item)
			if m["role"] == "proxy" {
				proxyIDs[text(m["id"])] = true
			}
		}
		for _, item := range kept {
			m := record(item)
			if m["role"] == "user" {
				userIDs[text(m["id"])] = true
			}
		}
		at := timestamp(s.options.Now())
		undone, err := undoProxyAnswersTx(ctx, tx, active, proxyIDs, at)
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"title", "body", "questions", "changeScope", "splitIssues", "heavyReason", "dependsOnIssues", "kind", "precedingProblems"} {
			next[key] = snapshot[key]
		}
		keepShownCrossApprovals(next, d, kept, userIDs)
		reasons := []any{}
		for _, question := range fleet.Items(snapshot["questions"]) {
			reasons = append(reasons, Record{"question": question, "reason": "代理の回答を取り消して戻した質問です。利用者の回答を待ちます。"})
		}
		revision, _ := integer(d["revision"])
		next["messages"], next["questionReasons"], next["revision"], next["updatedAt"], next["status"], next["ready"], next["run"], next["queued"], next["waitStartedAt"] = kept, reasons, revision+1, at, "intake_waiting", false, nil, false, nil
		SetDraftError(next, nil, nil)
		for k, v := range acceptance(at, nil) {
			next[k] = v
		}
		next["waitStartedAt"] = nil
		delete(next, "proxyPending")
		undo := Record{"messageId": messageID, "at": at, "messageCount": index, "answers": undone, "removed": removed, "via": "screen"}
		if approval.Statement != "" {
			undo["via"], undo["pc"], undo["statement"], undo["instructionAt"] = "chat", p.Name, approval.Statement, approval.InstructionAt
		}
		next["proxyUndos"] = append(fleet.Items(d["proxyUndos"]), undo)
		return next, nil
	})
	if err == nil {
		functions := s.engineSnapshot()
		if functions.Cancel != nil {
			functions.Cancel(id)
		}
	}
	return d, err
}

// activeProxyAnswersTx reads the active proxy answers of the draft and the snapshot saved
// with the answers of the given proxy message.
func activeProxyAnswersTx(ctx context.Context, tx *sql.Tx, id, messageID string) ([]Record, Record, error) {
	rows, err := tx.QueryContext(ctx, bindSQL("SELECT json FROM proxy_answers WHERE draft=? AND status='active' ORDER BY "+rowOrder(false)), id)
	if err != nil {
		return nil, nil, err
	}
	active := []Record{}
	var snapshot Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, nil, err
		}
		r, err := decode(raw)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		active = append(active, r)
		if r["messageId"] == messageID && record(r["snapshot"]) != nil {
			snapshot = record(r["snapshot"])
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	return active, snapshot, nil
}

// undoProxyAnswersTx marks the answers of the removed proxy messages undone and returns their IDs.
func undoProxyAnswersTx(ctx context.Context, tx *sql.Tx, active []Record, proxyIDs map[string]bool, at string) ([]any, error) {
	undone := []any{}
	for _, r := range active {
		if proxyIDs[text(r["messageId"])] {
			r["status"], r["undoneAt"] = "undone", at
			raw, err := json.Marshal(r)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, bindSQL("UPDATE proxy_answers SET status='undone',json=? WHERE id=?"), string(raw), r["id"]); err != nil {
				return nil, err
			}
			undone = append(undone, r["id"])
		}
	}
	return undone, nil
}

// keepShownCrossApprovals keeps only the cross-project approvals given in the kept
// conversation, and the proposal only when the kept conversation still shows it.
func keepShownCrossApprovals(next, d Record, kept []any, userIDs map[string]bool) {
	approvals := []any{}
	for _, item := range fleet.Items(d["crossApprovals"]) {
		a := record(item)
		if text(a["messageId"]) == "" || userIDs[text(a["messageId"])] {
			approvals = append(approvals, a)
		}
	}
	next["crossApprovals"] = approvals
	shown := ""
	for i := len(kept) - 1; i >= 0; i-- {
		m := record(kept[i])
		if m["role"] == "assistant" {
			shown = text(m["crossProposal"])
			break
		}
	}
	if shown == "" || text(record(d["crossProposal"])["fingerprint"]) != shown {
		delete(next, "crossProposal")
	}
}
