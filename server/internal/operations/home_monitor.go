package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
	"time"
)

func (s *Service) homeMonitorAlerts(ctx context.Context) ([]any, error) {
	out := []any{}
	if s.PostgreSQL == nil || s.Assignments == nil {
		return out, nil
	}
	v, e := (&hosts.Service{DB: s.PostgreSQL, Now: s.now, StartedAt: s.StartedAt}).HomeMonitor(ctx, s.Assignments)
	if e != nil {
		return nil, e
	}
	m := fleet.Object(v)
	if m["stopped"] != true {
		return out, nil
	}
	host := fleet.Text(m["host"])
	detected := m["lastHeartbeatAt"]
	last := "不明"
	if at, e := time.Parse(time.RFC3339Nano, fleet.Text(detected)); e == nil {
		last = at.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04") + "（日本時間）"
	} else {
		detected = iso(s.now())
	}
	return append(out, map[string]any{"id": "monitor-" + host, "kind": "monitor", "status": "monitor_stopped", "project": fleet.SystemProject, "host": host, "lastHeartbeatAt": m["lastHeartbeatAt"], "detectedAt": detected, "title": "監視が止まっています（" + host + "）", "detail": fmt.Sprintf("PC: %s\n最後の heartbeat: %s\n%s の監視の heartbeat が15分以上届いていません（監視が確認ごとに受付へ送る PUT /api/v1/hosts/%s。受付の hosts 表）。", host, last, host, host)}), nil
}
