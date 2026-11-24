package hosts

import (
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

var cliSemver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func ValidCLIVersions(value any) bool {
	rows, ok := value.([]any)
	if !ok || len(rows) > 2 {
		return false
	}
	seen := map[string]bool{}
	validText := func(v any, max int) bool {
		s, ok := v.(string)
		return ok && s != "" && contract.UTF16Length(s) <= max && !strings.ContainsAny(s, "\r\n")
	}
	for _, raw := range rows {
		r := fleet.Object(raw)
		cli := fleet.Text(r["cli"])
		if (cli != "codex" && cli != "claude") || seen[cli] || !validText(r["expected"], 100) || !integer(r["at"], 1, 8.64e15) {
			return false
		}
		seen[cli] = true
		current, exists := r["current"]
		if !exists {
			return false
		}
		if current != nil {
			value, ok := current.(string)
			if !ok || contract.UTF16Length(value) > 100 || !cliSemver.MatchString(value) {
				return false
			}
		}
		if reason, exists := r["reason"]; exists && !validText(reason, 200) {
			return false
		}
	}
	return true
}
