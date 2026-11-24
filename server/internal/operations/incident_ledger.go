package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
)

func (s *Service) mutateIncidents(ctx context.Context, change func(*[]any) error) error {
	if s.PostgreSQL == nil {
		return contract.NewProblem(503, "有事の保存先がありません。", "")
	}
	tx, e := s.PostgreSQL.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('issue-desk-incident-ledger'))"); e != nil {
		return e
	}
	var raw string
	e = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='incident-deadline' AND(expires_at IS NULL OR expires_at>$1)FOR UPDATE", s.now()).Scan(&raw)
	rows := []any{}
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if e == nil {
		if e = json.Unmarshal([]byte(raw), &rows); e != nil {
			return e
		}
		if rows == nil {
			return fmt.Errorf("有事の台帳が配列ではありません")
		}
	}
	if e = change(&rows); e != nil {
		return e
	}
	value, e := json.Marshal(rows)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO settings(key,value,expires_at,updated_at,updated_by)VALUES('incident-deadline',$1::jsonb,NULL,$2,'incident-deadline') ON CONFLICT(key)DO UPDATE SET value=excluded.value,expires_at=NULL,updated_at=excluded.updated_at,updated_by=excluded.updated_by", string(value), s.now()); e != nil {
		return e
	}
	return tx.Commit()
}
func incidentRow(rows []any, id string) map[string]any {
	for _, raw := range rows {
		r := fleet.Object(raw)
		if r["id"] == id {
			return r
		}
	}
	return nil
}
func (s *Service) incidentNote(row map[string]any, event, reason string) {
	row["history"] = append(fleet.List(row["history"]), map[string]any{"at": iso(s.now()), "event": event, "reason": usage.Redact(reason)})
}
func (s *Service) changeIncident(ctx context.Context, id string, change func(map[string]any) error) error {
	return s.mutateIncidents(ctx, func(rows *[]any) error {
		r := incidentRow(*rows, id)
		if r == nil {
			return contract.NewProblem(404, "有事の記録が見つかりません。", "")
		}
		return change(r)
	})
}
