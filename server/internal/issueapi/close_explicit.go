package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// CloseExplicit reuses the non-force close workflow after its caller has bound
// an explicit user request to the inspected snapshot.
func (s *Service) CloseExplicit(ctx context.Context, repo string, n int64, reason, updated string, p auth.Principal) (any, error) {
	if problem := auth.Authorize(p, auth.Policy{PC: true, Screen: true, RequireDelegation: true}); problem != nil {
		return nil, problem
	}
	if e := s.o.Project(repo); e != nil {
		return nil, e
	}
	return s.closeConfirmed(ctx, repo, n, Object{"id": operationID("explicit-close:" + via(p) + ":" + repo + fmt.Sprint(n) + ":" + updated + ":" + reason), "reason": "voluntary", "comment": reason, "expectedUpdatedAt": updated}, p)
}
func (s *Service) closeConfirmed(ctx context.Context, repo string, n int64, data Object, p auth.Principal) (any, error) {
	detail, e := s.Detail(ctx, repo, n, false, false)
	if e != nil {
		return nil, e
	}
	if expected := fleet.Text(data["expectedUpdatedAt"]); expected != "" && expected != fleet.Text(detail["updatedAt"]) {
		return nil, fail(409, "確認後に依頼が更新されました。")
	}
	if data["force"] != true {
		if reason := refusal(detail, "close"); reason != "" && detail["state"] != "closed" {
			return nil, fail(409, reason)
		}
	}
	id := fleet.Text(data["id"])
	if id == "" {
		basis := fmt.Sprintf("direct-close:%s#%d:%s:%s:%s", repo, n, fleet.Text(detail["updatedAt"]), fleet.Text(data["reason"]), fleet.Text(data["comment"]))
		if data["routeParsed"] != nil {
			basis += ":" + fmt.Sprint(data["routeParsed"])
		}
		id = operationID(basis)
	}
	if !uuid.MatchString(id) {
		return nil, fail(400, "送信IDが不正です。")
	}
	unlock := lock(&s.locks, "close:"+id)
	defer unlock()
	r, e := s.read(ctx, "close_requests", id)
	if e != nil {
		return nil, e
	}
	if r != nil && fmt.Sprint(r["route"]) != fmt.Sprint(data["routeParsed"]) {
		return nil, fail(409, "依頼IDは別の依頼で使用済みです。")
	}
	if r == nil {
		method, description := "screen", "画面から直接終了"
		if p.Kind == auth.PC {
			method, description = "explicit", "利用者の明示指示で終了"
		}
		r = Object{"id": id, "project": repo, "number": n, "reason": data["reason"], "comment": strings.TrimSpace(fleet.Text(data["comment"])), "description": description, "owner": via(p), "status": "closing", "createdAt": s.now(), "updatedAt": s.now(), "decision": Object{"method": method, "via": via(p), "at": s.now()}, "target": closeTarget(detail), "force": data["force"] == true}
		if data["routeParsed"] != nil {
			r["route"] = data["routeParsed"]
		}
		if e = s.save(ctx, "close_requests", id, r); e != nil {
			return nil, e
		}
	}
	if e = s.executeClose(ctx, r, true); e != nil {
		return nil, e
	}
	result, e := s.Detail(ctx, repo, n, true, false)
	if e != nil {
		return nil, e
	}
	result["alreadyClosed"] = r["status"] == "already_closed"
	result["prClose"] = r["prClose"]
	return result, nil
}
