package startup

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"net/url"
	"sort"
	"strings"
)

// pollAppDeployments uses the same idempotent enqueue operation as a merge. It
// also catches default-branch commits merged outside the job controller.
func (a *App) pollAppDeployments(ctx context.Context) error {
	targets, _, e := a.Fleet.Targets()
	if e != nil {
		return e
	}
	names := []string{}
	for repo := range targets {
		names = append(names, repo)
	}
	sort.Strings(names)
	failures := []error{}
	for _, repo := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e = a.pollAppDeployment(ctx, repo, targets[repo]); e != nil {
			failures = append(failures, errors.New("外部アプリの配備ジョブを登録できませんでした（"+repo+"）"))
		}
	}
	return errors.Join(failures...)
}
func (a *App) defaultBranchHead(ctx context.Context, repo string) (string, error) {
	info := map[string]any{}
	if e := a.Gitea.Do(ctx, "GET", "/repos/"+repo, nil, &info); e != nil {
		return "", e
	}
	branch := fleet.Text(info["default_branch"])
	if strings.TrimSpace(branch) == "" {
		return "", errors.New("既定ブランチを取得できません")
	}
	head := map[string]any{}
	if e := a.Gitea.Do(ctx, "GET", "/repos/"+repo+"/branches/"+url.PathEscape(branch), nil, &head); e != nil {
		return "", e
	}
	return fleet.Text(fleet.Object(head["commit"])["id"]), nil
}

func (a *App) pollAppDeployment(ctx context.Context, repo, host string) error {
	commit, err := a.defaultBranchHead(ctx, repo)
	if err != nil {
		return err
	}
	tx, e := a.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = (&hostd.Service{DB: a.DB}).EnqueueDeployment(ctx, tx, repo, commit, host); e != nil {
		return e
	}
	return tx.Commit()
}
