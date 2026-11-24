package startup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
)

// Call after newProviderFixture so the LIFO cleanup releases Execute before
// App.Close waits for it. Normal and failed paths may both release safely.
func newProviderCleanup(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	cleanup := make(chan struct{})
	release := sync.OnceFunc(func() { close(cleanup) })
	t.Cleanup(release)
	return cleanup, release
}

func TestProvidersWarmResultExpiresWhileOtherProviderRuns(t *testing.T) {
	for cachedIndex, name := range providerNames {
		t.Run(name, func(t *testing.T) {
			f := newProviderFixture(t)
			clock := setProviderClock(f)
			cachedGate, releaseCached := newProviderCleanup(t)
			slowGate, releaseSlow := newProviderCleanup(t)
			refreshGate, releaseRefresh := newProviderCleanup(t)
			f.hook = func(ctx context.Context, _ ai.ProcessRequest, index int, count int64) error {
				var gate <-chan struct{}
				if count == 1 {
					gate = slowGate
					if index == cachedIndex {
						gate = cachedGate
					}
				} else if index != cachedIndex {
					gate = refreshGate
				} else {
					return nil
				}
				select {
				case <-gate:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			prime := requestProviders(f.app, context.Background())
			waitProviderStarts(t, f, 2)
			releaseSlow()
			waitProviderCondition(t, f, func(c *providerCache) bool { return c.current[1-cachedIndex].finished })
			clock.Add(int64(time.Second))
			releaseCached()
			assertProviderRows(t, receiveProviderResult(t, prime), true)
			// The slow provider expired at 103; the cached provider expires at 104.
			clock.Store(time.Unix(104, 0).Add(-time.Nanosecond).UnixNano())
			const clients = 12
			requests := make([]<-chan providerResult, clients)
			for i := range requests {
				requests[i] = requestProviders(f.app, context.Background())
			}
			waitProviderStarts(t, f, 1)
			waitProviderCondition(t, f, func(c *providerCache) bool {
				return c.current[0].waiters == clients && c.current[1].waiters == clients
			})
			f.ready.Store(false)
			clock.Add(1) // Exactly the cached provider's deadline, after joining it.
			releaseRefresh()
			for _, request := range requests {
				assertProviderRows(t, receiveProviderResult(t, request), false)
			}
			assertProviderCalls(t, f, 2, 2)
			if f.maximum.Load() > 2 {
				t.Fatal("refresh exceeded the App's two slots")
			}
		})
	}
}

func TestProvidersColdResultExpiresWhileOtherProviderRuns(t *testing.T) {
	for earlyIndex, name := range providerNames {
		t.Run(name, func(t *testing.T) {
			f := newProviderFixture(t)
			clock := setProviderClock(f)
			earlyGate, releaseEarly := newProviderCleanup(t)
			lateGate, releaseLate := newProviderCleanup(t)
			f.hook = func(ctx context.Context, _ ai.ProcessRequest, index int, count int64) error {
				if count != 1 {
					return nil
				}
				gate := lateGate
				if index == earlyIndex {
					gate = earlyGate
				}
				select {
				case <-gate:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			request := requestProviders(f.app, context.Background())
			waitProviderStarts(t, f, 2)
			releaseEarly()
			waitProviderCondition(t, f, func(c *providerCache) bool { return c.current[earlyIndex].finished })
			f.ready.Store(false)
			clock.Add(int64(providerTTL + time.Nanosecond))
			releaseLate()
			assertProviderRows(t, receiveProviderResult(t, request), false)
			want := [2]int64{1, 1}
			want[earlyIndex]++
			assertProviderCalls(t, f, want[0], want[1])
		})
	}
}

func TestProvidersBlockedCleanupOnFailure(t *testing.T) {
	// A child intentionally fails before the normal release point. A marker
	// registered before the fixture proves App.Close finished during cleanup.
	if scenario := os.Getenv("ISSUE1007_CLEANUP_FAILURE"); scenario != "" {
		if scenario == "stalled" {
			// Simulate the cleanup regression itself. The parent must kill and
			// reap this process instead of relying on the suite's global timeout.
			t.Cleanup(func() {
				t.Log("intentional cleanup stall")
				time.Sleep(time.Hour)
			})
			t.Fatal("intentional failure before stalled cleanup")
		}
		t.Cleanup(func() { t.Log("App.Close completed after intentional failure") })
		f := newProviderFixture(t)
		cleanup, _ := newProviderCleanup(t)
		stopped := make(chan struct{}, 2)
		f.hook = func(ctx context.Context, _ ai.ProcessRequest, _ int, _ int64) error {
			<-ctx.Done()
			stopped <- struct{}{}
			<-cleanup
			return ctx.Err()
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request := requestProviders(f.app, ctx)
		waitProviderStarts(t, f, 2)
		cancel()
		if err := receiveProviderResult(t, request).err; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		for range 2 {
			<-stopped
		}
		if scenario == "queued" {
			requestProviders(f.app, context.Background())
			waitProviderCondition(t, f, func(c *providerCache) bool { return len(c.live) == 4 })
		}
		t.Fatal("intentional failure before normal release")
	}
	for _, scenario := range []string{"cancelled", "queued"} {
		t.Run(scenario, func(t *testing.T) {
			result := runProviderCleanupChild(t, scenario, 20*time.Second)
			output, err, contextErr := result.output, result.err, result.contextErr
			var exit *exec.ExitError
			if contextErr != nil || !errors.As(err, &exit) || exit.ExitCode() != 1 || strings.Contains(string(output), "WARNING: DATA RACE") ||
				!strings.Contains(string(output), "intentional failure before normal release") ||
				!strings.Contains(string(output), "App.Close completed after intentional failure") {
				t.Fatalf("failure cleanup did not finish: %v (context: %v)\n%s", err, contextErr, output)
			}
		})
	}
}

// The child normally exits in milliseconds (plus race runtime shutdown). Give
// it 20 seconds, capped at half the parent's remaining budget, and bound pipe
// draining too. CommandContext kills on expiry; CombinedOutput waits/reaps it.
type providerCleanupResult struct {
	output     []byte
	err        error
	contextErr error
	cmd        *exec.Cmd
}

func runProviderCleanupChild(t *testing.T, scenario string, budget time.Duration) providerCleanupResult {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		if remaining := time.Until(deadline) / 2; remaining < budget {
			budget = remaining
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProvidersBlockedCleanupOnFailure$", "-test.v")
	cmd.Env = append(os.Environ(), "ISSUE1007_CLEANUP_FAILURE="+scenario)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	return providerCleanupResult{output: output, err: err, contextErr: ctx.Err(), cmd: cmd}
}

func TestProvidersBlockedCleanupChildDeadline(t *testing.T) {
	result := runProviderCleanupChild(t, "stalled", 5*time.Second)
	output, err, contextErr, cmd := result.output, result.err, result.contextErr, result.cmd
	if !errors.Is(contextErr, context.DeadlineExceeded) || err == nil ||
		!strings.Contains(string(output), "intentional cleanup stall") {
		t.Fatalf("stalled child was not detected as a deadline failure: %v (context: %v)\n%s", err, contextErr, output)
	}
	if cmd.ProcessState == nil || !errors.Is(cmd.Process.Signal(os.Interrupt), os.ErrProcessDone) {
		t.Fatal("stalled child was not reaped")
	}
}
