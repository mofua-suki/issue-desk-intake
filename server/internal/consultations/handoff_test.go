package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func handoffDraft(t *testing.T, s *Service, id string) Record {
	t.Helper()
	d, err := s.options.Records.Change(context.Background(), "drafts", id, func(*sql.Tx, Record) (Record, error) {
		return Record{"id": id, "project": "owner/app", "status": "thinking", "queued": true, "revision": 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestQuiesceWaitsAndRecoversQueued(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	first := handoffDraft(t, s, todoID)
	secondID := "00000000-0000-4000-8000-000000000002"
	second := handoffDraft(t, s, secondID)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(ctx context.Context, d Record) (Record, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return Record{"reply": "保存", "questions": []any{"確認事項"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err = e.Start(ctx, todoID, first, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err = e.Start(ctx, secondID, second, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	timed, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err = e.Quiesce(timed); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	if _, err = e.Start(ctx, "other", second, auth.Principal{}); err == nil {
		t.Fatal("start accepted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Quiesce(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	close(release)
	wg.Wait()
	d, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || text(d["status"]) != "draft" || len(fleet.Items(d["messages"])) != 1 {
		t.Fatalf("saved: %v %v", d, err)
	}
	queued, _, err := s.options.Records.Get(ctx, "drafts", secondID)
	if err != nil || text(queued["status"]) != "thinking" || calls.Load() != 1 {
		t.Fatalf("queued: %v %v calls=%d", queued, err, calls.Load())
	}
	next, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"reply": "復旧", "questions": []any{"確認事項"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err = next.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for next.Busy(secondID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	queued, _, err = s.options.Records.Get(ctx, "drafts", secondID)
	if err != nil || text(queued["status"]) != "draft" {
		t.Fatalf("recovery: %v %v", queued, err)
	}
}

func TestQuiesceContinuationAndSlotRelease(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := handoffDraft(t, s, todoID)
	released, resume := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e, _ := NewEngine(ctx, EngineOptions{Service: s, Turn: func(ctx context.Context, d Record) (Record, error) {
		calls.Add(1)
		err := WithoutAISlot(ctx, func(context.Context) error { close(released); <-resume; return nil })
		return Record{"_continue": true, "reply": "続き"}, err
	}})
	defer e.Close()
	if _, err := e.Start(ctx, todoID, d, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	<-released
	done := make(chan error, 1)
	go func() { done <- e.Quiesce(ctx) }()
	// Observe the fence before allowing the started turn to continue.
	<-e.quiesced
	select {
	case err := <-done:
		t.Fatalf("returned while slot released: %v", err)
	default:
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || text(saved["status"]) != "thinking" || text(saved["retryAt"]) == "" || calls.Load() != 1 {
		t.Fatalf("continuation: %v %v calls=%d", saved, err, calls.Load())
	}
}

func TestQuiesceStartRaceAndShutdown(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := handoffDraft(t, s, todoID)
	entered := make(chan struct{}, 1)
	canceled := make(chan struct{})
	e, _ := NewEngine(ctx, EngineOptions{Service: s, Turn: func(ctx context.Context, _ Record) (Record, error) {
		entered <- struct{}{}
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}})
	defer e.Close()
	if _, err := e.Start(ctx, todoID, d, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+100)
		pending := handoffDraft(t, s, id)
		go func() { defer wg.Done(); _, _ = e.Start(ctx, id, pending, auth.Principal{}) }()
	}
	qctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.Quiesce(qctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	wg.Wait()
	select {
	case <-canceled:
		t.Fatal("quiesce canceled turn")
	default:
	}
	shutdownCtx, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	e.Shutdown(shutdownCtx)
	select {
	case <-canceled:
	default:
		t.Fatal("shutdown did not cancel turn")
	}
}

func TestQuiesceWaitsForDatabaseSave(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := handoffDraft(t, s, todoID)
	entered, release := make(chan struct{}), make(chan struct{})
	e, _ := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		close(entered)
		<-release
		return Record{"reply": "保存", "questions": []any{"確認事項"}}, nil
	}})
	defer e.Close()
	if _, err := e.Start(ctx, todoID, d, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	tx, err := s.options.Records.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, bindSQL("UPDATE drafts SET json=json WHERE id=?"), todoID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Quiesce(ctx) }()
	<-e.quiesced
	close(release)
	select {
	case err := <-done:
		t.Fatalf("save still locked: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || text(saved["status"]) != "draft" || len(fleet.Items(saved["messages"])) != 1 {
		t.Fatalf("save: %v %v", saved, err)
	}
}

func TestQuiesceStopsRetry(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := handoffDraft(t, s, todoID)
	base := s.options.Now()
	var advance atomic.Bool
	s.options.Now = func() time.Time {
		if advance.Load() {
			return base.Add(2 * time.Minute)
		}
		return base
	}
	persisted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) { calls.Add(1); return nil, temporaryFailure{} }, Publish: func(update Record) {
		saved, _, err := s.options.Records.Get(ctx, "drafts", text(update["id"]))
		if err == nil && text(saved["retryAt"]) != "" {
			once.Do(func() { close(persisted); <-release })
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	if _, err := e.Start(ctx, todoID, d, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-persisted:
	case <-time.After(10 * time.Second):
		t.Fatal("retry was not saved")
	}
	done := make(chan error, 1)
	go func() { done <- e.Quiesce(ctx) }()
	<-e.quiesced
	// The run loop sees an already-due retry when the save finishes.
	advance.Store(true)
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.Busy(todoID) {
		if time.Now().After(deadline) {
			t.Fatal("quiesced retry did not terminate")
		}
		time.Sleep(time.Millisecond)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	retry, parseErr := time.Parse(time.RFC3339Nano, text(saved["retryAt"]))
	if err != nil || parseErr != nil || retry.After(s.options.Now()) || text(saved["status"]) != "thinking" || calls.Load() != 1 {
		t.Fatalf("retry: %v %v calls=%d", saved, err, calls.Load())
	}
}

func TestQuiesceWaitsForStartedAutoSubmit(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := handoffDraft(t, s, todoID)
	entered, releaseTurn := make(chan struct{}), make(chan struct{})
	submitting, releaseSubmit := make(chan struct{}), make(chan struct{})
	var turnOnce, submitOnce sync.Once
	e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(ctx context.Context, _ Record) (Record, error) {
		close(entered)
		select {
		case <-releaseTurn:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return Record{"title": "件名", "body": "本文", "ready": true, "questions": []any{}}, nil
	}, Submit: func(ctx context.Context, d Record) (Record, error) {
		close(submitting)
		select {
		case <-releaseSubmit:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return s.options.Records.Change(ctx, "drafts", text(d["id"]), func(_ *sql.Tx, current Record) (Record, error) {
			current["status"] = "submitted"
			current["issue"] = Record{"number": 42}
			return current, nil
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	defer turnOnce.Do(func() { close(releaseTurn) })
	defer submitOnce.Do(func() { close(releaseSubmit) })
	if _, err := e.Start(ctx, todoID, d, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	done := make(chan error, 1)
	go func() { done <- e.Quiesce(ctx) }()
	<-e.quiesced
	turnOnce.Do(func() { close(releaseTurn) })
	select {
	case <-submitting:
	case <-time.After(10 * time.Second):
		t.Fatal("started turn did not reach automatic Submit")
	}
	select {
	case err := <-done:
		t.Fatalf("quiesce returned before Submit saved: %v", err)
	default:
	}
	submitOnce.Do(func() { close(releaseSubmit) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || saved["status"] != "submitted" || record(saved["issue"])["number"] != json.Number("42") {
		t.Fatalf("submit not saved before quiesce: %v %v", saved, err)
	}
}
