package consultations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"issue-desk-intake/server/internal/auth"
)

func (s *Service) StartTodo(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	if err := requireTier(data["tier"]); err != nil {
		return nil, err
	}
	functions := s.engineSnapshot()
	if functions.Start == nil {
		return nil, refuse(503, "相談の実行先を利用できません。")
	}
	var draft Record
	var snapshot []byte
	var project string
	preflight := true
	ready := errors.New("todo preflight complete")
	change := func(tx *sql.Tx, current Record) (Record, error) {
		if current == nil {
			var err error
			if draft, err = promotedTodoDraft(ctx, tx, id, p); err != nil {
				return nil, err
			}
			return nil, s.checkProject(text(draft["project"]))
		}
		// Hold the row until promotion commits; Records.Change initially reads without a lock.
		if !preflight {
			if err := lockUnchangedTodo(ctx, tx, id, current); err != nil {
				return nil, err
			}
		}
		if err := s.todoAccess(current, p); err != nil {
			return nil, err
		}
		if revision, present := data["revision"]; present {
			requested, valid := integer(revision)
			actual, _ := integer(current["revision"])
			if !valid || requested != actual {
				return nil, refuse(409, "TODOは別の画面から更新されています。")
			}
		}
		body := strings.TrimSpace(text(current["body"]))
		if body == "" {
			return nil, refuse(400, "TODOの内容を保存してから相談を開始してください。")
		}
		provider := text(current["provider"])
		if text(data["provider"]) != "" {
			provider = text(data["provider"])
		}
		provider, err := s.provider(provider)
		if err != nil {
			return nil, err
		}
		cp := text(data["consultationProvider"])
		if cp == "" {
			cp = text(current["consultationProvider"])
		}
		cp, err = consultationProvider(cp)
		if err != nil {
			return nil, err
		}
		images, err := s.todoImages(ctx, tx, id, Record{}, auth.Principal{Kind: auth.CLI}, current)
		if err != nil {
			return nil, err
		}
		state, err := json.Marshal([]any{current, images})
		if err != nil {
			return nil, err
		}
		if preflight {
			snapshot = state
			project = text(current["project"])
		} else if !bytes.Equal(snapshot, state) {
			return nil, refuse(409, "TODOは別の画面から更新されています。")
		}
		at := timestamp(s.options.Now())
		message := Record{"id": id, "role": "user", "text": body, "at": at, "images": images}
		if sendID := text(data["id"]); sendID != "" {
			if !recordID.MatchString(sendID) {
				return nil, refuse(400, "送信IDが不正です。")
			}
			message["requestId"] = sendID
		}
		tier, err := s.options.InitialTier(cp, text(data["tier"]), s.options.Now())
		if err != nil {
			return nil, err
		}
		if preflight {
			return nil, ready
		}
		draft = todoDraft(id, current, data, provider, cp, tier, message, at)
		for _, meta := range images {
			meta["messageId"] = id
			raw, err := json.Marshal(meta)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, bindSQL("UPDATE images SET scope=?,message=?,json=? WHERE id=? AND scope=?"), "draft:"+id, id, string(raw), meta["id"], "todo:"+id); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM images WHERE scope=?", "todo:"+id); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(draft)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), id, string(raw)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	_, err := s.options.Records.Change(ctx, "todos", id, change)
	if errors.Is(err, ready) {
		if project != "" {
			if _, err = s.CheckRepositoryArchive(ctx, project); err != nil {
				return nil, err
			}
		}
		preflight = false
		_, err = s.options.Records.Change(ctx, "todos", id, change)
	}
	if err != nil {
		return nil, err
	}
	if text(draft["status"]) == "thinking" {
		return functions.Start(ctx, id, draft, p)
	}
	return draft, nil
}

// promotedTodoDraft reads the draft an earlier start already promoted from the TODO.
func promotedTodoDraft(ctx context.Context, tx *sql.Tx, id string, p auth.Principal) (Record, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=?"), id).Scan(&raw); err == sql.ErrNoRows {
		return nil, refuse(404, "TODOが見つかりません。")
	} else if err != nil {
		return nil, err
	}
	draft, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if p.Kind == auth.PC && !p.Delegated && text(draft["owner"]) != owner(p) {
		return nil, refuse(404, "TODOが見つかりません。")
	}
	return draft, nil
}

// lockUnchangedTodo locks the TODO row and refuses when it differs from the record read first.
func lockUnchangedTodo(ctx context.Context, tx *sql.Tx, id string, current Record) error {
	var raw string
	if err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM todos WHERE id=? FOR UPDATE"), id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return refuse(409, "TODOは別の画面から更新されています。")
		}
		return err
	}
	locked, err := decode(raw)
	if err != nil {
		return err
	}
	before, err := json.Marshal(current)
	if err != nil {
		return err
	}
	after, err := json.Marshal(locked)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, after) {
		return refuse(409, "TODOは別の画面から更新されています。")
	}
	return nil
}

// todoDraft builds the queued draft that replaces the TODO.
func todoDraft(id string, current, data Record, provider, cp string, tier any, message Record, at string) Record {
	draft := Record{"id": id, "project": current["project"], "provider": provider, "consultationProvider": cp, "status": "thinking", "queued": true, "tier": tier, "questions": []any{}, "messages": []any{message}, "title": "", "body": "", "ready": false, "revision": 0, "createdAt": current["createdAt"], "updatedAt": at, "requestedTier": data["tier"]}
	for key, value := range acceptance(at, "accepted") {
		draft[key] = value
	}
	if text(current["owner"]) != "" {
		draft["owner"] = current["owner"]
	}
	return draft
}
