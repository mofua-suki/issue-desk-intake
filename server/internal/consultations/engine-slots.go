package consultations

import (
	"context"
	"sync"
)

type slotKey struct{}
type turnSlot struct {
	slots chan struct{}
	mu    sync.Mutex
	held  bool
}

func (s *turnSlot) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		<-s.slots
		s.held = false
	}
}
func (s *turnSlot) acquire(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		return nil
	}
	select {
	case s.slots <- struct{}{}:
		s.held = true
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A quota wait keeps the durable consultation active, while releasing the scarce AI slot.
func WithoutAISlot(ctx context.Context, wait func(context.Context) error) error {
	slot, _ := ctx.Value(slotKey{}).(*turnSlot)
	if slot == nil {
		return wait(ctx)
	}
	slot.release()
	if err := wait(ctx); err != nil {
		return err
	}
	return slot.acquire(ctx)
}
func (e *Engine) Shutdown(ctx context.Context) {
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		e.cancel()
	case <-ctx.Done():
		e.cancel()
		<-done
	}
}
