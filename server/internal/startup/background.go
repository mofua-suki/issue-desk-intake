package startup

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/httpapi"
	"issue-desk-intake/server/internal/notices"
	"issue-desk-intake/server/internal/numbers"
)

const noticeRepo = "mofua-suki/issue-desk-intake"
const legacyNoticeRepo = "mofua-suki/issue-desk"

func (a *App) startBackground() error {
	if err := a.startOperations(); err != nil {
		return err
	}
	if err := a.startGate(); err != nil {
		return err
	}
	if err := a.Engine.Recover(a.backgroundCtx); err != nil {
		return err
	}
	a.periodically("worker-maintenance", 30*time.Second, func(ctx context.Context) error { return httpapi.WorkerMaintenance(ctx, a.workerOptions()) })
	a.startFailedMergedRecovery()
	a.startFilingPending()
	a.periodically("numbering", 5*time.Minute, func(ctx context.Context) error { return a.numbering(ctx, false) })
	a.periodically("acceptance-transfers", 30*time.Second, a.resolveAcceptanceTransfers)
	a.periodically("app-deployments", 5*time.Minute, a.pollAppDeployments)
	a.periodically("daily-notes", time.Minute, a.reconcileDailyNotes)
	proposalsInterval := a.proposalsScanInterval
	if proposalsInterval <= 0 {
		proposalsInterval = time.Minute // proposals-scan production default
	}
	a.periodically("proposals-scan", proposalsInterval, a.Proposals.Scan)
	a.periodically("stalls-tick", time.Minute, a.Stalls.Tick)
	a.periodically("stalls-check", 24*time.Hour, func(ctx context.Context) error { _, err := a.Stalls.Check(ctx, a.Engine); return err })
	cleanup := &notices.Cleanup{Service: &notices.Service{DB: a.SQLite.DB()}, PostgreSQL: a.DB, ReadIssue: a.Gitea.Issue}
	a.periodically("notices-cleanup", time.Minute, cleanup.Tick)
	a.startRecoveryLoops()
	return nil
}

func (a *App) startFailedMergedRecovery() {
	a.periodically("failed-merged-recovery", 30*time.Second, a.Jobs.RecoverFailedMerged)
}

func (a *App) startFilingPending() {
	a.periodically("filing-pending", 30*time.Second, a.filingPendingCycle)
}

func (a *App) filingPendingCycle(ctx context.Context) error {
	reconcileErr := a.reconcileSplitParents(ctx)
	filingErr := a.Filing.FilePendingWithLifetime(ctx, a.ctx)
	if err := ctx.Err(); err != nil {
		return errors.Join(reconcileErr, filingErr, err)
	}
	return errors.Join(reconcileErr, filingErr, a.Jobs.ResolveFilingReferences(a.ctx))
}

func (a *App) reconcileSplitParents(ctx context.Context) error {
	entries, err := a.Records.ListDraftEntries(ctx, consultations.DraftReadRecovery)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Record["status"] != "submit_error" {
			continue
		}
		_, err := a.Consultations.ReconcileSplitParent(ctx, entry.Key, a.Filing.Busy, func(event consultations.Record) { a.Publish(map[string]any(event)) })
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (a *App) startRecoveryLoops() {
	if a.Projects != nil {
		a.startBackgroundTask(func(ctx context.Context) {
			if err := a.Projects.RunRecovery(ctx); err != nil && ctx.Err() == nil {
				a.Log("プロジェクト登録の再開を完了できませんでした。記録を保持しています。原因: " + contract.SafeCause(err))
			}
		})
	}
	if a.Issues != nil {
		a.startBackgroundTask(func(ctx context.Context) { _ = a.Issues.RunRecovery(ctx) })
	}
}
func (a *App) recoverSubmissions(ctx context.Context) error {
	drafts, err := recoveryDrafts(ctx, a.Records)
	if err != nil {
		return err
	}
	for _, d := range drafts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d["status"] != "submitting" && d["status"] != "submit_unknown" && d["status"] != "auto_ready" {
			continue
		}
		p, err := a.submissionPrincipal(ctx, d)
		if err != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err = a.Filing.SubmitAccepted(a.ctx, fleet.Text(d["id"]), consultations.Record{"revision": d["revision"]}, p); err != nil && a.ctx.Err() == nil {
			a.Log("保存済みの起票結果を照合中です。結果が不明な要求は再投稿しません。")
			if d["status"] == "auto_ready" {
				a.Engine.SaveAutoSubmitFailure(a.ctx, fleet.Text(d["id"]), d, err)
			}
		}
	}
	return nil
}

type recoveryDraftReader interface {
	ListDraftEntries(context.Context, consultations.DraftReadPurpose) ([]consultations.DraftFields, error)
	GetDrafts(context.Context, []string) ([]consultations.Record, error)
}

func recoveryDrafts(ctx context.Context, records recoveryDraftReader) ([]consultations.Record, error) {
	entries, err := records.ListDraftEntries(ctx, consultations.DraftReadRecovery)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, entry := range entries {
		status := entry.Record["status"]
		if status == "submitting" || status == "submit_unknown" || status == "auto_ready" {
			ids = append(ids, entry.Key)
		}
	}
	return records.GetDrafts(ctx, ids)
}

func (a *App) numbering(ctx context.Context, force bool) error {
	a.numberMu.Lock()
	defer a.numberMu.Unlock()
	if !force && !a.numberAt.IsZero() && time.Since(a.numberAt) < 5*time.Minute {
		return nil
	}
	repos := a.Repositories()
	lists := []numbers.List{}
	for _, repo := range repos {
		issues, err := a.Gitea.Issues(ctx, repo, "all")
		if err != nil {
			return err
		}
		candidates := []numbers.Candidate{}
		for _, issue := range issues {
			if administrativeIssue(repo, issue) {
				continue
			}
			candidates = append(candidates, numbers.Candidate{Number: int64(fleet.Number(issue["number"])), CreatedAt: fleet.Text(issue["created_at"]), PullRequest: issue["pull_request"] != nil})
		}
		lists = append(lists, numbers.List{Project: repo, Issues: candidates})
	}
	if _, err := a.Numbers.Assign(ctx, lists, repos, true); err != nil {
		return err
	}
	a.numberAt = time.Now()
	return nil
}
func administrativeIssue(repo string, issue map[string]any) bool {
	labels := map[string]bool{}
	for _, v := range fleet.List(issue["labels"]) {
		if s, ok := v.(string); ok {
			labels[s] = true
		} else {
			labels[fleet.Text(fleet.Object(v)["name"])] = true
		}
	}
	body := fleet.Text(issue["body"])
	return labels["issue-desk/coordination"] && regexp.MustCompile(`<!-- issue-desk-coordination \{[\s\S]*\} -->\s*$`).MatchString(body) || (repo == noticeRepo || repo == legacyNoticeRepo) && labels["issue-desk/notice"] && strings.Contains(body, "<!-- issue-desk-notice -->")
}

func (a *App) splitConsultation(ctx context.Context, parent consultations.Record, parts []string) (consultations.Record, error) {
	result, err := a.AI.SplitChildren(ctx, parent, parts)
	if err != nil {
		return nil, err
	}
	for _, v := range fleet.List(result["consultationChildren"]) {
		id := fleet.Text(v)
		d, ok, err := a.Records.Get(ctx, "drafts", id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		p, err := a.principal(ctx, fleet.Text(d["owner"]))
		if err != nil {
			return nil, err
		}
		if _, err = a.Engine.Start(ctx, id, d, p); err != nil {
			return nil, err
		}
	}
	return result, nil
}
