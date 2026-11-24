package statistics

import (
	"context"
	"strings"
	"time"
	"unicode"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

// pairはFIFO上限付きで、組数の二乗分のメモリを保持しない。
const waitScopePairLimit = 4096

type waitScopeKey struct {
	ID      int64
	Version int
}

type waitScopePair struct {
	Left, Right               int64
	LeftVersion, RightVersion int
}
type waitScopeCache struct {
	versions       map[int64]int
	histories      map[int64][]waitScopeVersion
	timeline       []time.Time
	timelineReady  bool
	legacyCuts     bool
	prepared       map[waitScopeKey][]string
	exemptions     map[string]map[string]bool
	pairs          map[waitScopePair]bool
	order          []waitScopePair
	next           int
	recomputations int
	assignments    *fleet.Assignments
}

func newWaitScopeCache(a *fleet.Assignments) *waitScopeCache {
	return &waitScopeCache{versions: map[int64]int{}, histories: map[int64][]waitScopeVersion{}, prepared: map[waitScopeKey][]string{}, exemptions: map[string]map[string]bool{}, pairs: map[waitScopePair]bool{}, assignments: a}
}
func (c *waitScopeCache) keys(ctx context.Context, j *waitJob) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cacheKey := waitScopeKey{ID: j.ID, Version: c.versions[j.ID]}
	if keys, ok := c.prepared[cacheKey]; ok {
		return keys, nil
	}
	ignore, ok := c.exemptions[j.Repo]
	if !ok {
		ignore = map[string]bool{}
		if c.assignments != nil {
			for _, path := range c.assignments.Exempt(j.Repo) {
				ignore[strings.Map(unicode.ToUpper, path)] = true
			}
		}
		c.exemptions[j.Repo] = ignore
	}
	keys := make([]string, 0, len(j.Scope))
	for _, path := range j.Scope {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := strings.Map(unicode.ToUpper, path)
		if !ignore[key] && !jobs.IsTestScopePath(path) {
			keys = append(keys, key)
		}
	}
	c.prepared[cacheKey] = keys
	return keys, nil
}
func (c *waitScopeCache) overlap(ctx context.Context, a, b *waitJob) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	pair := waitScopePair{Left: a.ID, Right: b.ID, LeftVersion: c.versions[a.ID], RightVersion: c.versions[b.ID]}
	if pair.Left > pair.Right {
		pair.Left, pair.Right = pair.Right, pair.Left
		pair.LeftVersion, pair.RightVersion = pair.RightVersion, pair.LeftVersion
	}
	if value, ok := c.pairs[pair]; ok {
		return value, nil
	}
	c.recomputations++
	x, err := c.keys(ctx, a)
	if err != nil {
		return false, err
	}
	y, err := c.keys(ctx, b)
	if err != nil {
		return false, err
	}
	overlap := false
outer:
	for _, left := range x {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for _, right := range y {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if left == "*" || right == "*" || left == right || strings.HasSuffix(left, "/") && strings.HasPrefix(right, left) || strings.HasSuffix(right, "/") && strings.HasPrefix(left, right) {
				overlap = true
				break outer
			}
		}
	}
	if len(c.order) < waitScopePairLimit {
		c.order = append(c.order, pair)
	} else {
		delete(c.pairs, c.order[c.next])
		c.order[c.next] = pair
		c.next = (c.next + 1) % waitScopePairLimit
	}
	c.pairs[pair] = overlap
	return overlap, nil
}

// Old versions remain available until their pairs are evicted by FIFO.
func (c *waitScopeCache) setVersion(id int64, version int) {
	c.versions[id] = version
}
