package issueapi

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"time"
)

// issueUpdatedAt preserves the upstream value only when no candidate can be parsed.
func issueUpdatedAt(upstream any, job *jobs.Job) any {
	candidates := []string{fleet.Text(upstream)}
	if job != nil {
		if job.ProgressAt != nil {
			candidates = append(candidates, *job.ProgressAt)
		}
		candidates = append(candidates, job.UpdatedAt)
	}
	var latest time.Time
	found := false
	for _, candidate := range candidates {
		parsed, err := time.Parse(time.RFC3339Nano, candidate)
		if err == nil && (!found || parsed.After(latest)) {
			latest, found = parsed, true
		}
	}
	if !found {
		return upstream
	}
	return latest.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
