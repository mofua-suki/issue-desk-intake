package consultations

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"issue-desk-intake/server/internal/contract"
)

// ConversationProjection separates the delivered messages from reference records
// that still need to be read. Indices address the original, ordered messages and
// their original references; omitted material is unconfirmed, not an AI summary.
type ConversationProjection struct {
	Messages []any                          `json:"messages"`
	Omitted  []OmittedConversationReference `json:"omitted"`
}

type OmittedConversationReference struct {
	MessageIndex   int            `json:"messageIndex"`
	ReferenceIndex int            `json:"referenceIndex"`
	Unconfirmed    bool           `json:"unconfirmed"`
	Source         map[string]any `json:"source"`
}

// omissionSource copies only scalar identifiers. Its canonical JSON is at most
// 512 UTF-16 units; omittedFields names invalid or oversized supplied values,
// and incomplete indicates that no usable target identifier remains.
func omissionSource(ref any) map[string]any {
	original := record(ref)
	out := map[string]any{"incomplete": true}
	omitted := []string{}
	for _, key := range []string{"desk", "project", "number", "gitea", "source", "version", "target"} {
		value, exists := original[key]
		if !exists {
			continue
		}
		valid := false
		switch v := value.(type) {
		case string:
			valid = v != "" && key != "desk" && key != "number" && key != "gitea"
		case int:
			valid = v > 0 && (key == "desk" || key == "number" || key == "gitea")
		case int64:
			valid = v > 0 && (key == "desk" || key == "number" || key == "gitea")
		case float64:
			valid = v > 0 && v == math.Trunc(v) && !math.IsInf(v, 0) && (key == "desk" || key == "number" || key == "gitea")
		case json.Number:
			n, err := strconv.ParseInt(string(v), 10, 64)
			valid = err == nil && n > 0 && (key == "desk" || key == "number" || key == "gitea")
		}
		if valid {
			out[key] = value
			probe := make(map[string]any, len(out)+1)
			for k, v := range out {
				probe[k] = v
			}
			probe["omittedFields"] = []string{"desk", "project", "number", "gitea", "source", "version", "target"}
			encoded, err := canonicalJSON(probe)
			if err == nil && contract.UTF16Length(encoded) <= 512 {
				continue
			}
			delete(out, key)
		}
		omitted = append(omitted, key)
	}
	if len(omitted) > 0 {
		out["omittedFields"] = omitted
	}
	_, desk := out["desk"]
	_, project := out["project"]
	_, number := out["number"]
	_, gitea := out["gitea"]
	_, source := out["source"]
	_, target := out["target"]
	out["incomplete"] = !(desk || (project && (number || gitea)) || (source && target))
	return out
}

func projectionReferences(value any) ([]any, bool) {
	switch refs := value.(type) {
	case []any:
		return refs, true
	case []map[string]any:
		out := make([]any, len(refs))
		for i, ref := range refs {
			out[i] = ref
		}
		return out, true
	case []Record:
		out := make([]any, len(refs))
		for i, ref := range refs {
			out[i] = ref
		}
		return out, true
	default:
		return nil, false
	}
}

// ProjectionShortfall reports UTF-16 lengths of the smallest representation,
// including the omission list, rather than just the message text.
type ProjectionShortfall struct {
	Limit     int
	Required  int
	Messages  int
	Omitted   int
	Shortfall int
}

func (e *ProjectionShortfall) Error() string {
	return fmt.Sprintf("conversation projection needs %d UTF-16 units (messages %d, omissions %d), limit %d, shortfall %d", e.Required, e.Messages, e.Omitted, e.Limit, e.Shortfall)
}

// projectConversation never edits the stored messages. It retains every message
// and every non-reference field, and only omits whole references. Later callers
// must deliver the omission list with the messages and treat it as unconfirmed.
func projectConversation(messages []any, limit int) (ConversationProjection, error) {
	result := ConversationProjection{Messages: append([]any{}, messages...), Omitted: []OmittedConversationReference{}}
	type position struct{ message, reference int }
	positions := []position{}
	for i, raw := range messages {
		m := record(raw)
		if m == nil {
			continue
		}
		refs, ok := projectionReferences(m["references"])
		if !ok {
			continue
		}
		for j := range refs {
			positions = append(positions, position{i, j})
		}
	}
	// Prefer recent material while preserving the original order of what is sent.
	removed := make(map[position]bool)
	rebuild := func() {
		result.Messages = append([]any{}, messages...)
		result.Omitted = []OmittedConversationReference{}
		for i, raw := range messages {
			m := record(raw)
			if m == nil {
				continue
			}
			refs, ok := projectionReferences(m["references"])
			if !ok {
				continue
			}
			next := make([]any, 0, len(refs))
			for j, ref := range refs {
				if removed[position{i, j}] {
					result.Omitted = append(result.Omitted, OmittedConversationReference{MessageIndex: i, ReferenceIndex: j, Unconfirmed: true, Source: omissionSource(ref)})
				} else {
					next = append(next, ref)
				}
			}
			c := clone(m)
			c["references"] = next
			result.Messages[i] = c
		}
	}
	measure := func(v any) (int, error) {
		encoded, err := canonicalJSON(v)
		if err != nil {
			return 0, err
		}
		return contract.UTF16Length(encoded), nil
	}
	size, err := measure(result)
	if err != nil {
		return ConversationProjection{}, err
	}
	for _, p := range positions {
		if size <= limit {
			break
		}
		removed[p] = true
		rebuild()
		candidate, err := measure(result)
		if err != nil {
			return ConversationProjection{}, err
		}
		if candidate >= size {
			delete(removed, p)
			rebuild()
		} else {
			size = candidate
		}
	}
	if size > limit {
		m, err := measure(result.Messages)
		if err != nil {
			return ConversationProjection{}, err
		}
		o, err := measure(result.Omitted)
		if err != nil {
			return ConversationProjection{}, err
		}
		return result, &ProjectionShortfall{Limit: limit, Required: size, Messages: m, Omitted: o, Shortfall: size - limit}
	}
	return result, nil
}

// ProjectConversation prepares an AI-only view without changing stored messages.
func ProjectConversation(messages []any, limit int) (ConversationProjection, error) {
	return projectConversation(messages, limit)
}
