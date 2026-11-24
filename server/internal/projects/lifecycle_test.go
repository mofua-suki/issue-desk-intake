package projects

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"sync/atomic"
	"testing"
	"time"
)

func lifecycleReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for lifecycle event")
		var zero T
		return zero
	}
}

func TestDrainOrdersFenceRecoveryAndAcceptedWork(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	finish := make(chan struct{})
	f.options.Answer = func(context.Context, Object) (Object, error) {
		close(started)
		<-finish
		return nil, nil
	}
	f.restart()
	_, _ = f.call("/api/projects", Object{"id": id, "text": "作成"})
	lifecycleReceive(t, started)
	fenced := make(chan struct{})
	stopping := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.s.Drain(context.Background(), func() { close(fenced) }, func() {
			<-fenced
			close(stopping)
			<-stopped
		})
	}()
	lifecycleReceive(t, stopping)
	select {
	case err := <-done:
		t.Fatalf("drain returned before recovery stopped: %v", err)
	default:
	}
	close(stopped)
	select {
	case err := <-done:
		t.Fatalf("drain returned before accepted work ended: %v", err)
	default:
	}
	close(finish)
	if err := lifecycleReceive(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryStopAndWaitAcceptedAcrossCreationAndFiling(t *testing.T) {
	f := newFixture(t)
	registered := make(chan struct{})
	continueCreation := make(chan struct{})
	submitted := make(chan context.Context, 1)
	continueSubmit := make(chan struct{})
	releasing := make(chan struct{})
	continueRelease := make(chan struct{})
	var available atomic.Bool
	var releases atomic.Int32
	f.options.Repositories = func() []string {
		if available.Load() {
			return []string{"owner/demo"}
		}
		return nil
	}
	f.options.Registered = func(ctx context.Context, _ Object) error {
		close(registered)
		select {
		case <-continueCreation:
			available.Store(true)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.options.Submit = func(ctx context.Context, _ Object) ([]Object, error) {
		submitted <- ctx
		select {
		case <-continueSubmit:
			return []Object{{"number": 42}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.options.Acquire = func(ctx context.Context, _ string) (context.Context, func(), error) {
		return ctx, func() {
			if releases.Add(1) == 2 {
				close(releasing)
				<-continueRelease
			}
		}, nil
	}
	f.restart()
	_, _ = f.call("/api/projects", Object{"id": id, "text": "作成"})
	p := f.wait("ready")
	if code, _ := f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]}); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	lifecycleReceive(t, registered)
	loopCtx, stopLoop := context.WithCancel(context.Background())
	loopDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() { loopDone <- f.s.runRecovery(loopCtx, false, func() { close(ready) }) }()
	lifecycleReceive(t, ready)
	stopLoop()
	if err := lifecycleReceive(t, loopDone); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- f.s.Drain(context.Background(), func() {}, func() {}) }()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.s.WaitAccepted(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	deadline, cancelDeadline := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelDeadline()
	if err := f.s.WaitAccepted(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline wait: %v", err)
	}
	select {
	case err := <-waitDone:
		t.Fatalf("wait returned during creation: %v", err)
	default:
	}
	close(continueCreation)
	submitCtx := lifecycleReceive(t, submitted)
	if submitCtx.Err() != nil {
		t.Fatalf("filing cancelled: %v", submitCtx.Err())
	}
	select {
	case err := <-waitDone:
		t.Fatalf("wait returned during filing: %v", err)
	default:
	}
	close(continueSubmit)
	lifecycleReceive(t, releasing)
	p, err := f.s.Snapshot(context.Background(), id)
	if err != nil || fleet.ObjectOrEmpty(p["filing"])["status"] != "filed" || fleet.Int64(fleet.ObjectOrEmpty(fleet.List(fleet.ObjectOrEmpty(p["filing"])["issues"])[0])["number"]) != 42 {
		t.Fatalf("filing not persisted: %v %v", p, err)
	}
	select {
	case err := <-waitDone:
		t.Fatalf("wait returned before release: %v", err)
	default:
	}
	close(continueRelease)
	if err := lifecycleReceive(t, waitDone); err != nil {
		t.Fatal(err)
	}
	if err := f.s.WaitAccepted(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunAndCloseCancelAcceptedWork(t *testing.T) {
	for _, mode := range []string{"run", "close"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			started := make(chan struct{})
			cancelled := make(chan struct{})
			finish := make(chan struct{})
			f.options.Answer = func(ctx context.Context, _ Object) (Object, error) {
				close(started)
				<-ctx.Done()
				close(cancelled)
				<-finish
				return nil, ctx.Err()
			}
			f.restart()
			if err := f.s.WaitAccepted(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, _ = f.call("/api/projects", Object{"id": id, "text": "作成"})
			lifecycleReceive(t, started)
			done := make(chan error, 1)
			if mode == "run" {
				ctx, cancel := context.WithCancel(context.Background())
				ready := make(chan struct{})
				go func() { done <- f.s.runRecovery(ctx, true, func() { close(ready) }) }()
				lifecycleReceive(t, ready)
				cancel()
			} else {
				go func() { f.s.Close(); done <- nil }()
			}
			lifecycleReceive(t, cancelled)
			select {
			case err := <-done:
				t.Fatalf("shutdown returned before work ended: %v", err)
			default:
			}
			close(finish)
			if err := lifecycleReceive(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}
