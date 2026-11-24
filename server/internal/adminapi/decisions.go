package adminapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func (s *service) projectAllowed(project string) bool {
	return project == "" || s.AllowProject != nil && s.AllowProject(project)
}
func (s *service) decisionFields(data, base map[string]any) (map[string]any, error) {
	pick := func(key string, fallback any) any {
		if v, ok := data[key]; ok {
			return v
		}
		if base != nil {
			return base[key]
		}
		return fallback
	}
	text, note := str(pick("text", nil)), pick("note", "")
	if note == nil {
		note = ""
	}
	noteText, noteOK := note.(string)
	if contract.UTF16Length(strings.TrimSpace(text)) < 1 || contract.UTF16Length(text) > 8000 || !noteOK || contract.UTF16Length(noteText) > 8000 {
		return nil, bad(400, "決定の本文と補足は8000文字以内で入力してください。")
	}
	project := pick("project", nil)
	if project == "" {
		project = nil
	}
	if project != nil {
		p, ok := project.(string)
		if !ok || base == nil && !s.projectAllowed(p) || base != nil && project != base["project"] && !s.projectAllowed(p) {
			return nil, bad(400, "登録済みのプロジェクトを指定してください。")
		}
	}
	enabled, ok := pick("enabled", true).(bool)
	if !ok {
		return nil, bad(400, "有効・無効はtrueかfalseで指定してください。")
	}
	return map[string]any{"text": strings.TrimSpace(text), "note": strings.TrimSpace(noteText), "project": project, "enabled": enabled}, nil
}
func sameDecision(a, b map[string]any) bool {
	for _, key := range []string{"text", "note", "project", "enabled"} {
		if !reflect.DeepEqual(a[key], b[key]) {
			return false
		}
	}
	return true
}
func exportedDecision(d map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "text", "note", "project", "enabled", "source", "revision", "createdAt", "updatedAt", "removedAt"} {
		if v, ok := d[key]; ok {
			out[key] = v
		}
	}
	return out
}
func (s *service) exportDecisions(entries []map[string]any) map[string]any {
	items := []map[string]any{}
	for _, d := range entries {
		items = append(items, exportedDecision(d))
	}
	return map[string]any{"format": "issue-desk-decisions", "version": 1, "exportedAt": iso(s.Now()), "decisions": items}
}
func (s *service) decisions(r *http.Request, p auth.Principal) (value any, status int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.AppDB.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback()
	path := strings.TrimPrefix(r.URL.Path, "/api/decisions")
	ctx := r.Context()
	if r.Method == http.MethodGet && (path == "" || path == "/export") {
		return s.listDecisions(ctx, tx, r, path)
	}
	id, step, e := idRoute(path, uuidPattern, "決定の記録が見つかりません。")
	if e != nil {
		return nil, 0, e
	}
	if r.Method == http.MethodGet && id != "" && step == "" {
		d, e := jsonGet(ctx, tx, "decisions", id)
		if e != nil {
			return nil, 0, e
		}
		if d == nil || d["removedAt"] != nil {
			return nil, 0, bad(404, "決定の記録が見つかりません。")
		}
		return d, 200, nil
	}
	if r.Method != http.MethodPost {
		return nil, 0, bad(404, "経路が見つかりません。")
	}
	data, e := body(r)
	if e != nil {
		return nil, 0, e
	}
	if id == "" {
		id = str(data["id"])
		if !uuidPattern.MatchString(id) {
			return nil, 0, bad(400, "決定の記録のIDが不正です。")
		}
	}
	d, e := jsonGet(ctx, tx, "decisions", id)
	if e != nil {
		return nil, 0, e
	}
	change, e := s.decisionChange(step, id, data, d, iso(s.Now()))
	if e != nil {
		return nil, 0, e
	}
	if !change.changed {
		return change.reply, change.status, nil
	}
	if e = s.storeDecision(ctx, tx, p, step, id, change.record); e != nil {
		return nil, 0, e
	}
	value = change.reply
	if value == nil {
		value = change.record
	}
	return value, change.status, nil
}

// listDecisions answers the project's page of decisions or the full export.
func (s *service) listDecisions(ctx context.Context, tx *sql.Tx, r *http.Request, path string) (any, int, error) {
	if path == "/export" {
		entries, err := jsonList(ctx, tx, "decisions")
		if err != nil {
			return nil, 0, err
		}
		return s.exportDecisions(entries), 200, nil
	}
	project := r.URL.Query().Get("project")
	if !s.projectAllowed(project) {
		return nil, 0, bad(400, "登録済みのプロジェクトを指定してください。")
	}
	page, problem := contract.ParsePagination(r.URL.Query())
	if problem != nil {
		return nil, 0, problem
	}
	result, err := jsonPage(ctx, tx, "decisions", decisionsWhere(), decisionsOrder(), []any{project, project}, page)
	return result, 200, err
}

// decisionUpdate is the result of applying a step to a decision. When changed
// is false, reply and status are answered without saving.
type decisionUpdate struct {
	record  map[string]any
	reply   any
	status  int
	changed bool
}

// decisionChange applies the create, save or remove step to the stored decision d.
func (s *service) decisionChange(step, id string, data, d map[string]any, now string) (decisionUpdate, error) {
	switch step {
	case "":
		fields, e := s.decisionFields(data, nil)
		if e != nil {
			return decisionUpdate{}, e
		}
		if d != nil {
			revision, _ := integer(d["revision"])
			if d["removedAt"] != nil || revision == 0 && !sameDecision(d, fields) {
				return decisionUpdate{}, bad(409, "決定の記録のIDは使用済みです。最新の内容を読み込んでください。")
			}
			return decisionUpdate{reply: d, status: 200}, nil
		}
		d = fields
		d["id"] = id
		d["source"] = "manual"
		d["revision"] = 0
		d["createdAt"] = now
		d["updatedAt"] = now
		return decisionUpdate{record: d, status: 201, changed: true}, nil
	case "save":
		if d == nil {
			return decisionUpdate{}, bad(404, "決定の記録が見つかりません。")
		}
		if d["removedAt"] != nil {
			return decisionUpdate{}, bad(409, "削除済みの決定の記録は編集できません。")
		}
		if v, ok := data["id"]; ok && !uuidPattern.MatchString(str(v)) {
			return decisionUpdate{}, bad(400, "送信IDが不正です。")
		}
		fields, e := s.decisionFields(data, d)
		if e != nil {
			return decisionUpdate{}, e
		}
		if str(data["id"]) != "" && d["requestId"] == data["id"] {
			if !sameDecision(d, fields) {
				return decisionUpdate{}, bad(409, "送信IDは別の内容で使用済みです。")
			}
			return decisionUpdate{reply: d, status: 200}, nil
		}
		revision, _ := integer(d["revision"])
		sent, ok := integer(data["revision"])
		if !ok || sent != revision {
			return decisionUpdate{}, bad(409, "別の画面から更新されています。最新の内容を読み込んでください。")
		}
		for key, v := range fields {
			d[key] = v
		}
		d["revision"] = revision + 1
		d["requestId"] = data["id"]
		d["updatedAt"] = now
		return decisionUpdate{record: d, changed: true}, nil
	case "remove":
		if d == nil {
			return decisionUpdate{}, bad(404, "決定の記録が見つかりません。")
		}
		if d["removedAt"] != nil {
			return decisionUpdate{reply: map[string]any{"id": id, "removed": true}, status: 200}, nil
		}
		revision, _ := integer(d["revision"])
		if v, exists := data["revision"]; exists {
			sent, ok := integer(v)
			if !ok || sent != revision {
				return decisionUpdate{}, bad(409, "別の画面から更新されています。")
			}
		}
		d["revision"] = revision + 1
		d["updatedAt"] = now
		d["removedAt"] = now
		return decisionUpdate{record: d, reply: map[string]any{"id": id, "removed": true}, changed: true}, nil
	default:
		return decisionUpdate{}, bad(404, "経路が見つかりません。")
	}
}

// storeDecision saves the decision with its audit record, then refreshes the
// mirror file and notifies the screens.
func (s *service) storeDecision(ctx context.Context, tx *sql.Tx, p auth.Principal, step, id string, d map[string]any) error {
	if e := jsonPut(ctx, tx, "decisions", id, d); e != nil {
		return e
	}
	if e := s.audit(ctx, tx, p, "decision:"+step, id); e != nil {
		return e
	}
	if e := tx.Commit(); e != nil {
		return e
	}
	if e := s.writeDecisionMirror(ctx); e != nil {
		s.log("決定の記録の写しを書けません。アプリのDBへの保存は完了しています。原因: " + contract.SafeCause(e))
	}
	s.notify("decision", id)
	return nil
}

func (s *service) allDecisions(ctx context.Context) ([]map[string]any, error) {
	tx, e := s.AppDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	return jsonList(ctx, tx, "decisions")
}
func decodeJSON(raw []byte, target any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	return d.Decode(target)
}
