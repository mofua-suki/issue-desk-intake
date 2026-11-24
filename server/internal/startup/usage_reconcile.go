package startup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
)

func (a *App) readIntakeCallUsage(ctx context.Context, id string) (map[string]any, error) {
	sum := sha256.Sum256([]byte("intake-call:" + id))
	var raw string
	err := a.DB.QueryRowContext(ctx, "SELECT json FROM usage_rows WHERE key=$1 AND source='intake' AND sender='intake'", hex.EncodeToString(sum[:])).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var row map[string]any
	err = json.Unmarshal([]byte(raw), &row)
	return row, err
}
