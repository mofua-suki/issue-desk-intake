package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func budgetContinueHTTP(t *testing.T, s *Service, token, contentType, body string) (int, Record) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://localhost:8787/api/attention/consultation-budget/"+todoID+"/continue", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	s.Register(mux)
	mux.ServeHTTP(w, r)
	var out Record
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("status=%d body=%q: %v", w.Code, w.Body.String(), err)
	}
	return w.Code, out
}

// 呼び出し上限後の続行APIは、認証と本文の検査に通った要求を続行し、断る要求は理由付きの応答で返す。
// 認証の結果を error 型の変数へ入れて nil 判定が効かず、通る要求でも panic していた経路の回帰を防ぐ。
func TestBudgetContinueRouteContinuesAndRefusesWithProblem(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	d := Record{"id": todoID, "project": "owner/app", "status": "draft", "consultationBudget": Record{"calls": 40, "weighted": 6000000, "stopped": Record{"at": "now"}, "stops": []any{Record{"at": "now"}}}}
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	statement := "質問「共通番号12の相談を上限を足して続行しますか」への利用者の回答: A"
	raw, _ := json.Marshal(Record{"operationId": "continue-route-1", "stopCount": 1, "statement": statement, "instructionAt": chatInstructionAt})

	token := stopToken(t, db, false)
	if code, out := budgetContinueHTTP(t, s, token, "application/json", string(raw)); code != 403 || out["error"] == nil {
		t.Fatalf("代行の無いトークンを断っていない: %d %v", code, out)
	}
	if _, err := db.SetDelegated(ctx, "monitor", true, s.options.Now()); err != nil {
		t.Fatal(err)
	}
	if code, out := budgetContinueHTTP(t, s, token, "text/plain", string(raw)); code != 400 {
		t.Fatalf("JSONでない本文を断っていない: %d %v", code, out)
	}
	code, out := budgetContinueHTTP(t, s, token, "application/json", string(raw))
	if code != 200 || out["status"] != "thinking" {
		t.Fatalf("続行できない: %d %v", code, out)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil {
		t.Fatal(err)
	}
	if entries := fleet.Items(record(saved["consultationBudget"])["continuations"]); saved["status"] != "thinking" || len(entries) != 1 {
		t.Fatal("続行を保存していない", saved)
	}
}
