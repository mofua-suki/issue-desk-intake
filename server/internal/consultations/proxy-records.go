package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

func proxyTables(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	_, err := db.ExecContext(ctx, bindSQL(`CREATE TABLE IF NOT EXISTS reply_decisions(id TEXT PRIMARY KEY,project TEXT,draft TEXT NOT NULL,at TEXT NOT NULL,json TEXT NOT NULL);CREATE INDEX IF NOT EXISTS reply_decisions_project ON reply_decisions(project,at);CREATE TABLE IF NOT EXISTS proxy_answers(id TEXT PRIMARY KEY,project TEXT,draft TEXT NOT NULL,at TEXT NOT NULL,status TEXT NOT NULL,json TEXT NOT NULL);CREATE INDEX IF NOT EXISTS proxy_answers_draft ON proxy_answers(draft);`))
	return err
}

func MaskCredentials(value string) string { return contract.MaskCredentials(value) }
func prefix(value string, max int) string {
	var out strings.Builder
	n := 0
	for _, r := range value {
		size := 1
		if r > 65535 {
			size = 2
		}
		if n+size > max {
			break
		}
		out.WriteRune(r)
		n += size
	}
	return out.String()
}
func recordReply(ctx context.Context, tx *sql.Tx, d, message Record) error {
	questions := []any{}
	for _, q := range fleet.Items(d["questions"]) {
		questions = append(questions, MaskCredentials(text(q)))
	}
	value := Record{"id": message["id"], "project": d["project"], "draft": d["id"], "questions": questions, "text": prefix(MaskCredentials(text(message["text"])), 2000), "at": message["at"]}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, insertIgnoreSQL("reply_decisions", []string{"id", "project", "draft", "at", "json"}, "VALUES(?,?,?,?,?)"), message["id"], d["project"], d["id"], message["at"], string(raw))
	return err
}
func (r *Records) ProxyDecisions(ctx context.Context, project string) ([]any, error) {
	manual, err := r.List(ctx, "decisions")
	if err != nil {
		return nil, err
	}
	entries := []Record{}
	for _, d := range manual {
		if d["removedAt"] == nil && d["enabled"] != false && (d["project"] == nil || d["project"] == project) {
			entries = append(entries, Record{"kind": "manual", "id": d["id"], "at": d["updatedAt"], "text": MaskCredentials(text(d["text"])), "note": MaskCredentials(text(d["note"]))})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return text(entries[i]["at"]) > text(entries[j]["at"]) })
	rows, err := r.DB.QueryContext(ctx, bindSQL("SELECT json FROM reply_decisions WHERE project=? ORDER BY at DESC,"+rowOrder(true)+" LIMIT 80"), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		entry, err := decode(raw)
		if err != nil {
			return nil, err
		}
		entry["kind"] = "reply"
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	chosen := []any{}
	for _, entry := range entries {
		if len(chosen) >= 20 {
			break
		}
		candidate := append(append([]any{}, chosen...), entry)
		raw, err := json.Marshal(candidate)
		if err != nil {
			return nil, err
		}
		if len(raw) <= 12000 {
			chosen = candidate
		}
	}
	sort.SliceStable(chosen, func(i, j int) bool { return text(record(chosen[i])["at"]) > text(record(chosen[j])["at"]) })
	return chosen, nil
}
func (r *Records) SaveProxy(ctx context.Context, id, run string, fields Record, answers []Record) (Record, error) {
	return r.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if d == nil || d["status"] != "thinking" || text(d["run"]) != run {
			return nil, refuse(409, "相談の実行権を失いました。")
		}
		if value, exists := fields["splitIssues"]; exists && !ApplySplitAnswer(d["splitIssues"], value, fields) {
			delete(fields, "splitIssues")
			// Undo restores the state presented immediately before the proxy answer.
			for _, row := range answers {
				if snapshot := record(row["snapshot"]); snapshot != nil {
					snapshot["splitIssues"] = d["splitIssues"]
				}
			}
		}
		delete(fields, "_cancelSplitIssues")
		for _, row := range answers {
			raw, err := json.Marshal(row)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, bindSQL("INSERT INTO proxy_answers(id,project,draft,at,status,json) VALUES(?,?,?,?,?,?)"), row["id"], row["project"], id, row["at"], "active", string(raw)); err != nil {
				return nil, err
			}
		}
		next := clone(d)
		for k, v := range fields {
			next[k] = v
		}
		return next, nil
	})
}
