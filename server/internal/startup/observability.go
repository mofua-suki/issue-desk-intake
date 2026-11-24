package startup

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/adminapi"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/operations"
	"math"
	"time"
)

func (a *App) initializeObservability() error {
	if raw, exists := a.Config.Values["hostDriftCompatReleases"]; exists {
		n, ok := raw.(float64)
		if !ok || n < 0 || math.Trunc(n) != n || n > 9007199254740991 {
			return errors.New("hostDriftCompatReleasesは0以上の整数です")
		}
		a.Drift.CompatLimit = int(n)
	}
	notify := func(ctx context.Context, notice map[string]any) (map[string]any, error) {
		result := adminapi.NotifyPhone(ctx, a.CurrentConfig().RuntimeDir, adminapi.PhoneNotice{Title: fleet.Text(notice["title"]), Body: fleet.Text(notice["body"]), URL: fleet.Text(notice["url"])}, nil)
		return map[string]any{"sent": result.Sent, "failed": result.Failed, "subscriptions": result.Subscriptions}, nil
	}
	a.AIFailures = operations.NewAIFailures(a.ctx, a.Config.RuntimeDir, notify)
	a.Runner.ObserveResult = a.AIFailures.Observe
	a.RuntimeMeasurements = operations.NewRuntimeMeasurements(a.DB, time.Now)
	a.startBackgroundTask(func(ctx context.Context) {
		if e := a.RuntimeMeasurements.Run(ctx, time.Second); e != nil && ctx.Err() == nil {
			a.Log("Goの周期計測が停止しました。原因: " + contract.SafeCause(e))
		}
	})
	a.RepositoryReads = &operations.RepositoryReads{}
	// Rechecked periodically against the current candidates, so a Gitea that
	// starts late or a changed candidate list clears the attention items.
	a.periodically("repository-reads", operations.RepositoryReadInterval, a.checkRepositoryReads)
	a.Drift.SettingsVersion = func(ctx context.Context) (string, error) { _, version, e := a.Fleet.Settings(ctx); return version, e }
	a.Drift.Notify = func(ctx context.Context, notice map[string]any) error { _, e := notify(ctx, notice); return e }
	return nil
}
func (a *App) firstDraftIssue(id string) (string, int64) {
	d, ok, e := a.Records.Get(a.ctx, "drafts", id)
	if e != nil || !ok {
		return "", 0
	}
	targets := fleet.List(d["issues"])
	if len(targets) == 0 {
		targets = []any{d["issue"]}
	}
	targets = append(targets, fleet.List(d["investigations"])...)
	for _, raw := range targets {
		target := fleet.Object(raw)
		value, valid := target["number"].(float64)
		if !valid || value <= 0 || math.Trunc(value) != value || value > 9007199254740991 {
			continue
		}
		number := int64(value)
		if number <= 0 {
			continue
		}
		repo := fleet.Text(target["project"])
		if repo == "" {
			repo = fleet.Text(d["project"])
		}
		if repo != "" {
			return repo, number
		}
	}
	return "", 0
}
