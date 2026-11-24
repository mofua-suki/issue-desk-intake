package startup

import (
	"context"
	"net/http"
	"sync"

	"issue-desk-intake/server/internal/httpapi"
)

// HandoffHTTP is an opt-in business-request boundary. Wrap the business handler,
// not /healthz. The zero value is ready to use.
type HandoffHTTP struct {
	// Unavailable answers requests that arrive after Begin. Set it before Handler
	// is called; nil rejects them as not accepted.
	Unavailable http.Handler
	mu          sync.Mutex
	stopped     bool
	active      int
	notice      chan struct{}
	idle        chan struct{}
}

func (h *HandoffHTTP) initLocked() {
	if h.notice == nil {
		h.notice = make(chan struct{})
		h.idle = make(chan struct{})
	}
}

func (h *HandoffHTTP) Handler(next http.Handler) http.Handler {
	reject := h.Unavailable
	if reject == nil {
		reject = httpapi.RejectDuringHandoff(next, func() bool { return true })
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.initLocked()
		if h.stopped {
			h.mu.Unlock()
			reject.ServeHTTP(w, r)
			return
		}
		// Registration and cutover share one lock. Track reads too: a nominal
		// read may persist maintenance results or still be using the store.
		h.active++
		notice := h.notice
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			h.active--
			if h.stopped && h.active == 0 {
				close(h.idle)
			}
			h.mu.Unlock()
		}()
		next.ServeHTTP(w, r.WithContext(httpapi.WithHandoff(r.Context(), notice)))
	})
}

// Stopped reports whether Begin closed the boundary.
func (h *HandoffHTTP) Stopped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped
}

// Begin permanently rejects new requests and wakes idle jobs/next requests.
// It never cancels the contexts of requests admitted before the boundary.
func (h *HandoffHTTP) Begin() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.initLocked()
	if !h.stopped {
		h.stopped = true
		close(h.notice)
		if h.active == 0 {
			close(h.idle)
		}
	}
}

// Wait waits for every admitted handler, including its final persistence.
// Begin must precede Wait; cancellation only ends the wait.
func (h *HandoffHTTP) Wait(ctx context.Context) error {
	h.mu.Lock()
	h.initLocked()
	idle := h.idle
	h.mu.Unlock()
	select {
	case <-idle:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
