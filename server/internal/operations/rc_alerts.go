package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"time"
)

func (s *Service) rcAlerts(ctx context.Context) ([]any, error) {
	saved, e := s.setting(ctx, "rc-update-reports")
	if e != nil {
		return nil, e
	}
	reports := fleet.Object(saved)
	for _, raw := range reports {
		r := fleet.Object(raw)
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["receivedAt"]))
		if len(r) != 2 || (r["timer"] != "enabled" && r["timer"] != "disabled") || e != nil || iso(at) != r["receivedAt"] {
			return []any{}, nil
		}
	}
	hosts, e := s.hostRows(ctx)
	if e != nil {
		return nil, e
	}
	out := []any{}
	for _, host := range []string{"a9max", "gt12", "x1-pro1"} {
		for _, row := range hosts {
			if row["host"] != host {
				continue
			}
			heartbeat, e := time.Parse(time.RFC3339Nano, fleet.Text(row["heartbeat_at"]))
			if e != nil || heartbeat.After(s.now()) || s.now().Sub(heartbeat) > 15*time.Minute {
				continue
			}
			r := fleet.Object(reports[host])
			at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["receivedAt"]))
			if e == nil && r["timer"] == "enabled" && s.now().Sub(at) < time.Hour {
				continue
			}
			last, timer := "無し", "無し"
			updated := iso(heartbeat)
			if r != nil {
				last, timer = fleet.Text(r["receivedAt"]), fleet.Text(r["timer"])
				updated = last
			}
			out = append(out, map[string]any{"key": "rc-update:" + host, "kind": "rc_update_missing", "kindName": "運用の見張り", "stateLabel": "お知らせ", "title": "配布タイマー issue-desk-rc-update の報告が届いていません（" + host + "）", "reason": fmt.Sprintf("PC: %s。最後の報告: %s、timer: %s。考えられる原因: タイマーが入っていない・無効、窓口が動かない、監視のイメージが報告に対応していない。確かめ方: systemctl --user list-timers 'issue-desk-rc*'、journalctl --user -u issue-desk-rc-update.service。", host, last, timer), "host": host, "updatedAt": updated, "project": fleet.SystemProject, "number": nil})
		}
	}
	return out, nil
}
