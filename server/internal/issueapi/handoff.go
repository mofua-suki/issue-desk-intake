package issueapi

import "context"

// WaitAccepted waits for background close work already registered with the
// service, including its final persistence. Callers must first stop recovery
// and prevent new requests from registering work. It does not cancel work or
// close the service. Cancelling ctx stops only the wait. If a valid ctx is
// cancelled while waiting, the goroutine in wg.Wait remains until accepted
// work finishes, and then exits. This assumes that work eventually finishes.
func (s *Service) WaitAccepted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ctx.Err()
	}
}
