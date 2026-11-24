package consultationai

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
)

type callTriggerKey struct{}
type repairDetailKey struct{}
type repairDetail struct{ kind, field string }

// Classify only histories whose latest user message has an unambiguous predecessor.
func initialCallTrigger(d Record, reads *readBudget) string {
	if _, exists := reads.value["inputs"]; exists && fleet.Number(fleet.Object(d["consultationBudget"])["calls"]) == 0 {
		return ""
	}
	if reads.newInput() {
		return "user_reply"
	}
	messages := fleet.Elements(d["messages"])
	users, assistant := 0, false
	for _, item := range messages {
		switch fleet.Text(fleet.Object(item)["role"]) {
		case "user":
			users++
		case "assistant":
			assistant = true
		default:
			return ""
		}
	}
	if len(messages) == 0 || fleet.Text(fleet.Object(messages[len(messages)-1])["role"]) != "user" {
		return ""
	}
	if users == 1 && !assistant {
		calls := fleet.Number(fleet.Object(d["consultationBudget"])["calls"])
		if calls == 0 {
			return "initial"
		}
		history := fleet.Elements(d["aiDiagnostics"])
		if len(history) > 0 {
			last := fleet.Object(history[len(history)-1])
			kind := fleet.Text(last["kind"])
			if fleet.Number(last["calls"]) == calls && fleet.Text(last["step"]) == usage.DraftStep && kind != "" && kind != "completed" && kind != "canceled_by_server" {
				return "failure_retry"
			}
		}
		return "recovery_continuation"
	}
	if _, exists := reads.value["inputs"]; exists {
		return ""
	}
	if users > 1 && assistant {
		return "user_reply"
	}
	return ""
}
