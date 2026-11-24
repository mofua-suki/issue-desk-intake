package statistics

import "context"

// Optional per-request instrumentation is kept out of the API response.
type statisticsWorkKey struct{}
type statisticsWork struct {
	tierRows           int
	waitEvents         int
	waitStartIntervals int
	stage              func(string)
}

func countTierWork(ctx context.Context, n int) {
	if w, ok := ctx.Value(statisticsWorkKey{}).(*statisticsWork); ok {
		w.tierRows += n
	}
}
func countWaitEvent(ctx context.Context) {
	if w, ok := ctx.Value(statisticsWorkKey{}).(*statisticsWork); ok {
		w.waitEvents++
	}
}
func waitStage(ctx context.Context, stage string) error {
	if w, ok := ctx.Value(statisticsWorkKey{}).(*statisticsWork); ok && w.stage != nil {
		w.stage(stage)
	}
	return ctx.Err()
}

func countWaitStartInterval(ctx context.Context) {
	if w, ok := ctx.Value(statisticsWorkKey{}).(*statisticsWork); ok {
		w.waitStartIntervals++
	}
}
