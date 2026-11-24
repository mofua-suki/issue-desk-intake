package startup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/runtimegate"
)

func TestHandoffFenceBeforeBackground(t *testing.T) {
	app, h, _ := handoffMinimal(t)
	entered, proceed, background := make(chan struct{}), make(chan struct{}), make(chan struct{})
	original := h.stage1[0].run
	h.stage1[0].run = func(ctx context.Context) error { close(entered); <-proceed; return original(ctx) }
	app.startBackgroundTask(func(ctx context.Context) { <-ctx.Done(); close(background) })
	done := make(chan error, 1)
	go func() { done <- h.Run(context.Background()) }()
	handoffAwait(t, entered)
	select {
	case <-background:
		t.Fatal("開始抑止前に停止した")
	default:
	}
	close(proceed)
	handoffAwait(t, background)
	if _, release, err := app.Gate.Acquire(context.Background(), "project"); err == nil {
		release()
		t.Fatal("背景停止時に開始抑止されていない")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Err changes immediately after the step completes while Done remains disabled,
// forcing the coordinator to observe the result before checking the deadline.
type completedDeadline struct{ expired atomic.Bool }

func (*completedDeadline) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*completedDeadline) Done() <-chan struct{}       { return nil }
func (c *completedDeadline) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return nil
}
func (*completedDeadline) Value(any) any { return nil }

func TestHandoffCompletedStepExcludedFromPending(t *testing.T) {
	ctx := &completedDeadline{}
	h := &Handoff{}
	failure := h.wait(ctx, "stage1", []handoffStep{{"finished", func(context.Context) error { ctx.expired.Store(true); return nil }}})
	if failure == nil || !errors.Is(failure, context.DeadlineExceeded) || len(failure.Pending) != 0 {
		t.Fatalf("完了済み処理が未完了に残った: %+v", failure)
	}
}

func TestHandoffAfterNormalCloseSharesResult(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			dir := t.TempDir()
			gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: dir, Capacity: 1})
			if err != nil {
				t.Fatal(err)
			}
			app := &App{ctx: ctx, cancel: cancel, Gate: gate}
			var releases atomic.Int32
			h, err := NewHandoff(app, handoffLeaseFunc(func() { releases.Add(1) }))
			if err != nil {
				t.Fatal(err)
			}
			if err := gate.Tick(); err != nil {
				t.Fatal(err)
			}
			if fail {
				if err := os.RemoveAll(filepath.Join(dir, "state")); err != nil {
					t.Fatal(err)
				}
			}
			app.Close()
			if !app.exclusiveClosed {
				t.Fatal("通常停止の処理が実行されない")
			}
			err = h.Run(context.Background())
			var failure *HandoffError
			if fail {
				if !errors.As(err, &failure) || failure.Operation != "resources" || releases.Load() != 0 {
					t.Fatalf("解放失敗が引き継がれない: %v", err)
				}
			} else if err != nil || releases.Load() != 1 {
				t.Fatalf("通常停止後の交代: %v", err)
			}
		})
	}
}

func TestHandoffRunsCommonShutdown(t *testing.T) {
	app, h, releases := handoffMinimal(t)
	if err := h.Run(context.Background()); err != nil || releases.Load() != 1 || !app.exclusiveClosed {
		t.Fatalf("共通終了処理が実行されない: %v", err)
	}
}
