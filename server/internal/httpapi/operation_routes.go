package httpapi

import (
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/operations"
	"net/http"
)

func (s *WorkerRoutes) operationsRoute(w http.ResponseWriter, r *http.Request) {
	p, ok := s.identity(w, r, auth.Policy{PC: true, RequireDelegation: true})
	if !ok {
		return
	}
	if e := workerMethod(r, "POST"); e != nil {
		workerFailure(w, e)
		return
	}
	body, e := workerBody(r)
	if e != nil {
		workerFailure(w, e)
		return
	}
	service := operations.Service{DB: s.options.AppDB, RuntimeDir: s.options.RuntimeDir, Now: s.options.Now}
	var value any
	if r.URL.Path == "/api/v1/ops-watch/lease" {
		value, e = service.Lease(r.Context(), p.Name, body)
	} else {
		value, e = service.ForceHandoff(r.Context(), p.Name, body)
	}
	if e != nil {
		workerFailure(w, e)
		return
	}
	workerSend(w, value, 200)
}
