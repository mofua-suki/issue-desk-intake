package startup

import (
	"context"
	"fmt"
	"strings"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/releasepolicy"
)

func publicLink(config map[string]any, repo, kind string, n int64) string {
	base := fleet.Text(config["publicUrl"])
	if base == "" {
		base = fleet.Text(config["giteaUrl"])
	}
	return fmt.Sprintf("%s/%s/%s/%d", strings.TrimRight(base, "/"), repo, kind, n)
}
func (a *App) releasePolicy(_ string, config map[string]any) map[string]any {
	repos := []string{}
	for _, v := range fleet.List(config["repositories"]) {
		if name := fleet.Text(fleet.Object(v)["name"]); fleet.ValidRepo(name) {
			repos = append(repos, name)
		}
	}
	return releasepolicy.Configuration(repos)
}
func (a *App) Providers(ctx context.Context) ([]map[string]any, error) {
	return a.cachedProviders(ctx)
}

// modelProviders adapts the status API rows to the model operations' provider identity.
func (a *App) modelProviders(ctx context.Context) ([]map[string]any, error) {
	// Operations must observe authentication now, independently of screen reads.
	config := a.CurrentConfig()
	rows := []map[string]any{}
	for _, p := range []string{"codex", "claude"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state := a.Runner.Status(ctx, p, config.RuntimeDir)
		rows = append(rows, providerRow(state))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, row := range rows {
		row["id"] = row["provider"]
		row["name"] = row["provider"]
	}
	return rows, nil
}
