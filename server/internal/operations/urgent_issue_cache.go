package operations

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
)

const urgentIssueTTL = 30 * time.Second
const urgentIssueWorkers = 16

type urgentIssueCache struct {
	mu      sync.Mutex
	entries map[string]urgentCacheEntry
}

var urgentCacheInitMu sync.Mutex

// InitUrgentIssueCache prepares shared state before a Service is copied.
func (s *Service) InitUrgentIssueCache() {
	urgentCacheInitMu.Lock()
	defer urgentCacheInitMu.Unlock()
	if s.urgentCache == nil {
		s.urgentCache = &urgentIssueCache{entries: make(map[string]urgentCacheEntry)}
	}
}

type urgentCacheEntry struct {
	closed  bool
	expires time.Time
}

func urgentIssueKey(repo string, issue int64) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(repo), issue)
}

func (s *Service) lookupUrgentIssues(ctx context.Context, list []map[string]any) map[string]bool {
	s.InitUrgentIssueCache()
	ttl := s.UrgentCacheTTL
	if ttl <= 0 {
		ttl = urgentIssueTTL
	}
	cache := s.urgentCache
	result := make(map[string]bool)
	type task struct{ key, path string }
	tasks := make([]task, 0, len(list))
	seen := make(map[string]bool)
	cache.mu.Lock()
	now := time.Now()
	for key, entry := range cache.entries {
		if !now.Before(entry.expires) {
			delete(cache.entries, key)
		}
	}
	for _, row := range list {
		repo, issue := fleet.Text(row["repo"]), int64(fleet.Number(row["issue"]))
		key := urgentIssueKey(repo, issue)
		if seen[key] {
			continue
		}
		seen[key] = true
		if entry, ok := cache.entries[key]; ok {
			result[key] = entry.closed
			continue
		}
		base, err := gitea.RepoPath(repo)
		if s.Gitea != nil && err == nil && issue > 0 {
			tasks = append(tasks, task{key, fmt.Sprintf("%s/issues/%d", base, issue)})
		}
	}
	cache.mu.Unlock()
	var mu sync.Mutex
	var wg sync.WaitGroup
	queue := make(chan task)
	for i := 0; i < urgentIssueWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range queue {
				if ctx.Err() != nil {
					continue
				}
				var current map[string]any
				if err := s.Gitea.Do(ctx, "GET", item.path, nil, &current); err != nil {
					continue
				}
				state, ok := current["state"].(string)
				if !ok || (state != "open" && state != "closed") {
					continue
				}
				closed := state == "closed"
				cache.mu.Lock()
				cache.entries[item.key] = urgentCacheEntry{closed, time.Now().Add(ttl)}
				cache.mu.Unlock()
				mu.Lock()
				result[item.key] = closed
				mu.Unlock()
			}
		}()
	}
dispatch:
	for _, item := range tasks {
		select {
		case queue <- item:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(queue)
	wg.Wait()
	return result
}
