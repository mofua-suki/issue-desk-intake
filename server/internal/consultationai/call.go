package consultationai

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"os"
	"path/filepath"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/models"
)

func wrapIdleTimeout(answer ai.Answer, err error) error {
	if err != nil && answer.Diagnostic.Kind == "timeout_idle" && !ai.IsIdleTimeout(err) {
		return &ai.IdleTimeoutError{Err: err}
	}
	return err
}

func (s *Service) wait(ctx context.Context, d Record, provider string, duration time.Duration) error {
	now := s.options.Now()
	until := s.rateWaits.extend(provider, now.Add(duration))
	duration = until.Sub(now)
	if err := s.save(ctx, d, Record{"rateLimitWait": Record{"provider": provider, "until": until.UTC().Format(time.RFC3339Nano)}}); err != nil {
		return err
	}
	if err := consultations.WithoutAISlot(ctx, func(ctx context.Context) error { return s.options.Wait(ctx, duration) }); err != nil {
		return err
	}
	return s.save(ctx, d, Record{"rateLimitWait": nil})
}
func (s *Service) call(ctx context.Context, d Record, config map[string]any, settings models.Settings, step, schema, input, full string, state *sessionState, budget *callBudget, images []string, sources map[string]bool) (ai.Answer, error) {
	return s.callDynamic(ctx, d, config, settings, step, schema, input, full, state, budget, images, sources)
}

// callDynamic also accepts the caller-provided Answer step.
func (s *Service) callDynamic(ctx context.Context, d Record, config map[string]any, settings models.Settings, step, schema, input, full string, state *sessionState, budget *callBudget, images []string, sources map[string]bool) (ai.Answer, error) {
	budget, err := s.callBudgetFor(ctx, d, config, budget)
	if err != nil {
		return ai.Answer{}, err
	}
	workspace := filepath.Join(s.options.RuntimeDir, "intake-workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return ai.Answer{}, err
	}
	trigger, _ := ctx.Value(callTriggerKey{}).(string)
	if step != usage.DraftStep {
		trigger = ""
	}
	if waited, err := s.resumeSavedWait(ctx, d); err != nil {
		return ai.Answer{}, err
	} else if waited {
		if step == usage.DraftStep {
			trigger = "rate_limit_retry"
		}
	}
	timeout := callTimeout(d, step, settings)
	timeoutRetried, rebuild := false, false
	missingRetryInFlight := false
	allowedHere, retrySucceeded := false, false
	defer func() {
		if allowedHere && !retrySucceeded {
			budget.allowedUnknown = 0
		}
	}()
	rateRetries := 0
	for {
		if err := s.waitForProvider(ctx, d, settings.Provider); err != nil {
			return ai.Answer{}, err
		}
		if err := s.checkCallAllowed(ctx, d, budget); err != nil {
			return ai.Answer{}, err
		}
		request, idErr := newCallRequest(ctx, d, config, settings, step, schema, input, workspace, images, timeout, trigger)
		if idErr != nil {
			return ai.Answer{}, idErr
		}
		if state != nil {
			if err := s.attachCallSession(ctx, d, &request, state, settings); err != nil {
				return ai.Answer{}, err
			}
		}
		if err := s.inputCheck(ctx, d, config, request, full); err != nil {
			return ai.Answer{}, err
		}
		reserved := false
		request.BeforeCall = func() error {
			if reserved {
				return nil
			}
			if budget != nil {
				if err := budget.reserveCall(ctx, request.CallID); err != nil {
					return err
				}
			}
			reserved = true
			s.observeSessionInput(d, state, request.Resume, request.Input, full)
			return nil
		}
		answer, err := s.options.Runner.Call(ctx, request)
		retryMissing, stopErr := s.accountCall(ctx, d, config, request, &answer, err, reserved, timeoutRetried, budget, state, sources)
		if retryMissing {
			allowedHere = true
		}
		if stopErr != nil {
			return answer, stopErr
		}
		if err == nil {
			if missingRetryInFlight {
				retrySucceeded = true
			}
			return answer, nil
		}
		if ctx.Err() != nil {
			return answer, ctx.Err()
		}
		if missingRetryInFlight {
			return answer, wrapIdleTimeout(answer, err)
		}
		if answer.Diagnostic.Kind == "timeout_idle" && !retryMissing {
			return answer, wrapIdleTimeout(answer, err)
		}
		var failure *ai.Failure
		if !retryMissing && errors.As(err, &failure) && failure.Kind == "rate_limit" && rateRetries < 3 {
			delays := []time.Duration{time.Minute, 3 * time.Minute, 10 * time.Minute}
			if waitErr := s.wait(ctx, d, settings.Provider, delays[rateRetries]+providerWaitSpread(d)); waitErr != nil {
				return answer, waitErr
			}
			rateRetries++
			if step == usage.DraftStep {
				trigger = "rate_limit_retry"
			}
			continue
		}
		var timed interface{ Timeout() bool }
		if !timeoutRetried && (retryMissing || errors.As(err, &timed) && timed.Timeout()) {
			timeoutRetried = true
			missingRetryInFlight = retryMissing
			if state != nil {
				state.ResetReason = "failure_retry"
				if dropErr := s.dropSession(ctx, d, state); dropErr != nil {
					return answer, dropErr
				}
			}
			if settings.Provider == "codex" && fleet.Text(fleet.Object(d["tier"])["level"]) == "heavy" {
				settings = s.standardRetrySettings(ctx, d, config, settings)
			}
			input = full
			if step == usage.DraftStep {
				trigger = "failure_retry"
			}
			continue
		}
		if request.Resume && !rebuild && errors.As(err, &failure) && failure.Kind == "execution" {
			rebuild = true
			if state != nil {
				state.ResetReason = "failure_retry"
				if dropErr := s.dropSession(ctx, d, state); dropErr != nil {
					return answer, dropErr
				}
			}
			input = full
			if step == usage.DraftStep {
				trigger = "failure_retry"
			}
			continue
		}
		return answer, err
	}
}

// callBudgetFor returns the budget of the call: the given one, the one of the running
// turn, or a new one read from the saved consultation.
func (s *Service) callBudgetFor(ctx context.Context, d Record, config map[string]any, budget *callBudget) (*callBudget, error) {
	if budget != nil {
		return budget, nil
	}
	if running, _ := ctx.Value(callBudgetKey{}).(*callBudget); running != nil {
		return running, nil
	}
	return s.budget(d, config)
}

// resumeSavedWait finishes the rate limit wait saved by an earlier attempt. It reports
// whether it waited.
func (s *Service) resumeSavedWait(ctx context.Context, d Record) (bool, error) {
	wait := fleet.Object(d["rateLimitWait"])
	if wait == nil {
		return false, nil
	}
	until, err := time.Parse(time.RFC3339Nano, fleet.Text(wait["until"]))
	if err != nil {
		return false, errors.New("保存された待機時刻が不正です")
	}
	delay := until.Sub(s.options.Now())
	if delay <= 0 {
		return false, nil
	}
	if err := s.wait(ctx, d, fleet.Text(wait["provider"]), delay); err != nil {
		return false, err
	}
	return true, nil
}

// accountCall records the usage of one call attempt and the session cost. stopErr ends the
// call with that error. retryMissing allows the attempt without a complete measurement to
// be retried once.
func (s *Service) accountCall(ctx context.Context, d Record, config map[string]any, request ai.Request, answer *ai.Answer, callErr error, reserved, timeoutRetried bool, budget *callBudget, state *sessionState, sources map[string]bool) (retryMissing bool, stopErr error) {
	if reserved {
		s.completeCallUsage(ctx, d, request, answer, sources)
	}
	// Accounting outlives the draft fence: a formal stop must not discard
	// measurements already returned by the terminated provider process.
	recordErr, budgetErr := s.recordCallUsage(ctx, request, *answer, callErr == nil, reserved, budget)
	if budgetErr != nil {
		return false, wrapIdleTimeout(*answer, budgetErr)
	}
	if recordErr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		holdErr := s.holdMeasurement(ctx, d, request, "record_failed", *answer, callErr, recordErr)
		return false, wrapIdleTimeout(*answer, holdErr)
	}
	if reserved {
		if _, complete := usageMeasurement(request, answer.Usage, config); !complete {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if callErr != nil && !timeoutRetried && s.timeoutMeasurementException(budget, *answer) {
				budget.timeoutExceptionUsed = true
				budget.allowedUnknown = 1
				retryMissing = true
			} else {
				reason := "missing"
				if answer.Usage.Partial != nil && *answer.Usage.Partial {
					reason = "partial"
				}
				holdErr := s.holdMeasurement(ctx, d, request, reason, *answer, callErr, nil)
				return false, wrapIdleTimeout(*answer, holdErr)
			}
		}
	}
	if state != nil && answer.Usage.SessionCostUSD != nil {
		if saveErr := s.rememberSessionCost(ctx, d, state, answer.Usage.SessionCostUSD); saveErr != nil {
			// 再試行の枠を立てた後の失敗でも呼び出し側の後始末で枠を戻せるよう、立てた値を返す。
			return retryMissing, saveErr
		}
	}
	return retryMissing, nil
}

// callTimeout gives retried calls and heavy draft calls the longer limit.
func callTimeout(d Record, step string, settings models.Settings) time.Duration {
	timeout := 5 * time.Minute
	if fleet.Number(d["attempts"]) > 0 || (step == usage.DraftStep && (settings.Tier == "heavy" || settings.Effort == "high")) {
		timeout = 10 * time.Minute
	}
	return timeout
}

// newCallRequest builds the provider request of one call attempt with a new call ID.
func newCallRequest(ctx context.Context, d Record, config map[string]any, settings models.Settings, step, schema, input, workspace string, images []string, timeout time.Duration, trigger string) (ai.Request, error) {
	request := ai.Request{Draft: fleet.Text(d["id"]), Project: fleet.Text(d["project"]), Step: step, Attempt: int(fleet.Number(d["attempts"])) + 1, Turn: int(fleet.Number(d["revision"])) + 1, Tier: settings.Tier, Provider: settings.Provider, Model: settings.Model, Effort: settings.Effort, Input: input, SchemaFile: schema, Workspace: workspace, Images: images, Timeout: timeout, IdleTimeout: 240 * time.Second, OutputTimeout: 240 * time.Second}
	callID, err := newID()
	if err != nil {
		return ai.Request{}, err
	}
	request.CallID = callID
	request.CallTrigger = trigger
	if trigger == "format_repair" {
		detail, _ := ctx.Value(repairDetailKey{}).(repairDetail)
		request.RepairKind, request.RepairField = detail.kind, detail.field
	}
	request.MaxInputChars = int(fleet.Number(fleet.Object(config["intake"])["maxInputChars"]))
	request.MaxInputBytes = int(fleet.Number(fleet.Object(config["intake"])["maxInputBytes"]))
	if settings.Provider == "claude" && len(images) > 0 {
		request.Tools = "Read"
	}
	return request, nil
}

// attachCallSession resumes the saved session in the request, starts a Claude session
// when none is saved, and keeps the session ID the provider reports.
func (s *Service) attachCallSession(ctx context.Context, d Record, request *ai.Request, state *sessionState, settings models.Settings) error {
	prepareSessionRequest(request, state)
	if settings.Provider == "claude" && state.ID == "" {
		id, err := newID()
		if err != nil {
			return err
		}
		if err := s.rememberSession(ctx, d, state, id); err != nil {
			return err
		}
		request.Session = id
		request.Resume = false
	}
	request.OnSession = func(id string) error {
		if state.ID != "" && state.ID != id {
			if err := s.dropSession(ctx, d, state); err != nil {
				return err
			}
		}
		return s.rememberSession(ctx, d, state, id)
	}
	return nil
}

// completeCallUsage adds the diagnostic timing and input measurement to the usage of a
// reserved call and keeps the diagnostic for the draft.
func (s *Service) completeCallUsage(ctx context.Context, d Record, request ai.Request, answer *ai.Answer, sources map[string]bool) {
	answer.Usage.FirstOutputMs = answer.Diagnostic.FirstOutputMs
	answer.Usage.DiagnosticKind = ""
	if usage.ValidDiagnosticKind(answer.Diagnostic.Kind) {
		answer.Usage.DiagnosticKind = answer.Diagnostic.Kind
	}
	measureInput(request, &answer.Usage, sources)
	s.rememberDiagnostic(ctx, d, request, *answer)
}

// checkCallAllowed refuses a call when the context ended, an earlier measurement is
// unresolved or the budget is spent.
func (s *Service) checkCallAllowed(ctx context.Context, d Record, budget *callBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.reconcileMeasurement(ctx, d, budget); err != nil {
		return err
	}
	if err := ensureMeasurement(d, budget); err != nil {
		return err
	}
	if budget != nil {
		if err := budget.check(); err != nil {
			return err
		}
	}
	return nil
}

// recordCallUsage records the usage of a reserved call and charges it to the budget.
func (s *Service) recordCallUsage(ctx context.Context, request ai.Request, answer ai.Answer, succeeded, reserved bool, budget *callBudget) (recordErr, budgetErr error) {
	if reserved && s.options.RecordUsage != nil {
		recordErr = s.options.RecordUsage(ctx, request, answer.Usage, answer.Elapsed, succeeded)
	}
	if reserved && budget != nil && recordErr == nil {
		budgetErr = budget.measured(ctx, request, answer.Usage)
	}
	return recordErr, budgetErr
}

// timeoutMeasurementException reports whether a timed-out call without a complete
// measurement may be retried once instead of holding the consultation.
func (s *Service) timeoutMeasurementException(budget *callBudget, answer ai.Answer) bool {
	return !budget.timeoutExceptionUsed && budget.allowedUnknown == 0 && fleet.Number(budget.value["unknownCalls"]) == 1 && budget.value["pendingCall"] == nil && s.options.RecordUsage != nil && (answer.Diagnostic.Kind == "timeout_idle" || answer.Diagnostic.Kind == "timeout_total")
}

// rememberSessionCost keeps the session's cumulative cost for the next resumed call.
func (s *Service) rememberSessionCost(ctx context.Context, d Record, state *sessionState, cost *float64) error {
	state.LastCost = cost
	if state.ID != "" {
		return s.rememberSession(ctx, d, state, state.ID)
	}
	return nil
}

// standardRetrySettings returns the standard-tier settings for a heavy Codex retry when
// they keep the same provider, and the current settings otherwise.
func (s *Service) standardRetrySettings(ctx context.Context, d Record, config map[string]any, settings models.Settings) models.Settings {
	standard := copyRecord(d)
	standard["tier"] = Record{"level": "standard"}
	if candidate, resolveErr := s.resolve(ctx, config, map[string]any(standard), "intake"); resolveErr == nil && candidate.Provider == settings.Provider {
		return candidate
	}
	return settings
}
