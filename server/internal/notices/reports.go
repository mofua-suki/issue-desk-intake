package notices

import (
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"time"
)

func (st settings) object(key string) (map[string]any, error) {
	var raw string
	err := st.conn.QueryRowContext(st.ctx, bindSQL("SELECT json FROM settings WHERE id=?"), key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var v map[string]any
	err = json.Unmarshal([]byte(raw), &v)
	if v == nil {
		v = map[string]any{}
	}
	return v, err
}
func (s *Service) rcUpdate(st settings, host string, body map[string]any) (any, error) {
	if len(body) != 2 || !only(body, "kind", "timer") || (body["timer"] != "enabled" && body["timer"] != "disabled") {
		return nil, problem(400, "rc-updateの報告の形が違います。")
	}
	reports, err := st.object("rc-update-reports")
	if err != nil {
		return nil, err
	}
	reports[host] = map[string]any{"timer": body["timer"], "receivedAt": contract.ISO(s.now())}
	return map[string]any{"host": host}, st.save("rc-update-reports", reports)
}
func (s *Service) appHealth(st settings, host string, body map[string]any) (any, error) {
	category := fleet.Text(body["category"])
	if !validAppHealth(body, category) {
		return nil, problem(400, "アプリの健康の報告の形が違います。")
	}
	if err := s.registeredAppHealthRepo(body["repo"]); err != nil {
		return nil, err
	}
	return s.recordAppHealth(st, host, body, category)
}

// validAppHealth はアプリの健康の報告の項目・種類・回数・時刻の形を確かめる。
func validAppHealth(body map[string]any, category string) bool {
	allowed := false
	for _, name := range []string{"ok", "fetch-failed", "store-failed", "disk-low", "stalled", "other"} {
		if category == name {
			allowed = true
		}
	}
	_, timeError := time.Parse(time.RFC3339Nano, fleet.Text(body["at"]))
	if len(body) != 6 || !only(body, "kind", "repo", "category", "count", "at", "intervalMinutes") || !allowed || !fleet.ValidRepo(fleet.Text(body["repo"])) || !integer(body["count"], 1, 1000000) || !integer(body["intervalMinutes"], 5, 1440) || timeError != nil {
		return false
	}
	return true
}

// registeredAppHealthRepo は報告のリポジトリが受付に登録されているかを確かめる。
func (s *Service) registeredAppHealthRepo(repo any) error {
	if s.Repositories == nil {
		return problem(503, "対象リポジトリを照合できません。")
	}
	names, err := s.Repositories()
	if err != nil {
		return err
	}
	registered := false
	for _, name := range names {
		if name == repo {
			registered = true
		}
	}
	if !registered {
		return problem(400, "登録されたリポジトリを指定してください。")
	}
	return nil
}

// recordAppHealth は報告を同じ報告の送り直しと照合したうえで、同じ種類が続いた回数と起票待ちを更新して保存する。
func (s *Service) recordAppHealth(st settings, host string, body map[string]any, category string) (any, error) {
	state, err := st.object("app-health")
	if err != nil {
		return nil, err
	}
	repos := fleet.Object(state["repos"])
	if repos == nil {
		repos = map[string]any{}
	}
	repo := fleet.Text(body["repo"])
	old := fleet.Object(repos[repo])
	if old != nil && old["lastAt"] == body["at"] && old["category"] == category {
		return map[string]any{"accepted": true, "duplicate": true}, nil
	}
	if old == nil && len(repos) >= 50 {
		return nil, problem(409, "健康の報告は50リポジトリまでです。")
	}
	streak := 1.0
	if old["category"] == category {
		streak = fleet.Number(old["streak"]) + 1
	}
	filed := fleet.List(old["filed"])
	if category == "ok" {
		streak = 0
		filed = []any{}
	}
	next := map[string]any{"host": host, "interval": body["intervalMinutes"], "lastRecv": s.now().UnixMilli(), "lastAt": body["at"], "category": category, "streak": streak, "count": body["count"], "filed": filed, "silent": false}
	pending := []any{}
	for _, raw := range fleet.List(state["pending"]) {
		p := fleet.Object(raw)
		if p["repo"] != repo || p["category"] != "silent" && category != "ok" {
			pending = append(pending, raw)
		}
	}
	seen := false
	for _, c := range filed {
		if c == category {
			seen = true
		}
	}
	if category != "ok" && streak >= 2 && !seen {
		next["filed"] = append(filed, category)
		pending = append(pending, map[string]any{"repo": repo, "category": category, "count": body["count"], "lastAt": body["at"]})
	}
	repos[repo] = next
	state["repos"] = repos
	state["pending"] = pending
	return map[string]any{"accepted": true, "duplicate": false}, st.save("app-health", state)
}
