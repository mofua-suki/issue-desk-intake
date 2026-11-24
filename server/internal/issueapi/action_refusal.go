package issueapi

import "issue-desk-intake/server/internal/fleet"

// 表示中の状態で各操作を許すかを判定し、許さないときの理由を返す。

func refusal(d Object, action string) string {
	status := fleet.Text(d["status"])
	switch action {
	case "reply":
		return ""
	case "retry":
		if oneOf(status, "failed", "prerequisite_waiting") || status == "waiting" && d["retryable"] == true {
			return ""
		}
	case "review", "fix":
		if status == "review_held" {
			return ""
		}
	case "resume":
		if status == "stopped" {
			return ""
		}
	case "answer":
		if status == "waiting" {
			return ""
		}
	case "stop":
		if !oneOf(status, "untracked", "pending", "stopped", "completed", "closed", "failed") {
			return ""
		}
	case "priority":
		if oneOf(status, "queued", "dependency_waiting") {
			return ""
		}
	case "handover":
		if d["state"] == "open" && oneOf(status, "untracked", "pending") {
			return ""
		}
	case "approve_permissions":
		target := fleet.ObjectOrEmpty(d["permissionApproval"])
		if d["state"] != "closed" && status == "review_held" && sha.MatchString(fleet.Text(target["head"])) && len(fleet.Items(target["files"])) > 0 && target["unavailable"] == nil {
			return ""
		}
	case "reason":
		if status == "closed" {
			return ""
		}
	case "close":
		if d["state"] == "open" && oneOf(status, "queued", "dependency_waiting", "pending", "untracked", "waiting", "review_held", "scope_waiting", "prerequisite_waiting", "failed", "stopped", "completed") {
			return ""
		}
	}
	return "現在の状態ではこの操作を実行できません。画面を読み込み直してください。"
}
