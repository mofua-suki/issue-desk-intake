package images

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// The source record is read in the image write transaction. A parallel submit,
// owner change or scope change cannot authorize an attachment from a stale view.
func (h *Handler) checkWriteScope(ctx context.Context, tx *sql.Tx, scope string, data map[string]any, p auth.Principal) error {
	table, id := "", ""
	if fleet.Text(data["todo"]) != "" {
		table, id = "todos", fleet.Text(data["todo"])
	} else if fleet.Text(data["draft"]) != "" {
		table, id = "drafts", fleet.Text(data["draft"])
	} else {
		return nil
	}
	var raw string
	var e error
	if table == "todos" {
		e = tx.QueryRowContext(ctx, bindSQL("SELECT json FROM todos WHERE id=?"), id).Scan(&raw)
	} else {
		e = tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=?"), id).Scan(&raw)
	}
	if e != nil {
		if e == sql.ErrNoRows {
			return fail(404, "画像の相談が見つかりません。")
		}
		return e
	}
	record := map[string]any{}
	if e := json.Unmarshal([]byte(raw), &record); e != nil {
		return e
	}
	if p.Kind == auth.PC && !p.Delegated && fleet.Text(record["owner"]) != "token:"+p.TokenID {
		return fail(404, "画像の相談が見つかりません。")
	}
	if table == "drafts" && !h.options.DraftWritable(record) {
		return fail(409, "現在画像を変更できません。")
	}
	if scope == "draft:"+id && record["unassigned"] == true && (record["project"] == nil || record["project"] == "") {
		return nil
	}
	if fleet.Text(record["project"]) != fleet.Text(data["project"]) {
		return fail(409, "画像の対象が別の操作で変更されました。")
	}
	if strings.HasPrefix(scope, "todo:") && scope != "todo:"+id {
		return fail(409, "画像の対象が一致しません。")
	}
	return nil
}
