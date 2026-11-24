package operations

import (
	"encoding/json"
	"strings"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
)

// Only filed followups without a concrete question use the saved failure report.
func recoveryFollowupWithoutQuestion(j *jobs.Job) bool {
	if j.Kind != "followup" || j.Status != "held" || j.HoldReason == nil || *j.HoldReason != "question" {
		return false
	}
	questions, ok := j.Result["questions"].([]any)
	if j.Result["questions"] != nil && !ok {
		return false
	}
	for _, q := range questions {
		if _, ok := q.(string); !ok {
			return false
		}
		if strings.TrimSpace(fleet.Text(q)) != "" {
			return false
		}
	}
	return true
}

func recoveryFollowupReport(j *jobs.Job) string {
	b, _ := json.Marshal(map[string]any{"summary": j.Result["summary"], "incomplete": j.Result["incomplete"]})
	return usage.Redact(string(b))
}
