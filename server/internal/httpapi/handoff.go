package httpapi

import (
	"context"
	"net/http"

	"issue-desk-intake/server/internal/contract"
)

type handoffContextKey struct{}

// WithHandoff attaches a broadcast notification to a request context. Its owner
// closes done to end idle long polls; this does not cancel the request or its
// database operations. Values, deadlines and cancellation remain with ctx.
// Notification creation and attachment to production requests are left to the
// handoff coordinator.
func WithHandoff(ctx context.Context, done <-chan struct{}) context.Context {
	return context.WithValue(ctx, handoffContextKey{}, done)
}

// HandoffDone returns nil when no notification is attached, disabling its select
// case. A nil notification also overrides any notification on a parent context.
func HandoffDone(ctx context.Context) <-chan struct{} {
	done, _ := ctx.Value(handoffContextKey{}).(<-chan struct{})
	return done
}

// RejectDuringHandoff checks the current state for each new business request.
// The caller must provide a concurrency-safe state reader. This component is
// deliberately not registered on the production routes yet.
func RejectDuringHandoff(next http.Handler, handingOff func() bool) http.Handler {
	if handingOff == nil {
		panic("handoff state reader is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handingOff() {
			contract.WriteNotAccepted(w, "受付を交代しています。少し待って同じ要求を再送してください。")
			return
		}
		next.ServeHTTP(w, r)
	})
}
