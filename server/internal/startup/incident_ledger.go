package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (a *App) incidentLedger(ctx context.Context) (any, error) {
	var raw string
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='incident-deadline' AND (expires_at IS NULL OR expires_at>$1)", time.Now()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []any{}, nil
	}
	if err != nil {
		return nil, errors.New("有事の来歴を共有DBから確認できません")
	}
	var rows []any
	if json.Unmarshal([]byte(raw), &rows) != nil || rows == nil {
		return nil, errors.New("有事の来歴の保存形式が不正です")
	}
	return rows, nil
}
