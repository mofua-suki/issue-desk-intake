package projects

import "issue-desk-intake/server/internal/fleet"

// presentationSnapshot captures the request-owned record and display state.
// Rendering clones the record and never reads mutable Service state.
type presentationSnapshot struct {
	record     Object
	running    bool
	creatable  bool
	deskNumber func(string, int64) any
}

// presentation must be called with s.mu held. The caller transfers ownership
// of p until rendering completes; all() already returns request-owned records.
func (s *Service) presentation(p Object) presentationSnapshot {
	return presentationSnapshot{record: p, running: s.running[fleet.Text(p["id"])],
		creatable: s.creatable(), deskNumber: s.o.DeskNumber}
}

// present retains the lock contract of single GET and create/mutate responses:
// callers must hold s.mu throughout, including idempotent response branches.
func (s *Service) present(p Object) Object {
	return s.presentation(p).present()
}

func (v presentationSnapshot) present() Object {
	p := clone(v.record)
	p["approvalValid"] = approved(p)
	actions := []string{}
	status := fleet.Text(p["status"])
	busy := v.running
	if !locked(p) && status != "thinking" && !busy {
		actions = append(actions, "chat")
		if status == "error" {
			actions = append(actions, "retry")
		}
	}
	if status == "ready" && p["definition"] != nil {
		actions = append(actions, "approve")
	}
	if approved(p) && v.creatable && !busy && (status == "create_approved" || status == "creation_failed" || status == "creation_blocked" || status == "creating") {
		actions = append(actions, "create")
	}
	if status == "registered" && fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])["registered"] != nil && !busy && fleet.ObjectOrEmpty(p["filing"])["status"] != "filed" && fleet.ObjectOrEmpty(p["filing"])["status"] != "host_waiting" {
		actions = append(actions, "file")
	}
	p["actions"] = actions
	p["creationLocked"] = locked(p)
	p["finishingTurn"] = status != "thinking" && busy
	p["creationUnavailable"] = !v.creatable
	if v.deskNumber != nil {
		for _, issue := range fleet.List(fleet.ObjectOrEmpty(p["filing"])["issues"]) {
			i := fleet.ObjectOrEmpty(issue)
			i["desk"] = v.deskNumber(fleet.Text(i["project"]), fleet.Int64(i["number"]))
		}
	}
	return p
}
