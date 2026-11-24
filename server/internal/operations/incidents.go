package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"time"
)

var incidentID = regexp.MustCompile(`^[a-f0-9-]{36}$`)

func (s *Service) IncidentAction(ctx context.Context, id, action, token string, body map[string]any) (any, error) {
	if !incidentID.MatchString(id) || len(body) != 0 {
		return nil, contract.NewProblem(400, "有事のIDと空の本文を確認してください。", "")
	}
	at := s.now()
	if s.PostgreSQL == nil {
		return nil, contract.NewProblem(503, "有事の保存先がありません。", "")
	}
	tx, e := s.PostgreSQL.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var raw string
	e = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='incident-deadline' AND(expires_at IS NULL OR expires_at>$1) FOR UPDATE", at).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, contract.NewProblem(404, "有事の記録が見つかりません。", "")
	}
	if e != nil {
		return nil, e
	}
	var rows []any
	if e = json.Unmarshal([]byte(raw), &rows); e != nil {
		return nil, e
	}
	row := findIncident(rows, id)
	if row == nil {
		return nil, contract.NewProblem(404, "有事の記録が見つかりません。", "")
	}
	if e = applyIncidentAction(row, action, token, at); e != nil {
		return nil, e
	}
	saved, e := json.Marshal(rows)
	if e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE settings SET value=$1::jsonb,expires_at=NULL,updated_at=$2,updated_by='incident-deadline' WHERE key='incident-deadline'", string(saved), at); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return map[string]any{"id": id, "state": row["state"]}, nil
}

// findIncident は有事の記録から送信IDの行を探す。無ければ nil を返す。
func findIncident(rows []any, id string) map[string]any {
	var row map[string]any
	for _, v := range rows {
		r := fleet.Object(v)
		if r["id"] == id {
			row = r
			break
		}
	}
	return row
}

// applyIncidentAction は有事への「自分で対応する」または確認済みの操作を、期限と状態を確かめて記録に反映する。
func applyIncidentAction(row map[string]any, action, token string, at time.Time) error {
	add := func(name, reason string) {
		row["history"] = append(fleet.List(row["history"]), map[string]any{"at": iso(at), "event": name, "reason": reason})
	}
	switch action {
	case "respond":
		if row["state"] != "user" {
			deadline, err := time.Parse(time.RFC3339Nano, fleet.Text(row["deadline"]))
			if row["state"] != "waiting" || err != nil || !at.Before(deadline) {
				return contract.NewProblem(409, "対応待ちの期限内だけ回答できます。", "")
			}
			row["state"] = "user"
			add("利用者が対応", "「自分で対応する」を受け付けました。")
		}
	case "acknowledge":
		if row["state"] == "waiting" {
			return contract.NewProblem(409, "対応・解消・期限切れの後だけ確認済みにできます。", "")
		}
		if row["acknowledgedAt"] == nil {
			row["acknowledgedAt"] = iso(at)
			reason := "利用者が記録を確認しました。"
			if token != "" {
				reason = "PC別トークン " + token + "（運用の代行）が確認済みにしました（受付 " + iso(at) + "）。"
			}
			add("確認済み", reason)
		}
	default:
		return contract.NewProblem(404, "操作が見つかりません。", "")
	}
	return nil
}

func (s *Service) AcknowledgeReview(ctx context.Context, round int64) (any, error) {
	c, e := s.DB.Conn(ctx)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	if e = sqlDialect.BeginLockedWrite(ctx, c, "usage-review-ack"); e != nil {
		return nil, e
	}
	defer c.ExecContext(context.Background(), bindSQL("ROLLBACK"))
	if _, e = c.ExecContext(ctx, bindSQL("CREATE TABLE IF NOT EXISTS usage_review_rounds(round INTEGER PRIMARY KEY,json TEXT NOT NULL); CREATE TABLE IF NOT EXISTS usage_review_acks(round INTEGER PRIMARY KEY,at TEXT NOT NULL)")); e != nil {
		return nil, e
	}
	var exists int
	e = c.QueryRowContext(ctx, bindSQL("SELECT 1 FROM usage_review_rounds WHERE round=?"), round).Scan(&exists)
	if e == sql.ErrNoRows {
		return nil, contract.NewProblem(404, "利用量の見直しの回が見つかりません。", "")
	}
	if e != nil {
		return nil, e
	}
	if _, e = c.ExecContext(ctx, bindSQL("INSERT INTO usage_review_acks(round,at) VALUES(?,?) ON CONFLICT(round)DO NOTHING"), round, iso(s.now())); e != nil {
		return nil, e
	}
	var at string
	if e = c.QueryRowContext(ctx, bindSQL("SELECT at FROM usage_review_acks WHERE round=?"), round).Scan(&at); e != nil {
		return nil, e
	}
	if _, e = c.ExecContext(ctx, bindSQL("COMMIT")); e != nil {
		return nil, e
	}
	return map[string]any{"round": round, "acknowledgedAt": at}, nil
}
