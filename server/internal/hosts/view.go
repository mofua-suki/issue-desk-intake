package hosts

import (
	"context"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"sort"
	"time"
)

func (s *Service) View(ctx context.Context, a *fleet.Assignments) (any, error) {
	live, err := s.readHostRows(ctx)
	if err != nil {
		return nil, err
	}
	ordered := viewHostNames(a, live)
	out := []any{}
	now := s.Now()
	leaseMinutes := fleet.Number(fleet.Object(a.Output["dispatch"])["leaseMinutes"])
	if leaseMinutes <= 0 {
		leaseMinutes = 15
	}
	imageLive := map[string]map[string]any{}
	for _, host := range ordered {
		imageLive[host] = live[host]
	}
	images, rollouts, imageErr := s.imageSnapshots(ctx, imageLive)
	samples, historyErr := s.loadHistory(ctx, now)
	loads, loadsErr := s.repositoryLoads(ctx, now)
	for _, name := range ordered {
		status := live[name]
		stamp := fleet.Text(status["heartbeat_at"])
		at, e := time.Parse(time.RFC3339Nano, stamp)
		alive := e == nil && now.Sub(at) <= time.Duration(leaseMinutes)*time.Minute
		drain := ActiveDrain(fleet.Object(status["drain"]), now, DefaultDuration())
		var ready any
		if status != nil {
			ready = status["ready"] == true
		}
		var updated any
		if e == nil {
			updated = contract.ISO(at)
		}
		var max any
		if host := a.Hosts[name]; host != nil {
			max = host.Max
		}
		health := fleet.Object(status["hostd_health"])
		var cycle any
		if health["cycle"] != nil {
			cycle = map[string]any{"cycle": health["cycle"]}
		}
		row := map[string]any{"name": name, "repositories": a.Repositories(name), "maxImplementations": max, "updatedAt": updated, "alive": alive, "load": status["load"], "implementing": status["implementing"], "publishing": status["publishing"], "capacity": status["capacity"], "ready": ready, "imageRevision": status["image_revision"], "drain": drain, "restartSafe": drain != nil && alive && status["implementing"] == 0.0 && status["publishing"] == 0.0, "intakeHold": status["intake_hold"], "selfCheck": selfChecks(fleet.Object(status["checks"])), "hostdHealth": publicHealth(cycle), "revision": status["revision"], "busy": status["busy"], "activity": nil, "environmentHold": nil, "claimedTotal": nil, "protocol": nil, "config": nil, "driftHold": nil, "rollout": fleet.Object(images[name])["rollout"], "image": fleet.Object(images[name])["image"], "overloaded": overloaded(samples[name], now, fleet.Object(fleet.Object(a.Output["dispatch"])["loadLimit"]), leaseMinutes), "loadHistory": loadSummary(samples[name]), "heartbeatMetrics": map[string]any{"available": false, "complete": false, "reasons": []string{"not_instrumented"}}}
		if host := a.Hosts[name]; host != nil {
			row["role"], row["testRepositories"] = host.Role, host.TestRepositories
		}
		capabilities := fleet.Object(status["capabilities"])
		row["executionPlanVersion"] = capabilities["executionPlanVersion"]
		row["executionCapabilities"] = executionCapabilities(capabilities)
		row["repositoryLoads"] = assignmentLoads(a, name, loads, loadsErr)
		row["hostLoad"] = hostLoad(name, loads, loadsErr)
		if imageErr != nil {
			delete(row, "image")
			row["imageUnavailable"] = "共有DBを読めませんでした"
		}
		if historyErr != nil {
			row["loadHistoryUnavailable"] = true
		}
		out = append(out, row)
	}
	var beat *time.Time
	if t, e := time.Parse(time.RFC3339Nano, fleet.Text(live[MonitorHost(a)]["heartbeat_at"])); e == nil {
		beat = &t
	}
	return map[string]any{"leaseMinutes": leaseMinutes, "homeMonitor": s.monitorSnapshot(a, beat, now), "releaseRollouts": rollouts, "reason": nil, "source": nil, "checkedAt": contract.ISO(now), "hosts": out}, nil
}

// readHostRows は hosts 表の行をPC名ごとに読む。
func (s *Service) readHostRows(ctx context.Context) (map[string]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT row_to_json(h)FROM hosts h ORDER BY host")
	if err != nil {
		return nil, err
	}
	values, _, err := appsql.JSONObjects[[]byte](rows, appsql.JSONRowsOptions{})
	if err != nil {
		return nil, err
	}
	live := map[string]map[string]any{}
	for _, value := range values {
		live[fleet.Text(value["host"])] = value
	}
	return live, nil
}

// viewHostNames は担当表と hosts 表にあるPCのうち、退役していないものを名前の順に並べる。
func viewHostNames(a *fleet.Assignments, live map[string]map[string]any) []string {
	names := map[string]bool{}
	for host := range a.Hosts {
		names[host] = true
	}
	for host := range live {
		names[host] = true
	}
	for _, host := range fleet.Strings(a.Output["retiredHosts"]) {
		delete(names, host)
	}
	ordered := []string{}
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	return ordered
}

// executionCapabilities はPCの申告から、実行環境の能力だけを真偽値（不正な値は nil）で取り出す。
func executionCapabilities(capabilities map[string]any) map[string]any {
	environment := map[string]any{}
	for _, key := range []string{"race", "gcc", "cgo", "docker", "browser", "systemd", "windows", "network"} {
		value, exists := fleet.Object(capabilities["environment"])[key]
		if exists {
			if boolean, ok := value.(bool); ok {
				environment[key] = boolean
			} else {
				environment[key] = nil
			}
		}
	}
	return environment
}

func selfChecks(checks map[string]any) any {
	if checks == nil {
		return nil
	}
	failed := []any{}
	for _, raw := range fleet.List(checks["items"]) {
		item := fleet.Object(raw)
		if item["ok"] == false && len(failed) < 10 {
			failed = append(failed, map[string]any{"name": clip(usage.Redact(fleet.Text(item["name"])), 100), "label": clip(usage.Redact(fleet.Text(item["label"])), 100), "detail": clip(usage.Redact(fleet.Text(item["detail"])), 200)})
		}
	}
	return map[string]any{"at": checks["at"], "ok": checks["ok"] == true, "settingsVersion": checks["settingsVersion"], "failed": failed}
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}
func publicHealth(value any) any {
	health := fleet.Object(fleet.Clone(value))
	cycle := fleet.Object(health["cycle"])
	if cycle == nil || !validPublicCycle(cycle) {
		return nil
	}
	allowed := map[string]bool{}
	for _, key := range []string{"versions", "id", "phase", "waitingFor", "result", "stage", "exceptionType", "reason", "httpStatus", "detail", "dockerFailure", "swapDiagnostic", "at", "receivedAt", "failures", "firstFailureAt", "lastSuccessAt"} {
		allowed[key] = true
	}
	for key := range cycle {
		if !allowed[key] {
			return nil
		}
	}
	if cycle["result"] != "success" && cycle["result"] != "failed" {
		return nil
	}
	received, receivedOK := cycle["receivedAt"].(float64)
	failures, failuresOK := cycle["failures"].(float64)
	if !receivedOK || received <= 0 || !failuresOK || failures < 0 {
		return nil
	}
	safe := map[string]any{}
	for key, value := range cycle {
		safe[key] = value
	}
	for _, key := range []string{"receivedAt", "firstFailureAt", "lastSuccessAt"} {
		if safe[key] != nil {
			n, ok := safe[key].(float64)
			if !ok || n <= 0 || n > received {
				return nil
			}
			safe[key] = contract.ISO(time.UnixMilli(int64(n)))
		}
	}
	for _, key := range []string{"detail", "exceptionType"} {
		if text, ok := safe[key].(string); ok {
			safe[key] = clip(usage.Redact(text), 300)
		}
	}
	return map[string]any{"cycle": safe}
}
