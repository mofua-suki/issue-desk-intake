package hosts

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// CycleDetail formats only validated stored data, including the diagnostic
// fields that explain why an image swap failed.
func CycleDetail(c map[string]any) []string {
	if !ValidStoredCycle(c) {
		return nil
	}
	out := []string{}
	if reason := fleet.Text(c["reason"]); reason != "" {
		if c["httpStatus"] != nil {
			reason += fmt.Sprintf("（HTTP %v）", c["httpStatus"])
		}
		out = append(out, "失敗の理由: "+reason)
	}
	if swap := fleet.Object(c["swapDiagnostic"]); swap != nil {
		state := "状態を取得できませんでした"
		if container := fleet.Object(swap["container"]); container != nil {
			state = fmt.Sprintf("%v / exit %v / restart %v", container["status"], container["exitCode"], container["restartCount"])
		}
		logs := fleet.Strings(swap["logTail"])
		line := fmt.Sprintf("直近のログ %d行", len(logs))
		if len(logs) > 0 {
			line += "\n  " + strings.Join(logs, "\n  ")
		}
		ready := "未取得"
		if v, ok := swap["ready"].(bool); ok {
			ready = fmt.Sprint(v)
		}
		out = append(out, "新しいclientのコンテナ: "+state, line, "最後の ready: "+ready)
	}
	if detail := fleet.Text(c["detail"]); detail != "" {
		out = append(out, "失敗の詳細: "+detail)
	}
	if docker := fleet.Object(c["dockerFailure"]); docker != nil {
		out = append(out, fmt.Sprintf("Dockerの失敗: %v (exit %v) %v", docker["command"], docker["exitCode"], docker["stderr"]))
	}
	if versions := fleet.Object(c["versions"]); versions != nil && versions["lastGood"] == nil {
		candidate := "previousも未記録のため候補なし"
		if versions["previous"] != nil {
			candidate = "previousを候補として検査（成功確認済みとは限りません）"
		}
		out = append(out, "切り戻し候補: lastGood未記録。"+candidate)
	}
	return out
}
