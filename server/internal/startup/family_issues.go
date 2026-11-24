package startup

import (
	"context"
	"fmt"
	"strings"
)

// familyIssues reads a shared repository history once per family calculation.
func (a *App) familyIssues(ctx context.Context, project string) (map[string]string, error) {
	issues, err := a.Gitea.Issues(ctx, project, "all")
	if err != nil {
		return nil, err
	}
	states := make(map[string]string, len(issues))
	for _, issue := range issues {
		if issue["number"] == nil {
			continue
		}
		state, _ := issue["state"].(string)
		if state == "open" || state == "closed" {
			states[strings.ToLower(fmt.Sprintf("%s#%v", project, issue["number"]))] = state
		}
	}
	return states, nil
}
