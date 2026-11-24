package startup

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/usage"
	"os"
	"path/filepath"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/reviewnotes"
)

// Reuse the submission-recovery timer and its immediate startup call. The
// registered cycle remains in wg while it starts the single daily worker, so
// Close can cancel and wait for both without racing a zero-count WaitGroup.
func (a *App) reconcileDailyNotes(ctx context.Context) error {
	err := a.recoverSubmissions(ctx)
	if ctx.Err() == nil && a.reviewNotesRunning.CompareAndSwap(false, true) {
		if !a.startBackgroundTask(func(ctx context.Context) {
			defer a.reviewNotesRunning.Store(false)
			if err := a.updateReviewNotes(ctx); err != nil && ctx.Err() == nil {
				a.Log("注意点の更新を完了できません。保存済みの状態を保持します。原因: " + contract.SafeCause(err))
			}
		}) {
			a.reviewNotesRunning.Store(false)
		}
	}
	return err
}

func (a *App) updateReviewNotes(ctx context.Context) error {
	values, err := a.Values()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(fleet.Object(values["assignments"])["reviewNotes"])
	if err != nil {
		return err
	}
	if string(raw) == "null" {
		raw = nil
	}
	config, err := reviewnotes.ParseConfig(raw)
	if err != nil {
		return err
	}
	var failures []error
	for _, repo := range a.Repositories() {
		if err := ctx.Err(); err != nil {
			return err
		}
		service := reviewnotes.Service{DB: a.DB, Classify: reviewnotes.AIClassifier(func(ctx context.Context, input string) (map[string]any, error) {
			return a.answerReviewNotes(ctx, repo, input)
		})}
		if err := a.reviewNotesDates.run(repo, time.Now, func() (bool, error) { return service.Run(ctx, repo, config) }); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (a *App) answerReviewNotes(ctx context.Context, repo, input string) (map[string]any, error) {
	// Resolve current settings AFTER capacity/drain waits, so temporary provider
	// switches made while queued apply to the actual call as well.
	ctx, release, err := a.acquireGate(ctx, "ai")
	if err != nil {
		return nil, err
	}
	defer release()
	values, err := a.Values()
	if err != nil {
		return nil, err
	}
	settings, err := a.Models.Resolve(ctx, values, map[string]any{"consultationProvider": "codex"}, models.ReviewNotesStep)
	if err != nil {
		return nil, err
	}
	workspace := filepath.Join(a.Config.RuntimeDir, "intake-workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return nil, err
	}
	request := ai.Request{
		Project: repo, Step: usage.ReviewNotesStep, Attempt: 1, Turn: 1,
		Provider: settings.Provider, Model: settings.Model, Effort: settings.Effort,
		Input: input, SchemaFile: filepath.Join(a.Config.ApplicationDir, "server", "schemas", "review-notes.schema.json"),
		Workspace: workspace, Tools: "", DataOnly: true, Timeout: 5 * time.Minute, IdleTimeout: 240 * time.Second,
		MaxInputChars: int(fleet.Number(fleet.Object(values["intake"])["maxInputChars"])),
		MaxInputBytes: int(fleet.Number(fleet.Object(values["intake"])["maxInputBytes"])),
	}
	started := false
	request.BeforeCall = func() error { started = true; return ctx.Err() }
	answer, callErr := a.Runner.Call(ctx, request)
	if started {
		answer.Usage.FirstOutputMs = answer.Diagnostic.FirstOutputMs
		// The existing recorder saves failures and cancellations without losing
		// returned usage when the app's context has already been cancelled.
		if err := a.recordUsage(ctx, request, answer.Usage, answer.Elapsed, callErr == nil); err != nil {
			return nil, errors.Join(callErr, err)
		}
	}
	return answer.Value, callErr
}
