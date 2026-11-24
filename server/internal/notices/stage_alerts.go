package notices

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"time"
)

func stageStatus(event map[string]any, now time.Time) string {
	if event["event"] == "verified" {
		return "確認済み"
	}
	if deadline, ok := timestamp(event["deadline"]); ok && !now.Before(deadline) {
		return "期限超過"
	}
	if event["event"] == "result" {
		return "結果待ち"
	}
	if at, ok := timestamp(event["at"]); ok && now.Sub(at) >= 2*time.Minute {
		return "heartbeat失効"
	}
	return "実行中"
}
func stageAction(event map[string]any, state, host string) string {
	file := "~/.issue-desk/stage-watch/" + fleet.Text(event["name"]) + ".json"
	switch state {
	case "実行中":
		return "watcherのheartbeatを受信しています。期限内にresultを待ち、期限を過ぎたら対象と出力を確認してください。"
	case "結果待ち":
		return "段取りの出力と" + file + "を確認し、対象の状態をissue-statusまたはentry-healthで実観測して成功確認を報告してください。"
	case "確認済み":
		return "実観測による成功確認を記録済みです。"
	case "期限超過":
		return "期限を超えています。段取りの出力と" + file + "を確認し、対象の実状態を確かめて、未完了なら再実行が必要か判断してください。"
	default:
		return fmt.Sprintf("heartbeatが失効しています。保持PC %sのwatcher（%s）と%sを確認し、対象の実状態を独立に確かめて、未完了ならwatcherの再開または段取りの再実行を行ってください。", host, fleet.Text(event["name"]), file)
	}
}
func (s *Service) StageAlerts(ctx context.Context) ([]any, error) {
	c, e := s.DB.Conn(ctx)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	rows, e := (settings{c, ctx}).read("stage-watch-operations")
	if e != nil {
		return nil, e
	}
	out := []any{}
	configured := map[string]bool{}
	if s.Repositories != nil {
		repos, e := s.Repositories()
		if e != nil {
			return nil, e
		}
		for _, repo := range repos {
			configured[repo] = true
		}
	}
	for _, raw := range rows {
		id, host, latest, matching, ok := latestStageEvent(fleet.Object(raw))
		if !ok {
			continue
		}
		target := fleet.Object(latest["target"])
		if target != nil && !configured[fleet.Text(target["project"])] {
			continue
		}
		out = append(out, stageAlert(id, host, latest, matching, s.now()))
	}
	return out, nil
}

// latestStageEvent は段取りの記録1件から、正しい形の出来事のうち保存済みの最新と一致する出来事を選ぶ。形が崩れていれば ok=false を返す。
func latestStageEvent(row map[string]any) (string, string, map[string]any, []map[string]any, bool) {
	id, host := fleet.Text(row["id"]), fleet.Text(row["host"])
	events := fleet.List(row["events"])
	if id == "" || host == "" || len(events) == 0 {
		return "", "", nil, nil, false
	}
	valid := true
	matching := []map[string]any{}
	for _, raw := range events {
		event := fleet.Object(raw)
		if !validStage(event) {
			valid = false
			break
		}
		if event["operationId"] == id {
			matching = append(matching, event)
		}
	}
	if !valid || len(matching) == 0 {
		return "", "", nil, nil, false
	}
	latest := matching[0]
	for _, ev := range matching[1:] {
		a, b := fleet.Text(latest["at"]), fleet.Text(ev["at"])
		if b > a || b == a && (ev["event"] == "result" || ev["event"] == "verified" && latest["event"] != "result") {
			latest = ev
		}
	}
	saved := fleet.Object(row["latest"])
	if !validStage(saved) || saved["operationId"] != id {
		return "", "", nil, nil, false
	}
	same := false
	for _, ev := range matching {
		if ev["id"] == saved["id"] && encode(ev) == encode(saved) {
			same = true
		}
	}
	legacy := saved["event"] == "verified" && latest["event"] == "result" && saved["at"] == latest["at"] && same
	if saved["id"] != latest["id"] && !legacy {
		return "", "", nil, nil, false
	}
	return id, host, latest, matching, true
}

// stageAlert は段取りの最新の出来事と経過から、要対応の行を組み立てる。
func stageAlert(id, host string, latest map[string]any, matching []map[string]any, now time.Time) map[string]any {
	target := fleet.Object(latest["target"])
	state := stageStatus(latest, now)
	ordered := append([]map[string]any{}, matching...)
	sort.SliceStable(ordered, func(i, j int) bool { return fleet.Text(ordered[i]["at"]) > fleet.Text(ordered[j]["at"]) })
	var heartbeat, verification, monitor, lastSuccess any
	monitor = matching[0]["observation"]
	for _, ev := range matching {
		if ev["event"] == "start" {
			monitor = ev["observation"]
			break
		}
	}
	for _, ev := range ordered {
		if heartbeat == nil && (ev["event"] == "start" || ev["event"] == "heartbeat") {
			heartbeat = ev["at"]
		}
		if verification == nil && ev["event"] == "verified" {
			verification = ev["observation"]
		}
		if fleet.Text(ev["lastSuccessAt"]) > fleet.Text(lastSuccess) {
			lastSuccess = ev["lastSuccessAt"]
		}
	}
	anomalies := any([]any{})
	var exit, outputs any
	if latest["event"] == "result" {
		anomalies, exit, outputs = latest["anomalies"], latest["exitCode"], latest["outputs"]
	}
	return map[string]any{"key": "stage-watch:" + id, "kind": "ops_watch", "stageWatch": true, "title": fleet.Text(latest["name"]) + "・" + state, "stateLabel": state, "host": host, "name": latest["name"], "target": latest["target"], "project": target["project"], "number": target["number"], "holder": latest["holder"], "startedAt": latest["startedAt"], "deadline": latest["deadline"], "heartbeatAt": heartbeat, "observation": latest["observation"], "monitorObservation": monitor, "successObservation": verification, "lastSuccessAt": lastSuccess, "event": latest["event"], "anomalies": anomalies, "exitCode": exit, "outputs": outputs, "updatedAt": latest["at"], "reason": stageAction(latest, state, host)}
}
