package consultations

import "issue-desk-intake/server/internal/fleet"

// MarkSplitCancellation records the exact AI reply before controller notices are appended.
func MarkSplitCancellation(answer map[string]any) {
	answer["_cancelSplitIssues"] = text(answer["reply"]) == "分割案をすべて取り消します。"
}

// ApplySplitAnswer protects an existing proposal from an unintentional empty answer.
func ApplySplitAnswer(current, value any, answer map[string]any) bool {
	if len(fleet.Items(current)) == 0 || len(fleet.Items(value)) > 0 {
		return true
	}
	if value == nil {
		return false
	}
	if marked, ok := answer["_cancelSplitIssues"].(bool); ok {
		return marked
	}
	return text(answer["reply"]) == "分割案をすべて取り消します。"
}
