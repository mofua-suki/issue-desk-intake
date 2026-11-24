// 受付の settings 表から期限内の保存値を読み、値と版（Settings）を返す。

package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"issue-desk-intake/server/internal/contract"
)

func (s *Service) Settings(ctx context.Context) (map[string]any, string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT key,value,expires_at FROM settings ORDER BY key")
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	values := map[string]any{}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	for rows.Next() {
		var key string
		var raw []byte
		var expires sql.NullTime
		if err = rows.Scan(&key, &raw, &expires); err != nil {
			return nil, "", err
		}
		if expires.Valid && !expires.Time.After(now) {
			continue
		}
		var value any
		if err = json.Unmarshal(raw, &value); err != nil {
			return nil, "", err
		}
		if value == nil {
			continue
		}
		var until any
		if expires.Valid {
			until = contract.ISO(expires.Time)
		}
		values[key] = map[string]any{"value": value, "expiresAt": until}
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	return values, Digest(values), nil
}
