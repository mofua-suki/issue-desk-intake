package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"net/http"
	"strings"
)

func (s *WorkerRoutes) hostsViewRoute(w http.ResponseWriter, r *http.Request) {
	policy := auth.Policy{PC: true, Screen: true, CLI: true}
	if strings.HasSuffix(r.URL.Path, "/hostd-command") {
		policy.RequireDelegation = true
		// 受付と同じPC上のスクリプトは、利用者が登録した許可（入れ直しの承認）がある install の発行だけできる。
		policy.CLI = r.Method == "POST"
	}
	principal, ok := s.identity(w, r, policy)
	if !ok {
		return
	}
	var value any
	var err error
	if r.URL.Path == "/api/hosts" {
		err = workerMethod(r, "GET")
		if err == nil {
			_, a, e := s.fleet.Load()
			err = e
			if err == nil {
				value, err = s.hosts.View(r.Context(), a)
			}
		}
	} else {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/hosts/"), "/")
		if len(parts) != 2 || !fleet.ValidHost(parts[0]) {
			err = contract.NewProblem(404, "PCが見つかりません。", "")
		} else {
			host, operation := parts[0], parts[1]
			by := "screen"
			if principal.Kind == auth.PC {
				by = "token:" + principal.Name
			} else if principal.Kind == auth.CLI {
				by = "受付と同じPC上のスクリプト"
			}
			switch operation {
			case "hostd-command":
				if r.Method != "GET" && r.Method != "POST" {
					err = contract.NewProblem(405, "指示はGETまたはPOSTです。", "")
				} else {
					var body map[string]any
					if r.Method == "POST" {
						body, err = workerBody(r)
					}
					if err == nil && principal.Kind == auth.CLI {
						by, err = s.hostdRetryGrant(r.Context(), host, body)
					}
					if err == nil {
						value, err = (&hostd.Service{DB: s.options.DB, Now: s.options.Now}).Commands(r.Context(), host, by, body, r.Method == "POST")
					}
				}
			case "drain":
				if r.Method != "POST" && r.Method != "DELETE" {
					err = contract.NewProblem(405, "停止印はPOSTまたはDELETEです。", "")
				} else {
					body := map[string]any{}
					if r.Method == "POST" {
						body, err = workerBody(r, "until", "reason", "indefinite")
					}
					if err == nil {
						value, err = s.hosts.Drain(r.Context(), host, by, body, r.Method == "POST")
					}
				}
			default:
				err = contract.NewProblem(404, "PCの経路が見つかりません。", "")
			}
		}
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}

// hostdRetryGrant は、受付と同じPC上のスクリプトによる install の発行を、有効な許可（hostd-repair の
// <PC名>:retry-failed-image）がある場合だけ通し、許可のIDを含む発行者名を返す。
func (s *WorkerRoutes) hostdRetryGrant(ctx context.Context, host string, body map[string]any) (string, error) {
	if fleet.Text(body["kind"]) != "install" {
		return "", contract.NewProblem(403, "この認証方法ではinstall以外の指示は発行できません。", "")
	}
	var id string
	err := s.options.DB.QueryRowContext(ctx, "SELECT id FROM permission_grants WHERE kind='hostd-repair' AND lower(target)=lower($1) AND revoked_at IS NULL AND expires_at>$2 ORDER BY created_at LIMIT 1", host+":retry-failed-image", s.options.Now()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", contract.NewProblem(403, "入れ直しの許可（"+host+":retry-failed-image）がありません。", "")
	}
	if err != nil {
		return "", err
	}
	return "grant:" + id, nil
}
