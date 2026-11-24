package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/deskcleanup"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostdrift"
	"issue-desk-intake/server/internal/hosts"
	"issue-desk-intake/server/internal/usage"
	"strings"
	"time"
)

func (s *Service) hostRows(ctx context.Context) ([]map[string]any, error) {
	if s.PostgreSQL == nil {
		return []map[string]any{}, nil
	}
	rows, e := s.PostgreSQL.QueryContext(ctx, "SELECT row_to_json(h)FROM hosts h ORDER BY host")
	if e != nil {
		return nil, e
	}
	out, _, e := appsql.JSONObjects[[]byte](rows, appsql.JSONRowsOptions{})
	return out, e
}
func (s *Service) hostAlerts(ctx context.Context) ([]any, error) {
	rows, e := s.hostRows(ctx)
	if e != nil {
		return nil, e
	}
	out := []any{}
	now := s.now()
	retired := map[string]bool{}
	if s.Assignments != nil {
		for _, h := range fleet.Strings(s.Assignments.Output["retiredHosts"]) {
			retired[h] = true
		}
	}
	for _, r := range rows {
		host := fleet.Text(r["host"])
		if retired[host] {
			continue
		}
		heartbeat, e := time.Parse(time.RFC3339Nano, fleet.Text(r["heartbeat_at"]))
		alive := e == nil && !heartbeat.After(now) && now.Sub(heartbeat) <= 15*time.Minute
		assigned := false
		if s.Assignments != nil {
			for _, list := range s.Assignments.Implementers {
				for _, h := range list {
					if host == h {
						assigned = true
					}
				}
			}
			for _, h := range s.Assignments.DeployHosts {
				if h == host {
					assigned = true
				}
			}
		}
		if e == nil && now.Sub(heartbeat) > 15*time.Minute && assigned && host != hosts.MonitorHost(s.Assignments) {
			out = append(out, map[string]any{"id": "monitor-" + host, "kind": "monitor", "status": "monitor_stopped", "project": fleet.SystemProject, "host": host, "lastHeartbeatAt": iso(heartbeat), "detectedAt": iso(heartbeat), "title": "監視が止まっています（" + host + "）", "detail": fmt.Sprintf("PC: %s\n最後の heartbeat: %s（日本時間）\n%s の監視の heartbeat が15分以上届いていません（監視が確認ごとに受付へ送る PUT /api/v1/hosts/%s。受付の hosts 表）。", host, heartbeat.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04"), host, host)})
		}
		cycle := fleet.Object(fleet.Object(r["hostd_health"])["cycle"])
		received := fleet.Number(cycle["receivedAt"])
		validReceived := hosts.ValidStoredCycle(cycle) && received > 0 && received <= float64(now.UnixMilli())
		if cycle["phase"] == "wait" && validReceived && float64(now.UnixMilli())-received < 15*60000 {
			out = append(out, map[string]any{"id": "hostd-wait-" + host, "kind": "hostd_retiring", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(time.UnixMilli(int64(received))), "title": "入れ替え待ち（実装中の作業の完了待ち）（" + host + "）", "detail": fmt.Sprintf("PC: %s\n段: %v\n待機対象: %v", host, cycle["stage"], cycle["waitingFor"])})
		}
		first, last := fleet.Number(cycle["firstFailureAt"]), fleet.Number(cycle["lastSuccessAt"])
		if validReceived && cycle["result"] == "failed" && first > 0 && (float64(now.UnixMilli())-first >= 30*60000 || last > 0 && float64(now.UnixMilli())-last > 30*60000) {
			lastText := "成功未受信"
			if last > 0 {
				lastText = iso(time.UnixMilli(int64(last)))
			}
			exception := fleet.Text(cycle["exceptionType"])
			if exception == "" {
				exception = "例外の型名は未受信"
			}
			lines := []string{"PC: " + host, fmt.Sprintf("最新の失敗段: %v", cycle["stage"]), "例外の型名: " + exception}
			lines = append(lines, hosts.CycleDetail(cycle)...)
			lines = append(lines, fmt.Sprintf("続けて失敗した回数: %v回", cycle["failures"]), "最後の成功時刻: "+lastText, fmt.Sprintf("PCの報告時刻: %v", cycle["at"]), "受付時刻: "+iso(time.UnixMilli(int64(received))), "成功周期を受信すると、この要対応は消えます。")
			out = append(out, map[string]any{"id": "hostd-stalled-" + host, "kind": "hostd_stalled", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(time.UnixMilli(int64(first))), "title": "hostd の周期が失敗し続けています（" + host + "）", "detail": usage.Redact(strings.Join(lines, "\n"))})
		}
		capabilities := fleet.Object(r["capabilities"])
		nativeTest, _ := capabilities["nativeTest"].(bool)
		platform, _ := capabilities["platform"].(string)
		registered := s.Assignments != nil && s.Assignments.Hosts[host] != nil
		nativeRunner := registered && s.Assignments.Hosts[host].Role == "test-only" && nativeTest && platform == "darwin"
		if alive && host != "nas" && registered && !nativeRunner && (!validReceived || float64(now.UnixMilli())-received > 60*60000) {
			detected := heartbeat
			if validReceived {
				detected = time.UnixMilli(int64(received)).Add(time.Hour)
			}
			lastText := "未受信（有効な周期の報告がありません）"
			if validReceived {
				lastText = iso(time.UnixMilli(int64(received)))
			}
			out = append(out, map[string]any{"id": "hostd-silent-" + host, "kind": "hostd_stalled", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(detected), "title": "hostd の報告がありません（" + host + "）", "detail": fmt.Sprintf("PC: %s\n最後の周期の受付時刻: %s\nheartbeat の時刻: %s\nUbuntu: bash <配布元の絶対パス>/deploy/client/linux-hostd/install.sh <client-dir> %s <受付URL>", host, lastText, iso(heartbeat), host)})
		}
	}
	home, e := s.homeMonitorAlerts(ctx)
	if e != nil {
		return nil, e
	}
	out = append(out, home...)
	return out, nil
}
func (s *Service) Stalls(ctx context.Context) ([]any, error) {
	out, e := s.collectStalls(ctx)
	if e != nil {
		return nil, e
	}
	filtered := withoutRetiredHosts(s.Assignments, out)
	if s.IncludeArchived {
		return filtered, nil
	}
	return deskcleanup.VisibleAlerts(ctx, s.DB, "stall", filtered)
}

// collectStalls は止まり・異常の要対応を、発生源ごとに読んで集める。
func (s *Service) collectStalls(ctx context.Context) ([]any, error) {
	out := s.runtimeAlerts()
	out = append(out, s.ContractMismatches.Alerts(s.RuntimeDir, s.now())...)
	rows, e := s.hostAlerts(ctx)
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	var driftSnapshot *hostdrift.Snapshot
	if s.Drift != nil && s.Assignments != nil {
		snapshot, e := s.Drift.Evaluate(ctx, s.Assignments)
		if e != nil {
			return nil, e
		}
		driftSnapshot = &snapshot
		out = append(out, snapshot.Alerts...)
	}
	if s.PostgreSQL != nil && s.Assignments != nil {
		v, e := (&hosts.Service{DB: s.PostgreSQL, Now: s.Now}).View(ctx, s.Assignments)
		if e != nil {
			return nil, e
		}
		rows, e = s.dispatchAlerts(ctx, fleet.Object(v))
		if e != nil {
			return nil, e
		}
		out = append(out, rows...)
		out = append(out, s.hostImageAlerts(fleet.Object(v))...)
		out = append(out, imageVersionAlerts(fleet.Object(v), driftSnapshot)...)
	}
	for _, read := range []func(context.Context) ([]any, error){s.usageStaleAlerts, s.canaryAlerts, s.urgentHolds, s.temporaryAlerts, s.designDocAlerts, s.cliAndChecks} {
		rows, e = read(ctx)
		if e != nil {
			return nil, e
		}
		out = append(out, rows...)
	}
	if s.DraftStallAlerts != nil {
		rows, e = s.DraftStallAlerts(ctx)
		if e != nil {
			return nil, e
		}
		out = append(out, rows...)
	}
	for _, read := range []func(context.Context) ([]any, error){s.AIAlerts, s.SystemAlerts} {
		if read != nil {
			rows, e = read(ctx)
			if e != nil {
				return nil, e
			}
			out = append(out, rows...)
		}
	}
	if s.LiveAIAlerts != nil {
		rows, e = s.LiveAIAlerts(ctx)
		if e != nil {
			return nil, e
		}
		out = append(out, rows...)
	}
	return out, nil
}

// withoutRetiredHosts は退役したPCの要対応を外す。
func withoutRetiredHosts(assignments *fleet.Assignments, out []any) []any {
	retired := map[string]bool{}
	if assignments != nil {
		for _, host := range fleet.Strings(assignments.Output["retiredHosts"]) {
			retired[host] = true
		}
	}
	filtered := []any{}
	for _, raw := range out {
		if !retired[fleet.Text(fleet.Object(raw)["host"])] {
			filtered = append(filtered, raw)
		}
	}
	return filtered
}
