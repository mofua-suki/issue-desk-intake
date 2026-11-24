package projects

import (
	"context"
	"time"
)

// RunRecovery resumes durable operations and periodically rechecks placement waits.
// Stopping this loop does not close the service or cancel accepted work.
func (s *Service) RunRecovery(ctx context.Context) error {
	return s.runRecovery(ctx, false, nil)
}

func (s *Service) runRecovery(ctx context.Context, closeOnCancel bool, ready func()) error {
	if e := s.Recover(ctx); e != nil {
		return e
	}
	if ready != nil {
		ready()
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if closeOnCancel {
				s.Close()
			}
			return nil
		case <-s.ctx.Done():
			return nil
		case <-ticker.C:
			if e := s.Recover(ctx); e != nil {
				return e
			}
		}
	}
}
