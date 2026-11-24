package consultations

import "issue-desk-intake/server/internal/fleet"

import "strings"

// stopQuestionlessOutcome prevents a completed turn from leaving an unactionable draft.
func stopQuestionlessOutcome(next, result Record) {
	if result["_continue"] == true || next["status"] != "draft" || next["ready"] == true || next["hold"] == true || text(next["parentSplit"]) != "" {
		return
	}
	budget := record(next["consultationBudget"])
	if budget["stopped"] == true || record(budget["stopped"]) != nil || len(fleet.Items(result["consultationChildren"])) > 0 || len(fleet.Items(next["splitChildren"])) > 0 || record(result["newProject"])["needed"] == true {
		return
	}
	for _, q := range fleet.Items(next["questions"]) {
		if strings.TrimSpace(text(q)) != "" {
			return
		}
	}
	next["questions"] = []any{}
	next["status"], next["ready"], next["retryAt"] = "stalled", false, nil
	SetDraftError(next, "起票に必要な確認を特定できず、質問のないまま相談が終了したため停止しました。保存された本文と応答を確認してください。", nil)
}
