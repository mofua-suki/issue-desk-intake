package completionplan

import "encoding/json"

// PendingAfter reads the persisted message boundary. Older records without a
// boundary may only resolve the pending question with the latest user message;
// no missing historical response is inferred during migration.
func PendingAfter(messages, value any) int {
	raw, _ := json.Marshal(messages)
	var rows []map[string]any
	_ = json.Unmarshal(raw, &rows)
	if value != nil {
		raw, _ = json.Marshal(value)
		var after int
		if json.Unmarshal(raw, &after) == nil && after >= 0 && after <= len(rows) {
			return after
		}
		return len(rows) // Invalid boundaries fail closed.
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i]["role"] == "user" {
			return i
		}
	}
	return len(rows)
}
