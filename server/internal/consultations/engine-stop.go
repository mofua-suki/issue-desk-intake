package consultations

// stopChange commits the persistent fence before the captured execution is
// cancelled. Replays do not cancel a later execution of the same consultation.
type stopChange func() (Record, bool, error)

func (e *Engine) stopExecution(id string, change stopChange) (Record, error) {
	// Start registers executions under controlMu. Do not hold mu during the
	// transaction: another writer may call Busy while holding a PostgreSQL row lock.
	e.controlMu.Lock()
	e.mu.Lock()
	cancel := e.active[id]
	e.mu.Unlock()
	d, changed, err := change()
	if err != nil {
		e.controlMu.Unlock()
		return nil, err
	}
	if changed && cancel != nil {
		cancel()
	}
	e.controlMu.Unlock()
	if changed {
		e.emit(d)
	}
	return d, nil
}
