package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"time"
	"unicode"
)

func safe(value any, max int) bool {
	v, ok := value.(string)
	if !ok || contract.UTF16Length(v) > max {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

var untilPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d{1,3})?)?(Z|[+-]\d{2}:\d{2})$`)

func ValidateDisable(provider string, data map[string]any, by string, now time.Time) (map[string]any, error) {
	if provider != "codex" && provider != "claude" {
		return nil, contract.NewProblem(400, "無効にできる実行先は codex・claude です。", "")
	}
	if !safe(by, 100) {
		return nil, contract.NewProblem(400, "設定者は制御文字を含まない100文字以内にしてください。", "")
	}
	until := contract.ISO(now.Add(24 * time.Hour))
	if value, exists := data["until"]; exists {
		until = fleet.Text(value)
	}
	parsed, e := parseUntil(until)
	if e != nil || !parsed.After(now) || parsed.Sub(now) > 7*24*time.Hour {
		return nil, contract.NewProblem(400, "期限は未来から7日以内のオフセット付きISO日時にしてください。", "")
	}
	entry := map[string]any{"until": until, "setAt": contract.ISO(now), "by": by}
	if reason, ok := data["reason"]; ok {
		if !safe(reason, 200) {
			return nil, contract.NewProblem(400, "理由は制御文字を含まない200文字以内にしてください。", "")
		}
		entry["reason"] = reason
	}
	return entry, nil
}
func parseUntil(v string) (time.Time, error) {
	if !untilPattern.MatchString(v) {
		return time.Time{}, contract.NewProblem(400, "期限が不正です。", "")
	}
	if len(v) >= 17 && v[16] != ':' {
		if v[16] == 'Z' {
			v = v[:16] + ":00Z"
		} else {
			v = v[:16] + ":00" + v[16:]
		}
	}
	return time.Parse(time.RFC3339Nano, v)
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s *Service) Disables(ctx context.Context, ready []map[string]any) (map[string]any, error) {
	out := []any{}
	for _, p := range []string{"codex", "claude"} {
		entry, e := s.active(ctx, p)
		if e != nil {
			return nil, e
		}
		if entry != nil {
			entry["provider"] = p
			out = append(out, entry)
		}
	}
	now := s.now()
	providers := []any{}
	for _, p := range []string{"codex", "claude"} {
		var found map[string]any
		for _, item := range ready {
			if item["id"] == p {
				found = item
				break
			}
		}
		if found == nil {
			found = map[string]any{"id": p, "name": p, "ready": false, "reason": "この実行先は設定されていません。"}
		}
		providers = append(providers, found)
	}
	return map[string]any{"disables": out, "providers": providers, "maxDays": 7, "defaultUntil": contract.ISO(now.Add(24 * time.Hour)), "maxUntil": contract.ISO(now.Add(7 * 24 * time.Hour))}, nil
}
func (s *Service) SetDisable(ctx context.Context, p string, data map[string]any, by string, ready []map[string]any) (map[string]any, error) {
	entry, e := ValidateDisable(p, data, by, s.now())
	if e != nil {
		return nil, e
	}
	other := "codex"
	if p == other {
		other = "claude"
	}
	available := false
	for _, r := range ready {
		if r["id"] == other && r["ready"] == true {
			available = true
		}
	}
	if !available {
		return nil, contract.NewProblem(400, other+" の準備ができていません。", "")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('issue-desk-provider-disable'))"); e != nil {
		return nil, e
	}

	var otherRaw []byte
	var otherExpires sql.NullTime
	e = tx.QueryRowContext(ctx, "SELECT value,expires_at FROM settings WHERE key=$1", "providerDisable."+other).Scan(&otherRaw, &otherExpires)
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	if e == nil {
		var otherEntry map[string]any
		if json.Unmarshal(otherRaw, &otherEntry) == nil && activeEntry(otherEntry, otherExpires, s.now()) {
			return nil, contract.NewProblem(400, "Codex と Claude を同時に無効にはできません。先にもう一方を取り消してください。", "")
		}
	}
	raw, e := json.Marshal(entry)
	if e != nil {
		return nil, e
	}
	until, _ := parseUntil(fleet.Text(entry["until"]))
	_, e = tx.ExecContext(ctx, "INSERT INTO settings(key,value,expires_at,updated_at,updated_by) VALUES($1,$2,$3,$4,$5) ON CONFLICT(key) DO UPDATE SET value=excluded.value,expires_at=excluded.expires_at,updated_at=excluded.updated_at,updated_by=excluded.updated_by", "providerDisable."+p, raw, until, s.now(), by)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return entry, nil
}
func (s *Service) Cancel(ctx context.Context, key string) (bool, error) {
	result, e := s.DB.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", key)
	if e != nil {
		return false, e
	}
	count, e := result.RowsAffected()
	return count > 0, e
}

func activeEntry(entry map[string]any, expires sql.NullTime, now time.Time) bool {
	until, e := parseUntil(fleet.Text(entry["until"]))
	if e != nil || !until.After(now) || expires.Valid && !expires.Time.After(now) {
		return false
	}
	set, e := parseUntil(fleet.Text(entry["setAt"]))
	if e != nil || !until.After(set) || until.Sub(set) > 7*24*time.Hour || !safe(entry["by"], 100) {
		return false
	}
	if r, ok := entry["reason"]; ok && !safe(r, 200) {
		return false
	}
	return true
}

func (s *Service) CancelDisable(ctx context.Context, provider string) (bool, error) {
	if provider != "codex" && provider != "claude" {
		return false, contract.NewProblem(400, "無効にできる実行先は codex・claude です。", "")
	}
	entry, e := s.active(ctx, provider)
	if e != nil || entry == nil {
		return false, e
	}
	return s.Cancel(ctx, "providerDisable."+provider)
}
