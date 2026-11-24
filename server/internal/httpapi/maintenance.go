package httpapi

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"issue-desk-intake/server/internal/jobs"
)

func WorkerMaintenance(ctx context.Context, options WorkerRouteOptions) error {
	config := &fleet.Service{DB: options.DB, FleetPath: options.FleetPath, AssignmentsPath: options.AssignmentsPath, Now: options.Now}
	_, a, err := config.Load()
	if err != nil {
		return err
	}
	store, err := jobs.New(options.DB, jobs.Options{Now: options.Now, AppDB: options.AppDB, ReadIssue: options.ReadIssue, Publish: options.Publish})
	if err != nil {
		return err
	}
	// 移管記録の照合と開始条件の更新は周期処理 acceptance-transfers だけが行う。
	sweep := store.Sweep(ctx, func(repo string) []string { return a.Exempt(repo) })
	releases := (&hostd.Service{DB: options.DB, Now: options.Now, Targets: config.Targets, Assignments: func() (*fleet.Assignments, error) { _, a, e := config.Load(); return a, e }}).Tick(ctx)
	return errors.Join(sweep, releases)
}
