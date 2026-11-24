package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

var continuationID = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

func (s *Service) ContinueBudget(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	approval, denied := auth.UserApproval(p, data["statement"], data["instructionAt"], s.options.Now(), "相談の続行")
	if denied != nil {
		return nil, denied
	}
	operation := text(data["operationId"])
	if !continuationID.MatchString(operation) {
		return nil, refuse(400, "続行の操作IDが必要です。")
	}
	d, err := s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if err := s.draftAccess(d, p); err != nil {
			return nil, err
		}
		budget := record(d["consultationBudget"])
		for _, entry := range fleet.Items(budget["continuations"]) {
			if record(entry)["id"] == operation {
				return d, nil
			}
		}
		if reason := s.refusal(d, "continue_budget"); reason != "" {
			return nil, refuse(409, reason)
		}
		stopCount, valid := integer(data["stopCount"])
		if d["status"] != "draft" || record(budget["stopped"]) == nil || !valid || stopCount != int64(len(fleet.Items(budget["stops"]))) {
			return nil, refuse(409, "この停止中の相談は続行できません。相談の最新状態を読み直してください。")
		}
		next := clone(d)
		budget = clone(budget)
		added := record(budget["added"])
		calls, _ := integer(added["calls"])
		weighted, _ := integer(added["weighted"])
		at := timestamp(s.options.Now())
		budget["added"] = Record{"calls": calls + 20, "weighted": weighted + 3000000}
		continuation := Record{"id": operation, "at": at, "calls": 20, "weighted": 3000000, "stop": stopCount, "via": "screen"}
		if approval.Statement != "" {
			continuation["via"], continuation["pc"], continuation["statement"], continuation["instructionAt"] = "chat", p.Name, approval.Statement, approval.InstructionAt
		}
		budget["continuations"] = append(fleet.Items(budget["continuations"]), continuation)
		delete(budget, "stopped")
		next["consultationBudget"] = budget
		for k, v := range acceptance(at, "resumed") {
			next[k] = v
		}
		next["status"], next["ready"], next["run"], next["updatedAt"] = "thinking", false, nil, at
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	functions := s.engineSnapshot()
	if functions.Start != nil && d["status"] == "thinking" {
		return functions.Start(ctx, id, d, p)
	}
	return d, nil
}
func (s *Service) continueBudgetRoute(w http.ResponseWriter, r *http.Request) {
	// Authenticate・Authorize・DecodeObject・Context は *contract.Problem を返す。
	// error 型の変数へ入れると nil でも nil と判定されないため、型のまま判定する。
	p, authProblem := s.options.Auth.Authenticate(r)
	if authProblem == nil {
		authProblem = auth.Authorize(p, auth.Policy{Screen: true, PC: true, RequireDelegation: true})
	}
	if authProblem != nil {
		contract.WriteProblem(w, authProblem)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/attention/consultation-budget/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "continue" || !recordID.MatchString(parts[0]) || r.Method != "POST" {
		contract.WriteProblem(w, refuse(404, "相談の続行先が見つかりません。"))
		return
	}
	data, decodeProblem := contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 128<<10)
	if decodeProblem != nil {
		contract.WriteProblem(w, decodeProblem)
		return
	}
	d, err := s.ContinueBudget(r.Context(), parts[0], data, p)
	if err != nil {
		contract.WriteProblem(w, problem(err))
		return
	}
	requestContext, contextProblem := s.options.Auth.Context(r)
	if contextProblem != nil {
		contract.WriteProblem(w, contextProblem)
		return
	}
	respond(w, 200, s.DraftView(d, p, requestContext.Origin))
}
