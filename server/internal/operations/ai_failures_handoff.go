package operations

// StopNotifications prevents new notification reservations and waits for all
// previously registered sends, including their result persistence. Observations
// and alerts remain available. It does not cancel sends already in progress.
// An incident detected after stopping remains unnotified in the saved record;
// a new instance loading it will notify on the next failure in the same scope.
func (s *AIFailures) StopNotifications() {
	s.mu.Lock()
	s.notificationsStopped = true
	s.mu.Unlock()
	s.wg.Wait()
}
