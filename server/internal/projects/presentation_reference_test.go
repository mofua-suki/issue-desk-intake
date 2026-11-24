package projects

import "issue-desk-intake/server/internal/fleet"

func referencePresent09f2c006(s *Service, p Object) Object {
	p = clone(p)
	p["approvalValid"] = approved(p)
	actions := []string{}
	status := fleet.Text(p["status"])
	busy := s.running[fleet.Text(p["id"])]
	if !locked(p) && status != "thinking" && !busy {
		actions = append(actions, "chat")
		if status == "error" {
			actions = append(actions, "retry")
		}
	}
	if status == "ready" && p["definition"] != nil {
		actions = append(actions, "approve")
	}
	if approved(p) && s.creatable() && !busy && (status == "create_approved" || status == "creation_failed" || status == "creation_blocked" || status == "creating") {
		actions = append(actions, "create")
	}
	if status == "registered" && fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])["registered"] != nil && !busy && fleet.ObjectOrEmpty(p["filing"])["status"] != "filed" && fleet.ObjectOrEmpty(p["filing"])["status"] != "host_waiting" {
		actions = append(actions, "file")
	}
	p["actions"] = actions
	p["creationLocked"] = locked(p)
	p["finishingTurn"] = status != "thinking" && busy
	p["creationUnavailable"] = !s.creatable()
	if s.o.DeskNumber != nil {
		for _, v := range fleet.List(fleet.ObjectOrEmpty(p["filing"])["issues"]) {
			i := fleet.ObjectOrEmpty(v)
			i["desk"] = s.o.DeskNumber(fleet.Text(i["project"]), fleet.Int64(i["number"]))
		}
	}
	return p
}
