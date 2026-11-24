package statistics

import (
	"encoding/json"
	"sort"
	"time"
)

type waitScopeVersion struct {
	at    time.Time
	scope []string
}

// buildWaitScopeHistory starts from the first narrowing before snapshot and
// reconstructs versions in event order, including future events. Older widening lacks before.
func buildWaitScopeHistory(j *waitJob) []waitScopeVersion {
	first := -1
	var scope []string
	for i, e := range j.ScopeEvents {
		if e.Kind == "scope_narrowed" {
			if v, ok := scopeSnapshot(e.Data["before"]); ok {
				scope, first = v, i
				break
			}
		}
	}
	if first < 0 {
		return []waitScopeVersion{{scope: j.Scope}}
	}
	versions := []waitScopeVersion{{scope: scope}}
	for _, e := range j.ScopeEvents[first:] {
		var next []string
		var ok bool
		switch e.Kind {
		case "scope_narrowed":
			next, ok = scopeSnapshot(e.Data["after"])
		case "scope_widened", "scope_wait":
			var add []string
			add, ok = scopeSnapshot(e.Data["add"])
			if ok {
				next = append([]string{}, scope...)
				seen := map[string]bool{}
				for _, path := range next {
					seen[path] = true
				}
				for _, path := range add {
					if !seen[path] {
						next = append(next, path)
						seen[path] = true
					}
				}
				if seen["*"] {
					next = []string{"*"}
				}
			}
		}
		if ok {
			scope = next
			versions = append(versions, waitScopeVersion{at: e.At, scope: scope})
		}
	}
	return versions
}

func scopeVersionAt(versions []waitScopeVersion, at time.Time) int {
	return sort.Search(len(versions)-1, func(i int) bool { return versions[i+1].at.After(at) })
}

func (c *waitScopeCache) scopeAt(j *waitJob, at time.Time) ([]string, int) {
	versions, ok := c.histories[j.ID]
	if !ok {
		versions = buildWaitScopeHistory(j)
		c.histories[j.ID] = versions
	}
	version := scopeVersionAt(versions, at)
	return versions[version].scope, version
}

func scopeSnapshot(v any) ([]string, bool) {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil, false
	}
	var paths []string
	if json.Unmarshal(b, &paths) != nil {
		return nil, false
	}
	return paths, true
}
