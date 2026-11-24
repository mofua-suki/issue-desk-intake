package issueapi

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/listscope"
	"issue-desk-intake/server/internal/numbers"
	"sort"
)

// UnreadProject is a candidate repository whose Issues could not be read.
// Reason is the Gitea client message only; it never carries credentials or
// external response bodies.
type UnreadProject struct {
	Project string `json:"project"`
	Reason  string `json:"reason"`
}

// repoReadError marks a failure of one repository's Issue read, as opposed to
// project validation, cancellation, or a failure while building rows.
type repoReadError struct{ err error }

func (e *repoReadError) Error() string { return e.err.Error() }
func (e *repoReadError) Unwrap() error { return e.err }

// UnreadReason is the reason shown for an unreadable candidate repository.
func UnreadReason(err error) string {
	var giteaErr *gitea.Error
	if errors.As(err, &giteaErr) {
		return giteaErr.Error()
	}
	return "Issueを読み取れません。"
}

// list serves GET /api/issues. Its bare array cannot say that rows are missing,
// so an unread candidate keeps the whole read failing instead of returning a
// partial array that callers would treat as complete.
func (s *Service) list(ctx context.Context) ([]Object, error) {
	rows, unread, err := s.listFiltered(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if len(unread) > 0 {
		return nil, unreadProblem(unread)
	}
	return rows, nil
}

// listScoped serves the scope query of GET /api/issues. Without it every row is
// returned as before. scope=active returns the range of the screen's active
// tabs; an unread candidate fails it like the full list, because the bare
// array cannot say that rows are missing.
func (s *Service) listScoped(ctx context.Context, data Object) ([]Object, error) {
	scope, has := data["scope"]
	if !has {
		return s.list(ctx)
	}
	if scope != "active" {
		return nil, fail(400, "scopeにはactiveだけ指定できます。")
	}
	if s.o.ActiveList == nil {
		// Only the injected reader knows which closed rows consultations and
		// close requests still need. Every row is the safe answer without it.
		return s.list(ctx)
	}
	rows, unread, err := s.o.ActiveList(ctx)
	if err != nil {
		return nil, err
	}
	if len(unread) > 0 {
		return nil, unreadProblem(unread)
	}
	if rows == nil {
		rows = []Object{}
	}
	return rows, nil
}
func unreadProblem(unread []UnreadProject) error {
	names := ""
	for i, u := range unread {
		if i > 0 {
			names += "、"
		}
		names += u.Project + "（" + u.Reason + "）"
	}
	return fail(503, "読み取れない候補のリポジトリがあるため、一覧がそろいません: "+names+"。読めた候補の一覧は /api/issues/search で取得できます。")
}

// listFiltered returns rows of every readable candidate and the candidates it
// could not read. A failure of the requested project, of every candidate, or
// of anything other than a repository read still fails the whole list.
func (s *Service) listFiltered(ctx context.Context, state, project string) ([]Object, []UnreadProject, error) {
	unread := []UnreadProject{}
	var firstReadErr error
	baseline, err := s.closeBaseline(ctx)
	if err != nil {
		return nil, nil, err
	}
	ctx = context.WithValue(ctx, listBaselineKey{}, listBaselineValue{baseline})
	var config Object
	configLoaded := false
	var snapshot *listSnapshot
	closed := []Object{}
	out := []Object{}
	lists := []numbers.List{}
	repos := s.repositories()
	var pending []listPendingRow
	draftIDs := map[string]bool{}
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if snapshot == nil {
			snapshot, err = s.listSnapshot(ctx, config)
			if err != nil {
				return err
			}
		}
		if listscope.Active(ctx) {
			ctx, pending, draftIDs = s.omitHiddenClosedRows(ctx, snapshot, pending)
		}
		rowCtx := context.WithValue(ctx, listSnapshotKey{}, snapshot)
		closedIDs := s.closedListRowIDs(snapshot, pending)
		if err = s.loadListDrafts(ctx, snapshot, draftIDs, closedIDs); err != nil {
			// On a batch failure, replay the original row order to preserve the
			// first error (including errors from origin lookups). Never hide a
			// transient batch failure if the diagnostic reads succeed.
			batchErr := err
			for _, item := range pending {
				if _, _, e := s.row(ctx, item.repo, item.issue); e != nil {
					return e
				}
			}
			return batchErr
		}
		for _, item := range pending {
			row, _, e := s.row(rowCtx, item.repo, item.issue)
			if e != nil {
				return e
			}
			out = append(out, row)
		}
		pending = nil
		draftIDs = map[string]bool{}
		return nil
	}
	readCtx, cancelReads := context.WithCancel(ctx)
	defer cancelReads()
	reads := newListReads(readCtx, repos, s.o.Project, s.o.Gitea.Issues)
	for _, repo := range repos {
		reads.fill()
		issues, e := reads.next()
		if e != nil {
			var readErr *repoReadError
			if !errors.As(e, &readErr) || ctx.Err() != nil || errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
				return nil, nil, e
			}
			// The requested project itself must stay an error, as before.
			if project != "" && repo == project {
				return nil, nil, readErr.err
			}
			if firstReadErr == nil {
				firstReadErr = readErr.err
			}
			unread = append(unread, UnreadProject{Project: repo, Reason: UnreadReason(readErr.err)})
			continue
		}
		list := numbers.List{Project: repo}
		for _, issue := range issues {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if coordination(repo, issue) {
				continue
			}
			n, ok := number(issue["number"])
			if ok {
				list.Issues = append(list.Issues, numbers.Candidate{Number: n, CreatedAt: fleet.Text(issue["created_at"]), PullRequest: issue["pull_request"] != nil})
			}
			if !configLoaded {
				config, err = s.configuration()
				if err != nil {
					return nil, nil, err
				}
				configLoaded = true
			}
			author := s.requestAuthorWithConfig(issue, config)
			if issue["pull_request"] != nil || !author {
				continue
			}
			if !ok {
				if e := flush(); e != nil {
					return nil, nil, e
				}
				return nil, nil, fail(502, "Issue番号を読み取れません。")
			}
			if issue["state"] == "closed" {
				closed = append(closed, Object{"project": repo, "number": issue["number"], "state": "closed", "closedAt": issue["closed_at"]})
			}
			if state != "" && issue["state"] != state || project != "" && repo != project {
				continue
			}
			pending = append(pending, listPendingRow{repo, issue})
			if s.o.Own(fleet.ObjectOrEmpty(issue["user"])) {
				if id := filing.DraftID(fleet.Text(issue["body"])); id != "" {
					draftIDs[id] = true
				}
			}
		}
		if !listscope.Active(ctx) {
			if e := flush(); e != nil {
				return nil, nil, e
			}
		}
		lists = append(lists, list)
	}
	if e := flush(); e != nil {
		return nil, nil, e
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(repos) > 0 && len(unread) == len(repos) {
		// Nothing was read: there is no partial result to return.
		return nil, nil, firstReadErr
	}
	if e := s.recordListResults(ctx, out, lists, repos, closed, baseline == nil, len(unread) == 0); e != nil {
		return nil, nil, e
	}
	sort.SliceStable(out, func(i, j int) bool { return fleet.Text(out[i]["updatedAt"]) > fleet.Text(out[j]["updatedAt"]) })
	return out, unread, nil
}

// recordListResults は読めた一覧から共通番号を振って行に付け、初回だけ閉じた Issue の基準を記録する。
// complete は候補をすべて読めたかで、欠けた一覧では初回の割り当てと基準の記録をしない。
func (s *Service) recordListResults(ctx context.Context, out []Object, lists []numbers.List, repos []string, closed []Object, firstBaseline, complete bool) error {
	// A list missing a candidate must not complete the initial assignment or
	// register projects for the first time; known projects are still numbered.
	if _, e := s.o.Numbers.Assign(ctx, lists, repos, complete); e != nil {
		return e
	}
	if len(out) > 0 {
		if e := s.attachDeskNumbers(ctx, out); e != nil {
			return e
		}
	}
	// The close-reason baseline records every closed Issue once; an incomplete
	// list would permanently omit the unread candidates' Issues.
	if firstBaseline && complete {
		if e := s.recordCloseBaseline(ctx, closed); e != nil {
			return e
		}
	}
	return nil
}

// listPendingRow は一覧の行にする前の Issue。
type listPendingRow struct {
	repo  string
	issue Object
}

// omitHiddenClosedRows は投稿の参照を全リポジトリで解決した後、範囲外の閉じた Issue を一覧から外し、残った行と読む相談の番号を返す。
func (s *Service) omitHiddenClosedRows(ctx context.Context, snapshot *listSnapshot, pending []listPendingRow) (context.Context, []listPendingRow, map[string]bool) {
	// Resolve posted references against every repository before omitting
	// rows. A partially visible publication must not resurrect a closed
	// Issue as a pending synthetic row on the client.
	identities := make([]map[string]any, 0, len(pending))
	for _, item := range pending {
		identities = append(identities, Object{"project": item.repo, "number": item.issue["number"]})
	}
	ctx = listscope.ResolveProtection(ctx, identities)
	retained := pending[:0]
	draftIDs := map[string]bool{}
	for _, item := range pending {
		repo, issue := item.repo, item.issue
		n, _ := number(issue["number"])
		sourceID := ""
		if s.o.Own(fleet.ObjectOrEmpty(issue["user"])) {
			sourceID = filing.DraftID(fleet.Text(issue["body"]))
		}
		job := snapshot.jobs[numbers.Issue{Project: repo, Number: n}]
		status := shownStatus(issue, job, false)
		continuation := job != nil && fleet.ObjectOrEmpty(job.View["continuation"])["pending"] == true
		if issue["state"] == "closed" && !oneOf(status, "running", "publishing", "scope_waiting", "releasing", "deploying") && !continuation && !unfinishedExecutionJob(job) && !listscope.Protected(ctx, repo, n, sourceID) {
			listscope.Omit(ctx, repo, n)
			continue
		}
		retained = append(retained, item)
		if sourceID != "" {
			draftIDs[sourceID] = true
		}
	}
	return ctx, retained, draftIDs
}

// closedListRowIDs は閉じた状態で表示する行の番号を集める。依存が残る待ちの行は閉じた情報を使わない。
func (s *Service) closedListRowIDs(snapshot *listSnapshot, pending []listPendingRow) map[string]bool {
	closedIDs := map[string]bool{}
	for _, item := range pending {
		key := numbers.Issue{Project: item.repo, Number: int64Number(item.issue["number"])}
		job := snapshot.jobs[key]
		// row overrides a queued job's closed status when dependencies
		// remain unresolved. Such rows never consume closure metadata.
		if job != nil && job.Status == "queued" && s.o.Postgres != nil && len(snapshot.dependencies[job.ID]) > 0 {
			continue
		}
		if shownStatus(item.issue, job, false) == "closed" {
			closedIDs[fmt.Sprintf("%s#%d", item.repo, int64Number(item.issue["number"]))] = true
		}
	}
	return closedIDs
}

// attachDeskNumbers は一覧の行に受付の共通番号を付ける。
func (s *Service) attachDeskNumbers(ctx context.Context, out []Object) error {
	assigned, e := s.o.Numbers.All(ctx)
	if e != nil {
		return e
	}
	for _, row := range out {
		n, _ := number(row["number"])
		if desk, found := assigned[numbers.Issue{Project: fleet.Text(row["project"]), Number: n}]; found {
			row["desk"] = desk
		}
	}
	return nil
}

// listReads keeps at most four independent reads in flight. Results and project
// validation errors are consumed in repository order, after the preceding rows.
const listReadLimit = 4

type listReadResult struct {
	issues []Object
	err    error
}
type listReads struct {
	ctx                   context.Context
	repos                 []string
	project               func(string) error
	issues                func(context.Context, string, string) ([]Object, error)
	nextStart, nextResult int
	pending               []chan listReadResult
	stopped               bool
}

func newListReads(ctx context.Context, repos []string, project func(string) error, issues func(context.Context, string, string) ([]Object, error)) *listReads {
	r := &listReads{ctx: ctx, repos: repos, project: project, issues: issues, pending: make([]chan listReadResult, len(repos))}
	r.fill()
	return r
}
func (r *listReads) fill() {
	for !r.stopped && r.nextStart < len(r.repos) && r.nextStart-r.nextResult < listReadLimit && r.ctx.Err() == nil {
		i := r.nextStart
		r.nextStart++
		ch := make(chan listReadResult, 1)
		r.pending[i] = ch
		repo := r.repos[i]
		if err := r.project(repo); err != nil {
			ch <- listReadResult{err: err}
			r.stopped = true
			break
		}
		go func() {
			issues, err := r.issues(r.ctx, repo, "all")
			if err != nil {
				err = &repoReadError{err}
			}
			ch <- listReadResult{issues, err}
		}()
	}
}
func (r *listReads) next() ([]Object, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	ch := r.pending[r.nextResult]
	select {
	case <-r.ctx.Done():
		return nil, r.ctx.Err()
	case result := <-ch:
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		r.nextResult++
		// Refill only after the caller has processed this result, on its next call.
		return result.issues, result.err
	}
}
