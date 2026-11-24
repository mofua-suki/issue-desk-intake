package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
)

type sessionPair struct {
	Hash  string
	Value json.RawMessage
}

func decodeSessions(raw []byte) ([]sessionPair, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]sessionPair, 0, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			return nil, errors.New("invalid session entry")
		}
		var hash string
		if err := json.Unmarshal(row[0], &hash); err != nil {
			return nil, err
		}
		out = append(out, sessionPair{Hash: hash, Value: row[1]})
	}
	return out, nil
}

func encodeSessions(rows []sessionPair) ([]byte, error) {
	out := make([][2]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		key, err := json.Marshal(row.Hash)
		if err != nil {
			return nil, err
		}
		out = append(out, [2]json.RawMessage{key, row.Value})
	}
	return json.Marshal(out)
}

func (s *SQLite) Session(ctx context.Context, hash string) (auth.Session, bool, error) {
	raw, found, err := s.Setting(ctx, "sessions")
	if err != nil || !found {
		return auth.Session{}, false, err
	}
	rows, err := decodeSessions(raw)
	if err != nil {
		return auth.Session{}, false, err
	}
	for _, row := range rows {
		if row.Hash != hash {
			continue
		}
		var value struct {
			CSRF    string `json:"csrf"`
			Expires int64  `json:"expires"`
		}
		if err := json.Unmarshal(row.Value, &value); err != nil {
			return auth.Session{}, false, err
		}
		return auth.Session{CSRF: value.CSRF, ExpiresAt: time.UnixMilli(value.Expires)}, true, nil
	}
	return auth.Session{}, false, nil
}

func (s *SQLite) updateSessions(ctx context.Context, edit func([]sessionPair) ([]sessionPair, error)) error {
	return appsql.Within(ctx, s.db.transactions, nil, func(tx appsql.Tx) error {
		db := &applicationDB{dialect: s.db.dialect, runner: tx}
		var raw string
		err := db.QueryRowContext(ctx, "SELECT json FROM settings WHERE id='sessions'").Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := decodeSessions([]byte(raw))
		if err != nil {
			return err
		}
		rows, err = edit(rows)
		if err != nil {
			return err
		}
		encoded, err := encodeSessions(rows)
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, "INSERT INTO settings(id,json) VALUES('sessions',?) ON CONFLICT(id) DO UPDATE SET json=excluded.json", string(encoded))
		return err
	})
}

func (s *SQLite) PutSession(ctx context.Context, hash string, value auth.Session) error {
	return s.updateSessions(ctx, func(rows []sessionPair) ([]sessionPair, error) {
		fields := make(map[string]json.RawMessage)
		position := -1
		for i, row := range rows {
			if row.Hash == hash {
				position = i
				if err := json.Unmarshal(row.Value, &fields); err != nil {
					return nil, err
				}
				if fields == nil {
					return nil, errors.New("invalid session value")
				}
				break
			}
		}
		csrf, err := json.Marshal(value.CSRF)
		if err != nil {
			return nil, err
		}
		fields["csrf"] = csrf
		fields["expires"] = json.RawMessage(fmt.Sprintf("%d", value.ExpiresAt.UnixMilli()))
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		pair := sessionPair{Hash: hash, Value: encoded}
		if position >= 0 {
			rows[position] = pair
		} else {
			rows = append(rows, pair)
		}
		return rows, nil
	})
}

func (s *SQLite) RevokeSession(ctx context.Context, hash string) error {
	return s.updateSessions(ctx, func(rows []sessionPair) ([]sessionPair, error) {
		out := make([]sessionPair, 0, len(rows))
		for _, row := range rows {
			if row.Hash != hash {
				out = append(out, row)
			}
		}
		return out, nil
	})
}
