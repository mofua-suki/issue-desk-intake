package startup

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/attachments"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/filingcheck"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/hostdrift"
	"issue-desk-intake/server/internal/httpapi"
	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/live"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/processactivity"
	"issue-desk-intake/server/internal/projects"
	"issue-desk-intake/server/internal/proposals"
	"issue-desk-intake/server/internal/runtimegate"
	"issue-desk-intake/server/internal/screenapi"
	"issue-desk-intake/server/internal/store"
)

// App owns resources opened only after the shared writer lease is acquired.
type App struct {
	RuntimeMeasurements   *operations.RuntimeMeasurements
	RepositoryReads       *operations.RepositoryReads
	AIFailures            *operations.AIFailures
	Operations            func() (*operations.Service, error)
	Config                *Config
	DB                    *sql.DB
	SQLite                *store.SQLite
	Auth                  *auth.Authenticator
	Records               *consultations.Records
	Gitea                 *gitea.Client
	Numbers               *numbers.Store
	Jobs                  *jobs.Store
	Models                *models.Service
	Runner                *ai.Runner
	providers             providerCache
	Gate                  *runtimegate.Gate
	gateMu                sync.RWMutex
	AI                    *consultationai.Service
	Filing                *filing.Service
	Checker               *filingcheck.Service
	Stalls                *consultations.ErrorStalls
	Drift                 *hostdrift.Service
	FilingCheck           func(context.Context, consultations.Record) error
	SizeReference         func(context.Context, string) (string, error)
	MeasurementReference  func(context.Context, string) (string, error)
	Consultations         *consultations.Service
	Engine                *consultations.Engine
	Live                  *live.Events
	Fleet                 *fleet.Service
	Issues                *issueapi.Service
	Projects              *projects.Service
	Proposals             *proposals.Service
	proposalsScanInterval time.Duration // Set before StartExclusive; zero uses the production default.
	Screen                *screenapi.Service
	Attachments           *attachments.Service
	Mux                   *http.ServeMux
	Revision              string
	Log                   func(string)
	mu                    sync.RWMutex
	ctx                   context.Context
	cancel                context.CancelFunc
	wg                    sync.WaitGroup
	exclusiveMu           sync.Mutex
	exclusiveStarted      bool
	exclusiveClosed       bool
	exclusiveErr          error
	exclusiveCtx          context.Context
	exclusiveCancel       context.CancelFunc
	backgroundPending     []func(context.Context)
	periodicNames         []string
	deferBackground       bool
	backgroundMu          sync.Mutex
	backgroundCtx         context.Context
	backgroundCancel      context.CancelFunc
	backgroundStopped     bool
	closeOnce             sync.Once
	closeErr              error
	numberMu              sync.Mutex
	numberAt              time.Time
	reviewNotesRunning    atomic.Bool
	reviewNotesDates      reviewNotesAttempts
	apiMu                 sync.Mutex
	apiReference          *consultationai.Reference
	intakeID              int64
	ownIDs                map[int64]bool
	writerIDs             map[int64]bool
	ContractMismatches    *operations.ContractMismatches
}

func OpenApp(ctx context.Context, config *Config, db *sql.DB, revision string, log func(string), mismatches ...*operations.ContractMismatches) (*App, error) {
	a, err := OpenBusinessApp(ctx, config, db, revision, log, mismatches...)
	if err != nil {
		return nil, err
	}
	if err = a.StartExclusive(ctx); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// OpenBusinessApp initializes the shared business services without starting exclusive work.
func OpenBusinessApp(ctx context.Context, config *Config, db *sql.DB, revision string, log func(string), mismatches ...*operations.ContractMismatches) (_ *App, err error) {
	if config == nil || db == nil {
		return nil, errors.New("Serverの設定と共有DBが必要です")
	}
	if log == nil {
		log = func(string) {}
	}
	lifecycle, cancel := context.WithCancel(ctx)
	a := &App{Config: config, DB: db, Revision: revision, Log: log, ctx: lifecycle, cancel: cancel, Mux: http.NewServeMux()}
	a.exclusiveCtx, a.exclusiveCancel = context.WithCancel(context.Background())
	a.deferBackground = true
	a.ContractMismatches = &operations.ContractMismatches{}
	if len(mismatches) > 0 && mismatches[0] != nil {
		a.ContractMismatches = mismatches[0]
	}
	defer func() {
		if err != nil {
			a.Close()
		}
	}()
	a.SQLite, err = openApplicationStore(ctx, config)
	if err != nil {
		return nil, err
	}
	a.startTopicSummaryBackfill(config.ApplicationSchema)
	repairSchemaDefaults(ctx, a.SQLite, config.ApplicationSchema, log)
	a.Auth, err = auth.New(auth.Config{PublicURL: config.PublicURL, Port: config.Port, InternalHost: os.Getenv("ISSUE_DESK_INTAKE_INTERNAL_HOST")}, &store.AuthStore{SQLite: a.SQLite, Postgres: db})
	if err != nil {
		return nil, err
	}
	spec, err := httpapi.OpenAPISpec(a.Auth, config.ApplicationDir)
	if err != nil {
		return nil, err
	}
	a.Mux.Handle("/api/openapi.json", spec)
	a.Fleet = &fleet.Service{DB: db, FleetPath: config.FleetPath, AssignmentsPath: config.AssignmentsPath, ReleasePolicy: a.releasePolicy}
	if _, err = config.Shared(ctx, a.Fleet); err != nil {
		return nil, err
	}
	token, err := config.CredentialPath()
	if err != nil {
		return nil, err
	}
	a.Gitea, err = gitea.New(fleet.Text(config.Values["giteaUrl"]), token, nil)
	if err != nil {
		return nil, err
	}
	if public := fleet.Text(config.Values["publicUrl"]); public != "" {
		a.Gitea, err = a.Gitea.WithPublicURL(public)
		if err != nil {
			return nil, err
		}
	}
	identity := map[string]any{}
	if err = a.Gitea.Do(ctx, "GET", "/user", nil, &identity); err != nil || identity["login"] != config.Values["intakeAccount"] {
		return nil, errors.New("Server専用のGiteaアカウントを確認できません")
	}
	if err = a.resolveWriters(ctx, identity); err != nil {
		return nil, err
	}
	a.Records, err = consultations.OpenRecords(ctx, a.SQLite.DB())
	if err != nil {
		return nil, err
	}
	a.Numbers, err = numbers.OpenLedger(ctx, a.SQLite.DB(), config.NumberLedgerPath())
	if err != nil {
		return nil, err
	}
	if _, err = a.Numbers.Reconcile(ctx); err != nil {
		return nil, err
	}
	a.Models = &models.Service{DB: db}
	a.Drift = &hostdrift.Service{DB: db, CompatLimit: 3}
	a.Runner = &ai.Runner{Codex: config.Codex, Claude: config.Claude, Security: ai.Security{RuntimeDir: config.RuntimeDir, ApplicationDir: config.ApplicationDir, TokenFiles: []string{token, fleet.Text(config.Values["tokenFile"]), fleet.Text(config.Values["workerTokenFile"])}}}
	a.Runner.ObserveLimits = a.recordLimits
	if err = a.initializeObservability(); err != nil {
		return nil, err
	}
	a.Live = live.New(lifecycle, a.Auth)
	a.Jobs, err = jobs.New(db, jobs.Options{AppDB: a.SQLite.DB(), ReadIssue: a.Gitea.Issue, ReadComments: a.Gitea.Comments, ReadPull: a.executionPull, ReportAuthor: a.executionReportAuthor, Publish: a.Publish})
	if err != nil {
		return nil, err
	}
	a.Attachments, err = attachments.New(attachments.Options{AppDB: a.SQLite.DB(), Gitea: a.Gitea, Own: a.Own, Publish: a.Publish})
	if err != nil {
		return nil, err
	}
	if err = a.registerBusiness(ctx); err != nil {
		return nil, err
	}
	if err = a.registerProjects(); err != nil {
		return nil, err
	}
	if err = a.registerProposals(); err != nil {
		return nil, err
	}
	if err = a.registerScreen(); err != nil {
		return nil, err
	}
	return a, nil
}

// StartExclusive starts the single-owner services once; it does not own the business lifetime.
func (a *App) StartExclusive(ctx context.Context) error {
	a.exclusiveMu.Lock()
	defer a.exclusiveMu.Unlock()
	if a.exclusiveStarted {
		if a.exclusiveErr != nil {
			return a.exclusiveErr
		}
		a.backgroundMu.Lock()
		stopped := a.backgroundStopped
		a.backgroundMu.Unlock()
		if stopped {
			return errors.New("単独実行は停止済みです")
		}
		return nil
	}
	if a.exclusiveClosed {
		return errors.New("Serverを停止中です")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.backgroundMu.Lock()
	if a.backgroundStopped {
		a.backgroundMu.Unlock()
		return errors.New("単独実行は停止済みです")
	}
	a.backgroundCtx, a.backgroundCancel = context.WithCancel(ctx)
	pending := a.backgroundPending
	a.backgroundPending = nil
	a.deferBackground = false
	a.backgroundMu.Unlock()
	a.cleanupAbandonedSessions()
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: a.Config.RuntimeDir, Revision: a.Revision, Capacity: int(fleet.Number(fleet.Object(a.Config.Values["intake"])["aiConcurrency"]))})
	if err != nil {
		a.cancelBackground()
		return err
	}
	a.setGate(gate)
	a.exclusiveStarted = true
	// Cancellation of the supplied owner context terminates the Engine independently of the business context.
	context.AfterFunc(ctx, func() {
		gate.BeginHandoff()
		_ = a.StopExclusive(context.Background())
		a.exclusiveCancel()
	})
	a.Consultations.BindEngine(a.Engine)
	for _, task := range pending {
		a.startBackgroundTask(task)
	}
	if err = a.startBackground(); err != nil {
		gate.BeginHandoff()
		a.cancelBackground()
		a.exclusiveErr = err
		return err
	}
	return nil
}

// StopExclusive fences new single-owner work, stops periodic tasks and waits for accepted turns.
func (a *App) StopExclusive(ctx context.Context) error {
	a.exclusiveMu.Lock()
	gate := a.currentGate()
	if gate != nil {
		gate.BeginHandoff()
	}
	a.cancelBackground()
	a.exclusiveMu.Unlock()
	a.wg.Wait()
	if a.Engine != nil {
		if err := a.Engine.Quiesce(ctx); err != nil {
			return err
		}
	}
	if gate != nil {
		return gate.WaitIdle(ctx)
	}
	return nil
}

func (a *App) Values() (map[string]any, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Config.Shared(a.ctx, a.Fleet)
}
func (a *App) CurrentConfig() *Config { a.mu.RLock(); defer a.mu.RUnlock(); return a.Config }
func (a *App) Repositories() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]string{}, a.Config.Repositories...)
}
func (a *App) Own(user map[string]any) bool {
	id, ok := accountID(user)
	return ok && a.ownIDs[id]
}
func (a *App) DedicatedAuthor(user map[string]any) bool {
	id, ok := accountID(user)
	return ok && id == a.intakeID
}
func (a *App) TrustedWriter(user map[string]any) bool {
	id, ok := accountID(user)
	return ok && a.writerIDs[id]
}
func (a *App) Publish(value map[string]any) {
	if a.Live != nil {
		a.Live.Publish(value)
	}
}
func (a *App) Project(repo string) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Config.Project(repo)
}
func (a *App) Link(repo, kind string, n int64) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return publicLink(a.Config.Values, repo, kind, n)
}
func (a *App) DeskNumber(repo string, n int64) any {
	v, ok, err := a.Numbers.Number(a.ctx, repo, n)
	if err != nil || !ok {
		return nil
	}
	return v
}
func (a *App) Home(repo string) string {
	_, assignments, err := a.Fleet.Load()
	if err != nil {
		return ""
	}
	return fleet.Text(fleet.Object(assignments.Output["repositories"])[repo])
}

func (a *App) Shutdown(ctx context.Context) {
	if a.Engine != nil {
		a.Engine.Shutdown(ctx)
	}
	a.Close()
}
func (a *App) Close() {
	_ = a.closeResources()
}

func (a *App) periodically(name string, interval time.Duration, run func(context.Context) error) {
	if name == "" {
		panic("periodic task name is required")
	}
	a.backgroundMu.Lock()
	a.periodicNames = append(a.periodicNames, name)
	a.backgroundMu.Unlock()
	a.startBackgroundTask(func(ctx context.Context) {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			started := time.Now()
			err := run(ctx)
			failed := err != nil && ctx.Err() == nil
			processactivity.Process.Periodic(name, time.Since(started), failed)
			if failed {
				a.Log("周期処理（" + name + "）を完了できません。保存済みの状態を保持して次回照合します。原因: " + contract.SafeCause(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	})
}

// startBackgroundTask serializes registration with stopping and waiting.
func (a *App) startBackgroundTask(run func(context.Context)) bool {
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	if a.backgroundStopped {
		return false
	}
	if a.deferBackground {
		a.backgroundPending = append(a.backgroundPending, run)
		return true
	}
	if a.backgroundCtx == nil {
		a.backgroundCtx, a.backgroundCancel = context.WithCancel(a.ctx)
	}
	ctx := a.backgroundCtx
	a.wg.Add(1)
	go func() { defer a.wg.Done(); run(ctx) }()
	return true
}

// StopBackground cancels periodic work and waits for its workers, including
// daily children. It leaves business services and their context alive.
// Registration stops before waiting, so callers can safely stop concurrently
// with task registration or normal shutdown.
func (a *App) StopBackground() {
	a.CancelBackground()
	a.wg.Wait()
}

// CancelBackground signals periodic workers without closing business services.
func (a *App) CancelBackground() {
	a.exclusiveMu.Lock()
	defer a.exclusiveMu.Unlock()
	a.cancelBackground()
}

func (a *App) cancelBackground() {
	a.backgroundMu.Lock()
	if !a.backgroundStopped {
		a.backgroundStopped = true
		if a.backgroundCancel != nil {
			a.backgroundCancel()
		}
	}
	a.backgroundMu.Unlock()
}
