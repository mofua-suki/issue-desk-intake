package runtimegate

import "context"

// BeginHandoff permanently prevents new work from being admitted by this Gate.
// Work continuing from an active filing or project remains admissible.
func (g *Gate) BeginHandoff() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.handoff {
		g.handoff = true
		g.notify()
	}
}

// WaitIdle waits until all admitted work has been released. Cancellation does
// not change the handoff state or cancel any admitted work.
func (g *Gate) WaitIdle(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.mu.Lock()
		if g.active["ai"] == 0 && g.active["filing"] == 0 && g.active["project"] == 0 {
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
