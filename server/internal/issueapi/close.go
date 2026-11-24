package issueapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

func closeReason(v any, unknown bool) bool {
	return oneOf(fleet.Text(v), "voluntary", "resolved", "duplicate", "other_route") || unknown && v == "unknown"
}

var closeRoutePattern = regexp.MustCompile(`^([^/#\s]+/[^/#\s]+)#([1-9][0-9]*)$`)

func parseCloseRoute(data Object) (Object, error) {
	value, present := data["route"]
	if !present {
		return nil, nil
	}
	text, ok := value.(string)
	match := closeRoutePattern.FindStringSubmatch(text)
	if !ok || fleet.Text(data["reason"]) != "other_route" || match == nil {
		return nil, fail(400, "route は other_route の owner/repo#PR番号で指定してください。")
	}
	n, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil || n > 9007199254740991 {
		return nil, fail(400, "route の PR番号が不正です。")
	}
	return Object{"repo": match[1], "pr": n}, nil
}
func (s *Service) closeRoute(ctx context.Context, method, path string, data Object, p auth.Principal) (any, error) {
	if path == "/api/close-reason" && method == "POST" {
		return s.saveCloseReason(ctx, data, p)
	}
	if (path == "/api/issues/close" || path == "/api/issue/close") && method == "POST" {
		return s.closeIssue(ctx, path, data, p)
	}
	if path == "/api/close-requests" && method == "GET" {
		return s.listOpenCloseRequests(ctx, data, p)
	}
	return s.closeRequestAction(ctx, method, path, data, p)
}

// saveCloseReason はクローズ済みの依頼の終了理由と別経路の記録を保存する。
func (s *Service) saveCloseReason(ctx context.Context, data Object, p auth.Principal) (any, error) {
	if p.Kind == auth.PC && !p.Delegated {
		return nil, fail(403, "運用代行が許されていません。")
	}
	route, routeErr := parseCloseRoute(data)
	if routeErr != nil {
		return nil, routeErr
	}
	repo, n, e := s.target(ctx, data)
	if e != nil {
		return nil, e
	}
	detail, e := s.Detail(ctx, repo, n, false, false)
	if e != nil {
		return nil, e
	}
	if detail["status"] != "closed" || !closeReason(data["reason"], true) {
		return nil, fail(409, "クローズ済みの依頼の終了理由を指定してください。")
	}
	if data["closedAt"] != nil && data["closedAt"] != detail["closedAt"] {
		return nil, fail(409, "閉じた日時が変わっています。")
	}
	record := Object{"project": repo, "number": n, "reason": data["reason"], "closedAt": detail["closedAt"], "savedAt": s.now()}
	if route != nil {
		record["route"] = route
	}
	if e = s.save(ctx, "close_reasons", fmt.Sprintf("%s#%d", repo, n), record); e != nil {
		return nil, e
	}
	return s.Detail(ctx, repo, n, p.Kind != auth.PC, false)
}

// closeIssue は依頼を閉じる指定を検査し、PCからはクローズ依頼として、画面からは確定として受け付ける。
func (s *Service) closeIssue(ctx context.Context, path string, data Object, p auth.Principal) (any, error) {
	delete(data, "routeParsed")
	if path == "/api/issue/close" && data["force"] == true {
		return nil, fail(400, "画面の終了操作はforceを受け付けません。")
	}
	if path == "/api/issue/close" && p.Kind != auth.Screen {
		return nil, fail(403, "この操作は利用者の画面から行ってください。")
	}
	route, routeErr := parseCloseRoute(data)
	if routeErr != nil {
		return nil, routeErr
	}
	repo, n, e := s.target(ctx, data)
	if e != nil {
		return nil, e
	}
	if !closeReason(data["reason"], false) || contract.UTF16Length(fleet.Text(data["comment"])) > 20000 {
		return nil, fail(400, "終了理由とコメントを確認してください。")
	}
	if route != nil {
		data["routeParsed"] = route
	}
	if p.Kind == auth.PC {
		return s.requestClose(ctx, repo, n, data, p)
	}
	return s.closeConfirmed(ctx, repo, n, data, p)
}

// listOpenCloseRequests は結果の出ていないクローズ依頼を、プロジェクトと状態で絞ってページに分けて返す。
func (s *Service) listOpenCloseRequests(ctx context.Context, data Object, p auth.Principal) (any, error) {
	if p.Kind == auth.PC && !p.Delegated {
		return nil, fail(403, "運用代行が許されていません。")
	}
	query := url.Values{}
	for _, key := range []string{"page", "limit"} {
		if value, ok := data[key]; ok {
			query[key] = []string{fleet.Text(value)}
		}
	}
	pagination, pageErr := contract.ParsePagination(query)
	if pageErr != nil {
		return nil, pageErr
	}
	project, status := fleet.Text(data["project"]), fleet.Text(data["status"])
	rows, e := s.closeRecords(ctx, closeOpenFilter())
	if e != nil {
		return nil, e
	}
	out := []Object{}
	for _, r := range rows {
		if closeRequestOpen(r) && (project == "" || fleet.Text(r["project"]) == project) && (status == "" || fleet.Text(r["status"]) == status) {
			out = append(out, closeView(r, true))
		}
	}
	return contract.Paginate(out, pagination), nil
}

// closeRequestAction はクローズ依頼1件を読み、閉じる・閉じないの判断を受け付ける。
func (s *Service) closeRequestAction(ctx context.Context, method, path string, data Object, p auth.Principal) (any, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/close-requests/"), "/")
	if !strings.HasPrefix(path, "/api/close-requests/") || len(parts) > 2 || !uuid.MatchString(parts[0]) {
		return nil, fail(404, "クローズ依頼が見つかりません。")
	}
	id := parts[0]
	unlock := lock(&s.locks, "close:"+id)
	defer unlock()
	r, e := s.read(ctx, "close_requests", id)
	if e != nil {
		return nil, e
	}
	if r == nil || p.Kind == auth.PC && !p.Delegated && r["owner"] != "token:"+p.TokenID {
		return nil, fail(404, "クローズ依頼が見つかりません。")
	}
	if e = s.o.Project(fleet.Text(r["project"])); e != nil {
		return nil, e
	}
	full := p.Kind != auth.PC || p.Delegated
	if method == "GET" && len(parts) == 1 {
		return closeView(r, full), nil
	}
	if method != "POST" || len(parts) != 2 || !oneOf(parts[1], "close", "keep") {
		return nil, fail(404, "クローズ依頼の操作が見つかりません。")
	}
	if p.Kind == auth.PC && !p.Delegated {
		return nil, fail(403, "運用代行が許されていません。")
	}
	if r["status"] != "confirm" {
		return nil, fail(409, "確認待ちの依頼ではありません。")
	}
	if parts[1] == "keep" {
		r["status"] = "declined"
		r["decision"] = Object{"method": map[bool]string{true: "screen", false: "delegate"}[p.Kind == auth.Screen], "via": via(p), "keep": true, "at": s.now()}
		r["message"] = "閉じない判断を保存しました。"
		e = s.saveClose(ctx, r)
		return closeView(r, true), e
	}
	if data["fingerprint"] != r["fingerprint"] {
		return nil, fail(409, "表示の後に確認内容が変わっています。")
	}
	r["status"] = "closing"
	r["decision"] = Object{"method": map[bool]string{true: "screen", false: "delegate"}[p.Kind == auth.Screen], "via": via(p), "at": s.now()}
	if e = s.saveClose(ctx, r); e != nil {
		return nil, e
	}
	e = s.executeClose(ctx, r, false)
	return closeView(r, true), e
}

func closeTarget(d Object) Object {
	return Object{"project": d["project"], "number": d["number"], "desk": d["desk"], "title": d["title"], "body": d["body"], "bodySha256": digest(d["body"]), "host": d["host"], "url": d["url"], "state": d["state"]}
}
func closeView(r Object, full bool) Object {
	out := clone(r)
	if !full {
		for _, k := range []string{"owner", "tokenId", "description", "comment", "material", "fingerprint", "baseline", "uploads"} {
			delete(out, k)
		}
	} else {
		actions := []string{}
		if r["status"] == "confirm" {
			actions = []string{"close", "keep"}
		}
		out["actions"] = actions
	}
	return out
}
func (s *Service) requestClose(ctx context.Context, repo string, n int64, data Object, p auth.Principal) (Object, error) {
	id := fleet.Text(data["id"])
	if !uuid.MatchString(id) || strings.TrimSpace(fleet.Text(data["description"])) == "" || contract.UTF16Length(fleet.Text(data["description"])) > 2000 {
		return nil, fail(400, "クローズ依頼のIDと説明を指定してください。")
	}
	unlock := lock(&s.locks, fmt.Sprintf("request-close:%s#%d", repo, n))
	defer unlock()
	owner := "token:" + p.TokenID
	r, e := s.read(ctx, "close_requests", id)
	if e != nil {
		return nil, e
	}
	if r != nil {
		if !sameCloseRequest(r, data, repo, n, owner) {
			return nil, fail(409, "依頼IDは別の依頼で使用済みです。")
		}
		return closeView(r, p.Delegated), nil
	}
	detail, e := s.checkCloseAllowed(ctx, repo, n, p)
	if e != nil {
		return nil, e
	}
	baseline, e := s.closeTimelineBaseline(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	r = Object{"id": id, "project": repo, "number": n, "reason": data["reason"], "description": strings.TrimSpace(fleet.Text(data["description"])), "comment": strings.TrimSpace(fleet.Text(data["comment"])), "owner": owner, "tokenId": p.TokenID, "pc": p.Name, "status": "reviewing", "baseline": baseline, "target": closeTarget(detail), "createdAt": s.now(), "updatedAt": s.now()}
	if data["routeParsed"] != nil {
		r["route"] = data["routeParsed"]
	}
	if detail["state"] == "closed" {
		r["status"] = "already_closed"
		r["message"] = "対象のIssueは既に閉じています。"
	}
	if e = s.saveClose(ctx, r); e != nil {
		return nil, e
	}
	if r["status"] == "reviewing" {
		s.launchClose(id)
	}
	return closeView(r, p.Delegated), nil
}

// sameCloseRequest は保存済みのクローズ依頼が、同じ送り手・対象・内容の送り直しかを判定する。
func sameCloseRequest(r, data Object, repo string, n int64, owner string) bool {
	return r["owner"] == owner && r["project"] == repo && fmt.Sprint(r["number"]) == fmt.Sprint(n) && r["reason"] == data["reason"] && r["description"] == strings.TrimSpace(fleet.Text(data["description"])) && r["comment"] == strings.TrimSpace(fleet.Text(data["comment"])) && fmt.Sprint(r["route"]) == fmt.Sprint(data["routeParsed"])
}

// checkCloseAllowed は依頼を閉じてよいか（担当のPC・運用代行、閉じられない理由、結果の出ていないクローズ依頼）を確かめ、依頼の詳細を返す。
func (s *Service) checkCloseAllowed(ctx context.Context, repo string, n int64, p auth.Principal) (Object, error) {
	detail, e := s.Detail(ctx, repo, n, false, false)
	if e != nil {
		return nil, e
	}
	config, e := s.configuration()
	if e != nil {
		return nil, e
	}
	if detail["host"] != fleet.Text(config["workerId"]) && !p.Delegated && detail["state"] != "closed" {
		return nil, fail(403, "別のPCの依頼を閉じるには運用代行の許可が必要です。")
	}
	if reason := s.closeBlocker(ctx, detail); reason != "" && detail["state"] != "closed" {
		return nil, fail(409, reason)
	}
	all, e := s.closeRecords(ctx, closeOpenFilter())
	if e != nil {
		return nil, e
	}
	for _, old := range all {
		if old["project"] == repo && fmt.Sprint(old["number"]) == fmt.Sprint(n) && oneOf(fleet.Text(old["status"]), "reviewing", "confirm", "closing") {
			return nil, fail(409, "このIssueには結果の出ていないクローズ依頼があります。")
		}
	}
	return detail, nil
}

// closeTimelineBaseline は依頼の出来事の最新の番号を、クローズの照合の基準として返す。
func (s *Service) closeTimelineBaseline(ctx context.Context, repo string, n int64) (int64, error) {
	events, e := s.timeline(ctx, repo, n)
	if e != nil {
		return 0, e
	}
	baseline := int64(0)
	for _, event := range events {
		v, _ := number(event["id"])
		if v > baseline {
			baseline = v
		}
	}
	return baseline, nil
}
func (s *Service) timeline(ctx context.Context, repo string, n int64) ([]Object, error) {
	return s.o.Gitea.Pages(ctx, fmt.Sprintf("/repos/%s/issues/%d/timeline", repo, n))
}
func (s *Service) closeBlocker(ctx context.Context, d Object) string {
	return s.closeBlockerFor(ctx, d, true)
}

// closeBlockerFor の checkBusy=false は、呼び手が issue のロックを既に持っているときに使う。
func (s *Service) closeBlockerFor(ctx context.Context, d Object, checkBusy bool) string {
	status := fleet.Text(d["status"])
	if !oneOf(status, "waiting", "queued", "project_waiting", "scope_waiting", "failed", "stopped", "review_held", "dependency_waiting", "prerequisite_waiting", "untracked", "completed") {
		return "この依頼は処理中または開始待ちのため閉じられません。"
	}
	repo := fleet.Text(d["project"])
	n, _ := number(d["number"])
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil || job != nil && job.Status == "running" {
		return "ジョブが実行中か、現在の状態を確認できません。"
	}
	// 操作の送信は issue のロックを持ったまま行う。ロックが空いているのに結果の無い操作は、
	// 失敗や再起動で残っただけなので、クローズを止めない（結果は終了コメントに残す）。
	// 読取の後にロックを見る。先に見ると、その間に始まった送信の行を残っただけと取り違える。
	if _, e := s.unresolvedActions(ctx, repo, n); e != nil {
		return "送信中の操作を確認できません。"
	}
	if checkBusy && s.issueBusy(repo, n) {
		return "この依頼への操作を送信中です。"
	}
	return ""
}

// issueBusy は、この依頼への操作（返信・再試行・優先度など）を今まさに送っているかを返す。
func (s *Service) issueBusy(repo string, n int64) bool {
	v, _ := s.locks.LoadOrStore(fmt.Sprintf("issue:%s#%d", repo, n), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}

// unresolvedActions は、送信を始めたが適用・投稿の結果を記録できていない操作を返す。
func (s *Service) unresolvedActions(ctx context.Context, repo string, n int64) ([]any, error) {
	rows, e := s.o.AppDB.QueryContext(ctx, bindSQL("SELECT id, json FROM actions WHERE "+jsonText("json", "$.payload.repo")+"=? AND "+safeJSONNumber("json", "$.payload.number")+"=? ORDER BY id"), repo, n)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, raw string
		if e = rows.Scan(&id, &raw); e != nil {
			return nil, e
		}
		r := Object{}
		if e = json.Unmarshal([]byte(raw), &r); e != nil {
			return nil, e
		}
		if r["attempted"] == true && r["applied"] != true && r["comment"] == nil {
			item := Object{"id": id, "action": fleet.Text(fleet.ObjectOrEmpty(r["payload"])["action"])}
			if r["postedComment"] != nil {
				// 画像付き返信は投稿済みで、画像の転送と本文の差し替えだけが残っている。
				item["posted"] = true
			}
			out = append(out, item)
		}
	}
	return out, rows.Err()
}
