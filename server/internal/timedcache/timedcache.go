// Package timedcache keeps computed values in memory for a fixed time, so that
// a result polled every few seconds is not computed again on every poll.
package timedcache

import (
	"context"
	"sync"
	"time"
)

type entry[V any] struct {
	value V
	at    time.Time
}

// Cache keeps values by key for ttl. A nil *Cache keeps nothing: Get never
// finds a value, and Put and Begin do nothing.
type Cache[V any] struct {
	mu      sync.Mutex
	loading chan struct{}
	ttl     time.Duration
	clone   func(V) V
	entries map[string]entry[V]
}

// New returns a cache whose values stay for ttl. clone must return a copy that
// shares no mutable state with its argument: the cache stores and returns
// copies, so a caller changing a value never changes what is kept.
func New[V any](ttl time.Duration, clone func(V) V) *Cache[V] {
	return &Cache[V]{loading: make(chan struct{}, 1), ttl: ttl, clone: clone, entries: map[string]entry[V]{}}
}

// Begin waits until no other caller is loading and returns the function that
// ends this caller's load. Callers that look up and then compute a missing
// value inside Begin compute it once instead of once per concurrent caller.
func (c *Cache[V]) Begin(ctx context.Context) (func(), error) {
	if c == nil {
		return func() {}, nil
	}
	select {
	case c.loading <- struct{}{}:
		return func() { <-c.loading }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Cache[V]) fresh(e entry[V], now time.Time) bool {
	// A clock set back makes the age negative; the value is then read again.
	return !now.Before(e.at) && now.Sub(e.at) < c.ttl
}

// Get returns a copy of the value kept for key, if it was put less than ttl
// before now. An empty key is never kept.
func (c *Cache[V]) Get(key string, now time.Time) (V, bool) {
	var zero V
	if c == nil || key == "" {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !c.fresh(e, now) {
		return zero, false
	}
	return c.clone(e.value), true
}

// Put keeps a copy of value for key from now, and drops the values that are no
// longer fresh.
func (c *Cache[V]) Put(key string, value V, now time.Time) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if !c.fresh(e, now) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = entry[V]{c.clone(value), now}
}

// CopyValue copies the maps and lists of a decoded-JSON shaped value
// (map[string]any and []any, at any depth). Other values are returned as they
// are, so they must be immutable (numbers, strings, booleans, nil).
func CopyValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		if v == nil {
			return v
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = CopyValue(item)
		}
		return out
	case []any:
		if v == nil {
			return v
		}
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = CopyValue(item)
		}
		return out
	}
	return value
}
