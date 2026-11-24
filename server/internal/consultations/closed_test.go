package consultations

import "testing"

func TestExplicitlyEndedDraftCannotStartNewWork(t *testing.T) {
	for _, action := range []string{"chat", "retry", "continue_budget", "submit", "resubmit", "edit", "todo", "discard", "finish", "comment"} {
		if reason := draftRefusal(Record{"status": "closed", "cleanup": Record{"reason": "本人指示"}}, action, false); reason == "" {
			t.Fatal(action)
		}
	}
}
