package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func (s *Service) withdrawal(ctx context.Context, tx *sql.Tx, d Record, operation string, p auth.Principal, data Record) error {
	via := "screen"
	var token any
	if p.Kind == auth.CLI {
		via = "key"
	}
	if p.Kind == auth.PC {
		via = "token"
		token = p.Name
	}
	var reason any
	if value, present := data["reason"]; present {
		body, ok := value.(string)
		if !ok {
			return refuse(400, "理由は1〜500文字で指定してください。")
		}
		body = strings.Map(func(r rune) rune {
			if r < 32 || r >= 127 && r <= 159 {
				return ' '
			}
			return r
		}, body)
		body = strings.TrimSpace(body)
		if body == "" || contract.UTF16Length(body) > 500 {
			return refuse(400, "理由は1〜500文字で指定してください。")
		}
		reason = body
	}
	_, err := tx.ExecContext(ctx, bindSQL("INSERT INTO draft_withdrawals(draft,project,title,operation,at,created_at,via,token_name,reason,parent) VALUES(?,?,?,?,?,?,?,?,?,?)"), d["id"], d["project"], d["title"], operation, timestamp(s.options.Now()), d["createdAt"], via, token, reason, nil)
	return err
}
func (s *Service) ReturnToTodo(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	var todo Record
	_, err := s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if d == nil {
			var err error
			todo, err = s.returnedTodoTx(ctx, tx, id, data, p)
			return nil, err
		}
		if err := s.draftAccess(d, p); err != nil {
			return nil, err
		}
		if p.Kind == auth.PC {
			if err := instructionAt(data["instructionAt"], d["createdAt"], s.options.Now()); err != nil {
				return nil, err
			}
		}
		if d["parentSplit"] != nil || has(d, "splitChildren") {
			return nil, refuse(409, "分割の親・子の相談はTODOに戻せません。分割の親の相談で終えてください。")
		}
		if reason := s.refusal(d, "todo"); reason != "" {
			return nil, refuse(409, reason)
		}
		first := Record{}
		for _, entry := range fleet.Items(d["messages"]) {
			if record(entry)["role"] == "user" {
				first = record(entry)
				break
			}
		}
		now := timestamp(s.options.Now())
		kept, err := moveFirstImagesToTodoTx(ctx, tx, id, first)
		if err != nil {
			return nil, err
		}
		cp := text(d["consultationProvider"])
		if cp == "" {
			cp = "codex"
		}
		created := d["createdAt"]
		if created == nil {
			created = now
		}
		todo = Record{"id": id, "project": d["project"], "provider": d["provider"], "consultationProvider": cp, "body": strings.TrimSpace(text(first["text"])), "images": kept, "revision": 0, "createdAt": created, "updatedAt": now, "returnedAt": now}
		raw, err := json.Marshal(todo)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO todos(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json"), id, string(raw)); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM images WHERE scope=?", "draft:"+id); err != nil {
			return nil, err
		}
		if err = s.withdrawal(ctx, tx, d, "todo", p, data); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err == nil {
		functions := s.engineSnapshot()
		if functions.Cancel != nil {
			functions.Cancel(id)
		}
	}
	return todo, err
}

// returnedTodoTx reads a TODO an earlier request already returned and checks that the
// principal may use it.
func (s *Service) returnedTodoTx(ctx context.Context, tx *sql.Tx, id string, data Record, p auth.Principal) (Record, error) {
	var raw string
	err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM todos WHERE id=?"), id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, refuse(404, "下書きが見つかりません。")
	}
	if err != nil {
		return nil, err
	}
	todo, err := decode(raw)
	if err != nil {
		return todo, err
	}
	if p.Kind == auth.PC {
		if err := instructionAt(data["instructionAt"], todo["createdAt"], s.options.Now()); err != nil {
			return todo, err
		}
	}
	return todo, s.todoAccess(todo, p)
}

// moveFirstImagesToTodoTx moves the images of the first user message back to the TODO.
func moveFirstImagesToTodoTx(ctx context.Context, tx *sql.Tx, id string, first Record) ([]Record, error) {
	kept := []Record{}
	for _, entry := range fleet.Items(first["images"]) {
		meta := record(entry)
		var raw string
		err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM images WHERE id=? AND scope=?"), meta["id"], "draft:"+id).Scan(&raw)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		saved, err := decode(raw)
		if err != nil {
			return nil, err
		}
		kept = append(kept, saved)
		if _, err = tx.ExecContext(ctx, bindSQL("UPDATE images SET scope=?,bound=1 WHERE id=? AND scope=?"), "todo:"+id, meta["id"], "draft:"+id); err != nil {
			return nil, err
		}
	}
	return kept, nil
}
func (s *Service) Discard(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	if d, found, err := s.options.Records.Get(ctx, "drafts", id); err != nil {
		return nil, err
	} else if found && (d["parentSplit"] != nil || has(d, "splitChildren")) {
		_, err := s.changeFamily(ctx, id, "discard", data, p)
		return Record{"id": id, "discarded": true}, err
	}
	_, err := s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if err := s.draftAccess(d, p); err != nil {
			return nil, err
		}
		if p.Kind == auth.PC {
			if err := instructionAt(data["instructionAt"], d["createdAt"], s.options.Now()); err != nil {
				return nil, err
			}
		}
		if reason := s.refusal(d, "discard"); reason != "" {
			return nil, refuse(409, reason)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM images WHERE scope=?", "draft:"+id); err != nil {
			return nil, err
		}
		if err := s.withdrawal(ctx, tx, d, "discard", p, data); err != nil {
			return nil, err
		}
		return nil, nil
	})
	return Record{"id": id, "discarded": true}, err
}
func (s *Service) Finish(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	if d, found, err := s.options.Records.Get(ctx, "drafts", id); err != nil {
		return nil, err
	} else if found && (d["parentSplit"] != nil || has(d, "splitChildren")) {
		result, err := s.changeFamily(ctx, id, "finish", data, p)
		if problem, ok := err.(*contract.Problem); ok && problem.Status == 409 && problem.Message == familyMessage && text(d["status"]) == "submitting" {
			return nil, refuse(409, "分割の起票処理中です。GET /api/drafts/"+id+" で結果を確認してください。")
		}
		return result, err
	}
	return s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if err := s.draftAccess(d, p); err != nil {
			return nil, err
		}
		if p.Kind == auth.PC {
			if err := instructionAt(data["instructionAt"], d["createdAt"], s.options.Now()); err != nil {
				return nil, err
			}
		}
		if reason := s.refusal(d, "finish"); reason != "" {
			return nil, refuse(409, reason)
		}
		next := clone(d)
		investigations := []Record{}
		now := timestamp(s.options.Now())
		for _, entry := range fleet.Items(d["investigations"]) {
			i := clone(record(entry))
			if i["status"] == "waiting" {
				i["status"], i["reason"], i["at"] = "skipped", "相談を終えたため、この後に届く報告は相談へ取り込みません", now
			}
			investigations = append(investigations, i)
		}
		if d["investigations"] != nil {
			next["investigations"] = investigations
		}
		delete(next, "investigationNotice")
		revision, _ := integer(d["revision"])
		next["status"], next["revision"], next["updatedAt"] = "submitted", revision+1, now
		next["finishedWithoutIssue"], next["finishedAt"] = true, now
		if value, ok := data["instructionAt"]; ok {
			next["finishInstructionAt"] = value
		}
		next["ready"] = false
		for _, key := range []string{"run", "queued", "retryAt"} {
			delete(next, key)
		}
		return next, nil
	})
}
