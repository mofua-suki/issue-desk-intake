package runtimegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func fixture(t *testing.T) (*Gate, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	g, err := New(Options{RuntimeDir: t.TempDir(), Revision: strings.Repeat("a", 40), Capacity: 1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g, &now
}
func drain(t *testing.T, g *Gate, now time.Time) int64 {
	t.Helper()
	until := now.Add(time.Minute).UnixMilli()
	if err := os.WriteFile(filepath.Join(g.directory(), "intake-drain.request"), []byte(strconv.FormatInt(until, 10)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := g.Tick(); err != nil {
		t.Fatal(err)
	}
	return until
}
func cancelledAcquire(t *testing.T, g *Gate, ctx context.Context, kind string) {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	if _, release, err := g.Acquire(bounded, kind); err == nil {
		release()
		t.Fatal("blocked acquisition succeeded", kind)
	}
}
func TestDrainWaitsForAcceptedWorkAndAllowsItsNestedAI(t *testing.T) {
	g, now := fixture(t)
	ctx, finish, err := g.Acquire(context.Background(), "filing")
	if err != nil {
		t.Fatal(err)
	}
	drain(t, g, *now)
	if _, err := os.Stat(filepath.Join(g.directory(), "intake-drain.held")); !os.IsNotExist(err) {
		t.Fatal("marked held while filing is active")
	}
	cancelledAcquire(t, g, context.Background(), "project")
	_, endAI, err := g.Acquire(ctx, "ai")
	if err != nil {
		t.Fatal("accepted filing could not finish its AI check", err)
	}
	endAI()
	finish()
	if err = g.Tick(); err != nil {
		t.Fatal(err)
	}
	held, err := os.ReadFile(filepath.Join(g.directory(), "intake-drain.held"))
	if err != nil || strings.TrimSpace(string(held)) == "" {
		t.Fatal("idle drain was not acknowledged", err)
	}
	cancelledAcquire(t, g, ctx, "ai")
	if err = os.Remove(filepath.Join(g.directory(), "intake-drain.request")); err != nil {
		t.Fatal(err)
	}
	if err = g.Tick(); err != nil {
		t.Fatal(err)
	}
	_, release, err := g.Acquire(context.Background(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
func TestCapacityCancellationAndReleaseAreBounded(t *testing.T) {
	g, _ := fixture(t)
	_, release, err := g.Acquire(context.Background(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	cancelledAcquire(t, g, context.Background(), "ai")
	release()
	release()
	_, release2, err := g.Acquire(context.Background(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	release2()
	g.mu.Lock()
	if g.active["ai"] != 0 {
		t.Fatal("release counted twice")
	}
	g.mu.Unlock()
}
func TestHeartbeatKeepsPreviousTimeOnShutdownAndIgnoresExpiredRequests(t *testing.T) {
	g, now := fixture(t)
	_, finish, err := g.Acquire(context.Background(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	drain(t, g, *now)
	var before struct {
		Time     int64  `json:"time"`
		PID      int    `json:"pid"`
		Revision string `json:"revision"`
		Busy     bool   `json:"busy"`
	}
	read := func(v any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(g.directory(), "intake-heartbeat.json"))
		if err != nil || json.Unmarshal(data, v) != nil {
			t.Fatal("invalid heartbeat", err)
		}
	}
	read(&before)
	if !before.Busy || before.PID < 1 || before.Revision != strings.Repeat("a", 40) {
		t.Fatal(before)
	}
	*now = now.Add(2 * time.Minute)
	if err = g.Tick(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(g.directory(), "intake-drain.held")); !os.IsNotExist(err) {
		t.Fatal("expired request remains held")
	}
	finish()
	read(&before)
	*now = now.Add(time.Minute)
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	var after struct {
		Time int64 `json:"time"`
		Busy bool  `json:"busy"`
	}
	read(&after)
	if after.Time != before.Time || after.Busy {
		t.Fatal("shutdown heartbeat could impersonate a new startup", before, after)
	}
}

func TestHandoffPersistsAndAllowsActiveContinuations(t *testing.T) {
	for _, kind := range []string{"filing", "project"} {
		t.Run(kind, func(t *testing.T) {
			g, now := fixture(t)
			ctx, finish, err := g.Acquire(context.Background(), kind)
			if err != nil {
				t.Fatal(err)
			}
			drain(t, g, *now)
			g.BeginHandoff()
			g.BeginHandoff()
			if _, end, err := g.Acquire(ctx, "ai"); err != nil {
				t.Fatal(err)
			} else {
				end()
			}
			finish()
			finish()
			if _, _, err := g.Acquire(ctx, "ai"); err == nil {
				t.Fatal("released context admitted")
			}
			if err := os.Remove(filepath.Join(g.directory(), "intake-drain.request")); err != nil {
				t.Fatal(err)
			}
			if err := g.Tick(); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"ai", "filing", "project"} {
				if _, _, err := g.Acquire(context.Background(), k); err == nil {
					t.Fatalf("%s admitted after removal", k)
				}
			}
			drain(t, g, *now)
			*now = now.Add(2 * time.Minute)
			if err := g.Tick(); err != nil {
				t.Fatal(err)
			}
			if _, _, err := g.Acquire(context.Background(), "filing"); err == nil {
				t.Fatal("admitted after expiry")
			}
			if err := g.WaitIdle(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHandoffUnblocksCapacityWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g, _ := fixture(t)
		_, release, err := g.Acquire(context.Background(), "ai")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		result := make(chan error, 1)
		go func() {
			_, end, err := g.Acquire(context.Background(), "ai")
			if end != nil {
				end()
			}
			result <- err
		}()
		synctest.Wait()
		g.BeginHandoff()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("waiting AI admitted")
			}
		case <-time.After(time.Second):
			t.Fatal("waiter did not return")
		}

	})
}

func TestWaitIdleCancellationAndRelease(t *testing.T) {
	g, _ := fixture(t)
	var releases []func()
	for _, kind := range []string{"ai", "filing", "project"} {
		_, release, err := g.Acquire(context.Background(), kind)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	g.BeginHandoff()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.WaitIdle(ctx); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := g.WaitIdle(ctx); err != context.DeadlineExceeded {
		t.Fatalf("deadline: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- g.WaitIdle(context.Background()) }()
	for _, release := range releases[:2] {
		release()
		release()
	}
	select {
	case <-done:
		t.Fatal("returned while project active")
	default:
	}
	releases[2]()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not finish")
	}
	if _, _, err := g.Acquire(context.Background(), "ai"); err == nil {
		t.Fatal("cancellation cleared handoff")
	}
}

func TestHandoffConcurrentAcquire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for i := 0; i < 100; i++ {
			g, _ := fixture(t)
			start := make(chan struct{})
			type result struct {
				release func()
				err     error
			}
			done := make(chan result, 1)
			go func() {
				<-start
				_, release, err := g.Acquire(context.Background(), "filing")
				done <- result{release, err}
			}()
			close(start)
			g.BeginHandoff()
			acquired := <-done
			if _, _, err := g.Acquire(context.Background(), "filing"); err == nil {
				t.Fatal("admitted after handoff")
			}
			idle := make(chan error, 1)
			go func() { idle <- g.WaitIdle(context.Background()) }()
			synctest.Wait()
			if acquired.err == nil {
				g.mu.Lock()
				active := g.active["filing"]
				g.mu.Unlock()
				if active != 1 {
					t.Fatalf("accepted work is not counted: %d", active)
				}
				select {
				case <-idle:
					t.Fatal("WaitIdle returned while accepted work remained")
				default:
				}
				acquired.release()
			}
			if err := <-idle; err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestHandoffWaitsForAlreadyAcceptedWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g, _ := fixture(t)
		_, release, err := g.Acquire(context.Background(), "filing")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		g.BeginHandoff()
		idle := make(chan error, 1)
		go func() { idle <- g.WaitIdle(context.Background()) }()
		synctest.Wait()
		select {
		case err := <-idle:
			t.Fatalf("accepted filing was not retained: %v", err)
		default:
		}
		release()
		if err := <-idle; err != nil {
			t.Fatal(err)
		}
	})
}
