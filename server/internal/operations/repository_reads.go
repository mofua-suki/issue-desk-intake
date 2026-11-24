package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"sync"
	"time"
)

// RepositoryReads keeps the result of the periodic check that the intake
// account can read every candidate repository's Issues. An unreadable candidate
// makes the lists partial (GET /api/issues/search unreadProjects), so it is
// shown as an attention item until a later check reads it or it stops being a
// candidate.
type RepositoryReads struct {
	mu        sync.Mutex
	checkedAt time.Time
	failures  []RepositoryReadFailure
}

type RepositoryReadFailure struct {
	Project string
	Reason  string
	// Since is when the candidate was first found unreadable in a row of checks.
	Since time.Time
}

// Check reads the given candidates (the current ones on every call) in order
// and replaces the recorded result, so a recovered or removed candidate's
// record disappears. A cancelled check records nothing. A newly unreadable
// candidate (or a changed reason) is logged once, and so is its recovery.
func (r *RepositoryReads) Check(ctx context.Context, repos []string, read func(context.Context, string) error, reason func(error) string, now func() time.Time, log func(string)) error {
	failures := []RepositoryReadFailure{}
	readable := map[string]bool{}
	for _, repo := range repos {
		err := read(ctx, repo)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			readable[repo] = true
			continue
		}
		failures = append(failures, RepositoryReadFailure{Project: repo, Reason: reason(err)})
	}
	at := now()
	r.mu.Lock()
	previous := map[string]string{}
	since := map[string]time.Time{}
	for _, failure := range r.failures {
		previous[failure.Project], since[failure.Project] = failure.Reason, failure.Since
	}
	for i := range failures {
		failures[i].Since = at
		if t, seen := since[failures[i].Project]; seen {
			failures[i].Since = t
		}
	}
	r.checkedAt, r.failures = at, failures
	r.mu.Unlock()
	if log == nil {
		return nil
	}
	for _, failure := range failures {
		if before, seen := previous[failure.Project]; !seen || before != failure.Reason {
			log(fmt.Sprintf("警告: 受付のアカウントで候補のリポジトリ %s のIssueを読めません（%s）。一覧・検索はこのリポジトリを除いた途中までの結果になります。", failure.Project, failure.Reason))
		}
	}
	for _, repo := range repos {
		if _, seen := previous[repo]; seen && readable[repo] {
			log(fmt.Sprintf("候補のリポジトリ %s のIssueを読めるようになりました。", repo))
		}
	}
	return nil
}

func (r *RepositoryReads) Alerts(ctx context.Context) ([]any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if r == nil {
		return []any{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []any{}
	for _, failure := range r.failures {
		out = append(out, map[string]any{
			"id":         "intake-repository-unreadable:" + failure.Project,
			"kind":       "intake_repository_unreadable",
			"status":     "stall_alert",
			"project":    fleet.SystemProject,
			"repo":       failure.Project,
			"detectedAt": iso(failure.Since),
			"title":      "受付のアカウントで候補のリポジトリ " + failure.Project + " を読めません",
			"detail": fmt.Sprintf("対象: %s\n理由: %s\n確認: 受付の定期確認（起動時と%d分ごと）\n影響: 依頼の一覧・検索はこのリポジトリを除いた途中までの結果になります（unreadProjectsに表示し、projectを指定しない検索はincomplete=true）。\n"+
				"対処: リポジトリの名前・存在と、受付のアカウントの読取権限を確かめてください。候補から外す場合は構成のリポジトリ一覧を直します。次の定期確認で読めるか候補から外れていれば、この項目は消えます。", failure.Project, failure.Reason, int(RepositoryReadInterval/time.Minute)),
		})
	}
	return out, nil
}

// RepositoryReadInterval is how often the intake rechecks candidate reads.
const RepositoryReadInterval = 2 * time.Minute
