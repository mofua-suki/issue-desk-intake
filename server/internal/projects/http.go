package projects

import (
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"net/http"
	"strings"
)

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !repoName.MatchString(s.o.Owner) {
		contract.WriteProblem(w, problem(404, "新しいプロジェクトの作成は設定されていません。"))
		return
	}
	principal, p := s.o.Auth.Authenticate(r)
	if p != nil {
		contract.WriteProblem(w, p)
		return
	}
	// 相談・作成・起票は画面だけ。読取と作成への同意（approve）は、運用の代行を許したトークンが利用者のチャットの承認の発言を添えて送れる。
	policy := auth.Policy{Screen: true}
	if r.Method == http.MethodGet || r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/approve") {
		policy = auth.Policy{Screen: true, PC: true, RequireDelegation: true}
	}
	if p = auth.Authorize(principal, policy); p != nil {
		contract.WriteProblem(w, p)
		return
	}
	var data Object
	var value any
	var err error
	status := 200
	if r.Method == http.MethodPost {
		data, p = contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 100000)
		if p != nil {
			contract.WriteProblem(w, p)
			return
		}
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/projects"), "/")
	id, action := "", ""
	if len(parts) > 1 {
		id = parts[1]
	}
	if len(parts) > 2 {
		action = parts[2]
	}
	if len(parts) > 3 || parts[0] != "" || id != "" && !uuidPattern.MatchString(id) {
		contract.WriteProblem(w, problem(404, "プロジェクトの相談が見つかりません。"))
		return
	}
	var presentations []presentationSnapshot
	s.mu.Lock()
	switch {
	case r.Method == "GET" && id == "" && r.URL.Path == "/api/projects":
		var records []Object
		records, err = s.all(r.Context())
		if err == nil {
			presentations = make([]presentationSnapshot, 0, len(records))
			for _, v := range records {
				presentations = append(presentations, s.presentation(v))
			}
		}
	case r.Method == "GET" && id != "" && action == "":
		var record Object
		record, err = s.load(r.Context(), id)
		if err == nil {
			value = s.present(record)
		}
	case r.Method == "POST" && id == "" && r.URL.Path == "/api/projects":
		value, err = s.create(r.Context(), data)
		status = 202
	case r.Method == "POST" && id != "":
		value, err = s.mutate(r.Context(), id, action, data, principal)
		if action == "chat" || action == "create" || action == "file" {
			status = 202
		}
	default:
		err = problem(404, "この操作はありません。")
	}
	s.mu.Unlock()
	if presentations != nil {
		out := make([]Object, 0, len(presentations))
		for _, v := range presentations {
			out = append(out, v.present())
		}
		value = out
	}
	if err != nil {
		var p *contract.Problem
		if !errors.As(err, &p) || p == nil {
			contract.LogFailure("プロジェクトのAPI "+r.URL.Path, err)
			p = problem(503, "プロジェクトの処理を完了できません。")
		}
		contract.WriteProblem(w, p)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
