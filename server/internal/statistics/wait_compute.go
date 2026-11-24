package statistics

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"time"
)

func waitStateAt(ctx context.Context, j *waitJob, at time.Time) (waitState, error) {
	if j.stateIndex != nil {
		if err := ctx.Err(); err != nil {
			return waitState{}, err
		}
		return j.stateIndex.at(ctx, j.Filed, at), nil
	}
	state := waitState{Status: "unknown"}
	hasHistory := false
	applied := false
	for _, e := range j.Events {
		countWaitEvent(ctx)
		if err := ctx.Err(); err != nil {
			return waitState{}, err
		}
		next, ok := waitTransition(e)
		if !ok {
			continue
		}
		hasHistory = true
		if !e.At.After(at) {
			state = next
			applied = true
		}
	}
	if !applied && hasHistory && !j.Filed.After(at) {
		state.Status = "absent"
	}
	return state, nil
}
func waitStartCause(ctx context.Context, cache *waitScopeCache, target *waitJob, all map[int64]*waitJob, at time.Time, a *fleet.Assignments, slots bool) (string, error) {
	states := map[int64]waitState{}
	for id, j := range all {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if j.Filed.After(at) {
			continue
		}
		state, err := waitStateAt(ctx, j, at)
		if err != nil {
			return "", err
		}
		states[id] = state
	}
	unknownDep := false
	for _, id := range target.Deps {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		state, exists := states[id]
		if !exists {
			if all[id] == nil {
				unknownDep = true
			} else {
				return "dependency", nil
			}
		} else if state.Status != "merged" && state.Status != "closed" {
			if state.Status == "unknown" {
				unknownDep = true
			} else {
				return "dependency", nil
			}
		}
	}
	if unknownDep {
		return "unknown", nil
	}
	targetAt := *target
	var targetVersion int
	targetAt.Scope, targetVersion = cache.scopeAt(target, at)
	missing := targetAt.Scope == nil
	unknown, noHost := false, false
	counts := map[string]int{}
	for id, state := range states {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if id == target.ID {
			continue
		}
		if state.Status == "unknown" {
			unknown = true
		}
		if state.Status != "running" {
			continue
		}
		if state.Host == "" {
			noHost = true
		} else {
			counts[state.Host]++
		}
		j := *all[id]
		var version int
		j.Scope, version = cache.scopeAt(all[id], at)
		if !strings.EqualFold(j.Repo, target.Repo) {
			continue
		}
		if j.Scope == nil {
			missing = true
		} else if targetAt.Scope != nil {
			cache.setVersion(targetAt.ID, targetVersion)
			cache.setVersion(j.ID, version)
			overlap, err := cache.overlap(ctx, &targetAt, &j)
			if err != nil {
				return "", err
			}
			if overlap {
				return "scopeOverlap", nil
			}
		}
	}
	if missing || unknown || noHost || !slots {
		return "unknown", nil
	}
	for _, host := range a.Implementers[target.Repo] {
		if counts[host] < a.Hosts[host].Max {
			return "idle", nil
		}
	}
	return "slotFull", nil
}
