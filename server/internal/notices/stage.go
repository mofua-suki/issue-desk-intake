package notices

import (
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"time"
)

func validStage(body map[string]any) bool {
	keys := []string{"kind", "event", "id", "operationId", "name", "at", "startedAt", "deadline", "target", "observation", "lastSuccessAt", "holder", "exitCode", "outputs", "anomalies"}
	if len(body) != len(keys) || !only(body, keys...) {
		return false
	}
	uuid := regexp.MustCompile(`^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
	if !uuid.MatchString(fleet.Text(body["id"])) || !uuid.MatchString(fleet.Text(body["operationId"])) || !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(fleet.Text(body["name"])) || body["holder"] != "watcher" {
		return false
	}
	at, ok := timestamp(body["at"])
	start, startOK := timestamp(body["startedAt"])
	if !ok || !startOK || at.Before(start) || contract.ISO(at) != body["at"] || contract.ISO(start) != body["startedAt"] {
		return false
	}
	for _, key := range []string{"deadline", "lastSuccessAt"} {
		if body[key] != nil {
			t, ok := timestamp(body[key])
			if !ok || contract.ISO(t) != body[key] || key == "lastSuccessAt" && t.After(at) {
				return false
			}
		}
	}
	event := fleet.Text(body["event"])
	if event != "start" && event != "heartbeat" && event != "result" && event != "verified" {
		return false
	}
	observation := fleet.Text(body["observation"])
	if observation != "process" && observation != "issue-status" && observation != "entry-health" {
		return false
	}
	if event == "verified" && (observation == "process" || body["lastSuccessAt"] != body["at"] || body["exitCode"] != nil) {
		return false
	}
	if body["exitCode"] != nil && !integer(body["exitCode"], -2147483648, 4294967295) || body["outputs"] != nil && !integer(body["outputs"], 0, 1000000) {
		return false
	}
	anomalies, ok := body["anomalies"].([]any)
	if !ok || len(anomalies) > 4 {
		return false
	}
	for _, value := range anomalies {
		if value != "exit" && value != "empty" && value != "late" && value != "overdue" {
			return false
		}
	}
	if event != "result" && (body["exitCode"] != nil || body["outputs"] != nil || len(anomalies) != 0) {
		return false
	}
	if body["target"] != nil {
		target := fleet.Object(body["target"])
		if len(target) != 2 || !only(target, "project", "number") || !fleet.ValidRepo(fleet.Text(target["project"])) || !integer(target["number"], 1, 9007199254740991) {
			return false
		}
	}
	return true
}
func sameRun(a, b map[string]any) bool {
	for _, key := range []string{"name", "startedAt", "deadline", "holder", "target"} {
		if encode(a[key]) != encode(b[key]) {
			return false
		}
	}
	return true
}
func (s *Service) stageOperation(st settings, host string, body map[string]any) (any, error) {
	if err := s.checkStageOperation(body); err != nil {
		return nil, err
	}
	rows, err := st.read("stage-watch-operations")
	if err != nil {
		return nil, err
	}
	rows, replayed, err := appendStageEvent(rows, host, body)
	if err != nil {
		return nil, err
	}
	if replayed {
		return map[string]any{"id": body["id"], "operationId": body["operationId"]}, nil
	}
	return map[string]any{"id": body["id"], "operationId": body["operationId"]}, st.save("stage-watch-operations", rows)
}

// checkStageOperation は段取りの運用記録の形・時刻・対象のリポジトリを確かめる。
func (s *Service) checkStageOperation(body map[string]any) error {
	if !validStage(body) {
		return problem(400, "段取りの運用記録の形が違います。")
	}
	at, _ := timestamp(body["at"])
	if at.After(s.now().Add(time.Minute)) {
		return problem(400, "運用記録の時刻が未来です。")
	}
	if target := fleet.Object(body["target"]); target != nil {
		if s.Repositories == nil {
			return problem(503, "対象リポジトリを照合できません。")
		}
		repos, err := s.Repositories()
		if err != nil {
			return err
		}
		found := false
		for _, repo := range repos {
			if repo == target["project"] {
				found = true
			}
		}
		if !found {
			return problem(400, "運用対象のリポジトリが設定にありません。")
		}
	}
	return nil
}

// appendStageEvent は運用記録の出来事を同じ運用の行へ足し、最新の出来事を更新する。同じ送信IDの送り直しなら replayed を返す。
func appendStageEvent(rows []any, host string, body map[string]any) ([]any, bool, error) {
	var row map[string]any
	for _, raw := range rows {
		r := fleet.Object(raw)
		if r["id"] == body["operationId"] {
			row = r
			if row["host"] != host {
				return nil, false, problem(409, "記録の保持PCが違います。")
			}
		}
		for _, rawEvent := range fleet.List(r["events"]) {
			e := fleet.Object(rawEvent)
			if e["id"] == body["id"] {
				if r["id"] != body["operationId"] || encode(e) != encode(body) {
					return nil, false, problem(409, "送信IDの内容が違います。")
				}
				return rows, true, nil
			}
		}
	}
	if row != nil {
		latest := fleet.Object(row["latest"])
		if !sameRun(latest, body) {
			return nil, false, problem(409, "運用対象・保持者は変更できません。")
		}
		row["events"] = append(fleet.List(row["events"]), body)
		if fleet.Text(body["at"]) > fleet.Text(latest["at"]) || body["at"] == latest["at"] && body["event"] == "result" {
			row["latest"] = body
		}
	} else {
		rows = append(rows, map[string]any{"id": body["operationId"], "host": host, "latest": body, "events": []any{body}})
	}
	return rows, false, nil
}
