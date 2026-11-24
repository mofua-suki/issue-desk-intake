package notices

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"time"
)

var noticeID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func expiredArchive(row map[string]any, now time.Time) bool {
	at, ok := timestamp(row["removedAt"])
	return ok && contract.ISO(at) == row["removedAt"] && now.Sub(at) > 30*24*time.Hour
}
func readArchive(st settings) ([]any, error) {
	rows, e := st.read("ops-watch-notices-archive")
	if e != nil {
		return nil, e
	}
	for _, raw := range rows {
		r := fleet.Object(raw)
		n := fleet.Object(r["notice"])
		if !contract.FilledText(r["id"], 1000) || !contract.FilledText(n["id"], 1000) {
			return nil, problem(500, "通知の退避記録を確認できません。")
		}
	}
	return rows, nil
}
func (s *Service) Archive(ctx context.Context) ([]any, error) {
	c, e := s.DB.Conn(ctx)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	rows, e := readArchive(settings{c, ctx})
	if e != nil {
		return nil, e
	}
	out := []any{}
	for _, raw := range rows {
		if !expiredArchive(fleet.Object(raw), s.now()) {
			out = append(out, raw)
		}
	}
	return out, nil
}
func (s *Service) Restore(ctx context.Context, id string, body map[string]any) (any, error) {
	if !noticeID.MatchString(id) || len(body) != 0 {
		return nil, problem(400, "退避IDと空の本文を確認してください。")
	}
	c, e := s.DB.Conn(ctx)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	if e = sqlDialect.BeginLockedWrite(ctx, c, "ops-watch-notices"); e != nil {
		return nil, e
	}
	defer c.ExecContext(context.Background(), bindSQL("ROLLBACK"))
	st := settings{c, ctx}
	rows, e := readArchive(st)
	if e != nil {
		return nil, e
	}
	var selected map[string]any
	for _, raw := range rows {
		r := fleet.Object(raw)
		if r["id"] == id {
			selected = r
			break
		}
	}
	if selected == nil || expiredArchive(selected, s.now()) {
		return nil, problem(404, "復元できる退避記録がありません。")
	}
	n := fleet.Object(selected["notice"])
	result := map[string]any{"id": id, "noticeId": n["id"], "restored": true}
	if selected["restoredAt"] == nil {
		current, e := st.read("ops-watch-notices")
		if e != nil {
			return nil, e
		}
		found := false
		for _, raw := range current {
			r := fleet.Object(raw)
			if r["id"] == n["id"] {
				if encode(r) != encode(n) {
					return nil, problem(409, "同じ通知IDの別の通知が保存されています。")
				}
				found = true
			}
		}
		if !found {
			if len(current) >= 1000 {
				return nil, problem(409, "運用の通知が1000件に達しました。")
			}
			if e = st.save("ops-watch-notices", append(current, n)); e != nil {
				return nil, e
			}
		}
		selected["restoredAt"] = contract.ISO(s.now())
		if e = st.save("ops-watch-notices-archive", rows); e != nil {
			return nil, e
		}
	}
	_, e = c.ExecContext(ctx, bindSQL("COMMIT"))
	return result, e
}
func (s *Service) Alerts(ctx context.Context) ([]any, error) {
	c, e := s.DB.Conn(ctx)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	rows, e := (settings{c, ctx}).read("ops-watch-notices")
	if e != nil {
		return nil, e
	}
	out := []any{}
	for _, raw := range rows {
		n := fleet.Object(raw)
		r := map[string]any{"key": "ops-watch:" + fleet.Text(n["id"]), "kind": "ops_watch", "kindName": "運用の見張り", "stateLabel": "お知らせ", "title": n["title"], "reason": n["reason"], "host": n["host"], "updatedAt": n["updatedAt"], "project": nil, "number": nil}
		if fleet.Text(n["cause"]) != "" {
			count := n["count"]
			if count == nil {
				count = 1
			}
			r["cause"], r["count"] = n["cause"], count
			r["reason"] = fmt.Sprintf("%s（同じ原因 %v件）", fleet.Text(n["reason"]), count)
		}
		if targets := fleet.List(n["issues"]); len(targets) > 0 {
			t := fleet.Object(targets[0])
			r["project"], r["number"] = t["repo"], t["number"]
		}
		out = append(out, r)
	}
	return out, nil
}
