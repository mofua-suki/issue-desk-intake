package startup

import (
	"context"
	"errors"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
)

func TestProvidersRepeatedSlowRefreshTerminates(t *testing.T) {
	for _, scenario := range []string{"exhausted", "cancel during refresh"} {
		t.Run(scenario, func(t *testing.T) {
			f := newProviderFixture(t)
			clock := setProviderClock(f)
			var gates [2][3]<-chan struct{}
			var release [2][3]func()
			for index := range gates {
				for call := range gates[index] {
					gates[index][call], release[index][call] = newProviderCleanup(t)
				}
			}
			f.hook = func(ctx context.Context, _ ai.ProcessRequest, index int, count int64) error {
				if count > int64(len(gates[index])) {
					<-ctx.Done()
					return ctx.Err()
				}
				select {
				case <-gates[index][count-1]:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			// No request deadline: termination must come from the refresh policy.
			const clients = 12
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests := make([]<-chan providerResult, clients)
			for i := range requests {
				requests[i] = requestProviders(f.app, ctx)
			}
			ownerCtx, cancelOwner := context.WithCancel(context.Background())
			defer cancelOwner()
			owner := requestProviders(f.app, ownerCtx)
			waitProviderStarts(t, f, 2)
			waitProviderCondition(t, f, func(c *providerCache) bool {
				return c.current[0].waiters == clients+1 && c.current[1].waiters == clients+1
			})
			cancelOwner()
			if result := receiveProviderResult(t, owner); !errors.Is(result.err, context.Canceled) {
				t.Fatalf("cancelled waiter: %+v", result)
			}
			waitProviderCondition(t, f, func(c *providerCache) bool {
				return c.current[0].waiters == clients && c.current[1].waiters == clients
			})

			// Every Codex check takes four virtual seconds; every Claude check
			// takes eight. Completions at 104/108, 112, 120 repeatedly expire the
			// other result, reproducing the alternating-refresh regression.
			clock.Store(time.Unix(104, 0).UnixNano())
			release[0][0]()
			waitProviderCondition(t, f, func(c *providerCache) bool { return c.current[0].finished })
			clock.Store(time.Unix(108, 0).UnixNano())
			release[1][0]()
			waitProviderStarts(t, f, 1)
			waitProviderCondition(t, f, func(c *providerCache) bool {
				return c.current[0].waiters == clients && c.current[1].waiters == clients
			})
			assertProviderCalls(t, f, 2, 1)
			clock.Store(time.Unix(112, 0).UnixNano())
			release[0][1]()
			waitProviderStarts(t, f, 1)
			waitProviderCondition(t, f, func(c *providerCache) bool {
				return c.current[0].waiters == clients && c.current[1].waiters == clients
			})
			assertProviderCalls(t, f, 2, 2)
			if scenario == "cancel during refresh" {
				cancel()
			} else {
				clock.Store(time.Unix(120, 0).UnixNano())
				release[1][1]()
			}
			for _, request := range requests {
				result := receiveProviderResult(t, request)
				want := errProviderFreshness
				if scenario == "cancel during refresh" {
					want = context.Canceled
				}
				if !errors.Is(result.err, want) || result.rows != nil {
					t.Fatalf("expected no stale rows and %v; got %+v", want, result)
				}
			}
			waitProviderCondition(t, f, func(c *providerCache) bool { return len(c.live) == 0 })
			assertProviderCalls(t, f, 2, 2)
			if f.maximum.Load() != 2 {
				t.Fatalf("maximum concurrency: %d", f.maximum.Load())
			}
			if scenario == "exhausted" {
				// Exhausting one request's budget does not poison future reads.
				release[0][2]()
				assertProviderRows(t, receiveProviderResult(t, requestProviders(f.app, context.Background())), true)
				assertProviderCalls(t, f, 3, 2)
			}
		})
	}
}
