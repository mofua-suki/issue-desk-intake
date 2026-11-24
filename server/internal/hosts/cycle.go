package hosts

import (
	"issue-desk-intake/server/internal/fleet"
	"math"
	"regexp"
	"strings"
	"time"
)

func cycleDetail(v any) bool {
	s, ok := v.(string)
	if !ok || len(s) > 200 || strings.Contains(s, "://") || strings.Contains(strings.ToLower(s), "idh_") || strings.ContainsRune(s, '\\') || regexp.MustCompile(`[A-Za-z]:`).MatchString(s) {
		return false
	}
	for _, r := range s {
		if r < 32 || r > 126 {
			return false
		}
	}
	return true
}
func cycleInteger(v any, min, max float64) bool {
	n, ok := v.(float64)
	return ok && n >= min && n <= max && math.Trunc(n) == n
}
func validPublicCycle(c map[string]any) bool {
	stages := []string{"recover", "docker", "client", "swap-finish", "acquire", "swap-start", "apps", "complete"}
	stageOK := false
	for _, stage := range stages {
		if c["stage"] == stage {
			stageOK = true
		}
	}
	if !stageOK {
		return false
	}
	at, e := time.Parse(time.RFC3339Nano, fleet.Text(c["at"]))
	if e != nil || at.UTC().Format("2006-01-02T15:04:05") != fleet.Text(c["at"])[:19] {
		return false
	}
	_, hasID := c["id"]
	_, hasPhase := c["phase"]
	if hasID != hasPhase {
		return false
	}
	if hasID && (!regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(fleet.Text(c["id"])) || (c["phase"] != "start" && c["phase"] != "wait" && c["phase"] != "finish")) {
		return false
	}
	if c["phase"] == "wait" && (c["result"] != "success" || c["stage"] != "swap-start" || c["waitingFor"] != "drain") {
		return false
	}
	if c["phase"] != "wait" && c["waitingFor"] != nil {
		return false
	}
	if c["phase"] == "start" && c["result"] != "failed" {
		return false
	}
	if reason := c["reason"]; reason != nil {
		valid := false
		for _, name := range []string{"http_400", "http_401", "http_403", "http_404", "http_5xx", "http_other", "network", "timeout", "docker_exit", "docker_timeout", "state", "other"} {
			if reason == name {
				valid = true
			}
		}
		if !valid {
			return false
		}
	}
	if c["detail"] != nil && !cycleDetail(c["detail"]) {
		return false
	}
	if c["exceptionType"] != nil && !regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_`]*(?:\\.[A-Za-z_][A-Za-z0-9_`]*)+(?:\\+[A-Za-z_][A-Za-z0-9_`]*)*$").MatchString(fleet.Text(c["exceptionType"])) {
		return false
	}
	if c["httpStatus"] != nil && !cycleInteger(c["httpStatus"], 100, 599) {
		return false
	}
	if raw, exists := c["versions"]; exists && !publicCycleVersions(fleet.Object(raw)) {
		return false
	}
	if raw := c["dockerFailure"]; raw != nil && !publicCycleDockerFailure(fleet.Object(raw)) {
		return false
	}
	if raw, exists := c["swapDiagnostic"]; exists && !publicCycleSwapDiagnostic(c, fleet.Object(raw)) {
		return false
	}
	received := fleet.Number(c["receivedAt"])
	if c["result"] == "success" {
		return c["failures"] == 0.0 && c["firstFailureAt"] == nil && c["lastSuccessAt"] == received && (c["stage"] == "complete" || c["phase"] == "wait") && c["detail"] == nil && c["dockerFailure"] == nil && c["exceptionType"] == nil && c["reason"] == nil && c["httpStatus"] == nil
	}
	return fleet.Number(c["failures"]) > 0 && fleet.Number(c["firstFailureAt"]) > 0 && fleet.Number(c["firstFailureAt"]) <= received
}

// publicCycleVersions は版の組（current・previous・lastGood）の形と版名を検査する。
func publicCycleVersions(v map[string]any) bool {
	if len(v) != 3 {
		return false
	}
	for _, key := range []string{"current", "previous", "lastGood"} {
		value, exists := v[key]
		if !exists {
			return false
		}
		if value == nil && key != "current" {
			continue
		}
		tag, ok := value.(string)
		if !ok || !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`).MatchString(tag) || strings.HasSuffix(tag, ".staging") || strings.HasSuffix(tag, ".check") {
			return false
		}
	}
	return true
}

// publicCycleDockerFailure はDockerの失敗の記録（コマンドの種類・終了コード・標準エラー）を検査する。
func publicCycleDockerFailure(d map[string]any) bool {
	if len(d) != 3 || !only(d, "command", "exitCode", "stderr") || !cycleDetail(d["stderr"]) || !cycleInteger(d["exitCode"], -2147483648, 2147483647) {
		return false
	}
	for _, name := range []string{"pull", "cp", "compose", "info", "image", "create", "rm", "other"} {
		if d["command"] == name {
			return true
		}
	}
	return false
}

// publicCycleSwapDiagnostic は切り替え開始の失敗に付く診断（容器の状態・ログの末尾・準備完了）を検査する。
func publicCycleSwapDiagnostic(c, d map[string]any) bool {
	if len(d) != 3 || !only(d, "container", "logTail", "ready") || c["result"] != "failed" || c["stage"] != "swap-start" {
		return false
	}
	if d["ready"] != nil {
		if _, ok := d["ready"].(bool); !ok {
			return false
		}
	}
	lines, ok := d["logTail"].([]any)
	if !ok || len(lines) > 40 {
		return false
	}
	for _, line := range lines {
		if !cycleDetail(line) {
			return false
		}
	}
	if raw := d["container"]; raw != nil {
		v := fleet.Object(raw)
		if len(v) != 3 || !only(v, "status", "exitCode", "restartCount") || !regexp.MustCompile(`^[a-z]{1,20}$`).MatchString(fleet.Text(v["status"])) || !cycleInteger(v["exitCode"], -2147483648, 2147483647) || !cycleInteger(v["restartCount"], 0, 2147483647) {
			return false
		}
	}
	return true
}

// ValidStoredCycle checks the persisted health snapshot before operational decisions.
func ValidStoredCycle(c map[string]any) bool { return validPublicCycle(c) }
