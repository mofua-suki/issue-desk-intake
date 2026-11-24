package completionplan

import (
	"encoding/json"
	"strings"
)

// AllowsCandidateRewrite is separate from receipt validity. An unrelated user
// answer preserves the receipt, but requires strict comparison of new candidates
// so a new instruction cannot be mistaken for an AI-only prose rewrite.
// Callers must validate the receipt against its saved presentation first.
func AllowsCandidateRewrite(receipt Receipt, messages any) bool {
	raw, _ := json.Marshal(messages)
	var rows []map[string]any
	if json.Unmarshal(raw, &rows) != nil || receipt.MessageID == "" {
		return false
	}
	found := false
	for _, row := range rows {
		if row["role"] != "user" {
			continue
		}
		if row["id"] == receipt.MessageID {
			found = true
			continue
		}
		if found {
			value, _ := row["text"].(string)
			switch strings.TrimSpace(value) {
			case "説明して", "説明してください", "詳しく説明してください", "詳しく教えてください", "この計画を説明して", "この計画を説明してください", "計画の説明をお願いします":
			default:
				return false
			}
		}
	}
	return found
}
