package hostdrift

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"time"
)

type settingsDrift struct {
	Version string
	Since   time.Time
}

func (s *Service) settingsAlerts(live []map[string]any, version string, now time.Time) []any {
	if s.settingsSeen == nil {
		s.settingsSeen = map[string]settingsDrift{}
	}
	out := []any{}
	seen := map[string]bool{}
	for _, r := range live {
		host := fleet.Text(r["host"])
		current := fleet.Text(fleet.Object(r["checks"])["settingsVersion"])
		if current == "" || version == "" || current == version {
			continue
		}
		seen[host] = true
		key := current + ":" + version
		state, ok := s.settingsSeen[host]
		if !ok || state.Version != key {
			state = settingsDrift{key, now}
			s.settingsSeen[host] = state
		}
		if now.Sub(state.Since) <= 30*time.Minute {
			continue
		}
		out = append(out, map[string]any{"id": "host-drift-settings-" + host, "kind": "host_drift", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(state.Since), "title": "運用の設定の版が受付と違います（" + host + "）", "detail": fmt.Sprintf("PC: %s\nPCの版: %s / 受付の版: %s\nずれを見た時刻: %s（日本時間）\nこのPCの監視が運用の設定を読めていません。監視の起動ログで理由を確かめてください。そろうと消えます。", host, current, version, state.Since.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04"))})
	}
	for host := range s.settingsSeen {
		if !seen[host] {
			delete(s.settingsSeen, host)
		}
	}
	return out
}
