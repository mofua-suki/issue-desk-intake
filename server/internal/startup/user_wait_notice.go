package startup

import (
	"context"
	"fmt"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
)

type userWaitReader struct {
	list   func(context.Context, string, []string, int, int) ([]*jobs.Job, int, error)
	number func(context.Context, string, int64) (int64, bool, error)
	usage  func(context.Context, *jobs.Job, jobs.ControlOptions) bool
}

func userWaitNoticeSources(ctx context.Context, reader userWaitReader, options jobs.ControlOptions) ([]operations.UserWaitNoticeItem, error) {
	var result []operations.UserWaitNoticeItem
	const pageSize = 1000
	for offset := 0; ; {
		page, total, err := reader.list(ctx, "", []string{"held"}, pageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, job := range page {
			if job.Kind == "investigation" {
				continue
			}
			waits, err := jobs.UserWaits(job, options.RunLimit, reader.usage(ctx, job, options))
			if err != nil {
				return nil, err
			}
			if len(waits) == 0 {
				continue
			}
			number, found, err := reader.number(ctx, job.Repo, job.Issue)
			if err != nil {
				return nil, err
			}
			if !found || number <= 0 {
				return nil, fmt.Errorf("共通番号を確認できません: %s#%d", job.Repo, job.Issue)
			}
			for _, kind := range waits {
				result = append(result, operations.UserWaitNoticeItem{Project: job.Repo, IssueNumber: job.Issue, CommonNumber: number, Title: job.Title, Kind: kind})
			}
		}
		offset += len(page)
		if offset >= total {
			return result, nil
		}
		if len(page) == 0 {
			return nil, fmt.Errorf("保留中ジョブの一覧が途中で終了しました")
		}
	}
}

func userWaitNoticeOptions(config map[string]any) (jobs.ControlOptions, error) {
	assignments := fleet.Object(config["assignments"])
	run, usage := fleet.Number(assignments["implementationRunLimit"]), fleet.Number(assignments["implementationUsageLimit"])
	if assignments == nil || run < 1 || usage < 1 {
		return jobs.ControlOptions{}, fmt.Errorf("利用者判断待ちの上限が未確認です")
	}
	return jobs.ControlOptions{RunLimit: int64(run), UsageLimit: usage, UsageWeights: fleet.Object(config["usageWeights"])}, nil
}

func (a *App) registerUserWaitNotice(register func(string, time.Duration, func(context.Context) error), d operationDependencies) {
	register("user-wait-notice", time.Minute, func(ctx context.Context) error {
		service, err := d.operations()
		if err != nil {
			return err
		}
		notice := operations.UserWaitNotice{Service: service, Notify: d.notify, Sources: func(ctx context.Context) ([]operations.UserWaitNoticeItem, error) {
			values, err := a.Values()
			if err != nil {
				return nil, err
			}
			options, err := userWaitNoticeOptions(values)
			if err != nil {
				return nil, err
			}
			return userWaitNoticeSources(ctx, userWaitReader{list: a.Jobs.List, number: a.Numbers.Number, usage: a.Jobs.UsageLimitHeld}, options)
		}}
		return notice.Reconcile(ctx)
	})
}
