package startup

import (
	"context"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/issueapi"
	"time"
)

// checkRepositoryReads confirms, at startup and then periodically, that the
// intake account can read the Issues of every current candidate repository.
// Unreadable ones are logged as warnings and kept as attention items
// (systemAlerts) until a later check reads them or they stop being candidates.
func (a *App) checkRepositoryReads(ctx context.Context) error {
	return a.RepositoryReads.Check(ctx, a.Repositories(), a.readRepositoryIssues, issueapi.UnreadReason, time.Now, a.Log)
}

// readRepositoryIssues reads one page of one Issue, the same endpoint family the
// lists read, without reading the whole history.
func (a *App) readRepositoryIssues(ctx context.Context, repo string) error {
	base, err := gitea.RepoPath(repo)
	if err != nil {
		return err
	}
	var issues []map[string]any
	return a.Gitea.Do(ctx, "GET", base+"/issues?type=issues&state=all&limit=1", nil, &issues)
}

// systemAlerts are the intake's own attention items.
func (a *App) systemAlerts(ctx context.Context) ([]any, error) {
	out := []any{}
	if a.RuntimeMeasurements != nil {
		rows, err := a.RuntimeMeasurements.Alerts(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	rows, err := a.RepositoryReads.Alerts(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, rows...), nil
}
