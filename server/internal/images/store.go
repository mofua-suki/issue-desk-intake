package images

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

type storedImage struct {
	Meta           map[string]any
	Scope, Message string
	Bytes          []byte
	Bound          bool
}

func load(ctx context.Context, tx *sql.Tx, id string) (*storedImage, error) {
	var raw string
	var bound int
	image := &storedImage{}
	e := tx.QueryRowContext(ctx, bindSQL("SELECT json,scope,message,bytes,bound FROM images WHERE id=?"), id).Scan(&raw, &image.Scope, &image.Message, &image.Bytes, &bound)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal([]byte(raw), &image.Meta); e != nil {
		return nil, e
	}
	image.Bound = bound != 0
	return image, nil
}
func (h *Handler) upload(ctx context.Context, scope string, data map[string]any, principal auth.Principal) (map[string]any, error) {
	id, message := fleet.Text(data["id"]), fleet.Text(data["messageId"])
	if !imageID.MatchString(id) || !imageID.MatchString(message) {
		return nil, fail(400, "画像・メッセージIDが不正です。")
	}
	raw, info, e := Decode(fleet.Text(data["data"]))
	if e != nil {
		return nil, e
	}
	tx, e := h.options.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if e = h.checkWriteScope(ctx, tx, scope, data, principal); e != nil {
		return nil, e
	}
	existing, e := load(ctx, tx, id)
	if e != nil {
		return nil, e
	}
	if existing != nil {
		if existing.Scope != scope || existing.Message != message || existing.Meta["sha256"] != info.SHA256 {
			return nil, fail(409, "画像IDは別の画像で使用済みです。")
		}
		meta := clone(existing.Meta)
		meta["bound"] = existing.Bound
		return meta, nil
	}
	var total, perMessage int
	if e = tx.QueryRowContext(ctx, bindSQL("SELECT count(*),COALESCE(sum(CASE WHEN message=? THEN 1 ELSE 0 END),0) FROM images WHERE scope=?"), message, scope).Scan(&total, &perMessage); e != nil {
		return nil, e
	}
	if perMessage >= PerMessage || strings.HasPrefix(scope, "draft:") && total >= PerConversation || strings.HasPrefix(scope, "todo:") && total >= PerConversation {
		return nil, fail(400, "画像は1メッセージ4枚、1相談全体12枚までです。")
	}
	meta := map[string]any{"id": id, "messageId": message, "mime": info.MIME, "ext": info.Ext, "width": info.Width, "height": info.Height, "size": len(raw), "sha256": info.SHA256}
	encoded, e := json.Marshal(meta)
	if e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, bindSQL("INSERT INTO images(id,scope,message,json,bytes) VALUES(?,?,?,?,?)"), id, scope, message, string(encoded), raw); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return meta, nil
}
func (h *Handler) read(ctx context.Context, id, scope string, data map[string]any) (map[string]any, []byte, error) {
	tx, e := h.options.AppDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return nil, nil, e
	}
	defer tx.Rollback()
	image, e := load(ctx, tx, id)
	if e != nil {
		return nil, nil, e
	}
	if image == nil {
		return nil, nil, fail(404, "画像が見つかりません。")
	}
	linked := false
	if image.Bound && fleet.Text(data["draft"]) == "" && image.Scope != scope {
		rows, e := tx.QueryContext(ctx, bindSQL("SELECT json FROM drafts ORDER BY "+rowOrder(false)))
		if e != nil {
			return nil, nil, e
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				return nil, nil, e
			}
			var d map[string]any
			if e = json.Unmarshal([]byte(raw), &d); e != nil {
				return nil, nil, e
			}
			source := fleet.Text(d["imageSourceId"])
			if source == "" {
				source = fleet.Text(d["id"])
			}
			issue, _ := d["issue"].(map[string]any)
			project := fleet.Text(issue["project"])
			if project == "" {
				project = fleet.Text(d["project"])
			}
			if image.Scope == "draft:"+source && project == fleet.Text(data["project"]) && fmt.Sprint(issue["number"]) == fmt.Sprint(data["number"]) {
				linked = true
				break
			}
		}
		if e = rows.Err(); e != nil {
			return nil, nil, e
		}
	}
	if image.Scope != scope && !linked {
		return nil, nil, fail(404, "画像が見つかりません。")
	}
	return image.Meta, image.Bytes, nil
}
func (h *Handler) remove(ctx context.Context, id, scope string, data map[string]any, principal auth.Principal) (map[string]any, error) {
	tx, e := h.options.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if e = h.checkWriteScope(ctx, tx, scope, data, principal); e != nil {
		return nil, e
	}
	image, e := load(ctx, tx, id)
	if e != nil {
		return nil, e
	}
	if image == nil {
		return map[string]any{"removed": true}, nil
	}
	if image.Scope != scope {
		return nil, fail(404, "画像が見つかりません。")
	}
	if image.Bound {
		if fleet.Text(data["todo"]) != "" {
			return nil, fail(409, "保存済みTODOの画像です。TODOの画面で削除を選び、保存してから外してください。")
		}
		return nil, fail(409, "送信済みメッセージの画像は削除できません。")
	}
	result, e := tx.ExecContext(ctx, "DELETE FROM images WHERE id=? AND scope=? AND bound=0", id, scope)
	if e != nil {
		return nil, e
	}
	count, e := result.RowsAffected()
	if e != nil {
		return nil, e
	}
	if count != 1 {
		return nil, fail(409, "画像は別の操作で使用されています。")
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return map[string]any{"removed": true}, nil
}
