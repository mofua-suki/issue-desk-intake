package consultations

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"log/slog"
	"sync"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

type EngineOptions struct {
	Stalls      *ErrorStalls
	Report      func(error)
	Service     *Service
	Turn        func(context.Context, Record) (Record, error)
	Submit      func(context.Context, Record) (Record, error)
	Principal   func(context.Context, string) (auth.Principal, error)
	Publish     func(Record)
	Concurrency int
}

type Engine struct {
	receipts  receiptEngine
	options   EngineOptions
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	controlMu sync.Mutex
	active    map[string]context.CancelFunc
	filings   map[string]bool
	closing   bool
	quiescing bool
	quiesced  chan struct{}
	drained   chan struct{}
	turns     int
	slots     chan struct{}
	wg        sync.WaitGroup
}

func NewEngine(ctx context.Context, options EngineOptions) (*Engine, error) {
	if options.Service == nil || options.Turn == nil {
		return nil, errors.New("相談の保存先と実行処理が必要です")
	}
	if options.Concurrency < 1 {
		options.Concurrency = 1
	}
	if options.Concurrency > 8 {
		return nil, errors.New("相談の同時実行数が上限を超えています")
	}
	instance, err := newID()
	if err != nil {
		return nil, err
	}
	lifecycle, cancel := context.WithCancel(ctx)
	return &Engine{receipts: receiptEngine{instance: instance}, options: options, ctx: lifecycle, cancel: cancel, active: map[string]context.CancelFunc{}, filings: map[string]bool{}, slots: make(chan struct{}, options.Concurrency), quiesced: make(chan struct{})}, nil
}
func (e *Engine) Start(ctx context.Context, id string, d Record, p auth.Principal) (Record, error) {
	if receipt := record(d["receipt"]); receipt != nil {
		return e.accept(ctx, receipt, p)
	}
	if text(d["status"]) != "thinking" {
		return d, nil
	}
	if ctx.Value(receiptClaimKey{}) == nil {
		r, err := e.draftReceipt(ctx, d)
		if err != nil {
			return nil, err
		}
		if r != nil {
			return e.accept(ctx, r, p)
		}
	}
	e.controlMu.Lock()
	defer e.controlMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.receiptStoppingLocked() {
		return d, refuse(503, "相談の実行処理を停止中です。保存済みの依頼は次回起動時に再開します。")
	}
	if _, running := e.active[id]; running {
		return d, nil
	}
	lifecycle, cancel := context.WithCancel(e.ctx)
	e.active[id] = cancel
	e.wg.Add(1)
	if claim := ctx.Value(receiptClaimKey{}); claim != nil {
		lifecycle = context.WithValue(lifecycle, receiptClaimKey{}, claim)
	}
	go e.run(lifecycle, id)
	return d, nil
}
func (e *Engine) Busy(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.active[id]
	return ok
}
func (e *Engine) Cancel(id string) {
	e.mu.Lock()
	cancel := e.active[id]
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (e *Engine) Close() { e.mu.Lock(); e.closing = true; e.cancel(); e.mu.Unlock(); e.wg.Wait() }
func (e *Engine) emit(d Record) {
	if e.options.Publish != nil {
		e.options.Publish(Record{"type": "draft", "id": d["id"], "revision": d["revision"]})
	}
}
func (e *Engine) principal(ctx context.Context, d Record) (auth.Principal, error) {
	if text(d["owner"]) == "" {
		return auth.Principal{Kind: auth.Screen}, nil
	}
	if e.options.Principal == nil {
		return auth.Principal{}, refuse(401, "送り手のトークンを確認できません。")
	}
	return e.options.Principal(ctx, text(d["owner"]))
}
func (e *Engine) run(ctx context.Context, id string) {
	defer e.wg.Done()
	defer func() { e.mu.Lock(); delete(e.active, id); e.mu.Unlock() }()
	first, found, readErr := e.options.Service.options.Records.Get(ctx, "drafts", id)
	if readErr != nil || !found {
		e.reportStop(ctx, id, first, "相談の読取", readErr)
		return
	}
	if at, err := time.Parse(time.RFC3339Nano, text(first["retryAt"])); err == nil {
		delay := at.Sub(e.options.Service.options.Now())
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-e.quiesced:
				timer.Stop()
				return
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
	}
	for ctx.Err() == nil {
		e.attempt(ctx, id)
		if claim, _ := ctx.Value(receiptClaimKey{}).(*receiptClaim); claim != nil && claim.lost.Load() {
			return
		}
		if ctx.Err() != nil {
			return
		}
		draft, found, err := e.options.Service.options.Records.Get(ctx, "drafts", id)
		if err != nil || !found || text(draft["status"]) != "thinking" || text(draft["retryAt"]) == "" {
			e.reportStop(ctx, id, draft, "再試行の確認", err)
			return
		}
		retryAt, err := time.Parse(time.RFC3339Nano, text(draft["retryAt"]))
		if err != nil {
			e.reportStop(ctx, id, draft, "再試行の時刻の確認", err)
			return
		}
		delay := retryAt.Sub(e.options.Service.options.Now())
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-e.quiesced:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}
func (e *Engine) attempt(ctx context.Context, id string) {
	slot := &turnSlot{slots: e.slots}
	select {
	case e.slots <- struct{}{}:
		slot.held = true
		defer slot.release()
	case <-ctx.Done():
		return
	case <-e.quiesced:
		return
	}
	if !e.beginReceiptWork() {
		return
	}
	defer e.endReceiptWork()
	service := e.options.Service
	runID, err := newID()
	if err != nil {
		e.reportStop(ctx, id, nil, "実行IDの生成", err)
		return
	}
	initial, _, err := service.options.Records.Get(ctx, "drafts", id)
	if err != nil || initial == nil {
		e.reportStop(ctx, id, initial, "相談の読取", err)
		return
	}
	principal, err := e.principal(ctx, initial)
	if err != nil {
		e.noteStop(ctx, id, initial, "送り手の確認", err)
		return
	}
	d, err := e.changeClaimedDraft(ctx, id, false, func(tx *sql.Tx, d Record) (Record, error) {
		if d == nil || text(d["status"]) != "thinking" {
			return d, nil
		}
		if text(d["owner"]) != text(initial["owner"]) {
			return nil, refuse(409, "相談の送り手が変わっています。")
		}
		if err = service.draftAccess(d, principal); err != nil {
			return nil, err
		}
		return claimForRun(d, service.options.Now(), runID), nil
	})
	if err != nil {
		if !errors.Is(err, errReceiptChanged) {
			e.noteStop(ctx, id, initial, "実行権の取得", err)
		}
		return
	}
	if d == nil || text(d["run"]) != runID {
		return
	}
	e.emit(d)
	waitStarted, _ := time.Parse(time.RFC3339Nano, text(d["waitStartedAt"]))
	remaining := waitStarted.Add(24 * time.Hour).Sub(service.options.Now())
	turnCtx, cancel := context.WithTimeout(ctx, remaining)
	turnCtx = context.WithValue(turnCtx, slotKey{}, slot)
	result, turnErr := e.options.Turn(turnCtx, d)
	expired := turnCtx.Err() != nil && ctx.Err() == nil
	var turnProblem *contract.Problem
	draining := !expired && errors.As(turnErr, &turnProblem) && turnProblem.Status == 503 && turnProblem.Code == "SERVER_DRAINING"
	cancel()
	if ctx.Err() != nil {
		return
	} // Persistent thinking is recovered only by the next leader.
	final, err := e.changeClaimedDraft(ctx, id, true, func(tx *sql.Tx, current Record) (Record, error) {
		if current == nil || text(current["status"]) != "thinking" || text(current["run"]) != runID {
			return nil, errReceiptChanged
		}
		return service.turnOutcome(current, result, turnErr, expired, draining)
	})
	if turnErr != nil && !draining {
		e.report(failureLogLine(id, d, turnErr))
	}
	if errors.Is(err, errReceiptChanged) {
		return
	}
	if err != nil {
		e.report(failureLogLine(id, d, AtStage("結果の保存", err)))
		return
	}
	if final == nil {
		return
	}
	if e.options.Stalls != nil && !draining {
		var failure error
		if turnErr != nil {
			failure = e.options.Stalls.Failed(ctx, final, turnErr)
		} else {
			failure = e.options.Stalls.Succeeded(ctx, final)
		}
		if failure != nil {
			e.report(failureLogLine(id, final, AtStage("失敗の集計の保存", failure)))
		}
	}
	e.emit(final)
	if text(final["status"]) == "auto_ready" && e.options.Submit != nil {
		submitted, err := e.options.Submit(ctx, final)
		if err != nil {
			e.reportStop(ctx, id, final, "自動の起票", err)
			e.SaveAutoSubmitFailure(ctx, id, final, err)
		} else if submitted != nil {
			e.emit(submitted)
		}
	}
}
func safeTurnError(d Record, err error) string {
	return PublicFailure(d, err, "相談の回答を確認できませんでした。")
}

// report はログへ1行残す。Report が無いときも捨てない。
func (e *Engine) report(line string) {
	if e.options.Report != nil {
		e.options.Report(errors.New(line))
		return
	}
	slog.Error(line)
}

// reportStop は処理を打ち切る原因をログへ残す。停止中の取り消しと、失敗ではない打ち切り（err が nil）は残さない。
func (e *Engine) reportStop(ctx context.Context, id string, d Record, stage string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	e.report(failureLogLine(id, d, AtStage(stage, err)))
}

// noteStop は相談が thinking のまま進めなくなった原因を、状態を変えずに lastFailure へ残す。
func (e *Engine) noteStop(ctx context.Context, id string, d Record, stage string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	cause := AtStage(stage, err)
	e.report(failureLogLine(id, d, cause))
	_, saveErr := e.changeClaimedDraft(ctx, id, false, func(_ *sql.Tx, current Record) (Record, error) {
		if current == nil || text(current["status"]) != "thinking" {
			return current, nil
		}
		next := clone(current)
		next["lastFailure"] = FailureRecord(timestamp(e.options.Service.options.Now()), current, cause, safeTurnError(current, cause))
		return next, nil
	})
	e.reportStop(ctx, id, d, "失敗の記録", saveErr)
}

// SaveAutoSubmitFailure は未送信の自動起票案に起票失敗を保存する。
func (e *Engine) SaveAutoSubmitFailure(ctx context.Context, id string, expected Record, failure error) {
	var problem *contract.Problem
	if ctx.Err() != nil || !errors.As(failure, &problem) || problem.Status < 400 || problem.Status >= 500 || problem.Status == 408 || problem.Status == 429 {
		return
	}
	service := e.options.Service
	expectedJSON, err := canonicalJSON(expected)
	if err != nil {
		e.report(failureLogLine(id, expected, AtStage("起票失敗の保存", err)))
		return
	}
	changed := false
	saved, err := service.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, current Record) (Record, error) {
		if current == nil || text(current["status"]) != "auto_ready" || current["attempted"] == true || current["issue"] != nil || has(current, "issues") || current["preparedJob"] != nil {
			return current, nil
		}
		actualJSON, err := canonicalJSON(current)
		if err != nil {
			return nil, err
		}
		if actualJSON != expectedJSON {
			return current, nil
		}
		next := clone(current)
		now := timestamp(service.options.Now())
		cause := AtStage("自動の起票", failure)
		message := problem.Message
		SetDraftError(next, message, problem.Code)
		revision, _ := integer(current["revision"])
		next["status"], next["lastFailure"], next["updatedAt"], next["revision"] = "submit_error", FailureRecord(now, current, cause, message), now, revision+1
		changed = true
		return next, nil
	})
	if err != nil {
		e.report(failureLogLine(id, expected, AtStage("起票失敗の保存", err)))
		return
	}
	if changed {
		e.emit(saved)
	}
}

// claimForRun moves a waiting draft to the new run, or stops it when the 24-hour
// response window has already passed.
func claimForRun(d Record, now time.Time, runID string) Record {
	next := clone(d)
	start, err := time.Parse(time.RFC3339Nano, text(d["waitStartedAt"]))
	if err != nil {
		start, err = time.Parse(time.RFC3339Nano, text(d["updatedAt"]))
		if err != nil {
			start = now
		}
		next["waitStartedAt"], next["waitSource"] = timestamp(start), "legacy"
	}
	if !now.Before(start.Add(24 * time.Hour)) {
		next["status"], next["ready"], next["run"], next["queued"], next["retryAt"] = "stalled", false, nil, false, nil
		next["stallCause"] = "timeout"
		SetDraftError(next, "24時間以内にAIの応答を返せなかったため、自動処理を停止しました。", nil)
		return next
	}
	next["queued"], next["run"], next["updatedAt"], next["retryAt"] = false, runID, timestamp(now), nil
	return next
}

// turnOutcome builds the draft state saved after the AI turn ends.
func (s *Service) turnOutcome(current, result Record, turnErr error, expired, draining bool) (Record, error) {
	next := clone(current)
	revision, _ := integer(current["revision"])
	next["revision"], next["updatedAt"], next["run"], next["queued"] = revision+1, timestamp(s.options.Now()), nil, false
	if expired {
		return s.expiredTurnOutcome(next, current, turnErr), nil
	}
	if turnErr != nil {
		return s.failedTurnOutcome(next, current, turnErr, draining), nil
	}
	return s.answeredTurnOutcome(next, current, result)
}

// expiredTurnOutcome stops a turn that exceeded the 24-hour response window.
func (s *Service) expiredTurnOutcome(next, current Record, turnErr error) Record {
	next["status"], next["ready"], next["retryAt"] = "stalled", false, nil
	next["stallCause"] = "timeout"
	SetDraftError(next, "24時間以内にAIの応答を返せなかったため、自動処理を停止しました。", nil)
	if turnErr != nil {
		next["lastFailure"] = FailureRecord(timestamp(s.options.Now()), current, turnErr, safeTurnError(current, turnErr))
	}
	return next
}

// failedTurnOutcome decides whether a failed turn is retried later or stopped as an error.
func (s *Service) failedTurnOutcome(next, current Record, turnErr error, draining bool) Record {
	if draining {
		// Also retry a temporary startup rejection if no handoff follows.
		next["status"], next["ready"], next["queued"], next["retryAt"] = "thinking", false, true, timestamp(s.options.Now().Add(time.Minute))
		return next
	}
	if ai.IsIdleTimeout(turnErr) {
		next["lastFailure"] = FailureRecord(timestamp(s.options.Now()), current, turnErr, safeTurnError(current, turnErr))
		next["status"], next["ready"], next["retryAt"], next["failureRetry"] = "error", false, nil, false
		SetDraftError(next, safeTurnError(current, turnErr), nil)
		return next
	}
	attempts, _ := integer(current["attempts"])
	attempts++
	next["attempts"], next["lastFailure"] = attempts, FailureRecord(timestamp(s.options.Now()), current, turnErr, safeTurnError(current, turnErr))
	timeoutStreak, _ := integer(current["timeoutStreak"])
	var timeout interface{ Timeout() bool }
	if errors.As(turnErr, &timeout) && timeout.Timeout() {
		timeoutStreak++
	} else {
		timeoutStreak = 0
	}
	next["timeoutStreak"] = timeoutStreak
	var temporary interface{ Transient() bool }
	if errors.As(turnErr, &temporary) && temporary.Transient() && timeoutStreak < 3 {
		delay := time.Minute
		for n := int64(1); n < attempts && delay < 15*time.Minute; n++ {
			delay *= 2
		}
		if delay > 15*time.Minute {
			delay = 15 * time.Minute
		}
		next["status"], next["ready"], next["queued"], next["retryAt"], next["failureRetry"] = "thinking", false, true, timestamp(s.options.Now().Add(delay)), true
		SetDraftError(next, nil, nil)
	} else {
		next["status"], next["ready"] = "error", false
		SetDraftError(next, safeTurnError(current, turnErr), nil)
	}
	return next
}

// answeredTurnOutcome copies the AI answer into the draft and appends its reply.
func (s *Service) answeredTurnOutcome(next, current, result Record) (Record, error) {
	for _, key := range []string{"executionPlan", "title", "body", "questions", "questionReasons", "ready", "splitIssues", "crossProject", "changeScope", "dependsOnIssues", "kind", "escalate", "escalateReason", "newProject", "heavyReason", "workType", "estimate", "basisConflicts", "readFiles", "reviewedFiles", "imageReviews", "inputBudget", "consultationBudget", "tier", "carriedContext", "carriedRounds", "tierRaised", "context", "progress", "consultationParts", "consultationChildren", "projectHandoff", "crossProposal", "crossApprovals", "completionPlan", "completionApproval", "completionProposal", "completionReconfirmation", "completionReconfirmationAfter", "completionComparison"} {
		if value, exists := result[key]; exists {
			if key == "splitIssues" && !ApplySplitAnswer(current[key], value, result) {
				continue
			}
			next[key] = value
		}
	}
	next["status"], next["waitStartedAt"] = "draft", nil
	SetDraftError(next, nil, nil)
	if reply := text(result["reply"]); reply != "" && result["_skipReply"] != true {
		messageID, err := newID()
		if err != nil {
			return nil, err
		}
		message := Record{"id": messageID, "role": "assistant", "text": reply, "at": timestamp(s.options.Now())}
		if fingerprint := text(record(result["crossProposal"])["fingerprint"]); fingerprint != "" {
			message["crossProposal"] = fingerprint
		}
		SaveCompletionPresentation(message, result)
		next["messages"] = append(fleet.Items(current["messages"]), message)
	}
	// The AI turn clears initialDocumentsCollecting and persists the initial documents before returning _continue.
	if result["_continue"] == true {
		at := timestamp(s.options.Now())
		next["status"], next["ready"], next["retryAt"], next["waitStartedAt"], next["waitSource"] = "thinking", false, at, at, "proxy"
		carried := next["context"]
		if sources, ok := record(next["progress"])["initialDocuments"]; ok && sources != nil {
			copy := Record{}
			for key, value := range record(carried) {
				copy[key] = value
			}
			copy["initialDocuments"] = sources
			// Preserve full read requests and versions, not just the display summary.
			copy["projects"] = record(next["progress"])["projects"]
			carried = copy
		}
		next["carriedContext"], next["progress"], next["carriedRounds"], next["tierRaised"] = carried, nil, 0, false
	}
	if result["status"] == "project_handoff" {
		next["status"] = "project_handoff"
	}
	stopQuestionlessOutcome(next, result)
	if next["ready"] == true && !has(next, "questions") && next["hold"] != true && next["unassigned"] != true && draftRefusal(next, "submit", false) == "" {
		next["status"] = "auto_ready"
	}
	return next, nil
}
