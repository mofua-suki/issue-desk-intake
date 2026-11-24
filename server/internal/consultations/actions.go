package consultations

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

func has(d Record, key string) bool { return len(fleet.Items(d[key])) > 0 }
func is(value string, values ...string) bool {
	for _, v := range values {
		if value == v {
			return true
		}
	}
	return false
}
func draftRefusal(d Record, action string, busy bool) string {
	status := text(d["status"])
	if status == "closed" {
		return "利用者の指示で終了した相談です。"
	}
	if action == "stop" {
		if status == "thinking" && d["issue"] == nil && !has(d, "issues") && !has(d, "investigations") {
			return ""
		}
		return "実行中または待機中の未起票の相談だけを停止できます。"
	}
	filed := is(status, "submitted", "submitting", "submit_unknown", "auto_ready")
	confirmed := d["ready"] == true && !has(d, "questions")
	stoppedThinking := status == "thinking" && d["_stopped"] == true
	if d["kind"] == "investigation" && is(action, "chat", "retry", "continue_budget", "submit", "resubmit", "edit", "todo") {
		return "調査の依頼は新規の実行を受け付けません。"
	}
	if children := len(fleet.Items(d["consultationChildren"])); children > 0 {
		parts := len(fleet.Items(d["consultationParts"]))
		if d["consultationParts"] == nil {
			if projected, ok := integer(d["_consultationPartCount"]); ok {
				parts = int(projected)
			}
		}
		resume := action == "retry" && children >= 2 && parts == children
		if !resume {
			return "子の相談へ引き継いでいます。各子の相談で続けてください。"
		}
	}
	if status == "project_handoff" && is(action, "chat", "retry", "submit", "resubmit", "edit", "todo", "discard", "proxy_undo") {
		return "新しいプロジェクトの相談へ引き継いでいます。"
	}
	budget := record(d["consultationBudget"])
	if (budget["stopped"] == true || record(budget["stopped"]) != nil) && is(action, "chat", "retry") {
		return "相談の上限で停止しています。「上限を足して続行する」を選んでください。"
	}
	if is(action, "submit", "resubmit") {
		for _, item := range fleet.Items(d["basisConflicts"]) {
			conflict := record(item)
			if strings.TrimSpace(text(conflict["resolution"])) == "" {
				return "判断の根拠の食い違いを確認してください。"
			}
		}
	}
	switch action {
	case "chat":
		if filed || status == "thinking" && !stoppedThinking {
			return "現在この下書きは変更できません。"
		}
		if busy {
			return "前回の処理を終了中です。"
		}
		return ""
	case "retry":
		if !is(status, "error", "stalled") && !stoppedThinking {
			return "再試行できる相談はありません。"
		}
		if busy {
			return "前回の処理を終了中です。"
		}
		return ""
	case "continue_budget":
		if status != "draft" || budget["stopped"] != true && record(budget["stopped"]) == nil {
			return "上限で停止している相談ではありません。"
		}
		if busy {
			return "前回の処理を終了中です。"
		}
		return ""
	case "submit":
		if status == "submitting" || is(status, "draft", "auto_ready") && confirmed {
			return ""
		}
		return "下書きを確認してから送信してください。"
	case "resubmit":
		if status == "submit_unknown" || status == "submit_error" && confirmed {
			return ""
		}
		return "下書きを確認してから送信してください。"
	case "comment":
		if status == "submitted" && record(d["issue"])["number"] != nil {
			return ""
		}
		return "起票済みの相談ではありません。"
	case "finish":
		if status == "auto_ready" && record(d["submissionRequest"]) != nil {
			return "起票を受付済みです。GET /api/drafts/" + text(d["id"]) + " で結果を確認してください。"
		}
		if status == "submitting" {
			return "起票処理中です。GET /api/drafts/" + text(d["id"]) + " で結果を確認してください。"
		}
		if status == "submit_unknown" {
			return "起票結果が不明です。GET /api/drafts/" + text(d["id"]) + " と対象リポジトリで結果を照合してください。"
		}
		if d["issue"] != nil || has(d, "issues") {
			issues := d["issue"]
			if issues == nil {
				issues = d["issues"]
			}
			return fmt.Sprintf("起票済みです（Issue: %v）。内容を確認し、必要なら停止・クローズを検討してください。", issues)
		}
		if busy && status != "thinking" && status != "auto_ready" {
			return "前回の処理を終了中です。"
		}
		if is(status, "draft", "intake_waiting", "submit_error", "investigating", "investigation_ready", "investigation_error", "error", "stalled", "thinking", "auto_ready") {
			return ""
		}
		return "未起票または調査結果待ちの相談ではありません。"
	case "edit":
		if status == "draft" && d["ready"] == true {
			return ""
		}
		return "AIとの確認を先に完了してください。"
	case "todo":
		if d["unassigned"] == true && d["project"] == nil {
			return "プロジェクト未定の相談はTODOに戻せません。"
		}
		if d["proposal"] != nil || d["issue"] != nil || has(d, "investigations") || filed || status == "submit_error" {
			return "起票済み・起票中の相談はTODOに戻せません。"
		}
		if status == "thinking" && !stoppedThinking {
			if d["queued"] != true && !(d["retryAt"] != nil && !busy) {
				return "AIが整理中の相談はTODOに戻せません。"
			}
		} else if busy {
			return "前回の処理を終了中です。"
		}
		for _, entry := range fleet.Items(d["messages"]) {
			if record(entry)["role"] == "user" {
				return ""
			}
		}
		return "依頼文が無い相談はTODOに戻せません。"
	case "discard":
		if d["issue"] != nil || has(d, "investigations") || filed {
			return "起票済み・起票中の相談は破棄できません。"
		}
		if status == "thinking" && !stoppedThinking || busy {
			return "AIの応答を待っています。"
		}
		return ""
	case "proxy_undo":
		if filed || status == "submit_error" || d["issue"] != nil || has(d, "issues") || has(d, "investigations") || d["parentSplit"] != nil || has(d, "splitChildren") {
			return "起票済み・起票中の相談では代理の回答を取り消せません。"
		}
		for _, entry := range fleet.Items(d["messages"]) {
			if record(entry)["role"] == "proxy" {
				return ""
			}
		}
		return "取り消せる代理の回答がありません。"
	}
	return "操作が不明です。"
}

func (s *Service) refusal(d Record, action string) string {
	return s.refusalWithFamily(d, action, s.familyViewRefusal)
}

func (s *Service) refusalWithFamily(d Record, action string, family func(Record, string) string) string {
	if d["status"] == "closed" {
		return "利用者の指示で終了した相談です。"
	}
	busy := false
	engine := s.engineSnapshot()
	if engine.Active != nil {
		busy = engine.Active(text(d["id"]))
		if text(d["status"]) == "thinking" && !busy {
			d = clone(d)
			d["_stopped"] = true
		}
	}
	if (d["parentSplit"] != nil || has(d, "splitChildren")) && is(action, "todo", "discard", "finish", "resubmit") {
		return family(d, action)
	}
	return draftRefusal(d, action, busy)
}

// retryHiddenForMeasurement affects presentation only; retry requests retain
// their existing refusal and execution behavior.
func retryHiddenForMeasurement(d Record) bool {
	budget := record(d["consultationBudget"])
	var unknown float64
	switch n := budget["unknownCalls"].(type) {
	case json.Number:
		unknown, _ = n.Float64()
	case float64:
		unknown = n
	case int:
		unknown = float64(n)
	case int64:
		unknown = float64(n)
	}
	return d["usageMeasurementHold"] != nil || budget["pendingCall"] != nil || unknown > 0
}
