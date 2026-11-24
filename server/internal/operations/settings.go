package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
)

func (s *Service) OperationalSetting(ctx context.Context, key string) (any, error) {
	if s.PostgreSQL == nil {
		return nil, contract.NewProblem(503, "運用設定の保存先がありません。", "")
	}
	var raw string
	e := s.PostgreSQL.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 AND(expires_at IS NULL OR expires_at>$2)", key, s.now()).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var value any
	e = json.Unmarshal([]byte(raw), &value)
	return value, e
}
