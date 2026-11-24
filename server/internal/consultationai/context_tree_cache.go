package consultationai

import (
	clist "container/list"
	"context"
	"errors"
	"maps"
	"sync"
)

// Charge strings and a conservative per-entry allowance; also cap the number
// of revisions so empty trees cannot grow the cache without bound.
const defaultTreeCacheBytes = 16 * 1024 * 1024
const maxCachedTrees = 128

type cachedTree struct {
	key     blobKey
	entries map[string]treeEntry
	size    int
}
type treeFlight struct {
	done    chan struct{}
	entries map[string]treeEntry
	err     error
}
type treeCache struct {
	mu          sync.Mutex
	limit, size int
	entries     map[blobKey]*clist.Element
	lru         clist.List
	flights     map[blobKey]*treeFlight
}

func treeSize(key blobKey, entries map[string]treeEntry) int {
	size := 128 + len(key.repo) + len(key.sha)
	for path, e := range entries {
		size += 192 + len(path) + len(e.Path) + len(e.SHA) + len(e.Type) + len(e.Mode)
	}
	return size
}
func (c *treeCache) putLocked(key blobKey, entries map[string]treeEntry) {
	limit := c.limit
	if limit == 0 {
		limit = defaultTreeCacheBytes
	}
	size := treeSize(key, entries)
	if size > limit {
		return
	}
	if c.entries == nil {
		c.entries = map[blobKey]*clist.Element{}
	}
	for c.size+size > limit || len(c.entries) >= maxCachedTrees {
		oldest := c.lru.Back()
		item := oldest.Value.(cachedTree)
		c.size -= item.size
		delete(c.entries, item.key)
		c.lru.Remove(oldest)
	}
	c.entries[key] = c.lru.PushFront(cachedTree{key, entries, size})
	c.size += size
}
func (c *treeCache) get(ctx context.Context, key blobKey, fetch func() (map[string]treeEntry, error)) (map[string]treeEntry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err, retry := c.once(ctx, key, fetch)
		if !retry {
			return entries, err
		}
	}
}
func (c *treeCache) once(ctx context.Context, key blobKey, fetch func() (map[string]treeEntry, error)) (map[string]treeEntry, error, bool) {
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		entries := entry.Value.(cachedTree).entries
		c.lru.MoveToFront(entry)
		c.mu.Unlock()
		return maps.Clone(entries), nil, false
	}
	if flight := c.flights[key]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err(), false
		case <-flight.done:
			if err := ctx.Err(); err != nil {
				return nil, err, false
			}
			if errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded) {
				return nil, nil, true
			}
			return maps.Clone(flight.entries), flight.err, false
		}
	}
	if c.flights == nil {
		c.flights = map[blobKey]*treeFlight{}
	}
	flight := &treeFlight{done: make(chan struct{}), err: errors.New("資料の一覧取得が異常終了しました")}
	c.flights[key] = flight
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if flight.err == nil {
			c.putLocked(key, flight.entries)
		}
		delete(c.flights, key)
		close(flight.done)
		c.mu.Unlock()
	}()
	flight.entries, flight.err = fetch()
	if err := ctx.Err(); err != nil {
		flight.err = err
	}
	if flight.err != nil {
		flight.entries = nil
	}
	return maps.Clone(flight.entries), flight.err, false
}
