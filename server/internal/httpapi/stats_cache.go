// 統計の計算結果を同じ鍵の要求で共有し、一定時間使い回すキャッシュ。

package httpapi

import (
	"context"
	"errors"
	"sync"
	"time"
)

type statsResult struct {
	body      []byte
	err       error
	cacheable bool
}

type statsEntry struct {
	result    statsResult
	completed time.Time
}

type statsWaiter struct{ ctx context.Context }

type statsFlight struct {
	active  map[*statsWaiter]struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	result  statsResult
}

type statsCache struct {
	mu      sync.Mutex
	entries map[string]statsEntry
	flights map[string]*statsFlight
}

func (c *statsCache) get(ctx context.Context, key string, now func() time.Time, calculate func(context.Context) statsResult) (statsResult, bool) {
	c.mu.Lock()
	if ctx.Err() != nil {
		c.mu.Unlock()
		return statsResult{}, false
	}
	if c.entries == nil {
		c.entries = make(map[string]statsEntry)
		c.flights = make(map[string]*statsFlight)
	}
	at := now()
	for k, entry := range c.entries {
		if !at.Before(entry.completed.Add(60 * time.Second)) {
			delete(c.entries, k)
		}
	}
	if entry, ok := c.entries[key]; ok {
		c.mu.Unlock()
		return entry.result, true
	}
	flight := c.flights[key]
	if flight != nil {
		if !flight.live() {
			delete(c.flights, key)
			flight.cancel()
			flight = nil
		}
	}
	if flight == nil {
		workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &statsFlight{done: make(chan struct{}), cancel: cancel, active: make(map[*statsWaiter]struct{})}
		c.flights[key] = flight
		go c.runFlight(key, flight, workCtx, cancel, now, calculate)
	}
	waiter := &statsWaiter{ctx: ctx}
	flight.active[waiter] = struct{}{}
	flight.waiters++
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		c.mu.Lock()
		flight.waiters--
		delete(flight.active, waiter)
		if flight.waiters == 0 && c.flights[key] == flight {
			delete(c.flights, key)
			flight.cancel()
		}
		c.mu.Unlock()
		return statsResult{}, false
	case <-flight.done:
		if ctx.Err() != nil {
			return statsResult{}, false
		}
		return flight.result, true
	}
}

// live は計算を待っている要求のうち、まだ切れていないものがあるかを返す。呼び出し側がロックを持つ。
func (flight *statsFlight) live() bool {
	for waiter := range flight.active {
		if waiter.ctx.Err() == nil {
			return true
		}
	}
	return false
}

// runFlight は統計を計算し、待っている要求が残っていれば結果を保存して、待ち手へ結果を渡す。
func (c *statsCache) runFlight(key string, flight *statsFlight, workCtx context.Context, cancel context.CancelFunc, now func() time.Time, calculate func(context.Context) statsResult) {
	result := func() (result statsResult) {
		defer func() {
			if recovered := recover(); recovered != nil {
				result = statsResult{err: errors.New("statistics calculation panic")}
			}
		}()
		return calculate(workCtx)
	}()
	c.mu.Lock()
	if c.flights[key] == flight {
		delete(c.flights, key)
		if result.cacheable && flight.live() && workCtx.Err() == nil {
			c.storeLocked(key, result, now())
		}
	}
	flight.result = result
	close(flight.done)
	c.mu.Unlock()
	cancel()
}

// storeLocked は結果を保存する。16件を超えるときは最も古い結果を捨てる。呼び出し側がロックを持つ。
func (c *statsCache) storeLocked(key string, result statsResult, completed time.Time) {
	if len(c.entries) >= 16 {
		var oldest string
		var t time.Time
		for k, e := range c.entries {
			if oldest == "" || e.completed.Before(t) {
				oldest, t = k, e.completed
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = statsEntry{result: result, completed: completed}
}
