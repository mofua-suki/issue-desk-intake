package screenapi

import "issue-desk-intake/server/internal/fleet"

// listDraftHandoff mirrors the shared classifier for stopped consultations.
func listDraftHandoff(row Object) bool {
	if row["kind"] != "draft" || !oneOf(fleet.Text(row["status"]), "stalled", "ai_stopped", "submit_error", "submit_unknown", "error") || row["executionActive"] == true || row["executionScheduled"] == true || truthy(row["retryAt"]) || row["hasChildren"] == true {
		return false
	}
	for _, key := range []string{"splitChildren", "consultationChildren"} {
		for _, child := range fleet.Items(row[key]) {
			if id, ok := child.(string); ok && id != "" {
				return false
			}
		}
	}
	return true
}

func listFollowupGroup(row Object) bool {
	delayed := row["kind"] == "issue" && row["status"] == "queued" && truthy(row["interruptDelay"])
	return truthy(row["followup"]) && !delayed && !(row["kind"] == "draft" && listAttentionClass(row) == "handoff")
}
