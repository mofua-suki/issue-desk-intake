package startup

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
)

func (a *App) executionReportAuthor(host string) string {
	_, assignments, err := a.Fleet.Load()
	if err != nil {
		return ""
	}
	return fleet.Text(fleet.Object(assignments.Output["workerAccounts"])[host])
}

func (a *App) executionPull(ctx context.Context, repo string, number int64) (map[string]any, error) {
	var out map[string]any
	err := a.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &out)
	return out, err
}
