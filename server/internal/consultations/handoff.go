package consultations

import "context"

// Quiesce prevents further turns and waits for started turns and their saves.
// A started turn may finish its automatic Submit; that submission and its save
// are included in the wait. No new turn starts after the fence.
// Its context limits only this wait; it never cancels a running turn.
func (e *Engine) Quiesce(ctx context.Context) error {
	e.mu.Lock()
	if !e.quiescing {
		e.quiescing = true
		close(e.quiesced)
		e.drained = make(chan struct{})
		if e.turns == 0 {
			close(e.drained)
		}
	}
	done := e.drained
	e.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) endTurn() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns--
	if e.quiescing && e.turns == 0 {
		close(e.drained)
	}
}
