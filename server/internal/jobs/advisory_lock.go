package jobs

import "strings"

// jobsAdvisoryLockKey identifies a repository or host lock shared by job operations.
func jobsAdvisoryLockKey(name string) string {
	return "issue-desk:jobs:" + strings.ToLower(name)
}
