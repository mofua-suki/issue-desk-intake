package consultationai

import (
	clist "container/list"
	"context"
	"errors"
	"sync"
)

const defaultBlobCacheBytes = 64 * 1024 * 1024

type blobKey struct{ repo, sha string }
type cachedBlob struct {
	key    blobKey
	source string
}
type blobFlight struct {
	done   chan struct{}
	source string
	err    error
}
type blobCache struct {
	mu      sync.Mutex
	limit   int // zero selects the default
	size    int
	entries map[blobKey]*clist.Element
	lru     clist.List
	flights map[blobKey]*blobFlight
}

func (c *blobCache) putLocked(key blobKey, source string) {
	limit := c.limit
	if limit == 0 {
		limit = defaultBlobCacheBytes
	}
	if len(source) > limit {
		return
	}
	if c.entries == nil {
		c.entries = make(map[blobKey]*clist.Element)
	}
	if old := c.entries[key]; old != nil {
		c.size -= len(old.Value.(cachedBlob).source)
		c.lru.Remove(old)
		delete(c.entries, key)
	}
	for c.size+len(source) > limit {
		old := c.lru.Back()
		item := old.Value.(cachedBlob)
		c.size -= len(item.source)
		delete(c.entries, item.key)
		c.lru.Remove(old)
	}
	c.entries[key] = c.lru.PushFront(cachedBlob{key, source})
	c.size += len(source)
}
func (c *blobCache) get(ctx context.Context, key blobKey, size int, fetch func() (string, error)) (string, error) {
	for {
		source, err, retry := c.getOnce(ctx, key, size, fetch)
		if !retry {
			return source, err
		}
	}
}

func (c *blobCache) getOnce(ctx context.Context, key blobKey, size int, fetch func() (string, error)) (string, error, bool) {
	c.mu.Lock()
	if item := c.entries[key]; item != nil {
		source := item.Value.(cachedBlob).source
		c.lru.MoveToFront(item)
		c.mu.Unlock()
		source, err := checkBlobSize(source, size)
		return source, err, false
	}
	if flight := c.flights[key]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err(), false
		case <-flight.done:
			if err := ctx.Err(); err != nil {
				return "", err, false
			}
			if flight.err != nil {
				if errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded) {
					return "", nil, true
				}
				return "", flight.err, false
			}
			source, err := checkBlobSize(flight.source, size)
			return source, err, false
		}
	}
	if c.flights == nil {
		c.flights = make(map[blobKey]*blobFlight)
	}
	flight := &blobFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()
	// The default error also releases waiters if fetch panics or calls Goexit.
	flight.err = errors.New("資料の取得が異常終了しました")
	defer func() {
		c.mu.Lock()
		if flight.err == nil {
			c.putLocked(key, flight.source)
		}
		delete(c.flights, key)
		close(flight.done)
		c.mu.Unlock()
	}()
	flight.source, flight.err = fetch()
	if err := ctx.Err(); err != nil {
		flight.source, flight.err = "", err
	}
	return flight.source, flight.err, false
}
func checkBlobSize(source string, size int) (string, error) {
	if len(source) != size {
		return "", errors.New("資料の内容・大きさ・UTF-8を確認できません")
	}
	return source, nil
}
