package issueapi

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"sync"
	"testing"
	"time"
)

// observedWaitContext signals when WaitAccepted has reached its select.
type observedWaitContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestRecoveryStopKeepsAcceptedCloseAndWaits(t *testing.T) {
	f := newFixture(t)
	f.api.issue["state"] = "closed"
	f.api.issue["closed_at"] = "2026-11-01T00:00:00Z"
	record := Object{"id": "11111111-1111-4111-8111-111111111111", "status": "closed", "project": "owner/repo", "number": 1, "notePending": true}
	f.api.comments = []Object{{"id": float64(1), "user": Object{"login": "intake"}, "body": closeNoteMarker(record)}}
	if err := f.s.saveClose(f.ctx, record); err != nil {
		t.Fatal(err)
	}
	blocked := &blockedCloseRead{API: f.api, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.o.Gitea = blocked
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- f.s.RunRecovery(ctx) }()
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("復旧処理が始まりません")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("復旧ループが止まりません")
	}
	waited := make(chan struct{})
	go func() { _ = f.s.WaitAccepted(context.Background()); close(waited) }()
	select {
	case <-waited:
		t.Fatal("処理完了前に待機が終わりました")
	case <-time.After(50 * time.Millisecond):
	}
	waitCtx, cancelActiveWait := context.WithCancel(context.Background())
	observed := &observedWaitContext{Context: waitCtx, entered: make(chan struct{})}
	activeWait := make(chan error, 1)
	go func() { activeWait <- f.s.WaitAccepted(observed) }()
	select {
	case <-observed.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("待機が始まりません")
	}
	cancelActiveWait()
	select {
	case err := <-activeWait:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("待機開始後のキャンセル: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("キャンセル後に待機が終わりません")
	}
	select {
	case <-waited:
		t.Fatal("待機のキャンセルで受理済み処理が終了しました")
	default:
	}
	canceled, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if err := f.s.WaitAccepted(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("キャンセル済みの待機: %v", err)
	}
	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	if err := f.s.WaitAccepted(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期限切れの待機: %v", err)
	}
	if f.s.ctx.Err() != nil {
		t.Fatal("Service が閉じられました")
	}
	close(blocked.release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("受理済み処理が完了しません")
	}
	saved, err := f.s.read(f.ctx, "close_requests", fleet.Text(record["id"]))
	if err != nil || saved["notePending"] != false {
		t.Fatalf("処理結果が保存されていません: %v %v", saved, err)
	}
}

func TestRunCancellationClosesServiceAndWaits(t *testing.T) {
	f := newFixture(t)
	record := Object{"id": "11111111-1111-4111-8111-111111111111", "status": "closed", "project": "owner/repo", "number": 1, "notePending": true}
	if err := f.s.saveClose(f.ctx, record); err != nil {
		t.Fatal(err)
	}
	blocked := &blockedCloseRead{API: f.api, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.o.Gitea = blocked
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx) }()
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("復旧処理が始まりません")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("通常停止が終わりません")
	}
	if f.s.ctx.Err() == nil {
		t.Fatal("通常停止で Service が閉じられていません")
	}
	if err := f.s.WaitAccepted(context.Background()); err != nil {
		t.Fatal(err)
	}
}
