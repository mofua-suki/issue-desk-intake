package issueapi

import (
	"context"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"strings"
)

func (s *Service) repositories() []string {
	if s.o.Repositories != nil {
		return s.o.Repositories()
	}
	out := []string{}
	for _, v := range fleet.Items(s.o.Config["repositories"]) {
		if name := fleet.Text(fleet.ObjectOrEmpty(v)["name"]); name != "" {
			out = append(out, name)
		}
	}
	return out
}
func (s *Service) requestAuthorChecked(issue Object) (bool, error) {
	config, e := s.configuration()
	if e != nil {
		return false, e
	}
	return s.requestAuthorWithConfig(issue, config), nil
}
func (s *Service) requestAuthorWithConfig(issue Object, config Object) bool {
	user := fleet.ObjectOrEmpty(issue["user"])
	dedicated := s.o.DedicatedAuthor
	if dedicated == nil {
		dedicated = s.o.Own
	}
	if dedicated != nil && dedicated(user) {
		return true
	}
	login := fleet.Text(user["login"])
	if login == "" {
		return false
	}
	for _, raw := range fleet.Items(config["allowedUsers"]) {
		if value, ok := raw.(string); ok && value == login {
			return true
		}
	}
	return false
}

func (s *Service) home(repo string, config Object) string {
	a := fleet.ObjectOrEmpty(config["assignments"])
	for k, v := range fleet.ObjectOrEmpty(a["repositories"]) {
		if strings.EqualFold(k, repo) {
			if host, ok := v.(string); ok {
				return host
			}
		}
	}
	return ""
}
func (s *Service) row(ctx context.Context, repo string, issue Object) (Object, *jobs.Job, error) {
	if _, ok := ctx.Value(usageDecisionKey{}).(*usageDecision); !ok {
		ctx = context.WithValue(ctx, usageDecisionKey{}, &usageDecision{})
	}
	n, ok := number(issue["number"])
	if !ok {
		return nil, nil, fail(502, "Issue番号を読み取れません。")
	}
	snapshot, batched := ctx.Value(listSnapshotKey{}).(*listSnapshot)
	job, e := s.rowJob(ctx, repo, n, snapshot, batched)
	if e != nil {
		return nil, nil, e
	}
	sourceID, source, e := s.rowSource(ctx, issue, snapshot, batched)
	if e != nil {
		return nil, nil, e
	}
	waiting := source["project"] == repo
	status := shownStatus(issue, job, waiting)
	if status == "waiting" && fixedTestHoldWithoutDecision(job, s.o.RunLimit) && !s.projectedUsageHeld(ctx, job) {
		status = "review_held"
	}
	if job != nil && job.Status == "queued" && s.o.Postgres != nil {
		waitingDeps, err := s.dependenciesPending(ctx, job, snapshot, batched)
		if err != nil {
			return nil, nil, err
		}
		if waitingDeps {
			status = "dependency_waiting"
		}
	}
	desk, e := s.rowDeskNumber(ctx, repo, n, snapshot, batched)
	if e != nil {
		return nil, nil, e
	}
	config := Object(nil)
	if batched {
		config = snapshot.config
	} else if config, e = s.configuration(); e != nil {
		return nil, nil, e
	}
	host := s.home(repo, config)
	if job != nil && job.LeaseHost != nil {
		host = *job.LeaseHost
	}
	intakeID := sourceID
	if parent := fleet.Text(source["parentSplit"]); parent != "" {
		intakeID = parent
	}
	shownTitle, _ := shownIssue(fleet.Text(issue["title"]), fleet.Text(issue["body"]), s.numberLookup(ctx, repo))
	r := Object{"project": repo, "number": n, "desk": desk, "title": issue["title"], "shownTitle": shownTitle, "state": issue["state"], "status": status, "host": host, "createdAt": issue["created_at"], "updatedAt": issueUpdatedAt(issue["updated_at"], job), "closedAt": issue["closed_at"], "url": s.link(repo, "issues", n), "followup": strings.Contains(fleet.Text(issue["body"]), "<!-- issue-desk:followup -->"), "workType": "unknown"}
	if waiting {
		r["intakeId"] = intakeID
	}
	if source["projectDefinition"] != nil {
		r["projectDefinition"] = source["projectDefinition"]
	}
	if origin, e := s.issueOrigin(ctx, repo, fleet.Text(issue["body"]), source); e != nil {
		return nil, nil, e
	} else if origin != nil {
		r["origin"] = origin
	}
	if job != nil {
		s.copyJobView(r, repo, status, job)
		if e := s.applyJobProjection(ctx, r, repo, status, job, batched); e != nil {
			return nil, nil, e
		}
	}
	r["workTypeLabel"] = workTypeLabels[fleet.Text(r["workType"])]
	if status == "closed" {
		reason, e := s.closeReason(ctx, repo, issue)
		if e != nil {
			return nil, nil, e
		}
		r["closeReason"] = reason
	}
	return r, job, nil
}

// rowJob は Issue の依頼を、一覧の読み取り結果か依頼の台帳から読む。無ければ nil を返す。
func (s *Service) rowJob(ctx context.Context, repo string, n int64, snapshot *listSnapshot, batched bool) (*jobs.Job, error) {
	if batched {
		return snapshot.jobs[numbers.Issue{Project: repo, Number: n}], nil
	}
	return s.o.Jobs.ByIssue(ctx, repo, n)
}

// rowSource は受付が起票した Issue の元の相談の番号と記録を、一覧の読み取り結果か保存から読む。受付の起票でなければ空を返す。
func (s *Service) rowSource(ctx context.Context, issue Object, snapshot *listSnapshot, batched bool) (string, Object, error) {
	sourceID := ""
	if s.o.Own(fleet.ObjectOrEmpty(issue["user"])) {
		sourceID = filing.DraftID(fleet.Text(issue["body"]))
	}
	if sourceID == "" {
		return "", nil, nil
	}
	if batched {
		return sourceID, snapshot.drafts[sourceID], nil
	}
	source, e := s.read(ctx, "drafts", sourceID)
	if e != nil {
		return "", nil, e
	}
	return sourceID, source, nil
}

// copyJobView は依頼の記録から一覧に出す欄（作業の種類・止まった理由・PR・待ちの優先度など）を行に写す。
func (s *Service) copyJobView(r Object, repo, status string, job *jobs.Job) {
	view := job.View
	r["workType"] = view["workType"]
	if r["workType"] == nil {
		r["workType"] = "unknown"
	}
	for _, k := range []string{"error", "dependencyReason", "scopeWait", "executionSettings", "releasePending", "publishPending", "deletionApproval", "review", "overLimitApprovals", "dispatchSkip", "dispatchRecord"} {
		if view[k] != nil {
			r[k] = view[k]
		}
	}
	if job.PR != nil {
		r["pr"] = Object{"number": *job.PR, "url": s.link(repo, "pulls", *job.PR)}
	}
	if status == "queued" || status == "dependency_waiting" {
		r["priority"] = jobPriorityView(job)
	}
}

// applyJobProjection は依頼の状態から作った表示を行に重ねる。検索の一括読みでは、表示の作成を後でまとめて行うよう予約する。
func (s *Service) applyJobProjection(ctx context.Context, r Object, repo, status string, job *jobs.Job, batched bool) error {
	for _, key := range []string{"result", "error", "review", "deletionApproval"} {
		delete(r, key)
	}
	apply := func(projection Object) {
		projection["followup"] = projection["followup"] == true || r["followup"] == true
		for key, value := range projection {
			r[key] = value
		}
		r["workTypeLabel"] = workTypeLabels[fleet.Text(r["workType"])]
	}
	if deferred, ok := ctx.Value(searchProjectionKey{}).(*searchProjections); ok && batched {
		wait, e := s.scopeWait(ctx, repo, job)
		if e != nil {
			return e
		}
		deferred.add(r, func() error {
			projection, e := s.jobProjectionWithWait(ctx, repo, status, job, wait)
			if e == nil {
				apply(projection)
			}
			return e
		})
		return nil
	}
	projection, e := s.jobProjection(ctx, repo, status, job)
	if e != nil {
		return e
	}
	apply(projection)
	return nil
}

// dependenciesPending は待ちの依頼にまだ揃っていない依存があるかを、一覧の読み取り結果かDBから判定する。
func (s *Service) dependenciesPending(ctx context.Context, job *jobs.Job, snapshot *listSnapshot, batched bool) (bool, error) {
	var pending int
	if batched {
		pending = len(snapshot.dependencies[job.ID])
	} else if err := s.o.Postgres.QueryRowContext(ctx, `SELECT count(*) FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id WHERE d.job_id=$1 AND NOT (`+jobs.DependencyPairReadySQL+`)`, job.ID).Scan(&pending); err != nil {
		return false, err
	}
	if !batched && pending == 0 {
		ready, err := jobs.DependenciesReady(ctx, s.o.Postgres, job.ID, job.View)
		if err != nil {
			return false, err
		}
		if !ready {
			pending++
		}
	}
	return pending > 0, nil
}

// rowDeskNumber は Issue の受付の共通番号を、一覧の読み取り結果か番号の台帳から読む。無ければ nil を返す。
func (s *Service) rowDeskNumber(ctx context.Context, repo string, n int64, snapshot *listSnapshot, batched bool) (any, error) {
	var desk any
	if batched {
		if value, found := snapshot.numbers[numbers.Issue{Project: repo, Number: n}]; found {
			desk = value
		}
	} else if n, found, e := s.o.Numbers.Number(ctx, repo, n); e != nil {
		return nil, e
	} else if found {
		desk = n
	}
	return desk, nil
}

// jobPriorityView は待ちの依頼の優先度と、利用者の指定かAIの判断かを表示の形にする。
func jobPriorityView(job *jobs.Job) Object {
	level := "normal"
	if job.Priority == 1 {
		level = "high"
	}
	mark := fleet.ObjectOrEmpty(job.View["priority"])
	source := "ai"
	if mark["source"] == "user" {
		source = "user"
	}
	return Object{"level": level, "source": source, "reason": mark["reason"], "by": mark["by"]}
}

func (s *Service) Detail(ctx context.Context, repo string, n int64, actions, summary bool) (Object, error) {
	return s.detail(ctx, repo, n, actions, summary, nil)
}

func (s *Service) detail(ctx context.Context, repo string, n int64, actions, summary bool, usageHeld *bool) (Object, error) {
	issue, e := s.detailIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if actions {
		if e := s.syncCloseReason(ctx, repo, n, issue); e != nil {
			return nil, e
		}
	}
	lookup := s.numberLookup(ctx, repo)
	ctx = context.WithValue(ctx, detailLookupKey{}, detailLookup{repo: repo, lookup: lookup})
	ctx = context.WithValue(ctx, dispatchDetailKey{}, true)
	ctx = context.WithValue(ctx, usageDecisionKey{}, &usageDecision{})
	r, job, e := s.row(ctx, repo, issue)
	if e != nil {
		return nil, e
	}
	comments, e := s.o.Gitea.Comments(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if job != nil {
		r["finalApprovalTransfers"] = finalApprovalTransfersView(job)
	}
	r["id"] = issue["id"]
	r["author"] = fleet.ObjectOrEmpty(issue["user"])["login"]
	shownTitle, shownBody := shownIssue(fleet.Text(issue["title"]), fleet.Text(issue["body"]), lookup)
	r["shownTitle"] = shownTitle
	setDetailBody(r, issue["body"], shownBody)
	r["images"] = []any{}
	r["comments"] = []any{}
	r["consultationProvider"] = "codex"
	r["generation"] = nil
	held := s.projectedUsageHeld(ctx, job)
	if usageHeld != nil {
		*usageHeld = held
	}
	r["retryable"] = oneOf(fleet.Text(r["status"]), "failed", "review_held", "prerequisite_waiting") || held
	r["dependencies"] = s.dependencies(ctx, repo, fleet.Text(issue["body"]))
	if !summary {
		waits := []any{}
		if job != nil && s.o.Postgres != nil {
			waits, e = s.dependencyWaits(ctx, job.ID)
			if e != nil {
				return nil, e
			}
		}
		r["dependencyWaits"] = waits
	}
	r["implementedBy"] = r["host"]
	if job != nil {
		if progress := executionPlanProjection(job); progress != nil {
			r["executionProgress"] = progress
		}
		if execution := job.View["workerExecution"]; execution != nil {
			r["workerExecution"] = execution
		}
		if result, ok := r["result"].(Object); ok {
			for key, value := range leaseDiagnosticsProjection(job.Result) {
				result[key] = value
			}
			if diagnostic := fixedTestDiagnosticProjection(job.Result); diagnostic != nil {
				result["fixedTestDiagnostic"] = diagnostic
			}
			if review := reviewFindingsProjection(job.Result); review != nil {
				result["reviewFindings"] = review
			}
		}
		r["generation"] = job.Generation
		r["permissionApproval"], e = s.permissionTarget(ctx, issue, job)
		if e != nil {
			return nil, e
		}
	}
	r["comments"] = s.shownComments(comments, lookup)
	if e := s.detailFields(ctx, repo, n, issue, r); e != nil {
		return nil, e
	}
	if !summary {
		if e := s.detailCloseHistory(ctx, repo, n, r); e != nil {
			return nil, e
		}
	}
	if actions {
		detailActions(r)
	}
	if summary {
		return summarizeDetail(r), nil
	}
	return r, nil
}

// detailIssue は依頼を読み、受付から登録した操作できる依頼かを確かめる。
func (s *Service) detailIssue(ctx context.Context, repo string, n int64) (Object, error) {
	if e := s.o.Project(repo); e != nil {
		return nil, e
	}
	issue, e := s.o.Gitea.Issue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	author, e := s.detailAuthorChecked(issue)
	if e != nil {
		return nil, e
	}
	if issue["pull_request"] != nil || !author || coordination(repo, issue) {
		return nil, fail(403, "この依頼を操作できません。")
	}
	return issue, nil
}

// syncCloseReason は開いている依頼の保存済みの終了理由を消し、閉じた依頼では終了理由の基準を記録する。
func (s *Service) syncCloseReason(ctx context.Context, repo string, n int64, issue Object) error {
	if issue["state"] != "closed" {
		if e := s.forgetCloseReason(ctx, repo, n); e != nil {
			return e
		}
	} else {
		_ = s.ensureCloseBaseline(ctx)
	}
	return nil
}

// finalApprovalTransfersView は最終承認を新しい版へ引き継いだ記録を表示の形にする。
func finalApprovalTransfersView(job *jobs.Job) []any {
	transfers := []any{}
	for _, raw := range fleet.Items(job.View["finalApprovalTransfers"]) {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		transfers = append(transfers, Object{"id": entry["id"], "fromHead": entry["fromHead"], "toHead": entry["toHead"], "diffFingerprint": entry["diffFingerprint"], "at": entry["at"]})
	}
	return transfers
}

// shownComments は状態の印を除いたコメントを、表示用の本文と種類にして並べる。
func (s *Service) shownComments(comments []Object, lookup numberLookup) []any {
	shown := []any{}
	for _, c := range comments {
		body := fleet.Text(c["body"])
		if strings.Contains(body, "<!-- gcr:issue-desk-state ") {
			continue
		}
		body = shownCommentMarkers.ReplaceAllString(body, "")
		shown = append(shown, Object{"id": c["id"], "author": fleet.ObjectOrEmpty(c["user"])["login"], "body": shownComment(fleet.Text(c["body"]), body, lookup), "createdAt": c["created_at"], "updatedAt": c["updated_at"], "bot": strings.Contains(fleet.Text(c["body"]), "<!-- gcr:"), "mine": s.o.Own(fleet.ObjectOrEmpty(c["user"])) || strings.Contains(fleet.Text(c["body"]), "<!-- issue-desk-action:"), "kind": commentKind(fleet.Text(c["body"])), "images": []any{}})
	}
	return shown
}

// detailActions は依頼の状態から今できる操作と、閉じられない理由を詳細に足す。
func detailActions(r Object) {
	a := []string{}
	for _, action := range []string{"retry", "resume", "answer", "fix", "reply", "review", "stop", "priority", "handover", "approve_permissions", "reason", "close"} {
		if refusal(r, action) == "" {
			a = append(a, action)
		}
	}
	r["actions"] = a
	r["closeRefusal"] = nil
	if r["state"] == "open" {
		if reason := refusal(r, "close"); reason != "" {
			r["closeRefusal"] = reason
		}
	}
}
func oneOf(v string, values ...string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func setDetailBody(row Object, body any, shownBody string) {
	row["body"] = body
	if shownBody == body {
		delete(row, "shownBody")
	} else {
		row["shownBody"] = shownBody
	}
}
