package hostd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"regexp"
	"strings"
	"time"
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var exceptionPattern = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_`]*(?:\\.[A-Za-z_][A-Za-z0-9_`]*)+(?:\\+[A-Za-z_][A-Za-z0-9_`]*)*$")

func safeDetail(v any) bool {
	value, ok := v.(string)
	if !ok || len(value) > 200 || strings.Contains(strings.ToLower(value), "idh_") || strings.Contains(value, "://") || strings.Contains(value, "\\") || regexp.MustCompile(`[A-Za-z]:`).MatchString(value) {
		return false
	}
	for _, r := range value {
		if r < 32 || r > 126 {
			return false
		}
	}
	return true
}
func integer(v any, min, max float64) bool {
	n, ok := v.(float64)
	return ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n == math.Trunc(n) && n >= min && n <= max
}
func validTag(v any) bool {
	text := fleet.Text(v)
	return versionPattern.MatchString(text) && !strings.HasSuffix(text, ".staging") && !strings.HasSuffix(text, ".check")
}
func validCycle(body map[string]any) bool {
	if body == nil || !only(body, "versions", "id", "phase", "waitingFor", "result", "stage", "exceptionType", "reason", "httpStatus", "detail", "dockerFailure", "swapDiagnostic", "at") || !one(body["result"], "success", "failed") || !one(body["stage"], "recover", "docker", "client", "swap-finish", "acquire", "swap-start", "apps", "complete") {
		return false
	}
	_, hasID := body["id"]
	_, hasPhase := body["phase"]
	if hasID != hasPhase || hasID && (!idPattern.MatchString(fleet.Text(body["id"])) || !one(body["phase"], "start", "wait", "finish") || body["phase"] == "start" && body["result"] != "failed") {
		return false
	}
	if body["phase"] == "wait" {
		if body["result"] != "success" || body["stage"] != "swap-start" || body["waitingFor"] != "drain" {
			return false
		}
	} else if _, exists := body["waitingFor"]; exists {
		return false
	}
	if detail := body["detail"]; detail != nil && !safeDetail(detail) {
		return false
	}
	if exception, exists := body["exceptionType"]; !exists || exception != nil && (!exceptionPattern.MatchString(fleet.Text(exception)) || contract.UTF16Length(fleet.Text(exception)) > 256) {
		return false
	}
	stamp := fleet.Text(body["at"])
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|\+00:00)$`).MatchString(stamp) || iso(at)[:19] != stamp[:19] {
		return false
	}
	if reason := body["reason"]; reason != nil && !one(reason, "http_400", "http_401", "http_403", "http_404", "http_5xx", "http_other", "network", "timeout", "docker_exit", "docker_timeout", "state", "other") {
		return false
	}
	if status := body["httpStatus"]; status != nil && !integer(status, 100, 599) {
		return false
	}
	if raw, exists := body["versions"]; exists && !validVersions(fleet.Object(raw)) {
		return false
	}
	if raw := body["dockerFailure"]; raw != nil && !validDockerFailure(fleet.Object(raw)) {
		return false
	}
	if raw, exists := body["swapDiagnostic"]; exists && !validSwapDiagnostic(body, fleet.Object(raw)) {
		return false
	}
	return body["result"] != "success" || (body["stage"] == "complete" || body["phase"] == "wait") && body["detail"] == nil && body["dockerFailure"] == nil && body["exceptionType"] == nil && body["reason"] == nil && body["httpStatus"] == nil
}

// validVersions は版の組（current・previous・lastGood）の形と版名を検査する。
func validVersions(v map[string]any) bool {
	return len(v) == 3 && only(v, "current", "previous", "lastGood") && validTag(v["current"]) && (v["previous"] == nil || validTag(v["previous"])) && (v["lastGood"] == nil || validTag(v["lastGood"]))
}

// validDockerFailure はDockerの失敗の記録（コマンドの種類・終了コード・標準エラー）を検査する。
func validDockerFailure(d map[string]any) bool {
	return len(d) == 3 && only(d, "command", "exitCode", "stderr") && one(d["command"], "pull", "cp", "compose", "info", "image", "create", "rm", "other") && integer(d["exitCode"], -2147483648, 2147483647) && safeDetail(d["stderr"])
}

// validSwapDiagnostic は切り替え開始の失敗に付く診断（容器の状態・ログの末尾・準備完了）を検査する。
func validSwapDiagnostic(body, d map[string]any) bool {
	if body["result"] != "failed" || body["stage"] != "swap-start" || len(d) != 3 || !only(d, "container", "logTail", "ready") {
		return false
	}
	if d["container"] != nil {
		c := fleet.Object(d["container"])
		if len(c) != 3 || !only(c, "status", "exitCode", "restartCount") || !regexp.MustCompile(`^[a-z]{1,20}$`).MatchString(fleet.Text(c["status"])) || !integer(c["exitCode"], -2147483648, 2147483647) || !integer(c["restartCount"], 0, 2147483647) {
			return false
		}
	}
	lines, ok := d["logTail"].([]any)
	if !ok || len(lines) > 40 {
		return false
	}
	for _, line := range lines {
		if !safeDetail(line) {
			return false
		}
	}
	if d["ready"] != nil {
		if _, ok := d["ready"].(bool); !ok {
			return false
		}
	}
	return true
}
func nextCycle(body, old map[string]any, at int64) map[string]any {
	if old["id"] != nil && body["id"] == nil {
		return old
	}
	if body["id"] != nil && old["id"] != nil {
		previous, _ := time.Parse(time.RFC3339Nano, fleet.Text(old["at"]))
		current, _ := time.Parse(time.RFC3339Nano, fleet.Text(body["at"]))
		if body["id"] == old["id"] && (old["phase"] == "finish" || body["phase"] == "start") || body["id"] != old["id"] && !current.After(previous) {
			return old
		}
	}
	received := float64(at)
	if v := fleet.Number(old["receivedAt"]); v > received {
		received = v
	}
	next := fleet.Object(fleet.Clone(body))
	next["receivedAt"] = received
	if body["result"] == "success" {
		next["failures"] = 0
		next["firstFailureAt"] = nil
		next["lastSuccessAt"] = received
	} else {
		increment := 1.0
		if body["id"] != nil && body["id"] == old["id"] && old["result"] == "failed" {
			increment = 0
		}
		next["failures"] = math.Min(9007199254740991, fleet.Number(old["failures"])+increment)
		first := old["firstFailureAt"]
		if first == nil {
			first = received
		}
		next["firstFailureAt"] = first
		next["lastSuccessAt"] = old["lastSuccessAt"]
	}
	return next
}
func completeCommand(state, body map[string]any, now time.Time) (map[string]any, error) {
	ok, valid := body["ok"].(bool)
	_, timeError := time.Parse(time.RFC3339Nano, fleet.Text(body["at"]))
	if len(body) != 4 || !only(body, "id", "ok", "reason", "at") || !idPattern.MatchString(fleet.Text(body["id"])) || !valid || !one(body["reason"], "done", "expired", "tag_missing", "verify_failed", "switch_failed", "no_target", "unsupported", "failed") || ok != (body["reason"] == "done") || timeError != nil {
		return nil, problem(400, "指示結果の形が違います。")
	}
	if state == nil {
		state = map[string]any{"command": nil, "pin": nil, "results": []any{}}
	}
	results := fleet.List(state["results"])
	for _, r := range results {
		previous := fleet.Object(r)
		if previous["id"] == body["id"] {
			if previous["ok"] != body["ok"] || previous["reason"] != body["reason"] || previous["at"] != body["at"] {
				return nil, problem(400, "報告済みの指示結果と違います。")
			}
			return state, nil
		}
	}
	command := fleet.Object(state["command"])
	if command == nil || command["id"] != body["id"] {
		return nil, problem(400, "対応する未実行の指示がありません。")
	}
	applyCommandResult(state, command, body, results, ok, now)
	return state, nil
}

// applyCommandResult は指示の結果を記録し、固定・追従の指示なら固定の版を更新して、自動の切り戻しの結果にも書く。
func applyCommandResult(state, command, body map[string]any, results []any, ok bool, now time.Time) {
	if ok && command["kind"] == "pin" {
		state["pin"] = map[string]any{"tag": command["tag"], "id": command["id"], "at": now.UnixMilli(), "by": command["by"]}
	}
	if ok && command["kind"] == "follow" {
		state["pin"] = nil
	}
	result := fleet.Object(fleet.Clone(body))
	result["kind"] = command["kind"]
	result["tag"] = command["tag"]
	result["reportedAt"] = now.UnixMilli()
	results = append(results, result)
	if len(results) > 8 {
		results = results[len(results)-8:]
	}
	state["command"] = nil
	state["results"] = results
	automatic := fleet.Object(state["automaticRollback"])
	for _, raw := range fleet.List(automatic["attempts"]) {
		attempt := fleet.Object(raw)
		if attempt["id"] == body["id"] {
			prefix := "failed:"
			if ok {
				prefix = "success:"
			}
			attempt["result"] = prefix + fleet.Text(body["reason"])
		}
	}
}

func (s *Service) Health(ctx context.Context, host string, body map[string]any, write bool) (any, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, "SELECT hostd_health FROM hosts WHERE host=$1 FOR UPDATE", host).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		if !write {
			return map[string]any{"hostdHealth": nil}, nil
		}
		return map[string]any{"saved": false}, nil
	}
	if err != nil {
		return nil, err
	}
	health := map[string]any{}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &health); err != nil {
			return nil, err
		}
	}
	if health == nil {
		health = map[string]any{}
	}
	if !write {
		out := map[string]any{"hostdHealth": nil}
		if cycle := health["cycle"]; cycle != nil {
			out["hostdHealth"] = map[string]any{"cycle": cycle}
		}
		return out, nil
	}
	if len(body) != 1 {
		return nil, problem(400, "hostd周期の報告の形が違います。")
	}
	if cycle, exists := body["cycle"]; exists {
		v := fleet.Object(cycle)
		if !validCycle(v) {
			return nil, problem(400, "hostd周期の報告の形が違います。")
		}
		health["cycle"] = nextCycle(v, fleet.Object(health["cycle"]), s.now().UnixMilli())
	} else if value, exists := body["commandResult"]; exists {
		commands, e := completeCommand(fleet.Object(health["commands"]), fleet.Object(value), s.now())
		if e != nil {
			return nil, e
		}
		health["commands"] = commands
	} else {
		return nil, problem(400, "hostd周期の報告の形が違います。")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosts SET hostd_health=$2::jsonb WHERE host=$1", host, encode(health)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"saved": true}, nil
}
