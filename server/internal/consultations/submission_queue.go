package consultations

// SubmitQueued registers accepted business work independently of the periodic
// scanner. Quiesce and Close include its final save, just like automatic filing.
func (e *Engine) SubmitQueued(d Record) {
	id := text(d["id"])
	if e.options.Submit == nil || !is(text(d["status"]), "auto_ready", "submitting", "submit_unknown") {
		return
	}
	e.mu.Lock()
	if e.receiptStoppingLocked() || e.filings[id] {
		e.mu.Unlock()
		return
	}
	e.filings[id] = true
	e.turns++
	e.wg.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		defer e.endTurn()
		defer func() { e.mu.Lock(); delete(e.filings, id); e.mu.Unlock() }()
		result, err := e.options.Submit(e.ctx, d)
		if err != nil {
			e.reportStop(e.ctx, id, d, "受理済みの起票", err)
			e.SaveAutoSubmitFailure(e.ctx, id, d, err)
		} else if result != nil {
			e.emit(result)
		}
	}()
}
