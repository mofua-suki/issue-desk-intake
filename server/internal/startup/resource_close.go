package startup

import (
	"errors"
	"sync"
)

// HandoffLease is implemented by leadership.Lease, whose Release also waits for
// its watcher and is idempotent with the normal Run defer.
type HandoffLease interface {
	Release()
}

// handoffResources is used only after all request and service completion
// barriers. App.Close's existing closeOnce is shared with ordinary shutdown;
// no copy of App or of its synchronization primitives may be made here.
type handoffResources struct {
	app         *App
	lease       HandoffLease
	releaseOnce sync.Once
}

// closeResources shares the complete shutdown sequence and its result with
// ordinary shutdown. A previous failure remains a failure for handoff.
func (a *App) closeResources() error {
	a.closeOnce.Do(func() {
		a.providers.close()
		a.exclusiveMu.Lock()
		a.exclusiveClosed = true
		a.exclusiveMu.Unlock()
		a.cancel()
		if a.exclusiveCancel != nil {
			a.exclusiveCancel()
		}
		a.CancelBackground()
		if a.Live != nil {
			a.Live.Close()
		}
		if a.Engine != nil {
			a.Engine.Close()
		}
		if a.Projects != nil {
			a.Projects.Close()
		}
		if a.Issues != nil {
			a.Issues.Close()
		}
		if a.AIFailures != nil {
			a.AIFailures.Close()
		}
		a.wg.Wait()
		if a.Gate != nil {
			a.closeErr = errors.Join(a.closeErr, a.Gate.Close())
		}
		if a.SQLite != nil {
			a.closeErr = errors.Join(a.closeErr, a.SQLite.Close())
		}
	})
	return a.closeErr
}

func (r *handoffResources) close() error {
	return r.app.closeResources()
}

func (r *handoffResources) release() {
	r.releaseOnce.Do(r.lease.Release)
}
