package startup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/runtimegate"
)

func requireDraining(t *testing.T, err error) {
	t.Helper()
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem.Status != 503 || problem.Code != "SERVER_DRAINING" {
		t.Fatalf("expected SERVER_DRAINING (503), got %v", err)
	}
}

func TestGateUnset(t *testing.T) {
	app := &App{}
	ctx, release, err := app.acquireGate(context.Background(), "filing")
	requireDraining(t, err)
	if ctx != nil || release != nil {
		t.Fatal("failed acquisition returned context or release")
	}
	_, err = (gatedRunner{app: app}).Call(context.Background(), ai.Request{})
	requireDraining(t, err)
}

func TestGateResolvedAfterRegistration(t *testing.T) {
	app := &App{}
	// These are the same values registered with the business services.
	acquire := app.acquireGate
	runner := gatedRunner{app: app}
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: t.TempDir(), Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close() })
	app.setGate(gate)
	ctx, release, err := acquire(context.Background(), "filing")
	if err != nil {
		t.Fatal(err)
	}
	gate.BeginHandoff()
	// A previously accepted filing can still acquire its AI slot during handoff.
	_, finish, err := app.acquireGate(ctx, "ai")
	if err != nil {
		t.Fatalf("accepted work could not continue: %v", err)
	}
	finish()
	release()
	_, _, err = acquire(context.Background(), "project")
	requireDraining(t, err)
	_, err = runner.Call(context.Background(), ai.Request{})
	requireDraining(t, err)
}

func TestGateAcquirePreservesCancellationAndRelease(t *testing.T) {
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: t.TempDir(), Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close() })
	app := &App{Gate: gate}
	_, release, err := app.acquireGate(context.Background(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = app.acquireGate(ctx, "ai")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	release()
	_, release, err = app.acquireGate(context.Background(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestRegisteredBusinessUsesCurrentGate(t *testing.T) {
	app := openLifecycleApp(t, lifecycleGitea, nil)
	app.StopBackground()
	original := app.currentGate()
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: t.TempDir(), Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	gate.BeginHandoff()
	app.setGate(gate)
	defer func() { app.setGate(original); _ = gate.Close() }()
	_, err = app.AI.Answer(context.Background(), map[string]any{"schema": `{"type":"object"}`})
	requireDraining(t, err)
}

func TestGateMessagesDescribeActualState(t *testing.T) {
	app := &App{}
	_, _, unset := app.acquireGate(context.Background(), "ai")
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: t.TempDir(), Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	gate.BeginHandoff()
	app.setGate(gate)
	_, _, draining := app.acquireGate(context.Background(), "ai")
	for _, tc := range []struct {
		err     error
		message string
	}{
		{unset, "Serverの準備中のため新しい処理を受け付けられません。"},
		{draining, "Serverの入れ替えのため新しい処理を止めています。"},
	} {
		requireDraining(t, tc.err)
		var problem *contract.Problem
		errors.As(tc.err, &problem)
		if problem.Message != tc.message {
			t.Errorf("message = %q, want %q", problem.Message, tc.message)
		}
	}
}

func TestReviewNotesUsesCurrentGate(t *testing.T) {
	app := &App{}
	_, err := app.answerReviewNotes(context.Background(), "owner/repo", "input")
	requireDraining(t, err)
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: t.TempDir(), Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	gate.BeginHandoff()
	app.setGate(gate)
	_, err = app.answerReviewNotes(context.Background(), "owner/repo", "input")
	requireDraining(t, err)
}

func TestGateConcurrentSetAndAcquire(t *testing.T) {
	app := &App{}
	_, release, err := app.acquireGate(context.Background(), "ai")
	requireDraining(t, err)
	if release != nil {
		t.Fatal("unset gate returned a release")
	}
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: t.TempDir(), Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close() })
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				app.setGate(gate)
				app.setGate(nil)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				_, release, err := app.acquireGate(context.Background(), "ai")
				if err != nil {
					var problem *contract.Problem
					if !errors.As(err, &problem) || problem.Status != 503 || problem.Code != "SERVER_DRAINING" || release != nil {
						t.Errorf("acquire: release=%v error=%v", release != nil, err)
					}
					continue
				}
				if release == nil {
					t.Error("successful acquisition returned no release")
					continue
				}
				release()
			}
		}()
	}
	close(start)
	wg.Wait()
	app.setGate(gate)
	_, release, err = app.acquireGate(context.Background(), "ai")
	if err != nil {
		t.Fatalf("acquisition after restoring gate: %v", err)
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := gate.WaitIdle(ctx); err != nil {
		t.Fatalf("waiting for gate to become idle (possible unreleased acquisition): %v", err)
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
}
