package startup

import (
	"context"
	"errors"
	"time"

	"issue-desk-intake/server/internal/adminapi"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
)

func (a *App) startOperations() error {
	return a.startOperationsWith(operationDependencies{
		operations: a.Operations,
		read:       a.Issues.ReadIncidentTarget,
		track: func(ctx context.Context, source *operations.Service, notify func(context.Context, map[string]any) (map[string]any, error)) error {
			tracker := operations.IssueTracking{Service: source, List: a.Jobs.List, ReadJob: a.Jobs.ByIssue, Pull: a.executionPull, ReadTarget: a.Issues.ReadIncidentTarget, Detail: func(ctx context.Context, repo string, issue int64) (map[string]any, error) {
				return a.Issues.Detail(ctx, repo, issue, false, false)
			}, PendingStop: func(ctx context.Context, job *jobs.Job) (bool, error) {
				if source.PostgreSQL == nil {
					return false, errors.New("停止指示を照合できません")
				}
				var pending bool
				err := source.PostgreSQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_commands WHERE target_host=$1 AND repo=$2 AND issue=$3 AND kind='stop' AND status IN ('queued','acked'))`, *job.LeaseHost, job.Repo, job.Issue).Scan(&pending)
				return pending, err
			}, Notify: notify}
			return tracker.Reconcile(ctx)
		},
		send: func(ctx context.Context, repo string, n int64, id, action, text string) error {
			_, err := a.Issues.SendIncidentAction(ctx, repo, n, id, action, text)
			return err
		},
		answer: func(ctx context.Context, request map[string]any) (map[string]any, error) {
			switch request["kind"] {
			case "ops-watch":
				return a.AI.Answer(ctx, map[string]any{"kind": "ops-watch", "items": request["items"]})
			case "slow-incident":
				return a.AI.Answer(ctx, map[string]any{"kind": "slow-incident", "schema": request["schema"], "prompt": request["prompt"], "data": request["data"]})
			default:
				return nil, errors.New("有事の判定種別が不正です")
			}
		},
		sender: a.Filing,
		notify: func(ctx context.Context, notice map[string]any) (map[string]any, error) {
			result := adminapi.NotifyPhone(ctx, a.CurrentConfig().RuntimeDir, adminapi.PhoneNotice{Title: fleet.Text(notice["title"]), Body: fleet.Text(notice["body"]), URL: fleet.Text(notice["url"])}, nil)
			return map[string]any{"sent": result.Sent, "failed": result.Failed, "subscriptions": result.Subscriptions}, nil
		},
	})
}

type operationDependencies struct {
	operations func() (*operations.Service, error)
	read       func(context.Context, string, int64) (map[string]any, error)
	send       func(context.Context, string, int64, string, string, string) error
	answer     func(context.Context, map[string]any) (map[string]any, error)
	sender     operations.SlowIncidentSender
	notify     func(context.Context, map[string]any) (map[string]any, error)
	register   func(string, time.Duration, func(context.Context) error)
	track      func(context.Context, *operations.Service, func(context.Context, map[string]any) (map[string]any, error)) error
}

func (a *App) startOperationsWith(d operationDependencies) error {
	if d.operations == nil {
		return errors.New("運用状態の取得口を確認できません")
	}
	register := d.register
	if register == nil {
		register = a.periodically
	}
	source, err := d.operations()
	if err != nil {
		return err
	}
	sourcesChecked := false
	monitor := &operations.IncidentMonitor{Service: source, Sources: func(ctx context.Context) ([]operations.IncidentSource, error) {
		current, err := d.operations()
		if err != nil {
			return nil, err
		}
		other, err := current.IncidentSources(ctx)
		if err != nil {
			return nil, err
		}
		slow, err := current.SlowWorkSources(ctx)
		sourcesChecked = err == nil
		return append(other, slow...), err
	}}
	monitor.Notify = d.notify
	monitor.Actions = &operations.IncidentActions{Service: source,
		Judge: func(ctx context.Context, items []any) (map[string]any, error) {
			return d.answer(ctx, map[string]any{"kind": "ops-watch", "items": items})
		},
		ReadTarget: d.read,
		Send:       d.send,
		ValidateSlowSource: func(ctx context.Context, f *operations.SlowIncidentFiling) error {
			return validateActiveSlowSource(ctx, source, f)
		},
	}
	register("operations-monitor", time.Minute, func(ctx context.Context) error {
		sourcesChecked = false
		err := monitor.Tick(ctx)
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		slowErr := reconcileSlowIncidents(ctx, source, monitor.Actions, d.sender, d.answer, err == nil && sourcesChecked)
		var trackErr error
		if d.track != nil && ctx.Err() == nil {
			trackErr = d.track(ctx, source, d.notify)
		}
		return errors.Join(err, slowErr, trackErr)
	})
	register("recover-jobs", time.Minute, a.recoverJobs)
	register("prerequisite-hold-observation", time.Minute, func(ctx context.Context) error {
		current, err := d.operations()
		if err != nil {
			return err
		}
		return current.ObservePrerequisiteHoldCycle(ctx)
	})
	a.registerUserWaitNotice(register, d)
	return nil
}
