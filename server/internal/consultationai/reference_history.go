package consultationai

import (
	"bytes"
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/references"
	"strings"
)

// Keep reference facts on the user message that introduced them. Controller facts
// must not be mistaken for a fresh decision or an assistant's assessment.
func (s *Service) saveResolvedReferences(ctx context.Context, d Record, refs any) error {
	messages := append([]any{}, fleet.Elements(d["messages"])...)
	for i := len(messages) - 1; i >= 0; i-- {
		m := fleet.Object(messages[i])
		if m["role"] != "user" {
			continue
		}
		next := map[string]any{}
		for key, value := range m {
			next[key] = value
		}
		next["references"] = refs
		messages[i] = next
		break
	}
	return s.save(ctx, d, Record{"references": refs, "messages": messages})
}

// promptReferences keeps the fallback for old records whose current reference
// data differs from the references carried by the latest user message.
func promptReferences(d Record) any {
	refs := d["references"]
	if len(fleet.Elements(refs)) == 0 {
		return refs
	}
	messages := fleet.Elements(d["messages"])
	for i := len(messages) - 1; i >= 0; i-- {
		message := fleet.Object(messages[i])
		if message["role"] != "user" {
			continue
		}
		if bytes.Equal(marshal(refs), marshal(message["references"])) {
			return nil
		}
		break
	}
	return refs
}

func holdUnreadHistory(d, answer Record) {
	desks := references.UnreadHistoryDesks(map[string]any(d))
	if len(desks) == 0 {
		return
	}
	question := references.HistoryQuestion(desks)
	controllerQuestion(answer, question, references.HistoryUnreadNotice)
	answer["ready"] = false
	tagged := false
	for _, raw := range fleet.Elements(answer["questionReasons"]) {
		r := fleet.Object(raw)
		tagged = tagged || r["question"] == question && r["controller"] == true
	}
	if !tagged {
		answer["questionReasons"] = append(fleet.Elements(answer["questionReasons"]), map[string]any{"question": question, "reason": references.HistoryUnreadNotice, "controller": true})
	}
	if !strings.Contains(fleet.Text(answer["reply"]), references.HistoryUnreadNotice) {
		answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"]) + "\n\n" + references.HistoryUnreadNotice + "\n" + fmt.Sprintf("%d. %s", len(fleet.Elements(answer["questions"])), question))
	}
}
