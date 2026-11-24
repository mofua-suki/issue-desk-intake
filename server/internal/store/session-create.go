package store

import (
	"context"
	"encoding/json"
	"time"

	"issue-desk-intake/server/internal/auth"
)

// CreateSession prunes expired sessions and applies the screen-session bound in
// the same transaction as insertion. Unknown fields on retained entries survive.
func (s *SQLite) CreateSession(ctx context.Context, hash string, value auth.Session, now time.Time) error {
	return s.updateSessions(ctx, func(rows []sessionPair) ([]sessionPair, error) {
		retained := make([]sessionPair, 0, len(rows)+1)
		for _, row := range rows {
			var session struct {
				Expires int64 `json:"expires"`
			}
			if err := json.Unmarshal(row.Value, &session); err != nil {
				return nil, err
			}
			if session.Expires >= now.UnixMilli() && row.Hash != hash {
				retained = append(retained, row)
			}
		}
		if len(retained) > 100 {
			retained = retained[len(retained)-100:]
		}
		raw, err := json.Marshal(struct {
			CSRF    string `json:"csrf"`
			Expires int64  `json:"expires"`
		}{value.CSRF, value.ExpiresAt.UnixMilli()})
		if err != nil {
			return nil, err
		}
		return append(retained, sessionPair{Hash: hash, Value: raw}), nil
	})
}

// ConsumeDesktopKey consumes one still-valid start link atomically. The value
// remains a JSON null, matching the setting's normal persistence representation.
func (s *SQLite) ConsumeDesktopKey(ctx context.Context, hash string, now time.Time) (bool, error) {
	query := `UPDATE settings SET json='null' WHERE id='desktopToken' AND json::jsonb->>'hash'=? AND (json::jsonb->>'expires')::numeric>=?`
	result, err := s.db.ExecContext(ctx, query, hash, now.UnixMilli())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
