package statistics

import (
	"context"
	"sort"
	"time"
)

// waitTimeline indexes every instant that can change a start-wait cause.
func waitTimeline(ctx context.Context, all map[int64]*waitJob) ([]time.Time, error) {
	var times []time.Time
	for _, job := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		times = append(times, job.Filed)
		for _, e := range job.Events {
			countWaitEvent(ctx)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if _, ok := waitTransition(e); ok || e.Kind == "scope_narrowed" || e.Kind == "scope_widened" {
				times = append(times, e.At)
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	return times, ctx.Err()
}

func waitTimelineCuts(times []time.Time, begin, end time.Time) []time.Time {
	if !begin.Before(end) {
		return nil
	}
	first := sort.Search(len(times), func(i int) bool { return times[i].After(begin) })
	last := sort.Search(len(times), func(i int) bool { return !times[i].Before(end) })
	return times[first:last]
}
