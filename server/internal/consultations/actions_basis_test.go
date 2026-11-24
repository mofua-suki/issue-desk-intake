package consultations

import "testing"

func TestDraftRefusalSubmitsOnceEveryBasisConflictIsResolved(t *testing.T) {
	draft := func(kind string, resolution any) Record {
		return Record{"status": "ready", "ready": true, "basisConflicts": []any{Record{"fact": "前提", "reason": "コードと違う", "kind": kind, "resolution": resolution}}}
	}
	for _, kind := range []string{"requester_fact", "user_decision", "mixed"} {
		for _, action := range []string{"submit", "resubmit"} {
			open := draftRefusal(draft(kind, nil), action, false)
			blank := draftRefusal(draft(kind, "  "), action, false)
			if open != "判断の根拠の食い違いを確認してください。" || blank != open {
				t.Fatal(kind, action, open, blank)
			}
			if got := draftRefusal(draft(kind, "訂正を反映した"), action, false); got == open {
				t.Fatal("resolved conflict still blocks", kind, action)
			}
		}
	}
}
