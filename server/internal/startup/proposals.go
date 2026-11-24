package startup

import (
	"context"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/proposals"
)

func (a *App) registerProposals() error {
	var err error
	a.Proposals, err = proposals.Register(a.Mux, proposals.Options{
		AppDB: a.SQLite.DB(), Postgres: a.DB, Auth: a.Auth, Gitea: a.Gitea,
		Config: a.Values, Project: a.Project, Desk: a.DeskNumber, Link: a.Link, Publish: a.Publish,
		AcceptedContext: a.ctx, Repositories: a.Repositories, Issues: a.Gitea.Issues, Comments: a.Gitea.Comments,
		Submit: func(ctx context.Context, id string, data consultations.Record) (consultations.Record, error) {
			return a.Filing.SubmitAccepted(ctx, id, data, auth.Principal{Kind: auth.Screen})
		},
	})
	return err
}

func (a *App) validateFollowup(ctx context.Context, d consultations.Record) error {
	if a.Proposals == nil {
		return contract.NewProblem(503, "後続案の照合を準備中です。", "")
	}
	return a.Proposals.ValidateDraft(ctx, d)
}

func (a *App) validateFollowupNewPost(ctx context.Context, d consultations.Record) error {
	if a.Proposals == nil {
		return contract.NewProblem(503, "後続案の照合を準備中です。", "")
	}
	return a.Proposals.ValidateNewPost(ctx, d)
}

func (a *App) prepareFollowup(ctx context.Context, d consultations.Record) (filing.Prepared, error) {
	if err := a.validateFollowup(ctx, d); err != nil {
		return filing.Prepared{}, err
	}
	config, err := a.Values()
	if err != nil {
		return filing.Prepared{}, err
	}
	// The proposal chooses the provider; the implementation tier follows the
	// filing rule (filing.DecideFollowupTier) instead of a fixed standard tier.
	level, err := a.Proposals.FollowupTier(ctx, d)
	if err != nil {
		return filing.Prepared{}, err
	}
	settings, err := models.ImplementationSettings(config, fleet.Text(d["provider"]), level)
	if err != nil {
		return filing.Prepared{}, err
	}
	labels, err := a.providerLabels(ctx, fleet.Text(d["project"]), settings.Provider)
	if err != nil {
		return filing.Prepared{}, err
	}
	return a.Proposals.PrepareFollowup(ctx, d, map[string]any{"provider": settings.Provider, "model": settings.Model, "effort": settings.Effort, "tier": level}, labels)
}
