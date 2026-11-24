package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"

	"issue-desk-intake/server/internal/auth"
)

const childMessage = "分割した依頼は、分割の親の相談でまとめて終える・破棄してください。"
const familyMessage = "処理中の親・子や終えられない子がある分割は終えられません。処理が終わってから確認してください。"

type familyReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func familyRead(ctx context.Context, db familyReader, id string) (Record, error) {
	var raw string
	err := db.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=?"), id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decode(raw)
}
func familyLoad(ctx context.Context, db familyReader, d Record) (Record, []Record, error) {
	return familyLoadWithRead(ctx, d, func(ctx context.Context, id string) (Record, error) {
		return familyRead(ctx, db, id)
	})
}
func familyLoadWithRead(ctx context.Context, d Record, read func(context.Context, string) (Record, error)) (Record, []Record, error) {
	parent := d
	if id := text(d["parentSplit"]); id != "" {
		var err error
		parent, err = read(ctx, id)
		if err != nil {
			return nil, nil, err
		}
	}
	children := []Record{}
	for _, id := range fleet.Items(parent["splitChildren"]) {
		c, err := read(ctx, text(id))
		if err != nil {
			return nil, nil, err
		}
		children = append(children, c)
	}
	return parent, children, nil
}
func hasFiled(d Record) bool {
	return d["issue"] != nil || has(d, "issues") || has(d, "investigations")
}

// filedChild recognizes implementation issues retained by a completed split child.
func filedChild(d Record) bool {
	if record(d["issue"])["number"] != nil {
		return true
	}
	if d["status"] == "submitted" {
		for _, raw := range fleet.Items(d["issues"]) {
			if number, ok := integer(record(raw)["number"]); ok && number > 0 {
				return true
			}
		}
	}
	return false
}
func (s *Service) busy(d Record) bool {
	functions := s.engineSnapshot()
	return d != nil && functions.Active != nil && functions.Active(text(d["id"]))
}
func (s *Service) unfiledFamily(p Record, cs []Record) bool {
	if p == nil || p["parentSplit"] != nil || !has(p, "splitChildren") || s.busy(p) || is(text(p["status"]), "submitting", "submitted", "auto_ready", "thinking") || hasFiled(p) {
		return false
	}
	ids := fleet.Items(p["splitChildren"])
	if len(ids) != len(cs) {
		return false
	}
	for i, c := range cs {
		if c == nil || text(ids[i]) == "" || c["id"] != ids[i] || c["parentSplit"] != p["id"] || hasFiled(c) || c["attempted"] == true || s.busy(c) || is(text(c["status"]), "submitting", "submitted", "submit_unknown", "thinking", "auto_ready") {
			return false
		}
	}
	return true
}
func (s *Service) partialFamily(p Record, cs []Record) bool {
	if p == nil || p["parentSplit"] != nil || !has(p, "splitChildren") || s.busy(p) || is(text(p["status"]), "thinking", "submitting", "auto_ready", "submitted") || len(cs) != len(fleet.Items(p["splitChildren"])) || len(cs) > len(fleet.Items(p["splitIssues"])) {
		return false
	}
	filed := false
	for i, c := range cs {
		if text(fleet.Items(p["splitChildren"])[i]) == "" {
			return false
		}
		if c == nil {
			continue
		}
		if c["id"] != fleet.Items(p["splitChildren"])[i] || c["parentSplit"] != p["id"] || s.busy(c) || is(text(c["status"]), "thinking", "submitting", "auto_ready") {
			return false
		}
		if filedChild(c) {
			filed = true
			continue
		}
		if hasFiled(c) || !(c["status"] == "submitted" && c["finishedWithoutIssue"] == true || is(text(c["status"]), "submit_error", "submit_unknown", "draft", "intake_waiting")) {
			return false
		}
	}
	return filed
}

// legacyFiledFamily validates the saved children before permitting a parent-only finish.
func (s *Service) legacyFiledFamily(p Record, cs []Record) bool {
	if p == nil || p["parentSplit"] != nil || !is(text(p["status"]), "draft", "submit_error") || s.busy(p) || hasFiled(p) || len(fleet.Items(p["splitIssues"])) != 0 {
		return false
	}
	ids := fleet.Items(p["splitChildren"])
	if len(ids) == 0 || len(ids) != len(cs) {
		return false
	}
	seen := map[string]bool{}
	for i, c := range cs {
		id := text(ids[i])
		if id == "" || seen[id] || c == nil || text(c["id"]) != id || c["parentSplit"] != p["id"] || s.busy(c) || is(text(c["status"]), "thinking", "submitting", "auto_ready") || !filedChild(c) {
			return false
		}
		seen[id] = true
	}
	return true
}
func closedFamily(d, p Record, cs []Record) bool {
	if d["status"] == "closed" || p["status"] == "closed" {
		return true
	}
	for _, c := range cs {
		if c["status"] == "closed" {
			return true
		}
	}
	return false
}
func (s *Service) familyRefusal(d, p Record, cs []Record, action string) string {
	if closedFamily(d, p, cs) {
		return "利用者の指示で終了した相談です。"
	}
	if action == "todo" {
		return "分割の親・子の相談はTODOに戻せません。分割の親の相談で終えてください。"
	}
	if action == "resubmit" {
		for _, c := range cs {
			if c["finishedWithoutIssue"] == true {
				return "起票せずに終えた子があるため再送できません。残りの子を終えてください。"
			}
		}
		return draftRefusal(d, action, s.busy(d))
	}
	if text(d["parentSplit"]) != "" {
		if action != "finish" || p == nil {
			return childMessage
		}
		member, filed := false, false
		for _, c := range cs {
			member = member || c["id"] == d["id"]
			filed = filed || filedChild(c)
		}
		if !member || !filed {
			return childMessage
		}
		if s.busy(d) || s.busy(p) || is(text(d["status"]), "thinking", "submitting", "auto_ready") || is(text(p["status"]), "thinking", "submitting", "auto_ready") {
			return familyMessage
		}
		if hasFiled(d) || !is(text(d["status"]), "submit_error", "submit_unknown", "draft", "intake_waiting") {
			return "起票結果・調査Issueの無い未起票の子だけ終えられます。"
		}
		return ""
	}
	if s.unfiledFamily(p, cs) || action == "finish" && (s.legacyFiledFamily(p, cs) || s.partialFamily(p, cs)) {
		return ""
	}
	return familyMessage
}
func (s *Service) familyViewRefusal(d Record, action string) string {
	p, cs, err := familyLoad(context.Background(), s.options.Records.DB, d)
	if err != nil {
		return "分割した相談全体の状態を読み取れません。"
	}
	return s.familyRefusal(d, p, cs, action)
}
func saveFamily(ctx context.Context, tx *sql.Tx, d Record) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, bindSQL("UPDATE drafts SET json=? WHERE id=?"), string(raw), d["id"])
	return err
}
func (s *Service) finishRecord(d Record) Record {
	n := clone(d)
	revision, _ := integer(d["revision"])
	n["status"], n["revision"], n["updatedAt"] = "submitted", revision+1, timestamp(s.options.Now())
	delete(n, "investigationNotice")
	entries := []Record{}
	for _, raw := range fleet.Items(d["investigations"]) {
		e := clone(record(raw))
		if e["status"] == "waiting" {
			e["status"], e["reason"], e["at"] = "skipped", "相談を終えたため、この後に届く報告は相談へ取り込みません", n["updatedAt"]
		}
		entries = append(entries, e)
	}
	if d["investigations"] != nil {
		n["investigations"] = entries
	}
	return n
}
func familyIssues(p Record, cs []Record) Record {
	n := clone(p)
	issues := []Record{}
	for _, c := range cs {
		var entries []any
		if record(c["issue"])["number"] != nil {
			entries = append(entries, c["issue"])
		}
		if c["status"] == "submitted" {
			for _, raw := range fleet.Items(c["issues"]) {
				duplicate := false
				for _, existing := range entries {
					a, b := record(existing), record(raw)
					ap, bp := text(a["project"]), text(b["project"])
					if ap == "" {
						ap = text(c["project"])
					}
					if bp == "" {
						bp = text(c["project"])
					}
					an, aok := integer(a["number"])
					bn, bok := integer(b["number"])
					if aok && bok && an == bn && ap == bp {
						duplicate = true
						break
					}
				}
				if !duplicate {
					entries = append(entries, raw)
				}
			}
		}
		for _, raw := range entries {
			if number, ok := integer(record(raw)["number"]); !ok || number <= 0 {
				continue
			}
			i := clone(record(raw))
			if text(i["project"]) == "" {
				i["project"] = c["project"]
			}
			issues = append(issues, i)
		}
	}
	n["issues"] = issues
	if len(issues) > 0 {
		n["issue"] = issues[0]
	}
	return n
}
func (s *Service) changeFamily(ctx context.Context, id, action string, data Record, principal auth.Principal) (Record, error) {
	return s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		p, cs, err := s.loadChangeableFamily(ctx, tx, d, action, data, principal)
		if err != nil {
			return nil, err
		}
		if text(d["parentSplit"]) != "" {
			return s.finishSplitChild(ctx, tx, d, p, cs)
		}
		legacy := action == "finish" && s.legacyFiledFamily(p, cs)
		partial := s.partialFamily(p, cs)
		if legacy {
			return s.finishRecord(familyIssues(d, cs)), nil
		}
		for _, c := range cs {
			if c == nil || partial && (filedChild(c) || c["status"] == "submitted" && c["finishedWithoutIssue"] == true) {
				continue
			}
			if action == "discard" {
				if err := s.withdrawal(ctx, tx, c, action, principal, data); err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, "DELETE FROM drafts WHERE id=?", c["id"]); err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, "DELETE FROM images WHERE scope=?", "draft:"+text(c["id"])); err != nil {
					return nil, err
				}
			} else {
				n := s.finishRecord(c)
				if partial {
					n["finishedWithoutIssue"] = true
				}
				if err := saveFamily(ctx, tx, n); err != nil {
					return nil, err
				}
			}
		}
		if action == "discard" {
			if err := s.withdrawal(ctx, tx, d, action, principal, data); err != nil {
				return nil, err
			}
			_, err := tx.ExecContext(ctx, "DELETE FROM images WHERE scope=?", "draft:"+id)
			return nil, err
		}
		if partial {
			d = familyIssues(d, cs)
		}
		return s.finishRecord(d), nil
	})
}

// loadChangeableFamily reads the split family of the draft and refuses the action when the
// principal may not access a member or the family state does not allow it.
func (s *Service) loadChangeableFamily(ctx context.Context, tx *sql.Tx, d Record, action string, data Record, principal auth.Principal) (Record, []Record, error) {
	if err := s.draftAccess(d, principal); err != nil {
		return nil, nil, err
	}
	if d["status"] == "closed" {
		return nil, nil, refuse(409, "利用者の指示で終了した相談です。")
	}
	if principal.Kind == auth.PC {
		if err := instructionAt(data["instructionAt"], d["createdAt"], s.options.Now()); err != nil {
			return nil, nil, err
		}
	}
	p, cs, err := familyLoad(ctx, tx, d)
	if err != nil {
		return nil, nil, err
	}
	if p != nil {
		if err := s.draftAccess(p, principal); err != nil {
			return nil, nil, err
		}
	}
	for _, c := range cs {
		if c != nil {
			if err := s.draftAccess(c, principal); err != nil {
				return nil, nil, err
			}
		}
	}
	if closedFamily(d, p, cs) {
		return nil, nil, refuse(409, "利用者の指示で終了した相談です。")
	}
	if reason := s.familyRefusal(d, p, cs, action); reason != "" {
		return nil, nil, refuse(409, reason)
	}
	return p, cs, nil
}

// finishSplitChild ends one child without an issue and finishes the parent when every
// child has been filed or ended.
func (s *Service) finishSplitChild(ctx context.Context, tx *sql.Tx, d, p Record, cs []Record) (Record, error) {
	n := s.finishRecord(d)
	n["finishedWithoutIssue"] = true
	all := len(cs) == len(fleet.Items(p["splitIssues"]))
	for i, c := range cs {
		if c["id"] == d["id"] {
			cs[i] = n
			c = n
		}
		all = all && c != nil && c["parentSplit"] == p["id"] && (filedChild(c) || c["status"] == "submitted" && c["finishedWithoutIssue"] == true)
	}
	if all {
		if err := saveFamily(ctx, tx, s.finishRecord(familyIssues(p, cs))); err != nil {
			return nil, err
		}
	}
	return n, nil
}
