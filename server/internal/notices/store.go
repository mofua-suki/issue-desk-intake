package notices

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"time"
)

type Service struct {
	DB           *sql.DB
	Now          func() time.Time
	Repositories func() ([]string, error)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func problem(status int, text string) error { return contract.NewProblem(status, text, "") }
func encode(v any) string                   { raw, _ := json.Marshal(v); return string(raw) }
func only(body map[string]any, keys ...string) bool {
	for k := range body {
		found := false
		for _, key := range keys {
			if k == key {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func integer(v any, min, max float64) bool {
	n, ok := v.(float64)
	return ok && n >= min && n <= max && n == float64(int64(n))
}
func timestamp(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	return t, e == nil && strings.HasSuffix(s, "Z")
}
func uuid(v any) string {
	sum := sha256.Sum256([]byte(encode(v)))
	d := hex.EncodeToString(sum[:])
	return d[:8] + "-" + d[8:12] + "-5" + d[13:16] + "-a" + d[17:20] + "-" + d[20:32]
}

type settings struct {
	conn *sql.Conn
	ctx  context.Context
}

func (st settings) read(key string) ([]any, error) {
	var raw string
	err := st.conn.QueryRowContext(st.ctx, bindSQL("SELECT json FROM settings WHERE id=?"), key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var value []any
	err = json.Unmarshal([]byte(raw), &value)
	return value, err
}
func (st settings) save(key string, value any) error {
	_, err := st.conn.ExecContext(st.ctx, bindSQL("INSERT INTO settings(id,json)VALUES(?,?)ON CONFLICT(id)DO UPDATE SET json=excluded.json"), key, encode(value))
	return err
}
func (s *Service) Receive(ctx context.Context, host string, body map[string]any) (any, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err = sqlDialect.BeginLockedWrite(ctx, conn, "ops-watch-notices"); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.Background(), bindSQL("ROLLBACK"))
	st := settings{conn, ctx}
	var value any
	switch body["kind"] {
	case "rc-update":
		value, err = s.rcUpdate(st, host, body)
	case "app-health":
		value, err = s.appHealth(st, host, body)
	case "ops-watch":
		value, err = s.ops(st, host, body)
	case "desk-checkout-lock":
		value, err = s.checkout(st, host, body)
	case "stage-watch":
		if _, exists := body["event"]; exists {
			value, err = s.stageOperation(st, host, body)
		} else {
			value, err = s.simple(st, host, body)
		}
	case "format-version", "remote-control", "boot-check":
		value, err = s.simple(st, host, body)
	default:
		err = problem(400, "お知らせの種類が不正です。")
	}
	if err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, bindSQL("COMMIT")); err != nil {
		return nil, err
	}
	return value, nil
}
func (s *Service) append(st settings, id, host, title, reason string) (any, error) {
	rows, err := st.read("ops-watch-notices")
	if err != nil {
		return nil, err
	}
	for _, raw := range rows {
		if fleet.Object(raw)["id"] == id {
			return map[string]any{"id": id}, nil
		}
	}
	if len(rows) >= 1000 {
		return nil, problem(409, "通知が1000件に達しました。")
	}
	rows = append(rows, map[string]any{"id": id, "host": host, "title": title, "reason": reason, "issues": []any{}, "updatedAt": contract.ISO(s.now())})
	return map[string]any{"id": id}, st.save("ops-watch-notices", rows)
}
func clean(v any) string {
	s := fleet.Text(v)
	s = regexp.MustCompile(`\b(?:idk_|idh_|sk-)[A-Za-z0-9_-]+`).ReplaceAllString(s, "[REDACTED]")
	s = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer\s+)?|(?:token|password|secret)\s*[:=]\s*)[^\s,;]+`).ReplaceAllString(s, "$1[REDACTED]")
	return s
}
