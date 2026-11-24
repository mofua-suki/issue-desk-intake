package startup

import (
	"context"
	"errors"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
)

// Rebuild from current settings each cycle; stale limits must not authorize work.
func (a *App) jobRecoveryPolicy() (*operations.JobRecovery, error) {
	source, err := a.Operations()
	if err != nil {
		return nil, err
	}
	config, err := a.Values()
	if err != nil {
		return nil, err
	}
	return jobRecoveryPolicy(source, config)
}

func jobRecoveryPolicy(source *operations.Service, config map[string]any) (*operations.JobRecovery, error) {
	// Values contains the validated current assignment snapshot. Do not read it
	// again through a helper which silently substitutes defaults after an error.
	assignments := fleet.Object(config["assignments"])
	if source == nil || assignments == nil {
		return nil, errors.New("自動復旧の現在の担当表と上限を確認できません")
	}
	run, review, usage := fleet.Number(assignments["implementationRunLimit"]), fleet.Number(assignments["reviewRoundLimit"]), fleet.Number(assignments["implementationUsageLimit"])
	if run < 1 || review < 1 || usage < 1 {
		return nil, errors.New("自動復旧の上限が未確認です。既定値へ切り替えません")
	}
	return &operations.JobRecovery{Service: source, MinAge: time.Minute, Limit: 3, RunLimit: int64(run), ReviewLimit: int64(review), UsageLimit: usage, UsageWeights: fleet.Object(config["usageWeights"])}, nil
}

func (a *App) jobRecoveryActions() (*operations.RecoveryActions, error) {
	recovery, err := a.jobRecoveryPolicy()
	if err != nil {
		return nil, err
	}
	return &operations.RecoveryActions{Recovery: recovery,
		RefreshLimits: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			latest, err := a.jobRecoveryPolicy()
			if err != nil {
				return err
			}
			*recovery = *latest
			return nil
		},
		Judge: func(ctx context.Context, items []any) (map[string]any, error) {
			return a.AI.Answer(ctx, map[string]any{"kind": "ops-watch", "items": items})
		},
		ReadTarget: a.Issues.ReadIncidentTarget,
		Send: func(ctx context.Context, repo string, n int64, id, action, text string) error {
			_, err := a.Issues.SendRecoveryAction(ctx, repo, n, id, action, text)
			return err
		},
	}, nil
}

func (a *App) recoverJobs(ctx context.Context) error {
	actions, err := a.jobRecoveryActions()
	if err != nil {
		return err
	}
	r := actions.Recovery
	resolver := &operations.PrerequisiteResolver{Service: r.Service, Store: a.Jobs, Control: jobs.ControlOptions{RunLimit: r.RunLimit, UsageLimit: r.UsageLimit, UsageWeights: r.UsageWeights}}
	merged := &operations.MergedPredecessorResolver{Service: r.Service}
	return runRecoveryCycle(ctx, func(ctx context.Context) error { return errors.Join(resolver.Resolve(ctx), merged.Resolve(ctx)) }, r.Candidates, boundedRecoveryAct(r, actions.Act))
}

func runRecoveryCycle(ctx context.Context, resolve func(context.Context) error, candidates func(context.Context) ([]operations.RecoveryCandidate, error), act func(context.Context, operations.RecoveryCandidate) (operations.RecoveryResult, error)) error {
	resolutionErr := resolve(ctx)
	items, err := candidates(ctx)
	if err != nil {
		return errors.Join(resolutionErr, err)
	}
	return errors.Join(resolutionErr, runJobRecoveryCandidates(ctx, items, act))
}

// One unreadable job must not prevent independent candidates from progressing.
func runJobRecoveryCandidates(ctx context.Context, candidates []operations.RecoveryCandidate, act func(context.Context, operations.RecoveryCandidate) (operations.RecoveryResult, error)) error {
	var problems []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(problems, err)...)
		}
		if _, err := act(ctx, candidate); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func boundedRecoveryAct(r *operations.JobRecovery, act func(context.Context, operations.RecoveryCandidate) (operations.RecoveryResult, error)) func(context.Context, operations.RecoveryCandidate) (operations.RecoveryResult, error) {
	return func(ctx context.Context, c operations.RecoveryCandidate) (operations.RecoveryResult, error) {
		actCtx, cancel := context.WithTimeout(ctx, r.ReservationTTL()/2)
		defer cancel()
		return act(actCtx, c)
	}
}
