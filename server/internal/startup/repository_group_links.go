package startup

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
)

type groupIssueClient interface {
	Issue(context.Context, string, int64) (map[string]any, error)
	Do(context.Context, string, string, any, any) error
}

type repositoryGroupLinksAdapter struct{ client groupIssueClient }

func (a repositoryGroupLinksAdapter) Get(ctx context.Context, repo string, number int64) (map[string]any, error) {
	return a.client.Issue(ctx, repo, number)
}

func (a repositoryGroupLinksAdapter) Update(ctx context.Context, repo string, number int64, body string, version any) error {
	path, err := gitea.RepoPath(repo)
	if err != nil || number < 1 || version == nil {
		return errors.New("Issueまたは本文の版が不正です")
	}
	return a.client.Do(ctx, http.MethodPatch, path+"/issues/"+strconv.FormatInt(number, 10), map[string]any{"body": body, "content_version": version}, nil)
}

func (a *App) filingRepositoryGroups(_ context.Context) ([][]string, error) {
	values, err := a.Values()
	if err != nil {
		return nil, err
	}
	var groups [][]string
	for _, raw := range fleet.List(fleet.Object(values["assignments"])["repositoryGroups"]) {
		group := []string{}
		for _, name := range fleet.List(raw) {
			group = append(group, fleet.Text(name))
		}
		groups = append(groups, group)
	}
	return groups, nil
}
