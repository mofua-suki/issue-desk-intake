package httpapi

import (
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/notices"
	"issue-desk-intake/server/internal/operations"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var incidentPath = regexp.MustCompile(`^/api/incidents/([a-f0-9-]{36})/(respond|acknowledge)$`)
var reviewAckPath = regexp.MustCompile(`^/api/usage-review/rounds/([1-9][0-9]{0,8})/acknowledge$`)
var opsNoticeID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func (s *WorkerRoutes) noticeControlRoute(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	incident := incidentPath.FindStringSubmatch(path)
	policy := auth.Policy{PC: true, Screen: true, CLI: true, RequireDelegation: true}
	if len(incident) > 0 && incident[2] == "respond" {
		policy.PC = false
	}
	p, ok := s.identity(w, r, policy)
	if !ok {
		return
	}
	ns := notices.Service{DB: s.options.AppDB, Now: s.options.Now}
	ops := operations.Service{DB: s.options.AppDB, PostgreSQL: s.options.DB, Now: s.options.Now}
	var value any
	var e error
	if path == "/api/ops-watch/archive" {
		if e = workerMethod(r, "GET"); e == nil {
			value, e = ns.Archive(r.Context())
		}
	} else {
		if e = workerMethod(r, "POST"); e == nil {
			var body map[string]any
			body, e = workerBody(r)
			if e == nil {
				switch {
				case len(incident) > 0:
					token := ""
					if p.Kind == auth.PC {
						token = p.Name
					}
					value, e = ops.IncidentAction(r.Context(), incident[1], incident[2], token, body)
				case reviewAckPath.MatchString(path):
					parts := reviewAckPath.FindStringSubmatch(path)
					var round int64
					round, e = strconv.ParseInt(parts[1], 10, 64)
					if e == nil {
						value, e = ops.AcknowledgeReview(r.Context(), round)
					}
				case strings.HasPrefix(path, "/api/ops-watch/archive/") && strings.HasSuffix(path, "/restore"):
					id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/ops-watch/archive/"), "/restore")
					value, e = ns.Restore(r.Context(), id, body)
				case strings.HasPrefix(path, "/api/ops-watch/notices/") && strings.HasSuffix(path, "/acknowledge"):
					id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/ops-watch/notices/"), "/acknowledge")
					if len(body) != 0 || !opsNoticeID.MatchString(id) {
						e = contract.NewProblem(400, "通知IDと空の本文を確認してください。", "")
					} else {
						value, e = ns.Acknowledge(r.Context(), id)
					}
				default:
					e = contract.NewProblem(404, "操作が見つかりません。", "")
				}
			}
		}
	}
	if e != nil {
		workerFailure(w, e)
		return
	}
	workerSend(w, value, 200)
}
