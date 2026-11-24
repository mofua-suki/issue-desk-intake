package projects

import "context"

// Drain establishes the caller's start fence, stops and joins recovery, then
// waits for accepted work. stopRecovery must not return before recovery exits;
// preventStart must also fence other paths that can register project work.
// Cancellation only stops the wait and never removes the fence.
func (s *Service) Drain(ctx context.Context, preventStart func(), stopRecovery func()) error {
	preventStart()
	stopRecovery()
	return s.WaitAccepted(ctx)
}

// WaitAccepted waits for accepted work, including continuations, persistence and
// execution-slot release. Before calling it, the caller must prevent new work
// from starting and wait for the recovery loop to exit. Cancelling ctx only
// stops this wait; it does not cancel accepted work.
func (s *Service) WaitAccepted(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
