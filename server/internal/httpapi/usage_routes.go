package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/usage"
)

func (s *WorkerRoutes) usageRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true})
	if !ok {
		return
	}
	if err := workerMethod(r, "POST"); err != nil {
		workerFailure(w, err)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		workerFailure(w, contract.NewProblem(400, "JSONで送ってください。", ""))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024+1))
	if err != nil {
		workerFailure(w, contract.NewProblem(400, "本文を読めません。", ""))
		return
	}
	if len(raw) > 1024*1024 {
		workerFailure(w, contract.NewProblem(400, "本文は1MiB以内です。", ""))
		return
	}
	var records map[string]json.RawMessage
	if json.Unmarshal(raw, &records) != nil || records == nil {
		workerFailure(w, contract.NewProblem(400, "本文は使用量のオブジェクトです。", ""))
		return
	}
	store := usage.Store{DB: s.options.DB}
	counts, err := store.Add(r.Context(), principal.Name, records)
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, counts, 200)
}
