package consultationai

import (
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
)

// conversationInput keeps the persisted conversation intact and rejects an oversized projection.
func conversationInput(d Record) (Record, bool, error) {
	projection, err := consultations.ProjectConversation(fleet.Elements(d["messages"]), 80000)
	if err != nil {
		return nil, false, err
	}
	if len(projection.Omitted) == 0 {
		return d, false, nil
	}
	view := make(Record, len(d)+1)
	for key, value := range d {
		view[key] = value
	}
	view["messages"] = projection.Messages
	// The top-level references must reflect the projected latest user message.
	messages := projection.Messages
	for i := len(messages) - 1; i >= 0; i-- {
		message := fleet.Object(messages[i])
		if message["role"] != "user" {
			continue
		}
		if _, exists := d["references"]; exists {
			view["references"] = message["references"]
		}
		break
	}
	view["conversationOmissions"] = Record{
		"positions": projection.Omitted,
		"meaning":   "位置は元の会話と各発言の参照の0始まりの番号です。出典が示す資料の本文は未配信・未確認です。必要なら既存の資料取得経路で確認してください。版のない出典は過去版を指定しません。",
	}
	return view, true, nil
}
