package httpapi

import (
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"net/http"
)

func (s *WorkerRoutes) hostdRoute(w http.ResponseWriter, r *http.Request, host, operation string) {
	if _, ok := s.identity(w, r, auth.Policy{Hostd: true, Host: host}); !ok {
		return
	}
	service := hostd.Service{DB: s.options.DB, Now: s.options.Now, Assignments: func() (*fleet.Assignments, error) { _, a, e := s.fleet.Load(); return a, e }, Targets: s.fleet.Targets, IssueState: s.options.IssueState}
	var value any
	var err error
	method := "GET"
	if oneHostd(operation, "image-result", "deploy-result", "reader-result", "live-check-result") {
		method = "POST"
	}
	if operation == "hostd-health" && r.Method == "POST" {
		method = "POST"
	}
	if operation == "drain" {
		method = r.Method
		if method != "PUT" && method != "DELETE" {
			err = contract.NewProblem(405, "停止印はPUTまたはDELETEです。", "")
		}
	}
	if err == nil {
		err = workerMethod(r, method)
	}
	body := map[string]any{}
	if err == nil && (method == "POST" || method == "PUT") && r.ContentLength != 0 {
		body, err = workerBodyWithLimit(r, 1024*1024)
	}
	if err == nil {
		switch operation {
		case "desired-image":
			value, err = service.Desired(r.Context(), host, r.URL.Query().Get("flavor"), r.URL.Query().Get("currentRevision"))
		case "image-result":
			value, err = service.ImageResult(r.Context(), host, body)
		case "hostd-health":
			value, err = service.Health(r.Context(), host, body, method == "POST")
		case "drain":
			value, err = service.Drain(r.Context(), host, method == "PUT", body)
		case "deploy-next":
			value, err = service.DeployNext(r.Context(), host)
		case "deploy-result":
			value, err = service.DeployResult(r.Context(), host, body)
		case "reader-next":
			value, err = service.ReaderNext(r.Context(), host)
		case "reader-result":
			value, err = service.ReaderResult(r.Context(), host, body)
		case "live-check-next":
			value, err = service.LiveNext(r.Context(), host)
		case "live-check-result":
			value, err = service.LiveResult(r.Context(), host, body)
		default:
			err = contract.NewProblem(404, "hostd経路が見つかりません。", "")
		}
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}
func oneHostd(v string, values ...string) bool {
	for _, x := range values {
		if v == x {
			return true
		}
	}
	return false
}
