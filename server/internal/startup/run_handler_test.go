package startup

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRunHandoffHandlerConstructedOnceAndReused(t *testing.T) {
	_, handoff, _ := handoffMinimal(t)
	constructed, served := 0, 0
	assets := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusNoContent)
	})
	var boundary atomic.Pointer[handoffHandler]
	boundary.Store(newHandoffHandler(func(next http.Handler) http.Handler {
		constructed++
		return handoff.HTTP.Handler(next)
	}, assets))
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		boundary.Load().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusNoContent {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
	}
	if constructed != 1 || served != 3 {
		t.Fatalf("constructed = %d, served = %d", constructed, served)
	}
	handoff.HTTP.Begin()
	w := httptest.NewRecorder()
	boundary.Load().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusServiceUnavailable || constructed != 1 || served != 3 {
		t.Fatalf("after handoff: status = %d, constructed = %d, served = %d", w.Code, constructed, served)
	}
}
