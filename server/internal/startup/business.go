package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"issue-desk-intake/server/internal/adminapi"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/filingcheck"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/httpapi"
	"issue-desk-intake/server/internal/images"
	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/notices"
	"issue-desk-intake/server/internal/sessions"
)

func (a *App) registerBusiness(ctx context.Context) (err error) {
	a.AI, err = consultationai.New(consultationai.Options{
		Config: func(ctx context.Context) (map[string]any, error) { return a.Values() }, Models: a.Models, Runner: gatedRunner{a.Runner, a}, Gitea: a.Gitea, Records: a.Records,
		RuntimeDir: a.Config.RuntimeDir, SchemaDir: filepath.Join(a.Config.ApplicationDir, "server", "schemas"),
		Reference: a.reference, ReferenceContinuation: a.referenceContinuation, EagerOpenRequests: true, Images: a.aiImages, ResolveNumbers: a.resolveNumbers, RecordUsage: a.recordUsage, ReadUsage: a.readIntakeCallUsage,
		Split:       a.splitConsultation,
		Environment: a.consultationEnvironment,
	})
	if err != nil {
		return err
	}
	a.Checker, err = filingcheck.New(filingcheck.Options{Records: a.Records, Gitea: a.Gitea, Jobs: a.Jobs, AppDB: a.SQLite.DB(), Postgres: a.DB, Config: func(context.Context) (map[string]any, error) { return a.Values() }, Project: a.Project, Own: a.Own, DeskNumber: a.DeskNumber, ResolvePreceding: a.resolvePreceding, Answer: a.AI.Answer, Revision: a.Revision, SchemaDir: filepath.Join(a.Config.ApplicationDir, "server", "schemas")})
	if err != nil {
		return err
	}
	a.FilingCheck = func(ctx context.Context, d consultations.Record) error {
		return a.Checker.Check(ctx, d, filing.CheckTrigger(ctx))
	}
	a.Filing, err = filing.New(ctx, filing.Options{
		RepositoryArchived: a.Gitea.RepositoryArchived,
		GroupIssueLinks:    repositoryGroupLinksAdapter{client: a.Gitea},
		RepositoryGroups:   a.filingRepositoryGroups,
		ValidateFollowup:   a.validateFollowup,
		ValidateNewPost:    a.validateFollowupNewPost,
		IncidentLedger:     a.incidentLedger, Acquire: a.acquireGate,
		Records: a.Records, API: a.Gitea, Jobs: a.Jobs, Project: a.Project, Own: a.Own, Publish: a.Publish, Attach: a.Attachments.Attach,
		Link: func(repo string, n int64) string { return a.Link(repo, "issues", n) },
		Build: func(ctx context.Context, d consultations.Record) (filing.Prepared, error) {
			if d["proposal"] != nil {
				return a.prepareFollowup(ctx, d)
			}
			c, err := a.filingConfig()
			if err != nil {
				return filing.Prepared{}, err
			}
			return filing.Prepare(ctx, c, d)
		},
		Check: func(ctx context.Context, d consultations.Record) error {
			if d["proposal"] != nil {
				return a.validateFollowup(ctx, d)
			}
			c, err := a.filingConfig()
			if err != nil {
				return err
			}
			if err = filing.CheckPolicy(ctx, a.Records, d, a.incidentLedger); err != nil {
				return err
			}
			return filing.ValidateSplit(ctx, c, d)
		},
		SplitPlan: filing.PlanSplit, EstimateScope: a.AI.EstimateScope, BackfillScope: a.backfillScope,
	})
	if err != nil {
		return err
	}
	a.Consultations, err = consultations.New(consultations.Options{FamilyJobs: a.familyJobs, FamilyIssues: a.familyIssues, Records: a.Records, Auth: a.Auth, PublicURL: a.Config.PublicURL, Project: a.Project,
		PublishDraft:       func(d consultations.Record) { a.Publish(map[string]any(d)) },
		RepositoryArchived: a.Gitea.RepositoryArchived,
		Providers:          func() []string { return []string{"codex", "claude"} },
		InitialTier: func(provider, requested string, at time.Time) (consultations.Record, error) {
			c, e := a.Values()
			if e != nil {
				return nil, e
			}
			v, e := models.InitialTier(c, map[string]any{"consultationProvider": provider, "requestedTier": requested}, at)
			return consultations.Record(v), e
		},
		SubmissionQueued: func(d consultations.Record) {
			if a.Engine != nil {
				a.Engine.SubmitQueued(d)
			}
		},
		ChangeTier: a.changeTier, Submit: a.Filing.SubmitExplicit,
	})
	if err != nil {
		return err
	}
	c, err := a.Values()
	if err != nil {
		return err
	}
	a.Stalls, err = consultations.NewErrorStalls(ctx, a.Consultations, a.Revision, a.Config.RuntimeDir)
	if err != nil {
		return err
	}
	a.Engine, err = consultations.NewEngine(a.exclusiveCtx, consultations.EngineOptions{Stalls: a.Stalls, Service: a.Consultations, Turn: a.AI.Turn, Submit: func(ctx context.Context, d consultations.Record) (consultations.Record, error) {
		p, e := a.submissionPrincipal(ctx, d)
		if e != nil {
			return nil, e
		}
		return a.Filing.SubmitAccepted(ctx, fleet.Text(d["id"]), consultations.Record{"revision": d["revision"]}, p)
	}, Principal: a.principal, Publish: func(d consultations.Record) { a.Publish(map[string]any(d)) }, Concurrency: int(fleet.Number(fleet.Object(c["intake"])["aiConcurrency"]))})
	if err != nil {
		return err
	}
	a.Consultations.Register(a.Mux)
	a.Mux.Handle("/api/events", a.Live)
	if err = sessions.Register(a.Mux, sessions.Options{Auth: a.Auth, Store: a.SQLite, PublicURL: a.Config.PublicURL}); err != nil {
		return err
	}
	if err = adminapi.Register(a.Mux, &adminapi.Options{AppDB: a.SQLite.DB(), Postgres: a.DB, RuntimeDir: a.Config.RuntimeDir, Auth: a.Auth, Config: c, Publish: a.Publish, AllowProject: func(repo string) bool { return a.Project(repo) == nil }, DirectPRBase: a.defaultBranchHead, Log: a.Log}); err != nil {
		return err
	}
	if err = images.Register(a.Mux, images.Options{AppDB: a.SQLite.DB(), Auth: a.Auth, Project: a.Project, Issue: func(ctx context.Context, repo string, n int64) error {
		if e := a.Project(repo); e != nil {
			return e
		}
		_, e := a.Gitea.Issue(ctx, repo, n)
		return e
	}, Draft: a.draftRecord("drafts"), Todo: a.draftRecord("todos"), DraftWritable: func(d map[string]any) bool {
		return d["status"] != "closed" && d["issue"] == nil && d["status"] != "submitted" && d["status"] != "submitting" && d["status"] != "submit_unknown"
	}}); err != nil {
		return err
	}
	limits := a.retryLimits(c)
	a.Issues, err = issueapi.Register(a.Mux, issueapi.Options{Principal: a.principal, Drafts: a.Records, ConfigReader: a.Values, DedicatedAuthor: a.DedicatedAuthor, AppDB: a.SQLite.DB(), Postgres: a.DB, Auth: a.Auth, Gitea: a.Gitea, Numbers: a.Numbers, Jobs: a.Jobs, Config: c, Project: a.Project, Repositories: a.Repositories, Own: a.Own, PublicLink: a.Link, Publish: a.Publish, Answer: a.AI.Answer, Attachments: a.Attachments, RunLimit: int64(limits.run), UsageLimit: limits.usage, UsageWeights: limits.weights, ActiveList: a.activeIssues})
	if err != nil {
		return err
	}
	if err = a.registerCleanup(); err != nil {
		return err
	}
	return httpapi.RegisterWorkerRoutes(a.Mux, a.workerOptions())
}

func (a *App) workerOptions() httpapi.WorkerRouteOptions {
	config := a.CurrentConfig()
	notice := &notices.Service{DB: a.SQLite.DB(), Repositories: func() ([]string, error) { return a.Repositories(), nil }}
	return httpapi.WorkerRouteOptions{ClaudeAPIKeyTokenFile: config.TokenFile, Publish: a.Publish, PublicLink: a.Link, SystemAlerts: a.systemAlerts, Gitea: a.Gitea, AIAlerts: a.AIFailures.Alerts, ContractMismatches: a.ContractMismatches, ReadComments: a.Gitea.Comments, ReportAuthor: a.executionReportAuthor, TrustedWriter: a.TrustedWriter, LiveAIAlerts: a.liveAIAlerts, DraftStallAlerts: a.draftStallAlerts, HostDrift: a.Drift, DB: a.DB, AppDB: a.SQLite.DB(), Auth: a.Auth, FleetPath: config.FleetPath, AssignmentsPath: config.AssignmentsPath, RuntimeDir: config.RuntimeDir, ModelConfig: a.Values, ModelProviders: a.modelProviders, ReadIssue: a.Gitea.Issue, IssueState: a.Gitea.SetIssueState, ReleasePolicy: a.releasePolicy, Notice: notice.Receive, TokenNames: a.SQLite.TokenNames, DeskNumber: a.DeskNumber, AttentionDraftAlerts: a.draftAlerts, StatisticsDrafts: func(ctx context.Context) ([]map[string]any, error) {
		rows, e := a.Records.List(ctx, "drafts")
		out := []map[string]any{}
		for _, d := range rows {
			out = append(out, map[string]any(d))
		}
		return out, e
	}, DraftIssue: a.firstDraftIssue}

}

func (a *App) draftAlerts(ctx context.Context) ([]any, error) {
	rows, err := a.AI.DraftAlerts(ctx)
	if err != nil {
		return nil, err
	}
	if a.Proposals == nil {
		return nil, errors.New("保存された提案の照合を準備中です")
	}
	proposals, err := a.Proposals.UnfiledProposalAlerts(ctx)
	if err != nil {
		return nil, err
	}
	return append(rows, proposals...), nil
}

// draftStallAlerts は止まりの一覧（/api/stalls/alerts）へ出す相談の通知を集める。
// 相談処理の例外（a.Stalls）と、起票前確認の判定が上限に達した相談・資料不足の集約（a.Checker）は、
// 同じ intake_error の形（id・title・detail）で返り、画面とdesk-alertsの要対応に並ぶ。
func (a *App) draftStallAlerts(ctx context.Context) ([]any, error) {
	rows, err := a.Stalls.Alerts(ctx)
	if err != nil {
		return nil, err
	}
	if a.Checker == nil {
		return nil, errors.New("起票前確認の照合を準備中です")
	}
	limits, err := a.Checker.FilingCheckLimitAlerts(ctx)
	if err != nil {
		return nil, err
	}
	return append(rows, limits...), nil
}

func (a *App) submissionPrincipal(ctx context.Context, d consultations.Record) (auth.Principal, error) {
	sender := fleet.Text(d["owner"])
	if request := fleet.Object(d["submissionRequest"]); request != nil {
		sender = fleet.Text(request["owner"])
	}
	return a.principal(ctx, sender)
}

func (a *App) principal(ctx context.Context, owner string) (auth.Principal, error) {
	if owner == "" {
		return auth.Principal{Kind: auth.Screen}, nil
	}
	if !strings.HasPrefix(owner, "token:") {
		return auth.Principal{}, contract.NewProblem(401, "送り手のトークンを確認できません。", "")
	}
	token, ok, err := a.SQLite.TokenByID(ctx, strings.TrimPrefix(owner, "token:"))
	if err != nil || !ok {
		return auth.Principal{}, contract.NewProblem(401, "送り手のトークンを確認できません。", "")
	}
	return auth.Principal{Kind: auth.PC, Name: token.Name, TokenID: token.ID, Delegated: token.Delegated}, nil
}
func (a *App) draftRecord(table string) func(context.Context, string) (map[string]any, error) {
	return func(ctx context.Context, id string) (map[string]any, error) {
		v, _, e := a.Records.Get(ctx, table, id)
		return map[string]any(v), e
	}
}

// retryLimits は、再試行の判定に使う実装回数・累計使用量の上限と重みを、担当表(assignments.json)から読む。
// 以前は設定にない usageLimit を読んでおり、上限が0になって、すべての再試行が「累計使用量が未知です」で断られた。
type retryLimitValues struct {
	run     float64
	usage   float64
	weights map[string]any
}

func (a *App) retryLimits(c map[string]any) retryLimitValues {
	out := retryLimitValues{run: fleet.Number(c["implementationRunLimit"]), usage: fleet.Number(c["usageLimit"]), weights: fleet.Object(c["usageWeights"])}
	if a.Fleet == nil {
		// 担当表を読めない最小の構成では、設定値と既定値だけを使う。
	} else if document, assignments, err := a.Fleet.Load(); err == nil {
		if out.run <= 0 {
			out.run = fleet.Number(assignments.Output["implementationRunLimit"])
		}
		if out.usage <= 0 {
			out.usage = fleet.Number(assignments.Output["implementationUsageLimit"])
		}
		if out.weights == nil {
			out.weights = fleet.Object(document.Defaults["usageWeights"])
		}
	}
	if out.usage <= 0 {
		out.usage = 8000000
	}
	return out
}

// consultationEnvironment uses the implementation assignment, not the fleet
// repository list, which may also include deployment-only machines.
func (a *App) consultationEnvironment(ctx context.Context, repo string) ([]consultationai.EnvironmentHost, error) {
	document, assignments, err := a.Fleet.Load()
	if err != nil {
		return nil, err
	}
	var result []consultationai.EnvironmentHost
	for _, name := range assignments.Implementers[repo] {
		values := fleet.Object(fleet.Clone(fleet.Object(fleet.EffectiveConfig(document, name)["environment"])))
		if values == nil {
			values = map[string]any{}
		}
		if target := fleet.NativeTestTarget(document, assignments, name, repo); target != "" {
			values["nativeTest"] = fleet.Object(fleet.EffectiveConfig(document, target)["environment"])
		}
		if a.DB != nil {
			var raw []byte
			var heartbeat time.Time
			err := a.DB.QueryRowContext(ctx, "SELECT capabilities, heartbeat_at FROM hosts WHERE host=$1", name).Scan(&raw, &heartbeat)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil && time.Since(heartbeat) >= 0 && time.Since(heartbeat) <= 15*time.Minute {
				var capabilities map[string]any
				if err := json.Unmarshal(raw, &capabilities); err != nil {
					return nil, err
				}
				values["executionPlanVersion"] = capabilities["executionPlanVersion"]
				for _, key := range []string{"race", "gcc", "cgo", "docker", "browser", "systemd", "windows", "network"} {
					if value, exists := fleet.Object(capabilities["environment"])[key]; exists {
						values[key] = nil
						if boolean, ok := value.(bool); ok {
							values[key] = boolean
						}
					}
				}
			}
		}
		result = append(result, consultationai.EnvironmentHost{Name: name, Values: values})
	}
	return result, nil
}

// cleanupAbandonedSessions removes conversation markers left by a stopped owner.
// It runs only while starting exclusive work: the writer lease is held, the
// previous owner has drained its accepted turns, and this App has not bound the
// Engine or served requests yet, so no marker belongs to a running turn.
// Cleanup continues past a marker or directory it cannot remove; each failure
// is logged on its own line with its file name without stopping the start, and
// the same files are tried again on the next start.
func (a *App) cleanupAbandonedSessions() {
	if a.AI == nil {
		return
	}
	err := a.AI.CleanupAbandoned()
	if err == nil {
		return
	}
	failures := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		failures = joined.Unwrap()
	}
	// One line per failure keeps every file name within the cause length limit.
	for _, failure := range failures {
		a.Log("中断した相談の会話記録を後片付けできませんでした。起動は続けます。原因: " + contract.SafeCause(failure))
	}
}
