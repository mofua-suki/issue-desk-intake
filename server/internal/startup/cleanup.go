package startup

import (
	"context"
	"issue-desk-intake/server/internal/deskcleanup"
)

func (a *App) registerCleanup() error {
	s := &deskcleanup.Service{DB: a.SQLite.DB(), Auth: a.Auth, Project: a.Project, Busy: func(id string) bool { return a.Engine.Busy(id) || a.Filing.Busy(id) }, Issue: func(ctx context.Context, repo string, n int64) (map[string]any, error) {
		return a.Issues.Detail(ctx, repo, n, false, false)
	}, History: func(ctx context.Context, repo string) ([]map[string]any, error) {
		return a.Gitea.Issues(ctx, repo, "all")
	}, Own: a.Own, CloseIssue: a.Issues.CloseExplicit, Publish: a.Publish, Alerts: func(ctx context.Context, kind string) ([]any, error) {
		o, e := a.Operations()
		if e != nil {
			return nil, e
		}
		copy := *o
		o = &copy
		o.IncludeArchived = true
		if kind == "attention" {
			return o.Attention(ctx)
		}
		return o.Stalls(ctx)
	}}
	for _, path := range []string{"/api/desk-cleanup/inspect", "/api/desk-cleanup/close", "/api/desk-cleanup/archive"} {
		a.Mux.Handle(path, s)
	}
	return nil
}
