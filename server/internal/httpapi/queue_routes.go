package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/dispatch"
	"issue-desk-intake/server/internal/fleet"
)

func (s *WorkerRoutes) commandRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true})
	if !ok {
		return
	}
	queue := dispatch.Queue{DB: s.options.DB}
	var value any
	var err error
	if r.URL.Path == "/api/v1/commands" {
		err = workerMethod(r, "GET")
		if raw, exists := r.URL.Query()["host"]; err == nil && exists && (len(raw) != 1 || raw[0] != principal.Name) {
			err = contract.NewProblem(403, "ほかのPC宛ての指示は読めません。", "")
		}
		after := int64(0)
		if err == nil && r.URL.Query().Get("after") != "" {
			var number float64
			number, err = strconv.ParseFloat(r.URL.Query().Get("after"), 64)
			if err == nil {
				after, err = workerInteger(number, 0, 9007199254740991)
			} else {
				err = contract.NewProblem(400, "afterは0以上の整数です。", "")
			}
		}
		if err == nil {
			var commands []*dispatch.Command
			commands, err = queue.Commands(r.Context(), principal.Name, after)
			next := after
			if len(commands) > 0 {
				next = commands[len(commands)-1].Seq
			}
			value = map[string]any{"commands": commands, "next": next}
		}
	} else {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/commands/"), "/")
		if len(parts) != 2 || parts[1] != "ack" {
			err = contract.NewProblem(404, "指示が見つかりません。", "")
		} else if err = workerMethod(r, "POST"); err == nil {
			var body map[string]any
			body, err = workerBodyWithLimit(r, 64*1024)
			if err == nil {
				if body["result"] != nil && fleet.Object(body["result"]) == nil {
					err = contract.NewProblem(400, "resultはオブジェクトです。", "")
				} else {
					var command *dispatch.Command
					command, err = queue.AckCommand(r.Context(), parts[0], principal.Name, fleet.Text(body["status"]), fleet.Object(body["result"]))
					value = map[string]any{"command": command}
				}
			}
		}
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}

func (s *WorkerRoutes) requestRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true})
	if !ok {
		return
	}
	queue := dispatch.Queue{DB: s.options.DB}
	var value any
	var err error
	status := 200
	if r.URL.Path == "/api/v1/requests" {
		value, status, err = s.submitRequest(r, queue, principal)
	} else if r.URL.Path == "/api/v1/requests/next" {
		value, err = s.nextRequest(r, queue, principal)
	} else {
		value, err = s.requestByID(r, queue, principal)
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, status)
}

// submitRequest はPCからの依頼を検査して登録する。新しく登録したときは201を返す。
func (s *WorkerRoutes) submitRequest(r *http.Request, queue dispatch.Queue, principal auth.Principal) (any, int, error) {
	var value any
	status := 200
	err := workerMethod(r, "POST")
	var body map[string]any
	if err == nil {
		body, err = workerBodyWithLimit(r, 1024*1024, "id", "kind", "repo", "payload")
	}
	if err == nil {
		request := dispatch.Request{ID: fleet.Text(body["id"]), Kind: fleet.Text(body["kind"]), Repo: fleet.Text(body["repo"]), Payload: fleet.Object(body["payload"]), Sender: principal.Name}
		if request.Kind == "merge_manual" || request.Kind == "pr_create" {
			// 権限保護対象・データ削除SQLのマージに要る印。送り手の値は信じず、運用代行を許したトークンの承認にだけ付ける。
			// pr_create の承認はPR番号が決まる前の事前承認で、merge_manual の approvalRequest から使う。
			delete(request.Payload, "approvalDelegated")
			if principal.Delegated && strings.TrimSpace(fleet.Text(request.Payload["approval"])) != "" {
				request.Payload["approvalDelegated"] = true
			}
			// 事前承認を使うマージ依頼も、送る時点で運用代行が許されているトークンからだけ受け付ける（作成後に代行を外したら使えない）。
			if request.Kind == "merge_manual" && fleet.Text(request.Payload["approvalRequest"]) != "" && !principal.Delegated {
				err = contract.NewProblem(403, "事前承認（approvalRequest）を使うマージは、運用代行を許したトークンからだけ依頼できます。", "")
			}
		}
		if request.Kind == "native_test" {
			var document fleet.Document
			var assignments *fleet.Assignments
			document, assignments, err = s.fleet.Load()
			if err == nil && (fleet.NativeTestTarget(document, assignments, principal.Name, request.Repo) == "" || fleet.NativeTestTarget(document, assignments, principal.Name, request.Repo) != fleet.Text(request.Payload["host"])) {
				err = contract.NewProblem(403, "Mac検証は実装担当から登録されたテスト担当にだけ依頼できます。", "")
			}
		}
		if err != nil {
			return value, status, err
		}
		if request.Kind == "full_test" {
			err = contract.NewProblem(403, "mainの確認は受付が依頼します。", "")
		} else {
			if request.Kind == "pr_create" && request.Payload["patch"] != nil {
				err = s.patchRepositoryRegistered(request.Repo)
			}
			if err == nil {
				var saved *dispatch.Request
				var created bool
				saved, created, err = queue.Submit(r.Context(), request)
				value = map[string]any{"request": saved}
				if created {
					status = 201
				}
			}
		}
	}
	return value, status, err
}

// patchRepositoryRegistered はパッチで作るPRのリポジトリが、受付のPCの設定のどれかに登録されているかを確かめる。
func (s *WorkerRoutes) patchRepositoryRegistered(repo string) error {
	document, _, loadErr := s.fleet.Load()
	if loadErr != nil {
		return loadErr
	}
	allowed := false
	configs := []map[string]any{document.Defaults}
	for _, config := range document.Hosts {
		configs = append(configs, config)
	}
	for _, config := range configs {
		for _, item := range fleet.List(config["repositories"]) {
			if fleet.Object(item)["name"] == repo {
				allowed = true
			}
		}
	}
	if !allowed {
		return contract.NewProblem(403, "パッチのリポジトリは受付に登録されていません。", "")
	}
	return nil
}

// nextRequest は担当表に従い、このPCが引き取れる次の依頼を渡す。
func (s *WorkerRoutes) nextRequest(r *http.Request, queue dispatch.Queue, principal auth.Principal) (any, error) {
	var value any
	err := workerMethod(r, "GET")
	if err == nil {
		var a *fleet.Assignments
		var document fleet.Document
		document, a, err = s.fleet.Load()
		if err == nil {
			if a.Hosts[principal.Name] == nil {
				err = contract.NewProblem(403, "担当表にないPCは依頼を引き取れません。", "")
			} else {
				kind := r.URL.Query().Get("kind")
				if a.Hosts[principal.Name].Role == "test-only" && kind != "native_test" {
					return nil, contract.NewProblem(403, "テスト専用PCは実装やPR操作を引き取れません。", "")
				}
				var home, impl []string
				home, impl, err = nextRequestRepositories(a, principal.Name, kind, r.URL.Query()["repo"])
				if err == nil {
					var request *dispatch.Request
					if kind == "native_test" {
						request, err = queue.NextNative(r.Context(), principal.Name, home, nativeTestSenders(document, a, principal.Name))
					} else {
						request, err = queue.Next(r.Context(), principal.Name, kind, home, impl)
					}
					value = map[string]any{"request": request}
				}
			}
		}
	}
	return value, err
}

// nextRequestRepositories は指定のリポジトリを、このPCが担当（またはMac検証の担当）のものと、実装を引き受けられるものに分ける。
func nextRequestRepositories(a *fleet.Assignments, host, kind string, repos []string) ([]string, []string, error) {
	var err error
	if len(repos) > 200 {
		err = contract.NewProblem(400, "repoが多すぎます。", "")
	}
	home, impl := []string{}, []string{}
	for _, repo := range repos {
		if !fleet.ValidRepo(repo) {
			err = contract.NewProblem(400, "repoの形が違います。", "")
			break
		}
		if kind == "native_test" {
			if a.CanTest(host, repo) {
				home = append(home, repo)
			}
		} else if a.Output["repositories"].(map[string]any)[repo] == host {
			home = append(home, repo)
		} else {
			for _, candidate := range a.Implementers[repo] {
				if candidate == host {
					impl = append(impl, repo)
				}
			}
		}
	}
	return home, impl, err
}

// nativeTestSenders はこのPCをMac検証の担当にしている実装担当のPCを、リポジトリごとに集める。
func nativeTestSenders(document fleet.Document, a *fleet.Assignments, host string) map[string][]string {
	senders := map[string][]string{}
	for _, repo := range a.Hosts[host].TestRepositories {
		for _, sender := range a.Implementers[repo] {
			if fleet.NativeTestTarget(document, a, sender, repo) == host {
				senders[repo] = append(senders[repo], sender)
			}
		}
	}
	return senders
}

// requestByID は依頼1件を読む、または結果の報告・取消などの操作を受け付ける。
func (s *WorkerRoutes) requestByID(r *http.Request, queue dispatch.Queue, principal auth.Principal) (any, error) {
	var value any
	var err error
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/requests/"), "/")
	if len(parts) < 1 || len(parts) > 2 || !dispatch.RequestID.MatchString(parts[0]) {
		err = contract.NewProblem(404, "依頼が見つかりません。", "")
	} else if accessErr := s.nativeRequestAccess(r, queue, parts[0], principal.Name); accessErr != nil {
		err = accessErr
	} else if len(parts) == 1 {
		err = workerMethod(r, "GET")
		if err == nil {
			var request *dispatch.Request
			request, err = queue.GetRequest(r.Context(), parts[0])
			if err == nil && request == nil {
				err = contract.NewProblem(404, "依頼が見つかりません。", "")
			}
			value = map[string]any{"request": request}
		}
	} else {
		err = workerMethod(r, "POST")
		var body map[string]any
		if err == nil && parts[1] == "result" {
			body, err = workerBodyWithLimit(r, 1024*1024)
		}
		if err == nil {
			var request *dispatch.Request
			request, err = queue.RequestAction(r.Context(), parts[0], principal.Name, parts[1], fleet.Object(body["result"]))
			value = map[string]any{"request": request}
			if parts[1] == "cancel" {
				value.(map[string]any)["cancelled"] = request != nil
			}
		}
	}
	return value, err
}
