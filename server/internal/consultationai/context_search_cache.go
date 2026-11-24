package consultationai

import "sync"

// A search result is bounded by maxSource. Limit the number of results as well
// so that empty results and keys cannot grow the cache without bound.
const searchCacheSlots = 16

type searchKey struct{ repo, revision, keyword string }

type searchCacheEntry struct {
	key    searchKey
	result string
}

type searchCache struct {
	mu      sync.Mutex
	next    int
	entries [searchCacheSlots]searchCacheEntry
}

func (c *searchCache) get(key searchKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range c.entries {
		if item.key == key {
			return item.result, true
		}
	}
	return "", false
}

func (c *searchCache) put(key searchKey, result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range c.entries {
		if item.key == key {
			return
		}
	}
	c.entries[c.next] = searchCacheEntry{key: key, result: result}
	c.next = (c.next + 1) % len(c.entries)
}
